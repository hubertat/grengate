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

	Thermo       *ThermoPayload  `json:",omitempty"`
	Light        *LightPayload   `json:",omitempty"`
	Shutter      *ShutterPayload `json:",omitempty"`
	MotionSensor *MotionPayload  `json:",omitempty"`

	// seq is the device's command sequence number (commands only, never sent): the write
	// completion hook compares it with the device's latest command (concept 3.3, rule 2).
	seq uint64
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

	// Ordering state (concept 3.3), guarded by mu.
	//
	// gen is bumped by every HomeKit write (rule 1), every finished write (rule 2) and every
	// push (rule 3); a read result is applied only if gen is still what it was when the read
	// was sent and no command is pending or in flight (rule 4).
	gen uint64
	// cmdSeq numbers the device's commands; the latest submitted one has cmdSeq.
	cmdSeq uint64
	// busy: a command is pending or in flight.
	busy bool
	// ready is set once a valid read result has been applied (concept 3.5). Never revoked.
	ready bool
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

// submitCommandLocked submits a command snapshot to the write lane (rule 1: the device has a
// command pending from now on). Call it with co.mu held, in the same critical section that
// changed the state the snapshot was built from: two callbacks for one device then reach
// the mailbox in the order they changed the device, so the latest state is the one that wins.
//
// Lock order: device mu, then lane mu. Submit never blocks, never calls hooks and never
// takes a device lock; the lane worker never holds its mailbox lock while it calls the hooks
// that take device locks. Keep it that way.
func (co *CluObject) submitCommandLocked(cmd ReqObject) <-chan laneResult {
	if cmd.Cmd == "" {
		cmd.Cmd = "SET"
	}
	co.gen++
	co.cmdSeq++
	co.busy = true
	cmd.seq = co.cmdSeq
	return co.clu.set.writer.Submit(cmd)[0]
}

// commandFinished records that the command with sequence number seq finished, acknowledged
// or not (rule 2). It reports whether that was the device's latest command, in which case
// the device is no longer busy and gets a confirm read (rule 5).
func (co *CluObject) commandFinished(seq uint64) (latest bool) {
	co.mu.Lock()
	defer co.mu.Unlock()
	co.gen++
	if seq != co.cmdSeq {
		return false
	}
	co.busy = false
	return true
}

// generation returns the device's generation (recorded when a read is sent, rule 4).
func (co *CluObject) generation() uint64 {
	co.mu.Lock()
	defer co.mu.Unlock()
	return co.gen
}

// isReady reports whether a valid read result has been applied.
func (co *CluObject) isReady() bool {
	co.mu.Lock()
	defer co.mu.Unlock()
	return co.ready
}

// acceptReadLocked is rule 4: a read sent at generation sentGen may be applied only if the
// generation is unchanged and no command is pending or in flight. Call with co.mu held.
func (co *CluObject) acceptReadLocked(sentGen uint64) bool {
	return co.gen == sentGen && !co.busy
}

// appliedLocked marks a valid read result as applied and returns its outcome; the first
// one always counts as a change. Call with co.mu held.
func (co *CluObject) appliedLocked(changed bool) ApplyOutcome {
	if !co.ready {
		changed = true
	}
	co.ready = true
	if changed {
		return ApplyChanged
	}
	return ApplyUnchanged
}

// waitCommand waits for the result of a submitted command (the shutter still waits for the
// GATE in this stage). Call it without the device lock. The outcome is logged by the write
// lane's completion hook; a superseded command is logged here.
func (co *CluObject) waitCommand(ch <-chan laneResult) laneResult {
	r := <-ch
	if r.Outcome == OutcomeSuperseded {
		co.clu.set.Debugf("write %s|%s: superseded by a newer command", co.clu.Id, co.GetMixedId())
	}
	return r
}
