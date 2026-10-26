// Package leader elects shard owners with Postgres session-level advisory locks.
//
// Shard k (0 ≤ k < N) is owned by whoever holds pg_advisory_lock(Base + k) on a dedicated session.
// With N = 1 this is plain leader election: one instance evaluates, the others stand by and take over
// within one Tick after the leader's session dies (Postgres drops session locks with the connection).
// With N > 1 each instance takes at most MaxPerInstance free shards, so replicas split the monitors
// (id % N); see README § Leader election and sharding.
package leader

import (
	"context"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultBase is the advisory lock key of shard 0 ("ttalert" in ASCII).
const DefaultBase int64 = 0x7474616c657274

// Elector holds shard locks on one pooled connection.
type Elector struct {
	Pool           *pgxpool.Pool
	Base           int64
	Shards         int
	MaxPerInstance int

	conn *pgxpool.Conn
	held map[int]bool
}

// Tick verifies the session, tries to take free shards and returns the held shards (sorted).
func (e *Elector) Tick(ctx context.Context) ([]int, error) {
	if e.Shards < 1 {
		e.Shards = 1
	}
	if e.MaxPerInstance < 1 || e.MaxPerInstance > e.Shards {
		e.MaxPerInstance = e.Shards
	}
	if e.conn != nil {
		if _, err := e.conn.Exec(ctx, "SELECT 1"); err != nil {
			e.drop()
		}
	}
	if e.conn == nil {
		c, err := e.Pool.Acquire(ctx)
		if err != nil {
			return nil, err
		}
		e.conn, e.held = c, map[int]bool{}
	}
	for k := 0; k < e.Shards && len(e.held) < e.MaxPerInstance; k++ {
		if e.held[k] {
			continue
		}
		var ok bool
		if err := e.conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", e.Base+int64(k)).Scan(&ok); err != nil {
			e.drop()
			return nil, err
		}
		if ok {
			e.held[k] = true
		}
	}
	return e.Held(), nil
}

// Held returns the shards currently held.
func (e *Elector) Held() []int {
	out := make([]int, 0, len(e.held))
	for k := range e.held {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

// drop forgets the session; its locks are released when the connection is destroyed.
func (e *Elector) drop() {
	if e.conn != nil {
		e.conn.Hijack().Close(context.Background())
	}
	e.conn, e.held = nil, nil
}

// Release unlocks everything and returns the connection.
func (e *Elector) Release() {
	if e.conn == nil {
		return
	}
	_, _ = e.conn.Exec(context.Background(), "SELECT pg_advisory_unlock_all()")
	e.conn.Release()
	e.conn, e.held = nil, nil
}
