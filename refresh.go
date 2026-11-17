package main

import (
	"context"
	"fmt"
	"time"
)

// refreshCounts is the outcome of one refresh (concept 3.6). Applied, Discarded, Missing
// and Failed add up to Objects; Changed is the part of Applied whose state changed.
type refreshCounts struct {
	Objects   int
	Applied   int
	Changed   int
	Discarded int // valid, but dropped by the ordering rules (concept 3.3, rule 4)
	Missing   int // not in the response, or invalid for the device
	Failed    int // their request failed
	Skipped   int // not due this cycle (stage 5; always 0 for now)
	Duration  time.Duration
}

// Refresh reads every device through the read lane and waits until each one has an
// outcome. Every request is bounded by the request timeout, so a refresh always completes.
// Only one refresh runs at a time. Applied state reaches HomeKit through the syncer, which
// Refresh does not wait for: a stalled HomeKit connection can block hap's SetValue
// indefinitely, and polling must go on regardless.
func (gs *GrentonSet) Refresh() refreshCounts {
	gs.refreshMu.Lock()
	defer gs.refreshMu.Unlock()

	start := time.Now()
	devs := gs.registry.devices()
	objs := make([]ReqObject, len(devs))
	for i, d := range devs {
		objs[i] = d.readRequest()
	}
	chans := gs.reader.Submit(objs...)

	c := refreshCounts{Objects: len(devs)}
	for _, ch := range chans {
		r := <-ch
		switch {
		case r.Outcome == OutcomeFailed:
			c.Failed++
		case r.Outcome == OutcomeMissing, r.Apply == ApplyInvalid: // logged per device by readCompleted
			c.Missing++
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

	summary := fmt.Sprintf("refresh: %d devices in %d ms: %d applied (%d changed), %d discarded, %d missing or invalid, %d failed",
		c.Objects, c.Duration.Milliseconds(), c.Applied, c.Changed, c.Discarded, c.Missing, c.Failed)
	if c.Missing > 0 || c.Failed > 0 {
		gs.Logf("%s", summary)
	} else {
		gs.Debugf("%s", summary)
	}
	return c
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
func (gs *GrentonSet) startup(ctx context.Context, autotest bool) {
	if autotest {
		gs.Autotest()
	}
	c := gs.Refresh()
	// No HomeKit controller is connected before the server starts, so the syncer cannot be
	// blocked by one: HomeKit starts with the state just read.
	<-gs.hkSync.flushed()
	gs.Logf("startup refresh: %d of %d devices read (%d missing or invalid, %d failed) in %d ms",
		c.Applied, c.Objects, c.Missing, c.Failed, c.Duration.Milliseconds())
}

// startCycling starts the refresh scheduler until ctx ends. Cycle boundaries count from
// now. Call it after the HomeKit server was created: hap.NewServer sets up the
// characteristics, which must not be synced concurrently.
func (gs *GrentonSet) startCycling(ctx context.Context) {
	clk := gs.clock
	if clk == nil {
		clk = realClock{}
	}
	cycle := gs.cycleDuration
	if cycle <= 0 {
		cycle = 10 * time.Second
	}
	s := &refreshScheduler{
		origin:  clk.Now(),
		cycle:   cycle,
		clock:   clk,
		refresh: func() { gs.Refresh() },
		logf:    gs.Logf,
	}
	go s.run(ctx)
}
