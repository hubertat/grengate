package main

import (
	"fmt"

	"github.com/brutella/hap/accessory"
)

type Light struct {
	CluObject

	State bool

	hk *accessory.Lightbulb
}

func (gl *Light) LoadReqObject(obj ReqObject) error {
	if obj.Kind != "Light" {
		return fmt.Errorf("Light LoadReqObject: wrong object kind (%s)", obj.Kind)
	}

	if obj.Light == nil {
		return fmt.Errorf("Light LoadReqObject: missing Light object")
	}

	gl.State = obj.Light.State
	gl.Sync()

	return nil
}

func (gl *Light) InitAll() {
	gl.Req = ReqObject{
		Kind: "Light",
		Clu:  gl.clu.Id,
		Id:   gl.GetMixedId(),

		// Light: gl,
	}
	gl.AppendHk()
}
func (gl *Light) AppendHk() *accessory.Lightbulb {
	info := accessory.Info{
		Name:         gl.Name,
		SerialNumber: fmt.Sprintf("%d", gl.Id),
		Manufacturer: "Grenton",
		Model:        gl.Kind,
	}

	gl.hk = accessory.NewLightbulb(info)
	gl.hk.Id = gl.GetLongId()

	gl.hk.Lightbulb.On.OnValueRemoteUpdate(gl.Set)
	// gl.hk.Lightbulb.On.OnValueRemoteGet(gl.Get)

	gl.clu.set.Logf("HK Lightbulb added (id: %x, type: %d", gl.hk.A.Id, gl.hk.A.Type)
	return gl.hk
}

func (gl *Light) Sync() {
	gl.hk.Lightbulb.On.SetValue(gl.State)
}

func (gl *Light) Get() bool {

	// if gl.clu.set.CheckFreshness() {
	// 	return gl.State
	// }

	err := gl.Update()
	if err != nil {
		gl.clu.set.Error(err)
	}

	return gl.State

}

func (gl *Light) Set(state bool) {

	gl.State = state
	gl.clu.set.Debugf("Light.Set() called: %s/%s -> %v", gl.clu.GetMixedId(), gl.GetMixedId(), state)

	req := gl.Req
	req.Light = gl
	_, err := gl.SendReq(req)

	if err != nil {
		gl.clu.set.Error(err)
	}
	gl.clu.set.Debugf("Light.Set() completed: %s/%s", gl.clu.GetMixedId(), gl.GetMixedId())
}
