package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brutella/hap/characteristic"
)

// fakeInflux records the write requests it receives.
type fakeInflux struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []fakeInfluxReq
}

type fakeInfluxReq struct {
	Path, Query, Auth, ContentType string
	Lines                          []string
}

func newFakeInflux(t *testing.T) *fakeInflux {
	f := &fakeInflux{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, fakeInfluxReq{
			Path: r.URL.Path, Query: r.URL.RawQuery, Auth: r.Header.Get("Authorization"),
			ContentType: r.Header.Get("Content-Type"),
			Lines:       strings.Split(strings.TrimRight(string(b), "\n"), "\n"),
		})
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeInflux) requests() []fakeInfluxReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeInfluxReq(nil), f.reqs...)
}

func (f *fakeInflux) lines() []string {
	var out []string
	for _, r := range f.requests() {
		out = append(out, r.Lines...)
	}
	return out
}

var testInfluxCfg = InfluxConfig{Enabled: true, Token: "tok", Org: "home org", Bucket: "metrics"}

func TestInflux_LineFormat(t *testing.T) {
	at := time.UnixMilli(1700000000123)
	var got []string
	w := newInfluxWriter(InfluxConfig{}, func(string, ...any) {})
	m := influxMetrics{w: w}

	m.Request(RequestEvent{Lane: writeLane, Time: at, Items: 1, Clus: 1, Bytes: 142, HTTP: 87 * time.Millisecond,
		QueueWait: 3 * time.Millisecond, Success: true, Clu: "CLU_012abcde", Id: "DOU0001"})
	m.Request(RequestEvent{Lane: readLane, Time: at, Items: 30, Clus: 2, Bytes: 2048, HTTP: 412 * time.Millisecond,
		QueueWait: 1500 * time.Microsecond, Overlap: true})
	m.Command(CommandEvent{Time: at, Clu: "CLU_012abcde", Id: "DOU0001", Outcome: OutcomeOK,
		QueueWait: 3 * time.Millisecond, HTTP: 87 * time.Millisecond, Total: 90 * time.Millisecond})
	m.Refresh(RefreshEvent{Time: at, Counts: refreshCounts{Objects: 60, Applied: 57, Changed: 2, Missing: 1, Failed: 2, Duration: 950 * time.Millisecond}})
	close(w.lines)
	for l := range w.lines {
		got = append(got, l)
	}

	want := []string{
		"grengate_command,clu_id=CLU_012abcde,component=gate_broker,object_id=DOU0001,operation=flush duration_ms=87i,object_count=1i,clu_count=1i,request_bytes=142i,success=1i,queue_wait_ms=3i,overlap=0i 1700000000123",
		"grengate_refresh,component=gate_broker,operation=flush duration_ms=412i,object_count=30i,clu_count=2i,request_bytes=2048i,success=0i,queue_wait_ms=1i,overlap=1i 1700000000123",
		"grengate_command,clu_id=CLU_012abcde,component=command,object_id=DOU0001,operation=command_endtoend duration_ms=90i,queue_wait_ms=3i,http_ms=87i 1700000000123",
		"grengate_refresh,component=grenton_set,operation=refresh_cycle duration_ms=950i,object_count=60i,changed_objects=2i,skipped_objects=0i,discarded_objects=0i,missing_objects=1i,failed_objects=2i 1700000000123",
	}
	if len(got) != len(want) {
		t.Fatalf("lines = %d, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d\n got: %s\nwant: %s", i, got[i], want[i])
		}
	}
}

func TestInflux_TagEscaping(t *testing.T) {
	line := influxLine("my measurement,x", map[string]string{"clu id": "CLU a,b=c", "empty": ""},
		[]influxField{{"field key", 5}}, time.UnixMilli(1))
	want := `my\ measurement\,x,clu\ id=CLU\ a\,b\=c field\ key=5i 1`
	if line != want {
		t.Errorf("\n got: %s\nwant: %s", line, want)
	}
}

func TestInflux_WritesToEndpoint(t *testing.T) {
	f := newFakeInflux(t)
	cfg := testInfluxCfg
	cfg.URL = f.srv.URL + "/"
	w := newInfluxWriter(cfg, func(string, ...any) {})
	w.start()
	m := influxMetrics{w: w}
	m.Refresh(RefreshEvent{Time: time.UnixMilli(5), Counts: refreshCounts{Objects: 1, Applied: 1}})
	m.Command(CommandEvent{Time: time.UnixMilli(6), Clu: "CLU_1", Id: "DOU0001"})
	w.close()

	reqs := f.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(reqs))
	}
	r := reqs[0]
	if r.Path != "/api/v2/write" {
		t.Errorf("path = %q", r.Path)
	}
	if r.Query != "bucket=metrics&org=home+org&precision=ms" {
		t.Errorf("query = %q", r.Query)
	}
	if r.Auth != "Token tok" {
		t.Errorf("Authorization = %q", r.Auth)
	}
	if !strings.HasPrefix(r.ContentType, "text/plain") {
		t.Errorf("Content-Type = %q", r.ContentType)
	}
	if len(r.Lines) != 2 || !strings.HasPrefix(r.Lines[0], "grengate_refresh,") || !strings.HasPrefix(r.Lines[1], "grengate_command,") {
		t.Errorf("lines = %q", r.Lines)
	}
	if w.sent.Load() != 2 || w.dropped.Load() != 0 {
		t.Errorf("sent %d dropped %d", w.sent.Load(), w.dropped.Load())
	}
}

