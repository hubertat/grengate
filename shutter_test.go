package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestShutter(maxTime int) *Shutter {
	return &Shutter{
		CluObject: CluObject{Id: 7, Kind: "ROL", Name: "blind"},
		MaxTime:   maxTime,
	}
}

func TestShutter_MoveRefusedWithoutMaxTime(t *testing.T) {
	logs := captureLog(t)
	sh := newTestShutter(0)
	clu := &Clu{Id: "CLU_012abcde", Shutters: []*Shutter{sh}}
	_, gate := newTestSet(t, clu)

	target := sh.hk.WindowCovering.TargetPosition
	done := make(chan struct{})
	go func() {
		defer close(done)
		target.SetValueRequest(0, httptest.NewRequest("PUT", "/characteristics", nil))
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("move with MaxTime 0 did not return at once")
	}

	if got := target.Value(); got != 100 {
		t.Errorf("HomeKit target = %v, want 100 (current position)", got)
	}
	if n := gate.RequestCount(""); n != 0 {
		t.Errorf("fake GATE received %d requests, want 0", n)
	}
	if !strings.Contains(logs.String(), "MaxTime") {
		t.Errorf("refusal not logged: %q", logs.String())
	}
	if sh.looping {
		t.Error("move loop started")
	}
}
