package main

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/brutella/hap/characteristic"
)

// One test per row of the ordering table in concept 3.3, each driven by holding fake GATE
// requests and releasing them in a chosen order. Assertions are on what HomeKit shows and
// on what the fake GATE received.

// syncRecorder records the values HomeKit syncs (not controller writes) set on a characteristic.
type syncRecorder struct {
	mu   sync.Mutex
	vals []any
}

func recordSyncs(c *characteristic.C) *syncRecorder {
	r := &syncRecorder{}
	c.OnCValueUpdate(func(_ *characteristic.C, nv, _ any, req *http.Request) {
		if req == nil {
			r.mu.Lock()
			r.vals = append(r.vals, nv)
			r.mu.Unlock()
		}
	})
	return r
}

func (r *syncRecorder) contains(v any) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, x := range r.vals {
		if x == v {
			return true
		}
	}
	return false
}

// hkWriteAsync performs a HomeKit write in its own goroutine.
func hkWriteAsync(wg *sync.WaitGroup, c *characteristic.C, v any) {
	wg.Add(1)
	go func() { defer wg.Done(); hkWrite(c, v) }()
}

// queuedIn reports whether the lane's mailbox holds an entry for the device id.
func queuedIn(l *lane, id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.queue {
		if e.obj.Id == id {
			return true
		}
	}
	return false
}

// readySet returns a set of lights that have been read once (all off) with HomeKit settled.
func readySet(t *testing.T, n int) (*GrentonSet, *fakeGate, *Clu) {
	t.Helper()
	clu := lightsClu(n)
	gs, g := newTestSet(t, clu)
	if c := gs.Refresh(); c.Applied != n {
		t.Fatalf("initial refresh = %+v", c)
	}
	settle(t, gs)
	return gs, g, clu
}

func onC(t *testing.T, l *Light) *characteristic.C {
	return findC(t, l.HkAccessory(), characteristic.TypeOn)
}

// Row 1: a write queued behind another request; a poll starts afterwards and returns the
// old state. Discarded: a command is pending (rule 4).
func TestOrdering_PollWhileWriteQueuedIsDiscarded(t *testing.T) {
	gs, g, clu := readySet(t, 2)
	on1, on2 := onC(t, clu.Lights[0]), onC(t, clu.Lights[1])
	rec := recordSyncs(on1)

	var wg sync.WaitGroup
	h := g.HoldNext(fakeSetPath)
	hkWriteAsync(&wg, on2, true) // in flight, held
	h.Wait(t)
	hkWriteAsync(&wg, on1, true) // queued behind it
	eventually(t, 2*time.Second, func() bool { return queuedIn(gs.writer, "DOU0001") }, "light 1 command queued")

	c := gs.Refresh() // the GATE still has both lights off
	if c.Discarded != 2 || c.Applied != 0 {
		t.Errorf("refresh = %+v, want both lights discarded (commands pending or in flight)", c)
	}
	syncHomeKit(t, gs)
	if on1.Val != true {
		t.Errorf("HomeKit On = %v after the poll, want true", on1.Val)
	}

	h.Release()
	wg.Wait()
	settle(t, gs)
	if on1.Val != true || g.Payload(tCLU, "DOU0001")["State"] != true {
		t.Errorf("after the write: HomeKit %v, GATE %v; want both on", on1.Val, g.Payload(tCLU, "DOU0001")["State"])
	}
	if rec.contains(false) {
		t.Error("HomeKit flipped back to off")
	}
}

// Row 2: a poll sent before a write, its result arrives after the write finished.
// Discarded: the generation changed (rules 1, 2, 4).
func TestOrdering_PollSentBeforeWriteIsDiscarded(t *testing.T) {
	gs, g, clu := readySet(t, 2)
	on1 := onC(t, clu.Lights[0])
	rec := recordSyncs(on1)

	h := g.HoldNext(fakeReadPath)
	done := make(chan refreshCounts, 1)
	go func() { done <- gs.Refresh() }()
	h.Wait(t) // the GATE answered "off" for both lights; the answer is held

	var wg sync.WaitGroup
	hkWriteAsync(&wg, on1, true)
	wg.Wait()
	eventually(t, 2*time.Second, func() bool { return g.RequestCount(fakeSetPath) == 1 && gs.writer.idle() },
		"the write to finish")

	h.Release()
	c := <-done
	if c.Discarded != 1 || c.Applied != 1 {
		t.Errorf("refresh = %+v, want light 1 discarded and light 2 applied", c)
	}
	settle(t, gs) // the confirm read of the write
	if on1.Val != true {
		t.Errorf("HomeKit On = %v, want true", on1.Val)
	}
	if rec.contains(false) {
		t.Error("HomeKit flipped back to off")
	}
}

// Row 4: the confirm read of an earlier write returns after a newer command was queued.
// Discarded: the generation changed and a command is pending (rules 1, 4).
func TestOrdering_ConfirmReadAfterNewerCommandIsDiscarded(t *testing.T) {
	gs, g, clu := readySet(t, 1)
	on1 := onC(t, clu.Lights[0])

	var wg sync.WaitGroup
	hr := g.HoldNext(fakeReadPath)
	hkWriteAsync(&wg, on1, true)
	hr.Wait(t) // the confirm read of "on" reached the GATE, which answers "on"

	hw := g.HoldNext(fakeSetPath)
	hkWriteAsync(&wg, on1, false)
	hw.Wait(t) // the newer command is in flight
	rec := recordSyncs(on1)

	hr.Release()
	eventually(t, 2*time.Second, gs.reader.idle, "the confirm read to complete")
	syncHomeKit(t, gs)
	if on1.Val != false {
		t.Errorf("HomeKit On = %v after the old confirm read, want false", on1.Val)
	}

	hw.Release()
	wg.Wait()
	settle(t, gs)
	if on1.Val != false || g.Payload(tCLU, "DOU0001")["State"] != false {
		t.Errorf("after the second write: HomeKit %v, GATE %v; want both off", on1.Val, g.Payload(tCLU, "DOU0001")["State"])
	}
	if rec.contains(true) {
		t.Error("HomeKit showed the old confirm read")
	}
}

// Row 5: a write fails. One confirm read puts HomeKit back to the real state (rules 2, 5).
func TestOrdering_FailedWriteIsRestoredByConfirmRead(t *testing.T) {
	cases := []struct {
		name string
		fail func(g *fakeGate)
	}{
		{"uncertain", func(g *fakeGate) { g.FailNext(fakeSetPath, 500, 1) }},
		{"per-item error", func(g *fakeGate) {
			g.SetWriteStyle(fakeNewStyle)
			g.FailItem(tCLU, "DOU0001", "boom")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gs, g, clu := readySet(t, 1)
			on1 := onC(t, clu.Lights[0])
			tc.fail(g)
			reads := g.RequestCount(fakeReadPath)

			hkWrite(on1, true)
			settle(t, gs)

			if g.Payload(tCLU, "DOU0001")["State"] != false {
				t.Fatal("the fake GATE applied the failed write")
			}
			if on1.Val != false {
				t.Errorf("HomeKit On = %v after the confirm read, want the GATE state false", on1.Val)
			}
			rs := g.Requests(fakeReadPath)[reads:]
			if len(rs) != 1 || len(rs[0].Items) != 1 || rs[0].Items[0]["Id"] != "DOU0001" {
				t.Errorf("read requests after the write = %d, want one confirm read of the light", len(rs))
			}
		})
	}
}
