package main

import (
	"context"
	"fmt"
	"time"
)

// refreshCounts is the outcome of one refresh (concept 3.6). Objects counts the devices the
// refresh queued; Applied, Discarded, Missing and Failed add up to it. Changed is the part
// of Applied whose state changed. Skipped counts the devices not due this cycle (thermostats
// on their slower cadence): they were not queued and are not part of Objects.
type refreshCounts struct {
	Objects   int
	Applied   int
	Changed   int
	Discarded int // valid, but dropped by the ordering rules (concept 3.3, rule 4)
	Missing   int // not in the response, or invalid for the device
	Failed    int // their request failed
	Skipped   int // not due this cycle; not part of Objects
	Duration  time.Duration
}

// defaultThermoCycle is the default minimum time between polls of a thermostat.
const defaultThermoCycle = 60 * time.Second

// dueForPoll reports whether a refresh starting at now reads the device, and records when a
// thermostat was queued. Every device is read in every refresh except a ready thermostat:
// temperatures move slowly and a thermostat read costs several GATE calls, so it is read
// only about every ThermoCycleInSeconds (concept 3.6). A thermostat that is not ready is
// read in every refresh, so the not-ready window stays short (concept 3.5). In the legacy
// write protocol thermostats are read in every refresh: a three-field command is built from
// the device's setpoint, state and mode, which must not be a cadence old. Call with
// refreshMu held.
func (gs *GrentonSet) dueForPoll(d device, now time.Time) bool {
	th, ok := d.(*Thermo)
	if !ok || gs.LegacyWriteScript {
		return true
	}
	if th.isReady() && !th.lastPoll.IsZero() && !thermoPollDue(now.Sub(th.lastPoll), gs.thermoCycle(), gs.cycle()) {
		return false
	}
	th.lastPoll = now
	return true
}

// thermoPollDue reports whether a thermostat last queued since ago is due, polling every
// at most once per refresh cycle. With every at or below one cycle that is every refresh.
// Otherwise it is due from half a cycle before every: refreshes start a little after their
// boundary, so a strict comparison would let a late start push every poll one cycle later.
// The effective period is every rounded to whole cycles.
func thermoPollDue(since, every, cycle time.Duration) bool {
	if every <= cycle {
		return true
	}
	return since >= every-cycle/2
}

func (gs *GrentonSet) thermoCycle() time.Duration {
	if gs.ThermoCycleInSeconds <= 0 {
		return defaultThermoCycle
	}
	return time.Duration(gs.ThermoCycleInSeconds) * time.Second
}

func (gs *GrentonSet) cycle() time.Duration {
	if gs.cycleDuration <= 0 {
		return 10 * time.Second
	}
	return gs.cycleDuration
}

// now is the time of the refresh scheduler's clock.
func (gs *GrentonSet) now() time.Time {
	if gs.clock == nil {
		return time.Now()
	}
	return gs.clock.Now()
}

