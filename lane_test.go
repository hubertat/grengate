package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestLane builds and starts a lane against the fake GATE (read path for read lanes,
// set path for write lanes). mut may adjust the config before the lane is built.
func newTestLane(t testing.TB, g *fakeGate, kind laneKind, mut func(*laneConfig)) *lane {
	t.Helper()
	cfg := laneConfig{kind: kind, timeout: 2 * time.Second}
	if kind == readLane {
		cfg.url, cfg.maxItems, cfg.asArray, cfg.merge = g.URL()+fakeReadPath, 30, true, mergeKeepPending
	} else {
		cfg.url, cfg.maxItems, cfg.asArray, cfg.merge = g.URL()+fakeSetPath, 1, false, mergeReplace
	}
	if mut != nil {
		mut(&cfg)
	}
	l := newLane(cfg)
	l.start()
	t.Cleanup(l.stop)
	return l
}

// waitResult reads one result with a deadline.
func waitResult(t testing.TB, ch <-chan laneResult) laneResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(3 * time.Second):
		t.Fatal("no result within 3s")
		return laneResult{}
	}
}

// assertPending fails if ch already holds a result.
func assertPending(t testing.TB, ch <-chan laneResult, what string) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("%s: unexpected result %+v", what, r)
	default:
	}
}

func lightCmd(id string, on bool) ReqObject {
	return ReqObject{Clu: tCLU, Id: id, Kind: "Light", Cmd: "SET", Light: &LightPayload{State: ptr(on)}}
}

func lightRead(id string) ReqObject {
	return ReqObject{Clu: tCLU, Id: id, Kind: "Light"}
}

// itemIds returns the Id of every item of a recorded request.
func itemIds(r fakeGateRequest) []string {
	var out []string
	for _, it := range r.Items {
		id, _ := it["Id"].(string)
		out = append(out, id)
	}
	return out
}

