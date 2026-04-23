package worker

import (
	"context"
	"log"
	"sync"
	"time"

	sqlite "github.com/caesarioshiddieq/claude-code-lark-channel/internal/sqlite"
)

// Fetcher is the DB interface the Pool needs to poll for work.
type Fetcher interface {
	NextInboxRowExcluding(ctx context.Context, busy []string) (sqlite.InboxRow, bool, error)
}

// ProcessDeps holds everything Pool needs to dispatch work.
type ProcessDeps struct {
	DB      Fetcher
	Process func(ctx context.Context, row sqlite.InboxRow)
}

// Pool dispatches inbox rows to up to N concurrent goroutines,
// enforcing per-task exclusion via a busy set.
type Pool struct {
	deps ProcessDeps
	n    int
	sem  chan struct{}
	busy sync.Map
	wg   sync.WaitGroup
}

// NewPool creates a Pool with the given deps and concurrency limit n (clamped to [1,4]).
func NewPool(deps ProcessDeps, n int) *Pool {
	if n < 1 {
		n = 1
	}
	if n > 4 {
		n = 4
	}
	return &Pool{deps: deps, n: n, sem: make(chan struct{}, n)}
}

// Run starts the dispatcher. Blocks until ctx is done and all in-flight jobs finish.
func (p *Pool) Run(ctx context.Context) {
	defer p.wg.Wait()
	for {
		busyKeys := p.snapshotBusy()
		row, found, err := p.deps.DB.NextInboxRowExcluding(ctx, busyKeys)
		if err != nil {
			log.Printf("pool: NextInboxRowExcluding: %v", err)
			select {
			case <-time.After(500 * time.Millisecond):
				continue
			case <-ctx.Done():
				return
			}
		}
		if !found {
			select {
			case <-time.After(500 * time.Millisecond):
				continue
			case <-ctx.Done():
				return
			}
		}
		p.busy.Store(row.TaskID, true)
		select {
		case p.sem <- struct{}{}:
		case <-ctx.Done():
			p.busy.Delete(row.TaskID)
			return
		}
		p.wg.Add(1)
		go p.runOne(ctx, row)
	}
}

func (p *Pool) snapshotBusy() []string {
	var out []string
	p.busy.Range(func(k, _ any) bool {
		out = append(out, k.(string))
		return true
	})
	return out
}

func (p *Pool) runOne(ctx context.Context, row sqlite.InboxRow) {
	defer p.wg.Done()
	defer func() { <-p.sem }()
	defer p.busy.Delete(row.TaskID)
	defer func() {
		if r := recover(); r != nil {
			log.Printf("pool: runOne panic task=%s phase=%s: %v",
				row.TaskID, row.Phase, r)
		}
	}()
	p.deps.Process(ctx, row)
}
