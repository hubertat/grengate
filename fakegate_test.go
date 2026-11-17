package main

// Fake Grenton GATE for tests.
//
// It is an httptest server with a read path (fakeReadPath) and a set path
// (fakeSetPath), a device table keyed by Clu+Id (case-insensitive) and a log
// of every request. Payload field names are the ones used by grenton/*.lua:
//
//	Light         {State bool}
//	Thermo        {TempMin, TempMax, TempTarget, TempHoliday, TempSetpoint, TempCurrent float64; Mode, State int}
//	Shutter       {MaxTime, State int}  (State: 0 stopped, 1 up, 2 down)
//	MotionSensor  {State bool}
//
// Typical use:
//
//	g := newFakeGate(t)                          // closed automatically at test end
//	g.SetLight("CLU_012abcde", "DOU0001", true)  // also SetThermo, SetShutter, SetMotion, SetDevice
//	gs, g := newTestSet(t, clu)                  // see helpers_test.go: GrentonSet wired to a fake GATE
//	g.AddDevicesFrom(gs)                         // register every device of a GrentonSet with default state
//
// Requests are processed on arrival (state is read or written and the response
// is built at that moment); holds, delays and hangs only postpone the HTTP
// response. A held write therefore already took effect on the fake GATE, as an
// uncertain outcome would on the real one. Failures by status and broken bodies
// are answered instead of processing: the state is not touched.
//
// Reads answer with an array of {Clu, Id, Kind, <Kind>: payload}, in request
// order, as read-script.lua does. A single object posted to the read path
// yields "[]" (the script loops with ipairs). Devices that are unknown, omitted
// (Omit) or have no payload (DropPayload) are handled as described on the methods.
//
// Writes answer in one of two styles (SetWriteStyle):
//
//	fakeOldStyle  update-script.lua today: a single object {Clu, Id, Kind, <Kind>: read-back payload};
//	              an unknown device or a FailItem device gives "{}". Array bodies are processed
//	              item by item and answered with an array of such objects (the old script would
//	              not do that; it is only here so tests can probe).
//	fakeNewStyle  concept 3.7: answer has the shape of the request, object in -> ack object out,
//	              array in -> array of acks in request order. Ack = {Clu, Id, Kind} plus "Error"
//	              when the item failed (unknown device, FailItem). A thermostat item sets only
//	              the fields present in it.
//
// Controlling behaviour (path is fakeReadPath, fakeSetPath or "" for both):
//
//	g.HoldNext(path) *heldRequest   next request on path: response is held until h.Release() / h.ReleaseWithStatus(code);
//	                                h.Wait() blocks until the request arrived and returns it
//	g.HangAll(path)                 every request on path is held until g.ReleaseAll() or Close()
//	g.Delay(path, d)                every response on path is delayed by d
//	g.FailNext(path, status, n)     next n requests answered with HTTP status (n < 0: all), not processed
//	g.BreakBodyNext(path, n)        next n requests answered 200 with a body that is not valid JSON
//	g.ClearRules()                  drops delays, pending holds/failures and releases held requests
//
// Device manipulation: Omit/Unomit (left out of read responses), DropPayload /
// DropField (invalid payloads), FailItem / ClearFailItem (per-item error on
// writes and an empty read item), Payload (inspect state), SetShutter with maxTime 0.
//
// Inspection: g.Requests(path), g.RequestCount(path), g.WaitForRequests(t, path, n)
// (polls with a deadline). A recorded request has Body (raw), IsArray and Items
// (decoded objects; one element for a single object).
//
// Close() (registered with t.Cleanup by newFakeGate) releases every held
// request first, so no handler goroutine is left behind.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	fakeReadPath = "/read"
	fakeSetPath  = "/set"
)

type fakeWriteStyle int

const (
	fakeOldStyle fakeWriteStyle = iota
	fakeNewStyle
)

