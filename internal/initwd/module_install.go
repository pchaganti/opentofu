// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2023 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package initwd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/apparentlymart/go-versions/versions"
	version "github.com/hashicorp/go-version"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	otelAttr "go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/configs/configload"
	"github.com/opentofu/opentofu/internal/getmodules"
	"github.com/opentofu/opentofu/internal/modsdir"
	"github.com/opentofu/opentofu/internal/registry"
	"github.com/opentofu/opentofu/internal/registry/regsrc"
	"github.com/opentofu/opentofu/internal/registry/response"
	"github.com/opentofu/opentofu/internal/tfdiags"
	"github.com/opentofu/opentofu/internal/tracing"
	"github.com/opentofu/opentofu/internal/tracing/traceattrs"
)

type ModuleInstaller struct {
	modsDir string
	loader  *configload.Loader
	reg     *registry.Client
	fetcher *getmodules.PackageFetcher

	// The keys in moduleVersions are resolved and trimmed registry source
	// addresses and the values are the registry response.
	registryPackageVersions map[addrs.ModuleRegistryPackage]*response.ModuleVersions

	// The keys in moduleVersionsUrl are the moduleVersion struct below and
	// addresses and the values are underlying remote source addresses.
	registryPackageSources map[moduleVersion]addrs.ModuleSourceRemote
}

type moduleVersion struct {
	module  addrs.ModuleRegistryPackage
	version string
}

// NewModuleInstaller constructs a new [ModuleInstaller] object whose methods
// will make use of the given dependencies.
//
// "loader" is the configuration loader to use to traverse the module tree
// of the configuration whose modules are being installed.
//
// "registryClient" is the client for the OpenTofu module registry protocol,
// used to fetch package metadata when installing remote modules indirectly
// through a registry-style module source address. This may be nil only if
// "remotePackageFetcher" is also nil, since registry source addresses are
// only resolvable when remote module packages are available.
//
// "remotePackageFetcher" is the client used for fetching actual module packages
// from concrete physical source locations, which can be either specified
// directly in the configuration or returned dynamically as part of the metadata
// fetched from an OpenTofu module registry. This argument can be nil, in which
// case no remote package sources are supported; this facility is included
// primarily for unit testing where only local modules are needed.
func NewModuleInstaller(modsDir string, loader *configload.Loader, registryClient *registry.Client, remotePackageFetcher *getmodules.PackageFetcher) *ModuleInstaller {
	return &ModuleInstaller{
		modsDir:                 modsDir,
		loader:                  loader,
		reg:                     registryClient,
		fetcher:                 remotePackageFetcher,
		registryPackageVersions: make(map[addrs.ModuleRegistryPackage]*response.ModuleVersions),
		registryPackageSources:  make(map[moduleVersion]addrs.ModuleSourceRemote),
	}
}

// InstallModules analyses the root module in the given directory and installs
// all of its direct and transitive dependencies into the given modules
// directory, which must already exist.
//
// Since InstallModules makes possibly-time-consuming calls to remote services,
// a hook interface is supported to allow the caller to be notified when
// each module is installed and, for remote modules, when downloading begins.
// LoadConfig guarantees that two hook calls will not happen concurrently but
// it does not guarantee any particular ordering of hook calls. This mechanism
// is for UI feedback only and does not give the caller any control over the
// process.
//
// If modules are already installed in the target directory, they will be
// skipped unless their source address or version have changed or unless
// the upgrade flag is set.
//
// InstallModules never deletes any directory, except in the case where it
// needs to replace a directory that is already present with a newly-extracted
// package.
//
// installErrsOnly installs modules but converts validation errors from
// building the configuration after installation to warnings. This is used by
// commands like `get` or `init -from-module` where the established behavior
// was only to install the requested module, and extra validation can break
// compatibility.
//
// If the returned diagnostics contains errors then the module installation
// may have wholly or partially completed. Modules must be loaded in order
// to find their dependencies, so this function does many of the same checks
// as LoadConfig as a side-effect.
//
// If successful (the returned diagnostics contains no errors) then the
// first return value is the early configuration tree that was constructed by
// the installation process.
func (i *ModuleInstaller) InstallModules(ctx context.Context, rootDir, testsDir string, upgrade, installErrsOnly bool, hooks ModuleInstallHooks, call configs.StaticModuleCall) (*configs.Config, tfdiags.Diagnostics) {
	log.Printf("[TRACE] ModuleInstaller: installing child modules for %s into %s", rootDir, i.modsDir)
	var diags tfdiags.Diagnostics

	rootMod, mDiags := i.loader.Parser().LoadConfigDirWithTests(rootDir, testsDir, call)
	if rootMod == nil {
		// We drop the diagnostics here because we only want to report module
		// loading errors after checking the core version constraints, which we
		// can only do if the module can be at least partially loaded.
		return nil, diags
	} else if vDiags := rootMod.CheckCoreVersionRequirements(nil, nil); vDiags.HasErrors() {
		// If the core version requirements are not met, we drop any other
		// diagnostics, as they may reflect language changes from future
		// OpenTofu versions.
		diags = diags.Append(vDiags)
	} else {
		diags = diags.Append(mDiags)
	}

	manifest, err := modsdir.ReadManifestSnapshotForDir(i.modsDir)
	if err != nil {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Failed to read modules manifest file",
			fmt.Sprintf("Error reading manifest for %s: %s.", i.modsDir, err),
		))
		return nil, diags
	}

	fetcher := i.fetcher

	if hooks == nil {
		// Use our no-op implementation as a placeholder
		hooks = ModuleInstallHooksImpl{}
	}

	// Create a manifest record for the root module. This will be used if
	// there are any relative-pathed modules in the root.
	manifest[""] = modsdir.Record{
		Key: "",
		Dir: rootDir,
	}
	walker := i.moduleInstallWalker(ctx, manifest, upgrade, hooks, fetcher)

	cfg, instDiags := i.installDescendentModules(ctx, rootMod, manifest, walker, installErrsOnly)
	diags = append(diags, instDiags...)

	return cfg, diags
}

