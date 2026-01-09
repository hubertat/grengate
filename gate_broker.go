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

	queueSpace chan struct{}   // Channel-based queue capacity management
	queueMap   map[string]bool // O(1) duplicate checking

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

	// Initialize channel-based queue management
	gb.queueSpace = make(chan struct{}, maxLength)
	// Fill channel with available slots
	for i := 0; i < maxLength; i++ {
		gb.queueSpace <- struct{}{}
	}
	gb.queueMap = make(map[string]bool)
}


func (gb *GateBroker) Queue(cErr chan error, objects ...ReqObject) (objectsLeft []ReqObject) {
	gb.working.Lock()
	defer gb.working.Unlock()

	if len(objects) == 0 {
		return
	}

	// Block until at least one space is available
	// This is the channel-based equivalent of: for spaceLeft() == 0 { sleep(10ms) }
	<-gb.queueSpace

	// Add error channel BEFORE processing objects (like old code)
	// This ensures SendReq won't block forever even if all objects are duplicates/rejected
	if cErr != nil {
		gb.requesting.Lock()
		gb.cErrors = append(gb.cErrors, cErr)
		gb.requesting.Unlock()
	}

	gb.requesting.Lock()
	defer gb.requesting.Unlock()

	emptyQueue := (len(gb.queue) == 0)
	objectsLeft = []ReqObject{}
	usedFirstSlot := false

	for _, obj := range objects {
		// Try to get a slot
		// First object uses the slot we already acquired above (blocking)
		// Subsequent objects try non-blocking
		var gotSlot bool
		if !usedFirstSlot {
			gotSlot = true
			usedFirstSlot = true
		} else {
			select {
			case <-gb.queueSpace:
				gotSlot = true
			default:
				gotSlot = false
			}
		}

		if !gotSlot {
			// No space available, add to objectsLeft for caller to retry
			objectsLeft = append(objectsLeft, obj)
			if gb.telemetry != nil {
				gb.telemetry.RecordQueueReject()
			}
			continue
		}

		// Check for duplicate using O(1) map lookup
		key := obj.getKey()
		if gb.queueMap[key] {
			// Duplicate found, return the slot and skip
			gb.queueSpace <- struct{}{}
			if gb.telemetry != nil {
				gb.telemetry.RecordQueueDuplicate()
			}
			continue
		}

		// Not a duplicate, add to queue (slot is consumed)
		gb.queue = append(gb.queue, obj)
		gb.queueMap[key] = true
		if gb.telemetry != nil {
			gb.telemetry.RecordQueueAdd()
		}
	}

	// Trigger flush
	if len(gb.queue) >= gb.MaxQueueLength {
		go gb.Flush()
	} else if emptyQueue {
		time.AfterFunc(gb.FlushPeriod, gb.Flush)
	}

	return
}

func (gb *GateBroker) emptyQueue() {
	// Save queue length before clearing
	flushedCount := len(gb.queue)

	// Clear queue and errors
	gb.queue = []ReqObject{}
	gb.cErrors = []chan error{}

	// Return space to channel for flushed items
	for i := 0; i < flushedCount; i++ {
		gb.queueSpace <- struct{}{}
	}

	// Clear duplicate tracking map
	gb.queueMap = make(map[string]bool)
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
	cluCount := gb.countUniqueCLUs()
	var jsonQ []byte
	if gb.MaxQueueLength > 1 {
		jsonQ, _ = json.Marshal(gb.queue)
	} else {
		jsonQ, _ = json.Marshal(gb.queue[0])
	}
	requestBytes := len(jsonQ)
	gb.u.Logf("GateBroker Flush: query prepared, count: %d, clus: %d, bytes: %d", objectCount, cluCount, requestBytes)
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
			gb.influxReporter.ReportFlushMetrics(objectCount, cluCount, requestBytes, elapsed.Milliseconds(), gb.isSetter, cluId, objectId, err)
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
			gb.influxReporter.ReportFlushMetrics(objectCount, cluCount, requestBytes, elapsed.Milliseconds(), gb.isSetter, cluId, objectId, err)
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
			gb.influxReporter.ReportFlushMetrics(objectCount, cluCount, requestBytes, elapsed.Milliseconds(), gb.isSetter, cluId, objectId, statusErr)
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
			gb.influxReporter.ReportFlushMetrics(objectCount, cluCount, requestBytes, elapsed.Milliseconds(), gb.isSetter, cluId, objectId, err)
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
		gb.influxReporter.ReportFlushMetrics(objectCount, cluCount, requestBytes, elapsed.Milliseconds(), gb.isSetter, cluId, objectId, nil)
	}

	gb.u.Logf("GateBroker Flush: completed %d objects, %d CLUs in %dms", objectCount, cluCount, elapsed.Milliseconds())
	gb.u.update(data)

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

// countUniqueCLUs returns the number of unique CLUs in the queue
func (gb *GateBroker) countUniqueCLUs() int {
	cluMap := make(map[string]bool)
	for _, obj := range gb.queue {
		cluMap[obj.Clu] = true
	}
	return len(cluMap)
}
