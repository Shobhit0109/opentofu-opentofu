package installdeps

import (
	"context"
	"iter"
	"sync"

	"github.com/apparentlymart/go-workgraph/workgraph"
)

// installTracker is a sort of scratchpad where we keep transient information
// about an installation sequence that's currently running.
type installTracker struct {
	installer *Installer
	destDir   string

	mu                         sync.Mutex
	moduleRegistryRequirements map[moduleRegistryRequirementKey]*moduleRegistryRequirement
}

// contextWithNewTracker internally instantiates a new [installTracker] and
// returns a derived [context.Context] that's associated with it.
func contextWithNewTracker(base context.Context, installer *Installer, destDir string) context.Context {
	return context.WithValue(base, trackerContextKey, &installTracker{
		installer: installer,
		destDir:   destDir,
	})
}

func currentInstaller(ctx context.Context) *Installer {
	tracker := trackerFromContext(ctx)
	return tracker.installer
}

func currentDestDir(ctx context.Context) string {
	tracker := trackerFromContext(ctx)
	return tracker.destDir
}

func (t *installTracker) Lock() {
	t.mu.Lock()
}

func (t *installTracker) Unlock() {
	t.mu.Unlock()
}

// collectRequestNames returns an iterable sequence of all of the
// [workgraph.RequestID] values for requests that have been started and a
// user-facing name for each one.
//
// This is intended only for promise-related error return paths where we
// defer the work of collecting user-facing names until we know we're already
// returning an error, and so the user-facing names don't exist at all in
// the happy path where everything resolves correctly.
//
// During iteration other interactions with the given context's [installTracker]
// will be blocked, so the caller should complete iteration quickly and then
// drop the sequence.
func collectRequestNames(ctx context.Context) iter.Seq2[workgraph.RequestID, string] {
	return func(yield func(workgraph.RequestID, string) bool) {
		tracker := trackerFromContext(ctx)
		tracker.yieldRequestNames(yield)
	}
}

func (t *installTracker) yieldRequestNames(yield func(workgraph.RequestID, string) bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !yieldRequestNamesInMap(t.moduleRegistryRequirements, yield) {
		return
	}
}

func yieldRequestNamesInMap[K comparable, V requestNameYielder](m map[K]V, yield func(workgraph.RequestID, string) bool) bool {
	for _, yielder := range m {
		if !yielder.yieldRequestNames(yield) {
			return false
		}
	}
	return true
}

type requestNameYielder interface {
	yieldRequestNames(yield func(workgraph.RequestID, string) bool) bool
}
