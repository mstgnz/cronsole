//go:build linux

package hostinfo

import (
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// These tests read a fixture tree instead of the machine's own.
//
// The reason is that the interesting cases are the ones the test machine is
// not: a container against its memory limit, a half-core CPU quota, an old v1
// hierarchy, a kernel that writes "max" where a number was expected. Running on
// whatever the CI runner happens to be tests one arrangement, and never the one
// that matters, because a wrong reading here is only wrong in production.
//
// They also pin the two decisions that make the numbers honest rather than
// merely present: the page cache is subtracted from memory used, and iowait
// counts as idle.

// fixture writes a cgroup and proc tree and points the readers at it.
func fixture(t *testing.T, files map[string]string) {
	t.Helper()

	root := t.TempDir()
	cg, proc := filepath.Join(root, "cgroup"), filepath.Join(root, "proc")
	for name, body := range files {
		var full string
		switch {
		case len(name) > 3 && name[:3] == "cg/":
			full = filepath.Join(cg, name[3:])
		case len(name) > 5 && name[:5] == "proc/":
			full = filepath.Join(proc, name[5:])
		default:
			t.Fatalf("fixture: %q must start with cg/ or proc/", name)
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("fixture: %v", err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("fixture: %v", err)
		}
	}

	// Restored rather than left set: the later tests in this package read the
	// real machine on purpose.
	oldCgroup, oldProc := cgroupRoot, procRoot
	cgroupRoot, procRoot = cg, proc
	t.Cleanup(func() { cgroupRoot, procRoot = oldCgroup, oldProc })
}

func TestACgroupMemoryLimitIsWhatGetsReported(t *testing.T) {
	// The whole reason this package exists. A pod limited to 512 MB on a large
	// node must report the 512, or the panel says everything is fine until the
	// kernel kills the process.
	fixture(t, map[string]string{
		"cg/memory.max":     "536870912\n",
		"cg/memory.current": "268435456\n",
		"cg/memory.stat":    "anon 100\ninactive_file 67108864\nslab 20\n",
		"proc/meminfo":      "MemTotal:       65805304 kB\nMemAvailable:   60000000 kB\n",
	})

	used, limit, ok := cgroupMemory()
	if !ok {
		t.Fatal("a container with a limit was reported as unlimited")
	}
	if limit != 536870912 {
		t.Errorf("limit = %d, want the cgroup's 536870912 and not the host's", limit)
	}
	// 256 MB in use, 64 MB of it reclaimable page cache. Counting the cache
	// would show a healthy container at a number that never comes down.
	if want := uint64(268435456 - 67108864); used != want {
		t.Errorf("used = %d, want %d with the page cache subtracted", used, want)
	}
}

func TestNoCgroupLimitFallsBackToTheHost(t *testing.T) {
	fixture(t, map[string]string{
		// What a kernel writes when nothing is limited.
		"cg/memory.max": "max\n",
		"proc/meminfo":  "MemTotal:       8000 kB\nMemAvailable:   3000 kB\n",
	})

	if _, _, ok := cgroupMemory(); ok {
		t.Error(`memory.max of "max" was read as a limit`)
	}

	used, total := hostMemory()
	if total != 8000*1024 {
		t.Errorf("total = %d, want 8000 kB in bytes", total)
	}
	// MemAvailable, not MemFree: what the kernel would hand back on demand is
	// not in use, and using MemFree makes every healthy Linux box look full.
	if want := uint64(5000 * 1024); used != want {
		t.Errorf("used = %d, want %d", used, want)
	}
}

func TestTheOlderCgroupHierarchyIsStillRead(t *testing.T) {
	fixture(t, map[string]string{
		"cg/memory/memory.limit_in_bytes": "1073741824\n",
		"cg/memory/memory.usage_in_bytes": "600000000\n",
		"cg/memory/memory.stat":           "total_inactive_file 100000000\n",
	})

	used, limit, ok := cgroupMemory()
	if !ok {
		t.Fatal("a v1 hierarchy with a limit was reported as unlimited")
	}
	if limit != 1073741824 {
		t.Errorf("limit = %d, want 1073741824", limit)
	}
	if want := uint64(500000000); used != want {
		t.Errorf("used = %d, want %d", used, want)
	}
}

func TestAnUnsetV1LimitIsNotALimit(t *testing.T) {
	// v1 has no "max": it writes a number so large it means the same thing, and
	// reading it literally reports a machine with four exabytes of memory.
	fixture(t, map[string]string{
		"cg/memory/memory.limit_in_bytes": "9223372036854771712\n",
		"proc/meminfo":                    "MemTotal: 100 kB\nMemAvailable: 40 kB\n",
	})

	if _, _, ok := cgroupMemory(); ok {
		t.Error("the v1 unlimited sentinel was read as a real limit")
	}
}

func TestMissingFilesReportNothingRatherThanGuessing(t *testing.T) {
	fixture(t, map[string]string{"cg/unrelated": "x"})

	if _, _, ok := cgroupMemory(); ok {
		t.Error("a tree with no memory files reported a limit")
	}
	if used, total := hostMemory(); used != 0 || total != 0 {
		t.Errorf("hostMemory with no meminfo = (%d, %d), want zeros", used, total)
	}
	if one, five, fifteen := loadAverage(); one != 0 || five != 0 || fifteen != 0 {
		t.Errorf("loadAverage with no file = (%v, %v, %v), want zeros", one, five, fifteen)
	}
	if got := uptime(); got != 0 {
		t.Errorf("uptime with no file = %d, want 0", got)
	}
	// The machine's core count, because a quota that cannot be read is not a
	// quota of zero.
	if got := availableCores(); got != float64(runtime.NumCPU()) {
		t.Errorf("cores = %v, want the machine's %d", got, runtime.NumCPU())
	}
}

func TestACPUQuotaIsFractionalCores(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  float64
	}{
		{"v2 half a core", map[string]string{"cg/cpu.max": "50000 100000\n"}, 0.5},
		{"v2 two cores", map[string]string{"cg/cpu.max": "200000 100000\n"}, 2},
		{"v2 no quota", map[string]string{"cg/cpu.max": "max 100000\n"}, float64(runtime.NumCPU())},
		{"v1 quarter core", map[string]string{
			"cg/cpu/cpu.cfs_quota_us":  "25000\n",
			"cg/cpu/cpu.cfs_period_us": "100000\n",
		}, 0.25},
		{"v1 no quota", map[string]string{
			"cg/cpu/cpu.cfs_quota_us":  "-1\n",
			"cg/cpu/cpu.cfs_period_us": "100000\n",
		}, float64(runtime.NumCPU())},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fixture(t, c.files)
			if got := availableCores(); math.Abs(got-c.want) > 0.0001 {
				t.Errorf("cores = %v, want %v", got, c.want)
			}
		})
	}
}

