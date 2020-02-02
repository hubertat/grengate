package main

import (
	"fmt"
	"log"
	"net/http"
	// "net/url"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"
	"bytes"
	"io/ioutil"

    "github.com/brutella/hc"
    "github.com/brutella/hc/accessory"
)

type ReqLight struct {
	Clu 	string		`json:"clu"`
	Dout 	string		`json:"dout"`
	Cmd		string		`json:"cmd,omitempty"`
	State	int			`json:"state,omitempty"`
}

type GrentonLight struct {
	Id    	uint32
	Name 	string
	State 	bool
	Kind	string

	clu		*GrentonClu
	block	sync.Mutex

	HkAcc	*accessory.Lightbulb
}

func (gl *GrentonLight) GetLongId() uint64 {
	return (uint64(gl.clu.GetIntId()) << 32) + uint64(gl.Id)
}

func (gl *GrentonLight) GetFullName() string {
	return fmt.Sprintf("%s%4d", gl.Kind, gl.Id)
}

func (gl *GrentonLight) GetMixedId() string {
	return fmt.Sprintf("%s%4d", gl.Kind, gl.Id)
}

func (gl *GrentonLight) GetReqLight() ReqLight {
	return ReqLight{
		Dout:  gl.GetMixedId(),
		Clu: gl.clu.GetMixedId(),
	}
}

func (gl *GrentonLight) AppendHk() *accessory.Lightbulb {
	info := accessory.Info{
		Name: gl.Name,
		SerialNumber: fmt.Sprintf("%d", gl.Id),
		Manufacturer: "Grenton",
		Model: gl.Kind,
		ID: gl.GetLongId(),
	}
	
	gl.HkAcc = accessory.NewLightbulb(info)
	gl.HkAcc.Lightbulb.On.OnValueRemoteUpdate(gl.Set)
	gl.HkAcc.Lightbulb.On.OnValueRemoteGet(gl.Get)
	log.Printf("HK Lightbulb added (id: %x, type: %d", gl.HkAcc.Accessory.ID, gl.HkAcc.Accessory.Type)
	return gl.HkAcc
}

func (gl *GrentonLight) Get() bool {
	if gl.clu.grentonSet.CheckFreshness() {
		return gl.State
	} 

	gl.clu.grentonSet.Refresh()

	for {
		if gl.clu.grentonSet.CheckFreshness() {
			return gl.State
		}
		time.Sleep(20 * time.Millisecond)
	}

}

func (gl *GrentonLight) Set(state bool) {
	
	var err error
	gl.block.Lock()
	defer gl.block.Unlock()

	stt := gl.GetReqLight()
	if state {
		stt.Cmd = "ON"
	} else {
		stt.Cmd = "OFF"
	}

	jsonQ, _ := json.Marshal(stt)
	// log.Printf("Setting state, url:__\n %s", gl.clu.grentonSet.Host + gl.clu.grentonSet.SetLightPath)
	// log.Printf("Setting state, query:__\n %s\n\n", jsonQ)
	req, err := http.NewRequest("POST", gl.clu.grentonSet.Host + gl.clu.grentonSet.SetLightPath, bytes.NewBuffer(jsonQ))
	if err != nil {
		return
	}

	req.Header.Set("Content-Type", "application/json")

	client := http.Client{
		Timeout: 10 * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		return
	}

	defer resp.Body.Close()
	if resp.StatusCode >= 300 || resp.StatusCode < 200 {
		log.Print(resp.Status)
		err = fmt.Errorf("Received non-success http response from grenton host.")
		return
	}

	bodyBytes, _ := ioutil.ReadAll(resp.Body)
	if string(bodyBytes) == "1" {
		gl.State = true
	} 

	return
}

type GrentonClu struct {
	Id     			string
	Name			string
	Lights 			[]*GrentonLight

	grentonSet		*GrentonSet
	block			sync.Mutex
}


func (gc *GrentonClu) GetIntId() uint32 {
	cluIdS := gc.Id[3:]
	var base int
	if strings.HasPrefix(cluIdS, "_") {
		base = 16
	} else {
		base = 10
	}
	uVal, err := strconv.ParseUint(cluIdS[1:], base, 32)
	if err != nil {
		log.Fatal("Converting clu id [%s] (to uint) failed: %v", gc.Id, err)
	}
	return uint32(uVal)

}
func (gc *GrentonClu) GetMixedId() string {
	return gc.Id
}
func (gc *GrentonClu) InitLights() {
	for _, light := range gc.Lights {
		light.clu = gc
		light.AppendHk()
	}
}

