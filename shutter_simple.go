package main

import (
	"fmt"

	"github.com/brutella/hc/accessory"
)

type ShutterSimple struct {
	CluObject

	hk					*accessory.Switch

	// State: 0 - stopped; 1 - going up; 2 - going down
	State				int		
	MaxTime				int
}

func (sh *ShutterSimple) InitAll() {
	sh.Req = ReqObject{
		Kind: "Shutter",
		Clu: sh.clu.Id,
		Id: sh.GetMixedId(),

	}
	sh.AppendHk()
}
func (sh *ShutterSimple) AppendHk() {
	info := accessory.Info{
		Name:         sh.Name,
		SerialNumber: fmt.Sprintf("%d", sh.Id),
		Manufacturer: "Grenton",
		Model:        sh.Kind,
		ID:           sh.GetLongId(),
	}

	sh.hk = accessory.NewSwitch(info)

	sh.hk.Switch.On.OnValueRemoteUpdate(sh.SetPosition)
	
	sh.clu.set.Logf("HK Switch added (id: %x | type %x)", sh.hk.Accessory.ID, sh.hk.Accessory.Type)
}


// SetPosition check which direction should move and call StartMoving func
func (sh *ShutterSimple) SetPosition(open bool) {
	sh.hk.Switch.On.SetValue(open)
	
	go sh.StartMoving(open)
}

// StartMoving	sends request to move up (or != up = down)
func (sh *ShutterSimple) StartMoving(up bool) {
	// SEND request
	req := sh.Req
	req.ShutterSimple = sh
	if up {
		req.Cmd = "MOVEUP"
	} else {
		req.Cmd = "MOVEDOWN"
	}
	obj, err := sh.SendReq(req)
	if err != nil {
		sh.clu.set.Error(fmt.Errorf("Shutter StartMoving: error from sending request:\n%v", err))
		return
	}
	sh.LoadReqObject(obj)
}

// LoadReqObject checks object received from http request end reads it into Shutter
func (sh *ShutterSimple) LoadReqObject(obj ReqObject) error {
	if obj.Kind != "Shutter" {
		return fmt.Errorf("Shutter LoadReqObject: wrong object kind (%s)", obj.Kind)
	}

	if obj.Shutter == nil {
		return fmt.Errorf("Shutter LoadReqObject: missing Shutter object")
	}

	sh.clu.set.Debugf("Shutter LoadReqObject loading: \n%+v", obj)

	sh.State = obj.Shutter.State
	sh.MaxTime = obj.Shutter.MaxTime

	return nil
}


// Sync for compatibility
func (sh *ShutterSimple) Sync() {
	
}