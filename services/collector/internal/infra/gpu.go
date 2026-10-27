package infra

import (
	"context"
	"encoding/csv"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/tasktrooper/telemetry/services/collector/internal/emit"
)

// GPUQuery is the nvidia-smi query (contracts §22).
const GPUQuery = "index,uuid,name,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw"

// GPUReader reads NVIDIA GPUs with nvidia-smi. Absent binary or a failing call → no samples and no
// error (GPU metrics are optional; they never flip trooper.collector.up).
type GPUReader struct {
	Bin     string // resolved nvidia-smi path ("" = disabled)
	Timeout time.Duration
}

// NewGPUReader uses override (TT_AGENT_NVIDIA_SMI) or nvidia-smi from PATH; nil when neither exists.
func NewGPUReader(override string) *GPUReader {
	bin := override
	if bin == "" {
		p, err := exec.LookPath("nvidia-smi")
		if err != nil {
			return nil
		}
		bin = p
	}
	return &GPUReader{Bin: bin, Timeout: 5 * time.Second}
}

// Scrape runs nvidia-smi once and appends trooper.host.gpu.* to r.
func (g *GPUReader) Scrape(ctx context.Context, r *emit.Result) {
	if g == nil || g.Bin == "" {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, g.Timeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, g.Bin, "--query-gpu="+GPUQuery, "--format=csv,noheader,nounits").Output()
	if err != nil {
		return
	}
	ParseGPUs(string(out), r)
}

// ParseGPUs parses nvidia-smi CSV (noheader, nounits). Values nvidia-smi cannot read
// ("[N/A]", "[Not Supported]") are skipped; memory is MiB.
func ParseGPUs(out string, r *emit.Result) {
	rd := csv.NewReader(strings.NewReader(out))
	rd.TrimLeadingSpace = true
	rd.FieldsPerRecord = -1
	recs, err := rd.ReadAll()
	if err != nil {
		return
	}
	for _, f := range recs {
		if len(f) < 8 {
			continue
		}
		idx, err := strconv.Atoi(strings.TrimSpace(f[0]))
		if err != nil {
			continue
		}
		attrs := []attribute.KeyValue{
			attribute.Int(emit.AttrGPUIndex, idx),
			attribute.String(emit.AttrGPUModel, strings.TrimSpace(f[2])),
			attribute.String(emit.AttrGPUUUID, strings.TrimSpace(f[1])),
		}
		num := func(s string) (float64, bool) {
			v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
			return v, err == nil
		}
		if v, ok := num(f[3]); ok {
			r.Gauge(emit.HostGPUUtilization, v/100, attrs...)
		}
		used, okU := num(f[4])
		total, okT := num(f[5])
		if okU {
			r.Gauge(emit.HostGPUMemoryBytes, used*(1<<20), attrs...)
		}
		if okU && okT && total > 0 {
			r.Gauge(emit.HostGPUMemory, used/total, attrs...)
		}
		if v, ok := num(f[6]); ok {
			r.Gauge(emit.HostGPUTemperature, v, attrs...)
		}
		if v, ok := num(f[7]); ok {
			r.Gauge(emit.HostGPUPower, v, attrs...)
		}
	}
}
