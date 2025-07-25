// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2023 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package remote

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	tfe "github.com/hashicorp/go-tfe"
	version "github.com/hashicorp/go-version"
	"github.com/mitchellh/cli"
	"github.com/mitchellh/colorstring"
	"github.com/opentofu/svchost"
	"github.com/opentofu/svchost/disco"
	"github.com/opentofu/svchost/svcauth"
	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/backend"
	backendLocal "github.com/opentofu/opentofu/internal/backend/local"
	"github.com/opentofu/opentofu/internal/configs/configschema"
	"github.com/opentofu/opentofu/internal/encryption"
	"github.com/opentofu/opentofu/internal/httpclient"
	"github.com/opentofu/opentofu/internal/logging"
	"github.com/opentofu/opentofu/internal/states/remote"
	"github.com/opentofu/opentofu/internal/states/statemgr"
	"github.com/opentofu/opentofu/internal/tfdiags"
	"github.com/opentofu/opentofu/internal/tofu"
	tfversion "github.com/opentofu/opentofu/version"
)

const (
	defaultParallelism = 10
	stateServiceID     = "state.v2"
	tfeServiceID       = "tfe.v2.1"
	genericHostname    = "localtofu.com"
)

// Remote is an implementation of EnhancedBackend that performs all
// operations in a remote backend.
type Remote struct {
	// CLI and Colorize control the CLI output. If CLI is nil then no CLI
	// output will be done. If CLIColor is nil then no coloring will be done.
	CLI      cli.Ui
	CLIColor *colorstring.Colorize

	// ContextOpts are the base context options to set when initializing a
	// new OpenTofu context. Many of these will be overridden or merged by
	// Operation. See Operation for more details.
	ContextOpts *tofu.ContextOpts

	// client is the remote backend API client.
	client *tfe.Client

	// lastRetry is set to the last time a request was retried.
	lastRetry time.Time

	// hostname of the remote backend server.
	hostname string

	// organization is the organization that contains the target workspaces.
	organization string

	// workspace is used to map the default workspace to a remote workspace.
	workspace string

	// prefix is used to filter down a set of workspaces that use a single
	// configuration.
	prefix string

	// services is used for service discovery
	services *disco.Disco

	// local, if non-nil, will be used for all enhanced behavior. This
	// allows local behavior with the remote backend functioning as remote
	// state storage backend.
	local backend.Enhanced

	// forceLocal, if true, will force the use of the local backend.
	forceLocal bool

	// opLock locks operations
	opLock sync.Mutex

	// ignoreVersionConflict, if true, will disable the requirement that the
	// local OpenTofu version matches the remote workspace's configured
	// version. This will also cause VerifyWorkspaceTerraformVersion to return
	// a warning diagnostic instead of an error.
	ignoreVersionConflict bool

	encryption encryption.StateEncryption
}

var _ backend.Backend = (*Remote)(nil)
var _ backend.Enhanced = (*Remote)(nil)
var _ backend.Local = (*Remote)(nil)

// New creates a new initialized remote backend.
func New(services *disco.Disco, enc encryption.StateEncryption) *Remote {
	return &Remote{
		services:   services,
		encryption: enc,
	}
}

// ConfigSchema implements backend.Enhanced.
func (b *Remote) ConfigSchema() *configschema.Block {
	return &configschema.Block{
		Attributes: map[string]*configschema.Attribute{
			"hostname": {
				Type:        cty.String,
				Optional:    true,
				Description: schemaDescriptions["hostname"],
			},
			"organization": {
				Type:        cty.String,
				Required:    true,
				Description: schemaDescriptions["organization"],
			},
			"token": {
				Type:        cty.String,
				Optional:    true,
				Description: schemaDescriptions["token"],
			},
		},

		BlockTypes: map[string]*configschema.NestedBlock{
			"workspaces": {
				Block: configschema.Block{
					Attributes: map[string]*configschema.Attribute{
						"name": {
							Type:        cty.String,
							Optional:    true,
							Description: schemaDescriptions["name"],
						},
						"prefix": {
							Type:        cty.String,
							Optional:    true,
							Description: schemaDescriptions["prefix"],
						},
					},
				},
				Nesting: configschema.NestingSingle,
			},
		},
	}
}