// fakeGateRequest is one recorded request.
type fakeGateRequest struct {
	Path    string
	Method  string
	Body    []byte
	IsArray bool
	Items   []map[string]any
}

type fakeDevice struct {
	Clu, Id, Kind string
	Payload       map[string]any // nil: device has no payload object
	omitted       bool
	failMsg       string
}

// heldRequest is a handle for a request held by HoldNext.
type heldRequest struct {
	arrived chan struct{}
	release chan int // status code to answer with; 0 = normal answer
	once    sync.Once
	req     fakeGateRequest
}

type fakeRule struct {
	path   string
	times  int // remaining uses, < 0 unlimited
	hold   *heldRequest
	hang   bool
	status int
	broken bool
}

type fakeGate struct {
	t   testing.TB
	srv *httptest.Server

	mu         sync.Mutex
	devices    map[string]*fakeDevice
	requests   []fakeGateRequest
	style      fakeWriteStyle
	rules      []*fakeRule
	delays     map[string]time.Duration
	done       chan struct{}
	closeOnce  sync.Once
	hangCh     chan struct{} // closed by ReleaseAll
	hangClosed bool
}

func newFakeGate(t testing.TB) *fakeGate {
	g := &fakeGate{
		t:       t,
		devices: map[string]*fakeDevice{},
		delays:  map[string]time.Duration{},
		done:    make(chan struct{}),
		hangCh:  make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc(fakeReadPath, g.handle)
	mux.HandleFunc(fakeSetPath, g.handle)
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.Close)
	return g
}

// URL returns the base URL (no trailing slash); use it as GrentonSet.Host.
func (g *fakeGate) URL() string { return g.srv.URL }

// Close releases every held request and shuts the server down. Safe to call twice.
func (g *fakeGate) Close() {
	g.closeOnce.Do(func() {
		close(g.done)
		g.srv.CloseClientConnections()
		g.srv.Close()
	})
}

func deviceKey(clu, id string) string { return strings.ToLower(clu) + "|" + strings.ToLower(id) }

// SetWriteStyle selects how writes are answered.
func (g *fakeGate) SetWriteStyle(s fakeWriteStyle) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.style = s
}

// SetDevice creates or replaces a device with the given payload (nil = no payload object).
func (g *fakeGate) SetDevice(clu, id, kind string, payload map[string]any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.devices[deviceKey(clu, id)] = &fakeDevice{Clu: clu, Id: id, Kind: kind, Payload: payload}
}

func (g *fakeGate) SetLight(clu, id string, on bool) {
	g.SetDevice(clu, id, "Light", map[string]any{"State": on})
}

func (g *fakeGate) SetMotion(clu, id string, on bool) {
	g.SetDevice(clu, id, "MotionSensor", map[string]any{"State": on})
}

// SetShutter registers a shutter; maxTime 0 gives the invalid "no travel time" payload.
func (g *fakeGate) SetShutter(clu, id string, maxTime, state int) {
	g.SetDevice(clu, id, "Shutter", map[string]any{"MaxTime": maxTime, "State": state})
}

// SetThermo registers a thermostat with all fields of the read script.
func (g *fakeGate) SetThermo(clu, id string, current, setpoint float64, mode, state int) {
	g.SetDevice(clu, id, "Thermo", map[string]any{
		"TempMin": 5.0, "TempMax": 30.0, "TempTarget": setpoint, "TempHoliday": 10.0,
		"TempSetpoint": setpoint, "TempCurrent": current, "Mode": mode, "State": state,
	})
}