func TestCPUUsageNeedsTwoSamples(t *testing.T) {
	fixture(t, map[string]string{
		"cg/cpu.max":  "100000 100000\n",
		"cg/cpu.stat": "usage_usec 1000000\nuser_usec 900000\n",
	})

	r := NewReader(t.TempDir())
	if got := r.cpuPercent(1); got != -1 {
		t.Errorf("the first reading = %v, want -1: a delta needs something to subtract from", got)
	}
	if got := r.cpuPercent(1); got < 0 || got > 100 {
		t.Errorf("the second reading = %v, which is not a percentage", got)
	}
}

func TestIowaitCountsAsIdle(t *testing.T) {
	// The classic way to report a machine as pegged when it is waiting on a
	// disk. Fields are: user nice system idle iowait irq softirq steal.
	fixture(t, map[string]string{
		"proc/stat": "cpu  100 0 0 700 200 0 0 0\ncpu0 100 0 0 700 200 0 0 0\n",
	})

	sample := sampleCPU(0)
	if !sample.ok {
		t.Fatal("/proc/stat was not read")
	}
	if sample.total != 1000 {
		t.Errorf("total = %v, want every field summed", sample.total)
	}
	// 100 busy out of 1000, with idle 700 and iowait 200 both idle.
	if sample.busy != 100 {
		t.Errorf("busy = %v, want 100 with iowait counted as idle", sample.busy)
	}
}