// PrepareConfig implements backend.Backend.
func (b *Remote) PrepareConfig(obj cty.Value) (cty.Value, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics
	if obj.IsNull() {
		return obj, diags
	}

	if val := obj.GetAttr("organization"); val.IsNull() || val.AsString() == "" {
		diags = diags.Append(tfdiags.AttributeValue(
			tfdiags.Error,
			"Invalid organization value",
			`The "organization" attribute value must not be empty.`,
			cty.Path{cty.GetAttrStep{Name: "organization"}},
		))
	}

	var name, prefix string
	if workspaces := obj.GetAttr("workspaces"); !workspaces.IsNull() {
		if val := workspaces.GetAttr("name"); !val.IsNull() {
			name = val.AsString()
		}
		if val := workspaces.GetAttr("prefix"); !val.IsNull() {
			prefix = val.AsString()
		}
	}

	// Make sure that we have either a workspace name or a prefix.
	if name == "" && prefix == "" {
		diags = diags.Append(tfdiags.AttributeValue(
			tfdiags.Error,
			"Invalid workspaces configuration",
			`Either workspace "name" or "prefix" is required.`,
			cty.Path{cty.GetAttrStep{Name: "workspaces"}},
		))
	}

	// Make sure that only one of workspace name or a prefix is configured.
	if name != "" && prefix != "" {
		diags = diags.Append(tfdiags.AttributeValue(
			tfdiags.Error,
			"Invalid workspaces configuration",
			`Only one of workspace "name" or "prefix" is allowed.`,
			cty.Path{cty.GetAttrStep{Name: "workspaces"}},
		))
	}

	return obj, diags
}

func (b *Remote) ServiceDiscoveryAliases() ([]backend.HostAlias, error) {
	aliasHostname, err := svchost.ForComparison(genericHostname)
	if err != nil {
		// This should never happen because the hostname is statically defined.
		return nil, fmt.Errorf("failed to create backend alias from alias %q. The hostname is not in the correct format. This is a bug in the backend", genericHostname)
	}

	targetHostname, err := svchost.ForComparison(b.hostname)
	if err != nil {
		// This should never happen because the 'to' alias is the backend host, which has likely
		// already been evaluated as a svchost.Hostname by now
		return nil, fmt.Errorf("failed to create backend alias to target %q. The hostname is not in the correct format", b.hostname)
	}

	return []backend.HostAlias{
		{
			From: aliasHostname,
			To:   targetHostname,
		},
	}, nil
}

