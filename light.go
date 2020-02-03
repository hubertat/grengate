package main

import (
	"fmt"

	"net/http"
	// "net/url"
	"encoding/json"
	"sync"
	"time"
	"bytes"
	"io/ioutil"

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
	return fmt.Sprintf("%s%04d", gl.Kind, gl.Id)
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
	gl.clu.grentonSet.Logf("HK Lightbulb added (id: %x, type: %d", gl.HkAcc.Accessory.ID, gl.HkAcc.Accessory.Type)
	return gl.HkAcc
}

func (gl *GrentonLight) Sync() {
	gl.HkAcc.Lightbulb.On.SetValue(gl.State)
}

func (gl *GrentonLight) Update() error {

	go gl.clu.grentonSet.Refresh()

	for ix := 0; ix < 500; ix++ {
		if gl.clu.grentonSet.CheckFreshness() {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}

	return fmt.Errorf("GrentonLight Get (for %s) error: GrentonSet Refresh timeout!\n", gl.Name)
}

func (gl *GrentonLight) Get() bool {

	if gl.clu.grentonSet.CheckFreshness() {
		return gl.State
	} 

	err := gl.Update()
	if err != nil {
		gl.clu.grentonSet.Error(err)
	}

	return gl.State

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
	gl.clu.grentonSet.Debugf("GrentonLight Set: \nurl: %s\nquery: %s\n", gl.clu.grentonSet.Host + gl.clu.grentonSet.SetLightPath, jsonQ)
	
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
		err = fmt.Errorf("GrentonLight Set: Received non-success http response from grenton host: %v", resp.Status)
		gl.clu.grentonSet.Error(err)
		return
	}

	bodyBytes, _ := ioutil.ReadAll(resp.Body)

	gl.State = string(bodyBytes) == "1"

	gl.Sync()

	return
}

func (gl *GrentonLight) TestGrentonGate() bool {
	gl.block.Lock()
	defer gl.block.Unlock()
	
	jsonQ, err := json.Marshal(gl.GetReqLight())
	if err != nil {
		gl.clu.grentonSet.Logf("TestGrentonGate failed (json) for light: %s | %s", gl.Name, gl.GetMixedId())
		return false
	}

	req, err := http.NewRequest("POST", gl.clu.grentonSet.Host + gl.clu.grentonSet.SetLightPath, bytes.NewBuffer(jsonQ))
	if err != nil {
		gl.clu.grentonSet.Logf("TestGrentonGate failed (request) for light: %s | %s", gl.Name, gl.GetMixedId())
		return false
	}

	req.Header.Set("Content-Type", "application/json")

	client := http.Client{
		Timeout: 10 * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		gl.clu.grentonSet.Logf("TestGrentonGate failed (client.Do) for light: %s | %s", gl.Name, gl.GetMixedId())
		return false
	}

	defer resp.Body.Close()
	if resp.StatusCode >= 300 || resp.StatusCode < 200 {
		gl.clu.grentonSet.Logf("TestGrentonGate failed (%s) for light: %s | %s",resp.Status, gl.Name, gl.GetMixedId())
		return false
	}

	return true
}