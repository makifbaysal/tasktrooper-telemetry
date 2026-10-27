package infra

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/process"
	"go.opentelemetry.io/otel/attribute"

	"github.com/tasktrooper/telemetry/services/collector/internal/emit"
)

// MaxCommand bounds process.command (contracts §20).
const MaxCommand = 128

// processTracker remembers each process's CPU time between scrapes (keyed by pid + start time so
// a reused pid is a new process).
type processTracker struct {
	fs   HostFS
	prev map[int32]procCPU
	at   time.Time
}

type procCPU struct {
	created int64
	cpu     float64 // user+system seconds
}

func newProcessTracker(fs HostFS) *processTracker {
	return &processTracker{fs: fs, prev: map[int32]procCPU{}}
}

type procSample struct {
	p      *process.Process
	cpu    float64 // cores
	hasCPU bool
	rss    float64
}

// scrape reports the top n processes: the busiest half by CPU, the rest by memory (so both a
// spinning process and a memory hog show up). Command arguments are never collected (PII):
// process.command is the first token of the command line only.
func (t *processTracker) scrape(ctx context.Context, n int, now time.Time) emit.Result {
	var r emit.Result
	procs, err := process.ProcessesWithContext(ctx)
	if err != nil {
		return r
	}
	elapsed := now.Sub(t.at).Seconds()
	first := t.at.IsZero()
	next := make(map[int32]procCPU, len(procs))
	samples := make([]procSample, 0, len(procs))
	for _, p := range procs {
		if ctx.Err() != nil {
			break
		}
		times, err := p.TimesWithContext(ctx)
		if err != nil {
			continue // gone or not readable
		}
		created, _ := p.CreateTimeWithContext(ctx)
		cur := times.User + times.System
		next[p.Pid] = procCPU{created: created, cpu: cur}
		s := procSample{p: p}
		if prev, ok := t.prev[p.Pid]; ok && !first && elapsed > 0 && prev.created == created {
			s.cpu = max(0, (cur-prev.cpu)/elapsed)
			s.hasCPU = true
		}
		if mi, err := p.MemoryInfoWithContext(ctx); err == nil && mi != nil {
			s.rss = float64(mi.RSS)
		}
		samples = append(samples, s)
	}
	t.prev, t.at = next, now

	picked := pickTop(samples, n)
	for _, s := range picked {
		name, _ := s.p.NameWithContext(ctx)
		cmd := ""
		if parts, err := s.p.CmdlineSliceWithContext(ctx); err == nil && len(parts) > 0 {
			cmd = FirstToken(parts[0])
		}
		if name == "" {
			name = filepath.Base(cmd)
		}
		attrs := []attribute.KeyValue{
			attribute.Int64(emit.AttrProcessPID, int64(s.p.Pid)),
			attribute.String(emit.AttrProcessExe, name),
			attribute.String(emit.AttrProcessCommand, cmd),
			attribute.String(emit.AttrContainerID, t.containerID(s.p.Pid)),
		}
		if s.hasCPU {
			r.Gauge(emit.ProcessCPU, s.cpu, attrs...)
		}
		r.Gauge(emit.ProcessMemoryBytes, s.rss, attrs...)
		if th, err := s.p.NumThreadsWithContext(ctx); err == nil {
			r.Gauge(emit.ProcessThreads, float64(th), attrs...)
		}
		if runtime.GOOS == "linux" {
			if fds, err := s.p.NumFDsWithContext(ctx); err == nil {
				r.Gauge(emit.ProcessFDs, float64(fds), attrs...)
			}
		}
	}
	return r
}

// pickTop selects ceil(n/2) processes by CPU (only those using CPU), then fills up to n by RSS.
func pickTop(samples []procSample, n int) []procSample {
	if n <= 0 || len(samples) == 0 {
		return nil
	}
	byCPU := slices.Clone(samples)
	slices.SortStableFunc(byCPU, func(a, b procSample) int {
		switch {
		case a.cpu > b.cpu:
			return -1
		case a.cpu < b.cpu:
			return 1
		case a.rss > b.rss:
			return -1
		case a.rss < b.rss:
			return 1
		}
		return 0
	})
	out := make([]procSample, 0, n)
	taken := map[int32]bool{}
	for _, s := range byCPU {
		if len(out) >= (n+1)/2 || s.cpu <= 0 {
			break
		}
		out = append(out, s)
		taken[s.p.Pid] = true
	}
	byMem := slices.Clone(samples)
	slices.SortStableFunc(byMem, func(a, b procSample) int {
		switch {
		case a.rss > b.rss:
			return -1
		case a.rss < b.rss:
			return 1
		}
		return 0
	})
	for _, s := range byMem {
		if len(out) >= n {
			break
		}
		if !taken[s.p.Pid] {
			out = append(out, s)
			taken[s.p.Pid] = true
		}
	}
	return out
}

// FirstToken returns the first whitespace-separated token of a command line, ≤ MaxCommand bytes.
// Some programs rewrite argv[0] to contain their arguments ("postgres: checkpointer"); only the
// part before the first space/colon-space is kept.
func FirstToken(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, " \t\n"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSuffix(s, ":")
	if len(s) > MaxCommand {
		s = s[:MaxCommand]
	}
	return s
}

var cgroupContainerID = regexp.MustCompile(`([0-9a-f]{64})`)

// containerID extracts the 64-hex container id from /proc/<pid>/cgroup (docker, containerd,
// cri-o, systemd slices); "" for host processes.
func (t *processTracker) containerID(pid int32) string {
	if runtime.GOOS != "linux" {
		return ""
	}
	b, err := os.ReadFile(t.fs.proc(strconv.Itoa(int(pid)), "cgroup"))
	if err != nil {
		return ""
	}
	return ContainerIDFromCgroup(string(b))
}

// ContainerIDFromCgroup returns the last 64-hex id in a cgroup file ("" when none).
func ContainerIDFromCgroup(s string) string {
	m := cgroupContainerID.FindAllString(s, -1)
	if len(m) == 0 {
		return ""
	}
	return m[len(m)-1]
}