// Configure implements backend.Enhanced.
func (b *Remote) Configure(ctx context.Context, obj cty.Value) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics
	if obj.IsNull() {
		return diags
	}

	// Get the hostname.
	if val := obj.GetAttr("hostname"); !val.IsNull() && val.AsString() != "" {
		b.hostname = val.AsString()
	} else {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Hostname is required for the remote backend",
			`OpenTofu does not provide a default "hostname" attribute, so it must be set to the hostname of the remote backend.`,
		))

		return diags
	}

	// Get the organization.
	if val := obj.GetAttr("organization"); !val.IsNull() {
		b.organization = val.AsString()
	}

	// Get the workspaces configuration block and retrieve the
	// default workspace name and prefix.
	if workspaces := obj.GetAttr("workspaces"); !workspaces.IsNull() {
		if val := workspaces.GetAttr("name"); !val.IsNull() {
			b.workspace = val.AsString()
		}
		if val := workspaces.GetAttr("prefix"); !val.IsNull() {
			b.prefix = val.AsString()
		}
	}

	// Determine if we are forced to use the local backend.
	b.forceLocal = os.Getenv("TF_FORCE_LOCAL_BACKEND") != ""

	serviceID := tfeServiceID
	if b.forceLocal {
		serviceID = stateServiceID
	}

	// Discover the service URL for this host to confirm that it provides
	// a remote backend API and to get the version constraints.
	service, err := b.discover(serviceID)

	// Historical note: in OpenTofu's predecessor project there was an
	// extra step here of checking some metadata returned by the remote
	// API describing which versions of the predecessor's CLI it considers
	// itself to be compatible with. Since OpenTofu's version numbers have
	// little relationship with those of its predecessor, and since this
	// API is intended for interacting with the commercial service offered
	// by the predecessor's vendor (so highly unlikely to be set with
	// OpenTofu's releases in mind) we just skip that here and let the
	// subsequent requests fail if the remote API isn't compatible with
	// the current implementation.

	// When we don't have any constraints errors, also check for discovery
	// errors before we continue.
	if err != nil {
		diags = diags.Append(tfdiags.AttributeValue(
			tfdiags.Error,
			strings.ToUpper(err.Error()[:1])+err.Error()[1:],
			"", // no description is needed here, the error is clear
			cty.Path{cty.GetAttrStep{Name: "hostname"}},
		))
		return diags
	}

	// Get the token from the config.
	var token string
	if val := obj.GetAttr("token"); !val.IsNull() {
		token = val.AsString()
	}

	// Retrieve the token for this host as configured in the credentials
	// section of the CLI Config File if no token was configured for this
	// host in the config.
	if token == "" {
		token, err = b.token()
		if err != nil {
			diags = diags.Append(tfdiags.AttributeValue(
				tfdiags.Error,
				strings.ToUpper(err.Error()[:1])+err.Error()[1:],
				"", // no description is needed here, the error is clear
				cty.Path{cty.GetAttrStep{Name: "hostname"}},
			))
			return diags
		}
	}

	// Return an error if we still don't have a token at this point.
	if token == "" {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Required token could not be found",
			fmt.Sprintf(
				"Run the following command to generate a token for %s:\n    %s",
				b.hostname,
				fmt.Sprintf("tofu login %s", b.hostname),
			),
		))
		return diags
	}

	cfg := &tfe.Config{
		Address:      service.String(),
		BasePath:     service.Path,
		Token:        token,
		Headers:      make(http.Header),
		RetryLogHook: b.retryLogHook,
	}

	// Set the version header to the current version.
	cfg.Headers.Set(tfversion.Header, tfversion.Version)

	// Update user-agent from 'go-tfe' to opentofu
	cfg.Headers.Set("User-Agent", httpclient.OpenTofuUserAgent(tfversion.String()))

	// Create the remote backend API client.
	b.client, err = tfe.NewClient(cfg)
	if err != nil {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Failed to create the remote backend client",
			fmt.Sprintf(
				`The "remote" backend encountered an unexpected error while creating the `+
					`remote backend client: %s.`, err,
			),
		))
		return diags
	}

	// Check if the organization exists by reading its entitlements.
	entitlements, err := b.client.Organizations.ReadEntitlements(context.Background(), b.organization)
	if err != nil {
		if err == tfe.ErrResourceNotFound {
			err = fmt.Errorf("organization %q at host %s not found.\n\n"+
				"Please ensure that the organization and hostname are correct "+
				"and that your API token for %s is valid.",
				b.organization, b.hostname, b.hostname)
		}
		diags = diags.Append(tfdiags.AttributeValue(
			tfdiags.Error,
			fmt.Sprintf("Failed to read organization %q at host %s", b.organization, b.hostname),
			fmt.Sprintf("The \"remote\" backend encountered an unexpected error while reading the "+
				"organization settings: %s", err),
			cty.Path{cty.GetAttrStep{Name: "organization"}},
		))
		return diags
	}

	// Configure a local backend for when we need to run operations locally.
	b.local = backendLocal.NewWithBackend(b, b.encryption)
	b.forceLocal = b.forceLocal || !entitlements.Operations

	// Enable retries for server errors as the backend is now fully configured.
	b.client.RetryServerErrors(true)

	return diags
}

// discover the remote backend API service URL and version constraints.
func (b *Remote) discover(serviceID string) (*url.URL, error) {
	hostname, err := svchost.ForComparison(b.hostname)
	if err != nil {
		return nil, err
	}

	host, err := b.services.Discover(context.TODO(), hostname)
	if err != nil {
		return nil, err
	}

	service, err := host.ServiceURL(serviceID)
	// Return the error, unless its a disco.ErrVersionNotSupported error.
	if _, ok := err.(*disco.ErrVersionNotSupported); !ok && err != nil {
		return nil, err
	}

	return service, nil
}

