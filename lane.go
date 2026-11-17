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
	"strings"
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

// mergeCommand is the write lane policy (concept 3.2): a thermostat command is merged
// field-wise into the pending one (the fields of the newer command win, the others stay)
// and the older submitters share the merged command's outcome; every other command
// replaces the pending one. The merged command keeps the newer command's sequence number.
func mergeCommand(pending, incoming ReqObject) (ReqObject, bool) {
	if pending.Thermo == nil || incoming.Thermo == nil {
		return mergeReplace(pending, incoming)
	}
	t := *pending.Thermo
	in := incoming.Thermo
	if in.TempSetpoint != nil {
		t.TempSetpoint = in.TempSetpoint
	}
	if in.State != nil {
		t.State = in.State
	}
	if in.Mode != nil {
		t.Mode = in.Mode
	}
	merged := incoming
	merged.Thermo = &t
	return merged, false
}

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
	// overlap is shared by both lanes; metrics receives one event per request and, on the
	// write lane, one per finished command.
	overlap *overlapTracker
	metrics Metrics

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
	// sending: the worker took entries and has not finished delivering their results.
	sending bool

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
	if cfg.overlap == nil {
		cfg.overlap = &overlapTracker{}
	}
	if cfg.metrics == nil {
		cfg.metrics = noMetrics{}
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

// resolveStopped gives up the submissions of one entry (one stamp for all of them) after
// the lane stopped: never sent, so a read failed and a command did not run.
func (l *lane) resolveStopped(subs []*laneSub) {
	r := laneResult{Outcome: l.notSentOutcome(), Err: errLaneStopped, Stamp: l.cfg.stamps.Add(1)}
	for _, s := range subs {
		s.deliver(r)
	}
}

// notSentOutcome is the outcome of an entry whose request never left: reads failed,
// commands definitely did not run.
func (l *lane) notSentOutcome() Outcome {
	if l.cfg.kind == writeLane {
		return OutcomeError
	}
	return OutcomeFailed
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
		for _, s := range subs { // each submission is an entry of its own
			l.resolveStopped([]*laneSub{s})
		}
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
	l.sending = true
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
		l.mu.Lock()
		l.sending = false
		l.mu.Unlock()
	}
}

// idle reports whether the mailbox is empty and no request is being sent or completed
// (its hooks have run and its results are delivered).
func (l *lane) idle() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.queue) == 0 && !l.sending
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
	switch {
	case err != nil:
		err = fmt.Errorf("encoding request: %w", err)
	case l.ctx.Err() != nil:
		err = errLaneStopped
	}
	if err != nil {
		results := make([]laneResult, len(batch))
		for i := range results {
			results[i] = laneResult{Outcome: l.notSentOutcome(), Err: err}
		}
		l.complete(batch, results, sent, len(body), false, err, false)
		return
	}

	if l.cfg.onSent != nil {
		l.cfg.onSent(batch)
	}
	l.cfg.debugf("%s lane: request body: %s", l.cfg.kind, body)
	l.cfg.overlap.begin(l.cfg.kind)
	resp, err := l.client.post(l.ctx, body)
	overlapped := l.cfg.overlap.end(l.cfg.kind)
	if err == nil {
		l.cfg.debugf("%s lane: response body: %s", l.cfg.kind, resp)
		var results []laneResult
		if l.cfg.kind == writeLane {
			results, err = decodeWriteResponse(batch, resp)
		} else {
			results, err = decodeReadResponse(batch, resp)
		}
		if err == nil {
			l.complete(batch, results, sent, len(body), overlapped, nil, true)
			return
		}
	}

	// Sent, but no usable answer: a read failed, a command may or may not have run.
	failed := OutcomeFailed
	if l.cfg.kind == writeLane {
		failed = OutcomeUncertain
	}
	results := make([]laneResult, len(batch))
	for i := range results {
		results[i] = laneResult{Outcome: failed, Err: err}
	}
	l.complete(batch, results, sent, len(body), overlapped, err, true)
}

// complete stamps the results, reports the request (log line and metrics), runs the
// completion hook per entry and delivers the outcomes. wasSent is false when the request
// never left (it could not be encoded, or the lane stopped): no request metric then.
func (l *lane) complete(batch []*laneEntry, results []laneResult, sent time.Time, bytes int, overlapped bool, reqErr error, wasSent bool) {
	done := time.Now()
	stamp := l.cfg.stamps.Add(1)

	ev := RequestEvent{
		Lane: l.cfg.kind, Time: done, Items: len(batch), Bytes: bytes,
		HTTP: done.Sub(sent), Success: reqErr == nil, Overlap: overlapped,
	}
	clus := map[string]bool{}
	for _, e := range batch {
		clus[strings.ToLower(e.obj.Clu)] = true
		if w := sent.Sub(e.firstSubmit()); w > ev.QueueWait {
			ev.QueueWait = w
		}
	}
	ev.Clus = len(clus)
	if len(batch) == 1 {
		ev.Clu, ev.Id = batch[0].obj.Clu, batch[0].obj.Id
	}
	switch {
	case !wasSent:
		l.cfg.logf("%s lane: request of %d items not sent: %v", l.cfg.kind, ev.Items, reqErr)
	case reqErr != nil:
		l.cfg.logf("%s lane: request of %d items, %d bytes failed after %d ms: %v",
			l.cfg.kind, ev.Items, ev.Bytes, ev.HTTP.Milliseconds(), reqErr)
	default:
		l.cfg.debugf("%s lane: request of %d items, %d bytes, %d ms (queue wait %d ms, overlap %v)",
			l.cfg.kind, ev.Items, ev.Bytes, ev.HTTP.Milliseconds(), ev.QueueWait.Milliseconds(), ev.Overlap)
	}
	if wasSent {
		l.cfg.metrics.Request(ev)
	}

	for i, e := range batch {
		r := results[i]
		r.Stamp, r.Sent, r.Completed = stamp, sent, done
		if l.cfg.onComplete != nil {
			l.cfg.onComplete(e, &r)
		}
		if l.cfg.kind == writeLane {
			first := e.firstSubmit()
			l.cfg.metrics.Command(CommandEvent{
				Time: done, Clu: e.obj.Clu, Id: e.obj.Id, Outcome: r.Outcome,
				QueueWait: sent.Sub(first), HTTP: done.Sub(sent), Total: done.Sub(first),
			})
		}
		for _, s := range e.subs {
			s.deliver(r)
		}
	}
}
