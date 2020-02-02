package main

import (
	"fmt"
	"log"
	"net/http"
	// "net/url"
	"encoding/json"
	"sync"
	"time"
	"bytes"
	"io/ioutil"
)

// type CluLight struct {
// 	Id    string
// 	Clu   string
// 	State int
// }

type ReqLight struct {
	Clu 	string		`json:"clu"`
	Dout 	string		`json:"dout"`
	Cmd		string		`json:"cmd,omitempty"`
	State	int			`json:"state,omitempty"`
}

type GrentonLight struct {
	Id    	string
	CluId	string
	State 	bool
	Block	sync.Mutex
}

// func (gl *GrentonLight) GetCluLight() CluLight {
// 	return CluLight{
// 		Id:  gl.Id,
// 		Clu: gl.CluId,
// 	}
// }
func (gl *GrentonLight) GetReqLight() ReqLight {
	return ReqLight{
		Dout:  gl.Id,
		Clu: gl.CluId,
	}
}

type GrentonClu struct {
	Id     string
	Lights []*GrentonLight
	Block	sync.Mutex
}


func (gc *GrentonClu) AppendLight(lightId string) (*GrentonLight) {
	gc.Block.Lock()
	defer gc.Block.Unlock()

	newLight := &GrentonLight{Id: lightId, CluId: gc.Id}
	log.Printf("Light added %s", newLight.Id)
	gc.Lights = append(gc.Lights, newLight)
	// log.Print(gc)
	return newLight
}

type GrentonSet struct {
	Host 			string
	ReadPath		string
	SetLightPath	string

	Clus 			[]*GrentonClu

	LastUpdated   	time.Time
	FreshDuration	time.Duration
	WaitingAnswer 	bool
	Block         	sync.Mutex
}

func (gs *GrentonSet) Refresh() {
	if gs.WaitingAnswer {
		log.Printf("GrentonSet [%v] Refresh: already waiting for answer, skipping\n", &gs)
		return
	}
	gs.WaitingAnswer = true
	err := gs.RequestAndUpdate()

	if err != nil {
		log.Print(err)
		log.Printf("GrentonSet [%v] Refresh: request failed: %v\n", &gs, err)
		gs.WaitingAnswer = false
		return
		// err = gs.RequestAndUpdate()
		// if err != nil {
		// 	log.Print(err)
		// 	log.Print("GrentonSet Refresh: request failed, giving up.")
		// 	return
		// }
	}

	gs.LastUpdated = time.Now()
	gs.WaitingAnswer = false
	log.Printf("GrentonSet [%v] Refresh finished\n", &gs)
}

func (gs *GrentonSet) RequestAndUpdate() error {
	log.Print("GrentonSet RequestAndUpdate +|-/|+|-/|+|-/|")

	gs.Block.Lock()
	defer gs.Block.Unlock()
	
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
		if clu.Id == fClu {
			for _, light := range clu.Lights {
				if light.Id == fLight {
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
		if clu.Id == cluId {
			return clu
		}
	}

	newClu := &GrentonClu{Id: cluId}
	gs.Clus = append(gs.Clus, newClu)
	log.Printf("CLU added %s", newClu.Id)
	return newClu
}

func (gs *GrentonSet) AppendLight(cluId, lightId string) (*GrentonLight) {
	gs.Block.Lock()
	defer gs.Block.Unlock()
		
	clu := gs.FindCluOrNew(cluId)
	newL := clu.AppendLight(lightId)

	return newL
}

func (gs *GrentonSet) SetLight(light *GrentonLight, state bool) (raw string, err error) {
	light.Block.Lock()
	defer light.Block.Unlock()

	stt := light.GetReqLight()
	if state {
		stt.Cmd = "ON"
	} else {
		stt.Cmd = "OFF"
	}

	jsonQ, _ := json.Marshal(stt)
	// log.Printf("SetLight req:__\n %s\n\n", jsonQ)
	req, err := http.NewRequest("POST", gs.Host + gs.SetLightPath, bytes.NewBuffer(jsonQ))
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
    raw = string(bodyBytes)
    
	return
}

func (gs *GrentonSet) HandleLight(w http.ResponseWriter, r *http.Request) {

	rL := &ReqLight{}
	
    // bodyBytes, _ := ioutil.ReadAll(r.Body)
    // bodyString := string(bodyBytes)
    // log.Printf("ReqLight: \n%s\n", bodyString)

    // err := json.Unmarshal(bodyBytes, rL)
	err := json.NewDecoder(r.Body).Decode(rL)
	if err !=  nil {
		log.Print("GrentonSet HandleLight: error processing request: ", err)
		return
	}
	
	log.Printf("GrentonSet HandleLight: started for %s|%s", rL.Clu, rL.Dout)

	light, err := gs.FindLight(rL.Clu, rL.Dout)
	if err != nil {
		// http.Error(w, "Light not found", 404)
		log.Print("GrentonSet HandleLight: light not found, adding new and requestign data")
		light = gs.AppendLight(rL.Clu, rL.Dout)
		err = gs.RequestAndUpdate()
		if err != nil {
			log.Print("Error during request for new light: %v", err)
		}
	}

	var rawState string

	switch rL.Cmd {
	case "ON":
		rawState, err = gs.SetLight(light, true)
	case "OFF": 
		rawState, err = gs.SetLight(light, false)
	default:
		err = nil
	}
	if err != nil {
		log.Print("GrentonSet HandleLight error during SetLight: %v", err)
		return
	}

	if len(rawState) > 0 {
		// log.Print(rawState)
		fmt.Fprint(w, rawState)
		return
	}

	
	if time.Since(gs.LastUpdated) > gs.FreshDuration {
		log.Printf("GrentonSet HandleLight (%s|%s): data not fresh, refreshing...", rL.Clu, rL.Dout)
		go gs.Refresh()
	}

	for time.Since(gs.LastUpdated) > gs.FreshDuration {

		time.Sleep(10 * time.Millisecond)
	}

	if light.State {
		fmt.Fprint(w, "1")
	} else {
		fmt.Fprint(w, "0")
	}
}

func main() {
	log.Print("Starting grengate")

	gren := GrentonSet{}
	gren.Host = "http://10.100.81.73/"
	gren.ReadPath = "multi/read/"
	gren.SetLightPath = "homebridge"
	gren.FreshDuration = 6 * time.Second

	log.Print("Starting http server")
	http.HandleFunc("/grengate/light/", gren.HandleLight)
	log.Fatal(http.ListenAndServe(":7732", nil))

	
	// rL := ReqLight{Clu: "CLU_AAxx", Dout: "DO0099"}
	// light, err := gren.FindLight(rL.Clu, rL.Dout)

	// if  err != nil {
	// 	log.Print("No light found, adding:")
	// 	light = gren.AppendLight(rL.Clu, rL.Dout)
	// }

	// log.Print(gren)
	// log.Print(light)

}
