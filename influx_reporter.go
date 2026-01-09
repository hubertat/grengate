package main

import (
	"time"

	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/influxdata/influxdb-client-go/v2/api"
)

// InfluxConfig holds InfluxDB configuration
type InfluxConfig struct {
	Enabled bool   `json:"Enabled"`
	URL     string `json:"URL"`
	Token   string `json:"Token"`
	Org     string `json:"Org"`
	Bucket  string `json:"Bucket"`
}

// InfluxReporter sends metrics to InfluxDB v2
type InfluxReporter struct {
	client   influxdb2.Client
	writeAPI api.WriteAPI
	enabled  bool
	org      string
	bucket   string
}

// NewInfluxReporter creates a new InfluxDB reporter
func NewInfluxReporter(cfg InfluxConfig) *InfluxReporter {
	if !cfg.Enabled {
		return &InfluxReporter{enabled: false}
	}

	client := influxdb2.NewClient(cfg.URL, cfg.Token)
	writeAPI := client.WriteAPI(cfg.Org, cfg.Bucket)

	return &InfluxReporter{
		client:   client,
		writeAPI: writeAPI,
		enabled:  true,
		org:      cfg.Org,
		bucket:   cfg.Bucket,
	}
}

// ReportQueueMetrics sends queue operation metrics to InfluxDB
func (ir *InfluxReporter) ReportQueueMetrics(added, rejected, duplicates int64, brokerType string) {
	if !ir.enabled {
		return
	}

	p := influxdb2.NewPoint("grengate_telemetry",
		map[string]string{
			"operation":   "queue",
			"component":   "gate_broker",
			"broker_type": brokerType, // "read" or "write"
		},
		map[string]interface{}{
			"added":      added,
			"rejected":   rejected,
			"duplicates": duplicates,
		},
		time.Now())

	ir.writeAPI.WritePoint(p)
}

// ReportFlushMetrics sends flush operation metrics to InfluxDB
func (ir *InfluxReporter) ReportFlushMetrics(objectCount int, durationMs int64, isWrite bool, cluId, objectId string, err error) {
	if !ir.enabled {
		return
	}

	success := 1
	if err != nil {
		success = 0
	}

	brokerType := "read"
	if isWrite {
		brokerType = "write"
	}

	tags := map[string]string{
		"operation":   "flush",
		"component":   "gate_broker",
		"broker_type": brokerType,
	}

	// Add CLU and object IDs only if single object (write operations typically)
	if cluId != "" && objectId != "" {
		tags["clu_id"] = cluId
		tags["object_id"] = objectId
	}

	p := influxdb2.NewPoint("grengate_telemetry",
		tags,
		map[string]interface{}{
			"duration_ms":  durationMs,
			"object_count": objectCount,
			"success":      success,
		},
		time.Now())

	ir.writeAPI.WritePoint(p)
}

// ReportCommandMetrics sends command operation metrics to InfluxDB
func (ir *InfluxReporter) ReportCommandMetrics(totalMs, queueWaitMs int64, cluId, objectId string) {
	if !ir.enabled {
		return
	}

	httpMs := totalMs - queueWaitMs

	tags := map[string]string{
		"operation": "command",
		"component": "command",
	}

	// Add CLU and object IDs for command tracking
	if cluId != "" {
		tags["clu_id"] = cluId
	}
	if objectId != "" {
		tags["object_id"] = objectId
	}

	p := influxdb2.NewPoint("grengate_telemetry",
		tags,
		map[string]interface{}{
			"duration_ms":   totalMs,
			"queue_wait_ms": queueWaitMs,
			"http_ms":       httpMs,
		},
		time.Now())

	ir.writeAPI.WritePoint(p)
}

// ReportRefreshMetrics sends refresh cycle metrics to InfluxDB
func (ir *InfluxReporter) ReportRefreshMetrics(objectCount, changedCount, skippedCount int, durationMs int64) {
	if !ir.enabled {
		return
	}

	p := influxdb2.NewPoint("grengate_telemetry",
		map[string]string{
			"operation": "refresh",
			"component": "grenton_set",
		},
		map[string]interface{}{
			"duration_ms":     durationMs,
			"object_count":    objectCount,
			"changed_objects": changedCount,
			"skipped_objects": skippedCount,
		},
		time.Now())

	ir.writeAPI.WritePoint(p)
}

// Close closes the InfluxDB client
func (ir *InfluxReporter) Close() {
	if ir.enabled {
		ir.client.Close()
	}
}