func (i *ModuleInstaller) moduleInstallWalker(_ context.Context, manifest modsdir.Manifest, upgrade bool, hooks ModuleInstallHooks, fetcher *getmodules.PackageFetcher) configs.ModuleWalker {
	return configs.ModuleWalkerFunc(
		func(ctx context.Context, req *configs.ModuleRequest) (*configs.Module, *version.Version, hcl.Diagnostics) {
			var diags hcl.Diagnostics

			if req.SourceAddr == nil {
				// If the parent module failed to parse the module source
				// address, we can't load it here. Return nothing as the parent
				// module's diagnostics should explain this.
				return nil, nil, diags
			}

			if req.Name == "" {
				// An empty string for a module instance name breaks our
				// manifest map, which uses that to indicate the root module.
				// Because we descend into modules which have errors, we need
				// to look out for this case, but the config loader's
				// diagnostics will report the error later.
				return nil, nil, diags
			}

			if !hclsyntax.ValidIdentifier(req.Name) {
				// A module with an invalid name shouldn't be installed at all. This is
				// mostly a concern for remote modules, since we need to be able to convert
				// the name to a valid path.
				return nil, nil, diags
			}

			key := manifest.ModuleKey(req.Path)
			instPath := i.packageInstallPath(req.Path)

			ctx, span := tracing.Tracer().Start(ctx,
				fmt.Sprintf("Install Module %q", req.Name),
				trace.WithAttributes(
					otelAttr.String(traceattrs.ModuleCallName, req.Name),
					otelAttr.String(traceattrs.ModuleSource, req.SourceAddr.String()),
				))
			defer span.End()

			log.Printf("[DEBUG] Module installer: begin %s", key)

			// First we'll check if we need to upgrade/replace an existing
			// installed module, and delete it out of the way if so.
			replace := upgrade
			if !replace {
				record, recorded := manifest[key]
				switch {
				case !recorded:
					log.Printf("[TRACE] ModuleInstaller: %s is not yet installed", key)
					span.AddEvent("Module not yet installed")
					replace = true
				case record.SourceAddr != req.SourceAddr.String():
					log.Printf("[TRACE] ModuleInstaller: %s source address has changed from %q to %q", key, record.SourceAddr, req.SourceAddr)
					span.AddEvent("Module source address changed")
					replace = true
				case record.Version != nil && !req.VersionConstraint.Required.Check(record.Version):
					log.Printf("[TRACE] ModuleInstaller: %s version %s no longer compatible with constraints %s", key, record.Version, req.VersionConstraint.Required)
					span.AddEvent("Module version constraint changed")
					replace = true
				}
			}

			// If we _are_ planning to replace this module, then we'll remove
			// it now so our installation code below won't conflict with any
			// existing remnants.
			if replace {
				if _, recorded := manifest[key]; recorded {
					log.Printf("[TRACE] ModuleInstaller: discarding previous record of %s prior to reinstall", key)
				}
				delete(manifest, key)
				// Deleting a module invalidates all of its descendent modules too.
				keyPrefix := key + "."
				for subKey := range manifest {
					if strings.HasPrefix(subKey, keyPrefix) {
						if _, recorded := manifest[subKey]; recorded {
							log.Printf("[TRACE] ModuleInstaller: also discarding downstream %s", subKey)
						}
						delete(manifest, subKey)
					}
				}
			}

			record, recorded := manifest[key]
			if !recorded {
				// Clean up any stale cache directory that might be present.
				// If this is a local (relative) source then the dir will
				// not exist, but we'll ignore that.
				log.Printf("[TRACE] ModuleInstaller: cleaning directory %s prior to install of %s", instPath, key)
				err := os.RemoveAll(instPath)
				if err != nil && !os.IsNotExist(err) {
					log.Printf("[TRACE] ModuleInstaller: failed to remove %s: %s", key, err)
					diags = diags.Append(&hcl.Diagnostic{
						Severity: hcl.DiagError,
						Summary:  "Failed to remove local module cache",
						Detail: fmt.Sprintf(
							"OpenTofu tried to remove %s in order to reinstall this module, but encountered an error: %s",
							instPath, err,
						),
					})
					return nil, nil, diags
				}
			} else {
				// If this module is already recorded and its root directory
				// exists then we will just load what's already there and
				// keep our existing record.
				info, err := os.Stat(record.Dir)
				if err == nil && info.IsDir() {
					mod, mDiags := i.loader.Parser().LoadConfigDir(record.Dir, req.Call)
					if mod == nil {
						// nil indicates an unreadable module, which should never happen,
						// so we return the full loader diagnostics here.
						diags = diags.Extend(mDiags)
					} else if vDiags := mod.CheckCoreVersionRequirements(req.Path, req.SourceAddr); vDiags.HasErrors() {
						// If the core version requirements are not met, we drop any other
						// diagnostics, as they may reflect language changes from future
						// OpenTofu versions.
						diags = diags.Extend(vDiags)
					} else {
						diags = diags.Extend(mDiags)
					}

					log.Printf("[TRACE] ModuleInstaller: Module installer: %s %s already installed in %s", key, record.Version, record.Dir)
					return mod, record.Version, diags
				}
			}

			// If we get down here then it's finally time to actually install
			// the module. There are some variants to this process depending
			// on what type of module source address we have.

			switch addr := req.SourceAddr.(type) {

			case addrs.ModuleSourceLocal:
				log.Printf("[TRACE] ModuleInstaller: %s has local path %q", key, addr.String())
				span.SetAttributes(otelAttr.String("opentofu.module.source_type", "local"))
				mod, mDiags := i.installLocalModule(ctx, req, key, manifest, hooks)
				mDiags = maybeImproveLocalInstallError(req, mDiags)
				diags = append(diags, mDiags...)
				return mod, nil, diags

			case addrs.ModuleSourceRegistry:
				log.Printf("[TRACE] ModuleInstaller: %s is a registry module at %s", key, addr.String())
				span.SetAttributes(otelAttr.String("opentofu.module.source_type", "registry"))
				mod, v, mDiags := i.installRegistryModule(ctx, req, key, instPath, addr, manifest, hooks, fetcher)
				diags = append(diags, mDiags...)
				return mod, v, diags

			case addrs.ModuleSourceRemote:
				log.Printf("[TRACE] ModuleInstaller: %s address %q will be handled by go-getter", key, addr.String())
				mod, mDiags := i.installGoGetterModule(ctx, req, key, instPath, manifest, hooks, fetcher)
				diags = append(diags, mDiags...)
				return mod, nil, diags

			default:
				// Shouldn't get here, because there are no other implementations
				// of addrs.ModuleSource.
				panic(fmt.Sprintf("unsupported module source address %#v", addr))
			}
		},
	)
}

