package main

import "sync"

// hkSyncer pushes device state to HomeKit off the lane workers: hap's SetValue notifies
// connected controllers over the network, which a lane hook must not wait for.
// A device marked several times before it is synced is synced once, with its latest state.
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

// close stops the syncer; pending barriers are released.
func (s *hkSyncer) close() {
	close(s.stop)
	<-s.done
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
			s.mu.Lock()
			if len(s.queue) == 0 {
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
