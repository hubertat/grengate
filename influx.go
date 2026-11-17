package main

// Optional InfluxDB v2 reporting (concept 3.8): line protocol posted to /api/v2/write over
// plain HTTP, without the client library. Points are buffered and sent in the background;
// when the buffer is full or the endpoint fails, points are dropped and counted, never
// waited for.
//
// Schema (all fields are integers and carry the "i" suffix):
//
//	grengate_command,component=gate_broker,operation=flush[,clu_id,object_id]  one write request
//	    duration_ms, object_count, clu_count, request_bytes, success, queue_wait_ms, overlap
//	grengate_command,component=command,operation=command_endtoend,clu_id,object_id  one command
//	    duration_ms (submit to result), queue_wait_ms (submit to sent), http_ms
//	grengate_refresh,component=gate_broker,operation=flush[,clu_id,object_id]  one read request
//	    same fields as the write flush
//	grengate_refresh,component=grenton_set,operation=refresh_cycle  one refresh
//	    duration_ms, object_count, changed_objects, skipped_objects, discarded_objects,
//	    missing_objects, failed_objects
//
// clu_id and object_id are set on flush points only when the request carried one object.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// InfluxConfig is the optional "InfluxDB" block of the config file. Off by default.
type InfluxConfig struct {
	Enabled bool
	URL     string // e.g. http://localhost:8086
	Token   string
	Org     string
	Bucket  string
}

const (
	influxBufferLines   = 2000
	influxBatchLines    = 500
	influxFlushInterval = 10 * time.Second
	influxTimeout       = 5 * time.Second
)

// influxWriter buffers lines and posts them in the background.
type influxWriter struct {
	writeURL string
	token    string
	client   *http.Client
	interval time.Duration
	batch    int
	logf     func(string, ...any)

	lines   chan string
	dropped atomic.Int64
	sent    atomic.Int64

	stop chan struct{}
	done chan struct{}
}

func newInfluxWriter(cfg InfluxConfig, logf func(string, ...any)) *influxWriter {
	q := url.Values{}
	q.Set("org", cfg.Org)
	q.Set("bucket", cfg.Bucket)
	q.Set("precision", "ms")
	return &influxWriter{
		writeURL: strings.TrimRight(cfg.URL, "/") + "/api/v2/write?" + q.Encode(),
		token:    cfg.Token,
		client:   &http.Client{Timeout: influxTimeout},
		interval: influxFlushInterval,
		batch:    influxBatchLines,
		logf:     logf,
		lines:    make(chan string, influxBufferLines),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

func (w *influxWriter) start() { go w.run() }

// close sends what is buffered (bounded by the HTTP timeout) and stops the writer.
func (w *influxWriter) close() {
	close(w.stop)
	<-w.done
}

// write queues one line. It never blocks: a full buffer drops the line.
func (w *influxWriter) write(line string) {
	select {
	case w.lines <- line:
	default:
		w.dropped.Add(1)
	}
}

func (w *influxWriter) run() {
	defer close(w.done)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	var pending []string
	failing := false
	flush := func() {
		if len(pending) == 0 {
			return
		}
		err := w.post(pending)
		switch {
		case err != nil:
			total := w.dropped.Add(int64(len(pending)))
			if !failing {
				w.logf("InfluxDB write failed, dropping points until it works again (%d dropped so far): %v", total, err)
			}
			failing = true
		default:
			w.sent.Add(int64(len(pending)))
			if failing {
				w.logf("InfluxDB write works again (%d points dropped so far)", w.dropped.Load())
			}
			failing = false
		}
		pending = nil
	}
	for {
		select {
		case l := <-w.lines:
			pending = append(pending, l)
			if len(pending) >= w.batch {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-w.stop:
			for len(w.lines) > 0 {
				pending = append(pending, <-w.lines)
			}
			flush()
			return
		}
	}
}

func (w *influxWriter) post(lines []string) error {
	body := strings.Join(lines, "\n") + "\n"
	ctx, cancel := context.WithTimeout(context.Background(), influxTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.writeURL, bytes.NewBufferString(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Token "+w.token)
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// influxMetrics turns metric events into line protocol points.
type influxMetrics struct {
	w *influxWriter
}

func (m influxMetrics) Request(e RequestEvent) {
	measurement := "grengate_refresh"
	if e.Lane == writeLane {
		measurement = "grengate_command"
	}
	tags := map[string]string{"component": "gate_broker", "operation": "flush"}
	if e.Items == 1 {
		tags["clu_id"], tags["object_id"] = e.Clu, e.Id
	}
	m.w.write(influxLine(measurement, tags, []influxField{
		{"duration_ms", e.HTTP.Milliseconds()},
		{"object_count", int64(e.Items)},
		{"clu_count", int64(e.Clus)},
		{"request_bytes", int64(e.Bytes)},
		{"success", boolInt(e.Success)},
		{"queue_wait_ms", e.QueueWait.Milliseconds()},
		{"overlap", boolInt(e.Overlap)},
	}, e.Time))
}

func (m influxMetrics) Command(e CommandEvent) {
	tags := map[string]string{"component": "command", "operation": "command_endtoend", "clu_id": e.Clu, "object_id": e.Id}
	m.w.write(influxLine("grengate_command", tags, []influxField{
		{"duration_ms", e.Total.Milliseconds()},
		{"queue_wait_ms", e.QueueWait.Milliseconds()},
		{"http_ms", e.HTTP.Milliseconds()},
	}, e.Time))
}

func (m influxMetrics) Refresh(e RefreshEvent) {
	c := e.Counts
	tags := map[string]string{"component": "grenton_set", "operation": "refresh_cycle"}
	m.w.write(influxLine("grengate_refresh", tags, []influxField{
		{"duration_ms", c.Duration.Milliseconds()},
		{"object_count", int64(c.Objects)},
		{"changed_objects", int64(c.Changed)},
		{"skipped_objects", int64(c.Skipped)},
		{"discarded_objects", int64(c.Discarded)},
		{"missing_objects", int64(c.Missing)},
		{"failed_objects", int64(c.Failed)},
	}, e.Time))
}

type influxField struct {
	key string
	val int64
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

var (
	influxMeasurementEscaper = strings.NewReplacer(`,`, `\,`, ` `, `\ `)
	influxKeyEscaper         = strings.NewReplacer(`,`, `\,`, `=`, `\=`, ` `, `\ `)
)

// influxLine formats one point: tags sorted by key (empty values left out, as line protocol
// does not allow them), integer fields with the "i" suffix, millisecond timestamp.
func influxLine(measurement string, tags map[string]string, fields []influxField, t time.Time) string {
	var b strings.Builder
	b.WriteString(influxMeasurementEscaper.Replace(measurement))
	keys := make([]string, 0, len(tags))
	for k, v := range tags {
		if v != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteByte(',')
		b.WriteString(influxKeyEscaper.Replace(k))
		b.WriteByte('=')
		b.WriteString(influxKeyEscaper.Replace(tags[k]))
	}
	for i, f := range fields {
		if i == 0 {
			b.WriteByte(' ')
		} else {
			b.WriteByte(',')
		}
		b.WriteString(influxKeyEscaper.Replace(f.key))
		b.WriteByte('=')
		b.WriteString(strconv.FormatInt(f.val, 10))
		b.WriteByte('i')
	}
	b.WriteByte(' ')
	b.WriteString(strconv.FormatInt(t.UnixMilli(), 10))
	return b.String()
}
