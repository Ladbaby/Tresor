package providers

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"context"
	"tresor/internal/config"
	"tresor/internal/jsplugins"
	"tresor/internal/proxy"
	"tresor/internal/store"
	"tresor/internal/wire"
)

// Result reports what an install or update actually changed. Applied and Kept
// are always disjoint; Conflicts carries the human-readable reasons a field was
// kept, so the UI can explain drift instead of silently overwriting an edit.
type Result struct {
	DownstreamID string   `json:"downstream_id"`
	ProfileID    string   `json:"profile_id"`
	Version      int      `json:"version"`
	Digest       string   `json:"digest"`
	Applied      []string `json:"applied"`
	Kept         []string `json:"kept"`
	Conflicts    []string `json:"conflicts,omitempty"`
	// InstalledPackages records the code this install actually downloaded, so the
	// UI can confirm one-click setup instead of asking the user to check.
	InstalledPackages []InstalledPackage `json:"installed_packages,omitempty"`
}

// InstalledPackage is one plugin package an install downloaded and enabled.
type InstalledPackage struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

// InstallOptions carries what the user supplies at install time: the credentials
// the profile is not allowed to contain, and consent for plugin code.
type InstallOptions struct {
	// DownstreamID overrides the default (the profile id). Empty means use the
	// profile id; a collision gets a numeric suffix rather than overwriting.
	DownstreamID string
	// APIKey is required for an api_key profile.
	APIKey string
	// ClientSecret is optional for an oauth profile (public clients have none).
	ClientSecret string
	// AllowPlugins must be true for the profile's plugin bindings to be
	// recorded. Installing code is a separate, explicit decision from
	// installing data.
	AllowPlugins bool
	// HostFeatures and HostVersion describe this daemon, so a profile's own
	// `requires` block can be enforced at install time. A profile installed
	// directly (inline, or with a catalog index that omits the fields) is then held
	// to the same bar as one the index pre-screened, instead of failing later
	// inside the plugin runtime.
	HostFeatures []string
	HostVersion  string
}

// PackageInstaller installs a plugin package from a URL. The plugin runtime
// implements it; declaring the narrow interface here keeps this package free of
// a dependency on the runtime and lets tests substitute a recorder.
type PackageInstaller interface {
	InstallRemote(ctx context.Context, input jsplugins.RemoteInput, mode proxy.Mode) (jsplugins.Manifest, error)
}

// Installer installs profiles into a store.
type Installer struct {
	store *store.Store
	// cfg is the live config the store writes back to. Nil means "do not
	// write back", which keeps the installer usable in tests and CLI contexts
	// that manage the YAML themselves.
	cfg *config.AppConfig
	// packages installs the code a profile binding points at. Nil means this
	// daemon cannot install packages, so a binding that carries a URL is reported
	// as a conflict instead of failing the whole install.
	packages PackageInstaller
	// proxyMode is how the package download is routed.
	proxyMode proxy.Mode
}

// NewInstaller returns an Installer backed by s. cfg may be nil when the caller
// does not want YAML write-back (tests, dry runs).
func NewInstaller(s *store.Store, cfg *config.AppConfig) *Installer {
	return &Installer{store: s, cfg: cfg}
}

// WithPackages enables one-click install: a profile binding that carries a URL is
// downloaded and installed as part of the same operation.
func (in *Installer) WithPackages(p PackageInstaller, mode proxy.Mode) *Installer {
	in.packages = p
	in.proxyMode = mode
	return in
}

// installPackages downloads and installs every binding that carries a URL,
// returning a per-binding outcome. A binding without a URL is left to the caller:
// it may already be installed, and the engine resolves it by id at request time.
func (in *Installer) installPackages(p *Profile, res *Result) {
	for _, b := range p.Plugins {
		if b.URL == "" {
			if b.Version != "" {
				res.Kept = append(res.Kept, "plugin:"+b.ID)
			}
			continue
		}
		if in.packages == nil {
			res.Conflicts = append(res.Conflicts,
				fmt.Sprintf("plugin %q was not downloaded: package installation is unavailable on this daemon", b.ID))
			continue
		}
		// The recipe names the package it wants, and ExpectID makes the installer
		// enforce that during verification — a URL serving a different package is
		// refused before it is persisted or enabled, rather than being installed and
		// then rejected.
		manifest, err := in.packages.InstallRemote(context.Background(), jsplugins.RemoteInput{
			URL:      b.URL,
			SHA256:   b.SHA256,
			ExpectID: b.ID,
		}, in.proxyMode)
		if err != nil {
			res.Conflicts = append(res.Conflicts,
				fmt.Sprintf("plugin %q could not be installed: %v", b.ID, err))
			continue
		}
		res.InstalledPackages = append(res.InstalledPackages, InstalledPackage{
			ID:      manifest.ID,
			Version: manifest.Version,
		})
	}
}

