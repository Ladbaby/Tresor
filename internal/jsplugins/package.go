// Package jsplugins executes trusted, local, synchronous V2 plugin packages.
// Limits and interruption are safeguards, not a hard process-memory sandbox.
package jsplugins

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/dop251/goja"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const MaxSourceBytes = 8 << 20
const MaxValueBytes = 8 << 20

// Collection has a decoded body/state ceiling; wire envelopes may expand each
// byte into a six-character JSON escape. Ordinary hooks keep MaxValueBytes.
const MaxCollectionBytes = 64 << 20
const maxCollectionWireBytes = 6*MaxCollectionBytes + (1 << 20)
const MaxFiles = 128
const MaxEvents = 256

var hooks = map[string]bool{"request": true, "responseHeaders": true, "response": true, "streamStart": true, "streamEvent": true, "streamEnd": true, "collectStart": true, "collectEvent": true, "collectEnd": true, "finish": true, "upstreamStart": true, "upstreamEvent": true, "upstreamEnd": true}
var features = map[string]bool{"sse.events.v2": true, "sse.collect.v2": true, "sse.upstream.v2": true, "buffer.v1": true, "json.lossless.v1": true, "query.v1": true, "encoding.v1": true, "json.patch.v1": true, "log.v1": true}
var identity = regexp.MustCompile(`^[a-z][a-z0-9_-]*(?:/[a-z][a-z0-9_-]*)?$`)
var versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?(?:\+[A-Za-z0-9.-]+)?$`)

type Manifest struct {
	ID           string         `json:"id"`
	Version      string         `json:"version"`
	Description  string         `json:"description,omitempty"`
	APIVersion   int            `json:"api_version"`
	Entry        string         `json:"entry"`
	Requires     []string       `json:"requires"`
	Permissions  []string       `json:"permissions"`
	ConfigSchema map[string]any `json:"config_schema"`
}
type Package struct {
	Manifest   Manifest
	Digest     string
	files      map[string]string
	programs   map[string]*goja.Program
	schema     *jsonschema.Schema
	Compatible bool
	granted    map[string]bool
}
type PackageInfo struct {
	Manifest
	Digest     string     `json:"digest"`
	Active     bool       `json:"active"`
	Selected   bool       `json:"selected"`
	Compatible bool       `json:"compatible"`
	Diagnostic string     `json:"diagnostic,omitempty"`
	Executable bool       `json:"executable"`
	Approved   []string   `json:"approved"`
	Provenance Provenance `json:"provenance"`
}

func safeFile(name string) bool {
	if name == "" || len(name) > 240 || strings.ContainsAny(name, "\\:\x00") || strings.HasPrefix(name, "/") || path.Clean(name) != name {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "." || part == ".." || part == "" || strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
			return false
		}
		upper := strings.ToUpper(strings.Split(part, ".")[0])
		if upper == "CON" || upper == "PRN" || upper == "AUX" || upper == "NUL" || regexp.MustCompile(`^(COM|LPT)[0-9]$`).MatchString(upper) {
			return false
		}
	}
	return true
}

// ValidPackageID accepts plain or namespaced IDs. Native-ID collision checks
// are handled separately by Manager; namespacing is recommended for third parties.
func ValidPackageID(id string) bool {
	return identity.MatchString(id)
}

func LoadPackage(files map[string]string) (*Package, error) {
	return loadPackage(files, false)
}

// Cache discovery validates identity and digest without compiling or executing
// unsupported entrypoints. Installation remains executable-V2-only.
func loadPackage(files map[string]string, discover bool) (*Package, error) {
	digest, err := SourceDigest(files)
	if err != nil {
		return nil, err
	}
	copied := make(map[string]string, len(files))
	names := make([]string, 0, len(files))
	for name, text := range files {
		copied[name] = text
		names = append(names, name)
	}
	var manifest Manifest
	if err := StrictJSON([]byte(files["manifest.json"]), &manifest); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if !ValidPackageID(manifest.ID) {
		return nil, fmt.Errorf("plugin ID must be a lowercase plain name or namespace/name; each segment must start with a letter and contain only letters, digits, underscores or hyphens")
	}
	if !versionPattern.MatchString(manifest.Version) || manifest.APIVersion < 1 {
		return nil, fmt.Errorf("invalid package/API version")
	}
	compatible := manifest.APIVersion == 2
	if !compatible && !discover {
		return nil, fmt.Errorf("unsupported API version %d: install an API V2 package", manifest.APIVersion)
	}
	if !safeFile(manifest.Entry) || !strings.HasSuffix(manifest.Entry, ".js") {
		return nil, fmt.Errorf("invalid entry")
	}
	if _, ok := files[manifest.Entry]; !ok {
		return nil, fmt.Errorf("entry missing")
	}
	permissions := map[string]bool{}
	for _, permission := range manifest.Permissions {
		if permission != "log.v1" || permissions[permission] {
			return nil, fmt.Errorf("unsupported/duplicate permission %q", permission)
		}
		permissions[permission] = true
	}
	seen := map[string]bool{}
	for _, feature := range manifest.Requires {
		if (compatible && !features[feature]) || seen[feature] {
			return nil, fmt.Errorf("unsupported/duplicate feature %q", feature)
		}
		seen[feature] = true
	}
	if seen["log.v1"] != permissions["log.v1"] {
		return nil, fmt.Errorf("log.v1 requires matching capability and permission")
	}
	if manifest.ConfigSchema == nil {
		manifest.ConfigSchema = map[string]any{"type": "object"}
	}
	// Schema compilation has no network loader: remote references must be bundled
	// into the schema itself. Fix the dialect rather than trusting a package URL.
	if dialect, ok := manifest.ConfigSchema["$schema"]; ok && dialect != "https://json-schema.org/draft/2020-12/schema" {
		return nil, fmt.Errorf("schema must use draft 2020-12")
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(noSchemaLoader{})
	if err := compiler.AddResource("https://tresor.invalid/plugin-config", manifest.ConfigSchema); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	schema, err := compiler.Compile("https://tresor.invalid/plugin-config")
	if err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	pkg := &Package{Manifest: manifest, files: copied, programs: map[string]*goja.Program{}, schema: schema, Compatible: compatible}
	sort.Strings(names)
	for _, name := range names {
		if compatible && strings.HasSuffix(name, ".js") {
			program, err := goja.Compile(name, "(function(module,exports,require){\"use strict\";\n"+copied[name]+"\n})", true)
			if err != nil {
				return nil, fmt.Errorf("compile %s: %w", name, err)
			}
			pkg.programs[name] = program
		}
	}
	pkg.Digest = digest
	if !compatible {
		return pkg, nil
	}
	instance, err := newInstance(pkg, map[string]any{}, false)
	if err != nil {
		return nil, err
	}
	// Load verifies exports and all top-level imports before install/activation.
	if instance.HasHook("collectStart") || instance.HasHook("collectEvent") || instance.HasHook("collectEnd") {
		if !seen["sse.collect.v2"] || !instance.HasHook("collectStart") || !instance.HasHook("collectEvent") || !instance.HasHook("collectEnd") {
			return nil, fmt.Errorf("collection requires feature and all three hooks")
		}
	}
	for _, hook := range []string{"upstreamStart", "upstreamEvent", "upstreamEnd"} {
		if instance.HasHook(hook) && !seen["sse.upstream.v2"] {
			return nil, fmt.Errorf("upstream hooks require sse.upstream.v2")
		}
	}
	return pkg, nil
}

type noSchemaLoader struct{}

func (noSchemaLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("external schema reference forbidden: %s", url)
}