func (i *ModuleInstaller) installDescendentModules(ctx context.Context, rootMod *configs.Module, manifest modsdir.Manifest, installWalker configs.ModuleWalker, installErrsOnly bool) (*configs.Config, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics

	// When attempting to initialize the current directory with a module
	// source, some use cases may want to ignore configuration errors from the
	// building of the entire configuration structure, but we still need to
	// capture installation errors. Because the actual module installation
	// happens in the ModuleWalkFunc callback while building the config, we
	// need to create a closure to capture the installation diagnostics
	// separately.
	var instDiags hcl.Diagnostics
	walker := installWalker
	if installErrsOnly {
		walker = configs.ModuleWalkerFunc(func(ctx context.Context, req *configs.ModuleRequest) (*configs.Module, *version.Version, hcl.Diagnostics) {
			mod, version, diags := installWalker.LoadModule(ctx, req)
			instDiags = instDiags.Extend(diags)
			return mod, version, diags
		})
	}

	cfg, cDiags := configs.BuildConfig(ctx, rootMod, walker)
	diags = diags.Append(cDiags)
	if installErrsOnly {
		// We can't continue if there was an error during installation, but
		// return all diagnostics in case there happens to be anything else
		// useful when debugging the problem. Any instDiags will be included in
		// diags already.
		if instDiags.HasErrors() {
			return cfg, diags
		}

		// If there are any errors here, they must be only from building the
		// config structures. We don't want to block initialization at this
		// point, so convert these into warnings. Any actual errors in the
		// configuration will be raised as soon as the config is loaded again.
		// We continue below because writing the manifest is required to finish
		// module installation.
		diags = tfdiags.OverrideAll(diags, tfdiags.Warning, nil)
	}

	err := manifest.WriteSnapshotToDir(i.modsDir)
	if err != nil {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Failed to update module manifest",
			fmt.Sprintf("Unable to write the module manifest file: %s", err),
		))
	}

	return cfg, diags
}