// writeBack persists the installed state to YAML when a config was supplied.
func (in *Installer) writeBack() error {
	if in.cfg == nil || in.cfg.ConfigPath == "" {
		return nil
	}
	return in.store.WriteConfig(in.cfg)
}

// appliedSnapshot is the exact set of field values a profile wrote. A later
// update compares the live downstream against this: a field that still matches
// was untouched by the user and may be overwritten; one that does not was
// edited and is preserved.
type appliedSnapshot struct {
	BaseURL     string                `json:"base_url"`
	ApiFormats  []string              `json:"api_formats"`
	FormatURLs  map[string]string     `json:"format_urls"`
	FormatPaths map[string]string     `json:"format_paths"`
	Models      []string              `json:"models"`
	Plugins     []config.PipelineStep `json:"plugins"`
	AuthType    string                `json:"auth_type"`
	AuthFlow    string                `json:"auth_flow,omitempty"`
	// AuthWire is tracked so a profile that renames its dialect (or adopts one)
	// applies on update. Without it, the field the provider most depends on was
	// the one an update could never change.
	AuthWire string `json:"auth_wire,omitempty"`
	// Auth is the profile's full auth declaration, kept in memory only (never
	// serialized): it is what an update copies when the shape changed.
	Auth *config.DownstreamAuthCfg `json:"-"`
}

// snapshotOf builds the snapshot for the downstream a profile describes.
func (p *Profile) snapshotOf(plugins []config.PipelineStep) appliedSnapshot {
	s := appliedSnapshot{
		BaseURL:     p.Provider.BaseURL,
		ApiFormats:  append([]string{}, p.Provider.ApiFormats...),
		FormatURLs:  cloneMap(p.Provider.FormatURLs),
		FormatPaths: cloneMap(p.Provider.FormatPaths),
		Models:      append([]string{}, p.Provider.Models...),
		Plugins:     plugins,
	}
	if p.Provider.Auth != nil {
		s.AuthType = p.Provider.Auth.Type
		s.AuthFlow = p.Provider.Auth.Flow
		s.AuthWire = p.Provider.Auth.Wire
		s.Auth = p.Provider.Auth.AuthConfig()
	}
	if s.AuthType == "" {
		s.AuthType = "api_key"
	}
	if s.ApiFormats == nil {
		s.ApiFormats = []string{}
	}
	if s.Models == nil {
		s.Models = []string{}
	}
	if s.Plugins == nil {
		s.Plugins = []config.PipelineStep{}
	}
	return s
}

// bindingsToSteps converts plugin bindings into downstream pipeline steps.
func bindingsToSteps(bindings []PluginBinding) []config.PipelineStep {
	steps := make([]config.PipelineStep, 0, len(bindings))
	for i, b := range bindings {
		id := b.ID
		if id == "" {
			id = fmt.Sprintf("bind-%d", i)
		}
		cfg := b.Config
		if cfg == nil {
			cfg = map[string]any{}
		}
		steps = append(steps, config.PipelineStep{ID: id, PluginID: b.ID, Config: cfg})
	}
	return steps
}