// AddDevicesFrom registers every device of gs with a default state, unless already registered.
func (g *fakeGate) AddDevicesFrom(gs *GrentonSet) {
	add := func(clu *Clu, co *CluObject, def func()) {
		g.mu.Lock()
		_, ok := g.devices[deviceKey(clu.Id, co.GetMixedId())]
		g.mu.Unlock()
		if !ok {
			def()
		}
	}
	for _, clu := range gs.Clus {
		for _, l := range clu.Lights {
			add(clu, &l.CluObject, func() { g.SetLight(clu.Id, l.GetMixedId(), false) })
		}
		for _, th := range clu.Therms {
			add(clu, &th.CluObject, func() { g.SetThermo(clu.Id, th.GetMixedId(), 20, 21, 0, 0) })
		}
		for _, sh := range clu.Shutters {
			add(clu, &sh.CluObject, func() { g.SetShutter(clu.Id, sh.GetMixedId(), 20000, 0) })
		}
		for _, ms := range clu.MotionSensors {
			add(clu, &ms.CluObject, func() { g.SetMotion(clu.Id, ms.GetMixedId(), false) })
		}
	}
}

func (g *fakeGate) with(clu, id string, fn func(d *fakeDevice)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	d, ok := g.devices[deviceKey(clu, id)]
	if !ok {
		g.t.Fatalf("fakeGate: device %s|%s not registered", clu, id)
		return
	}
	fn(d)
}

// Omit leaves the device out of read responses until Unomit.
func (g *fakeGate) Omit(clu, id string)   { g.with(clu, id, func(d *fakeDevice) { d.omitted = true }) }
func (g *fakeGate) Unomit(clu, id string) { g.with(clu, id, func(d *fakeDevice) { d.omitted = false }) }

// DropPayload makes read items of the device come without the payload object.
func (g *fakeGate) DropPayload(clu, id string) {
	g.with(clu, id, func(d *fakeDevice) { d.Payload = nil })
}

// DropField removes one payload field (e.g. "TempCurrent") from the device.
func (g *fakeGate) DropField(clu, id, field string) {
	g.with(clu, id, func(d *fakeDevice) { delete(d.Payload, field) })
}

// FailItem makes writes to the device fail per item with msg (new style: ack with Error;
// old style: empty object). Reads of the device are unaffected.
func (g *fakeGate) FailItem(clu, id, msg string) {
	g.with(clu, id, func(d *fakeDevice) { d.failMsg = msg })
}
func (g *fakeGate) ClearFailItem(clu, id string) {
	g.with(clu, id, func(d *fakeDevice) { d.failMsg = "" })
}

// Payload returns a copy of the device payload.
func (g *fakeGate) Payload(clu, id string) map[string]any {
	var out map[string]any
	g.with(clu, id, func(d *fakeDevice) { out = copyMap(d.Payload) })
	return out
}

func copyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Requests returns the recorded requests of a path ("" = all) in arrival order.
func (g *fakeGate) Requests(path string) []fakeGateRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []fakeGateRequest
	for _, r := range g.requests {
		if path == "" || r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func (g *fakeGate) RequestCount(path string) int { return len(g.Requests(path)) }

// WaitForRequests polls until at least n requests were recorded on path (2 s deadline).
func (g *fakeGate) WaitForRequests(t testing.TB, path string, n int) []fakeGateRequest {
	t.Helper()
	var reqs []fakeGateRequest
	eventually(t, 2*time.Second, func() bool {
		reqs = g.Requests(path)
		return len(reqs) >= n
	}, "fake GATE to receive %d requests on %q", n, path)
	return reqs
}

func (g *fakeGate) addRule(r *fakeRule) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rules = append(g.rules, r)
}

// HoldNext holds the response of the next request on path.
func (g *fakeGate) HoldNext(path string) *heldRequest {
	h := &heldRequest{arrived: make(chan struct{}), release: make(chan int, 1)}
	g.addRule(&fakeRule{path: path, times: 1, hold: h})
	return h
}

// HangAll holds every request on path until ReleaseAll or Close.
func (g *fakeGate) HangAll(path string) { g.addRule(&fakeRule{path: path, times: -1, hang: true}) }

// ReleaseAll releases requests held by HangAll (and stops future hangs from holding).
func (g *fakeGate) ReleaseAll() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.releaseAllLocked()
}

func (g *fakeGate) releaseAllLocked() {
	if !g.hangClosed {
		g.hangClosed = true
		close(g.hangCh)
	}
}