func TestAMalformedProcStatIsNotASample(t *testing.T) {
	// Rather than a sample of zero, which would be read as an idle machine.
	fixture(t, map[string]string{"proc/stat": "cpu  not a number\n"})

	if sampleCPU(0).ok {
		t.Error("a malformed /proc/stat produced a sample")
	}
}

func TestLoadAndUptimeAreParsed(t *testing.T) {
	fixture(t, map[string]string{
		"proc/loadavg": "0.52 0.44 0.39 1/512 12345\n",
		"proc/uptime":  "86400.12 340000.55\n",
	})

	one, five, fifteen := loadAverage()
	if one != 0.52 || five != 0.44 || fifteen != 0.39 {
		t.Errorf("load = (%v, %v, %v), want (0.52, 0.44, 0.39)", one, five, fifteen)
	}
	if got := uptime(); got != 86400 {
		t.Errorf("uptime = %d, want 86400", got)
	}
}

func TestAContainerReadsAsAContainerEndToEnd(t *testing.T) {
	// The whole reading, assembled the way the dashboard receives it.
	fixture(t, map[string]string{
		"cg/memory.max":     "1073741824\n",
		"cg/memory.current": "536870912\n",
		"cg/memory.stat":    "inactive_file 0\n",
		"cg/cpu.max":        "50000 100000\n",
		"cg/cpu.stat":       "usage_usec 5000000\n",
		"proc/meminfo":      "MemTotal: 65805304 kB\nMemAvailable: 60000000 kB\n",
		"proc/loadavg":      "1.00 0.50 0.25 1/1 1\n",
		"proc/uptime":       "3600.00 1.00\n",
	})

	s := NewReader(t.TempDir()).Read()

	if !s.Supported {
		t.Fatal("Linux reported itself unsupported")
	}
	// The source is what makes the number readable: "80% of 512 MB (container
	// limit)" and "80% of 64 GB (host)" are different facts.
	if s.Source != SourceCgroup {
		t.Errorf("source = %q, want %q when a memory limit is set", s.Source, SourceCgroup)
	}
	if s.MemTotalBytes != 1073741824 {
		t.Errorf("total = %d, want the container's limit rather than the host's", s.MemTotalBytes)
	}
	if p := s.MemPercent(); math.Abs(p-50) > 0.001 {
		t.Errorf("memory = %v%%, want 50", p)
	}
	if math.Abs(s.CPUCores-0.5) > 0.0001 {
		t.Errorf("cores = %v, want the 0.5 the quota allows", s.CPUCores)
	}
	if s.Load1 != 1 || s.UptimeSec != 3600 {
		t.Errorf("load1 = %v, uptime = %d, want 1 and 3600", s.Load1, s.UptimeSec)
	}
	// Disk comes from the real filesystem, not the fixture: it is a statfs call
	// rather than a file, and the temp directory is a real path.
	if s.DiskTotalBytes == 0 {
		t.Error("no disk total was read for the working directory")
	}
}

func TestKeyedReadsTakeTheNamedFieldOnly(t *testing.T) {
	fixture(t, map[string]string{
		"cg/memory.stat": "anon 111\ninactive_file 222\nfile 333\n",
	})

	if got := readKeyed(cgPath(cgMemStat), "inactive_file"); got != 222 {
		t.Errorf("inactive_file = %d, want 222", got)
	}
	if got := readKeyed(cgPath(cgMemStat), "absent"); got != 0 {
		t.Errorf("a missing key = %d, want 0", got)
	}
}

func TestSubtractingMoreThanThereIsGivesZero(t *testing.T) {
	// Unsigned arithmetic, so the wrap would report sixteen exabytes in use and
	// the panel would show a number nobody could explain.
	if got := subtractSaturating(10, 25); got != 0 {
		t.Errorf("10 - 25 = %d, want 0", got)
	}
	if got := subtractSaturating(25, 10); got != 15 {
		t.Errorf("25 - 10 = %d, want 15", got)
	}
}