// Install creates a downstream (plus provenance, plugin bindings, and aliases
// whose groups are free) from a profile.
func (in *Installer) Install(p *Profile, opts InstallOptions) (*Result, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if err := p.Requires.Check(opts.HostFeatures, opts.HostVersion); err != nil {
		return nil, fmt.Errorf("provider profile %s: %w", p.ID, err)
	}
	digest, err := p.Digest()
	if err != nil {
		return nil, err
	}

	id := strings.TrimSpace(opts.DownstreamID)
	if id == "" {
		id = p.ID
	}
	id, err = in.freeDownstreamID(id)
	if err != nil {
		return nil, err
	}

	auth := p.Provider.Auth.AuthConfig()
	if auth.Type == "api_key" {
		if strings.TrimSpace(opts.APIKey) == "" {
			return nil, fmt.Errorf("provider %s: an API key is required", p.ID)
		}
		auth.APIKey = opts.APIKey
	} else if opts.ClientSecret != "" {
		auth.ClientSecret = opts.ClientSecret
	}

	ds := &store.Downstream{
		ID:             id,
		Name:           p.Name,
		BaseURL:        p.Provider.BaseURL,
		ApiFormats:     append([]string{}, p.Provider.ApiFormats...),
		FormatURLs:     cloneMap(p.Provider.FormatURLs),
		FormatPaths:    cloneMap(p.Provider.FormatPaths),
		IsEnabled:      true,
		Auth:           auth,
		OutputModelIDs: append([]string{}, p.Provider.Models...),
	}
	if opts.AllowPlugins {
		ds.Plugins = bindingsToSteps(p.Plugins)
	}
	if ds.ApiFormats == nil {
		ds.ApiFormats = []string{}
	}
	if err := in.store.CreateDownstream(ds); err != nil {
		return nil, err
	}

	res := &Result{DownstreamID: id, ProfileID: p.ID, Version: p.Version, Digest: digest}
	if opts.AllowPlugins {
		in.installPackages(p, res)
	}
	// Checked after the packages land, because the recipe may be what supplies
	// the dialect it names. A provider whose dialect is still missing would fail
	// on every request, so say so now instead of at first use.
	in.checkDialect(p, res)
	for _, field := range []string{"provider", "auth", "models"} {
		res.Applied = append(res.Applied, field)
	}
	if len(ds.Plugins) > 0 {
		res.Applied = append(res.Applied, "plugins")
	} else if len(p.Plugins) > 0 {
		res.Kept = append(res.Kept, "plugins")
		res.Conflicts = append(res.Conflicts, "plugin bindings were not installed because plugin consent was not given")
	}

	snapshot, err := store.MarshalApplied(p.snapshotOf(ds.Plugins))
	if err != nil {
		return nil, err
	}
	if err := in.store.SetDownstreamProfile(&store.DownstreamProfile{
		DownstreamID: id,
		ProfileID:    p.ID,
		Version:      p.Version,
		Digest:       digest,
		Source:       "local",
		Applied:      snapshot,
	}); err != nil {
		return nil, err
	}

	in.applyAliases(p, id, res)

	if err := in.writeBack(); err != nil {
		return res, err
	}
	return res, nil
}

// checkDialect reports a missing dialect. It is a conflict rather than an error:
// the provider is otherwise installed, and installing the package later makes it
// work without re-running the profile.
func (in *Installer) checkDialect(p *Profile, res *Result) {
	if p.Provider.Auth == nil || p.Provider.Auth.Wire == "" {
		return
	}
	if _, ok := wire.For(p.Provider.Auth.Wire); ok {
		return
	}
	res.Conflicts = append(res.Conflicts,
		fmt.Sprintf("auth.wire %q is not available yet: install the plugin package that provides it, "+
			"or requests will fail until it is installed", p.Provider.Auth.Wire))
}

// applyAliases creates the profile's suggested aliases, but only for input
// models no existing group claims. A provider install must never rewrite routing
// the user set up.
func (in *Installer) applyAliases(p *Profile, downstreamID string, res *Result) {
	for _, a := range p.Aliases {
		exists, err := in.store.AliasGroupExists(a.InputModelID)
		if err != nil {
			res.Kept = append(res.Kept, "alias:"+a.InputModelID)
			res.Conflicts = append(res.Conflicts, fmt.Sprintf("alias %s could not be checked: %v", a.InputModelID, err))
			continue
		}
		if exists {
			res.Kept = append(res.Kept, "alias:"+a.InputModelID)
			res.Conflicts = append(res.Conflicts, fmt.Sprintf("alias group %q already exists; left as it is", a.InputModelID))
			continue
		}
		if err := in.store.CreateAlias(&store.Alias{
			InputModelID:  a.InputModelID,
			DownstreamID:  downstreamID,
			OutputModelID: a.OutputModelID,
			IsRegex:       a.IsRegex,
			IsActive:      true,
		}); err != nil {
			res.Kept = append(res.Kept, "alias:"+a.InputModelID)
			res.Conflicts = append(res.Conflicts, fmt.Sprintf("alias %s was not created: %v", a.InputModelID, err))
			continue
		}
		res.Applied = append(res.Applied, "alias:"+a.InputModelID)
	}
}