// Delay delays every response on path by d (0 clears).
func (g *fakeGate) Delay(path string, d time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.delays[path] = d
}

// FailNext answers the next n requests on path with status (n < 0: all of them).
func (g *fakeGate) FailNext(path string, status, n int) {
	g.addRule(&fakeRule{path: path, times: n, status: status})
}

// BreakBodyNext answers the next n requests with status 200 and a body that is not JSON.
func (g *fakeGate) BreakBodyNext(path string, n int) {
	g.addRule(&fakeRule{path: path, times: n, broken: true})
}

// ClearRules drops delays and pending rules; held requests are released.
func (g *fakeGate) ClearRules() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, r := range g.rules {
		if r.hold != nil {
			r.hold.Release()
		}
	}
	g.rules = nil
	g.delays = map[string]time.Duration{}
	g.releaseAllLocked()
}

// Wait blocks until the held request arrived (2 s deadline) and returns it.
func (h *heldRequest) Wait(t testing.TB) fakeGateRequest {
	t.Helper()
	select {
	case <-h.arrived:
		return h.req
	case <-time.After(2 * time.Second):
		t.Fatalf("fakeGate: held request did not arrive")
		return fakeGateRequest{}
	}
}

// Arrived is closed once the held request reached the fake GATE.
func (h *heldRequest) Arrived() <-chan struct{} { return h.arrived }

// Release lets the held request be answered normally.
func (h *heldRequest) Release() { h.ReleaseWithStatus(0) }

// ReleaseWithStatus answers the held request with an HTTP status instead (0 = normal answer).
func (h *heldRequest) ReleaseWithStatus(code int) {
	h.once.Do(func() { h.release <- code })
}

func (g *fakeGate) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	rec := fakeGateRequest{Path: r.URL.Path, Method: r.Method, Body: body}
	var raw any
	if err := json.Unmarshal(body, &raw); err == nil {
		switch v := raw.(type) {
		case []any:
			rec.IsArray = true
			for _, it := range v {
				if m, ok := it.(map[string]any); ok {
					rec.Items = append(rec.Items, m)
				}
			}
		case map[string]any:
			rec.Items = []map[string]any{v}
		}
	}

	g.mu.Lock()
	rule := g.takeRuleLocked(rec.Path)
	delay := g.delays[rec.Path] + g.delays[""]
	if rule != nil && rule.hold != nil {
		rule.hold.req = rec
	}
	var resp any
	process := rule == nil || rule.hold != nil || rule.hang
	if process {
		resp = g.processLocked(rec)
	}
	g.requests = append(g.requests, rec)
	g.mu.Unlock()
	if rule != nil && rule.hold != nil {
		close(rule.hold.arrived)
	}

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-g.done:
			return
		case <-r.Context().Done():
			return
		}
	}

	status := 0
	switch {
	case rule != nil && rule.hold != nil:
		select {
		case status = <-rule.hold.release:
		case <-g.done:
			return
		case <-r.Context().Done():
			return
		}
	case rule != nil && rule.hang:
		select {
		case <-g.hangCh:
		case <-g.done:
			return
		case <-r.Context().Done():
			return
		}
	case rule != nil && rule.status != 0:
		status = rule.status
	case rule != nil && rule.broken:
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Clu": [broken`))
		return
	}
	if status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// takeRuleLocked returns the first rule matching path and consumes one use.
func (g *fakeGate) takeRuleLocked(path string) *fakeRule {
	for i, r := range g.rules {
		if r.path != "" && r.path != path {
			continue
		}
		if r.times == 0 {
			continue
		}
		if r.times > 0 {
			r.times--
		}
		if r.times == 0 {
			g.rules = append(g.rules[:i:i], g.rules[i+1:]...)
		}
		return r
	}
	return nil
}

func (g *fakeGate) processLocked(rec fakeGateRequest) any {
	if rec.Path == fakeReadPath {
		return g.readLocked(rec)
	}
	return g.writeLocked(rec)
}

