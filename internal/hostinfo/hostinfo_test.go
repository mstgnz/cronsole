package hostinfo

import (
	"math"
	"testing"
)

func TestPercentsRefuseToDivideByZero(t *testing.T) {
	// -1 rather than 0. A panel that prints 0% for "we could not read this"
	// says the machine is idle, which is the opposite of the truth and the
	// reason somebody would stop trusting the whole card.
	var unknown Stats
	if got := unknown.MemPercent(); got != -1 {
		t.Errorf("MemPercent with no total = %v, want -1", got)
	}
	if got := unknown.DiskPercent(); got != -1 {
		t.Errorf("DiskPercent with no total = %v, want -1", got)
	}
}

func TestPercents(t *testing.T) {
	s := Stats{
		MemUsedBytes: 512, MemTotalBytes: 1024,
		DiskUsedBytes: 3, DiskTotalBytes: 4,
	}
	if got := s.MemPercent(); math.Abs(got-50) > 0.001 {
		t.Errorf("MemPercent = %v, want 50", got)
	}
	if got := s.DiskPercent(); math.Abs(got-75) > 0.001 {
		t.Errorf("DiskPercent = %v, want 75", got)
	}
}

func TestReadIsSafeEverywhere(t *testing.T) {
	// The service is built and tested on macOS and deployed on Linux. This must
	// not panic or block on either; where there is nothing to read it reports
	// Supported false and the screen hides the card.
	r := NewReader(t.TempDir())
	s := r.Read()

	if !s.Supported {
		if s.CPUPercent != -1 {
			t.Errorf("an unsupported platform reported CPU %v, want -1", s.CPUPercent)
		}
		return
	}

	// On Linux the totals should be real, and a percentage must never be a
	// figure somebody would act on wrongly.
	if s.MemTotalBytes == 0 {
		t.Error("supported, but no memory total was read")
	}
	if s.CPUCores <= 0 {
		t.Errorf("cores = %v, want a positive number", s.CPUCores)
	}
	if p := s.MemPercent(); p < 0 || p > 100 {
		t.Errorf("memory = %v%%, which is not a percentage", p)
	}
	if s.Source != SourceHost && s.Source != SourceCgroup {
		t.Errorf("source = %q, want host or cgroup", s.Source)
	}
}

func TestFirstCPUReadingIsUnknown(t *testing.T) {
	// CPU usage is a delta. The first reading has nothing to subtract from, and
	// inventing a figure would look measured while being arbitrary.
	r := NewReader(t.TempDir())
	if s := r.Read(); s.Supported && s.CPUPercent != -1 {
		t.Errorf("the first CPU reading = %v, want -1 until there are two samples", s.CPUPercent)
	}
}

func TestReadIsCached(t *testing.T) {
	// A dashboard that refreshes every thirty seconds and several operators
	// watching it must not turn into a /proc read per request.
	r := NewReader(t.TempDir())
	first := r.Read()
	second := r.Read()
	if !first.ReadAt.Equal(second.ReadAt) {
		t.Error("two reads inside the interval produced two samples")
	}
}
