package main

import (
	"fmt"
	"log"
	"net/http"
	"bytes"
	"encoding/json"
	"io/ioutil"
	"sync"
	"time"

	"github.com/brutella/hc/accessory"
)

// GrentonSet is main struct representing settings and having child Clu structs 
type GrentonSet struct {
	Host         string
	ReadPath     string
	SetLightPath string

	Clus []*Clu

	HkPin     string
	HkSetupId string

	FreshInSeconds  int
	CycleInSeconds  int
	Verbose         bool
	PerformAutotest bool
	QueryLimit		int

	lastUpdated   time.Time
	freshDuration time.Duration
	cycleDuration time.Duration
	cycling       *time.Ticker
	waitingAnswer bool
	block         sync.Mutex
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
func (gs *GrentonSet) GetAllHkAcc() (slc []*accessory.Accessory) {
	slc = []*accessory.Accessory{}

	for _, clu := range gs.Clus {
		slc = append(slc, clu.GetAllHkAcc()...)
	}

	return
}

// Refres is calling RequestAndUpdate function to get fresh values
func (gs *GrentonSet) Refresh() {
	if gs.waitingAnswer {
		gs.Debugf("GrentonSet [%v] Refresh: already waiting for answer, skipping\n", &gs)
		return
	}
	gs.waitingAnswer = true

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

	for ix := 0; ix * gs.QueryLimit < len(query); ix++ {

		start := ix * gs.QueryLimit
		stop := (ix + 1) * gs.QueryLimit
		
		gs.Debugf("GrentonSet Refresh doing pass from %d until %d.\n", start, stop)

		if stop > len(query) {
			stop = len(query)
		}

		err := gs.RequestAndUpdate(query[start:stop])

		if err != nil {
			gs.Error((fmt.Errorf("GrentonSet [%v] Refresh: request failed: %v\n", &gs, err)))
			gs.waitingAnswer = false
			return
		}
	}

	gs.lastUpdated = time.Now()
	gs.waitingAnswer = false
	gs.Debugf("GrentonSet [%v] Refresh finished\n", &gs)
}

// RequestAndUpdate collects all needed objects and make POST request to update state of objects
func (gs *GrentonSet) RequestAndUpdate(query []ReqObject) error {
	gs.Logf("GrentonSet RequestAndUpdate: started [%v]", &gs)

	gs.block.Lock()
	defer gs.block.Unlock()


	jsonQ, _ := json.Marshal(query)
	gs.Logf("GrentonSet RequestAndUpdate: query prepared, size: %d", len(jsonQ))
	gs.Debugf("GrentonSet RequestAndUpdate: json query:\n%s\n", jsonQ)
	req, err := http.NewRequest("POST", gs.Host+gs.ReadPath, bytes.NewBuffer(jsonQ))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")

	client := http.Client{
		Timeout: 10 * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("GrentonSet RequestAndUpdate http Client failed:\n%v", err)
	}

	defer resp.Body.Close()
	if resp.StatusCode >= 300 || resp.StatusCode < 200 {
		log.Print(resp.Status)
		return fmt.Errorf("Received non-success http response from grenton host.")
	}

	data := []ReqObject{}

	bodyBytes, _ := ioutil.ReadAll(resp.Body)
	bodyString := string(bodyBytes)
	gs.Debugf("GrentonSet RequestAndUpdate: received body:\n%s\n", bodyString)

	err = json.Unmarshal(bodyBytes, &data)
	log.Printf("\n\n%+v\n", data)
	if err != nil {
		return err
	}

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

	return nil
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
