package main

import (
	"strings"
	"sync"
	"testing"

	"github.com/brutella/hap/characteristic"
)

// recordingMetrics records every event.
type recordingMetrics struct {
	mu        sync.Mutex
	requests  []RequestEvent
	commands  []CommandEvent
	refreshes []RefreshEvent
}

func (m *recordingMetrics) Request(e RequestEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, e)
}

func (m *recordingMetrics) Command(e CommandEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.commands = append(m.commands, e)
}

func (m *recordingMetrics) Refresh(e RefreshEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshes = append(m.refreshes, e)
}

func (m *recordingMetrics) snapshot() (req []RequestEvent, cmd []CommandEvent, ref []RefreshEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append(req, m.requests...), append(cmd, m.commands...), append(ref, m.refreshes...)
}

func (m *recordingMetrics) lane(k laneKind) []RequestEvent {
	req, _, _ := m.snapshot()
	var out []RequestEvent
	for _, e := range req {
		if e.Lane == k {
			out = append(out, e)
		}
	}
	return out
}

func newMetricsSet(t *testing.T, configure func(*GrentonSet), clus ...*Clu) (*GrentonSet, *fakeGate, *recordingMetrics) {
	t.Helper()
	m := &recordingMetrics{}
	gs, g := newTestSetWith(t, func(gs *GrentonSet) {
		gs.metrics = m
		if configure != nil {
			configure(gs)
		}
	}, clus...)
	return gs, g, m
}

func TestMetrics_EachEventReportedOnce(t *testing.T) {
	clu := lightsClu(3)
	gs, g, m := newMetricsSet(t, withQueryLimit(2), clu)

	gs.Refresh()
	reads := m.lane(readLane)
	if len(reads) != 2 {
		t.Fatalf("read request events = %d, want 2 (one per request)", len(reads))
	}
	reqs := g.Requests(fakeReadPath)
	for i, e := range reads {
		if e.Items != len(reqs[i].Items) || e.Bytes != len(reqs[i].Body) || !e.Success || e.Clus != 1 {
			t.Errorf("read event %d = %+v, request had %d items, %d bytes", i, e, len(reqs[i].Items), len(reqs[i].Body))
		}
	}
	if e := reads[1]; e.Clu != tCLU || e.Id != "DOU0003" {
		t.Errorf("single-item request event identity = %s|%s", e.Clu, e.Id)
	}
	_, cmds, refs := m.snapshot()
	if len(cmds) != 0 || len(refs) != 1 {
		t.Fatalf("commands = %d, refreshes = %d; want 0, 1", len(cmds), len(refs))
	}
	if c := refs[0].Counts; c.Objects != 3 || c.Applied != 3 || c.Changed != 3 || c.Duration <= 0 {
		t.Errorf("refresh event = %+v", c)
	}

	syncHomeKit(t, gs) // hap characteristics are not synchronised: no HomeKit write during a sync
	hkWrite(findC(t, clu.Lights[0].HkAccessory(), characteristic.TypeOn), true)
	settle(t, gs)
	writes := m.lane(writeLane)
	_, cmds, _ = m.snapshot()
	if len(writes) != 1 || len(cmds) != 1 {
		t.Fatalf("write request events = %d, command events = %d; want 1, 1", len(writes), len(cmds))
	}
	if w := writes[0]; w.Items != 1 || w.Clu != tCLU || w.Id != "DOU0001" || !w.Success || w.Bytes != len(g.Requests(fakeSetPath)[0].Body) {
		t.Errorf("write request event = %+v", w)
	}
	if c := cmds[0]; c.Clu != tCLU || c.Id != "DOU0001" || c.Outcome != OutcomeOK || c.Total < c.HTTP || c.Total < c.QueueWait {
		t.Errorf("command event = %+v", c)
	}
}

