package main

import (
	"sync"
	"time"
)

// Telemetry tracks operational metrics with simple moving averages
type Telemetry struct {
	mu sync.Mutex

	// Queue operation counts
	QueueAdds       int64
	QueueRejects    int64
	QueueDuplicates int64

	// Flush metrics (read broker)
	FlushCount   int64
	FlushErrors  int64
	FlushAvgMs   int64 // Simple moving average of HTTP request time

	// Setter flush metrics (write commands)
	SetterFlushCount  int64
	SetterFlushErrors int64
	SetterFlushAvgMs  int64 // Simple moving average of write request time

	// Command metrics (end-to-end command tracking)
	CommandCount       int64
	CommandAvgMs       int64 // Average total command time (queue wait + flush)
	CommandQueueWaitMs int64 // Average time waiting in queue before flush

	// Refresh metrics
	RefreshCount   int64
	RefreshAvgMs   int64 // Average refresh cycle duration
	RefreshObjects int64 // Total objects processed (cumulative)
	RefreshChanged int64 // Objects that changed state (cumulative)
	RefreshSkipped int64 // Objects skipped (Stage 5, cumulative)

	lastReset time.Time
}

// RecordQueueAdd records a successful queue addition
func (t *Telemetry) RecordQueueAdd() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.QueueAdds++
}

// RecordQueueReject records a rejected queue item
func (t *Telemetry) RecordQueueReject() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.QueueRejects++
}

// RecordQueueDuplicate records a duplicate that was skipped
func (t *Telemetry) RecordQueueDuplicate() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.QueueDuplicates++
}

// RecordFlush records a read broker flush operation (HTTP request to read devices)
func (t *Telemetry) RecordFlush(duration time.Duration, objectCount int, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.FlushCount++
	if err != nil {
		t.FlushErrors++
		return // Don't include failed requests in average
	}

	// Simple moving average: new_avg = (old_avg * (count-1) + new_value) / count
	durationMs := duration.Milliseconds()
	if t.FlushCount == 1 {
		t.FlushAvgMs = durationMs
	} else {
		t.FlushAvgMs = (t.FlushAvgMs*(t.FlushCount-1) + durationMs) / t.FlushCount
	}
}

// RecordSetterFlush records a setter flush operation (HTTP request to write command)
func (t *Telemetry) RecordSetterFlush(duration time.Duration, objectCount int, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.SetterFlushCount++
	if err != nil {
		t.SetterFlushErrors++
		return
	}

	durationMs := duration.Milliseconds()
	if t.SetterFlushCount == 1 {
		t.SetterFlushAvgMs = durationMs
	} else {
		t.SetterFlushAvgMs = (t.SetterFlushAvgMs*(t.SetterFlushCount-1) + durationMs) / t.SetterFlushCount
	}
}

// RecordCommand records an end-to-end command (from SendReq to response)
func (t *Telemetry) RecordCommand(totalDuration, queueWaitDuration time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.CommandCount++

	totalMs := totalDuration.Milliseconds()
	queueWaitMs := queueWaitDuration.Milliseconds()

	if t.CommandCount == 1 {
		t.CommandAvgMs = totalMs
		t.CommandQueueWaitMs = queueWaitMs
	} else {
		t.CommandAvgMs = (t.CommandAvgMs*(t.CommandCount-1) + totalMs) / t.CommandCount
		t.CommandQueueWaitMs = (t.CommandQueueWaitMs*(t.CommandCount-1) + queueWaitMs) / t.CommandCount
	}
}

// RecordRefresh records a refresh cycle
func (t *Telemetry) RecordRefresh(duration time.Duration, objectCount, changedCount, skippedCount int) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.RefreshCount++
	t.RefreshObjects += int64(objectCount)
	t.RefreshChanged += int64(changedCount)
	t.RefreshSkipped += int64(skippedCount)

	durationMs := duration.Milliseconds()
	if t.RefreshCount == 1 {
		t.RefreshAvgMs = durationMs
	} else {
		t.RefreshAvgMs = (t.RefreshAvgMs*(t.RefreshCount-1) + durationMs) / t.RefreshCount
	}
}

// GetStats returns current statistics snapshot
func (t *Telemetry) GetStats() TelemetryStats {
	t.mu.Lock()
	defer t.mu.Unlock()

	return TelemetryStats{
		QueueAdds:          t.QueueAdds,
		QueueRejects:       t.QueueRejects,
		QueueDuplicates:    t.QueueDuplicates,
		FlushCount:         t.FlushCount,
		FlushErrors:        t.FlushErrors,
		FlushAvgMs:         t.FlushAvgMs,
		SetterFlushCount:   t.SetterFlushCount,
		SetterFlushErrors:  t.SetterFlushErrors,
		SetterFlushAvgMs:   t.SetterFlushAvgMs,
		CommandCount:       t.CommandCount,
		CommandAvgMs:       t.CommandAvgMs,
		CommandQueueWaitMs: t.CommandQueueWaitMs,
		RefreshCount:       t.RefreshCount,
		RefreshAvgMs:       t.RefreshAvgMs,
		RefreshObjects:     t.RefreshObjects,
		RefreshChanged:     t.RefreshChanged,
		RefreshSkipped:     t.RefreshSkipped,
		UptimeSeconds:      int64(time.Since(t.lastReset).Seconds()),
	}
}

// Reset clears all metrics
func (t *Telemetry) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()

	*t = Telemetry{lastReset: time.Now()}
}

// TelemetryStats is a snapshot of telemetry data
type TelemetryStats struct {
	QueueAdds          int64
	QueueRejects       int64
	QueueDuplicates    int64
	FlushCount         int64
	FlushErrors        int64
	FlushAvgMs         int64
	SetterFlushCount   int64
	SetterFlushErrors  int64
	SetterFlushAvgMs   int64
	CommandCount       int64
	CommandAvgMs       int64
	CommandQueueWaitMs int64
	RefreshCount       int64
	RefreshAvgMs       int64
	RefreshObjects     int64
	RefreshChanged     int64
	RefreshSkipped     int64
	UptimeSeconds      int64
}
