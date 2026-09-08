//go:build linux

package hostinfo

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The two trees every figure below is read from.
//
// Variables rather than constants so the tests can point them at a fixture and
// exercise the parsing for a machine they are not running on: a v1 hierarchy, a
// quota this container does not have, a memory limit that is actually reached.
// Nothing in the service writes them.
var (
	cgroupRoot = "/sys/fs/cgroup"
	procRoot   = "/proc"
)

func cgPath(name string) string   { return filepath.Join(cgroupRoot, name) }
func procPath(name string) string { return filepath.Join(procRoot, name) }

// cgroup v2 names. A unified hierarchy mounts these at the root of the
// process's own cgroup, so a container reads its own limits without knowing its
// cgroup name.
const (
	cgMemMax     = "memory.max"
	cgMemCurrent = "memory.current"
	cgMemStat    = "memory.stat"
	cgCPUMax     = "cpu.max"
	cgCPUStat    = "cpu.stat"
)

// cgroup v1 names, for older hosts. Kubernetes on an older kernel still lands
// here, so it is worth the twenty lines.
const (
	cg1MemLimit  = "memory/memory.limit_in_bytes"
	cg1MemUsage  = "memory/memory.usage_in_bytes"
	cg1MemStat   = "memory/memory.stat"
	cg1CPUQuota  = "cpu/cpu.cfs_quota_us"
	cg1CPUPeriod = "cpu/cpu.cfs_period_us"
	cg1CPUUsage  = "cpuacct/cpuacct.usage"
)

// unlimited is what a cgroup writes when no limit is set. v1 writes a number so
// large it is the same statement.
const unlimitedV1 = 1 << 62

func (r *Reader) read() Stats {
	s := Stats{Supported: true, Source: SourceHost}

	// Memory first, because it is what decides the Source: a memory limit is
	// the sign that this process is confined, and it is the limit that kills.
	if used, total, ok := cgroupMemory(); ok {
		s.MemUsedBytes, s.MemTotalBytes = used, total
		s.Source = SourceCgroup
	} else {
		s.MemUsedBytes, s.MemTotalBytes = hostMemory()
	}

	s.CPUCores = availableCores()
	s.CPUPercent = r.cpuPercent(s.CPUCores)

	s.DiskPath = r.path
	s.DiskUsedBytes, s.DiskTotalBytes = diskUsage(r.path)

	s.Load1, s.Load5, s.Load15 = loadAverage()
	s.UptimeSec = uptime()
	return s
}

// cgroupMemory returns the container's usage and limit, and whether a limit is
// actually set. No limit means the figures would be the host's anyway, so the
// caller should read the host instead and say so.
func cgroupMemory() (used, limit uint64, ok bool) {
	// v2.
	if raw, err := os.ReadFile(cgPath(cgMemMax)); err == nil {
		text := strings.TrimSpace(string(raw))
		if text == "max" {
			return 0, 0, false
		}
		limit, err := strconv.ParseUint(text, 10, 64)
		if err != nil || limit == 0 {
			return 0, 0, false
		}
		current := readUint(cgPath(cgMemCurrent))
		// memory.current counts the page cache, which the kernel reclaims under
		// pressure. Reporting it as used shows a healthy container sitting at
		// 95% forever, which trains everyone to ignore the number. Subtracting
		// reclaimable file pages is what the OOM killer effectively does.
		return subtractSaturating(current, readKeyed(cgPath(cgMemStat), "inactive_file")), limit, true
	}

	// v1.
	if raw, err := os.ReadFile(cgPath(cg1MemLimit)); err == nil {
		limit, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil || limit == 0 || limit >= unlimitedV1 {
			return 0, 0, false
		}
		current := readUint(cgPath(cg1MemUsage))
		return subtractSaturating(current, readKeyed(cgPath(cg1MemStat), "total_inactive_file")), limit, true
	}

	return 0, 0, false
}

// hostMemory reads the machine's, using MemAvailable rather than MemFree.
// MemFree excludes the cache the kernel would hand back on demand and makes
// every healthy Linux box look nearly full.
func hostMemory() (used, total uint64) {
	raw, err := os.ReadFile(procPath("meminfo"))
	if err != nil {
		return 0, 0
	}

	var totalKB, availableKB uint64
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			totalKB = firstUint(line)
		case strings.HasPrefix(line, "MemAvailable:"):
			availableKB = firstUint(line)
		}
	}
	if totalKB == 0 {
		return 0, 0
	}
	return subtractSaturating(totalKB, availableKB) * 1024, totalKB * 1024
}

// availableCores is the cgroup quota where one is set, otherwise the machine's
// core count. A pod with "cpu: 500m" gets 0.5, and 100% then means it is
// throttled rather than that the node is busy.
func availableCores() float64 {
	if raw, err := os.ReadFile(cgPath(cgCPUMax)); err == nil {
		fields := strings.Fields(string(raw))
		if len(fields) == 2 && fields[0] != "max" {
			quota, err1 := strconv.ParseFloat(fields[0], 64)
			period, err2 := strconv.ParseFloat(fields[1], 64)
			if err1 == nil && err2 == nil && period > 0 && quota > 0 {
				return quota / period
			}
		}
	}
	if quota := readInt(cgPath(cg1CPUQuota)); quota > 0 {
		if period := readInt(cgPath(cg1CPUPeriod)); period > 0 {
			return float64(quota) / float64(period)
		}
	}
	return float64(runtime.NumCPU())
}