// Update applies a newer profile version to a downstream without discarding the
// user's edits. Each profile-owned field is compared against the last applied
// snapshot: unchanged fields take the new value, changed fields are kept and
// reported.
func (in *Installer) Update(downstreamID string, p *Profile, opts InstallOptions) (*Result, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if err := p.Requires.Check(opts.HostFeatures, opts.HostVersion); err != nil {
		return nil, fmt.Errorf("provider profile %s: %w", p.ID, err)
	}
	ds, err := in.store.GetDownstream(downstreamID)
	if err != nil {
		return nil, err
	}
	prov, err := in.store.GetDownstreamProfile(downstreamID)
	if err != nil {
		return nil, err
	}
	if prov == nil {
		return nil, fmt.Errorf("downstream %s was not installed from a provider profile", downstreamID)
	}
	var prev appliedSnapshot
	if prov.Applied != "" && prov.Applied != "{}" {
		if err := json.Unmarshal([]byte(prov.Applied), &prev); err != nil {
			// An unreadable snapshot means we cannot tell an edit from an
			// untouched field, so the safe choice is to keep everything and say
			// so rather than overwrite the user's work.
			return nil, fmt.Errorf("downstream %s has an unreadable applied snapshot; reinstall instead of updating: %w", downstreamID, err)
		}
	}

	digest, err := p.Digest()
	if err != nil {
		return nil, err
	}
	res := &Result{DownstreamID: downstreamID, ProfileID: p.ID, Version: p.Version, Digest: digest}

	want := p.snapshotOf(prev.Plugins)
	next := *ds

	// A newer profile may add or change the code it needs. Installing it here keeps
	// update and install equivalent; skipping it left the old package active and
	// reported nothing, so the provider kept running yesterday's dialect.
	if opts.AllowPlugins {
		in.installPackages(p, res)
	}

	// base_url
	switch {
	case ds.BaseURL == prev.BaseURL && ds.BaseURL != want.BaseURL:
		next.BaseURL = want.BaseURL
		res.Applied = append(res.Applied, "base_url")
	case ds.BaseURL != prev.BaseURL && ds.BaseURL != want.BaseURL:
		res.Kept = append(res.Kept, "base_url")
		res.Conflicts = append(res.Conflicts, fmt.Sprintf("base_url was edited locally (%q); kept it", ds.BaseURL))
	}

	// api_formats
	if equalStrings(ds.ApiFormats, prev.ApiFormats) {
		if !equalStrings(ds.ApiFormats, want.ApiFormats) {
			next.ApiFormats = want.ApiFormats
			res.Applied = append(res.Applied, "api_formats")
		}
	} else if !equalStrings(ds.ApiFormats, want.ApiFormats) {
		res.Kept = append(res.Kept, "api_formats")
		res.Conflicts = append(res.Conflicts, fmt.Sprintf("api_formats were edited locally (%v); kept them", ds.ApiFormats))
	}

	// format_urls / format_paths
	applyMapField("format_urls", ds.FormatURLs, prev.FormatURLs, want.FormatURLs, &next.FormatURLs, res)
	applyMapField("format_paths", ds.FormatPaths, prev.FormatPaths, want.FormatPaths, &next.FormatPaths, res)

	// models
	if equalStrings(ds.OutputModelIDs, prev.Models) {
		if !equalStrings(ds.OutputModelIDs, want.Models) {
			next.OutputModelIDs = want.Models
			res.Applied = append(res.Applied, "models")
		}
	} else if !equalStrings(ds.OutputModelIDs, want.Models) {
		res.Kept = append(res.Kept, "models")
		res.Conflicts = append(res.Conflicts, "model list was edited locally; kept it")
	}

	// auth structure (never credentials)
	applyAuth(next.Auth, ds.Auth, prev, want, res)

	// plugin bindings
	if opts.AllowPlugins {
		wantSteps := bindingsToSteps(p.Plugins)
		if equalSteps(ds.Plugins, prev.Plugins) {
			if !equalSteps(ds.Plugins, wantSteps) {
				next.Plugins = wantSteps
				res.Applied = append(res.Applied, "plugins")
			}
		} else if !equalSteps(ds.Plugins, wantSteps) {
			res.Kept = append(res.Kept, "plugins")
			res.Conflicts = append(res.Conflicts, "plugin bindings were edited locally; kept them")
		}
	} else if len(p.Plugins) > 0 {
		res.Kept = append(res.Kept, "plugins")
		res.Conflicts = append(res.Conflicts, "plugin bindings were not updated because plugin consent was not given")
	}

	// Credentials the user supplied replace what is stored; absent ones stay.
	if opts.APIKey != "" && next.Auth != nil {
		next.Auth.APIKey = opts.APIKey
	}
	if opts.ClientSecret != "" && next.Auth != nil {
		next.Auth.ClientSecret = opts.ClientSecret
	}

	if err := in.store.UpdateDownstream(&next); err != nil {
		return nil, err
	}

	// The snapshot records what the profile now owns: the values actually
	// written for applied fields, and the old values for anything kept, so a
	// later update still recognizes the user's edit as theirs.
	snap := want
	if !contains(res.Applied, "base_url") {
		snap.BaseURL = prev.BaseURL
	}
	if !contains(res.Applied, "api_formats") {
		snap.ApiFormats = prev.ApiFormats
	}
	if !contains(res.Applied, "format_urls") {
		snap.FormatURLs = prev.FormatURLs
	}
	if !contains(res.Applied, "format_paths") {
		snap.FormatPaths = prev.FormatPaths
	}
	if !contains(res.Applied, "models") {
		snap.Models = prev.Models
	}
	if !contains(res.Applied, "plugins") {
		snap.Plugins = prev.Plugins
	}
	applied, err := store.MarshalApplied(snap)
	if err != nil {
		return nil, err
	}
	if err := in.store.SetDownstreamProfile(&store.DownstreamProfile{
		DownstreamID: downstreamID,
		ProfileID:    p.ID,
		Version:      p.Version,
		Digest:       digest,
		Source:       prov.Source,
		Applied:      applied,
	}); err != nil {
		return nil, err
	}

	// Checked after the packages land, because the profile may be what supplies the
	// dialect it names.
	in.checkDialect(p, res)

	if err := in.writeBack(); err != nil {
		return res, err
	}
	return res, nil
}

