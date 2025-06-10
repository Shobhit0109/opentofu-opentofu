package installdeps

import (
	"iter"

	"github.com/hashicorp/go-version"
	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

type moduleCall struct {
	Name            string
	SourceAddr      addrs.ModuleSource
	SourceAddrRange tfdiags.SourceRange
	Versions        version.Constraints
	VersionsRange   tfdiags.SourceRange
}

// moduleCallsForModule returns an iterable sequence of all of the
// module sources that the given module requires.
func moduleCallsForModule(module *configs.Module) iter.Seq[moduleCall] {
	return func(yield func(moduleCall) bool) {
		for _, mc := range module.ModuleCalls {
			dep := moduleCall{
				Name:            mc.Name,
				SourceAddr:      mc.SourceAddr,
				SourceAddrRange: tfdiags.SourceRangeFromHCL(mc.Source.Range()),
				Versions:        mc.Version.Required,
				VersionsRange:   tfdiags.SourceRangeFromHCL(mc.Version.DeclRange),
			}
			if !yield(dep) {
				return
			}
		}
	}
}
