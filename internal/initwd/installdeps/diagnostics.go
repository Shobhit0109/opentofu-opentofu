package installdeps

import (
	"context"
	"fmt"
	"strings"

	"github.com/apparentlymart/go-workgraph/workgraph"

	"github.com/opentofu/opentofu/internal/tfdiags"
)

// once is a wrapper around workgraph.Once that adapts it in two ways that
// make it more convenient to use in this package:
//
//   - It uses our [context.Context] conventions for passing cross-cutting
//     concerns like [workgraph.Worker].
//   - It returns [tfdiags.Diagnostics] instead of [error].
type once[T any] struct {
	inner workgraph.Once[withDiagnostics[T]]
}

func (o *once[T]) Do(ctx context.Context, f func(ctx context.Context) (T, tfdiags.Diagnostics)) (T, tfdiags.Diagnostics) {
	withDiags, err := o.inner.Do(workerFromContext(ctx), func(w *workgraph.Worker) (withDiagnostics[T], error) {
		ctx := contextWithWorker(ctx, w)
		v, diags := f(ctx)
		return withDiagnostics[T]{
			value: v,
			diags: diags,
		}, nil
	})

	if err != nil {
		// Since we return our own errors through the diagnostics, an error
		// here is always from the workgraph package, and we'll translate
		// those into diagnostics before we return.
		var zero T
		return zero, diagnosticsForWorkgraphError(ctx, err)
	}
	return withDiags.value, withDiags.diags
}

func (o *once[T]) RequestID() workgraph.RequestID {
	return o.inner.RequestID()
}

type withDiagnostics[T any] struct {
	value T
	diags tfdiags.Diagnostics
}

// maybe represents a value that may or may not be known.
//
// The zero value of a maybe type represents "unknown". Construct known values
// using [known].
type maybe[T any] struct {
	v  T
	ok bool
}

// Retrieves the known value if any, along with a flag indicating whether the
// value is known.
func (m maybe[T]) Get() (T, bool) {
	return m.v, m.ok
}

// known builds a [maybe] value that is known.
func known[T any](v T) maybe[T] {
	return maybe[T]{v: v, ok: true}
}

// known builds a [maybe] value that is unknown.
//
// The result is always the zero value of its type.
func unknown[T any]() maybe[T] {
	return maybe[T]{}
}

// mapMaybe derives a [maybe[U]] from a [maybe[T]] using a callback function.
//
// If m is unknown then the result is also unknown and f is not called at all.
func mapMaybe[T, U any](m maybe[T], f func(T) U) maybe[U] {
	v, isKnown := m.Get()
	if !isKnown {
		return unknown[U]()
	}
	return known(f(v))
}

// maybePtr returns a known maybe of the same pointer if it's non-nil, or
// an unknown maybe of the same pointer type if the pointer is nil.
//
// This is an adapter for situations where a nil pointer represents the
// "unknown" case, typically used in conjunction with [mapMaybe].
func maybePtr[T any](ptr *T) maybe[*T] {
	if ptr == nil {
		return unknown[*T]()
	}
	return known(ptr)
}

func diagnosticsForWorkgraphError(ctx context.Context, err error) tfdiags.Diagnostics {
	// The workgraph errors include workgraph.RequestID values identifying
	// the requests in an opaque way, so we'll build a lookup table of
	// suitable friendly names for each one. We intentionally defer that
	// until we actually have an error to return because on the happy
	// path we have no need for user-friendly request names at all.

	reqNames := make(map[workgraph.RequestID]string)
	for reqId, name := range collectRequestNames(ctx) {
		reqNames[reqId] = name
	}

	var diags tfdiags.Diagnostics
	switch err := err.(type) {
	case workgraph.ErrUnresolved:
		reqName, ok := reqNames[err.RequestID]
		if !ok {
			reqName = "<unknown request>"
		}
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Dependency installation failed",
			fmt.Sprintf("During dependency installation, %q was left unresolved. This is a bug in OpenTofu.", reqName),
		))
	case workgraph.ErrSelfDependency:
		var detailBuf strings.Builder
		detailBuf.WriteString("Unresolvable cyclic dependency detected during installation:\n")
		for _, reqId := range err.RequestIDs {
			reqName, ok := reqNames[reqId]
			if !ok {
				reqName = "<unknown dependency>"
			}
			fmt.Fprintf(&detailBuf, "  - %s\n", reqName)
		}
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Dependency installation failed",
			detailBuf.String(),
		))
	default:
		// We should not get here unless workgraph starts returning a new
		// kind of error in a future version.
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Dependency installation failed",
			fmt.Sprintf("An internal error occurred while installing dependencies: %s.", tfdiags.FormatError(err)),
		))
	}
	return diags
}

type withSourceRange[T any] struct {
	Value       T
	SourceRange tfdiags.SourceRange
}