func (i *ModuleInstaller) installLocalModule(ctx context.Context, req *configs.ModuleRequest, key string, manifest modsdir.Manifest, hooks ModuleInstallHooks) (*configs.Module, hcl.Diagnostics) {
	var diags hcl.Diagnostics

	_, span := tracing.Tracer().Start(ctx, "Install Local Module",
		trace.WithAttributes(otelAttr.String(traceattrs.ModuleCallName, req.Name)),
		trace.WithAttributes(otelAttr.String(traceattrs.ModuleSource, req.SourceAddr.String())),
	)
	defer span.End()

	parentKey := manifest.ModuleKey(req.Parent.Path)
	parentRecord, recorded := manifest[parentKey]
	if !recorded {
		// This is indicative of a bug rather than a user-actionable error
		panic(fmt.Errorf("missing manifest record for parent module %s", parentKey))
	}

	if len(req.VersionConstraint.Required) != 0 {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Invalid version constraint",
			Detail:   fmt.Sprintf("Cannot apply a version constraint to module %q (at %s:%d) because it has a relative local path.", req.Name, req.CallRange.Filename, req.CallRange.Start.Line),
			Subject:  req.CallRange.Ptr(),
		})
	}

	// For local sources we don't actually need to modify the
	// filesystem at all because the parent already wrote
	// the files we need, and so we just load up what's already here.
	newDir := filepath.Join(parentRecord.Dir, req.SourceAddr.String())

	log.Printf("[TRACE] ModuleInstaller: %s uses directory from parent: %s", key, newDir)
	// it is possible that the local directory is a symlink
	newDir, err := filepath.EvalSymlinks(newDir)
	if err != nil {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Unreadable module directory",
			Detail:   fmt.Sprintf("Unable to evaluate directory symlink: %s", err.Error()),
		})
	}

	// Finally we are ready to try actually loading the module.
	mod, mDiags := i.loader.Parser().LoadConfigDir(newDir, req.Call)
	if mod == nil {
		// nil indicates missing or unreadable directory, so we'll
		// discard the returned diags and return a more specific
		// error message here.
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Unreadable module directory",
			Detail:   fmt.Sprintf("The directory %s could not be read for module %q at %s:%d.", newDir, req.Name, req.CallRange.Filename, req.CallRange.Start.Line),
		})
	} else if vDiags := mod.CheckCoreVersionRequirements(req.Path, req.SourceAddr); vDiags.HasErrors() {
		// If the core version requirements are not met, we drop any other
		// diagnostics, as they may reflect language changes from future
		// OpenTofu versions.
		diags = diags.Extend(vDiags)
	} else {
		diags = diags.Extend(mDiags)
	}

	if diags.HasErrors() {
		tracing.SetSpanError(span, diags)
	}

	// Note the local location in our manifest.
	manifest[key] = modsdir.Record{
		Key:        key,
		Dir:        newDir,
		SourceAddr: req.SourceAddr.String(),
	}
	log.Printf("[DEBUG] Module installer: %s installed at %s", key, newDir)
	hooks.Install(key, nil, newDir)

	return mod, diags
}

// versionRegexp is used to handle edge cases around prerelease version constraints
// when installing registry modules, its usage is discouraged in favor of the
// public hashicorp/go-version API.
var versionRegexp = regexp.MustCompile(version.VersionRegexpRaw)