func TestLane_SubmitDuringHangingRequest(t *testing.T) {
	g := newFakeGate(t)
	for i := 0; i < 101; i++ {
		g.SetLight(tCLU, lightId(i), false)
	}
	g.HangAll(fakeSetPath)
	l := newTestLane(t, g, writeLane, nil)

	first := l.Submit(lightCmd(lightId(0), true))[0]
	g.WaitForRequests(t, fakeSetPath, 1)

	var durations []time.Duration
	var chans []<-chan laneResult
	for i := 1; i <= 100; i++ {
		start := time.Now()
		chans = append(chans, l.Submit(lightCmd(lightId(i), true))[0])
		durations = append(durations, time.Since(start))
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	if med := durations[len(durations)/2]; med > time.Millisecond {
		t.Errorf("median submission took %v while a request hangs, want < 1ms", med)
	}
	if worst := durations[len(durations)-1]; worst > 50*time.Millisecond {
		t.Errorf("slowest submission took %v while a request hangs", worst)
	}
	assertPending(t, first, "hanging command")
	if n := g.RequestCount(fakeSetPath); n != 1 {
		t.Errorf("requests while hanging = %d, want 1", n)
	}

	g.ReleaseAll()
	if r := waitResult(t, first); r.Outcome != OutcomeOK {
		t.Errorf("first command = %v (%v)", r.Outcome, r.Err)
	}
	for _, ch := range chans {
		if r := waitResult(t, ch); r.Outcome != OutcomeOK {
			t.Errorf("queued command = %v (%v)", r.Outcome, r.Err)
		}
	}
}

func lightId(i int) string { return fmt.Sprintf("DOU%04d", i) }

// concurrencyServer answers like the GATE scripts (read: [], write: echo) and records the
// highest number of requests it was handling at the same time.
func concurrencyServer(t testing.TB) (*httptest.Server, *atomic.Int32) {
	var inFlight, maxInFlight atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		defer inFlight.Add(-1)
		time.Sleep(2 * time.Millisecond) // server-side work, keeps requests overlapping if the lane allowed it
		var body any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path == fakeReadPath {
			_, _ = w.Write([]byte("[]"))
			return
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &maxInFlight
}

func TestLane_NeverMoreThanOneRequestInFlight(t *testing.T) {
	for _, kind := range []laneKind{readLane, writeLane} {
		t.Run(kind.String(), func(t *testing.T) {
			srv, maxInFlight := concurrencyServer(t)
			cfg := laneConfig{kind: kind, timeout: 2 * time.Second, maxItems: 3, asArray: kind == readLane}
			if kind == readLane {
				cfg.url, cfg.merge = srv.URL+fakeReadPath, mergeKeepPending
			} else {
				cfg.url, cfg.merge = srv.URL+fakeSetPath, mergeReplace
			}
			l := newLane(cfg)
			l.start()
			t.Cleanup(l.stop)

			var wg sync.WaitGroup
			for w := 0; w < 10; w++ {
				wg.Add(1)
				go func(w int) {
					defer wg.Done()
					for i := 0; i < 5; i++ {
						obj := lightRead(lightId(w*10 + i))
						if kind == writeLane {
							obj = lightCmd(lightId(w*10+i), true)
						}
						waitResult(t, l.Submit(obj)[0])
					}
				}(w)
			}
			wg.Wait()
			if m := maxInFlight.Load(); m != 1 {
				t.Errorf("max requests in flight = %d, want 1", m)
			}
		})
	}
}

func TestLane_WaitingCommandIsReplacedAndSuperseded(t *testing.T) {
	g := newFakeGate(t)
	g.SetLight(tCLU, "DOU0001", false)
	g.SetLight(tCLU, "DOU0002", false)
	l := newTestLane(t, g, writeLane, nil)

	h := g.HoldNext(fakeSetPath)
	a := l.Submit(lightCmd("DOU0001", true))[0]
	h.Wait(t)
	b := l.Submit(lightCmd("DOU0002", true))[0]
	c := l.Submit(lightCmd("DOU0002", false))[0]

	rb := waitResult(t, b) // resolved by the replacing submission, before the held request returns
	if rb.Outcome != OutcomeSuperseded {
		t.Fatalf("replaced command = %v, want superseded", rb.Outcome)
	}
	if rb.Stamp != 0 {
		t.Errorf("superseded result carries stamp %d, want 0 (no evidence)", rb.Stamp)
	}
	assertPending(t, a, "held command")

	h.Release()
	if r := waitResult(t, a); r.Outcome != OutcomeOK {
		t.Errorf("held command = %v (%v)", r.Outcome, r.Err)
	}
	if r := waitResult(t, c); r.Outcome != OutcomeOK {
		t.Errorf("replacing command = %v (%v)", r.Outcome, r.Err)
	}
	reqs := g.Requests(fakeSetPath)
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2 (the replaced command is never sent)", len(reqs))
	}
	if id := reqs[1].Items[0]["Id"]; id != "DOU0002" {
		t.Errorf("second request for %v", id)
	}
	if got := g.Payload(tCLU, "DOU0002")["State"]; got != false {
		t.Errorf("DOU0002 state at GATE = %v, want false (latest command)", got)
	}
}

func TestLane_CommandInRequestBeingSentIsNotReplaced(t *testing.T) {
	g := newFakeGate(t)
	g.SetLight(tCLU, "DOU0001", false)
	l := newTestLane(t, g, writeLane, nil)

	h := g.HoldNext(fakeSetPath)
	a := l.Submit(lightCmd("DOU0001", true))[0]
	h.Wait(t)
	b := l.Submit(lightCmd("DOU0001", false))[0]
	assertPending(t, a, "in-flight command after a newer submission")

	h.Release()
	if r := waitResult(t, a); r.Outcome != OutcomeOK {
		t.Errorf("in-flight command = %v, want ok", r.Outcome)
	}
	if r := waitResult(t, b); r.Outcome != OutcomeOK {
		t.Errorf("newer command = %v, want ok", r.Outcome)
	}
	reqs := g.Requests(fakeSetPath)
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	if g.Payload(tCLU, "DOU0001")["State"] != false {
		t.Error("final state should be the newer command's")
	}
}

func TestLane_UnreadResultsDoNotBlockWorker(t *testing.T) {
	g := newFakeGate(t)
	for i := 0; i < 40; i++ {
		g.SetLight(tCLU, lightId(i), false)
	}
	w := newTestLane(t, g, writeLane, nil)
	r := newTestLane(t, g, readLane, func(c *laneConfig) { c.maxItems = 5 })

	for i := 0; i < 40; i++ {
		w.Submit(lightCmd(lightId(i), i%2 == 0)) // results never read
		r.Submit(lightRead(lightId(i)))
		r.Submit(lightRead(lightId(i))) // merged, both channels unread
	}
	if res := waitResult(t, w.Submit(lightCmd(lightId(0), false))[0]); res.Outcome != OutcomeOK {
		t.Errorf("write after unread results = %v", res.Outcome)
	}
	if res := waitResult(t, r.Submit(lightRead(lightId(0)))[0]); res.Outcome != OutcomeOK {
		t.Errorf("read after unread results = %v", res.Outcome)
	}
}

func TestLane_MergedReadsEachGetOutcomeOnOwnChannel(t *testing.T) {
	g := newFakeGate(t)
	g.SetLight(tCLU, "DOU0001", false)
	g.SetLight(tCLU, "DOU0002", true)
	l := newTestLane(t, g, readLane, nil)

	h := g.HoldNext(fakeReadPath)
	first := l.Submit(lightRead("DOU0001"))[0]
	h.Wait(t)
	var merged []<-chan laneResult
	for i := 0; i < 3; i++ {
		merged = append(merged, l.Submit(lightRead("dou0002"))[0]) // case differs: same device
	}
	merged = append(merged, l.Submit(lightRead("DOU0002"), lightRead("DOU0002"))...)
	h.Release()
	waitResult(t, first)

	var stamp uint64
	for i, ch := range merged {
		r := waitResult(t, ch)
		if r.Outcome != OutcomeOK || r.Item == nil || r.Item.Light == nil || r.Item.Light.State == nil || !*r.Item.Light.State {
			t.Errorf("merged read %d = %+v", i, r)
		}
		if i == 0 {
			stamp = r.Stamp
		} else if r.Stamp != stamp {
			t.Errorf("merged read %d stamp %d, want %d (same outcome)", i, r.Stamp, stamp)
		}
	}
	reqs := g.Requests(fakeReadPath)
	if len(reqs) != 2 || len(reqs[1].Items) != 1 {
		t.Fatalf("requests = %d, second has %v; want the device read once", len(reqs), itemIds(reqs[len(reqs)-1]))
	}
}

func TestLane_StampsIncreaseInCompletionOrder(t *testing.T) {
	g := newFakeGate(t)
	g.SetLight(tCLU, "DOU0001", false)
	stamps := new(atomic.Uint64)
	r := newTestLane(t, g, readLane, func(c *laneConfig) { c.stamps = stamps })
	w := newTestLane(t, g, writeLane, func(c *laneConfig) { c.stamps = stamps })

	h := g.HoldNext(fakeReadPath)
	read := r.Submit(lightRead("DOU0001"))[0]
	h.Wait(t)
	write1 := waitResult(t, w.Submit(lightCmd("DOU0001", true))[0])
	write2 := waitResult(t, w.Submit(lightCmd("DOU0001", false))[0])
	h.Release()
	rr := waitResult(t, read)

	if !(write1.Stamp > 0 && write1.Stamp < write2.Stamp && write2.Stamp < rr.Stamp) {
		t.Errorf("stamps write1=%d write2=%d read=%d: want increasing in completion order", write1.Stamp, write2.Stamp, rr.Stamp)
	}
	if !(write1.Sent.Before(rr.Completed) && rr.Sent.Before(write1.Sent)) {
		t.Error("read was sent first but must complete last")
	}
}

func TestLane_HooksSeeEntriesAndAnnotateResults(t *testing.T) {
	g := newFakeGate(t)
	g.SetLight(tCLU, "DOU0001", true)
	g.SetLight(tCLU, "DOU0002", true)
	var sentBeforeGate atomic.Bool
	l := newTestLane(t, g, readLane, func(c *laneConfig) {
		c.onSent = func(es []*laneEntry) {
			sentBeforeGate.Store(g.RequestCount(fakeReadPath) == 0)
			for i, e := range es {
				e.Meta = i + 100
			}
		}
		c.onComplete = func(e *laneEntry, r *laneResult) {
			if r.Outcome == OutcomeOK && e.Meta == 101 {
				r.Apply = ApplyChanged
			}
		}
	})
	res := l.Submit(lightRead("DOU0001"), lightRead("DOU0002"))
	r1, r2 := waitResult(t, res[0]), waitResult(t, res[1])
	if !sentBeforeGate.Load() {
		t.Error("onSent ran after the GATE received the request")
	}
	if r1.Apply != ApplyNone || r2.Apply != ApplyChanged {
		t.Errorf("annotations = %v, %v; want none, changed (Meta carried from onSent)", r1.Apply, r2.Apply)
	}
}

func TestLane_StopResolvesPendingSubmissions(t *testing.T) {
	g := newFakeGate(t)
	g.SetLight(tCLU, "DOU0001", false)
	g.SetLight(tCLU, "DOU0002", false)
	g.HangAll(fakeSetPath)
	l := newTestLane(t, g, writeLane, nil)
	inFlight := l.Submit(lightCmd("DOU0001", true))[0]
	g.WaitForRequests(t, fakeSetPath, 1)
	waiting := l.Submit(lightCmd("DOU0002", true))[0]

	l.stop()
	if r := waitResult(t, inFlight); r.Outcome != OutcomeUncertain {
		t.Errorf("in-flight command on stop = %v, want uncertain", r.Outcome)
	}
	if r := waitResult(t, waiting); r.Outcome != OutcomeError {
		t.Errorf("never-sent command on stop = %v, want error", r.Outcome)
	}
	if r := waitResult(t, l.Submit(lightCmd("DOU0001", true))[0]); r.Outcome != OutcomeError {
		t.Errorf("submission after stop = %v, want error", r.Outcome)
	}
}

func TestLane_StopGivesMergedSubmissionsOneStamp(t *testing.T) {
	g := newFakeGate(t)
	g.SetLight(tCLU, "DOU0001", false)
	g.SetLight(tCLU, "DOU0002", false)
	g.HangAll(fakeReadPath)
	l := newTestLane(t, g, readLane, nil)
	l.Submit(lightRead("DOU0001"))
	g.WaitForRequests(t, fakeReadPath, 1)
	merged := []<-chan laneResult{l.Submit(lightRead("DOU0002"))[0], l.Submit(lightRead("DOU0002"))[0]}

	l.stop()
	r1, r2 := waitResult(t, merged[0]), waitResult(t, merged[1])
	if r1.Outcome != OutcomeFailed || r2.Outcome != OutcomeFailed {
		t.Errorf("outcomes %v, %v; want failed", r1.Outcome, r2.Outcome)
	}
	if r1.Stamp == 0 || r1.Stamp != r2.Stamp {
		t.Errorf("stamps %d, %d; want one stamp for the entry", r1.Stamp, r2.Stamp)
	}
}

func TestLane_CommandThatCannotBeSentIsError(t *testing.T) {
	g := newFakeGate(t)
	g.SetThermo(tCLU, "THE0002", 20, 21, 0, 0)
	l := newTestLane(t, g, writeLane, nil)
	cmd := ReqObject{Clu: tCLU, Id: "THE0002", Kind: "Thermo", Cmd: "SET", Thermo: &ThermoPayload{TempSetpoint: ptr(math.NaN())}}
	r := waitResult(t, l.Submit(cmd)[0])
	if r.Outcome != OutcomeError || r.Err == nil {
		t.Errorf("outcome %v (%v), want error: the command was never sent", r.Outcome, r.Err)
	}
	if n := g.RequestCount(""); n != 0 {
		t.Errorf("fake GATE received %d requests", n)
	}
}
