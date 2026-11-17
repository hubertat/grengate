package main

import (
	"fmt"

	"github.com/brutella/hap/accessory"
	"github.com/brutella/hap/characteristic"
)

// HomeKit representations available for a Light (binary DOUT output).
// The Grenton side is identical for all of them; only the HomeKit accessory differs.
const (
	HkTypeLightbulb = "Lightbulb"
	HkTypeOutlet    = "Outlet"
)

type Light struct {
	CluObject

	State bool

	// HkType selects the HomeKit accessory: "Lightbulb" (default when empty) or "Outlet".
	HkType string `json:",omitempty"`

	hkA     *accessory.A
	hkOn    *characteristic.On
	hkInUse *characteristic.OutletInUse // nil unless HkType is Outlet
}

// validateHkType returns an error when HkType is set to an unsupported value.
func (gl *Light) validateHkType() error {
	switch gl.HkType {
	case "", HkTypeLightbulb, HkTypeOutlet:
		return nil
	}
	return fmt.Errorf("light %q (%s%04d): unsupported HkType %q, use %q or %q",
		gl.Name, gl.Kind, gl.Id, gl.HkType, HkTypeLightbulb, HkTypeOutlet)
}

func (gl *Light) applyRead(obj ReqObject, sentGen, _ uint64) (ApplyOutcome, error) {
	p, err := validLight(obj)
	if err != nil {
		return ApplyInvalid, err
	}
	gl.mu.Lock()
	defer gl.mu.Unlock()
	if !gl.acceptReadLocked(sentGen) {
		return ApplyDiscarded, nil
	}
	changed := gl.State != *p.State
	gl.State = *p.State
	return gl.appliedLocked(changed), nil
}

func (gl *Light) InitAll() {
	gl.Req = ReqObject{
		Kind: "Light",
		Clu:  gl.clu.Id,
		Id:   gl.GetMixedId(),
	}
	gl.AppendHk()
}

// AppendHk creates the HomeKit accessory matching HkType.
func (gl *Light) AppendHk() *accessory.A {
	info := accessory.Info{
		Name:         gl.Name,
		SerialNumber: fmt.Sprintf("%d", gl.Id),
		Manufacturer: "Grenton",
		Model:        gl.Kind,
	}

	switch gl.HkType {
	case HkTypeOutlet:
		hk := accessory.NewOutlet(info)
		gl.hkA, gl.hkOn, gl.hkInUse = hk.A, hk.Outlet.On, hk.Outlet.OutletInUse
	default:
		hk := accessory.NewLightbulb(info)
		gl.hkA, gl.hkOn = hk.A, hk.Lightbulb.On
	}

	gl.hkA.Id = gl.GetLongId()
	gl.hkOn.OnValueRemoteUpdate(gl.Set)

	gl.clu.set.Logf("HK %s added (id: %x, type: %d)", gl.hkTypeName(), gl.hkA.Id, gl.hkA.Type)
	return gl.hkA
}

// HkAccessory returns the HomeKit accessory created by AppendHk.
func (gl *Light) HkAccessory() *accessory.A {
	return gl.hkA
}

func (gl *Light) hkTypeName() string {
	if gl.HkType == "" {
		return HkTypeLightbulb
	}
	return gl.HkType
}

func (gl *Light) Sync() {
	gl.syncMu.Lock()
	defer gl.syncMu.Unlock()
	gl.mu.Lock()
	state := gl.State
	gl.mu.Unlock()

	gl.hkOn.SetValue(state)
	if gl.hkInUse != nil {
		// Grenton has no power sensing, so "in use" mirrors the on/off state.
		gl.hkInUse.SetValue(state)
	}
}

// Set is the HomeKit write of On (rule 1): the optimistic state and the command snapshot
// are set and submitted under the device lock, and Set returns without waiting for the
// GATE. A light needs no baseline, so the write goes through whether it is ready or not.
// The write lane's completion hook handles the result.
func (gl *Light) Set(state bool) {
	gl.mu.Lock()
	defer gl.mu.Unlock()
	gl.State = state
	req := gl.Req
	req.Light = &LightPayload{State: ptr(state)}
	gl.submitCommandLocked(req)
}
