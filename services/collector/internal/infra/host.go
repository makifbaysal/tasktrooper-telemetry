package infra

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	psnet "github.com/shirou/gopsutil/v4/net"

	"github.com/tasktrooper/telemetry/services/collector/internal/emit"
	"github.com/tasktrooper/telemetry/services/collector/internal/rate"
)

// HostFS locates the host's filesystems when the agent runs in a container (DaemonSet with
// hostPID and read-only /proc, /sys, / mounts). Empty fields = the agent's own view.
type HostFS struct {
	Proc string // TT_AGENT_PROCFS → HOST_PROC (default /host/proc when it exists)
	Sys  string // HOST_SYS (default /host/sys when it exists)
	Root string // TT_AGENT_ROOTFS → HOST_ROOT (default /host/root when it exists): disk usage, /etc/hostname, machine-id
}

// DetectHostFS resolves the host mounts from the environment and applies them to gopsutil
// (HOST_PROC / HOST_SYS / HOST_ETC / HOST_ROOT), which reads these variables on every call.
func DetectHostFS() HostFS {
	pick := func(envs []string, def string) string {
		for _, e := range envs {
			if v := os.Getenv(e); v != "" {
				return v
			}
		}
		if st, err := os.Stat(def); err == nil && st.IsDir() {
			return def
		}
		return ""
	}
	fs := HostFS{
		Proc: pick([]string{"TT_AGENT_PROCFS", "HOST_PROC"}, "/host/proc"),
		Sys:  pick([]string{"TT_AGENT_SYSFS", "HOST_SYS"}, "/host/sys"),
		Root: pick([]string{"TT_AGENT_ROOTFS", "HOST_ROOT"}, "/host/root"),
	}
	set := func(k, v string) {
		if v != "" && os.Getenv(k) == "" {
			_ = os.Setenv(k, v)
		}
	}
	set("HOST_PROC", fs.Proc)
	set("HOST_SYS", fs.Sys)
	set("HOST_ROOT", fs.Root)
	if fs.Root != "" {
		set("HOST_ETC", filepath.Join(fs.Root, "etc"))
		set("HOST_VAR", filepath.Join(fs.Root, "var"))
		set("HOST_RUN", filepath.Join(fs.Root, "run"))
	}
	return fs
}

func (fs HostFS) proc(p ...string) string {
	base := fs.Proc
	if base == "" {
		base = "/proc"
	}
	return filepath.Join(append([]string{base}, p...)...)
}

func (fs HostFS) root(p string) string {
	if fs.Root == "" {
		return p
	}
	return filepath.Join(fs.Root, p)
}

// HostName returns host.name: TT_AGENT_HOSTNAME, NODE_NAME, the host's /etc/hostname, os.Hostname.
func (fs HostFS) HostName() string {
	if v := os.Getenv("TT_AGENT_HOSTNAME"); v != "" {
		return v
	}
	if v := os.Getenv("NODE_NAME"); v != "" {
		return v
	}
	if fs.Root != "" {
		if b, err := os.ReadFile(fs.root("/etc/hostname")); err == nil {
			if h := strings.TrimSpace(string(b)); h != "" {
				return h
			}
		}
	}
	h, _ := os.Hostname()
	return h
}

// HostID returns host.id: the machine-id (contracts §20), else gopsutil's host id
// (DMI product uuid / IOPlatformUUID / boot id).
func (fs HostFS) HostID() string {
	for _, p := range []string{fs.root("/etc/machine-id"), fs.root("/var/lib/dbus/machine-id")} {
		if b, err := os.ReadFile(p); err == nil {
			if id := strings.TrimSpace(string(b)); id != "" {
				return id
			}
		}
	}
	id, _ := host.HostID()
	return id
}

// ---------------------------------------------------------------- host scraper

// HostScraper reports trooper.host.* and the top processes (trooper.process.*).
type HostScraper struct {
	FS       HostFS
	TopN     int
	GPU      *GPUReader // nil = no NVIDIA GPU / nvidia-smi
	counters *rate.Counters
	procs    *processTracker
	prevCPU  *cpu.TimesStat
	now      func() time.Time
}

