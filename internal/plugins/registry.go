package plugins

import (
	"fmt"
	"sync"

	"tresor/internal/engine"
)

// registry holds all available plugin factories.
type registry struct {
	mu      sync.RWMutex
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
	r.register("custom_header", engine.PluginInfo{
		ID:          "custom_header",
		Description: "Injects custom HTTP headers into the forwarded request",
		ConfigSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"headers": map[string]interface{}{
					"type":                 "object",
					"description":          "Key-value pairs of HTTP headers to inject",
					"additionalProperties": map[string]interface{}{"type": "string"},
				},
			},
			"required": []interface{}{"headers"},
		},
	}, func(config map[string]interface{}) (interface{}, error) {
		return NewCustomHeaderPlugin(config)
	})

	r.register("openai2anthropic", engine.PluginInfo{
		ID:          "openai2anthropic",
		Description: "Converts OpenAI Chat Completion requests to Anthropic Messages format",
		ConfigSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{},
		},
	}, func(config map[string]interface{}) (interface{}, error) {
		return &OpenAI2Anthropic{}, nil
	})

	r.register("fix_anthropic_images", engine.PluginInfo{
		ID:          "fix_anthropic_images",
		Description: "Extracts image parts from tool_result.content[] and promotes them to top-level message content for Anthropic-compatible backends",
		ConfigSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{},
		},
	}, func(config map[string]interface{}) (interface{}, error) {
		return &FixAnthropicImages{}, nil
	})

	r.register("anthropic2openai", engine.PluginInfo{
		ID:          "anthropic2openai",
		Description: "Converts Anthropic Messages requests to OpenAI Chat Completion format",
		ConfigSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{},
		},
	}, func(config map[string]interface{}) (interface{}, error) {
		return &Anthropic2OpenAI{}, nil
	})

	return r
}

func (r *registry) register(id string, info engine.PluginInfo, factory engine.PluginFactory) {
	r.factories[id] = factory
	r.info[id] = info
}

// CreatePlugin instantiates a plugin by ID with the given configuration.
func (r *registry) CreatePlugin(pluginID string, config map[string]interface{}) (interface{}, error) {
	r.mu.RLock()
	factory, ok := r.factories[pluginID]
	r.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("plugin %q not found", pluginID)
	}

	return factory(config)
}

// ListPlugins returns metadata about all registered plugins.
func (r *registry) ListPlugins() []engine.PluginInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	plugins := make([]engine.PluginInfo, 0, len(r.info))
	for _, info := range r.info {
		plugins = append(plugins, info)
	}
	return plugins
}

// Ensure registry implements engine.PluginRegistry.
var _ engine.PluginRegistry = (*registry)(nil)
