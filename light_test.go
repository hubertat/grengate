package main

import (
	"net/http/httptest"
	"os"
	"testing"

	"github.com/brutella/hap/accessory"
	"github.com/brutella/hap/characteristic"
)

func newTestLight(hkType string) *Light {
	return &Light{
		CluObject: CluObject{Id: 1234, Kind: "DOU", Name: "test"},
		HkType:    hkType,
	}
}

// findC returns the first characteristic of the given type in the accessory.
func findC(t *testing.T, a *accessory.A, cType string) *characteristic.C {
	t.Helper()
	for _, s := range a.Ss {
		for _, c := range s.Cs {
			if c.Type == cType {
				return c
			}
		}
	}
	t.Fatalf("characteristic %s not found in accessory", cType)
	return nil
}

func TestLight_DefaultHkTypeIsLightbulb(t *testing.T) {
	clu, _ := newTestClu(t, newTestLight(""))

	if got := clu.GetAllHkAcc()[0].Type; got != accessory.TypeLightbulb {
		t.Errorf("accessory type = %d, want Lightbulb (%d)", got, accessory.TypeLightbulb)
	}
}

func TestLight_OutletHkTypeCreatesOutlet(t *testing.T) {
	clu, _ := newTestClu(t, newTestLight(HkTypeOutlet))

	if got := clu.GetAllHkAcc()[0].Type; got != accessory.TypeOutlet {
		t.Errorf("accessory type = %d, want Outlet (%d)", got, accessory.TypeOutlet)
	}
}

func TestLight_OutletKeepsAccessoryId(t *testing.T) {
	light := newTestLight(HkTypeOutlet)
	clu, _ := newTestClu(t, light)

	if got := clu.GetAllHkAcc()[0].Id; got != light.GetLongId() {
		t.Errorf("accessory id = %d, want %d", got, light.GetLongId())
	}
}

func TestLight_UnknownHkTypeIsRejected(t *testing.T) {
	light := newTestLight("Toaster")
	if err := light.validateHkType(); err == nil {
		t.Error("expected error for unknown HkType, got nil")
	}
}

func TestLight_OutletReadUpdatesOnAndInUse(t *testing.T) {
	light := newTestLight(HkTypeOutlet)
	clu := &Clu{Id: "CLU_012abcde", Lights: []*Light{light}}
	gs, gate := newTestSet(t, clu)
	gate.SetLight(clu.Id, light.GetMixedId(), true)

	if c := gs.Refresh(); c.Applied != 1 {
		t.Fatalf("refresh = %+v, want the outlet applied", c)
	}
	syncHomeKit(t, gs)

	a := light.HkAccessory()
	if v := findC(t, a, characteristic.TypeOn).Val; v != true {
		t.Errorf("On = %v, want true", v)
	}
	if v := findC(t, a, characteristic.TypeOutletInUse).Val; v != true {
		t.Errorf("OutletInUse = %v, want true", v)
	}
}

func TestLight_OutletRemoteUpdateSendsLightRequest(t *testing.T) {
	light := newTestLight(HkTypeOutlet)
	_, gate := newTestClu(t, light)
	gate.SetWriteStyle(fakeNewStyle)

	on := findC(t, light.HkAccessory(), characteristic.TypeOn)
	on.SetValueRequest(true, httptest.NewRequest("PUT", "/characteristics", nil))

	reqs := gate.WaitForRequests(t, fakeSetPath, 1)
	if len(reqs) != 1 {
		t.Fatalf("set requests = %d, want 1", len(reqs))
	}
	r := reqs[0]
	if r.IsArray || len(r.Items) != 1 {
		t.Fatalf("request = %s, want a single object", r.Body)
	}
	item := r.Items[0]
	if item["Kind"] != "Light" || item["Id"] != "DOU1234" || item["Clu"] != "CLU_012abcde" {
		t.Errorf("request identity = %v", item)
	}
	if l, _ := item["Light"].(map[string]any); l == nil || l["State"] != true {
		t.Errorf("request payload = %v, want Light.State=true", item["Light"])
	}
	if got := gate.Payload("CLU_012abcde", "DOU1234")["State"]; got != true {
		t.Errorf("fake GATE state = %v, want true", got)
	}
	if gate.RequestCount(fakeReadPath) != 0 {
		t.Error("a write must not trigger a read")
	}
}

func TestConfig_RejectsUnknownLightHkType(t *testing.T) {
	path := t.TempDir() + "/config.json"
	cfg := `{"Clus":[{"Id":"CLU_012abcde","Lights":[{"Id":1,"Kind":"DOU","HkType":"Toaster"}]}]}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	gs := &GrentonSet{}
	if err := gs.Config(path); err == nil {
		t.Error("expected Config to fail for unknown HkType, got nil")
	}
}

func TestConfig_AcceptsOutletHkType(t *testing.T) {
	path := t.TempDir() + "/config.json"
	cfg := `{"Clus":[{"Id":"CLU_012abcde","Lights":[{"Id":1,"Kind":"DOU","HkType":"Outlet"}]}]}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	gs := &GrentonSet{}
	if err := gs.Config(path); err != nil {
		t.Fatalf("Config: %v", err)
	}
	if got := gs.Clus[0].Lights[0].HkType; got != HkTypeOutlet {
		t.Errorf("HkType = %q, want %q", got, HkTypeOutlet)
	}
}
