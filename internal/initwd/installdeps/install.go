package installdeps

import (
	"context"
	"fmt"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/getmodules"
	"github.com/opentofu/opentofu/internal/getproviders"
	"github.com/opentofu/opentofu/internal/providercache"
	"github.com/opentofu/opentofu/internal/registry"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

type Installer struct {
	globalProviderCache  *providercache.Dir
	providerSource       getproviders.Source
	sourcePkgFetcher     *getmodules.PackageFetcher
	moduleRegistryClient *registry.Client
}

// InstallDependencies discovers and installs all of the dependencies required
// by the configuration starting at the given root module into the given
// destination directory.
//
// This is really just a prototype of a workgraph-based installer to get some
// experience using the workgraph API. What it produces cannot actually
// be used by today's OpenTofu to do any real work. Maybe we'll conclude that
// it's worth reworking the existing module and provider installers in this
// way for real someday, but that is not a goal for this implementation.
func (i *Installer) InstallDependencies(ctx context.Context, rootModuleEarly *configs.Module, destDir string, events *InstallEvents) tfdiags.Diagnostics {
	ctx = contextWithEvents(ctx, events)
	ctx = contextWithNewTracker(ctx, i, destDir)
	return installModuleDependencies(ctx, addrs.RootModule, rootModuleEarly)
}

func installModuleDependencies(ctx context.Context, addr addrs.Module, module *configs.Module) tfdiags.Diagnostics {
	var wg workGroup

	evts := eventsFromContext(ctx)
	if evts.ModuleDependenciesStart != nil {
		ctx = evts.ModuleDependenciesStart(ctx, addr)
	}

	for call := range moduleCallsForModule(module) {
		wg.Run(ctx, func(ctx context.Context) tfdiags.Diagnostics {
			return installModuleForCall(ctx, addr, call)
		})

	}

	diags := wg.Complete(ctx)
	if evts.ModuleDependenciesComplete != nil {
		evts.ModuleDependenciesComplete(ctx, addr, diags)
	}
	return diags
}

func installModuleForCall(ctx context.Context, callerAddr addrs.Module, call moduleCall) tfdiags.Diagnostics {
	evts := eventsFromContext(ctx)

	calleeAddr := callerAddr.Child(call.Name)

	switch sourceAddr := call.SourceAddr.(type) {
	case addrs.ModuleSourceLocal:
		panic("local source addresses not implemented yet")

	case addrs.ModuleSourceRemote:
		panic("remote source addresses not implemented yet")

	case addrs.ModuleSourceRegistry:
		req := getModuleRegistryRequirement(ctx, sourceAddr.Package, call.Versions)
		ctx := ctx // local context just for this branch
		if evts.RegistryModuleResolveStart != nil {
			ctx = evts.RegistryModuleResolveStart(ctx, callerAddr.Child())
		}

	default:
		panic(fmt.Sprintf("unhandled source address type %T", sourceAddr))
	}

}