// token returns the token for this host as configured in the credentials
// section of the CLI Config File. If no token was configured, an empty
// string will be returned instead.
func (b *Remote) token() (string, error) {
	hostname, err := svchost.ForComparison(b.hostname)
	if err != nil {
		return "", err
	}
	creds, err := b.services.CredentialsForHost(context.TODO(), hostname)
	if err != nil {
		log.Printf("[WARN] Failed to get credentials for %s: %s (ignoring)", b.hostname, err)
		return "", nil
	}

	// HostCredentialsWithToken is a variant of [svcauth.HostCredentials]
	// that also offers direct access to a stored token. This is a weird
	// need that applies only to this legacy cloud backend since it uses
	// a client library for a particular vendor's API that isn't designed
	// to integrate with svcauth. This is a surgical patch to keep this
	// working similarly to how it did in our predecessor project until
	// we decide on a more definite future for this backend.
	type HostCredentialsWithToken interface {
		svcauth.HostCredentials
		Token() string
	}
	if creds, ok := creds.(HostCredentialsWithToken); ok {
		return creds.Token(), nil
	}
	return "", nil
}

// retryLogHook is invoked each time a request is retried allowing the
// backend to log any connection issues to prevent data loss.
func (b *Remote) retryLogHook(attemptNum int, resp *http.Response) {
	if b.CLI != nil {
		// Ignore the first retry to make sure any delayed output will
		// be written to the console before we start logging retries.
		//
		// The retry logic in the TFE client will retry both rate limited
		// requests and server errors, but in the remote backend we only
		// care about server errors so we ignore rate limit (429) errors.
		if attemptNum == 0 || (resp != nil && resp.StatusCode == 429) {
			// Reset the last retry time.
			b.lastRetry = time.Now()
			return
		}

		if attemptNum == 1 {
			b.CLI.Output(b.Colorize().Color(strings.TrimSpace(initialRetryError)))
		} else {
			b.CLI.Output(b.Colorize().Color(strings.TrimSpace(
				fmt.Sprintf(repeatedRetryError, time.Since(b.lastRetry).Round(time.Second)))))
		}
	}
}

// Workspaces implements backend.Enhanced.
func (b *Remote) Workspaces(context.Context) ([]string, error) {
	if b.prefix == "" {
		return nil, backend.ErrWorkspacesNotSupported
	}
	return b.workspaces()
}

// workspaces returns a filtered list of remote workspace names.
func (b *Remote) workspaces() ([]string, error) {
	options := &tfe.WorkspaceListOptions{}
	switch {
	case b.workspace != "":
		options.Search = b.workspace
	case b.prefix != "":
		options.Search = b.prefix
	}

	// Create a slice to contain all the names.
	var names []string

	for {
		wl, err := b.client.Workspaces.List(context.Background(), b.organization, options)
		if err != nil {
			return nil, err
		}

		for _, w := range wl.Items {
			if b.workspace != "" && w.Name == b.workspace {
				names = append(names, backend.DefaultStateName)
				continue
			}
			if b.prefix != "" && strings.HasPrefix(w.Name, b.prefix) {
				names = append(names, strings.TrimPrefix(w.Name, b.prefix))
			}
		}

		// Exit the loop when we've seen all pages.
		if wl.CurrentPage >= wl.TotalPages {
			break
		}

		// Update the page number to get the next page.
		options.PageNumber = wl.NextPage
	}

	// Sort the result so we have consistent output.
	sort.StringSlice(names).Sort()

	return names, nil
}

// WorkspaceNamePattern provides an appropriate workspace renaming pattern for backend migration
// purposes (handled outside of this package), based on previous usage of this backend with the
// 'prefix' workspace functionality. As of this writing, see meta_backend.migrate.go
func (b *Remote) WorkspaceNamePattern() string {
	if b.prefix != "" {
		return b.prefix + "*"
	}

	return ""
}

