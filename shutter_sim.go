package main

// Shutter simulation (concept 3.4). Grenton reports whether a shutter's motor runs (State
// 0 stopped, 1 up, 2 down) but not where the shutter is, so grengate simulates the
// position: 1 % per MaxTime/100 while the motor is believed to move.
//
// Every shutter has one goroutine (shutterSim.run) that owns the simulation. Everything that
// can change it arrives through one inbox: new targets from HomeKit, results of the
// shutter's commands, read observations and clock ticks. Posting to the inbox never blocks
// (append under a mutex, then a non-blocking wake-up), so neither a hap callback, nor the
// read worker, nor the write lane waits for the simulation. A command's result is carried
// to the inbox by a small goroutine per command that waits on the command's result channel:
// the write lane delivers every result exactly once, so it always ends.
//
// The simulation keeps two things apart:
//
//   - Evidence: what the motor does. The belief (stopped, moving up, moving down) changes
//     only on the acknowledgement of a command, of any generation, or on a read observation
//     that passed the ordering rules, and only if the evidence's completion stamp is higher
//     than that of the evidence last applied. Failed and superseded commands are evidence of
//     nothing.
//   - Authority: which target counts. Every HomeKit target starts a movement generation;
//     the latest one alone holds the target.
//
// The clock runs while the motor is believed to move because of a command of ours: towards
// the target if the movement belongs to the latest generation, else up to the end stop. A
// movement nobody here started (a wall switch) shows in HomeKit but does not move the
// position.

