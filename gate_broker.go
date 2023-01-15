package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"sync"
	"time"
)

const httpReadTimeout = 10 * time.Second

type GateBroker struct {
	FlushPeriod    time.Duration
	MaxQueueLength int
	PostPath       string

	queue      []ReqObject
	cErrors    []chan error
	u          updater
	working    sync.Mutex
	requesting sync.Mutex
}

type updater interface {
	update([]ReqObject)
	Logf(string, ...interface{})
	Debugf(string, ...interface{})
}

func (gb *GateBroker) Init(u updater, maxLength int, flushPeriod time.Duration) {
	gb.u = u
	gb.MaxQueueLength = maxLength
	gb.FlushPeriod = flushPeriod
}

func (gb *GateBroker) checkIfPresent(obj ReqObject) bool {
	if len(gb.queue) == 0 {
		return false
	}

	for _, q := range gb.queue {
		if obj.Equal(q) {
			return true
		}
	}

	return false
}

func (gb *GateBroker) Queue(cErr chan error, objects ...ReqObject) (objectsLeft []ReqObject) {
	gb.working.Lock()
	defer gb.working.Unlock()

	if len(objects) == 0 {
		return
	}

	for gb.spaceLeft() == 0 {
		time.Sleep(10 * time.Millisecond)
	}

	if cErr != nil {
		gb.cErrors = append(gb.cErrors, cErr)
	}

	gb.requesting.Lock()
	defer gb.requesting.Unlock()

	emptyQueue := (len(gb.queue) == 0)
	objectsLeft = []ReqObject{}

	for _, obj := range objects {
		if !gb.checkIfPresent(obj) {
			if gb.spaceLeft() > 0 {
				gb.queue = append(gb.queue, obj)
			} else {
				objectsLeft = append(objectsLeft, obj)
			}
		}
	}

	if gb.spaceLeft() == 0 {
		go gb.Flush()
	} else {
		if !emptyQueue {
			time.AfterFunc(gb.FlushPeriod, gb.Flush)
		}
	}
	return
}

func (gb *GateBroker) emptyQueue() {
	gb.queue = []ReqObject{}
	gb.cErrors = []chan error{}
}

func (gb *GateBroker) flushErrors(err error) {
	for _, ce := range gb.cErrors {
		ce <- err
	}
}

func (gb *GateBroker) Flush() {
	defer gb.emptyQueue()
	gb.requesting.Lock()
	defer gb.requesting.Unlock()

	if len(gb.queue) == 0 {
		gb.u.Logf("]![ GateBroker tried to flush on empty queue! Skipping!\n")
		return
	}

	var jsonQ []byte
	if gb.MaxQueueLength > 1 {
		jsonQ, _ = json.Marshal(gb.queue)
	} else {
		jsonQ, _ = json.Marshal(gb.queue[0])
	}
	gb.u.Logf("GateBroker Flush: query prepared, count: %d, bytes: %d", len(gb.queue), len(jsonQ))
	gb.u.Debugf("GateBroker Flush: json query:\n%s\n", jsonQ)
	req, err := http.NewRequest("POST", gb.PostPath, bytes.NewBuffer(jsonQ))
	if err != nil {
		gb.flushErrors(err)
		gb.u.Logf("New POST reques failed: ", err)
		return
	}

	req.Header.Set("Content-Type", "application/json")

	client := http.Client{
		Timeout: httpReadTimeout,
	}
	resp, err := client.Do(req)
	if err != nil {
		gb.flushErrors(err)
		gb.u.Logf("GateBroker RequestAndUpdate http Client failed:\n%v", err)
		return
	}

	defer resp.Body.Close()
	if resp.StatusCode >= 300 || resp.StatusCode < 200 {
		gb.flushErrors(fmt.Errorf("GateBroker received non-success http response from grenton host: %s", resp.Status))
		gb.u.Logf("GateBroker received non-success http response from grenton host: ", resp.Status)
		return
	}

	data := []ReqObject{}

	bodyBytes, _ := ioutil.ReadAll(resp.Body)
	bodyString := string(bodyBytes)
	gb.u.Debugf("GrentonSet RequestAndUpdate: received body:\n%s\n", bodyString)

	err = json.Unmarshal(bodyBytes, &data)
	if err != nil {
		gb.flushErrors(err)
		gb.u.Logf("Unmarshal data error: ", err)
		return
	}
	gb.u.Logf("GateBroker Flush finished, will update.")
	gb.u.update(data)

}

func (gb *GateBroker) spaceLeft() int {
	return gb.MaxQueueLength - len(gb.queue)
}