func itemKey(item map[string]any) (clu, id, kind string) {
	clu, _ = item["Clu"].(string)
	id, _ = item["Id"].(string)
	kind, _ = item["Kind"].(string)
	return
}

func (g *fakeGate) readLocked(rec fakeGateRequest) any {
	out := []any{}
	if !rec.IsArray {
		return out // read script iterates with ipairs: a single object reads nothing
	}
	for _, item := range rec.Items {
		clu, id, kind := itemKey(item)
		d, ok := g.devices[deviceKey(clu, id)]
		if !ok || d.omitted {
			continue
		}
		entry := map[string]any{"Clu": clu, "Id": id, "Kind": kind}
		if d.Payload != nil {
			entry[d.Kind] = copyMap(d.Payload)
		}
		out = append(out, entry)
	}
	return out
}

func (g *fakeGate) writeLocked(rec fakeGateRequest) any {
	acks := make([]any, 0, len(rec.Items))
	for _, item := range rec.Items {
		acks = append(acks, g.writeItemLocked(item))
	}
	if rec.IsArray {
		return acks
	}
	if len(acks) == 0 {
		return map[string]any{}
	}
	return acks[0]
}

func (g *fakeGate) writeItemLocked(item map[string]any) any {
	clu, id, kind := itemKey(item)
	d, ok := g.devices[deviceKey(clu, id)]
	ack := map[string]any{"Clu": clu, "Id": id, "Kind": kind}
	if !ok || d.failMsg != "" {
		if g.style == fakeNewStyle {
			msg := "unknown device"
			if ok {
				msg = d.failMsg
			}
			ack["Error"] = msg
			return ack
		}
		return map[string]any{}
	}

	if d.Payload == nil {
		d.Payload = map[string]any{}
	}
	payload, _ := item[kind].(map[string]any)
	switch kind {
	case "Light":
		if v, ok := payload["State"]; ok {
			d.Payload["State"] = v
		}
	case "Thermo":
		fields := []string{"TempSetpoint", "State", "Mode"}
		for _, f := range fields {
			if v, ok := payload[f]; ok {
				d.Payload[f] = v
			}
		}
	case "Shutter":
		cmd, _ := item["Cmd"].(string)
		switch cmd {
		case "MOVEUP":
			d.Payload["State"] = 1
		case "MOVEDOWN":
			d.Payload["State"] = 2
		case "STOP":
			d.Payload["State"] = 0
		}
	case "MotionSensor":
		if v, ok := payload["State"]; ok {
			d.Payload["State"] = v
		}
	}

	if g.style == fakeNewStyle {
		return ack
	}
	ack[kind] = copyMap(d.Payload)
	return ack
}

// ---------------------------------------------------------------------------
// Tests of the fake itself
// ---------------------------------------------------------------------------

const (
	tCLU = "CLU_012abcde"
	tLID = "DOU0001"
)

func postJSON(t testing.TB, url, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func decodeJSON(t testing.TB, s string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), v); err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
}

func lightItem(clu, id string, on bool) string {
	b, _ := json.Marshal(map[string]any{"Clu": clu, "Id": id, "Kind": "Light", "Cmd": "SET", "Light": map[string]any{"State": on}})
	return string(b)
}

