package scrape

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"

	"github.com/tasktrooper/telemetry/services/collector/internal/config"
	"github.com/tasktrooper/telemetry/services/collector/internal/emit"
)

// Host reports the machine the agent runs on as trooper.db.cpu / memory / disk of a database.
// Used in Agent mode (sidecar, DaemonSet on the DB node, or a VM next to the database), where
// SQL cannot see OS-level resources. In a container, gopsutil reads the host's /proc only when
// HOST_PROC / HOST_SYS are mounted (the Helm chart does this for kind=DaemonSet).
type Host struct {
	t        config.Database
	role     func() string
	DiskPath string
	log      *slog.Logger
}

// NewHost builds a host scraper for database t. role returns the database scraper's last role.
func NewHost(t config.Database, diskPath string, role func() string, log *slog.Logger) *Host {
	if diskPath == "" {
		diskPath = "/"
	}
	// prime cpu.Percent(0): the first call returns usage since boot
	_, _ = cpu.Percent(0, false)
	return &Host{t: t, role: role, DiskPath: diskPath, log: log}
}

// Close implements Scraper.
func (h *Host) Close() error { return nil }

// Scrape implements Scraper.
func (h *Host) Scrape(ctx context.Context) (emit.Result, error) {
	var r emit.Result
	a := emit.DBAttrs(h.t.Key(), h.t.DisplayName(), SystemName(h.t.Engine), h.t.NodeName(), h.role())
	var errs []error
	if p, err := cpu.PercentWithContext(ctx, 0, false); err == nil && len(p) > 0 {
		r.Gauge(emit.DBCPU, p[0]/100, a...)
	} else if err != nil {
		errs = append(errs, err)
	}
	if v, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		r.Gauge(emit.DBMemory, v.UsedPercent/100, a...)
	} else {
		errs = append(errs, err)
	}
	if d, err := disk.UsageWithContext(ctx, h.DiskPath); err == nil {
		r.Gauge(emit.DBDisk, d.UsedPercent/100, a...)
	} else {
		errs = append(errs, err)
	}
	if len(errs) == 3 {
		return r, fmt.Errorf("host metrics: %v", errs[0])
	}
	return r, nil
}

// SystemName maps the config engine to the db.system attribute value.
func SystemName(engine string) string {
	switch engine {
	case "postgres":
		return "postgresql"
	default:
		return engine
	}
}
