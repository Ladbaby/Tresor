package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"tresor/internal/integrations"
	"tresor/internal/jsplugins"
)

func (s *integrationSnapshot) serviceFor(p *ExternalPlugin, name string) *integrations.Snapshot {
	if p.services != nil {
		return p.services[name]
	}
	if s == nil {
		return nil
	}
	return s.services[name]
}

// configuredBinding resolves only enabled rule steps using one package snapshot.
func (e *Engine) configuredBinding(binding string) *integrations.Snapshot {
	rules, err := e.store.ListRules()
	if err != nil {
		return nil
	}
	registry := e.registry
	if source, ok := registry.(interface{ Snapshot() PluginRegistry }); ok {
		registry = source.Snapshot()
	}
	if registry == nil {
		return nil
	}
	var found *integrations.Snapshot
	for _, rule := range rules {
		if !rule.IsEnabled {
			continue
		}
		var steps []PipelineStepConfig
		if json.Unmarshal([]byte(rule.PipelineConfig), &steps) != nil {
			continue
		}
		for _, step := range steps {
			if step.ID == "" {
				continue
			}
			plugin, err := registry.CreatePlugin(step.PluginID, step.Config)
			if err != nil {
				continue
			}
			p, ok := plugin.(*ExternalPlugin)
			if !ok {
				continue
			}
			p.ruleID, p.stepID = rule.ID, step.ID
			clients, err := p.configuredServices()
			if err != nil {
				continue
			}
			for name, client := range clients {
				if stepServiceBinding(rule.ID, step.ID, name) == binding {
					if found != nil {
						return nil
					}
					found = client
				}
			}
		}
	}
	return found
}

func (p *ExternalPlugin) executionService(name string) string {
	if p.services != nil {
		return stepServiceBinding(p.ruleID, p.stepID, name)
	}
	return name
}

func stepServiceBinding(rule, step, service string) string {
	sum := sha256.Sum256([]byte(rule + "\x00" + step + "\x00" + service))
	return hex.EncodeToString(sum[:])
}

// configuredServices constructs connections from one detached rule-step instance.
func (p *ExternalPlugin) configuredServices() (map[string]*integrations.Snapshot, error) {
	source, ok := p.runtime.(interface {
		ServiceConfigs() map[string]jsplugins.ServiceConnection
	})
	if !ok {
		return nil, nil
	}
	result := map[string]*integrations.Snapshot{}
	for name, cfg := range source.ServiceConfigs() {
		if cfg.ControlToken != "" && cfg.ControlToken == cfg.Token {
			return nil, fmt.Errorf("control and outbound credentials must differ")
		}
		client, err := integrations.NewConfiguredClient(cfg.Service, cfg.Token, cfg.ControlToken, externalValueLimit)
		if err != nil {
			return nil, fmt.Errorf("invalid service configuration for step %s", p.stepID)
		}
		result[name] = client
	}
	return result, nil
}
