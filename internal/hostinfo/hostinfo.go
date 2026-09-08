// Package hostinfo reports the health of the machine this service runs on.
//
// It exists because of how Cronsole is deployed: not as a hosted product with
// accounts, but installed on somebody's own VM. On that VM there is no other
// console, so "is the box itself in trouble" is a question this screen has to
// answer. A scheduler reporting healthy jobs on a machine that is out of memory
// is reporting the past.
//
// # Why this is not just /proc
//
// Reading /proc inside a container is misleading in the one direction that
// matters. A pod limited to 512 MB on a 64 GB node reads 64 GB from
// /proc/meminfo and looks idle right up until the kernel kills it. The limit
// that applies is the cgroup's, so that is read first and the host is the
// fallback.
//
// Every reading therefore carries its Source, and the screen says which. "80% of
// 512 MB (container limit)" and "80% of 64 GB (host)" are different facts and
// must not look the same.
package hostinfo

import (
	"context"
	"sync"
	"time"
)

// Source says what the figures are measured against.
type Source string

const (
	// SourceCgroup means a limit is set on this process's cgroup, so the
	// figures are the container's own. This is the Docker and Kubernetes case.
	SourceCgroup Source = "cgroup"
	// SourceHost means no limit applies, so the figures are the whole machine's.
	// This is the plain VM case, and the one this feature was asked for.
	SourceHost Source = "host"
)

// Stats is one reading.
//
// Every total is the total that ACTUALLY APPLIES, which is the cgroup limit when
// there is one. A caller comparing Used against Total is therefore asking the
// right question without knowing where it is running.
type Stats struct {
	// Supported is false where there is nothing to read, which today means any
	// platform other than Linux. The screen hides the panel rather than showing
	// zeros, because a row of zeros reads as "everything is fine".
	Supported bool   `json:"supported"`
	Source    Source `json:"source"`

	// CPUPercent is of the CPU actually available, so 100 means the quota is
	// saturated rather than one core of many being busy. Negative until two
	// samples exist: usage is a delta and the first reading has nothing to
	// subtract from.
	CPUPercent float64 `json:"cpu_percent"`
	// CPUCores is how much CPU is available: the cgroup quota where one is set,
	// otherwise the machine's core count. Fractional under a quota.
	CPUCores float64 `json:"cpu_cores"`

	MemUsedBytes  uint64 `json:"mem_used_bytes"`
	MemTotalBytes uint64 `json:"mem_total_bytes"`

	DiskUsedBytes  uint64 `json:"disk_used_bytes"`
	DiskTotalBytes uint64 `json:"disk_total_bytes"`
	DiskPath       string `json:"disk_path"`

	// Load is the machine's, always. A container does not have a load average
	// of its own: /proc/loadavg reports the host's even from inside one. It is
	// still worth showing on a VM, which is the deployment this serves, and the
	// screen labels it as the host's.
	Load1  float64 `json:"load_1"`
	Load5  float64 `json:"load_5"`
	Load15 float64 `json:"load_15"`

	UptimeSec int64     `json:"uptime_sec"`
	ReadAt    time.Time `json:"read_at"`
}

// MemPercent is memory used against the limit that applies, or -1 when the
// total is unknown. Never a division by zero dressed up as 0%.
func (s Stats) MemPercent() float64 {
	if s.MemTotalBytes == 0 {
		return -1
	}
	return float64(s.MemUsedBytes) / float64(s.MemTotalBytes) * 100
}

// DiskPercent is disk used against the filesystem holding the working
// directory, or -1 when it could not be read.
func (s Stats) DiskPercent() float64 {
	if s.DiskTotalBytes == 0 {
		return -1
	}
	return float64(s.DiskUsedBytes) / float64(s.DiskTotalBytes) * 100
}

// Reader samples the machine.
//
// It holds the previous CPU sample, because CPU usage is a delta and a single
// reading cannot express it. That state is the only reason this is a type
// rather than a function.
type Reader struct {
	mu sync.Mutex

	// path is the filesystem whose usage is reported: the working directory,
	// which is where the binary and any local state live. The database is
	// usually elsewhere and is not this panel's business.
	path string

	lastCPU   cpuSample
	lastStats Stats
	lastRead  time.Time
	// minInterval keeps a refreshing dashboard from reading /proc on every
	// request. The figures do not change meaningfully in less than this, and a
	// CPU delta over a very short window is mostly noise.
	minInterval time.Duration
}

// cpuSample is enough to compute usage between two readings.
type cpuSample struct {
	// busy is cumulative busy time. Its unit does not matter as long as it is
	// consistent between samples, because only the ratio is used.
	busy  float64
	total float64
	at    time.Time
	ok    bool
}

// NewReader wires a reader over the given path, which should be somewhere the
// service actually writes.
func NewReader(path string) *Reader {
	if path == "" {
		path = "."
	}
	return &Reader{path: path, minInterval: 2 * time.Second}
}

// Start samples in the background until the context is cancelled.
//
// Only CPU needs this. It is a delta between two readings, so without a sampler
// running the first person to open the dashboard sees nothing, and the second
// reading is a thirty second average taken while they were reading the first.
// A short cadence here means the figure is current whenever anybody looks, and
// is measured over a window that reflects the machine now.
func (r *Reader) Start(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = 5 * time.Second
	}
	go func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		r.Read()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.Read()
			}
		}
	}()
}

// Read returns the current state, cached for a short interval.
func (r *Reader) Read() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.lastRead.IsZero() && time.Since(r.lastRead) < r.minInterval {
		return r.lastStats
	}

	stats := r.read()
	stats.ReadAt = time.Now()
	r.lastStats = stats
	r.lastRead = stats.ReadAt
	return stats
}
