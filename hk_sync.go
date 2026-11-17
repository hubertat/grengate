package main

import (
	"sync"
	"time"
)

// hkSyncCloseWait bounds how long close waits for a sync in progress: hap's SetValue can
// block on a stalled HomeKit connection.
const hkSyncCloseWait = time.Second

// hkSyncer pushes device state to HomeKit off the lane workers: hap's SetValue notifies
// connected controllers over the network, which a lane hook must not wait for.
// A device marked several times before it is synced is synced once, with its latest state.
//
// A HomeKit write can land while its device is being synced, after Sync copied the device
// state and before its SetValue: the SetValue then puts the stale copy into hap's cache,
// and hap would drop the user's next write of that value as unchanged. Every HomeKit write
// callback therefore marks its device after changing the state (hap calls it after storing
// the written value): the mark is taken after the running sync was dequeued, so one more
// sync follows and copies the written state.
type hkSyncer struct {
	mu     sync.Mutex
	queue  []hkSyncItem
	dirty  map[device]bool
	closed bool

	wake chan struct{}
	stop chan struct{}
	done chan struct{}
}

// hkSyncItem is a device to sync, or a barrier to close once everything before it is done.
type hkSyncItem struct {
	d       device
	barrier chan struct{}
}

func newHkSyncer() *hkSyncer {
	return &hkSyncer{
		dirty: map[device]bool{},
		wake:  make(chan struct{}, 1),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
}

func (s *hkSyncer) start() { go s.run() }

// close stops the syncer; pending barriers are released. It waits at most hkSyncCloseWait
// for a sync in progress; a sync stuck in hap is left to finish on its own.
func (s *hkSyncer) close() {
	close(s.stop)
	select {
	case <-s.done:
	case <-time.After(hkSyncCloseWait):
	}
	s.mu.Lock()
	s.closed = true
	rest := s.queue
	s.queue = nil
	s.mu.Unlock()
	for _, it := range rest {
		if it.barrier != nil {
			close(it.barrier)
		}
	}
}

// mark queues a HomeKit sync of d. It never blocks.
func (s *hkSyncer) mark(d device) {
	s.mu.Lock()
	if !s.dirty[d] {
		s.dirty[d] = true
		s.queue = append(s.queue, hkSyncItem{d: d})
	}
	s.mu.Unlock()
	s.kick()
}

// flushed returns a channel that is closed once every sync marked before the call is done.
func (s *hkSyncer) flushed() <-chan struct{} {
	ch := make(chan struct{})
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		close(ch)
		return ch
	}
	s.queue = append(s.queue, hkSyncItem{barrier: ch})
	s.mu.Unlock()
	s.kick()
	return ch
}

func (s *hkSyncer) kick() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *hkSyncer) run() {
	defer close(s.done)
	for {
		select {
		case <-s.wake:
		case <-s.stop:
			return
		}
		for {
			select {
			case <-s.stop:
				return
			default:
			}
			s.mu.Lock()
			if s.closed || len(s.queue) == 0 {
				s.mu.Unlock()
				break
			}
			it := s.queue[0]
			s.queue = s.queue[1:]
			if it.d != nil {
				delete(s.dirty, it.d) // a mark from now on queues a new sync
			}
			s.mu.Unlock()

			if it.barrier != nil {
				close(it.barrier)
				continue
			}
			it.d.Sync()
		}
	}
}