func TestFakeGate_ReadReturnsArrayWithPayloads(t *testing.T) {
	g := newFakeGate(t)
	g.SetLight(tCLU, tLID, true)
	g.SetThermo(tCLU, "THE0002", 19.5, 21, 1, 1)

	code, body := postJSON(t, g.URL()+fakeReadPath,
		`[{"Clu":"`+tCLU+`","Id":"`+tLID+`","Kind":"Light"},{"Clu":"`+tCLU+`","Id":"THE0002","Kind":"Thermo"},{"Clu":"`+tCLU+`","Id":"DOU0099","Kind":"Light"}]`)
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	var got []map[string]any
	decodeJSON(t, body, &got)
	if len(got) != 2 {
		t.Fatalf("got %d items, want 2 (unknown device skipped): %s", len(got), body)
	}
	if got[0]["Light"].(map[string]any)["State"] != true {
		t.Errorf("light payload = %v", got[0])
	}
	if got[1]["Thermo"].(map[string]any)["TempCurrent"] != 19.5 {
		t.Errorf("thermo payload = %v", got[1])
	}
	reqs := g.Requests(fakeReadPath)
	if len(reqs) != 1 || !reqs[0].IsArray || len(reqs[0].Items) != 3 {
		t.Errorf("recorded = %+v", reqs)
	}
}

func TestFakeGate_ReadOfSingleObjectIsEmpty(t *testing.T) {
	g := newFakeGate(t)
	g.SetLight(tCLU, tLID, true)
	_, body := postJSON(t, g.URL()+fakeReadPath, lightItem(tCLU, tLID, false))
	if strings.TrimSpace(body) != "[]" {
		t.Errorf("body = %q, want []", body)
	}
}

func TestFakeGate_WriteOldStyleAnswersObjectWithReadBack(t *testing.T) {
	g := newFakeGate(t)
	g.SetWriteStyle(fakeOldStyle)
	g.SetLight(tCLU, tLID, false)

	_, body := postJSON(t, g.URL()+fakeSetPath, lightItem(tCLU, tLID, true))
	var got map[string]any
	decodeJSON(t, body, &got)
	if got["Id"] != tLID || got["Kind"] != "Light" || got["Light"].(map[string]any)["State"] != true {
		t.Errorf("old style answer = %s", body)
	}
	if g.Payload(tCLU, tLID)["State"] != true {
		t.Error("state not applied")
	}
	reqs := g.Requests(fakeSetPath)
	if len(reqs) != 1 || reqs[0].IsArray {
		t.Errorf("recorded = %+v, want one single-object request", reqs)
	}
}

func TestFakeGate_WriteNewStyleMirrorsRequestShape(t *testing.T) {
	g := newFakeGate(t)
	g.SetWriteStyle(fakeNewStyle)
	g.SetLight(tCLU, tLID, false)
	g.SetLight(tCLU, "DOU0002", false)

	// object in, ack object out
	_, body := postJSON(t, g.URL()+fakeSetPath, lightItem(tCLU, tLID, true))
	var obj map[string]any
	decodeJSON(t, body, &obj)
	if obj["Id"] != tLID || obj["Kind"] != "Light" || obj["Light"] != nil || obj["Error"] != nil {
		t.Errorf("single ack = %s", body)
	}

	// array in, array of acks in the same order, per-item error injected
	g.FailItem(tCLU, "DOU0002", "boom")
	_, body = postJSON(t, g.URL()+fakeSetPath,
		"["+lightItem(tCLU, "DOU0002", true)+","+lightItem(tCLU, "DOU0777", true)+","+lightItem(tCLU, tLID, false)+"]")
	var arr []map[string]any
	decodeJSON(t, body, &arr)
	if len(arr) != 3 {
		t.Fatalf("acks = %s", body)
	}
	if arr[0]["Id"] != "DOU0002" || arr[0]["Error"] != "boom" {
		t.Errorf("ack 0 = %v", arr[0])
	}
	if arr[1]["Id"] != "DOU0777" || arr[1]["Error"] == nil {
		t.Errorf("ack 1 (unknown device) = %v", arr[1])
	}
	if arr[2]["Id"] != tLID || arr[2]["Error"] != nil {
		t.Errorf("ack 2 = %v", arr[2])
	}
	if g.Payload(tCLU, tLID)["State"] != false || g.Payload(tCLU, "DOU0002")["State"] != false {
		t.Error("state wrong: item 3 should apply, failed item must not")
	}
}

