package main

// A lane serves one GATE endpoint (concept 3.1): a mailbox of pending requests keyed by
// device, and one worker that sends them, strictly one HTTP request at a time.
//
// Submit never blocks: it adds to or merges into the mailbox under a mutex that is never
// held during HTTP, then wakes the worker. Every submission gets its own one-slot result
// channel that is written exactly once, so a caller that never reads it costs nothing.
//
// How a submission for a device that is already waiting in the mailbox is folded in is the
// lane's mergeFunc: reads keep every submitter waiting for the one entry (all get the same
// outcome), writes replace the command and resolve the older submission as superseded.
// An entry is out of reach of merging once the worker took it into a request.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type laneKind int

const (
	readLane laneKind = iota
	writeLane
)

func (k laneKind) String() string {
	if k == writeLane {
		return "write"
	}
	return "read"
}

// Outcome is what happened to one submission.
type Outcome int

const (
	// OutcomeOK: read: the response carried an item for the device (whether it was valid
	// and applied is in laneResult.Apply); write: the command was acknowledged.
	OutcomeOK Outcome = iota
	// OutcomeError: write only: the command did not run (per-item error, request rejected
	// as a whole, or never sent because the lane stopped).
	OutcomeError
	// OutcomeUncertain: write only: the command may or may not have run (timeout, transport
	// error, non-2xx, unreadable body, no acknowledgement for it in the response).
	OutcomeUncertain
	// OutcomeSuperseded: write only: replaced in the mailbox by a newer command before it was sent.
	OutcomeSuperseded
	// OutcomeMissing: read only: the response had no item for the device.
	OutcomeMissing
	// OutcomeFailed: read only: the request failed as a whole (or the lane stopped).
	OutcomeFailed
)

func (o Outcome) String() string {
	switch o {
	case OutcomeOK:
		return "ok"
	case OutcomeError:
		return "error"
	case OutcomeUncertain:
		return "uncertain"
	case OutcomeSuperseded:
		return "superseded"
	case OutcomeMissing:
		return "missing"
	case OutcomeFailed:
		return "failed"
	}
	return fmt.Sprintf("Outcome(%d)", int(o))
}

// ApplyOutcome is what the device made of a read item (set by the read lane's completion hook).
type ApplyOutcome int

const (
	ApplyNone      ApplyOutcome = iota // not applied: the read did not return an item
	ApplyChanged                       // valid, applied, device state changed
	ApplyUnchanged                     // valid, applied, device state unchanged
	ApplyInvalid                       // item present but not valid for the device: not applied
	ApplyDiscarded                     // valid but dropped by the ordering rules (stage 3)
)

// laneResult is the outcome of one submission.
type laneResult struct {
	Outcome Outcome
	// Apply is set for reads with OutcomeOK by the completion hook.
	Apply ApplyOutcome
	// Item is the read item from the response (reads with OutcomeOK only).
	Item *ReqObject
	Err  error
	// Stamp orders evidence across both lanes: taken from one counter when the response
	// completed or the request was given up. 0 for superseded submissions (no evidence).
	Stamp uint64
	// Submitted is when this submission was made, Sent when its request left, Completed
	// when the response completed (zero when never sent).
	Submitted, Sent, Completed time.Time
}

// mergeFunc folds a new submission into the pending entry of the same device. It returns
// the object the entry sends from now on, and whether the submissions already in the entry
// are superseded (true) or keep waiting for the entry's outcome (false).
type mergeFunc func(pending, incoming ReqObject) (merged ReqObject, supersede bool)

// mergeKeepPending is the read lane policy: one read per device, every submitter waits for it.
func mergeKeepPending(pending, _ ReqObject) (ReqObject, bool) { return pending, false }

// mergeReplace is the write lane policy for lights and shutters: latest command wins.
func mergeReplace(_, incoming ReqObject) (ReqObject, bool) { return incoming, true }

type laneSub struct {
	ch chan laneResult
	at time.Time
}

