package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"strings"
	"time"

	"github.com/brutella/hap/accessory"
	"github.com/pkg/errors"
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

	FreshInSeconds  int
	CycleInSeconds  int
	Verbose         bool
	PerformAutotest bool
	QueryLimit      int

	SetterQueueSize int // Write batch size (default: 5, old: 1)
	SetterFlushMs   int // Write flush period in ms (default: 50, old: 200)

	InfluxDB InfluxConfig

	lastUpdated   time.Time
	freshDuration time.Duration
	cycleDuration time.Duration
	cycling       *time.Ticker

	broker GateBroker
	setter GateBroker

	telemetry      *Telemetry
	influxReporter *InfluxReporter
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

	confDuration, err := time.ParseDuration(fmt.Sprintf("%ds", gs.FreshInSeconds))
	if gs.FreshInSeconds > 0 && err == nil {
		gs.freshDuration = confDuration
	} else {
		gs.Logf("GrentonSet Config: error parsing fresh duration from config, using default")
		gs.freshDuration = 3 * time.Second
	}
	confDuration, err = time.ParseDuration(fmt.Sprintf("%ds", gs.CycleInSeconds))
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

	// Stage 3: Write path optimization defaults
	// NOTE: SetterQueueSize must stay at 1 until Stage 6 (Lua script optimization)
	// The Grenton update-script.lua only handles single objects, not arrays
	if gs.SetterQueueSize == 0 {
		gs.SetterQueueSize = 1 // Must be 1 - Grenton Lua doesn't support batch writes yet
	}
	if gs.SetterFlushMs == 0 {
		gs.SetterFlushMs = 50 // Flush after 50ms for lower latency (was 200ms)
	}

	// Initialize telemetry
	gs.telemetry = &Telemetry{lastReset: time.Now()}

	// Initialize InfluxDB reporter
	gs.influxReporter = NewInfluxReporter(gs.InfluxDB)

	// Initialize brokers with telemetry and InfluxDB reporter
	gs.broker = GateBroker{}
	gs.broker.Init(gs, gs.QueryLimit, gs.freshDuration, gs.telemetry, gs.influxReporter, false)
	gs.broker.PostPath = gs.Host + gs.ReadPath

	gs.setter = GateBroker{}
	setterFlushPeriod := time.Duration(gs.SetterFlushMs) * time.Millisecond
	gs.setter.Init(gs, gs.SetterQueueSize, setterFlushPeriod, gs.telemetry, gs.influxReporter, true)
	gs.setter.PostPath = gs.GetSetPath()

	return nil
}