// Refresh reads every device that is due (dueForPoll) through the read lane and waits
// until each one has an outcome. Every request is bounded by the request timeout, so a refresh always completes.
// Only one refresh runs at a time. Applied state reaches HomeKit through the syncer, which
// Refresh does not wait for: a stalled HomeKit connection can block hap's SetValue
// indefinitely, and polling must go on regardless.
func (gs *GrentonSet) Refresh() refreshCounts {
	gs.refreshMu.Lock()
	defer gs.refreshMu.Unlock()

	start := time.Now()
	now := gs.now()
	var c refreshCounts
	var queued []device
	var objs []ReqObject
	for _, d := range gs.registry.devices() {
		if !gs.dueForPoll(d, now) {
			c.Skipped++
			continue
		}
		queued = append(queued, d)
		objs = append(objs, d.readRequest())
	}
	c.Objects = len(objs)
	chans := gs.reader.Submit(objs...)

	for i, ch := range chans {
		r := <-ch
		switch {
		case r.Outcome == OutcomeFailed:
			c.Failed++
			retryNextRefresh(queued[i])
		case r.Outcome == OutcomeMissing, r.Apply == ApplyInvalid: // logged per device by readCompleted
			c.Missing++
			retryNextRefresh(queued[i])
		case r.Apply == ApplyDiscarded:
			c.Discarded++
		default:
			c.Applied++
			if r.Apply == ApplyChanged {
				c.Changed++
			}
		}
	}
	c.Duration = time.Since(start)
	gs.metrics.Refresh(RefreshEvent{Time: time.Now(), Counts: c})

	summary := fmt.Sprintf("refresh: %d devices in %d ms (%d not due): %d applied (%d changed), %d discarded, %d missing or invalid, %d failed",
		c.Objects, c.Duration.Milliseconds(), c.Skipped, c.Applied, c.Changed, c.Discarded, c.Missing, c.Failed)
	if c.Missing > 0 || c.Failed > 0 {
		gs.Logf("%s", summary)
	} else {
		gs.Debugf("%s", summary)
	}
	return c
}

// retryNextRefresh makes a thermostat whose read failed, or came back missing or invalid,
// due again in the next refresh instead of a full cadence later. Call with refreshMu held.
func retryNextRefresh(d device) {
	if th, ok := d.(*Thermo); ok {
		th.lastPoll = time.Time{}
	}
}

func describeDevice(d device) string {
	co := d.base()
	clu := ""
	if co.clu != nil {
		clu = co.clu.Id
	}
	return fmt.Sprintf("%q (%s|%s)", co.Name, clu, co.GetMixedId())
}

// startup runs what has to happen before the HomeKit server is created: the optional
// autotest and one refresh. It returns once the startup refresh is complete, whatever its
// outcome (the request timeout bounds how long that takes), and HomeKit shows its result.
// The refresh cycle starts separately (startCycling), once the HomeKit server exists.
//
// ctx is the process lifecycle (SIGTERM). If it ends during startup, the workers are
// stopped (Stop): the lanes abort the request in flight and resolve everything still
// queued without sending it, so the autotest and the refresh return at once, and startup
// returns ctx.Err(). The caller must not go on to start HomeKit then.
func (gs *GrentonSet) startup(ctx context.Context, autotest bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	stopOnCancel := context.AfterFunc(ctx, gs.Stop)
	defer stopOnCancel()

	if autotest {
		gs.Autotest()
		if err := ctx.Err(); err != nil {
			gs.Logf("startup cancelled during the autotest: %v", err)
			return err
		}
	}
	c := gs.Refresh()
	if err := ctx.Err(); err != nil {
		gs.Logf("startup cancelled during the startup refresh: %v", err)
		return err
	}
	// The shutter simulations take what the refresh observed; then no HomeKit controller is
	// connected before the server starts, so the syncer cannot be blocked by one: HomeKit
	// starts with the state just read.
	for _, sh := range gs.shutters() {
		<-sh.sim.flushed()
	}
	<-gs.hkSync.flushed()
	gs.Logf("startup refresh: %d of %d devices read (%d missing or invalid, %d failed) in %d ms",
		c.Applied, c.Objects, c.Missing, c.Failed, c.Duration.Milliseconds())
	return nil
}

// startCycling starts the refresh scheduler until ctx ends. Cycle boundaries count from
// now. Call it after the HomeKit server was created: hap.NewServer sets up the
// characteristics, which must not be synced concurrently.
func (gs *GrentonSet) startCycling(ctx context.Context) {
	clk := gs.clock
	if clk == nil {
		clk = realClock{}
	}
	s := &refreshScheduler{
		origin:  clk.Now(),
		cycle:   gs.cycle(),
		clock:   clk,
		refresh: func() { gs.Refresh() },
		logf:    gs.Logf,
	}
	go s.run(ctx)
}