// DeleteWorkspace implements backend.Enhanced.
func (b *Remote) DeleteWorkspace(_ context.Context, name string, _ bool) error {
	if b.workspace == "" && name == backend.DefaultStateName {
		return backend.ErrDefaultWorkspaceNotSupported
	}
	if b.prefix == "" && name != backend.DefaultStateName {
		return backend.ErrWorkspacesNotSupported
	}

	// Configure the remote workspace name.
	switch {
	case name == backend.DefaultStateName:
		name = b.workspace
	case b.prefix != "" && !strings.HasPrefix(name, b.prefix):
		name = b.prefix + name
	}

	client := &remoteClient{
		client:       b.client,
		organization: b.organization,
		workspace: &tfe.Workspace{
			Name: name,
		},
		encryption: b.encryption,
	}

	return client.Delete(context.TODO())
}

// StateMgr implements backend.Enhanced.
func (b *Remote) StateMgr(ctx context.Context, name string) (statemgr.Full, error) {
	if b.workspace == "" && name == backend.DefaultStateName {
		return nil, backend.ErrDefaultWorkspaceNotSupported
	}
	if b.prefix == "" && name != backend.DefaultStateName {
		return nil, backend.ErrWorkspacesNotSupported
	}

	// Configure the remote workspace name.
	switch {
	case name == backend.DefaultStateName:
		name = b.workspace
	case b.prefix != "" && !strings.HasPrefix(name, b.prefix):
		name = b.prefix + name
	}

	workspace, err := b.client.Workspaces.Read(ctx, b.organization, name)
	if err != nil && err != tfe.ErrResourceNotFound {
		return nil, fmt.Errorf("Failed to retrieve workspace %s: %w", name, err)
	}

	if err == tfe.ErrResourceNotFound {
		options := tfe.WorkspaceCreateOptions{
			Name: tfe.String(name),
		}

		// We only set the OpenTofu Version for the new workspace if this is
		// a release candidate or a final release.
		if tfversion.Prerelease == "" || strings.HasPrefix(tfversion.Prerelease, "rc") {
			options.TerraformVersion = tfe.String(tfversion.String())
		}

		workspace, err = b.client.Workspaces.Create(ctx, b.organization, options)
		if err != nil {
			return nil, fmt.Errorf("Error creating workspace %s: %w", name, err)
		}
	}

	// This is a fallback error check. Most code paths should use other
	// mechanisms to check the version, then set the ignoreVersionConflict
	// field to true. This check is only in place to ensure that we don't
	// accidentally upgrade state with a new code path, and the version check
	// logic is coarser and simpler.
	if !b.ignoreVersionConflict {
		wsv := workspace.TerraformVersion
		// Explicitly ignore the pseudo-version "latest" here, as it will cause
		// plan and apply to always fail.
		if wsv != tfversion.String() && wsv != "latest" {
			return nil, fmt.Errorf("Remote workspace OpenTofu version %q does not match local OpenTofu version %q", workspace.TerraformVersion, tfversion.String())
		}
	}

	client := &remoteClient{
		client:       b.client,
		organization: b.organization,
		workspace:    workspace,

		// This is optionally set during OpenTofu Enterprise runs.
		runID: os.Getenv("TFE_RUN_ID"),

		encryption: b.encryption,
	}

	state := remote.NewState(client, b.encryption)
	if client.runID != "" {
		// client.runID will be set if we're running a Terraform Cloud
		// or Terraform Enterprise remote execution environment, in which
		// case we'll disable intermediate snapshots to avoid extra storage
		// costs for Terraform Enterprise customers.
		// Other implementations of the remote state protocol should not run
		// in contexts where there's a "TFE Run ID" and so are not affected
		// by this special case.
		state.DisableIntermediateSnapshots()
	}
	return state, nil
}

func isLocalExecutionMode(execMode string) bool {
	return execMode == "local"
}

