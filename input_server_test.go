package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/brutella/hap/characteristic"
)

// postUpdate sends a push to the input server handler and returns the HTTP status.
func postUpdate(is *InputServer, body string) int {
	req := httptest.NewRequest(http.MethodPost, "/update", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	is.HandleRequest(rec, req)
	return rec.Code
}

func TestInputServer_MotionPushUsesRegistry(t *testing.T) {
	clu := allKindsClu()
	gs, _ := newTestSet(t, clu)
	is := NewInputServer(gs, 0)

	if code := postUpdate(is, `{"Clu":"clu_012ABCDE","Id":"pir0004","State":true}`); code != http.StatusOK {
		t.Errorf("status = %d", code)
	}
	syncHomeKit(t, gs)
	motion := findC(t, clu.MotionSensors[0].GetA(), characteristic.TypeMotionDetected)
	if v := motion.Val; v != true {
		t.Errorf("motion detected = %v, want true", v)
	}
	// The push carries the sensor's whole state: it answers HomeKit reads from now on.
	if _, status := hkRead(motion); status != 0 {
		t.Errorf("read status after a push = %d, want 0", status)
	}
}

func TestInputServer_UnknownDeviceOrBadPayloadIs4xx(t *testing.T) {
	clu := allKindsClu()
	gs, _ := newTestSet(t, clu)
	is := NewInputServer(gs, 0)
	cases := []struct {
		name, body string
		want       int
	}{
		{"unknown device", `{"Clu":"CLU_012abcde","Id":"PIR0099","State":true}`, http.StatusNotFound},
		{"unknown CLU", `{"Clu":"CLU_other","Id":"PIR0004","State":true}`, http.StatusNotFound},
		{"not a motion sensor", `{"Clu":"CLU_012abcde","Id":"DOU0001","State":true}`, http.StatusNotFound},
		{"not JSON", `{"Clu":`, http.StatusBadRequest},
		{"no State", `{"Clu":"CLU_012abcde","Id":"PIR0004"}`, http.StatusBadRequest},
		{"no Id", `{"Clu":"CLU_012abcde","State":true}`, http.StatusBadRequest},
		{"State not a bool", `{"Clu":"CLU_012abcde","Id":"PIR0004","State":"yes"}`, http.StatusBadRequest},
	}
	for _, c := range cases {
		if code := postUpdate(is, c.body); code != c.want {
			t.Errorf("%s: status %d, want %d", c.name, code, c.want)
		}
	}
	syncHomeKit(t, gs)
	if v := findC(t, clu.Lights[0].HkAccessory(), characteristic.TypeOn).Val; v != false {
		t.Errorf("light changed by a motion push: On = %v", v)
	}
	if v := findC(t, clu.MotionSensors[0].GetA(), characteristic.TypeMotionDetected).Val; v != false {
		t.Errorf("motion sensor changed by a rejected push: %v", v)
	}
}

// Ordering table row 3: an older poll result arrives after a newer push. Discarded: the
// push bumped the generation (rules 3, 4).
func TestOrdering_PollOlderThanPushIsDiscarded(t *testing.T) {
	clu := allKindsClu()
	gs, g := newTestSet(t, clu)
	gs.Refresh()
	settle(t, gs)
	is := NewInputServer(gs, 0)
	motion := findC(t, clu.MotionSensors[0].GetA(), characteristic.TypeMotionDetected)
	rec := recordSyncs(motion)

	h := g.HoldNext(fakeReadPath)
	done := make(chan refreshCounts, 1)
	go func() { done <- gs.Refresh() }()
	h.Wait(t) // the GATE answered "no motion"; the answer is held

	if code := postUpdate(is, `{"Clu":"CLU_012abcde","Id":"PIR0004","State":true}`); code != http.StatusOK {
		t.Fatalf("push status = %d", code)
	}
	h.Release()
	c := <-done
	if c.Discarded != 1 || c.Applied != 3 {
		t.Errorf("refresh = %+v, want the motion sensor discarded, the rest applied", c)
	}
	settle(t, gs)
	if motion.Val != true {
		t.Errorf("motion detected = %v, want the pushed true", motion.Val)
	}
	if rec.contains(false) {
		t.Error("HomeKit showed the older poll result")
	}
}