func TestInflux_EndpointDownDropsAndNeverBlocks(t *testing.T) {
	logs := captureLog(t)
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close() // nothing listens any more
	cfg := testInfluxCfg
	cfg.URL = down.URL
	gs := &GrentonSet{}
	w := newInfluxWriter(cfg, gs.Logf)
	w.batch = 10
	w.start()

	start := time.Now()
	for i := 0; i < 3*influxBufferLines; i++ {
		w.write("grengate_refresh,component=x f=1i 1")
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("writes took %v with the endpoint down", d)
	}
	w.close()
	if w.sent.Load() != 0 || w.dropped.Load() != 3*influxBufferLines {
		t.Errorf("sent %d, dropped %d; want all %d dropped", w.sent.Load(), w.dropped.Load(), 3*influxBufferLines)
	}
	if n := strings.Count(logs.String(), "InfluxDB write failed"); n != 1 {
		t.Errorf("failure logged %d times, want once per outage:\n%s", n, logs.String())
	}
}

func TestInflux_FullBufferDropsInsteadOfBlocking(t *testing.T) {
	w := newInfluxWriter(testInfluxCfg, func(string, ...any) {}) // not started: nothing drains
	for i := 0; i < influxBufferLines+7; i++ {
		w.write("x f=1i 1")
	}
	if got := w.dropped.Load(); got != 7 {
		t.Errorf("dropped = %d, want 7", got)
	}
}

// Every point written through a running GrentonSet matches the schema of concept 3.8.
func TestInflux_PointsFromGrentonSetMatchSchema(t *testing.T) {
	f := newFakeInflux(t)
	clu := lightsClu(2)
	gs, _ := newTestSetWith(t, func(gs *GrentonSet) {
		gs.InfluxDB = testInfluxCfg
		gs.InfluxDB.URL = f.srv.URL
	}, clu)
	gs.Refresh()
	syncHomeKit(t, gs) // hap characteristics are not synchronised: no HomeKit write during a sync
	hkWrite(findC(t, clu.Lights[0].HkAccessory(), characteristic.TypeOn), true)
	gs.Stop() // flushes the buffer

	tagSets := map[string]string{
		"grengate_refresh,component=gate_broker,operation=flush":         "duration_ms object_count clu_count request_bytes success queue_wait_ms overlap",
		"grengate_command,component=gate_broker,operation=flush":         "duration_ms object_count clu_count request_bytes success queue_wait_ms overlap",
		"grengate_command,component=command,operation=command_endtoend":  "duration_ms queue_wait_ms http_ms",
		"grengate_refresh,component=grenton_set,operation=refresh_cycle": "duration_ms object_count changed_objects skipped_objects discarded_objects missing_objects failed_objects",
	}
	lineRe := regexp.MustCompile(`^([a-z_]+)((?:,[a-z_]+=[^ ,]+)+) ((?:[a-z_]+=-?\d+i,?)+) (\d+)$`)
	seen := map[string]int{}
	for _, l := range f.lines() {
		m := lineRe.FindStringSubmatch(l)
		if m == nil {
			t.Errorf("line does not match the schema: %s", l)
			continue
		}
		var tags, ids []string
		for _, kv := range strings.Split(m[2][1:], ",") {
			k := strings.SplitN(kv, "=", 2)[0]
			if k == "clu_id" || k == "object_id" {
				ids = append(ids, k)
				continue
			}
			tags = append(tags, kv)
		}
		key := m[1] + "," + strings.Join(tags, ",")
		wantFields, ok := tagSets[key]
		if !ok {
			t.Errorf("unexpected point %s", l)
			continue
		}
		var fields []string
		for _, kv := range strings.Split(m[3], ",") {
			fields = append(fields, strings.SplitN(kv, "=", 2)[0])
		}
		if strings.Join(fields, " ") != wantFields {
			t.Errorf("%s: fields %v, want %s", key, fields, wantFields)
		}
		if strings.Contains(key, "command_endtoend") && len(ids) != 2 {
			t.Errorf("command point without clu_id/object_id: %s", l)
		}
		seen[key]++
	}
	for key := range tagSets {
		if seen[key] == 0 {
			t.Errorf("no %s point written", key)
		}
	}
}

func TestConfig_InfluxDBBlock(t *testing.T) {
	path := t.TempDir() + "/config.json"
	cfg := `{"FreshInSeconds": 5, "InfluxDB": {"Enabled": true, "URL": "http://influx:8086", "Token": "t", "Org": "o", "Bucket": "b"},
		"Clus":[{"Id":"CLU_012abcde","Lights":[{"Id":1,"Kind":"DOU"}]}]}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	gs := &GrentonSet{}
	if err := gs.Config(path); err != nil {
		t.Fatalf("Config: %v", err)
	}
	want := InfluxConfig{Enabled: true, URL: "http://influx:8086", Token: "t", Org: "o", Bucket: "b"}
	if gs.InfluxDB != want {
		t.Errorf("InfluxDB = %+v", gs.InfluxDB)
	}

	gs2 := &GrentonSet{}
	path2 := t.TempDir() + "/config.json"
	if err := os.WriteFile(path2, []byte(`{"Clus":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := gs2.Config(path2); err != nil || gs2.InfluxDB.Enabled {
		t.Errorf("InfluxDB without a block: %+v, %v; want disabled", gs2.InfluxDB, err)
	}
}
