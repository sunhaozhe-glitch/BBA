package taskengine

import (
	"context"
	"errors"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Dedicated task AND rotation sessions share these limits, independently of
// Pool.MaxConns. All Workers using a Store's Pool share its eight slots. Even
// arbitrarily many Stores in this process cannot open more than 32 additional
// task/rotation connections (including connections still dialing or closing).
// These bounds do not cover other dedicated connection users such as content
// guards, nor the sum of configured pools or other OS processes.
const (
	taskLeaseStoreLimit   = 8
	taskLeaseProcessLimit = 32
)

// ErrLeaseConnectionBudget distinguishes local overload from advisory-lock
// contention (nil lease, nil error). Admission never waits for capacity.
var ErrLeaseConnectionBudget = errors.New("taskengine: dedicated lease connection budget exhausted")

type leaseConnectionBudget struct {
	mu     sync.Mutex
	total  int
	byPool map[*pgxpool.Pool]int
}

var processLeaseConnections leaseConnectionBudget

func (b *leaseConnectionBudget) acquire(ctx context.Context, pool *pgxpool.Pool) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if b.total >= taskLeaseProcessLimit || b.byPool[pool] >= taskLeaseStoreLimit {
		return nil, ErrLeaseConnectionBudget
	}
	if b.byPool == nil {
		b.byPool = make(map[*pgxpool.Pool]int)
	}
	b.total++
	b.byPool[pool]++
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			b.total--
			b.byPool[pool]--
			// Do not retain closed or idle Stores indefinitely: at most 32 keys exist.
			if b.byPool[pool] == 0 {
				delete(b.byPool, pool)
			}
		})
	}, nil
}
