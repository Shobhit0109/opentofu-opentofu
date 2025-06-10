package installdeps

import (
	"context"
	"sync"

	"github.com/apparentlymart/go-workgraph/workgraph"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

// newWorkerContext returns a new context that inherits all
// cancellation/deadlines/values from the given base context except that it's
// associated with a new [workgraph.Worker].
//
// It doesn't matter whether the base worker already has a worker or not.
func newWorkerContext(base context.Context) context.Context {
	return context.WithValue(base, workerContextKey, workgraph.NewWorker())
}

func contextWithWorker(base context.Context, worker *workgraph.Worker) context.Context {
	return context.WithValue(base, workerContextKey, worker)
}

// async runs f in a separate goroutine, with its context associated with
// a newly-allocated [workgraph.Worker] so that it can wait for other promises
// independently of the caller.
func async(ctx context.Context, f func(ctx context.Context)) {
	ctx = newWorkerContext(ctx)
	go f(ctx)
}

type workGroup struct {
	wg sync.WaitGroup

	diags tfdiags.Diagnostics
	mu    sync.Mutex
}

func (wg *workGroup) Run(ctx context.Context, f func(ctx context.Context) tfdiags.Diagnostics) {
	wg.wg.Add(1)
	ctx = newWorkerContext(ctx)
	go func() {
		moreDiags := f(ctx)
		if len(moreDiags) != 0 {
			wg.mu.Lock()
			wg.diags = wg.diags.Append(moreDiags)
			wg.mu.Unlock()
		}
		wg.wg.Done()
	}()
}

func (wg *workGroup) Complete(ctx context.Context) tfdiags.Diagnostics {
	wg.wg.Wait()
	return wg.diags
}