func TestFakeGate_NewStyleThermoSetsOnlyPresentFields(t *testing.T) {
	g := newFakeGate(t)
	g.SetWriteStyle(fakeNewStyle)
	g.SetThermo(tCLU, "THE0002", 19, 21, 0, 0)
	postJSON(t, g.URL()+fakeSetPath, `{"Clu":"`+tCLU+`","Id":"THE0002","Kind":"Thermo","Thermo":{"TempSetpoint":23.5}}`)
	p := g.Payload(tCLU, "THE0002")
	if p["TempSetpoint"] != 23.5 || p["State"] != 0 || p["Mode"] != 0 {
		t.Errorf("payload = %v", p)
	}
}

func TestFakeGate_ShutterCommands(t *testing.T) {
	g := newFakeGate(t)
	g.SetShutter(tCLU, "ROL0003", 20000, 0)
	for cmd, want := range map[string]int{"MOVEUP": 1, "MOVEDOWN": 2, "STOP": 0} {
		postJSON(t, g.URL()+fakeSetPath, `{"Clu":"`+tCLU+`","Id":"ROL0003","Kind":"Shutter","Cmd":"`+cmd+`"}`)
		if got := g.Payload(tCLU, "ROL0003")["State"]; got != want {
			t.Errorf("%s: State = %v, want %d", cmd, got, want)
		}
	}
}

func TestFakeGate_OmitAndInvalidPayloads(t *testing.T) {
	g := newFakeGate(t)
	g.SetLight(tCLU, tLID, true)
	g.SetLight(tCLU, "DOU0002", true)
	g.SetThermo(tCLU, "THE0003", 20, 21, 0, 0)
	g.SetShutter(tCLU, "ROL0004", 0, 0)
	query := `[{"Clu":"` + tCLU + `","Id":"DOU0001","Kind":"Light"},{"Clu":"` + tCLU + `","Id":"DOU0002","Kind":"Light"},{"Clu":"` + tCLU + `","Id":"THE0003","Kind":"Thermo"},{"Clu":"` + tCLU + `","Id":"ROL0004","Kind":"Shutter"}]`
	read := func() []map[string]any {
		_, body := postJSON(t, g.URL()+fakeReadPath, query)
		var got []map[string]any
		decodeJSON(t, body, &got)
		return got
	}

	g.Omit(tCLU, tLID)
	g.DropPayload(tCLU, "DOU0002")
	g.DropField(tCLU, "THE0003", "TempCurrent")
	got := read()
	if len(got) != 3 {
		t.Fatalf("items = %d, want 3 (one omitted)", len(got))
	}
	if got[0]["Id"] != "DOU0002" || got[0]["Light"] != nil {
		t.Errorf("payload-less item = %v", got[0])
	}
	if _, ok := got[1]["Thermo"].(map[string]any)["TempCurrent"]; ok {
		t.Errorf("TempCurrent still present: %v", got[1])
	}
	if mt := got[2]["Shutter"].(map[string]any)["MaxTime"]; mt != float64(0) {
		t.Errorf("MaxTime = %v, want 0", mt)
	}

	g.Unomit(tCLU, tLID)
	if len(read()) != 4 {
		t.Error("Unomit did not bring the device back")
	}
}

func TestFakeGate_HoldNextAndReleaseInChosenOrder(t *testing.T) {
	g := newFakeGate(t)
	g.SetLight(tCLU, tLID, true)
	h1 := g.HoldNext(fakeReadPath)
	h2 := g.HoldNext(fakeReadPath)

	type result struct {
		n    int
		code int
	}
	results := make(chan result, 2)
	send := func(n int) {
		code, _ := postJSON(t, g.URL()+fakeReadPath, `[{"Clu":"`+tCLU+`","Id":"`+tLID+`","Kind":"Light"}]`)
		results <- result{n, code}
	}
	go send(1)
	h1.Wait(t)
	go send(2)
	h2.Wait(t)

	select {
	case r := <-results:
		t.Fatalf("request %d answered while held", r.n)
	default:
	}

	h2.Release()
	if r := <-results; r.n != 2 || r.code != 200 {
		t.Errorf("first answer = %+v, want request 2 with 200", r)
	}
	h1.ReleaseWithStatus(503)
	if r := <-results; r.n != 1 || r.code != 503 {
		t.Errorf("second answer = %+v, want request 1 with 503", r)
	}

	// third request is not held
	if code, _ := postJSON(t, g.URL()+fakeReadPath, `[]`); code != 200 {
		t.Errorf("unheld request status = %d", code)
	}
}