func (b *Remote) fetchWorkspace(ctx context.Context, organization string, name string) (*tfe.Workspace, error) {
	remoteWorkspaceName := b.getRemoteWorkspaceName(name)
	// Retrieve the workspace for this operation.
	w, err := b.client.Workspaces.Read(ctx, b.organization, remoteWorkspaceName)
	if err != nil {
		switch err {
		case context.Canceled:
			return nil, err
		case tfe.ErrResourceNotFound:
			return nil, fmt.Errorf(
				"workspace %s not found\n\n"+
					"The configured \"remote\" backend returns '404 Not Found' errors for resources\n"+
					"that do not exist, as well as for resources that a user doesn't have access\n"+
					"to. If the resource does exist, please check the rights for the used token",
				name,
			)
		default:
			err := fmt.Errorf(
				"the configured \"remote\" backend encountered an unexpected error:\n\n%w",
				err,
			)
			return nil, err
		}
	}

	return w, nil
}

// Operation implements backend.Enhanced.
func (b *Remote) Operation(ctx context.Context, op *backend.Operation) (*backend.RunningOperation, error) {
	w, err := b.fetchWorkspace(ctx, b.organization, op.Workspace)

	if err != nil {
		return nil, err
	}

	// OpenTofu remote version conflicts are not a concern for operations. We
	// are in one of three states:
	//
	// - Running remotely, in which case the local version is irrelevant;
	// - Workspace configured for local operations, in which case the remote
	//   version is meaningless;
	// - Forcing local operations with a remote backend, which should only
	//   happen in the Terraform Cloud worker, in which case the OpenTofu
	//   versions by definition match.
	b.IgnoreVersionConflict()

	// Check if we need to use the local backend to run the operation.
	if b.forceLocal || isLocalExecutionMode(w.ExecutionMode) {
		// Record that we're forced to run operations locally to allow the
		// command package UI to operate correctly
		b.forceLocal = true
		log.Printf("[DEBUG] Remote backend is delegating %s to the local backend", op.Type)
		return b.local.Operation(ctx, op)
	}

	// Set the remote workspace name.
	op.Workspace = w.Name

	// Determine the function to call for our operation
	var f func(context.Context, context.Context, context.Context, *backend.Operation, *tfe.Workspace) (*tfe.Run, error)
	switch op.Type {
	case backend.OperationTypePlan:
		f = b.opPlan
	case backend.OperationTypeApply:
		f = b.opApply
	case backend.OperationTypeRefresh:
		return nil, fmt.Errorf(
			"\n\nThe \"refresh\" operation is not supported when using the \"remote\" backend. " +
				"Use \"tofu apply -refresh-only\" instead.")
	default:
		return nil, fmt.Errorf(
			"\n\nThe \"remote\" backend does not support the %q operation.", op.Type)
	}

	// Lock
	b.opLock.Lock()

	// Build our running operation
	// the runningCtx is only used to block until the operation returns.
	runningCtx, done := context.WithCancel(context.Background())
	runningOp := &backend.RunningOperation{
		Context:   runningCtx,
		PlanEmpty: true,
	}

	// stopCtx wraps the context passed in, and is used to signal a graceful Stop.
	stopCtx, stop := context.WithCancel(ctx)
	runningOp.Stop = stop

	// cancelCtx is used to cancel the operation immediately, usually
	// indicating that the process is exiting.
	cancelCtx, cancel := context.WithCancel(context.Background())
	runningOp.Cancel = cancel

	panicHandler := logging.PanicHandlerWithTraceFn()

	// Do it.
	go func() {
		defer panicHandler()
		defer done()
		defer stop()
		defer cancel()

		defer b.opLock.Unlock()

		r, opErr := f(ctx, stopCtx, cancelCtx, op, w)
		if opErr != nil && opErr != context.Canceled {
			var diags tfdiags.Diagnostics
			diags = diags.Append(opErr)
			op.ReportResult(runningOp, diags)
			return
		}

		if r == nil && opErr == context.Canceled {
			runningOp.Result = backend.OperationFailure
			return
		}

		if r != nil {
			// Retrieve the run to get its current status.
			r, err := b.client.Runs.Read(cancelCtx, r.ID)
			if err != nil {
				var diags tfdiags.Diagnostics
				diags = diags.Append(generalError("Failed to retrieve run", err))
				op.ReportResult(runningOp, diags)
				return
			}

			// Record if there are any changes.
			runningOp.PlanEmpty = !r.HasChanges

			if opErr == context.Canceled {
				if err := b.cancel(cancelCtx, op, r); err != nil {
					var diags tfdiags.Diagnostics
					diags = diags.Append(generalError("Failed to retrieve run", err))
					op.ReportResult(runningOp, diags)
					return
				}
			}

			if r.Status == tfe.RunCanceled || r.Status == tfe.RunErrored {
				runningOp.Result = backend.OperationFailure
			}
		}
	}()

	// Return the running operation.
	return runningOp, nil
}

