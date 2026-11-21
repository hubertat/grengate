package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/brutella/hap/characteristic"
)

// syncHomeKit waits (with a deadline) until every HomeKit sync queued so far is done.
func syncHomeKit(t testing.TB, gs *GrentonSet) {
	t.Helper()
	select {
	case <-gs.hkSync.flushed():
	case <-time.After(2 * time.Second):
		t.Fatal("HomeKit sync did not finish")
	}
}

// A HomeKit controller connection that stalls blocks hap's SetValue (notification write
// without deadline). Polling and shutdown must not depend on it.
func TestRefresh_CompletesWhileHomeKitSyncIsBlocked(t *testing.T) {
	clu := lightsClu(2)
	gs, g := newTestSet(t, clu)
	block := make(chan struct{})
	entered := make(chan struct{}, 1)
	on := findC(t, clu.Lights[0].HkAccessory(), characteristic.TypeOn)
	on.OnCValueUpdate(func(*characteristic.C, any, any, *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-block
	})
	released := false
	release := func() {
		if !released {
			released = true
			close(block)
		}
	}
	t.Cleanup(release)

	g.SetLight(tCLU, "DOU0001", true)
	within(t, 2*time.Second, "first refresh", func() { gs.Refresh() })
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("HomeKit sync never reached the blocking characteristic")
	}

	// The sync goroutine is stuck in SetValue: refreshes still complete and still read.
	g.SetLight(tCLU, "DOU0002", true)
	within(t, 2*time.Second, "refresh while HomeKit sync is blocked", func() {
		if c := gs.Refresh(); c.Applied != 2 || c.Changed != 1 {
			t.Errorf("refresh = %+v, want 2 applied, 1 changed", c)
		}
	})
	within(t, 3*time.Second, "Stop while HomeKit sync is blocked", gs.Stop)
	release()
}

// within runs fn and fails the test if it does not return in time.
func within(t testing.TB, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %v", what, d)
	}
}
