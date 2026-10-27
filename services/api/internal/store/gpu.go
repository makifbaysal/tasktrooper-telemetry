package store

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/timerange"
)

// GPUs (contracts §22, deploy/clickhouse/004_gpu.sql): trooper-agent's nvidia-smi gauges, one row per GPU and minute.
// A host row carries the latest minute of each GPU; a GPU that stopped reporting goneAfter before its host did is gone.

var gpuCols = []string{"utilization", "memory", "memory_bytes", "temperature_c", "power_w"}

// hostGPUs returns host → its GPUs (by index) within the snapshot window; host "" = every host.
func (d *DB) hostGPUs(ctx context.Context, q Q, cluster, host string) (map[string][]gpuRow, error) {
	where, args := andEq("", nil, "cluster", cluster)
	where, args = andEq(where, args, "host", host)
	rows, err := d.latest(ctx, gaugeQuery{table: "gpus_1m", keys: []string{"host", "toString(gpu_index)"}, cols: gpuCols,
		strs: []string{"gpu_uuid", "model"}, where: where, args: args}, snapFrom(q), snapTo(q))
	if err != nil {
		return nil, err
	}
	out := map[string][]gpuRow{}
	for _, r := range rows {
		idx, _ := strconv.Atoi(r.keys[1])
		out[r.keys[0]] = append(out[r.keys[0]], gpuRow{last: r.last, g: model.HostGPU{Index: idx, UUID: r.s[0], Model: r.s[1],
			Utilization: r.v[0], Memory: r.v[1], MemoryBytes: r.v[2], TemperatureC: r.v[3], PowerW: r.v[4]}})
	}
	return out, nil
}

type gpuRow struct {
	g    model.HostGPU
	last time.Time
}

// AttachGPUs sets Host.GPUs (always non-nil) from the GPU rows, dropping GPUs gone before the host's newest minute.
func AttachGPUs(hosts []model.Host, gpus map[string][]gpuRow) {
	for i := range hosts {
		h := &hosts[i]
		h.GPUs = []model.HostGPU{}
		last := time.UnixMilli(h.LastSeen)
		for _, r := range gpus[h.ID] {
			if last.Sub(r.last) > goneAfter {
				continue
			}
			h.GPUs = append(h.GPUs, r.g)
		}
		sort.Slice(h.GPUs, func(a, b int) bool { return h.GPUs[a].Index < h.GPUs[b].Index })
	}
}

// gpuSeries is HostSeries.GPU: utilization (0..1) per GPU, ids gpu<index>, ordered by index.
func (d *DB) gpuSeries(ctx context.Context, w timerange.Window, host string, gpus []model.HostGPU) (timerange.Set, error) {
	byIdx, err := d.gaugeBuckets(ctx, w, seriesQuery{table: "gpus_1m", entity: []string{"gpu_index", "gpu_uuid"},
		group: "toString(gpu_index)", cols: []string{"utilization"}, where: " AND host = ?", args: []any{host}, perBucket: "avg"})
	if err != nil {
		return timerange.Set{}, err
	}
	return GPUSet(w, gpus, byIdx), nil
}

// GPUSet builds the per-GPU utilization SeriesSet (one series per current GPU, label "GPU <i> · <model>").
func GPUSet(w timerange.Window, gpus []model.HostGPU, byIdx map[string][][]float64) timerange.Set {
	ids := make([]string, len(gpus))
	for i, g := range gpus {
		ids[i] = fmt.Sprintf("gpu%d", g.Index)
	}
	b := timerange.NewBuilder(w, ids...)
	for i, g := range gpus {
		label := fmt.Sprintf("GPU %d", g.Index)
		if g.Model != "" {
			label += " · " + g.Model
		}
		b.Label(ids[i], label)
		if s := byIdx[strconv.Itoa(g.Index)]; s != nil {
			copy(b.Data(ids[i]), s[0])
		}
	}
	return b.Build()
}