import (
	"slices"
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

// endStop is the position a movement in direction m ends at.
func endStop(m motorState) int {
	if m == motorUp {
		return 100
	}
	return 0
}

// Shutter commands (the Cmd values update-script.lua understands).
const (
	cmdUp   = "MOVEUP"
	cmdDown = "MOVEDOWN"
	cmdStop = "STOP"
)

// shutterCommand is a command the simulation submitted.
type shutterCommand struct {
	cmd string
	gen uint64 // movement generation that submitted it
	// stamp is the completion stamp of its result (once known).
	stamp uint64
}

// motor is what an acknowledgement of the command proves the motor does.
func (c *shutterCommand) motor() motorState {
	switch c.cmd {
	case cmdUp:
		return motorUp
	case cmdDown:
		return motorDown
	}
	return motorStopped
}

type simEventKind int

const (
	evTarget   simEventKind = iota // a new target from HomeKit
	evResult                       // the result of a command
	evObserved                     // a read observation that passed the ordering rules
	evTick                         // a clock step
	evBarrier                      // closes done once everything posted before it is handled
)

type simEvent struct {
	kind simEventKind
	// evTarget: the movement generation the HomeKit callback started, and its target.
	gen    uint64
	target int
	// evResult
	cmd    *shutterCommand
	result laneResult
	// evObserved: the observed motor state and the completion stamp of its read.
	state motorState
	stamp uint64
	// evTick: the clock run the tick belongs to.
	run uint64
	// evBarrier
	done chan struct{}
}

// observation is a motor state seen by a read, with the read's completion stamp.
type observation struct {
	state motorState
	stamp uint64
}

// generation is a movement generation: one HomeKit target and what it has done so far.
type generation struct {
	n      uint64 // 0: no target yet
	target int
	// travel counts the clock steps of this generation's own movement.
	travel int
	// stopSent: its automatic STOP was submitted.
	stopSent bool
}

// shutterSim is the simulation of one shutter.
type shutterSim struct {
	sh    *Shutter
	clock timerClock

	// resultHook, if set (tests only), runs in a command's forwarding goroutine before the
	// result is posted, so a test can make a result arrive late.
	resultHook func(c *shutterCommand, r laneResult)

	// Inbox, guarded by imu. imu is a leaf lock: nothing else is locked while it is held,
	// so posting is safe with the device lock held.
	imu      sync.Mutex
	inbox    []simEvent
	posts    uint64 // events posted so far
	handling bool   // the goroutine is handling an event it took from the inbox
	// outstanding counts submitted commands whose result was not taken from the inbox yet.
	outstanding int
	started     bool
	stopped     bool
	wake        chan struct{}
	quit        chan struct{}
	done        chan struct{}
	stopOnce    sync.Once

	// Simulation state, owned by the goroutine.

	// belief is what the motor is believed to do (mirrored in Shutter.State); beliefStamp
	// is the completion stamp of the evidence it was last set from. Evidence with a lower
	// stamp does not change it.
	belief      motorState
	beliefStamp uint64
	// lastObs is the newest observation.
	lastObs observation
	// moveGen is the generation the believed movement belongs to: the generation of the
	// command whose acknowledgement started it (or that took it over). 0: not ours.
	moveGen uint64
	// gen is the latest movement generation.
	gen generation
	// cmds are the submitted commands whose result was not handled yet, oldest first.
	cmds []*shutterCommand
	// pos is the believed position (mirrored in Shutter.currentPosition).
	pos int
	// The clock: clockStop cancels the pending step (nil while the clock does not run);
	// clockRun numbers the steps, so a step that fired after it was cancelled is ignored.
	clockStop func() bool
	clockRun  uint64
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

// observed posts a read observation (called by the read worker with the device lock held).
func (s *shutterSim) observed(state int, stamp uint64) {
	s.post(simEvent{kind: evObserved, state: motorFromState(state), stamp: stamp})
}

// newTarget posts a HomeKit target (called by the HomeKit callback with the device lock
// held, so targets arrive in the order of their generations).
func (s *shutterSim) newTarget(gen uint64, target int) {
	s.post(simEvent{kind: evTarget, gen: gen, target: target})
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
	if ev.kind == evResult {
		s.outstanding--
	}
	return ev, true
}

// shutdown stops the clock and drops what is left in the inbox; waiting barriers are
// released.
func (s *shutterSim) shutdown() {
	if s.clockStop != nil {
		s.clockStop()
		s.clockStop = nil
	}
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
	case evTarget:
		s.handleTarget(ev.gen, ev.target)
	case evResult:
		s.handleResult(ev.cmd, ev.result)
	case evObserved:
		s.handleObserved(ev.state, ev.stamp)
	case evTick:
		s.handleTick(ev.run)
	}
	s.checkEnd()
	s.checkAutoStop()
	s.updateClock()
	s.publish()
}

// handleTarget starts a new movement generation for a HomeKit target. From standstill, or
// against the believed movement, the generation submits its command (MOVEUP, MOVEDOWN, or
// STOP when the target is the position); a command still waiting in the write mailbox is
// replaced by it. If the motor already moves towards the target and no command could change
// that, the movement simply becomes the new generation's.
func (s *shutterSim) handleTarget(n uint64, target int) {
	s.gen = generation{n: n, target: target}
	cmd := cmdStop
	switch {
	case target > s.pos:
		cmd = cmdUp
	case target < s.pos:
		cmd = cmdDown
	}
	c := &shutterCommand{cmd: cmd, gen: n}
	if m := c.motor(); m != motorStopped && m == s.belief && len(s.cmds) == 0 {
		s.claim(n)
		return
	}
	s.submit(c)
}

// submit sends a command to the write lane. Its result comes back through the inbox.
func (s *shutterSim) submit(c *shutterCommand) {
	sh := s.sh
	sh.mu.Lock()
	req := sh.Req
	req.Cmd = c.cmd
	ch := sh.submitCommandLocked(req)
	sh.mu.Unlock()
	sh.clu.set.Debugf("shutter %s: %s submitted (generation %d)", describeDevice(sh), c.cmd, c.gen)

	s.cmds = append(s.cmds, c)
	s.imu.Lock()
	s.outstanding++
	s.imu.Unlock()
	hook := s.resultHook
	go func() {
		r := <-ch
		if hook != nil {
			hook(c, r)
		}
		s.post(simEvent{kind: evResult, cmd: c, result: r})
	}()
}

// handleResult takes the result of a command. Only an acknowledgement is evidence.
func (s *shutterSim) handleResult(c *shutterCommand, r laneResult) {
	s.cmds = slices.DeleteFunc(s.cmds, func(x *shutterCommand) bool { return x == c })
	if r.Outcome != OutcomeOK {
		return
	}
	c.stamp = r.Stamp
	m := c.motor()
	if c.stamp > s.beliefStamp {
		s.setBelief(m, c.stamp)
		if m != motorStopped {
			s.claim(c.gen)
		}
		return
	}
	// A late acknowledgement: the command ran, but newer evidence says what the motor does
	// now. If that is the movement this command started (an observation saw it first), the
	// movement is the command's.
	if m != motorStopped && m == s.belief && s.moveGen == 0 {
		s.claim(c.gen)
	}
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

// setBelief applies evidence with the given completion stamp (newer than beliefStamp). A
// changed belief belongs to no generation until an acknowledgement claims it.
func (s *shutterSim) setBelief(m motorState, stamp uint64) {
	s.beliefStamp = stamp
	if m == s.belief {
		return
	}
	s.sh.clu.set.Debugf("shutter %s: motor believed %v (was %v)", describeDevice(s.sh), m, s.belief)
	s.belief = m
	s.moveGen = 0
}

// claim makes the believed movement generation n's. For the latest generation its travel
// starts now.
func (s *shutterSim) claim(n uint64) {
	if s.moveGen == n {
		return
	}
	s.moveGen = n
	if n == s.gen.n {
		s.gen.travel = 0
	}
}

// checkEnd ends the simulation once the motor is believed stopped and the latest
// generation waits for no command of its own: the target becomes the position (an
// externally observed stop, or a STOP that ran).
func (s *shutterSim) checkEnd() {
	if s.belief != motorStopped || s.latestBusy() {
		return
	}
	s.setTarget(s.pos)
}

// checkAutoStop submits the STOP at the target. Only the latest generation does, and only
// for its own movement, which an acknowledgement (or a movement it took over) started: the
// STOP can never replace its own unsent move. A target at an end stop gets a full travel
// time first, as the loop-based simulation did, so the motor surely reaches the end stop
// whatever the error of the simulated position.
func (s *shutterSim) checkAutoStop() {
	g := &s.gen
	if g.n == 0 || s.moveGen != g.n || g.stopSent || s.latestBusy() || !s.atTarget() || s.travelLeft() {
		return
	}
	g.stopSent = true
	s.submit(&shutterCommand{cmd: cmdStop, gen: g.n})
}

// atTarget reports whether the believed movement reached (or passed) the latest target.
func (s *shutterSim) atTarget() bool {
	switch s.belief {
	case motorUp:
		return s.pos >= s.gen.target
	case motorDown:
		return s.pos <= s.gen.target
	}
	return false
}

// travelLeft reports whether the latest generation's movement to an end stop has not run a
// full travel time yet.
func (s *shutterSim) travelLeft() bool {
	return (s.gen.target == 0 || s.gen.target == 100) && s.gen.travel < 100
}

// latestBusy reports whether the latest generation waits for the result of a command.
func (s *shutterSim) latestBusy() bool {
	return slices.ContainsFunc(s.cmds, func(c *shutterCommand) bool { return c.gen == s.gen.n })
}

// setTarget changes the latest generation's target, and what HomeKit shows as the target
// unless a newer HomeKit target is already on its way.
func (s *shutterSim) setTarget(target int) {
	s.gen.target = target
	sh := s.sh
	sh.mu.Lock()
	if sh.targetGen == s.gen.n && sh.targetPosition != target {
		sh.targetPosition = target
		s.dirty = true
	}
	sh.mu.Unlock()
}

// limit is where the clock stops moving the position: the latest generation's target for
// its own movement, else the end stop.
func (s *shutterSim) limit() int {
	if s.gen.n != 0 && s.moveGen == s.gen.n {
		return s.gen.target
	}
	return endStop(s.belief)
}

// nextPos is the position one clock step further, if the clock moves it.
func (s *shutterSim) nextPos() (int, bool) {
	if s.moveGen == 0 {
		return s.pos, false
	}
	lim := s.limit()
	switch {
	case s.belief == motorUp && s.pos < lim:
		return s.pos + 1, true
	case s.belief == motorDown && s.pos > lim:
		return s.pos - 1, true
	}
	return s.pos, false
}

func (s *shutterSim) handleTick(run uint64) {
	if s.clockStop == nil || run != s.clockRun {
		return // cancelled after it fired
	}
	s.clockStop = nil // a step fires once; updateClock schedules the next
	if s.gen.n != 0 && s.moveGen == s.gen.n {
		s.gen.travel++
	}
	if next, ok := s.nextPos(); ok {
		s.pos = next
	}
}

// clockNeeded reports whether the clock has something to do: move the position, or count
// the latest generation's travel to an end stop.
func (s *shutterSim) clockNeeded() bool {
	if _, ok := s.nextPos(); ok {
		return true
	}
	g := s.gen
	return g.n != 0 && s.moveGen == g.n && !g.stopSent && s.travelLeft()
}

// updateClock starts or stops the clock as needed. One step is MaxTime/100.
func (s *shutterSim) updateClock() {
	need := s.clockNeeded()
	switch {
	case need && s.clockStop == nil:
		s.clockRun++
		run := s.clockRun
		s.clockStop = s.clock.AfterFunc(s.sh.stepDuration(), func() { s.post(simEvent{kind: evTick, run: run}) })
	case !need && s.clockStop != nil:
		s.clockStop()
		s.clockStop = nil
	}
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
