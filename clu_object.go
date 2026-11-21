package main

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/brutella/hap"
	"github.com/brutella/hap/accessory"
	"github.com/brutella/hap/service"
)

// hkNoResponse is the HAP status for a device that cannot answer (concept 3.5): HomeKit
// shows it as not responding.
const hkNoResponse = hap.JsonStatusServiceCommunicationFailure

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

	// owners are the devices whose commands this command carries, each with that command's
	// sequence number (commands only, never sent). Normally one: the submitting device.
	// The write completion hook finishes the command for each of them (concept 3.3, rule 2).
	owners []cmdOwner
}

// cmdOwner is a device that submitted a command and the command's sequence number.
type cmdOwner struct {
	co  *CluObject
	seq uint64
}

// mergeOwners returns the owners of a command that replaced or merged into a pending one:
// the incoming command's owners, plus the pending command's owners of other devices (their
// command is folded in or replaced, but must still finish for them). A device's own older
// command needs no finishing: its newer one is now the latest.
func mergeOwners(pending, incoming ReqObject) []cmdOwner {
	out := append([]cmdOwner(nil), incoming.owners...)
	for _, p := range pending.owners {
		if !slices.ContainsFunc(out, func(o cmdOwner) bool { return o.co == p.co }) {
			out = append(out, p)
		}
	}
	return out
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
	cmd.owners = []cmdOwner{{co: co, seq: co.cmdSeq}}
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

// guardReads makes HomeKit reads of the accessory's characteristics (all services except
// the accessory information) answer with an error status while the device is not ready,
// so it shows as not responding instead of showing made-up defaults (concept 3.5).
// hap also calls ValueRequest with a nil request to build the /accessories listing, where
// HAP requires a value for every readable characteristic: that call gets the cached value.
func (co *CluObject) guardReads(a *accessory.A) {
	for _, s := range a.Ss {
		if s.Type == service.TypeAccessoryInformation {
			continue
		}
		for _, c := range s.Cs {
			c.ValueRequestFunc = func(req *http.Request) (any, int) {
				if req != nil && !co.isReady() {
					return nil, hkNoResponse
				}
				return c.Val, 0
			}
		}
	}
}

// writeGuard returns a hap SetValueRequestFunc for a write that needs the device's
// baseline (concept 3.5): while the device is not ready the write is refused with an error
// status, logged, and a read of the device is queued. hap calls it before it stores the
// value and before the OnValueRemoteUpdate callbacks, which do the write when it passes.
func (co *CluObject) writeGuard(what string) func(any, *http.Request) (any, int) {
	return func(v any, _ *http.Request) (any, int) {
		if co.isReady() {
			return nil, 0
		}
		co.refuseWrite(fmt.Sprintf("%s to %v", what, v))
		return nil, hkNoResponse
	}
}

// refuseWrite logs a refused HomeKit write and queues a read of the device.
func (co *CluObject) refuseWrite(what string) {
	clu := co.clu
	clu.set.Logf("%q (%s|%s): %s refused: not read from the GATE yet; reading it now",
		co.Name, clu.Id, co.GetMixedId(), what)
	clu.set.reader.Submit(co.readRequest())
}
