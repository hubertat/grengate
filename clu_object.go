package main

import (
	"fmt"
	"sync"
	"encoding/json"
	"net/http"
	"bytes"
	"time"
	"io/ioutil"
)

type ReqObject struct {
	Clu  	string
	Id   	string
	Kind 	string
	Cmd  	string `json:",omitempty"`
	Source  string `json:",omitempty"`

	Thermo *Thermo `json:",omitempty"`
	Light  *Light  `json:",omitempty"`
}

type CluObject struct {
	Id   uint32 
	Name string 
	Kind string 

	Req ReqObject	`json:"-"`

	clu   *Clu       `json:"-"`
	block sync.Mutex `json:"-"`
}


func (co *CluObject) GetLongId() uint64 {
	return (uint64(co.clu.GetIntId()) << 32) + uint64(co.Id)
}

// func (co *CluObject) GetFullName() string {
// 	return fmt.Sprintf("%s%4d", co.Kind, co.Id)
// }

func (co *CluObject) GetMixedId() string {
	return fmt.Sprintf("%s%04d", co.Kind, co.Id)
}

func (co *CluObject) TestGrentonGate(ro ReqObject) bool {
	co.block.Lock()
	defer co.block.Unlock()

	jsonQ, err := json.Marshal(ro)
	if err != nil {
		co.clu.set.Logf("TestGrentonGate failed (json) for CluObject: %s | %s", co.Name, co.GetMixedId())
		return false
	}

	req, err := http.NewRequest("POST", co.clu.set.Host+co.clu.set.SetLightPath, bytes.NewBuffer(jsonQ))
	if err != nil {
		co.clu.set.Logf("TestGrentonGate failed (request) for CluObject: %s | %s", co.Name, co.GetMixedId())
		return false
	}

	req.Header.Set("Content-Type", "application/json")

	client := http.Client{
		Timeout: 10 * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		co.clu.set.Logf("TestGrentonGate failed (client.Do) for CluObject: %s | %s", co.Name, co.GetMixedId())
		return false
	}

	defer resp.Body.Close()
	if resp.StatusCode >= 300 || resp.StatusCode < 200 {
		co.clu.set.Logf("TestGrentonGate failed (%s) for CluObject: %s | %s", resp.Status, co.Name, co.GetMixedId())
		return false
	}

	return true
}

func (co *CluObject) Update() error {

	go co.clu.set.Refresh()

	for ix := 0; ix < 500; ix++ {
		if co.clu.set.CheckFreshness() {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}

	return fmt.Errorf("CluObject (%s|%s) Update error: GrentonSet Refresh timeout!\n", co.Name, co.GetMixedId())
}

func (gl *CluObject) SendReq(input ReqObject) (result ReqObject, err error) {

	gl.block.Lock()
	defer gl.block.Unlock()

	input.Cmd = "SET"
	jsonQ, _ := json.Marshal(input)
	gl.clu.set.Debugf("SendReq: \nurl: %s\nquery: %s\n", gl.clu.set.GetSetPath(), jsonQ)
	req, err := http.NewRequest("POST", gl.clu.set.GetSetPath(), bytes.NewBuffer(jsonQ))
	if err != nil {
		gl.clu.set.Error(fmt.Errorf("SendReq: http.NewRequest error: %w", err))
		return
	}

	req.Header.Set("Content-Type", "application/json")
	client := http.Client{
		Timeout: 10 * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		gl.clu.set.Error(fmt.Errorf("SendReq: http.Client.Do error: %w", err))
		return
	}

	defer resp.Body.Close()
	if resp.StatusCode >= 300 || resp.StatusCode < 200 {
		gl.clu.set.Error(fmt.Errorf("SendReq: Received non-success http response from grenton host: %v", resp.Status))
		return
	}

	bodyBytes, _ := ioutil.ReadAll(resp.Body)
	gl.clu.set.Debugf("SendReq received response: \n%s\n", bodyBytes)
	err = json.Unmarshal(bodyBytes, &result)
	if err != nil {
		gl.clu.set.Error(fmt.Errorf("SendReq: error during json Unmarshal: %w", err))
		return
	}

	return
}
