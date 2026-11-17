package main

// Shutter simulation (concept 3.4). Grenton reports whether a shutter's motor runs (State
// 0 stopped, 1 up, 2 down) but not where the shutter is, so grengate simulates the
// position: 1 % per MaxTime/100 while the motor is believed to move.
//
// Every shutter has one goroutine (shutterSim.run) that owns the simulation. Everything that
// can change it arrives through one inbox: new targets from HomeKit, results of the
// shutter's commands, read observations and clock ticks. Posting to the inbox never blocks
// (append under a mutex, then a non-blocking wake-up), so neither a hap callback, nor the
// read worker, nor the write lane waits for the simulation.

import (
	"sync"
	"time"
)

// timerClock schedules the ticks of the shutter simulation; injected so tests control time.
type timerClock interface {
	// AfterFunc calls f in its own goroutine after d and returns a function that cancels
	// the call if it has not happened yet.
	AfterFunc(d time.Duration, f func()) (stop func() bool)
}

func (realClock) AfterFunc(d time.Duration, f func()) func() bool {
	return time.AfterFunc(d, f).Stop
}

// motorState is what a shutter's motor does, with Grenton's State values.
type motorState int

const (
	motorStopped motorState = 0
	motorUp      motorState = 1
	motorDown    motorState = 2
)

// motorFromState maps a Grenton State to a motor state; unknown values count as stopped.
func motorFromState(state int) motorState {
	switch m := motorState(state); m {
	case motorUp, motorDown:
		return m
	}
	return motorStopped
}

func (m motorState) String() string {
	switch m {
	case motorUp:
		return "moving up"
	case motorDown:
		return "moving down"
	}
	return "stopped"
}

type simEventKind int

const (
	evObserved simEventKind = iota // a read observation that passed the ordering rules
	evBarrier                      // closes done once everything posted before it is handled
)

type simEvent struct {
	kind simEventKind
	// evObserved: the observed motor state and the completion stamp of its read.
	state motorState
	stamp uint64
	// evBarrier
	done chan struct{}
}

// observation is a motor state seen by a read, with the read's completion stamp.
type observation struct {
	state motorState
	stamp uint64
}

// shutterSim is the simulation of one shutter.
type shutterSim struct {
	sh    *Shutter
	clock timerClock

	// Inbox, guarded by imu. imu is a leaf lock: nothing else is locked while it is held,
	// so posting is safe with the device lock held.
	imu      sync.Mutex
	inbox    []simEvent
	posts    uint64 // events posted so far
	handling bool   // the goroutine is handling an event it took from the inbox
	started  bool
	stopped  bool
	wake     chan struct{}
	quit     chan struct{}
	done     chan struct{}
	stopOnce sync.Once

	// Simulation state, owned by the goroutine.

	// belief is what the motor is believed to do (mirrored in Shutter.State); beliefStamp
	// is the completion stamp of the evidence it was last set from. Evidence with a lower
	// stamp does not change it.
	belief      motorState
	beliefStamp uint64
	// lastObs is the newest observation.
	lastObs observation
	// pos is the believed position (mirrored in Shutter.currentPosition).
	pos int
	// dirty: a value HomeKit shows changed and needs a sync.
	dirty bool
}

func newShutterSim(sh *Shutter) *shutterSim {
	return &shutterSim{
		sh:   sh,
		pos:  sh.currentPosition,
		wake: make(chan struct{}, 1),
		quit: make(chan struct{}),
		done: make(chan struct{}),
	}
}

// start runs the goroutine. Call once, before stop.
func (s *shutterSim) start(clock timerClock) {
	s.clock = clock
	s.imu.Lock()
	s.started = true
	s.imu.Unlock()
	go s.run()
}

// stop ends the goroutine and waits for it. Events posted from then on are dropped.
// Safe to call more than once, and for a simulation that was never started.
func (s *shutterSim) stop() {
	s.stopOnce.Do(func() {
		s.imu.Lock()
		started := s.started
		s.imu.Unlock()
		close(s.quit)
		if started {
			<-s.done
		} else {
			s.shutdown()
		}
	})
}

// post adds an event to the inbox and wakes the goroutine. It never blocks.
func (s *shutterSim) post(ev simEvent) {
	s.imu.Lock()
	if s.stopped {
		s.imu.Unlock()
		if ev.done != nil {
			close(ev.done)
		}
		return
	}
	s.inbox = append(s.inbox, ev)
	s.posts++
	s.imu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// observed posts a read observation (called by the read worker, possibly with the device
// lock held).
func (s *shutterSim) observed(state int, stamp uint64) {
	s.post(simEvent{kind: evObserved, state: motorFromState(state), stamp: stamp})
}

// flushed returns a channel that is closed once every event posted before the call is
// handled (at once if the simulation is stopped).
func (s *shutterSim) flushed() <-chan struct{} {
	done := make(chan struct{})
	s.post(simEvent{kind: evBarrier, done: done})
	return done
}

func (s *shutterSim) run() {
	defer close(s.done)
	defer s.shutdown()
	for {
		select {
		case <-s.quit:
			return
		case <-s.wake:
		}
		for {
			select {
			case <-s.quit:
				return
			default:
			}
			ev, ok := s.take()
			if !ok {
				break
			}
			s.handle(ev)
		}
	}
}

// take removes the oldest event from the inbox.
func (s *shutterSim) take() (simEvent, bool) {
	s.imu.Lock()
	defer s.imu.Unlock()
	if len(s.inbox) == 0 {
		s.handling = false
		return simEvent{}, false
	}
	ev := s.inbox[0]
	s.inbox[0] = simEvent{}
	s.inbox = s.inbox[1:]
	s.handling = true
	return ev, true
}

// shutdown drops what is left in the inbox; waiting barriers are released.
func (s *shutterSim) shutdown() {
	s.imu.Lock()
	s.stopped = true
	s.handling = false
	rest := s.inbox
	s.inbox = nil
	s.imu.Unlock()
	for _, ev := range rest {
		if ev.done != nil {
			close(ev.done)
		}
	}
}

func (s *shutterSim) handle(ev simEvent) {
	switch ev.kind {
	case evBarrier:
		close(ev.done)
		return
	case evObserved:
		s.handleObserved(ev.state, ev.stamp)
	}
	s.publish()
}

// handleObserved applies a read observation: it is evidence of what the motor does.
func (s *shutterSim) handleObserved(m motorState, stamp uint64) {
	if stamp > s.lastObs.stamp {
		s.lastObs = observation{m, stamp}
	}
	if stamp > s.beliefStamp {
		s.setBelief(m, stamp)
	}
}

// setBelief applies evidence with the given completion stamp (newer than beliefStamp).
func (s *shutterSim) setBelief(m motorState, stamp uint64) {
	s.beliefStamp = stamp
	if m == s.belief {
		return
	}
	s.sh.clu.set.Debugf("shutter %s: motor believed %v (was %v)", describeDevice(s.sh), m, s.belief)
	s.belief = m
}

// publish copies what HomeKit shows into the device fields and queues a HomeKit sync if
// anything changed.
func (s *shutterSim) publish() {
	sh := s.sh
	sh.mu.Lock()
	if sh.currentPosition != s.pos || sh.State != int(s.belief) {
		sh.currentPosition, sh.State = s.pos, int(s.belief)
		s.dirty = true
	}
	sh.mu.Unlock()
	if s.dirty {
		s.dirty = false
		sh.clu.set.hkSync.mark(sh)
	}
}