// InitClus initialize every clu object, calls inner InitAll and sets pointer to parent struct
func (gs *GrentonSet) InitClus() {
	for _, clu := range gs.Clus {
		clu.set = gs
		clu.InitAll()
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

// Refresh is calling RequestAndUpdate function to get fresh values
func (gs *GrentonSet) Refresh() {
	startTime := time.Now()

	query := []ReqObject{}
	for _, clu := range gs.Clus {
		for _, light := range clu.Lights {
			if light != nil {
				query = append(query, light.Req)
			}
		}
		for _, thermo := range clu.Therms {
			if thermo != nil {
				query = append(query, thermo.Req)
			}
		}
		for _, sht := range clu.Shutters {
			if sht != nil {
				query = append(query, sht.Req)
			}
		}
		for _, mosens := range clu.MotionSensors {
			if mosens != nil {
				query = append(query, mosens.Req)
			}
		}
	}

	objectCount := len(query)
	gs.Logf("Refresh: queuing %d objects", objectCount)

	objectsPending := gs.broker.Queue(nil, query...)
	for len(objectsPending) > 0 {
		objectsPending = gs.broker.Queue(nil, objectsPending...)
	}

	// Record refresh telemetry
	elapsed := time.Since(startTime)
	changedCount := 0  // TODO: Track changed count in update() method
	skippedCount := 0  // For Stage 5
	if gs.telemetry != nil {
		gs.telemetry.RecordRefresh(elapsed, objectCount, changedCount, skippedCount)
	}
	if gs.influxReporter != nil {
		gs.influxReporter.ReportRefreshMetrics(objectCount, changedCount, skippedCount, elapsed.Milliseconds())
	}
	gs.Logf("Refresh: completed %d objects in %dms", objectCount, elapsed.Milliseconds())

	gs.lastUpdated = time.Now()

	gs.Debugf("GrentonSet [%v] Refresh finished\n", &gs)
}

// RequestAndUpdate collects all needed objects and make POST request to update state of objects
func (gs *GrentonSet) RequestAndUpdate(query []ReqObject) error {
	gs.Logf("GrentonSet RequestAndUpdate: started [%v]", &gs)

	objectsPending := gs.broker.Queue(nil, query...)
	for len(objectsPending) > 0 {
		objectsPending = gs.broker.Queue(nil, objectsPending...)
	}
	return nil
}

func (gs *GrentonSet) update(data []ReqObject) {
	for _, object := range data {
		var err error
		switch object.Kind {
		default:
			gs.Logf("GrentonSet RequestAndUpdate: unmatched object kind: %s\n", object.Kind)
		case "Light":
			var light *Light
			light, err = gs.FindLight(object.Clu, object.Id)
			if err == nil {
				gs.Debugf("GrentonSet RequestAndUpdate: found light from request, state: %+v\n", object)
				err = light.LoadReqObject(object)
			}
		case "Thermo":
			var thermo *Thermo
			thermo, err = gs.FindThermo(object.Clu, object.Id)
			if err == nil {
				gs.Debugf("GrentonSet RequestAndUpdate: found thermo from request, state: %+v\n", object)
				err = thermo.LoadReqObject(object)
			}
		case "Shutter":
			var shutter *Shutter
			shutter, err = gs.FindShutter(object.Clu, object.Id)
			if err == nil {
				gs.Debugf("GrentonSet RequestAndUpdate: found shutter from request, state: %+v\n", object)
				err = shutter.LoadReqObject(object)
			}
		case "MotionSensor":
			var sensor *MotionSensor
			sensor, err = gs.FindMotionSensor(object.Clu, object.Id)
			if err == nil {
				gs.Debugf("GrentonSet RequestAndUpdate: found motion sensor from request, state: %v\n", object)
				err = sensor.LoadReqObject(object)
			}
		}
		if err != nil {
			gs.Error(errors.Wrapf(err, "RequestAndUpdate loading [%s|%s] failed.", object.Clu, object.Id))
		}
	}
}

// FindThermo returns a Thermo object belonging to selected clu and with selected id
func (gs *GrentonSet) FindThermo(fClu, fLight string) (found *Thermo, err error) {
	gs.Debugf("GrentonSet FindThermo: Looking for thermo: in %s id: %s\n", fLight, fClu)
	for _, clu := range gs.Clus {
		if clu.GetMixedId() == fClu {
			for _, thermo := range clu.Therms {
				if thermo.GetMixedId() == fLight && thermo != nil {
					found = thermo
					err = nil
					return
				}
			}
		}
	}
	err = fmt.Errorf("Thermostat not found [clu: %s id: %s]", fLight, fClu)
	return
}

// FindLight returns a Light object belonging to selected clu and with selected id
func (gs *GrentonSet) FindLight(fClu, fLight string) (found *Light, err error) {
	gs.Debugf("GrentonSet FindLight: Looking for light: in %s id: %s\n", fLight, fClu)
	for _, clu := range gs.Clus {
		if clu.GetMixedId() == fClu {
			for _, light := range clu.Lights {
				if light.GetMixedId() == fLight && light != nil {
					found = light
					err = nil
					return
				}
			}
		}
	}
	err = fmt.Errorf("Light not found [clu: %s id: %s]", fLight, fClu)
	return
}

// FindShutter returns a Shutter object belnging to selected clu
func (gs *GrentonSet) FindShutter(fClu, fShutter string) (found *Shutter, err error) {
	gs.Debugf("GrentonSet FindShutter: Looking for shutter: in %s id: %s\n", fShutter, fClu)
	for _, clu := range gs.Clus {
		if clu.GetMixedId() == fClu {
			for _, sht := range clu.Shutters {
				if sht.GetMixedId() == fShutter && sht != nil {
					found = sht
					err = nil
					return
				}
			}
		}
	}
	err = fmt.Errorf("Shutter not found [clu: %s id: %s]", fShutter, fClu)
	return
}

// FindMotionSensor returns a MotionSensor object from selected clu and with provided id
func (gs *GrentonSet) FindMotionSensor(fClu, fSensor string) (*MotionSensor, error) {
	gs.Debugf("GrentonSet FindMotionSensor: Looking for sensor in clu %s with id %s\n", fClu, fSensor)
	for _, clu := range gs.Clus {
		if strings.EqualFold(clu.GetMixedId(), fClu) {
			for _, sens := range clu.MotionSensors {
				if strings.EqualFold(sens.GetMixedId(), fSensor) && sens != nil {
					return sens, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("sensor not found [clu: %s id: %s]", fClu, fSensor)
}

// CheckFreshness checks if time passed from last refresh is greater than set treshold
func (gs *GrentonSet) CheckFreshness() bool {
	return time.Since(gs.lastUpdated) <= gs.freshDuration
}

// TestAllGrentonGate iterates every object and checks individually if request is success http code
func (gs *GrentonSet) TestAllGrentonGate() {
	gs.Logf("GrentonSet TestAllGrentonGate: Performing Grenton GATE test for all")
	for _, clu := range gs.Clus {
		for _, light := range clu.Lights {
			if !light.TestGrentonGate(light.Req) {
				gs.Logf("GrentonSet TestAllGrentonGate: Test failed for %s|%s, waiting and repeating", light.Name, light.GetMixedId())
				time.Sleep(3 * time.Second)
				if !light.TestGrentonGate(light.Req) {
					gs.Logf("GrentonSet TestAllGrentonGate: Test failed again (%s|%s), removing from clu/set", light.Name, light.GetMixedId())
					light = nil
				}
			}
		}

		for _, light := range clu.Therms {
			if !light.TestGrentonGate(light.Req) {
				gs.Logf("GrentonSet TestAllGrentonGate: Test failed for %s|%s, waiting and repeating", light.Name, light.GetMixedId())
				time.Sleep(3 * time.Second)
				if !light.TestGrentonGate(light.Req) {
					gs.Logf("GrentonSet TestAllGrentonGate: Test failed again (%s|%s), removing from clu/set", light.Name, light.GetMixedId())
					light = nil
				}
			}
		}
	}
	gs.Logf("GrentonSet TestAllGrentonGate: GATE test finished")
}

// StartCycling starts a goroutine which periodically refreshes state of all objects
func (gs *GrentonSet) StartCycling() {
	go func() {
		gs.cycling = time.NewTicker(gs.cycleDuration)

		for {
			select {
			case <-gs.cycling.C:
				go gs.Refresh()
			}
		}
	}()
}
