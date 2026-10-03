package sysload

import (
	"os"
	"runtime"
	"testing"
	"time"
)

func TestSampleAndDisk(t *testing.T) {
	s := NewSampler(50 * time.Millisecond)
	s.Get()
	time.Sleep(150 * time.Millisecond)
	sm := s.Get()
	if runtime.GOOS == "linux" || runtime.GOOS == "windows" {
		if !sm.CPUOK || sm.CPU < 0 || sm.CPU > 1 {
			t.Errorf("cpu = %v ok=%v", sm.CPU, sm.CPUOK)
		}
		if !sm.MemOK || sm.MemFree == 0 || sm.MemFree > sm.MemTotal {
			t.Errorf("mem = %d/%d ok=%v", sm.MemFree, sm.MemTotal, sm.MemOK)
		}
	}
	if free, ok := DiskFree(os.TempDir()); !ok || free == 0 {
		t.Errorf("disk free = %d ok=%v", free, ok)
	}
}
