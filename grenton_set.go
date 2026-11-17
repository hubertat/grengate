package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
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

	lastUpdated   time.Time
	freshDuration time.Duration
	cycleDuration time.Duration
	cycling       *time.Ticker

	broker GateBroker
	setter GateBroker

	registry *registry
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

	gs.initBrokers()

	return nil
}

// initBrokers creates the read and write brokers from the already resolved settings.
func (gs *GrentonSet) initBrokers() {
	gs.broker = GateBroker{}
	gs.broker.Init(gs, gs.QueryLimit, gs.freshDuration)
	gs.broker.PostPath = gs.Host + gs.ReadPath

	gs.setter = GateBroker{}
	gs.setter.Init(gs, 1, 200*time.Millisecond)
	gs.setter.PostPath = gs.GetSetPath()
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

// Refresh queues a read for every device and applies the answers
func (gs *GrentonSet) Refresh() {

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

	objectsPending := gs.broker.Queue(nil, query...)
	for len(objectsPending) > 0 {
		objectsPending = gs.broker.Queue(nil, objectsPending...)
	}

	gs.lastUpdated = time.Now()

	gs.Debugf("GrentonSet [%v] Refresh finished\n", &gs)
}

func (gs *GrentonSet) update(data []ReqObject) {
	for _, object := range data {
		d, ok := gs.registry.find(object.Clu, object.Id)
		if !ok {
			gs.Logf("GrentonSet update: no device %s|%s (%s) configured", object.Clu, object.Id, object.Kind)
			continue
		}
		if err := loadReqObject(d, object); err != nil {
			gs.Error(errors.Wrapf(err, "loading [%s|%s] failed", object.Clu, object.Id))
		}
	}
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
