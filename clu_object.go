package main

import (
	"fmt"
	"strings"
	"sync"
)

type ReqObject struct {
	Clu    string
	Id     string
	Kind   string
	Cmd    string `json:",omitempty"`
	Source string `json:",omitempty"`

	Thermo       *Thermo       `json:",omitempty"`
	Light        *Light        `json:",omitempty"`
	Shutter      *Shutter      `json:",omitempty"`
	MotionSensor *MotionSensor `json:",omitempty"`
}

func (ro ReqObject) Equal(to ReqObject) bool {
	return strings.EqualFold(ro.Clu, to.Clu) && strings.EqualFold(ro.Id, to.Id) && strings.EqualFold(ro.Kind, to.Kind) && strings.EqualFold(ro.Cmd, to.Cmd)
}

type CluObject struct {
	Id   uint32
	Name string
	Kind string

	Req ReqObject `json:"-"`

	clu *Clu `json:"-"`

	// mu guards the device's state fields (everything a read, a push or a HomeKit callback
	// changes). It is held only for field access: never during HTTP and never while
	// calling into hap (SetValue), so the hap callback path cannot deadlock on it.
	mu sync.Mutex
	// syncMu serialises pushes to HomeKit, so two Syncs of one device cannot interleave
	// and leave an older state showing. Taken before mu, never inside it.
	syncMu sync.Mutex

	// loaded is set once a valid read result has been applied.
	loaded bool
}

// wireIdentity copies the identity fields the device structs carry on the wire, for
// command snapshots built under the lock (the snapshot must not point at the live device).
func (co *CluObject) wireIdentity() CluObject {
	return CluObject{Id: co.Id, Name: co.Name, Kind: co.Kind}
}

func (co *CluObject) GetLongId() uint64 {
	if co.clu == nil {
		panic("CluObject GetLongId() called, but clu not set.")
	}
	return (uint64(co.clu.GetIntId()) << 32) + uint64(co.Id)
}

// func (co *CluObject) GetFullName() string {
// 	return fmt.Sprintf("%s%4d", co.Kind, co.Id)
// }

func (co *CluObject) GetMixedId() string {
	return fmt.Sprintf("%s%04d", co.Kind, co.Id)
}

// sendCommand submits a command to the write lane and waits for its result (HomeKit
// callbacks still wait for the GATE in this stage). The outcome is logged by the write
// lane's completion hook; a superseded command is logged here.
func (co *CluObject) sendCommand(cmd ReqObject) laneResult {
	if cmd.Cmd == "" {
		cmd.Cmd = "SET"
	}
	gs := co.clu.set
	r := <-gs.writer.Submit(cmd)[0]
	if r.Outcome == OutcomeSuperseded {
		gs.Debugf("write %s|%s (%s): superseded by a newer command", cmd.Clu, cmd.Id, cmd.Kind)
	}
	return r
}
