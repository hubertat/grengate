package main

import (
	"bytes"
	"log"
	"sync"
	"testing"
	"time"
)

// eventually polls cond until it is true or the deadline passes, then fails the test.
func eventually(t testing.TB, within time.Duration, cond func() bool, msg string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for "+msg, args...)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// syncBuffer is a bytes.Buffer safe for concurrent log writes and reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLog redirects the standard logger into a buffer until the test ends.
func captureLog(t testing.TB) *syncBuffer {
	t.Helper()
	b := &syncBuffer{}
	prev := log.Writer()
	log.SetOutput(b)
	t.Cleanup(func() { log.SetOutput(prev) })
	return b
}

// newTestSet builds a GrentonSet wired to a fresh fake GATE without a config file.
// The devices of the given CLUs are initialised (HomeKit accessories created) and
// registered with the fake GATE in their default state. Workers are stopped at test end.
func newTestSet(t testing.TB, clus ...*Clu) (*GrentonSet, *fakeGate) {
	t.Helper()
	return newTestSetWith(t, nil, clus...)
}

// newTestSetWith is newTestSet with a hook that adjusts the GrentonSet (QueryLimit,
// requestTimeout, ...) before its devices are initialised and its workers started.
func newTestSetWith(t testing.TB, configure func(gs *GrentonSet), clus ...*Clu) (*GrentonSet, *fakeGate) {
	t.Helper()
	g := newFakeGate(t)
	gs := &GrentonSet{
		Host:          g.URL(),
		ReadPath:      fakeReadPath,
		SetLightPath:  fakeSetPath,
		QueryLimit:    30,
		cycleDuration: 10 * time.Second,
		Clus:          clus,
	}
	if configure != nil {
		configure(gs)
	}
	gs.InitClus()
	gs.startWorkers()
	t.Cleanup(gs.Stop)
	g.AddDevicesFrom(gs)
	return gs, g
}

// newTestClu returns a CLU with the given lights inside a GrentonSet wired to a fake GATE.
func newTestClu(t testing.TB, lights ...*Light) (*Clu, *fakeGate) {
	t.Helper()
	clu := &Clu{Id: "CLU_012abcde", Lights: lights}
	_, g := newTestSet(t, clu)
	return clu, g
}