// applyMapField applies the three-way rule to one map-valued field.
func applyMapField(name string, current, prev, want map[string]string, dst *map[string]string, res *Result) {
	if equalMaps(current, prev) {
		if !equalMaps(current, want) {
			*dst = cloneMap(want)
			res.Applied = append(res.Applied, name)
		}
		return
	}
	if !equalMaps(current, want) {
		res.Kept = append(res.Kept, name)
		res.Conflicts = append(res.Conflicts, fmt.Sprintf("%s was edited locally; kept it", name))
	}
}

// applyAuth refreshes the publishable auth structure while preserving whatever
// credentials are already stored. Only the type and flow are compared, because
// they are what decides how a request is authenticated; the rest of an OAuth
// block is copied in wholesale when the user has not replaced it.
// applyAuth reconciles the stored auth block with the profile's. current is the
// live block (which holds the user's credentials); next is the one about to be
// written; want is the profile's shape, including the full AuthSpec.
func applyAuth(next, current *config.DownstreamAuthCfg, prev, want appliedSnapshot, res *Result) {
	if current == nil {
		return
	}
	if current.Type != prev.AuthType && current.Type != want.AuthType {
		res.Kept = append(res.Kept, "auth")
		res.Conflicts = append(res.Conflicts, fmt.Sprintf("auth method was changed locally (%q); kept it", current.Type))
		return
	}
	if current.Type == want.AuthType && current.Flow == want.AuthFlow && current.Wire == want.AuthWire {
		return // already what the profile wants
	}
	// Structure changed in the profile. Keep the credentials the user supplied —
	// the profile has none — and take everything else from the profile. Copying
	// from `current` instead would drop exactly what a new profile version exists
	// to change: rotated endpoints, a new client id, a renamed dialect.
	merged := *current
	if want.Auth != nil {
		w := *want.Auth
		w.APIKey = current.APIKey
		w.ClientSecret = current.ClientSecret
		merged = w
	} else {
		merged.Type = want.AuthType
		merged.Flow = want.AuthFlow
		merged.Wire = want.AuthWire
	}
	if next != nil {
		*next = merged
	}
	res.Applied = append(res.Applied, "auth")
}

// freeDownstreamID returns id when unused, otherwise id-2, id-3, ... so
// installing twice does not clobber the first install.
func (in *Installer) freeDownstreamID(id string) (string, error) {
	if _, err := in.store.GetDownstream(id); err != nil {
		return id, nil // not found
	}
	for n := 2; n < 1000; n++ {
		candidate := fmt.Sprintf("%s-%d", id, n)
		if _, err := in.store.GetDownstream(candidate); err != nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("cannot find a free downstream id for %q", id)
}

func cloneMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalSteps(a, b []config.PipelineStep) bool {
	ja, errA := canonicalSteps(a)
	jb, errB := canonicalSteps(b)
	if errA != nil || errB != nil {
		return false
	}
	return ja == jb
}

func canonicalSteps(steps []config.PipelineStep) (string, error) {
	sorted := append([]config.PipelineStep{}, steps...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	b, err := json.Marshal(sorted)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}
