package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"time"

	"github.com/brutella/hap/accessory"
)

// GrentonSet is main struct representing settings and having child Clu structs
type GrentonSet struct {
	Host         string
	ReadPath     string
	SetLightPath string

	BridgeName string

	Clus []*Clu

	HkPin  string
	HkPath string

	FreshInSeconds  int
	CycleInSeconds  int
	Verbose         bool
	PerformAutotest bool
	QueryLimit      int
	InputServerPort int

	lastUpdated   time.Time
	freshDuration time.Duration
	cycleDuration time.Duration
	cycling       *time.Ticker

	broker GateBroker
	setter GateBroker
	input  InputServer
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

	gs.broker = GateBroker{}
	gs.broker.Init(gs, gs.QueryLimit, gs.freshDuration)
	gs.broker.PostPath = gs.Host + gs.ReadPath

	gs.setter = GateBroker{}
	gs.setter.Init(gs, 1, 200*time.Millisecond)
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
	}

	objectsPending := gs.broker.Queue(nil, query...)
	for len(objectsPending) > 0 {
		objectsPending = gs.broker.Queue(nil, objectsPending...)
	}

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
		switch object.Kind {
		default:
			gs.Logf("GrentonSet RequestAndUpdate: unmatched object kind: %s\n", object.Kind)
		case "Light":
			light, err := gs.FindLight(object.Clu, object.Id)
			if err == nil {
				gs.Debugf("GrentonSet RequestAndUpdate: found light from request, state: %+v\n", object)
				err = light.LoadReqObject(object)
				if err != nil {
					gs.Error(fmt.Errorf("GrentonSet RequestAndUpdate loading (%s|%s) failed: %w", object.Clu, object.Id, err))
				}
			}
		case "Thermo":
			thermo, err := gs.FindThermo(object.Clu, object.Id)
			if err == nil {
				gs.Debugf("GrentonSet RequestAndUpdate: found thermo from request, state: %+v\n", object)
				err = thermo.LoadReqObject(object)
				if err != nil {
					gs.Error(fmt.Errorf("GrentonSet RequestAndUpdate loading (%s|%s) failed: %w", object.Clu, object.Id, err))
				}
			}
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
