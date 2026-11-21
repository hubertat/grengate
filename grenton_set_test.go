package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func duplicateLines(log string) []string {
	var out []string
	for _, l := range strings.Split(log, "\n") {
		if strings.Contains(l, "duplicate accessory id") {
			out = append(out, l)
		}
	}
	return out
}

func TestDuplicateAccessoryIdIsReported(t *testing.T) {
	logs := captureLog(t)
	// Same CLU and device number, different kind: the kind is not part of the accessory ID.
	clu := &Clu{
		Id:       "CLU_012abcde",
		Lights:   []*Light{{CluObject: CluObject{Id: 5, Kind: "DOU", Name: "kitchen lamp"}}},
		Shutters: []*Shutter{{CluObject: CluObject{Id: 5, Kind: "ROL", Name: "kitchen blind"}, MaxTime: 20000}},
	}
	// Decimal CLU ids lose their first digit, so these two collide too.
	clu2 := &Clu{Id: "CLU0123456", Lights: []*Light{{CluObject: CluObject{Id: 9, Kind: "DOU", Name: "hall"}}}}
	clu3 := &Clu{Id: "CLU9123456", Lights: []*Light{{CluObject: CluObject{Id: 9, Kind: "DOU", Name: "garage"}}}}

	gs, _ := newTestSet(t, clu, clu2, clu3)

	lines := duplicateLines(logs.String())
	if len(lines) != 2 {
		t.Fatalf("duplicate lines = %d, want 2:\n%s", len(lines), logs.String())
	}
	for i, names := range [][2]string{{"kitchen lamp", "kitchen blind"}, {"hall", "garage"}} {
		if !strings.Contains(lines[i], names[0]) || !strings.Contains(lines[i], names[1]) {
			t.Errorf("line %q does not name both %q and %q", lines[i], names[0], names[1])
		}
	}
	// startup continues: every accessory was still created
	if n := len(gs.GetAllHkAcc()); n != 4 {
		t.Errorf("accessories = %d, want 4", n)
	}
}

func TestNoDuplicateWarningForUniqueIds(t *testing.T) {
	logs := captureLog(t)
	clu := &Clu{Id: "CLU_012abcde", Lights: []*Light{
		{CluObject: CluObject{Id: 1, Kind: "DOU", Name: "a"}},
		{CluObject: CluObject{Id: 2, Kind: "DOU", Name: "b"}},
	}}
	newTestSet(t, clu)
	if lines := duplicateLines(logs.String()); len(lines) != 0 {
		t.Errorf("unexpected duplicate lines: %v", lines)
	}
}

// The write protocol and tuning fields load from the config file; absent, they default to
// the new protocol, 5 commands per request, a 30 ms window and a 60 s thermostat cadence.
func TestConfig_WriteProtocolAndTuningFields(t *testing.T) {
	load := func(t *testing.T, json string) *GrentonSet {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(json), 0o600); err != nil {
			t.Fatal(err)
		}
		gs := &GrentonSet{}
		if err := gs.Config(path); err != nil {
			t.Fatal(err)
		}
		return gs
	}

	gs := load(t, `{"LegacyWriteScript": true, "SetterQueueSize": 8, "SetterFlushMs": 50, "ThermoCycleInSeconds": 120}`)
	if !gs.LegacyWriteScript || gs.SetterQueueSize != 8 || gs.SetterFlushMs != 50 || gs.ThermoCycleInSeconds != 120 {
		t.Errorf("loaded %v %d %d %d", gs.LegacyWriteScript, gs.SetterQueueSize, gs.SetterFlushMs, gs.ThermoCycleInSeconds)
	}

	gs = load(t, `{"Host": "http://gate"}`)
	gs.metrics = noMetrics{}
	cfg := gs.writeLaneConfig()
	if gs.LegacyWriteScript || !cfg.asArray || cfg.maxItems != 5 || cfg.window != 30*time.Millisecond {
		t.Errorf("defaults: legacy %v, array %v, %d per request, window %v; want new protocol, 5, 30ms",
			gs.LegacyWriteScript, cfg.asArray, cfg.maxItems, cfg.window)
	}
	if d := gs.thermoCycle(); d != 60*time.Second {
		t.Errorf("thermostat cadence %v, want 60s", d)
	}
}
