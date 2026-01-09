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

	telemetry      *Telemetry
	influxReporter *InfluxReporter
	isSetter       bool
}

type updater interface {
	update([]ReqObject)
	Logf(string, ...interface{})
	Debugf(string, ...interface{})
}

func (gb *GateBroker) Init(u updater, maxLength int, flushPeriod time.Duration, telemetry *Telemetry, influxReporter *InfluxReporter, isSetter bool) {
	gb.u = u
	gb.MaxQueueLength = maxLength
	gb.FlushPeriod = flushPeriod
	gb.telemetry = telemetry
	gb.influxReporter = influxReporter
	gb.isSetter = isSetter
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
				if gb.telemetry != nil {
					gb.telemetry.RecordQueueAdd()
				}
			} else {
				objectsLeft = append(objectsLeft, obj)
				if gb.telemetry != nil {
					gb.telemetry.RecordQueueReject()
				}
			}
		} else {
			// Record duplicate
			if gb.telemetry != nil {
				gb.telemetry.RecordQueueDuplicate()
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
	startTime := time.Now()
	defer gb.emptyQueue()
	gb.requesting.Lock()
	defer gb.requesting.Unlock()

	if len(gb.queue) == 0 {
		gb.u.Logf("]![ GateBroker tried to flush on empty queue! Skipping!\n")
		return
	}

	objectCount := len(gb.queue)
	var jsonQ []byte
	if gb.MaxQueueLength > 1 {
		jsonQ, _ = json.Marshal(gb.queue)
	} else {
		jsonQ, _ = json.Marshal(gb.queue[0])
	}
	gb.u.Logf("GateBroker Flush: query prepared, count: %d, bytes: %d", objectCount, len(jsonQ))
	gb.u.Debugf("GateBroker Flush: json query:\n%s\n", jsonQ)
	req, err := http.NewRequest("POST", gb.PostPath, bytes.NewBuffer(jsonQ))
	if err != nil {
		gb.flushErrors(err)
		gb.u.Logf("New POST reques failed: ", err)
		// Record failed flush
		elapsed := time.Since(startTime)
		cluId, objectId := gb.getCluAndObjectId()
		if gb.telemetry != nil {
			if gb.isSetter {
				gb.telemetry.RecordSetterFlush(elapsed, objectCount, err)
			} else {
				gb.telemetry.RecordFlush(elapsed, objectCount, err)
			}
		}
		if gb.influxReporter != nil {
			gb.influxReporter.ReportFlushMetrics(objectCount, elapsed.Milliseconds(), gb.isSetter, cluId, objectId, err)
		}
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
		// Record failed flush
		elapsed := time.Since(startTime)
		cluId, objectId := gb.getCluAndObjectId()
		if gb.telemetry != nil {
			if gb.isSetter {
				gb.telemetry.RecordSetterFlush(elapsed, objectCount, err)
			} else {
				gb.telemetry.RecordFlush(elapsed, objectCount, err)
			}
		}
		if gb.influxReporter != nil {
			gb.influxReporter.ReportFlushMetrics(objectCount, elapsed.Milliseconds(), gb.isSetter, cluId, objectId, err)
		}
		return
	}

	defer resp.Body.Close()
	if resp.StatusCode >= 300 || resp.StatusCode < 200 {
		statusErr := fmt.Errorf("GateBroker received non-success http response from grenton host: %s", resp.Status)
		gb.flushErrors(statusErr)
		gb.u.Logf("GateBroker received non-success http response from grenton host: ", resp.Status)
		// Record failed flush
		elapsed := time.Since(startTime)
		cluId, objectId := gb.getCluAndObjectId()
		if gb.telemetry != nil {
			if gb.isSetter {
				gb.telemetry.RecordSetterFlush(elapsed, objectCount, statusErr)
			} else {
				gb.telemetry.RecordFlush(elapsed, objectCount, statusErr)
			}
		}
		if gb.influxReporter != nil {
			gb.influxReporter.ReportFlushMetrics(objectCount, elapsed.Milliseconds(), gb.isSetter, cluId, objectId, statusErr)
		}
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
		// Record failed flush
		elapsed := time.Since(startTime)
		cluId, objectId := gb.getCluAndObjectId()
		if gb.telemetry != nil {
			if gb.isSetter {
				gb.telemetry.RecordSetterFlush(elapsed, objectCount, err)
			} else {
				gb.telemetry.RecordFlush(elapsed, objectCount, err)
			}
		}
		if gb.influxReporter != nil {
			gb.influxReporter.ReportFlushMetrics(objectCount, elapsed.Milliseconds(), gb.isSetter, cluId, objectId, err)
		}
		return
	}

	// Record successful flush
	elapsed := time.Since(startTime)
	cluId, objectId := gb.getCluAndObjectId()
	if gb.telemetry != nil {
		if gb.isSetter {
			gb.telemetry.RecordSetterFlush(elapsed, objectCount, nil)
		} else {
			gb.telemetry.RecordFlush(elapsed, objectCount, nil)
		}
	}
	if gb.influxReporter != nil {
		gb.influxReporter.ReportFlushMetrics(objectCount, elapsed.Milliseconds(), gb.isSetter, cluId, objectId, nil)
	}

	gb.u.Logf("GateBroker Flush: completed %d objects in %dms", objectCount, elapsed.Milliseconds())
	gb.u.update(data)

}

func (gb *GateBroker) spaceLeft() int {
	return gb.MaxQueueLength - len(gb.queue)
}

// getCluAndObjectId extracts CLU and object IDs from queue (for single-object operations)
func (gb *GateBroker) getCluAndObjectId() (string, string) {
	// Only return IDs if single object in queue (typical for setter/write operations)
	if len(gb.queue) == 1 {
		return gb.queue[0].Clu, gb.queue[0].Id
	}
	// Multiple objects or empty queue - return empty strings
	return "", ""
}
