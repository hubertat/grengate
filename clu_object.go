package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type ReqObject struct {
	Clu    string
	Id     string
	Kind   string
	Cmd    string `json:",omitempty"`
	Source string `json:",omitempty"`

	Thermo       *Thermo       `json:",omitempty"`
	Light        *Light        `json:",omitempty"`
	Shutter      *Shutter      `json:",omitempty"`
	MotionSensor *MotionSensor `json:",omitempty"`
}

func (ro ReqObject) Equal(to ReqObject) bool {
	return strings.EqualFold(ro.Clu, to.Clu) && strings.EqualFold(ro.Id, to.Id) && strings.EqualFold(ro.Kind, to.Kind) && strings.EqualFold(ro.Cmd, to.Cmd)
}

type CluObject struct {
	Id   uint32
	Name string
	Kind string

	Req ReqObject `json:"-"`

	clu *Clu `json:"-"`

	// loaded is set once a valid read result has been applied.
	loaded bool
}

func (co *CluObject) GetLongId() uint64 {
	if co.clu == nil {
		panic("CluObject GetLongId() called, but clu not set.")
	}
	return (uint64(co.clu.GetIntId()) << 32) + uint64(co.Id)
}

// func (co *CluObject) GetFullName() string {
// 	return fmt.Sprintf("%s%4d", co.Kind, co.Id)
// }

func (co *CluObject) GetMixedId() string {
	return fmt.Sprintf("%s%04d", co.Kind, co.Id)
}

func (co *CluObject) TestGrentonGate(ro ReqObject) bool {
	co.clu.block.Lock()
	defer co.clu.block.Unlock()

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

func (gl *CluObject) SendReq(input ReqObject) (result ReqObject, err error) {

	if input.Cmd == "" {
		input.Cmd = "SET"
	}

	errors := make(chan error)

	gl.clu.set.setter.Queue(errors, input)

	err = <-errors

	return
}
