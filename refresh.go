package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// refreshCounts is the outcome of one refresh (concept 3.6). Applied, Discarded, Missing
// and Failed add up to Objects; Changed is the part of Applied whose state changed.
type refreshCounts struct {
	Objects   int
	Applied   int
	Changed   int
	Discarded int // dropped by the ordering rules (stage 3; always 0 for now)
	Missing   int // not in the response, or invalid for the device
	Failed    int // their request failed
	Skipped   int // not due this cycle (stage 5; always 0 for now)
	Duration  time.Duration
}

// Refresh reads every device through the read lane and waits until each one has an
// outcome. Every request is bounded by the request timeout, so a refresh always completes.
// Only one refresh runs at a time. HomeKit shows the applied state when Refresh returns.
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
	var problems []string
	for i, ch := range chans {
		r := <-ch
		switch {
		case r.Outcome == OutcomeFailed:
			c.Failed++
		case r.Outcome == OutcomeMissing:
			c.Missing++
			problems = append(problems, describeDevice(devs[i])+": missing from response")
		case r.Apply == ApplyInvalid:
			c.Missing++
			problems = append(problems, fmt.Sprintf("%s: invalid: %v", describeDevice(devs[i]), r.Err))
		case r.Apply == ApplyDiscarded:
			c.Discarded++
		default:
			c.Applied++
			if r.Apply == ApplyChanged {
				c.Changed++
			}
		}
	}
	<-gs.hkSync.flushed()
	c.Duration = time.Since(start)

	summary := fmt.Sprintf("refresh: %d devices in %d ms: %d applied (%d changed), %d discarded, %d missing or invalid, %d failed",
		c.Objects, c.Duration.Milliseconds(), c.Applied, c.Changed, c.Discarded, c.Missing, c.Failed)
	if c.Missing > 0 || c.Failed > 0 {
		if len(problems) > 0 {
			summary += "; " + strings.Join(problems, "; ")
		}
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

// startup runs what has to happen before the HomeKit server starts: the optional autotest,
// one refresh, and the start of the refresh cycle. It returns once the startup refresh is
// complete, whatever its outcome; the request timeout bounds how long that takes.
func (gs *GrentonSet) startup(ctx context.Context, autotest bool) {
	if autotest {
		gs.Logf("Testing all Grenton elements")
		gs.TestAllGrentonGate()
	}
	c := gs.Refresh()
	gs.Logf("startup refresh: %d of %d devices read (%d missing or invalid, %d failed) in %d ms",
		c.Applied, c.Objects, c.Missing, c.Failed, c.Duration.Milliseconds())
	gs.StartCycling()
}
