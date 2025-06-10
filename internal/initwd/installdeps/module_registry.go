package installdeps

import (
	"context"
	"fmt"

	"github.com/apparentlymart/go-versions/versions"
	"github.com/apparentlymart/go-workgraph/workgraph"
	"github.com/hashicorp/go-version"
	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

type moduleRegistryRequirement struct {
	addr       addrs.ModuleRegistryPackage
	versions   version.Constraints
	resultOnce once[*moduleRegistryResult]
}

type moduleRegistryRequirementKey struct {
	addr     addrs.ModuleRegistryPackage
	versions string
}

func getModuleRegistryRequirement(ctx context.Context, addr addrs.ModuleRegistryPackage, versions version.Constraints) *moduleRegistryRequirement {
	tracker := trackerFromContext(ctx)
	tracker.Lock()
	defer tracker.Unlock()

	key := moduleRegistryRequirementKey{
		addr:     addr,
		versions: versions.String(),
	}

	if _, exists := tracker.moduleRegistryRequirements[key]; !exists {
		tracker.moduleRegistryRequirements[key] = &moduleRegistryRequirement{
			addr: addr,
		}
	}
	return tracker.moduleRegistryRequirements[key]
}

// RemoteSourceAddrChecked returns the remote source address corresponding to
// this module package, or diagnostic errors describing why that isn't possible.
func (m *moduleRegistryRequirement) RemoteSourceAddrChecked(ctx context.Context) (addrs.ModuleSourceRemote, tfdiags.Diagnostics) {
	result, diags := m.result(ctx)
	if diags.HasErrors() {
		var zero addrs.ModuleSourceRemote
		return zero, diags
	}
	return result.remoteSource, diags
}

// RemoteSourceAddr is like [moduleRegistryPackage.RemoteSourceAddrChecked] but
// returns an unknown result if translation to a remote source address isn't
// possible.
//
// Use this for intermediate results, on the assumption that the diagnostics
// from the "checked" variant will be returned by a different return path.
func (m *moduleRegistryRequirement) RemoteSourceAddr(ctx context.Context) maybe[addrs.ModuleSourceRemote] {
	ret, diags := m.RemoteSourceAddrChecked(ctx)
	if diags.HasErrors() {
		return unknown[addrs.ModuleSourceRemote]()
	}
	return known(ret)
}

// Selected version returns the selected version if selection was successful,
// or an unknown result if version selection failed.
//
// Error diagnostics are not available for this result, because the diagnostics
// would always exactly match those returned by
// [moduleRegistryPackage.RemoteSourceAddrChecked].
func (m *moduleRegistryRequirement) SelectedVersion(ctx context.Context) maybe[versions.Version] {
	// Intentionally ignoring diags because they should be returned via another return path.
	result, _ := m.result(ctx)
	return mapMaybe(maybePtr(result), func(result *moduleRegistryResult) versions.Version {
		return result.selectedVersion
	})
}

func (m *moduleRegistryRequirement) result(ctx context.Context) (*moduleRegistryResult, tfdiags.Diagnostics) {
	return m.resultOnce.Do(ctx, func(ctx context.Context) (*moduleRegistryResult, tfdiags.Diagnostics) {
		var diags tfdiags.Diagnostics
		diags = diags.Append(fmt.Errorf("module registry resolution not yet implemented"))
		return nil, diags
	})
}

func (m *moduleRegistryRequirement) yieldRequestNames(yield func(workgraph.RequestID, string) bool) bool {
	return yield(m.resultOnce.RequestID(), m.addr.String())
}

type moduleRegistryResult struct {
	selectedVersion versions.Version
	remoteSource    addrs.ModuleSourceRemote
}
