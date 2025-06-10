package installdeps

import (
	"context"

	"github.com/apparentlymart/go-workgraph/workgraph"
)

// Within this package only we use some special context.Context values to
// deal with cross-cutting concerns that would otherwise end up being separate
// arguments to almost every single function.
//
// Although this is arguably a somewhat non-idomatic use of context.Context,
// it means that we can carry around these cross-cutting concerns along with
// any other cross-cutting concerns provided by an external caller with only
// one boilerplate argument on each function.

type contextKey rune

const eventsContextKey = contextKey('E')
const workerContextKey = contextKey('W')
const trackerContextKey = contextKey('T')

var noEvents = &InstallEvents{}

func eventsFromContext(ctx context.Context) *InstallEvents {
	evts, ok := ctx.Value(eventsContextKey).(*InstallEvents)
	if !ok {
		return noEvents
	}
	return evts
}

func workerFromContext(ctx context.Context) *workgraph.Worker {
	worker, ok := ctx.Value(workerContextKey).(*workgraph.Worker)
	if !ok {
		panic("no worker handle in this context")
	}
	return worker
}

func trackerFromContext(ctx context.Context) *installTracker {
	tracker, ok := ctx.Value(workerContextKey).(*installTracker)
	if !ok {
		panic("no install tracker in this context")
	}
	return tracker
}
