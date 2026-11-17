package main

import (
	"sync"
	"time"
)

// Metrics receives one call per event, each from one place in the code (concept 3.8).
// Calls come from lane workers and refresh; an implementation must never block them.
type Metrics interface {
	// Request: one GATE request finished (or was given up), read or write.
	Request(RequestEvent)
	// Command: one write command finished. Superseded commands are not reported.
	Command(CommandEvent)
	// Refresh: one refresh finished.
	Refresh(RefreshEvent)
}

// RequestEvent describes one GATE request.
type RequestEvent struct {
	Lane      laneKind
	Time      time.Time // when the request completed
	Items     int
	Clus      int // distinct CLUs among the items
	Bytes     int // request body size
	QueueWait time.Duration
	HTTP      time.Duration
	Success   bool // the request as a whole succeeded (per-item errors do not count)
	Overlap   bool // the other lane had a request in flight at some time during this one
	Clu, Id   string
}

// CommandEvent describes one write command from submission to its result.
type CommandEvent struct {
	Time      time.Time
	Clu, Id   string
	Outcome   Outcome
	QueueWait time.Duration // submit to request sent
	HTTP      time.Duration // the request that carried it
	Total     time.Duration // submit to result
}

// RefreshEvent describes one refresh.
type RefreshEvent struct {
	Time   time.Time
	Counts refreshCounts
}

// noMetrics is the default: nothing is recorded.
type noMetrics struct{}

func (noMetrics) Request(RequestEvent) {}
func (noMetrics) Command(CommandEvent) {}
func (noMetrics) Refresh(RefreshEvent) {}

// overlapTracker records whether a request of one lane overlapped in time with a request
// of the other lane.
type overlapTracker struct {
	mu         sync.Mutex
	inFlight   [2]bool
	overlapped [2]bool
}

// begin marks a request of lane k as in flight.
func (o *overlapTracker) begin(k laneKind) {
	o.mu.Lock()
	defer o.mu.Unlock()
	other := 1 - k
	o.inFlight[k] = true
	o.overlapped[k] = o.inFlight[other]
	if o.inFlight[other] {
		o.overlapped[other] = true
	}
}

// end marks the request of lane k as done and reports whether it overlapped.
func (o *overlapTracker) end(k laneKind) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.inFlight[k] = false
	v := o.overlapped[k]
	o.overlapped[k] = false
	return v
}