func TestFakeGate_HangAllAndReleaseAll(t *testing.T) {
	g := newFakeGate(t)
	g.HangAll("")
	done := make(chan int, 2)
	go func() { c, _ := postJSON(t, g.URL()+fakeReadPath, `[]`); done <- c }()
	go func() { c, _ := postJSON(t, g.URL()+fakeSetPath, `{}`); done <- c }()
	g.WaitForRequests(t, "", 2)
	select {
	case <-done:
		t.Fatal("request answered while hanging")
	case <-time.After(50 * time.Millisecond):
	}
	g.ReleaseAll()
	for i := 0; i < 2; i++ {
		if c := <-done; c != 200 {
			t.Errorf("status %d after release", c)
		}
	}
}

func TestFakeGate_CloseReleasesHungRequests(t *testing.T) {
	g := newFakeGate(t)
	g.HangAll("")
	g.HoldNext(fakeSetPath)
	done := make(chan struct{}, 2)
	for _, p := range []string{fakeReadPath, fakeSetPath} {
		go func(p string) {
			resp, err := http.Post(g.URL()+p, "application/json", strings.NewReader(`[]`))
			if err == nil {
				resp.Body.Close()
			}
			done <- struct{}{}
		}(p)
	}
	g.WaitForRequests(t, "", 2)
	g.Close()
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("hung request not released by Close")
		}
	}
}

func TestFakeGate_FailAndBrokenBody(t *testing.T) {
	g := newFakeGate(t)
	g.SetLight(tCLU, tLID, true)
	g.FailNext(fakeReadPath, 500, 1)
	g.BreakBodyNext(fakeReadPath, 1)

	if code, _ := postJSON(t, g.URL()+fakeReadPath, `[]`); code != 500 {
		t.Errorf("status = %d, want 500", code)
	}
	code, body := postJSON(t, g.URL()+fakeReadPath, `[]`)
	var v any
	if code != 200 || json.Unmarshal([]byte(body), &v) == nil {
		t.Errorf("broken body: status %d body %q", code, body)
	}
	if code, body := postJSON(t, g.URL()+fakeReadPath, `[]`); code != 200 || strings.TrimSpace(body) != "[]" {
		t.Errorf("recovered answer: %d %q", code, body)
	}
	// the set path was not affected by read rules, and failed writes do not touch state
	g.FailNext(fakeSetPath, 502, -1)
	if code, _ := postJSON(t, g.URL()+fakeSetPath, lightItem(tCLU, tLID, false)); code != 502 {
		t.Errorf("set status = %d, want 502", code)
	}
	if g.Payload(tCLU, tLID)["State"] != true {
		t.Error("failed write changed state")
	}
	if g.RequestCount(fakeSetPath) != 1 {
		t.Errorf("failed request not recorded")
	}
}

func TestFakeGate_DelayDelaysResponse(t *testing.T) {
	g := newFakeGate(t)
	g.Delay(fakeReadPath, 80*time.Millisecond)
	start := time.Now()
	postJSON(t, g.URL()+fakeReadPath, `[]`)
	if d := time.Since(start); d < 80*time.Millisecond {
		t.Errorf("answered after %v, want >= 80ms", d)
	}
	start = time.Now()
	postJSON(t, g.URL()+fakeSetPath, `{}`)
	if d := time.Since(start); d > 70*time.Millisecond {
		t.Errorf("set path delayed by %v", d)
	}
}