func (i *ModuleInstaller) installRegistryModule(ctx context.Context, req *configs.ModuleRequest, key string, instPath string, addr addrs.ModuleSourceRegistry, manifest modsdir.Manifest, hooks ModuleInstallHooks, fetcher *getmodules.PackageFetcher) (*configs.Module, *version.Version, hcl.Diagnostics) {
	var diags hcl.Diagnostics

	ctx, span := tracing.Tracer().Start(ctx, "Install Registry Module",
		trace.WithAttributes(otelAttr.String(traceattrs.ModuleCallName, req.Name)),
		trace.WithAttributes(otelAttr.String(traceattrs.ModuleSource, req.SourceAddr.String())),
		trace.WithAttributes(otelAttr.String(traceattrs.ModuleVersion, req.VersionConstraint.Required.String())),
	)
	defer span.End()

	if i.reg == nil || fetcher == nil {
		// Only local package sources are available when we have no registry
		// client or no fetcher, since both would be needed for successful install.
		// (This special situation is primarily for use in tests.)
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Registry-style module sources not supported",
			Detail:   "Only local module sources are supported in this context.",
			Subject:  req.CallRange.Ptr(),
		})
		tracing.SetSpanError(span, diags)
		return nil, nil, diags
	}

	hostname := addr.Package.Host
	reg := i.reg
	var resp *response.ModuleVersions
	var exists bool

	// A registry entry isn't _really_ a module package, but we'll pretend it's
	// one for the sake of this reporting by just trimming off any source
	// directory.
	packageAddr := addr.Package

	// Our registry client is still using the legacy model of addresses, so
	// we'll shim it here for now.
	regsrcAddr := regsrc.ModuleFromRegistryPackageAddr(packageAddr)

	// check if we've already looked up this module from the registry
	if resp, exists = i.registryPackageVersions[packageAddr]; exists {
		log.Printf("[TRACE] %s using already found available versions of %s at %s", key, addr, hostname)
	} else {
		var err error
		log.Printf("[DEBUG] %s listing available versions of %s at %s", key, addr, hostname)
		resp, err = reg.ModuleVersions(ctx, regsrcAddr)
		if err != nil {
			if registry.IsModuleNotFound(err) {
				suggestion := ""
				if hostname == addrs.DefaultModuleRegistryHost {
					suggestion = "\n\nIf you believe this module is missing from the registry, please submit a issue on the OpenTofu Registry https://github.com/opentofu/registry/issues/new/choose"
				}

				diags = diags.Append(&hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "Module not found",
					Detail:   fmt.Sprintf("Module %s (%q from %s:%d) cannot be found in the module registry at %s.%s", addr.Package.ForRegistryProtocol(), req.Name, req.CallRange.Filename, req.CallRange.Start.Line, hostname, suggestion),
					Subject:  req.CallRange.Ptr(),
				})
			} else if errors.Is(err, context.Canceled) {
				diags = diags.Append(&hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "Module installation was interrupted",
					Detail:   fmt.Sprintf("Received interrupt signal while retrieving available versions for module %q.", req.Name),
				})
			} else {
				diags = diags.Append(&hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "Error accessing remote module registry",
					Detail:   fmt.Sprintf("Failed to retrieve available versions for module %q (%s:%d) from %s: %s.", req.Name, req.CallRange.Filename, req.CallRange.Start.Line, hostname, err),
					Subject:  req.CallRange.Ptr(),
				})
			}
			tracing.SetSpanError(span, diags)
			return nil, nil, diags
		}
		i.registryPackageVersions[packageAddr] = resp
	}

	// The response might contain information about dependencies to allow us
	// to potentially optimize future requests, but we don't currently do that
	// and so for now we'll just take the first item which is guaranteed to
	// be the address we requested.
	if len(resp.Modules) < 1 {
		// Should never happen, but since this is a remote service that may
		// be implemented by third-parties we will handle it gracefully.
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Invalid response from remote module registry",
			Detail:   fmt.Sprintf("The registry at %s returned an invalid response when OpenTofu requested available versions for module %q (%s:%d).", hostname, req.Name, req.CallRange.Filename, req.CallRange.Start.Line),
			Subject:  req.CallRange.Ptr(),
		})
		return nil, nil, diags
	}

	modMeta := resp.Modules[0]

	var latestMatch *version.Version
	var latestVersion *version.Version
	for _, mv := range modMeta.Versions {
		v, err := version.NewVersion(mv.Version)
		if err != nil {
			// Should never happen if the registry server is compliant with
			// the protocol, but we'll warn if not to assist someone who
			// might be developing a module registry server.
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagWarning,
				Summary:  "Invalid response from remote module registry",
				Detail:   fmt.Sprintf("The registry at %s returned an invalid version string %q for module %q (%s:%d), which OpenTofu ignored.", hostname, mv.Version, req.Name, req.CallRange.Filename, req.CallRange.Start.Line),
				Subject:  req.CallRange.Ptr(),
			})
			continue
		}

		// If we've found a pre-release version then we'll ignore it unless
		// it was exactly requested.
		//
		// The prerelease checking will be handled by a different library for
		// 2 reasons. First, this other library automatically includes the
		// "prerelease versions must be exactly requested" behaviour that we are
		// looking for. Second, this other library is used to handle all version
		// constraints for the provider logic and this is the first step to
		// making the module and provider version logic match.
		if v.Prerelease() != "" {
			// At this point all versions published by the module with
			// prerelease metadata will be checked. Users may not have even
			// requested this prerelease so don't print lots of unnecessary #
			// warnings.
			//
			// FIXME: Due to a historical implementation error, this is using the
			// wrong version constraint parser: it's expecting npm/cargo-style
			// syntax rather than the Ruby-style syntax OpenTofu otherwise
			// uses. This should have been written to use
			// versions.MeetingConstraintsStringRuby instead, but changing it
			// now risks having OpenTofu select a prerelease in more situations
			// than it did before, and so we need to understand the implications
			// of that better before we improve this. For now that means that
			// it's effectively disallowed to use anything other than a single
			// exact version constraint to select a prerelease version: any attempt
			// to combine a prerelease selection with another constraint will
			// cause all prerelease versions to be excluded from the selection.
			// For more information:
			//     https://github.com/opentofu/opentofu/issues/2117
			constraint := req.VersionConstraint.Required.String()
			acceptableVersions, err := versions.MeetingConstraintsString(constraint)
			if err != nil {
				// apparentlymart/go-versions purposely doesn't accept "v" prefixes.
				// However, hashicorp/go-version does, which leads to inconsistent
				// errors when specifying constraints that contain prerelease
				// versions with "v" prefixes. This creates a semantically equivalent
				// constraint with all prefixes stripped so it can be checked
				// against apparentlymart/go-versions. This is definitely a hack but
				// one we've accepted to minimize the risk of regressing the handling
				// of any other version constraint input until we have developed a
				// better understanding of what syntax is currently allowed for version
				// constraints and how different constraints are handled.
				//
				// strippedConstraint should not live beyond this scope.
				strippedConstraint := string(versionRegexp.ReplaceAllFunc([]byte(constraint), func(match []byte) []byte {
					if match[0] == 'v' {
						return match[1:]
					}
					return match
				}))
				if strippedConstraint != constraint {
					log.Printf("[WARN] ModuleInstaller: %s (while evaluating %q) failed parsing, so will retry with 'v' prefixes removed (%s)\n    before: %s\n    after:  %s", key, v, err.Error(), constraint, strippedConstraint)
					acceptableVersions, err = versions.MeetingConstraintsString(strippedConstraint)
					if err != nil {
						log.Printf("[WARN] ModuleInstaller: %s ignoring %q because the stripped version constraints (%q) could not be parsed either: %s", key, v, strippedConstraint, err.Error())
						continue
					}
				} else {
					// If the error here is "commas are not needed to separate version selections"
					// then that's an expected (though highly unfortunate) consequence of the
					// incorrect use of MeetingConstraintsString above. Refer to the earlier FIXME
					// comment for more information.
					log.Printf("[WARN] ModuleInstaller: %s ignoring %q because the version constraints (%q) could not be parsed: %s", key, v, strippedConstraint, err.Error())
					continue
				}
			}

			// Validate the version is also readable by the other versions
			// library.
			version, err := versions.ParseVersion(v.String())
			if err != nil {
				log.Printf("[WARN] ModuleInstaller: %s ignoring %s because the version (%s) reported by the module could not be parsed: %s", key, v, v.String(), err.Error())
				continue
			}

			// Finally, check if the prerelease is acceptable to version. As
			// highlighted previously, we go through all of this because the
			// apparentlymart/go-versions library handles prerelease constraints
			// in the approach we want to.
			if !acceptableVersions.Has(version) {
				log.Printf("[TRACE] ModuleInstaller: %s ignoring %s because it is a pre-release and was not requested exactly", key, v)
				continue
			}
			log.Printf("[TRACE] ModuleInstaller: %s accepting %s because it is a pre-release that was requested exactly", key, v)

			// If we reach here, it means this prerelease version was exactly
			// requested according to the extra constraints of this library.
			// We fall through and allow the other library to also validate it
			// for consistency.
		}

		if latestVersion == nil || v.GreaterThan(latestVersion) {
			latestVersion = v
		}

		if req.VersionConstraint.Required.Check(v) {
			if latestMatch == nil || v.GreaterThan(latestMatch) {
				latestMatch = v
			}
		}
	}

	if latestVersion == nil {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Module has no versions",
			Detail:   fmt.Sprintf("Module %q (%s:%d) has no versions available on %s.", addr, req.CallRange.Filename, req.CallRange.Start.Line, hostname),
			Subject:  req.CallRange.Ptr(),
		})
		tracing.SetSpanError(span, diags)
		return nil, nil, diags
	}

	if latestMatch == nil {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Unresolvable module version constraint",
			Detail:   fmt.Sprintf("There is no available version of module %q (%s:%d) which matches the given version constraint. The newest available version is %s.", addr, req.CallRange.Filename, req.CallRange.Start.Line, latestVersion),
			Subject:  req.CallRange.Ptr(),
		})
		tracing.SetSpanError(span, diags)
		return nil, nil, diags
	}

	// Report up to the caller that we're about to start downloading.
	hooks.Download(key, packageAddr.String(), latestMatch)

	// If we manage to get down here then we've found a suitable version to
	// install, so we need to ask the registry where we should download it from.
	// The response to this is a go-getter-style address string.

	// first check the cache for the download URL
	moduleAddr := moduleVersion{module: packageAddr, version: latestMatch.String()}
	if _, exists := i.registryPackageSources[moduleAddr]; !exists {
		realAddrRaw, err := reg.ModuleLocation(ctx, regsrcAddr, latestMatch.String())
		if err != nil {
			log.Printf("[ERROR] %s from %s %s: %s", key, addr, latestMatch, err)
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Error accessing remote module registry",
				Detail:   fmt.Sprintf("Failed to retrieve a download URL for %s %s from %s: %s", addr, latestMatch, hostname, err),
			})
			tracing.SetSpanError(span, diags)
			return nil, nil, diags
		}
		realAddr, err := addrs.ParseModuleSource(realAddrRaw)
		if err != nil {
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Invalid package location from module registry",
				Detail:   fmt.Sprintf("Module registry %s returned invalid source location %q for %s %s: %s.", hostname, realAddrRaw, addr, latestMatch, err),
			})
			tracing.SetSpanError(span, diags)
			return nil, nil, diags
		}

		span.SetAttributes(otelAttr.String(traceattrs.ModuleSource, realAddr.String()))

		switch realAddr := realAddr.(type) {
		// Only a remote source address is allowed here: a registry isn't
		// allowed to return a local path (because it doesn't know what
		// its being called from) and we also don't allow recursively pointing
		// at another registry source for simplicity's sake.
		case addrs.ModuleSourceRemote:
			i.registryPackageSources[moduleAddr] = realAddr
		default:
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Invalid package location from module registry",
				Detail:   fmt.Sprintf("Module registry %s returned invalid source location %q for %s %s: must be a direct remote package address.", hostname, realAddrRaw, addr, latestMatch),
			})
			tracing.SetSpanError(span, diags)
			return nil, nil, diags
		}
	}

	dlAddr := i.registryPackageSources[moduleAddr]

	log.Printf("[TRACE] ModuleInstaller: %s %s %s is available at %q", key, packageAddr, latestMatch, dlAddr.Package)

	err := fetcher.FetchPackage(ctx, instPath, dlAddr.Package.String())
	if errors.Is(err, context.Canceled) {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Module download was interrupted",
			Detail:   fmt.Sprintf("Interrupt signal received when downloading module %s.", addr),
		})
		return nil, nil, diags
	}
	if err != nil {
		// Errors returned by go-getter have very inconsistent quality as
		// end-user error messages, but for now we're accepting that because
		// we have no way to recognize any specific errors to improve them
		// and masking the error entirely would hide valuable diagnostic
		// information from the user.
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Failed to download module",
			Detail:   fmt.Sprintf("Could not download module %q (%s:%d) source code from %q: %s.", req.Name, req.CallRange.Filename, req.CallRange.Start.Line, dlAddr, err),
			Subject:  req.CallRange.Ptr(),
		})
		return nil, nil, diags
	}

	log.Printf("[TRACE] ModuleInstaller: %s %q was downloaded to %s", key, dlAddr.Package, instPath)

	// Incorporate any subdir information from the original path into the
	// address returned by the registry in order to find the final directory
	// of the target module.
	finalAddr := dlAddr.FromRegistry(addr)
	subDir := filepath.FromSlash(finalAddr.Subdir)
	modDir := filepath.Join(instPath, subDir)

	log.Printf("[TRACE] ModuleInstaller: %s should now be at %s", key, modDir)

	// Finally we are ready to try actually loading the module.
	mod, mDiags := i.loader.Parser().LoadConfigDir(modDir, req.Call)
	if mod == nil {
		// nil indicates missing or unreadable directory, so we'll
		// discard the returned diags and return a more specific
		// error message here. For registry modules this actually
		// indicates a bug in the code above, since it's not the
		// user's responsibility to create the directory in this case.
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Unreadable module directory",
			Detail:   fmt.Sprintf("The directory %s could not be read. This is a bug in OpenTofu and should be reported.", modDir),
		})
	} else if vDiags := mod.CheckCoreVersionRequirements(req.Path, req.SourceAddr); vDiags.HasErrors() {
		// If the core version requirements are not met, we drop any other
		// diagnostics, as they may reflect language changes from future
		// OpenTofu versions.
		diags = diags.Extend(vDiags)
	} else {
		diags = diags.Extend(mDiags)
	}

	// Note the local location in our manifest.
	manifest[key] = modsdir.Record{
		Key:        key,
		Version:    latestMatch,
		Dir:        modDir,
		SourceAddr: req.SourceAddr.String(),
	}
	log.Printf("[DEBUG] Module installer: %s installed at %s", key, modDir)
	hooks.Install(key, latestMatch, modDir)

	return mod, latestMatch, diags
}