// NewHostScraper builds the scraper.
func NewHostScraper(fs HostFS, topN int) *HostScraper {
	return &HostScraper{FS: fs, TopN: topN, counters: rate.NewCounters(), procs: newProcessTracker(fs), now: time.Now}
}

// Close implements scrape.Scraper.
func (h *HostScraper) Close() error { return nil }

// Scrape implements scrape.Scraper. Individual readings that fail are skipped (no sample → -Inf
// in ClickHouse); the scrape fails only when CPU and memory are both unreadable.
func (h *HostScraper) Scrape(ctx context.Context) (emit.Result, error) {
	var r emit.Result
	now := h.now()
	var errs []error

	if t, err := cpu.TimesWithContext(ctx, false); err == nil && len(t) > 0 {
		cur := t[0]
		if h.prevCPU != nil {
			if v, ok := cpuBusy(*h.prevCPU, cur); ok {
				r.Gauge(emit.HostCPU, v)
			}
		}
		h.prevCPU = &cur
	} else {
		errs = append(errs, fmt.Errorf("cpu: %w", orNoData(err)))
	}
	if n, err := cpu.CountsWithContext(ctx, true); err == nil && n > 0 {
		r.Gauge(emit.HostCPUCores, float64(n))
	}
	if v, err := mem.VirtualMemoryWithContext(ctx); err == nil && v.Total > 0 {
		used := float64(v.Total - v.Available)
		if v.Available > v.Total {
			used = float64(v.Used)
		}
		r.Gauge(emit.HostMemory, used/float64(v.Total))
		r.Gauge(emit.HostMemoryBytes, used)
	} else {
		errs = append(errs, fmt.Errorf("memory: %w", orNoData(err)))
	}
	if len(errs) == 2 {
		return r, errors.Join(errs...)
	}
	if v, ok := h.fullestDisk(ctx); ok {
		r.Gauge(emit.HostDisk, v)
	}
	if rd, wr, ok := h.diskIO(ctx); ok {
		if v, ok := h.counters.Rate("disk_read", rd, now); ok {
			r.Gauge(emit.HostDiskRead, v)
		}
		if v, ok := h.counters.Rate("disk_write", wr, now); ok {
			r.Gauge(emit.HostDiskWrite, v)
		}
	}
	if in, out, ok := h.netIO(ctx); ok {
		if v, ok := h.counters.Rate("net_in", in, now); ok {
			r.Gauge(emit.HostNetIn, v)
		}
		if v, ok := h.counters.Rate("net_out", out, now); ok {
			r.Gauge(emit.HostNetOut, v)
		}
	}
	if l, err := load.AvgWithContext(ctx); err == nil {
		r.Gauge(emit.HostLoad1, l.Load1)
	}
	if up, err := host.UptimeWithContext(ctx); err == nil && up > 0 {
		r.Gauge(emit.HostUptimeS, float64(up))
	}
	if h.TopN > 0 {
		r.Merge(h.procs.scrape(ctx, h.TopN, now))
	}
	h.GPU.Scrape(ctx, &r)
	return r, nil
}

func orNoData(err error) error {
	if err == nil {
		return errors.New("no data")
	}
	return err
}

// cpuBusy is 1 − Δ(idle+iowait)/Δtotal between two readings.
func cpuBusy(prev, cur cpu.TimesStat) (float64, bool) {
	total := func(t cpu.TimesStat) float64 {
		return t.User + t.System + t.Idle + t.Nice + t.Iowait + t.Irq + t.Softirq + t.Steal
	}
	dt := total(cur) - total(prev)
	if dt <= 0 {
		return 0, false
	}
	idle := (cur.Idle + cur.Iowait) - (prev.Idle + prev.Iowait)
	return rate.Clamp01(1 - idle/dt), true
}