func (gc *GrentonClu) GetAllHkAcc() (slc []*accessory.Accessory) {
	slc = []*accessory.Accessory{}

	for _, light := range gc.Lights {
		slc = append(slc, light.HkAcc.Accessory)
	}

	return
}


type GrentonSet struct {
	Host 			string
	ReadPath		string
	SetLightPath	string

	Clus 			[]*GrentonClu

	HkPin			string
	HkSetupId		string

	FreshInSeconds	int

	lastUpdated   	time.Time
	freshDuration	time.Duration
	waitingAnswer 	bool
	block         	sync.Mutex
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
		log.Printf("GrentonSet [%v] Refresh: already waiting for answer, skipping\n", &gs)
		return
	}
	gs.waitingAnswer = true
	err := gs.RequestAndUpdate()

	if err != nil {
		log.Print(err)
		log.Printf("GrentonSet [%v] Refresh: request failed: %v\n", &gs, err)
		gs.waitingAnswer = false
		return
	}

	gs.lastUpdated = time.Now()
	gs.waitingAnswer = false
	log.Printf("GrentonSet [%v] Refresh finished\n", &gs)
}

func (gs *GrentonSet) RequestAndUpdate() error {
	log.Print("GrentonSet RequestAndUpdate +|-/|+|-/|+|-/|")

	gs.block.Lock()
	defer gs.block.Unlock()
	
	query := []ReqLight{}
	for _, clu := range gs.Clus {
		for _, light := range clu.Lights {
			query = append(query, light.GetReqLight())
		}
	}
	// log.Print(query)
	jsonQ, _ := json.Marshal(query)
	// log.Printf("%s", jsonQ)
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

	data := []ReqLight{}

	// bodyBytes, _ := ioutil.ReadAll(resp.Body)
    // bodyString := string(bodyBytes)
    // log.Printf("----------\n\n%s\n\n", bodyString)

    // err = json.Unmarshal(bodyBytes, &data)
	err = json.NewDecoder(resp.Body).Decode(&data)

	if err != nil {
		return err
	}

	// filled := 0
	for _, resLight := range data {
		myLight, err := gs.FindLight(resLight.Clu, resLight.Dout)
		if err == nil {
			// log.Printf("Non err light processed, state: %v\n", resLight.State)
			myLight.State = resLight.State == 1
			// log.Printf("Light: %-v\n", myLight)
			// filled++
		}
	}

	return nil
}

func (gs *GrentonSet) FindLight(fClu, fLight string) (found *GrentonLight, err error) {
	// log.Printf("Looking for light: in %s id: %s\n", fLight, fClu)
	for _, clu := range gs.Clus {
		if clu.GetMixedId() == fClu {
			for _, light := range clu.Lights {
				if light.GetMixedId() == fLight {
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


func (gs *GrentonSet) FindCluOrNew(cluId string) (*GrentonClu) {
	for _, clu := range gs.Clus {
		if clu.GetMixedId() == cluId {
			return clu
		}
	}

	newClu := &GrentonClu{Id: cluId}
	gs.Clus = append(gs.Clus, newClu)
	log.Printf("CLU added %s", newClu.Id)
	return newClu
}


func (gs *GrentonSet) CheckFreshness() bool {
	return time.Since(gs.lastUpdated) <= gs.freshDuration
}


func main() {
	log.Print("Starting grengate")

	configPath := "./config.json"
	configFile, err := ioutil.ReadFile(configPath)
	if err != nil {
		log.Fatalf("Error openning config file: %v", err)
	}

	gren := GrentonSet{}
	err = json.Unmarshal([]byte(configFile), &gren)
	if err != nil {
		log.Fatalf("Error loading config: %v", err)
	}

	// gren.Host = "http://10.100.81.73/"
	// gren.ReadPath = "multi/read/"
	// gren.SetLightPath = "homebridge"
	

	if gren.FreshInSeconds > 0 {
		gren.freshDuration, err = time.ParseDuration(fmt.Sprintf("%ds", gren.FreshInSeconds))
		if err != nil {
			log.Print("error parsing duration from config, using default")
			gren.freshDuration = 6 * time.Second
		}
	} else {
		gren.freshDuration = 6 * time.Second	
	}

	gren.InitClus()

	log.Print("HomeKit init")

	// create an accessory
    info := accessory.Info{Name: "Lamp"}
    ac := accessory.NewSwitch(info)
  
    config := hc.Config{
    	Pin: gren.HkPin,
    	SetupId: gren.HkSetupId,
    }
    t, err := hc.NewIPTransport(config, ac.Accessory, gren.GetAllHkAcc()...)
    if err != nil {
        log.Panic(err)
    }
    
    hc.OnTermination(func(){
        <-t.Stop()
    })
    
    t.Start()




}