func (b *Remote) cancel(cancelCtx context.Context, op *backend.Operation, r *tfe.Run) error {
	if r.Actions.IsCancelable {
		// Only ask if the remote operation should be canceled
		// if the auto approve flag is not set.
		if !op.AutoApprove {
			v, err := op.UIIn.Input(cancelCtx, &tofu.InputOpts{
				Id:          "cancel",
				Query:       "\nDo you want to cancel the remote operation?",
				Description: "Only 'yes' will be accepted to cancel.",
			})
			if err != nil {
				return generalError("Failed asking to cancel", err)
			}
			if v != "yes" {
				if b.CLI != nil {
					b.CLI.Output(b.Colorize().Color(strings.TrimSpace(operationNotCanceled)))
				}
				return nil
			}
		} else {
			if b.CLI != nil {
				// Insert a blank line to separate the outputs.
				b.CLI.Output("")
			}
		}

		// Try to cancel the remote operation.
		err := b.client.Runs.Cancel(cancelCtx, r.ID, tfe.RunCancelOptions{})
		if err != nil {
			return generalError("Failed to cancel run", err)
		}
		if b.CLI != nil {
			b.CLI.Output(b.Colorize().Color(strings.TrimSpace(operationCanceled)))
		}
	}

	return nil
}

// IgnoreVersionConflict allows commands to disable the fall-back check that
// the local OpenTofu version matches the remote workspace's configured
// OpenTofu version. This should be called by commands where this check is
// unnecessary, such as those performing remote operations, or read-only
// operations. It will also be called if the user uses a command-line flag to
// override this check.
func (b *Remote) IgnoreVersionConflict() {
	b.ignoreVersionConflict = true
}

