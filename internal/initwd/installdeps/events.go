package installdeps

import (
	"context"

	"github.com/apparentlymart/go-versions/versions"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

type InstallEvents struct {
	ModuleDependenciesStart    func(ctx context.Context, addr addrs.Module) context.Context
	ModuleDependenciesComplete func(ctx context.Context, addr addrs.Module, diags tfdiags.Diagnostics)

	RegistryModuleResolveStart   func(ctx context.Context, addr addrs.Module, sourceAddr addrs.ModuleSourceRegistry) context.Context
	RegistryModuleResolveSuccess func(ctx context.Context, addr addrs.Module, sourceAddr addrs.ModuleSourceRegistry, version versions.Version)
	RegistryModuleResolveFailed  func(ctx context.Context, addr addrs.Module)
}

func contextWithEvents(base context.Context, events *InstallEvents) context.Context {
	return context.WithValue(base, eventsContextKey, events)
}