func (s *laneSub) deliver(r laneResult) {
	r.Submitted = s.at
	s.ch <- r // one-slot channel, written exactly once: never blocks
}

// laneEntry is one device's pending request.
type laneEntry struct {
	key  string
	obj  ReqObject
	subs []*laneSub
	// Meta is free for the lane's owner: the request-sent hook may set it (stage 3 records
	// the device generation here) and the completion hook reads it. Only touched by the worker.
	Meta any

	sent time.Time
}

// Obj returns the object sent for this entry.
func (e *laneEntry) Obj() ReqObject { return e.obj }

// firstSubmit is the oldest submission still waiting on this entry.
func (e *laneEntry) firstSubmit() time.Time {
	var first time.Time
	for _, s := range e.subs {
		if first.IsZero() || s.at.Before(first) {
			first = s.at
		}
	}
	return first
}

var errLaneStopped = errors.New("lane stopped")

type laneConfig struct {
	kind laneKind
	url  string
	// timeout bounds one HTTP request (default 10 s).
	timeout time.Duration
	// maxItems is the most entries one request carries.
	maxItems int
	// asArray sends the request as a JSON array; false sends one single object per request
	// (legacy write protocol; maxItems is then 1).
	asArray bool
	merge   mergeFunc
	// stamps is the process-wide completion counter shared by both lanes.
	stamps *atomic.Uint64

	logf, debugf func(string, ...any)

	// onSent is called by the worker with the entries of a request just before it is sent.
	// onComplete is called by the worker once per entry with its outcome, before the outcome
	// is delivered, and may annotate it (Apply, Err). Neither may block.
	onSent     func(entries []*laneEntry)
	onComplete func(e *laneEntry, r *laneResult)
}