// cpuPercent is usage since the previous sample.
//
// Returns -1 until there are two samples: usage is a delta, and inventing a
// figure from one reading would be a number that looks measured and is not.
func (r *Reader) cpuPercent(cores float64) float64 {
	sample := sampleCPU(cores)
	previous := r.lastCPU
	r.lastCPU = sample

	if !sample.ok || !previous.ok {
		return -1
	}
	elapsed := sample.total - previous.total
	if elapsed <= 0 {
		return -1
	}
	percent := (sample.busy - previous.busy) / elapsed * 100
	if percent < 0 {
		return 0
	}
	if percent > 100 {
		// A quota can be exceeded slightly across a period boundary. Saying
		// 103% would only make somebody doubt the whole panel.
		return 100
	}
	return percent
}

// sampleCPU prefers the cgroup's own accounting, which counts only this
// container's processes. /proc/stat counts the whole machine, so in a container
// it reports the node's busy neighbours as if they were this service.
func sampleCPU(cores float64) cpuSample {
	now := time.Now()

	// v2: usage_usec is this cgroup's cumulative CPU time.
	if usec := readKeyed(cgPath(cgCPUStat), "usage_usec"); usec > 0 && cores > 0 {
		return cpuSample{
			busy: float64(usec) / 1e6,
			// The denominator is wall time times the cores available, so the
			// ratio is "how much of what we may use".
			total: float64(now.UnixNano()) / 1e9 * cores,
			at:    now,
			ok:    true,
		}
	}
	// v1: nanoseconds.
	if ns := readUint(cgPath(cg1CPUUsage)); ns > 0 && cores > 0 {
		return cpuSample{
			busy:  float64(ns) / 1e9,
			total: float64(now.UnixNano()) / 1e9 * cores,
			at:    now,
			ok:    true,
		}
	}

	// The host. /proc/stat's first line is cumulative jiffies across all cores.
	raw, err := os.ReadFile(procPath("stat"))
	if err != nil {
		return cpuSample{}
	}
	line, _, _ := strings.Cut(string(raw), "\n")
	fields := strings.Fields(line)
	if len(fields) < 8 || fields[0] != "cpu" {
		return cpuSample{}
	}

	var total, idle float64
	for i, field := range fields[1:] {
		value, err := strconv.ParseFloat(field, 64)
		if err != nil {
			return cpuSample{}
		}
		total += value
		// Fields 3 and 4 are idle and iowait. Both are the CPU having nothing
		// to do; counting iowait as busy is the classic way to report a machine
		// as pegged when it is waiting on a disk.
		if i == 3 || i == 4 {
			idle += value
		}
	}
	return cpuSample{busy: total - idle, total: total, at: now, ok: true}
}

// diskUsage reports the filesystem holding path.
//
// Used is total minus what is available to an unprivileged process, not minus
// free: the reserved blocks are not usable by this service, and counting them
// as free means the panel says 3% left while writes are already failing.
func diskUsage(path string) (used, total uint64) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return 0, 0
	}
	blockSize := uint64(fs.Bsize)
	total = fs.Blocks * blockSize
	available := fs.Bavail * blockSize
	return subtractSaturating(total, available), total
}

func loadAverage() (one, five, fifteen float64) {
	raw, err := os.ReadFile(procPath("loadavg"))
	if err != nil {
		return 0, 0, 0
	}
	fields := strings.Fields(string(raw))
	if len(fields) < 3 {
		return 0, 0, 0
	}
	one, _ = strconv.ParseFloat(fields[0], 64)
	five, _ = strconv.ParseFloat(fields[1], 64)
	fifteen, _ = strconv.ParseFloat(fields[2], 64)
	return one, five, fifteen
}

func uptime() int64 {
	raw, err := os.ReadFile(procPath("uptime"))
	if err != nil {
		return 0
	}
	first, _, _ := strings.Cut(strings.TrimSpace(string(raw)), " ")
	seconds, err := strconv.ParseFloat(first, 64)
	if err != nil {
		return 0
	}
	return int64(seconds)
}

// --- small readers ----------------------------------------------------------

func readUint(path string) uint64 {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	value, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0
	}
	return value
}

func readInt(path string) int64 {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	value, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0
	}
	return value
}

// readKeyed reads "key value" lines, which is the shape of memory.stat and
// cpu.stat.
func readKeyed(path, key string) uint64 {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		name, value, found := strings.Cut(line, " ")
		if !found || name != key {
			continue
		}
		parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return 0
		}
		return parsed
	}
	return 0
}

func firstUint(line string) uint64 {
	for _, field := range strings.Fields(line) {
		if value, err := strconv.ParseUint(field, 10, 64); err == nil {
			return value
		}
	}
	return 0
}

// subtractSaturating avoids the wrap that makes an unsigned subtraction report
// sixteen exabytes of memory in use.
func subtractSaturating(a, b uint64) uint64 {
	if b > a {
		return 0
	}
	return a - b
}