func (i *ModuleInstaller) installGoGetterModule(ctx context.Context, req *configs.ModuleRequest, key string, instPath string, manifest modsdir.Manifest, hooks ModuleInstallHooks, fetcher *getmodules.PackageFetcher) (*configs.Module, hcl.Diagnostics) {
	var diags hcl.Diagnostics

	if fetcher == nil {
		// Only local package sources are available when we have no fetcher.
		// (This special situation is primarily for use in tests.)
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Remote module sources not supported",
			Detail:   "Only local module sources are supported in this context.",
			Subject:  req.CallRange.Ptr(),
		})
		return nil, diags
	}

	// Report up to the caller that we're about to start downloading.
	addr := req.SourceAddr.(addrs.ModuleSourceRemote)
	packageAddr := addr.Package
	hooks.Download(key, packageAddr.String(), nil)

	if len(req.VersionConstraint.Required) != 0 {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Invalid version constraint",
			Detail:   fmt.Sprintf("Cannot apply a version constraint to module %q (at %s:%d) because it doesn't come from a module registry.", req.Name, req.CallRange.Filename, req.CallRange.Start.Line),
			Subject:  req.CallRange.Ptr(),
		})
		return nil, diags
	}

	err := fetcher.FetchPackage(ctx, instPath, packageAddr.String())
	if err != nil {
		// go-getter generates a poor error for an invalid relative path, so
		// we'll detect that case and generate a better one.
		if _, ok := err.(*getmodules.MaybeRelativePathErr); ok {
			log.Printf(
				"[TRACE] ModuleInstaller: %s looks like a local path but is missing ./ or ../",
				req.SourceAddr,
			)
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Module not found",
				Detail: fmt.Sprintf(
					"The module address %q could not be resolved.\n\n"+
						"If you intended this as a path relative to the current "+
						"module, use \"./%s\" instead. The \"./\" prefix "+
						"indicates that the address is a relative filesystem path.",
					req.SourceAddr, req.SourceAddr,
				),
			})
		} else {
			// Errors returned by go-getter have very inconsistent quality as
			// end-user error messages, but for now we're accepting that because
			// we have no way to recognize any specific errors to improve them
			// and masking the error entirely would hide valuable diagnostic
			// information from the user.
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Failed to download module",
				Detail:   fmt.Sprintf("Could not download module %q (%s:%d) source code from %q: %s", req.Name, req.CallRange.Filename, req.CallRange.Start.Line, packageAddr, err),
				Subject:  req.CallRange.Ptr(),
			})
		}
		return nil, diags
	}

	modDir, err := getmodules.ExpandSubdirGlobs(instPath, addr.Subdir)
	if err != nil {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Failed to expand subdir globs",
			Detail:   err.Error(),
		})
		return nil, diags
	}

	log.Printf("[TRACE] ModuleInstaller: %s %q was downloaded to %s", key, addr, modDir)

	// Finally we are ready to try actually loading the module.
	mod, mDiags := i.loader.Parser().LoadConfigDir(modDir, req.Call)
	if mod == nil {
		// nil indicates missing or unreadable directory, so we'll
		// discard the returned diags and return a more specific
		// error message here. For go-getter modules this actually
		// indicates a bug in the code above, since it's not the
		// user's responsibility to create the directory in this case.
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Unreadable module directory",
			Detail:   fmt.Sprintf("The directory %s could not be read. This is a bug in OpenTofu and should be reported.", modDir),
		})
	} else if vDiags := mod.CheckCoreVersionRequirements(req.Path, req.SourceAddr); vDiags.HasErrors() {
		// If the core version requirements are not met, we drop any other
		// diagnostics, as they may reflect language changes from future
		// OpenTofu versions.
		diags = diags.Extend(vDiags)
	} else {
		diags = diags.Extend(mDiags)
	}

	// Note the local location in our manifest.
	manifest[key] = modsdir.Record{
		Key:        key,
		Dir:        modDir,
		SourceAddr: req.SourceAddr.String(),
	}
	log.Printf("[DEBUG] Module installer: %s installed at %s", key, modDir)
	hooks.Install(key, nil, modDir)

	return mod, diags
}