type lane struct {
	cfg    laneConfig
	client *gateClient

	mu      sync.Mutex
	queue   []*laneEntry
	index   map[string]*laneEntry
	stopped bool

	wake   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

func newLane(cfg laneConfig) *lane {
	if !cfg.asArray || cfg.maxItems < 1 {
		cfg.maxItems = 1
	}
	if cfg.merge == nil {
		cfg.merge = mergeKeepPending
	}
	if cfg.stamps == nil {
		cfg.stamps = new(atomic.Uint64)
	}
	if cfg.logf == nil {
		cfg.logf = func(string, ...any) {}
	}
	if cfg.debugf == nil {
		cfg.debugf = func(string, ...any) {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &lane{
		cfg:    cfg,
		client: newGateClient(cfg.url, cfg.timeout),
		index:  map[string]*laneEntry{},
		wake:   make(chan struct{}, 1),
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
	}
}

// start runs the worker. Call once.
func (l *lane) start() { go l.run() }

// stop aborts the request in flight, stops the worker and resolves everything still in the
// mailbox (reads as failed, writes as not run). Later submissions are resolved the same way.
func (l *lane) stop() {
	l.mu.Lock()
	l.stopped = true
	l.mu.Unlock()
	l.cancel()
	<-l.done
	l.mu.Lock()
	rest := l.queue
	l.queue, l.index = nil, map[string]*laneEntry{}
	l.mu.Unlock()
	for _, e := range rest {
		l.resolveStopped(e.subs)
	}
}

func (l *lane) resolveStopped(subs []*laneSub) {
	o := OutcomeFailed
	if l.cfg.kind == writeLane {
		o = OutcomeError
	}
	for _, s := range subs {
		s.deliver(laneResult{Outcome: o, Err: errLaneStopped, Stamp: l.cfg.stamps.Add(1)})
	}
}

// Submit queues objects (one per device) and returns one result channel per object, in
// order. All objects enter the mailbox under one lock, so an idle worker sees them together.
func (l *lane) Submit(objs ...ReqObject) []<-chan laneResult {
	now := time.Now()
	chans := make([]<-chan laneResult, len(objs))
	subs := make([]*laneSub, len(objs))
	for i := range objs {
		subs[i] = &laneSub{ch: make(chan laneResult, 1), at: now}
		chans[i] = subs[i].ch
	}

	var superseded []*laneSub
	l.mu.Lock()
	if l.stopped {
		l.mu.Unlock()
		l.resolveStopped(subs)
		return chans
	}
	for i, obj := range objs {
		key := entryKey(obj.Clu, obj.Id)
		if e, ok := l.index[key]; ok {
			merged, supersede := l.cfg.merge(e.obj, obj)
			e.obj = merged
			if supersede {
				superseded = append(superseded, e.subs...)
				e.subs = nil
			}
			e.subs = append(e.subs, subs[i])
			continue
		}
		e := &laneEntry{key: key, obj: obj, subs: []*laneSub{subs[i]}}
		l.index[key] = e
		l.queue = append(l.queue, e)
	}
	l.mu.Unlock()

	for _, s := range superseded {
		s.deliver(laneResult{Outcome: OutcomeSuperseded})
	}
	select {
	case l.wake <- struct{}{}:
	default:
	}
	return chans
}

// take removes the oldest entries, at most one request's worth, from the mailbox.
func (l *lane) take() []*laneEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := min(len(l.queue), l.cfg.maxItems)
	if n == 0 {
		return nil
	}
	batch := make([]*laneEntry, n)
	copy(batch, l.queue)
	l.queue = append([]*laneEntry(nil), l.queue[n:]...)
	for _, e := range batch {
		delete(l.index, e.key)
	}
	return batch
}

func (l *lane) run() {
	defer close(l.done)
	for {
		if l.ctx.Err() != nil {
			return
		}
		batch := l.take()
		if batch == nil {
			select {
			case <-l.wake:
				continue
			case <-l.ctx.Done():
				return
			}
		}
		l.send(batch)
	}
}

func (l *lane) send(batch []*laneEntry) {
	var body []byte
	var err error
	if l.cfg.asArray {
		objs := make([]ReqObject, len(batch))
		for i, e := range batch {
			objs[i] = e.obj
		}
		body, err = json.Marshal(objs)
	} else {
		body, err = json.Marshal(batch[0].obj)
	}

	sent := time.Now()
	for _, e := range batch {
		e.sent = sent
	}
	if err == nil {
		if l.cfg.onSent != nil {
			l.cfg.onSent(batch)
		}
		l.cfg.debugf("%s lane: request body: %s", l.cfg.kind, body)
		var resp []byte
		resp, err = l.client.post(l.ctx, body)
		if err == nil {
			l.cfg.debugf("%s lane: response body: %s", l.cfg.kind, resp)
			var results []laneResult
			if l.cfg.kind == writeLane {
				results, err = decodeWriteResponse(batch, resp)
			} else {
				results, err = decodeReadResponse(batch, resp)
			}
			if err == nil {
				l.complete(batch, results, sent, len(body), nil)
				return
			}
		}
	}

	failed := OutcomeFailed
	if l.cfg.kind == writeLane {
		failed = OutcomeUncertain
	}
	results := make([]laneResult, len(batch))
	for i := range results {
		results[i] = laneResult{Outcome: failed, Err: err}
	}
	l.complete(batch, results, sent, len(body), err)
}

func (l *lane) complete(batch []*laneEntry, results []laneResult, sent time.Time, bytes int, reqErr error) {
	done := time.Now()
	stamp := l.cfg.stamps.Add(1)
	ms := done.Sub(sent).Milliseconds()
	if reqErr != nil {
		l.cfg.logf("%s lane: request of %d items, %d bytes failed after %d ms: %v", l.cfg.kind, len(batch), bytes, ms, reqErr)
	} else {
		l.cfg.debugf("%s lane: request of %d items, %d bytes done in %d ms", l.cfg.kind, len(batch), bytes, ms)
	}
	for i, e := range batch {
		r := results[i]
		r.Stamp, r.Sent, r.Completed = stamp, sent, done
		if l.cfg.onComplete != nil {
			l.cfg.onComplete(e, &r)
		}
		for _, s := range e.subs {
			s.deliver(r)
		}
	}
}
