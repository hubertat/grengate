package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/brutella/hap/accessory"
)

// GrentonSet is main struct representing settings and having child Clu structs
type GrentonSet struct {
	Host            string
	ReadPath        string
	SetLightPath    string
	InputServerPort int

	BridgeName string

	Clus []*Clu

	HkPin  string
	HkPath string

	// FreshInSeconds is accepted so existing config files load; it has no effect.
	FreshInSeconds  int
	CycleInSeconds  int
	Verbose         bool
	PerformAutotest bool
	QueryLimit      int

	// InfluxDB is the optional metrics reporting (off by default).
	InfluxDB InfluxConfig

	cycleDuration time.Duration
	// clock drives the refresh scheduler (nil: real time).
	clock clock

	registry *registry

	// requestTimeout bounds every GATE request (default 10 s; tests shorten it).
	requestTimeout time.Duration
	// stamps is the completion counter shared by both lanes (concept 3.1).
	stamps atomic.Uint64
	reader *lane
	writer *lane
	hkSync *hkSyncer
	// metrics receives request, command and refresh events (default: none recorded).
	metrics Metrics
	overlap overlapTracker
	influx  *influxWriter

	stopOnce sync.Once

	// refreshMu lets only one refresh run at a time.
	refreshMu sync.Mutex
}

// Debugf logs info, when Verbose option is on
func (gs *GrentonSet) Debugf(format string, v ...interface{}) {
	if gs.Verbose {
		gs.Logf(format, v...)
	}
}

// Logf logs info
func (gs *GrentonSet) Logf(format string, v ...interface{}) {
	log.Printf(format, v...)
}

// Error logs errors, needed when function is running in goroutine
func (gs *GrentonSet) Error(err error) {
	log.Printf("!__Error_:\n%s\n___\n", err.Error())
}

// Config is loading config file from provided path
func (gs *GrentonSet) Config(path string) error {

	configFile, err := ioutil.ReadFile(path)
	if err != nil {
		return fmt.Errorf("GrentonSet Config: error openning config file: %w", err)
	}

	err = json.Unmarshal([]byte(configFile), gs)
	if err != nil {
		return fmt.Errorf("GrentonSet Config: error loading config json: %w", err)
	}

	for _, clu := range gs.Clus {
		for _, light := range clu.Lights {
			if err := light.validateHkType(); err != nil {
				return fmt.Errorf("GrentonSet Config: clu %s: %w", clu.Id, err)
			}
		}
	}

	confDuration, err := time.ParseDuration(fmt.Sprintf("%ds", gs.CycleInSeconds))
	if gs.CycleInSeconds > 0 && err == nil {
		gs.cycleDuration = confDuration
	} else {
		gs.Logf("GrentonSet Config: error parsing cycle duration from config, using default")
		gs.cycleDuration = 10 * time.Second
	}

	if gs.QueryLimit == 0 {
		gs.QueryLimit = 30
	}

	if gs.HkPath == "" {
		gs.HkPath = "hk"
	}

	return nil
}

// startWorkers creates the lanes and the HomeKit syncer and starts them. Call after
// InitClus and before any refresh or HomeKit callback can run.
func (gs *GrentonSet) startWorkers() {
	if gs.QueryLimit <= 0 {
		gs.QueryLimit = 30
	}
	if gs.metrics == nil {
		gs.metrics = noMetrics{}
		switch {
		case gs.InfluxDB.Enabled && gs.InfluxDB.URL == "":
			gs.Logf("InfluxDB: enabled but no URL configured, metrics are not reported")
		case gs.InfluxDB.Enabled:
			gs.influx = newInfluxWriter(gs.InfluxDB, gs.Logf)
			gs.influx.start()
			gs.metrics = influxMetrics{w: gs.influx}
			gs.Logf("InfluxDB: reporting metrics to %s (org %q, bucket %q)", gs.InfluxDB.URL, gs.InfluxDB.Org, gs.InfluxDB.Bucket)
		}
	}
	gs.hkSync = newHkSyncer()
	gs.hkSync.start()

	gs.reader = newLane(laneConfig{
		kind:       readLane,
		url:        gs.Host + gs.ReadPath,
		timeout:    gs.requestTimeout,
		maxItems:   gs.QueryLimit,
		asArray:    true,
		merge:      mergeKeepPending,
		stamps:     &gs.stamps,
		overlap:    &gs.overlap,
		metrics:    gs.metrics,
		logf:       gs.Logf,
		debugf:     gs.Debugf,
		onSent:     gs.readSent,
		onComplete: gs.readCompleted,
	})
	gs.reader.start()

	// Legacy write protocol (the script deployed today): one single-object command per request.
	gs.writer = newLane(laneConfig{
		kind:       writeLane,
		url:        gs.GetSetPath(),
		timeout:    gs.requestTimeout,
		maxItems:   1,
		asArray:    false,
		merge:      mergeCommand,
		stamps:     &gs.stamps,
		overlap:    &gs.overlap,
		metrics:    gs.metrics,
		logf:       gs.Logf,
		debugf:     gs.Debugf,
		onComplete: gs.writeCompleted,
	})
	gs.writer.start()
}

// Stop stops the workers and sends the buffered metrics. Pending submissions are resolved
// as failed. Safe to call more than once.
func (gs *GrentonSet) Stop() {
	gs.stopOnce.Do(gs.stop)
}