func TestMetrics_SupersededCommandIsNotReported(t *testing.T) {
	gs, g, m := newMetricsSet(t, nil, lightsClu(2))
	h := g.HoldNext(fakeSetPath)
	a := gs.writer.Submit(lightCmd("DOU0001", true))[0]
	h.Wait(t)
	b := gs.writer.Submit(lightCmd("DOU0002", true))[0]
	c := gs.writer.Submit(lightCmd("DOU0002", false))[0]
	h.Release()
	waitResult(t, a)
	if r := waitResult(t, b); r.Outcome != OutcomeSuperseded {
		t.Fatalf("b = %v", r.Outcome)
	}
	waitResult(t, c)

	_, cmds, _ := m.snapshot()
	if len(cmds) != 2 {
		t.Fatalf("command events = %d, want 2 (superseded command emits none)", len(cmds))
	}
	if cmds[1].Id != "DOU0002" || cmds[1].QueueWait <= 0 {
		t.Errorf("second command event = %+v, want DOU0002 with its queue wait", cmds[1])
	}
	if w := m.lane(writeLane); len(w) != 2 || w[1].QueueWait <= 0 {
		t.Errorf("write request events = %+v", w)
	}
}

func TestMetrics_FailedRequestReportedAsUnsuccessful(t *testing.T) {
	gs, g, m := newMetricsSet(t, nil, lightsClu(1))
	g.FailNext(fakeReadPath, 500, 1)
	gs.Refresh()
	if r := m.lane(readLane); len(r) != 1 || r[0].Success {
		t.Errorf("read events = %+v, want one unsuccessful", r)
	}
	_, _, refs := m.snapshot()
	if len(refs) != 1 || refs[0].Counts.Failed != 1 {
		t.Errorf("refresh events = %+v", refs)
	}
}

func TestMetrics_Overlap(t *testing.T) {
	clu := lightsClu(2)
	gs, g, m := newMetricsSet(t, nil, clu)

	// A write while a read is held open: both overlap.
	h := g.HoldNext(fakeReadPath)
	done := make(chan struct{})
	go func() { defer close(done); gs.Refresh() }()
	h.Wait(t)
	waitResult(t, gs.writer.Submit(lightCmd("DOU0001", true))[0])
	h.Release()
	<-done

	w, r := m.lane(writeLane), m.lane(readLane)
	if len(w) != 1 || !w[0].Overlap {
		t.Errorf("write during a held read: %+v, want overlap", w)
	}
	if len(r) != 1 || !r[0].Overlap {
		t.Errorf("held read with a write during it: %+v, want overlap", r)
	}
	settle(t, gs)

	// Alone: no overlap (a write and a refresh, one after another; commands submitted to the
	// lane directly have no device, so no confirm read).
	waitResult(t, gs.writer.Submit(lightCmd("DOU0002", true))[0])
	settle(t, gs)
	gs.Refresh()
	w, r = m.lane(writeLane), m.lane(readLane)
	if len(w) != 2 || len(r) != 2 {
		t.Fatalf("events: %d writes, %d reads; want 2 writes and 2 reads", len(w), len(r))
	}
	for _, e := range append(w[1:], r[1:]...) {
		if e.Overlap {
			t.Errorf("%v request without the other lane busy flagged as overlapping", e.Lane)
		}
	}
}

func TestMetrics_OneDebugLogLinePerRequest(t *testing.T) {
	logs := captureLog(t)
	gs, _ := newTestSetWith(t, func(gs *GrentonSet) {
		gs.QueryLimit = 2
		gs.Verbose = true
	}, lightsClu(3))
	gs.Refresh()
	out := logs.String()
	if n := strings.Count(out, "read lane: request of"); n != 2 {
		t.Errorf("request log lines = %d, want 2:\n%s", n, out)
	}
	if !strings.Contains(out, "read lane: request body: [") {
		t.Error("verbose log lacks the JSON body")
	}

	logs2 := captureLog(t)
	gs.Verbose = false
	gs.Refresh()
	if out := logs2.String(); strings.Contains(out, "request of") || strings.Contains(out, "body") {
		t.Errorf("request lines without Verbose:\n%s", out)
	}
}