// realFS are the filesystem types considered for trooper.host.disk.
var skipFS = map[string]bool{
	"tmpfs": true, "devtmpfs": true, "overlay": true, "squashfs": true, "proc": true, "sysfs": true,
	"cgroup": true, "cgroup2": true, "nsfs": true, "autofs": true, "devpts": true, "mqueue": true,
	"debugfs": true, "tracefs": true, "securityfs": true, "pstore": true, "bpf": true, "configfs": true,
	"fusectl": true, "hugetlbfs": true, "binfmt_misc": true, "rpc_pipefs": true, "ramfs": true, "devfs": true,
	"nullfs": true, "fuse.lxcfs": true, "shm": true, "efivarfs": true, "iso9660": true, "nfs": true, "nfs4": true,
	"cifs": true, "smbfs": true, "fuse.sshfs": true,
}

// fullestDisk returns the highest used ratio over local filesystems ≥ 100 MB.
func (h *HostScraper) fullestDisk(ctx context.Context) (float64, bool) {
	parts, err := disk.PartitionsWithContext(ctx, false)
	if err != nil {
		return 0, false
	}
	best, ok := 0.0, false
	seen := map[string]bool{}
	for _, p := range parts {
		if skipFS[p.Fstype] || seen[p.Device] && p.Device != "" || strings.HasPrefix(p.Mountpoint, "/var/lib/kubelet/") ||
			strings.HasPrefix(p.Mountpoint, "/var/lib/docker/") || strings.HasPrefix(p.Mountpoint, "/snap/") ||
			strings.HasPrefix(p.Mountpoint, "/run/") || strings.HasPrefix(p.Mountpoint, "/boot/efi") {
			continue
		}
		seen[p.Device] = true
		u, err := disk.UsageWithContext(ctx, h.FS.root(p.Mountpoint))
		if err != nil || u.Total < 100<<20 {
			continue
		}
		if v := u.UsedPercent / 100; !ok || v > best {
			best, ok = v, true
		}
	}
	return rate.Clamp01(best), ok
}

var partitionName = regexp.MustCompile(`^(sd[a-z]+|vd[a-z]+|xvd[a-z]+|hd[a-z]+)\d+$|^(nvme\d+n\d+|mmcblk\d+)p\d+$`)

// diskIO sums read/write bytes over whole physical disks (no partitions, loop, ram, dm, md).
func (h *HostScraper) diskIO(ctx context.Context) (read, write float64, ok bool) {
	io, err := disk.IOCountersWithContext(ctx)
	if err != nil || len(io) == 0 {
		return 0, 0, false
	}
	for name, c := range io {
		if skipDisk(name) {
			continue
		}
		read += float64(c.ReadBytes)
		write += float64(c.WriteBytes)
		ok = true
	}
	return read, write, ok
}

func skipDisk(name string) bool {
	for _, p := range []string{"loop", "ram", "zram", "dm-", "md", "sr", "fd", "nbd"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return partitionName.MatchString(name)
}

var virtualIface = []string{"lo", "veth", "docker", "br-", "cni", "flannel", "cali", "vxlan", "tunl", "kube-", "virbr",
	"lxc", "cilium", "weave", "tun", "tap", "utun", "gif", "stf", "awdl", "llw", "anpi", "bridge", "ap", "genev", "nodelocaldns", "eni"}

func skipIface(name string) bool {
	if name == "lo" || name == "lo0" {
		return true
	}
	for _, p := range virtualIface {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// netIO sums rx/tx bytes over physical interfaces. On Linux the host's namespace is read via
// /proc/1/net/dev (the agent's own /proc/net is the pod's namespace unless hostNetwork).
func (h *HostScraper) netIO(ctx context.Context) (in, out float64, ok bool) {
	var stats []psnet.IOCountersStat
	var err error
	if runtime.GOOS == "linux" {
		stats, err = psnet.IOCountersByFileWithContext(ctx, true, h.FS.proc("1", "net", "dev"))
	}
	if runtime.GOOS != "linux" || err != nil {
		stats, err = psnet.IOCountersWithContext(ctx, true)
	}
	if err != nil {
		return 0, 0, false
	}
	for _, s := range stats {
		if skipIface(s.Name) {
			continue
		}
		in += float64(s.BytesRecv)
		out += float64(s.BytesSent)
		ok = true
	}
	return in, out, ok
}