func (gs *GrentonSet) stop() {
	if gs.reader != nil {
		gs.reader.stop()
	}
	if gs.writer != nil {
		gs.writer.stop()
	}
	if gs.hkSync != nil {
		gs.hkSync.close()
	}
	if gs.influx != nil {
		gs.influx.close()
	}
}

// readSent is the read lane's send hook: it records the generation of each device in the
// request (concept 3.3, rule 4).
func (gs *GrentonSet) readSent(entries []*laneEntry) {
	for _, e := range entries {
		obj := e.Obj()
		if d, ok := gs.registry.find(obj.Clu, obj.Id); ok {
			e.Meta = d.base().generation()
		}
	}
}

// readCompleted is the read lane's completion hook: a read item is validated and, if the
// ordering rules allow, applied by its device; the outcome is recorded in r.Apply and a
// HomeKit sync is queued for an applied result. The sync is queued after every applied
// read, changed or not, so HomeKit values the user changed without effect (a failed write)
// return to the device state.
func (gs *GrentonSet) readCompleted(e *laneEntry, r *laneResult) {
	if r.Outcome != OutcomeOK {
		return
	}
	obj := e.Obj()
	d, ok := gs.registry.find(obj.Clu, obj.Id)
	if !ok {
		r.Apply, r.Err = ApplyInvalid, fmt.Errorf("no device %s|%s configured", obj.Clu, obj.Id)
		return
	}
	if r.Item == nil { // the item could not be decoded (r.Err)
		r.Apply = ApplyInvalid
		return
	}
	sentGen, _ := e.Meta.(uint64)
	r.Apply, r.Err = d.applyRead(*r.Item, sentGen, r.Stamp)
	switch r.Apply {
	case ApplyChanged, ApplyUnchanged:
		gs.hkSync.mark(d)
	case ApplyDiscarded:
		gs.Debugf("read %s|%s: discarded by the ordering rules", obj.Clu, obj.Id)
	}
}

// writeCompleted is the write lane's completion hook: it logs the outcome of each command
// and records that the command finished (rule 2). If it was the device's latest command, a
// confirm read of the device is queued (rule 5): whether the command ran or not, the read
// brings the device and HomeKit to the state the GATE has. Nobody needs to wait for the
// result. A write response is an acknowledgement only and is never loaded as device state.
func (gs *GrentonSet) writeCompleted(e *laneEntry, r *laneResult) {
	obj := e.Obj()
	what := obj.Kind
	if obj.Cmd != "" && obj.Cmd != "SET" {
		what += " " + obj.Cmd
	}
	ms := r.Completed.Sub(r.Sent).Milliseconds()
	switch r.Outcome {
	case OutcomeOK:
		gs.Logf("write %s|%s (%s): acknowledged in %d ms", obj.Clu, obj.Id, what, ms)
	case OutcomeError:
		gs.Error(fmt.Errorf("write %s|%s (%s) failed: %v", obj.Clu, obj.Id, what, r.Err))
	case OutcomeUncertain:
		gs.Error(fmt.Errorf("write %s|%s (%s): outcome uncertain after %d ms: %v", obj.Clu, obj.Id, what, ms, r.Err))
	}

	d, ok := gs.registry.find(obj.Clu, obj.Id)
	if !ok {
		return
	}
	if d.base().commandFinished(obj.seq) {
		gs.reader.Submit(d.readRequest())
	}
}

// InitClus initialize every clu object, calls inner InitAll and sets pointer to parent struct
func (gs *GrentonSet) InitClus() {
	for _, clu := range gs.Clus {
		clu.set = gs
		clu.InitAll()
	}
	gs.checkDuplicateIds()
	gs.registry = newRegistry(gs.Clus, gs.Logf)
}

// checkDuplicateIds logs one line per accessory ID that is already used by another
// device, naming both. The ID is built from CLU number and device number only, so
// different devices can collide; startup continues because such a setup runs today.
func (gs *GrentonSet) checkDuplicateIds() {
	seen := map[uint64]string{}
	check := func(clu *Clu, co *CluObject) {
		id := co.GetLongId()
		desc := fmt.Sprintf("%q (%s|%s)", co.Name, clu.Id, co.GetMixedId())
		if first, ok := seen[id]; ok {
			gs.Logf("WARNING: duplicate accessory id %x: %s and %s; HomeKit will mix them up", id, first, desc)
			return
		}
		seen[id] = desc
	}
	for _, clu := range gs.Clus {
		for _, d := range clu.Lights {
			check(clu, &d.CluObject)
		}
		for _, d := range clu.Therms {
			check(clu, &d.CluObject)
		}
		for _, d := range clu.MotionSensors {
			check(clu, &d.CluObject)
		}
		for _, d := range clu.Shutters {
			check(clu, &d.CluObject)
		}
	}
}

// GetSetPath returns http path for setting value in grenton gate
func (gs *GrentonSet) GetSetPath() string {
	return gs.Host + gs.SetLightPath
}

// GetAllHkAcc returns a slice with every HomeKit Accessory pointer
func (gs *GrentonSet) GetAllHkAcc() (slc []*accessory.A) {
	slc = []*accessory.A{}

	for _, clu := range gs.Clus {
		slc = append(slc, clu.GetAllHkAcc()...)
	}

	return
}
