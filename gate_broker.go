package main

import (
	"sync"
	"time"
)

type GateBroker struct {
	RetentionPeriod time.Duration
	MaxQueueLength  int

	queue   []ReqObject
	working sync.Mutex
}
