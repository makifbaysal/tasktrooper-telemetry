//go:build integration

package integration

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/pkg/config"

	"github.com/tasktrooper/telemetry/services/alerting/internal/conn"
	"github.com/tasktrooper/telemetry/services/alerting/internal/leader"
)

func TestLeaderElection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pg, err := conn.Postgres(ctx, config.Load().PostgresURL)
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	defer pg.Close()
	base := leader.DefaultBase + 9201_500

	// single shard: one leader, one standby, failover on release
	a := &leader.Elector{Pool: pg, Base: base, Shards: 1}
	b := &leader.Elector{Pool: pg, Base: base, Shards: 1}
	ha, err := a.Tick(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hb, err := b.Tick(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ha, []int{0}) || len(hb) != 0 {
		t.Fatalf("a=%v b=%v", ha, hb)
	}
	a.Release()
	if hb, _ = b.Tick(ctx); !slices.Equal(hb, []int{0}) {
		t.Fatalf("standby did not take over: %v", hb)
	}
	b.Release()

	// four shards, at most two per instance: replicas split them
	c := &leader.Elector{Pool: pg, Base: base + 10, Shards: 4, MaxPerInstance: 2}
	d := &leader.Elector{Pool: pg, Base: base + 10, Shards: 4, MaxPerInstance: 2}
	defer c.Release()
	defer d.Release()
	hc, _ := c.Tick(ctx)
	hd, _ := d.Tick(ctx)
	if !slices.Equal(hc, []int{0, 1}) || !slices.Equal(hd, []int{2, 3}) {
		t.Fatalf("c=%v d=%v", hc, hd)
	}
}
