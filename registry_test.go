package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/brutella/hap/characteristic"
)

func allKindsClu() *Clu {
	return &Clu{
		Id:            "CLU_012abcde",
		Lights:        []*Light{{CluObject: CluObject{Id: 1, Kind: "DOU", Name: "lamp"}}},
		Therms:        []*Thermo{{CluObject: CluObject{Id: 2, Kind: "THE", Name: "thermo"}, Source: "sensor"}},
		Shutters:      []*Shutter{{CluObject: CluObject{Id: 3, Kind: "ROL", Name: "blind"}}},
		MotionSensors: []*MotionSensor{{CluObject: CluObject{Id: 4, Kind: "PIR", Name: "motion"}}},
	}
}

func TestRegistry_FindsEveryKindCaseInsensitive(t *testing.T) {
	clu := allKindsClu()
	gs, _ := newTestSet(t, clu)

	cases := []struct {
		clu, id string
		want    *CluObject
	}{
		{"CLU_012abcde", "DOU0001", &clu.Lights[0].CluObject},
		{"clu_012ABCDE", "the0002", &clu.Therms[0].CluObject},
		{"CLU_012abcde", "ROL0003", &clu.Shutters[0].CluObject},
		{"CLU_012ABCDE", "PIR0004", &clu.MotionSensors[0].CluObject},
	}
	for _, c := range cases {
		d, ok := gs.registry.find(c.clu, c.id)
		if !ok || d.base() != c.want {
			t.Errorf("find(%s, %s) = %v, %v", c.clu, c.id, d, ok)
		}
	}
	if _, ok := gs.registry.find("CLU_012abcde", "DOU0002"); ok {
		t.Error("unknown id found")
	}
	if _, ok := gs.registry.find("CLU_other", "DOU0001"); ok {
		t.Error("device found under another CLU")
	}
	if n := len(gs.registry.devices()); n != 4 {
		t.Errorf("devices = %d, want 4", n)
	}
}

func TestRegistry_RoutesReadItemsByIdentity(t *testing.T) {
	clu := allKindsClu()
	gs, g := newTestSet(t, clu)
	// The GATE answers with identities in other letter case.
	g.RespondNext(fakeReadPath, `[{"Clu":"clu_012abcde","Id":"dou0001","Kind":"Light","Light":{"State":true}},
		{"Clu":"CLU_012abcde","Id":"PIR0004","Kind":"MotionSensor","MotionSensor":{"State":true}},
		{"Clu":"CLU_012abcde","Id":"DOU0099","Kind":"Light","Light":{"State":true}}]`)
	if c := gs.Refresh(); c.Applied != 2 || c.Missing != 2 {
		t.Errorf("refresh = %+v, want 2 applied, 2 missing", c)
	}
	syncHomeKit(t, gs)
	if v := findC(t, clu.Lights[0].HkAccessory(), characteristic.TypeOn).Val; v != true {
		t.Errorf("light On = %v, want true", v)
	}
	if v := findC(t, clu.MotionSensors[0].GetA(), characteristic.TypeMotionDetected).Val; v != true {
		t.Errorf("motion detected = %v, want true", v)
	}
}

func TestRegistry_ReportsSameCluAndIdTwice(t *testing.T) {
	logs := captureLog(t)
	clu := &Clu{Id: "CLU_012abcde", Lights: []*Light{
		{CluObject: CluObject{Id: 1, Kind: "DOU", Name: "first"}},
		{CluObject: CluObject{Id: 1, Kind: "DOU", Name: "second"}},
	}}
	gs, _ := newTestSet(t, clu)
	if d, _ := gs.registry.find("CLU_012abcde", "DOU0001"); d.base().Name != "first" {
		t.Errorf("registered %q, want the first one", d.base().Name)
	}
	if !strings.Contains(logs.String(), `"second"`) {
		t.Errorf("duplicate not reported: %s", logs.String())
	}
}

func TestInputServer_MotionPushUsesRegistry(t *testing.T) {
	clu := allKindsClu()
	gs, _ := newTestSet(t, clu)
	is := NewInputServer(gs, 0)

	post := func(body string) int {
		req := httptest.NewRequest(http.MethodPost, "/update", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		is.HandleRequest(rec, req)
		return rec.Code
	}

	if code := post(`{"Clu":"clu_012ABCDE","Id":"pir0004","State":true}`); code != http.StatusOK {
		t.Errorf("status = %d", code)
	}
	if v := findC(t, clu.MotionSensors[0].GetA(), characteristic.TypeMotionDetected).Val; v != true {
		t.Errorf("motion detected = %v, want true", v)
	}
	// A light is not a motion sensor: nothing changes (status codes change in stage 3).
	post(`{"Clu":"CLU_012abcde","Id":"DOU0001","State":true}`)
	if v := findC(t, clu.Lights[0].HkAccessory(), characteristic.TypeOn).Val; v != false {
		t.Errorf("light changed by a motion push: On = %v", v)
	}
}
