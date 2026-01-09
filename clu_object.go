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

func (co *CluObject) Update() error {
	if co.clu.set.CheckFreshness() {
		return nil
	}

	return co.clu.set.RequestAndUpdate([]ReqObject{co.Req})

}

func (gl *CluObject) SendReq(input ReqObject) (result ReqObject, err error) {
	// Start timing total command duration
	commandStartTime := time.Now()

	if input.Cmd == "" {
		input.Cmd = "SET"
	}

	errors := make(chan error)

	// Time when we start queueing
	queueStartTime := time.Now()

	gl.clu.set.setter.Queue(errors, input)

	// Queue wait time ends when flush completes (err received)
	err = <-errors
	queueWaitDuration := time.Since(queueStartTime)

	// Total command duration
	totalDuration := time.Since(commandStartTime)

	// Record command telemetry
	if gl.clu.set.telemetry != nil {
		gl.clu.set.telemetry.RecordCommand(totalDuration, queueWaitDuration)
	}
	if gl.clu.set.influxReporter != nil {
		gl.clu.set.influxReporter.ReportCommandMetrics(
			totalDuration.Milliseconds(),
			queueWaitDuration.Milliseconds(),
		)
	}

	gl.clu.set.Debugf("SendReq: total=%dms, queueWait=%dms",
		totalDuration.Milliseconds(), queueWaitDuration.Milliseconds())

	return

	// gl.clu.block.Lock()
	// defer gl.clu.block.Unlock()

	// jsonQ, _ := json.Marshal(input)
	// gl.clu.set.Debugf("SendReq: \nurl: %s\nquery: %s\n", gl.clu.set.GetSetPath(), jsonQ)
	// req, err := http.NewRequest("POST", gl.clu.set.GetSetPath(), bytes.NewBuffer(jsonQ))
	// if err != nil {
	// 	gl.clu.set.Error(fmt.Errorf("SendReq: http.NewRequest error: %w", err))
	// 	return
	// }

	// req.Header.Set("Content-Type", "application/json")
	// client := http.Client{
	// 	Timeout: 10 * time.Second,
	// }
	// resp, err := client.Do(req)
	// if err != nil {
	// 	gl.clu.set.Error(fmt.Errorf("SendReq: http.Client.Do error: %w", err))
	// 	return
	// }

	// defer resp.Body.Close()
	// if resp.StatusCode >= 300 || resp.StatusCode < 200 {
	// 	gl.clu.set.Error(fmt.Errorf("SendReq: Received non-success http response from grenton host: %v", resp.Status))
	// 	return
	// }

	// bodyBytes, _ := ioutil.ReadAll(resp.Body)
	// gl.clu.set.Debugf("SendReq received response: \n%s\n", bodyBytes)
	// err = json.Unmarshal(bodyBytes, &result)
	// if err != nil {
	// 	gl.clu.set.Error(fmt.Errorf("SendReq: error during json Unmarshal: %w", err))
	// 	return
	// }

	// return
}
