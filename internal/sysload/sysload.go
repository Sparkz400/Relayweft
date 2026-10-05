// Package sysload measures how busy the machine is (CPU, free memory, free
// disk) so Relayweft can hold back new agents instead of tipping a machine
// over. Every measurement is best effort: ok=false means unknown, and
// unknown never blocks anything.
package sysload

import (
	"sync"
	"time"
)

// Sample is one reading.
type Sample struct {
	CPU      float64 // 0..1 busy fraction over the last interval
	CPUOK    bool
	MemFree  uint64 // bytes available
	MemTotal uint64
	MemOK    bool
	At       time.Time
}

// Sampler keeps a fresh reading in the background.
type Sampler struct {
	mu     sync.Mutex
	last   Sample
	prevB  uint64
	prevT  uint64
	start  sync.Once
	period time.Duration
}

// NewSampler returns a sampler that reads every period once started.
func NewSampler(period time.Duration) *Sampler { return &Sampler{period: period} }

// Get returns the latest sample, starting the sampler on first use.
func (s *Sampler) Get() Sample {
	s.start.Do(func() {
		s.read()
		go func() {
			for range time.Tick(s.period) {
				s.read()
			}
		}()
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func (s *Sampler) read() {
	busy, total, cpuOK := cpuTimes()
	free, memTotal, memOK := memory()
	s.mu.Lock()
	defer s.mu.Unlock()
	sm := Sample{MemFree: free, MemTotal: memTotal, MemOK: memOK, At: time.Now()}
	if cpuOK && s.prevT != 0 && total > s.prevT {
		sm.CPU = float64(busy-s.prevB) / float64(total-s.prevT)
		sm.CPUOK = true
	} else if s.last.CPUOK {
		sm.CPU, sm.CPUOK = s.last.CPU, true
	}
	if cpuOK {
		s.prevB, s.prevT = busy, total
	}
	s.last = sm
}

// DiskFree returns the bytes available to this user on the volume holding
// path (which must exist).
func DiskFree(path string) (uint64, bool) { return diskFree(path) }
