package main

import (
	"fmt"
	"log"
	"net/http"
	// "net/url"
	"encoding/json"
	"io/ioutil"
	"sync"
	"time"
	"bytes"

    "github.com/brutella/hc/accessory"
)

type GrentonSet struct {
	Host 			string
	ReadPath		string
	SetLightPath	string

	Clus 			[]*GrentonClu

	HkPin			string
	HkSetupId		string

	FreshInSeconds	int
	CycleInSeconds	int
	Verbose			bool

	lastUpdated   	time.Time
	freshDuration	time.Duration
	cycleDuration	time.Duration
	cycling			*time.Ticker
	waitingAnswer 	bool
	block         	sync.Mutex
}


func (gs *GrentonSet) Debugf(format string, v ...interface{}) {
	if gs.Verbose {
		gs.Logf(format, v...)
	}
}
func (gs *GrentonSet) Logf(format string, v ...interface{}) {
	log.Printf(format, v...)
}
func (gs *GrentonSet) Error(err error) {
	log.Printf("!__Error_:\n%s\n___\n", err.Error())
}

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
	if gs.FreshInSeconds > 0 && err != nil {
		gs.freshDuration = confDuration
	} else {
		gs.Logf("GrentonSet Config: error parsing fresh duration from config, using default")
		gs.freshDuration = 3 * time.Second
	}
	confDuration, err = time.ParseDuration(fmt.Sprintf("%ds", gs.CycleInSeconds))
	if gs.CycleInSeconds > 0 && err != nil {
		gs.cycleDuration = confDuration
	} else {
		gs.Logf("GrentonSet Config: error parsing cycle duration from config, using default")
		gs.cycleDuration = 10 * time.Second
	}
	return nil
}

func (gs *GrentonSet) InitClus() {
	for _, clu := range gs.Clus {
		clu.grentonSet = gs
		clu.InitLights()
	}
}

func (gs *GrentonSet) GetAllHkAcc() (slc []*accessory.Accessory) {
	slc = []*accessory.Accessory{}

	for _, clu := range gs.Clus {
		slc = append(slc, clu.GetAllHkAcc()...)
	}

	return
}

func (gs *GrentonSet) Refresh() {
	if gs.waitingAnswer {
		gs.Debugf("GrentonSet [%v] Refresh: already waiting for answer, skipping\n", &gs)
		return
	}
	gs.waitingAnswer = true
	err := gs.RequestAndUpdate()

	if err != nil {
		gs.Error((fmt.Errorf("GrentonSet [%v] Refresh: request failed: %v\n", &gs, err)))
		gs.waitingAnswer = false
		return
	}

	gs.lastUpdated = time.Now()
	gs.waitingAnswer = false
	gs.Debugf("GrentonSet [%v] Refresh finished\n", &gs)
}

func (gs *GrentonSet) RequestAndUpdate() error {
	gs.Logf("GrentonSet RequestAndUpdate: started [%v]", &gs)

	gs.block.Lock()
	defer gs.block.Unlock()
	
	query := []ReqObject{}
	for _, clu := range gs.Clus {
		for _, light := range clu.Lights {
			if light != nil {
				query = append(query, light.GetReqObject())
			}
		}
	}
	
	jsonQ, _ := json.Marshal(query)
	gs.Debugf("GrentonSet RequestAndUpdate: json query:\n%s\n", jsonQ)
	req, err := http.NewRequest("POST", gs.Host + gs.ReadPath, bytes.NewBuffer(jsonQ))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")

	client := http.Client{
		Timeout: 10 * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
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
	if err != nil {
		return err
	}

	for _, object := range data {
		switch object.Kind	{
		default:
			gs.Logf("GrentonSet RequestAndUpdate: unmatched object kind: %s\n", object.Kind)
		case "DOU":
			light, err := gs.FindLight(object.Clu, object.Id)	
			if err == nil {
				gs.Debugf("GrentonSet RequestAndUpdate: found light from request, state: %+v\n", object)
				err = light.LoadReqObject(object)
				if err != nil {
					gs.Error(fmt.Errorf("GrentonSet RequestAndUpdate loading (%s|%s) failed: %w", object.Clu, object.Id, err))
				}
			}
		}
	}

	return nil
}

func (gs *GrentonSet) FindLight(fClu, fLight string) (found *GrentonLight, err error) {
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


func (gs *GrentonSet) CheckFreshness() bool {
	return time.Since(gs.lastUpdated) <= gs.freshDuration
}

func (gs *GrentonSet) TestAllGrentonGate() {
	gs.Logf("GrentonSet TestAllGrentonGate: Performing Grenton GATE test for all")
	for _, clu := range gs.Clus {
		for _, light := range clu.Lights {
			if !light.TestGrentonGate() {
				gs.Logf("GrentonSet TestAllGrentonGate: Test failed for %s|%s, waiting and repeating", light.Name, light.GetMixedId())
				time.Sleep(3 * time.Second)
				if !light.TestGrentonGate() {
					gs.Logf("GrentonSet TestAllGrentonGate: Test failed again (%s|%s), removing from clu/set", light.Name, light.GetMixedId())
					light = nil
				}
			}
		}
	}
	gs.Logf("GrentonSet TestAllGrentonGate: GATE test finished")
}

func (gs *GrentonSet) StartCycling() {
	go func() {
		gs.cycling = time.NewTicker(gs.cycleDuration)

		for {
			select {
			case <- gs.cycling.C:
				go gs.Refresh()
			}
		}
	}()
}