func (i *ModuleInstaller) packageInstallPath(modulePath addrs.Module) string {
	return filepath.Join(i.modsDir, strings.Join(modulePath, "."))
}

// maybeImproveLocalInstallError is a helper function which can recognize
// some specific situations where it can return a more helpful error message
// and thus replace the given errors with those if so.
//
// If this function can't do anything about a particular situation then it
// will just return the given diags verbatim.
//
// This function's behavior is only reasonable for errors returned from the
// ModuleInstaller.installLocalModule function.
func maybeImproveLocalInstallError(req *configs.ModuleRequest, diags hcl.Diagnostics) hcl.Diagnostics {
	if !diags.HasErrors() {
		return diags
	}

	// The main situation we're interested in detecting here is whether the
	// current module or any of its ancestors use relative paths that reach
	// outside of the "package" established by the nearest non-local ancestor.
	// That's never really valid, but unfortunately we historically didn't
	// have any explicit checking for it and so now for compatibility in
	// situations where things just happened to "work" we treat this as an
	// error only in situations where installation would've failed anyway,
	// so we can give a better error about it than just a generic
	// "directory not found" or whatever.
	//
	// Since it's never actually valid to relative out of the containing
	// package, we just assume that any failed local package install which
	// does so was caused by that, because to stop doing it should always
	// improve the situation, even if it leads to another error describing
	// a different problem.

	// To decide this we need to find the subset of our ancestors that
	// belong to the same "package" as our request, along with the closest
	// ancestor that defined that package, and then we can work forwards
	// to see if any of the local paths "escaped" the package.
	type Step struct {
		Path       addrs.Module
		SourceAddr addrs.ModuleSource
	}
	var packageDefiner Step
	var localRefs []Step
	localRefs = append(localRefs, Step{
		Path:       req.Path,
		SourceAddr: req.SourceAddr,
	})
	current := req.Parent // a configs.Config where Children isn't populated yet
	for {
		if current == nil || current.SourceAddr == nil {
			// We've reached the root module, in which case we aren't
			// in an external "package" at all and so our special case
			// can't apply.
			return diags
		}
		if _, ok := current.SourceAddr.(addrs.ModuleSourceLocal); !ok {
			// We've found the package definer, then!
			packageDefiner = Step{
				Path:       current.Path,
				SourceAddr: current.SourceAddr,
			}
			break
		}

		localRefs = append(localRefs, Step{
			Path:       current.Path,
			SourceAddr: current.SourceAddr,
		})
		current = current.Parent
	}
	// Our localRefs list is reversed because we were traversing up the tree,
	// so we'll flip it the other way and thus walk "downwards" through it.
	for i, j := 0, len(localRefs)-1; i < j; i, j = i+1, j-1 {
		localRefs[i], localRefs[j] = localRefs[j], localRefs[i]
	}

	// Our method here is to start with a known base path prefix and
	// then apply each of the local refs to it in sequence until one of
	// them causes us to "lose" the prefix. If that happens, we've found
	// an escape to report. This is not an exact science but good enough
	// heuristic for choosing a better error message.
	const prefix = "*/" // NOTE: this can find a false negative if the user chooses "*" as a directory name, but we consider that unlikely
	packageAddr, startPath := splitAddrSubdir(packageDefiner.SourceAddr)
	currentPath := path.Join(prefix, startPath)
	for _, step := range localRefs {
		rel := step.SourceAddr.String()

		nextPath := path.Join(currentPath, rel)
		if !strings.HasPrefix(nextPath, prefix) { // ESCAPED!
			escapeeAddr := step.Path.String()

			var newDiags hcl.Diagnostics

			// First we'll copy over any non-error diagnostics from the source diags
			for _, diag := range diags {
				if diag.Severity != hcl.DiagError {
					newDiags = newDiags.Append(diag)
				}
			}

			// ...but we'll replace any errors with this more precise error.
			var suggestion string
			if strings.HasPrefix(packageAddr, "/") || filepath.VolumeName(packageAddr) != "" {
				// It might be somewhat surprising that OpenTofu treats
				// absolute filesystem paths as "external" even though it
				// treats relative paths as local, so if it seems like that's
				// what the user was doing then we'll add an additional note
				// about it.
				suggestion = "\n\nOpenTofu treats absolute filesystem paths as external modules which establish a new module package. To treat this directory as part of the same package as its caller, use a local path starting with either \"./\" or \"../\"."
			}
			newDiags = newDiags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Local module path escapes module package",
				Detail: fmt.Sprintf(
					"The given source directory for %s would be outside of its containing package %q. Local source addresses starting with \"../\" must stay within the same package that the calling module belongs to.%s",
					escapeeAddr, packageAddr, suggestion,
				),
			})

			return newDiags
		}

		currentPath = nextPath
	}

	// If we get down here then we have nothing useful to do, so we'll just
	// echo back what we were given.
	return diags
}

func splitAddrSubdir(addr addrs.ModuleSource) (string, string) {
	switch addr := addr.(type) {
	case addrs.ModuleSourceRegistry:
		subDir := addr.Subdir
		addr.Subdir = ""
		return addr.String(), subDir
	case addrs.ModuleSourceRemote:
		return addr.Package.String(), addr.Subdir
	case nil:
		panic("splitAddrSubdir on nil addrs.ModuleSource")
	default:
		return addr.String(), ""
	}
}