// VerifyWorkspaceTerraformVersion compares the local OpenTofu version against
// the workspace's configured OpenTofu version. If they are equal, this means
// that there are no compatibility concerns, so it returns no diagnostics.
//
// If the versions differ,
func (b *Remote) VerifyWorkspaceTerraformVersion(workspaceName string) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics

	workspace, err := b.getRemoteWorkspace(context.Background(), workspaceName)
	if err != nil {
		// If the workspace doesn't exist, there can be no compatibility
		// problem, so we can return. This is most likely to happen when
		// migrating state from a local backend to a new workspace.
		if err == tfe.ErrResourceNotFound {
			return nil
		}

		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Error looking up workspace",
			fmt.Sprintf("Workspace read failed: %s", err),
		))
		return diags
	}

	// If the workspace has the pseudo-version "latest", all bets are off. We
	// cannot reasonably determine what the intended OpenTofu version is, so
	// we'll skip version verification.
	if workspace.TerraformVersion == "latest" {
		return nil
	}

	// If the workspace has remote operations disabled, the remote OpenTofu
	// version is effectively meaningless, so we'll skip version verification.
	if isLocalExecutionMode(workspace.ExecutionMode) {
		return nil
	}

	remoteVersion, err := version.NewSemver(workspace.TerraformVersion)
	if err != nil {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Error looking up workspace",
			fmt.Sprintf("Invalid OpenTofu version: %s", err),
		))
		return diags
	}

	v014 := version.Must(version.NewSemver("0.14.0"))
	if tfversion.SemVer.LessThan(v014) || remoteVersion.LessThan(v014) {
		// Versions of OpenTofu prior to 0.14.0 will refuse to load state files
		// written by a newer version of OpenTofu, even if it is only a patch
		// level difference. As a result we require an exact match.
		if tfversion.SemVer.Equal(remoteVersion) {
			return diags
		}
	}
	if tfversion.SemVer.GreaterThanOrEqual(v014) && remoteVersion.GreaterThanOrEqual(v014) {
		// Versions of OpenTofu after 0.14.0 should be compatible with each
		// other.  At the time this code was written, the only constraints we
		// are aware of are:
		//
		// - 0.14.0 is guaranteed to be compatible with versions up to but not
		//   including 1.3.0
		v130 := version.Must(version.NewSemver("1.3.0"))
		if tfversion.SemVer.LessThan(v130) && remoteVersion.LessThan(v130) {
			return diags
		}
		// - Any new OpenTofu state version will require at least minor patch
		//   increment, so x.y.* will always be compatible with each other
		tfvs := tfversion.SemVer.Segments64()
		rwvs := remoteVersion.Segments64()
		if len(tfvs) == 3 && len(rwvs) == 3 && tfvs[0] == rwvs[0] && tfvs[1] == rwvs[1] {
			return diags
		}
	}

	// Even if ignoring version conflicts, it may still be useful to call this
	// method and warn the user about a mismatch between the local and remote
	// OpenTofu versions.
	severity := tfdiags.Error
	if b.ignoreVersionConflict {
		severity = tfdiags.Warning
	}

	suggestion := " If you're sure you want to upgrade the state, you can force OpenTofu to continue using the -ignore-remote-version flag. This may result in an unusable workspace."
	if b.ignoreVersionConflict {
		suggestion = ""
	}
	diags = diags.Append(tfdiags.Sourceless(
		severity,
		"OpenTofu version mismatch",
		fmt.Sprintf(
			"The local OpenTofu version (%s) does not match the configured version for remote workspace %s/%s (%s).%s",
			tfversion.String(),
			b.organization,
			workspace.Name,
			workspace.TerraformVersion,
			suggestion,
		),
	))

	return diags
}

func (b *Remote) IsLocalOperations() bool {
	return b.forceLocal
}

func generalError(msg string, err error) error {
	var diags tfdiags.Diagnostics

	if urlErr, ok := err.(*url.Error); ok {
		err = urlErr.Err
	}

	switch err {
	case context.Canceled:
		return err
	case tfe.ErrResourceNotFound:
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			fmt.Sprintf("%s: %v", msg, err),
			`The configured "remote" backend returns '404 Not Found' errors for resources `+
				`that do not exist, as well as for resources that a user doesn't have access `+
				`to. If the resource does exist, please check the rights for the used token.`,
		))
		return diags.Err()
	default:
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			fmt.Sprintf("%s: %v", msg, err),
			`The configured "remote" backend encountered an unexpected error. Sometimes `+
				`this is caused by network connection problems, in which case you could retry `+
				`the command. If the issue persists please open a support ticket to get help `+
				`resolving the problem.`,
		))
		return diags.Err()
	}
}

// The newline in this error is to make it look good in the CLI!
const initialRetryError = `
[reset][yellow]There was an error connecting to the remote backend. Please do not exit
OpenTofu to prevent data loss! Trying to restore the connection...
[reset]
`

const repeatedRetryError = `
[reset][yellow]Still trying to restore the connection... (%s elapsed)[reset]
`

const operationCanceled = `
[reset][red]The remote operation was successfully cancelled.[reset]
`

const operationNotCanceled = `
[reset][red]The remote operation was not cancelled.[reset]
`

var schemaDescriptions = map[string]string{
	"hostname":     "The remote backend hostname to connect to.",
	"organization": "The name of the organization containing the targeted workspace(s).",
	"token": "The token used to authenticate with the remote backend. If credentials for the\n" +
		"host are configured in the CLI Config File, then those will be used instead.",
	"name": "A workspace name used to map the default workspace to a named remote workspace.\n" +
		"When configured only the default workspace can be used. This option conflicts\n" +
		"with \"prefix\"",
	"prefix": "A prefix used to filter workspaces using a single configuration. New workspaces\n" +
		"will automatically be prefixed with this prefix. If omitted only the default\n" +
		"workspace can be used. This option conflicts with \"name\"",
}
