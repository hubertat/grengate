package main

import (
	"fmt"
	"strings"
)

// autotestReport is the outcome of an autotest: devices that passed and one line per
// device that failed.
type autotestReport struct {
	Passed   int
	Failures []string
}

// Autotest reads every device through the read lane (array requests, like a refresh) and
// passes a device only if the response holds an item with its Clu and Id (matched by the
// lane), its Kind and a valid payload for that kind (checked by the device). It only
// reports: failing devices stay configured. A passing device's state is applied as by any read.
func (gs *GrentonSet) Autotest() autotestReport {
	gs.Logf("autotest: reading every device from the GATE")
	devs := gs.registry.devices()
	objs := make([]ReqObject, len(devs))
	for i, d := range devs {
		objs[i] = d.readRequest()
	}
	chans := gs.reader.Submit(objs...)

	var rep autotestReport
	for i, ch := range chans {
		r := <-ch
		var problem string
		switch {
		case r.Outcome == OutcomeFailed:
			problem = fmt.Sprintf("read request failed: %v", r.Err)
		case r.Outcome == OutcomeMissing:
			problem = "no item with its Clu and Id in the response"
		case r.Apply == ApplyInvalid:
			problem = fmt.Sprintf("invalid item: %v", r.Err)
		case r.Outcome != OutcomeOK:
			problem = fmt.Sprintf("unexpected outcome %v", r.Outcome)
		}
		if problem == "" {
			rep.Passed++
			continue
		}
		rep.Failures = append(rep.Failures, fmt.Sprintf("%s %s: %s", objs[i].Kind, describeDevice(devs[i]), problem))
	}

	if len(rep.Failures) == 0 {
		gs.Logf("autotest: all %d devices passed", rep.Passed)
	} else {
		gs.Logf("autotest: %d of %d devices FAILED (they stay configured):\n  %s",
			len(rep.Failures), len(devs), strings.Join(rep.Failures, "\n  "))
	}
	return rep
}
