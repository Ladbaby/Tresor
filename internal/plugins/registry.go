package plugins

import (
	"fmt"
	"sort"

	"tresor/internal/engine"
	"tresor/internal/jsplugins"
)

// registry holds all available plugin factories.
// ponytail: maps are populated only in NewRegistry and read-only after; no lock needed.
type registry struct {
	factories map[string]engine.PluginFactory
	info      map[string]engine.PluginInfo
}

// NewRegistry creates a new plugin registry and registers all built-in plugins.
func NewRegistry() engine.PluginRegistry {
	r := &registry{
		factories: make(map[string]engine.PluginFactory),
		info:      make(map[string]engine.PluginInfo),
	}

	// Register built-in plugins
	r.register("openai2anthropic", engine.PluginInfo{
		ID:          "openai2anthropic",
		Description: "Converts OpenAI Chat Completion requests to Anthropic Messages format",
		ConfigSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, func(config map[string]interface{}) (interface{}, error) {
		return &OpenAI2Anthropic{}, nil
	})

	r.register("anthropic2openai", engine.PluginInfo{
		ID:          "anthropic2openai",
		Description: "Converts Anthropic Messages requests to OpenAI Chat Completion format",
		ConfigSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, func(config map[string]interface{}) (interface{}, error) {
		return &Anthropic2OpenAI{}, nil
	})

	r.register("responses2openai", engine.PluginInfo{
		ID:          "responses2openai",
		Description: "Converts OpenAI Responses API requests to Chat Completions format and vice versa",
		ConfigSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, func(config map[string]interface{}) (interface{}, error) {
		return &Responses2OpenAI{}, nil
	})

	r.register("responses2anthropic", engine.PluginInfo{
		ID:          "responses2anthropic",
		Description: "Converts OpenAI Responses API requests to Anthropic Messages format and vice versa",
		ConfigSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, func(config map[string]interface{}) (interface{}, error) {
		return &Responses2Anthropic{}, nil
	})

	r.register("openai2responses", engine.PluginInfo{
		ID:          "openai2responses",
		Description: "Converts OpenAI Chat Completion requests to Responses API format",
		ConfigSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, func(config map[string]interface{}) (interface{}, error) {
		return &OpenAI2Responses{}, nil
	})

	r.register("anthropic2responses", engine.PluginInfo{
		ID:          "anthropic2responses",
		Description: "Converts Anthropic Messages requests to Responses API format",
		ConfigSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, func(config map[string]interface{}) (interface{}, error) {
		return &Anthropic2Responses{}, nil
	})

	r.register("openai2gemini", engine.PluginInfo{
		ID:          "openai2gemini",
		Description: "Converts OpenAI Chat Completion requests to Google Gemini generateContent format",
		ConfigSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, func(config map[string]interface{}) (interface{}, error) {
		return &OpenAI2Gemini{}, nil
	})

	r.register("anthropic2gemini", engine.PluginInfo{
		ID:          "anthropic2gemini",
		Description: "Converts Anthropic Messages requests to Google Gemini generateContent format",
		ConfigSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, func(config map[string]interface{}) (interface{}, error) {
		return &Anthropic2Gemini{}, nil
	})

	r.register("gemini2openai", engine.PluginInfo{
		ID:          "gemini2openai",
		Description: "Converts Google Gemini generateContent requests to OpenAI Chat Completion format and vice versa",
		ConfigSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, func(config map[string]interface{}) (interface{}, error) {
		return &Gemini2OpenAI{}, nil
	})

	r.register("gemini2anthropic", engine.PluginInfo{
		ID:          "gemini2anthropic",
		Description: "Converts Google Gemini generateContent requests to Anthropic Messages format and vice versa",
		ConfigSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, func(config map[string]interface{}) (interface{}, error) {
		return &Gemini2Anthropic{}, nil
	})

	r.register("gemini2responses", engine.PluginInfo{
		ID:          "gemini2responses",
		Description: "Converts Google Gemini generateContent requests to OpenAI Responses API format and vice versa",
		ConfigSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, func(config map[string]interface{}) (interface{}, error) {
		return &Gemini2Responses{}, nil
	})

	return r
}

func (r *registry) register(id string, info engine.PluginInfo, factory engine.PluginFactory) {
	r.factories[id] = factory
	r.info[id] = info
}

// CreatePlugin instantiates a plugin by ID with the given configuration.
func (r *registry) CreatePlugin(pluginID string, config map[string]interface{}) (interface{}, error) {
	factory, ok := r.factories[pluginID]
	if !ok {
		return nil, fmt.Errorf("plugin %q not found", pluginID)
	}

	return factory(config)
}

// Native format converters are internal capabilities, not user-rule plugins.
func (r *registry) IsInternalPlugin(id string) bool { _, ok := r.factories[id]; return ok }

// ListPlugins returns only user-rule plugin metadata.
func (r *registry) ListPlugins() []engine.PluginInfo { return []engine.PluginInfo{} }

// NewManagedRegistry composes protected built-ins with live local JS packages.
func NewManagedRegistry(manager *jsplugins.Manager) engine.PluginRegistry {
	return &managedRegistry{builtin: NewRegistry(), manager: manager}
}

type managedRegistry struct {
	builtin engine.PluginRegistry
	manager *jsplugins.Manager
}

// Snapshot pins every package used by one request, including retries.
func (r *managedRegistry) Snapshot() engine.PluginRegistry {
	packages, unavailable := r.manager.ExecutionSnapshot()
	return &packageSnapshot{builtin: r.builtin, packages: packages, unavailable: unavailable}
}

func (r *managedRegistry) CreatePlugin(id string, config map[string]interface{}) (interface{}, error) {
	return r.Snapshot().CreatePlugin(id, config)
}

func (r *managedRegistry) IsInternalPlugin(id string) bool {
	return engine.IsInternalPlugin(r.builtin, id)
}
func (r *managedRegistry) ListPlugins() []engine.PluginInfo { return r.Snapshot().ListPlugins() }

type packageSnapshot struct {
	builtin     engine.PluginRegistry
	unavailable map[string]string
	packages    map[string]*jsplugins.Package
}

func (r *packageSnapshot) CreatePlugin(id string, config map[string]interface{}) (interface{}, error) {
	if reason := r.unavailable[id]; reason != "" {
		return nil, fmt.Errorf("plugin %s %s", id, reason)
	}
	if pkg, ok := r.packages[id]; ok {
		instance, err := jsplugins.NewInstance(pkg, config)
		if err != nil {
			return nil, err
		}
		return engine.NewExternalPlugin(instance), nil
	}
	return r.builtin.CreatePlugin(id, config)
}

func (r *packageSnapshot) IsInternalPlugin(id string) bool {
	return engine.IsInternalPlugin(r.builtin, id)
}

func (r *packageSnapshot) ListPlugins() []engine.PluginInfo {
	result := r.builtin.ListPlugins()
	for _, pkg := range r.packages {
		m := pkg.Manifest
		result = append(result, engine.PluginInfo{ID: m.ID, Description: m.Description, ConfigSchema: m.ConfigSchema})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

// BuiltinIDs returns all identities protected from local package overrides.
func BuiltinIDs() []string {
	var ids []string
	for id := range NewRegistry().(*registry).factories {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Ensure registry implements engine.PluginRegistry.
var _ engine.PluginRegistry = (*registry)(nil)
