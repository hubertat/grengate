package main

import (
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/brutella/hap/accessory"
	"github.com/brutella/hap/characteristic"
)

// newTestClu returns a Clu wired to a GrentonSet whose setter never flushes
// during a test, so queued write requests can be inspected.
func newTestClu(lights ...*Light) *Clu {
	gs := &GrentonSet{}
	gs.setter.Init(gs, 10, time.Hour, nil, nil, true)

	clu := &Clu{Id: "CLU_012abcde", Lights: lights}
	gs.Clus = []*Clu{clu}
	gs.InitClus()
	return clu
}

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
	clu := newTestClu(newTestLight(""))

	if got := clu.GetAllHkAcc()[0].Type; got != accessory.TypeLightbulb {
		t.Errorf("accessory type = %d, want Lightbulb (%d)", got, accessory.TypeLightbulb)
	}
}

func TestLight_OutletHkTypeCreatesOutlet(t *testing.T) {
	clu := newTestClu(newTestLight(HkTypeOutlet))

	if got := clu.GetAllHkAcc()[0].Type; got != accessory.TypeOutlet {
		t.Errorf("accessory type = %d, want Outlet (%d)", got, accessory.TypeOutlet)
	}
}

func TestLight_OutletKeepsAccessoryId(t *testing.T) {
	light := newTestLight(HkTypeOutlet)
	clu := newTestClu(light)

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

func TestLight_OutletLoadReqObjectUpdatesOnAndInUse(t *testing.T) {
	light := newTestLight(HkTypeOutlet)
	newTestClu(light)

	err := light.LoadReqObject(ReqObject{Kind: "Light", Light: &Light{State: true}})
	if err != nil {
		t.Fatalf("LoadReqObject: %v", err)
	}

	a := light.HkAccessory()
	if v := findC(t, a, characteristic.TypeOn).Val; v != true {
		t.Errorf("On = %v, want true", v)
	}
	if v := findC(t, a, characteristic.TypeOutletInUse).Val; v != true {
		t.Errorf("OutletInUse = %v, want true", v)
	}
}

func TestLight_OutletRemoteUpdateQueuesLightRequest(t *testing.T) {
	light := newTestLight(HkTypeOutlet)
	clu := newTestClu(light)

	on := findC(t, light.HkAccessory(), characteristic.TypeOn)
	on.SetValueRequest(true, httptest.NewRequest("PUT", "/characteristics", nil))

	q := clu.set.setter.queue
	if len(q) != 1 {
		t.Fatalf("setter queue length = %d, want 1", len(q))
	}
	if q[0].Kind != "Light" || q[0].Id != "DOU1234" || q[0].Light == nil || !q[0].Light.State {
		t.Errorf("queued request = %+v, want Kind=Light Id=DOU1234 State=true", q[0])
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
