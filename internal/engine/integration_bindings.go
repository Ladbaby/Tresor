package engine

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// configuredBindings enumerates enabled rule steps using one package snapshot.
func (e *Engine) configuredBindings() (map[string]*integrations.Snapshot, error) {
	if e.store == nil {
		return nil, fmt.Errorf("integration binding store unavailable")
	}
	rules, err := e.store.ListRules()
	if err != nil {
		return nil, err
	}
	registry := e.registry
	if source, ok := registry.(interface{ Snapshot() PluginRegistry }); ok {
		registry = source.Snapshot()
	}
	if registry == nil {
		return nil, fmt.Errorf("integration plugin registry unavailable")
	}
	result := map[string]*integrations.Snapshot{}
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
			var clients map[string]*integrations.Snapshot
			if source, ok := registry.(interface {
				ServiceConnections(string, map[string]interface{}) (map[string]jsplugins.ServiceConnection, error)
			}); ok {
				configs, err := source.ServiceConnections(step.PluginID, step.Config)
				if err != nil {
					continue
				}
				clients = map[string]*integrations.Snapshot{}
				for name, cfg := range configs {
					if cfg.ControlToken != "" && cfg.ControlToken == cfg.Token {
						return nil, fmt.Errorf("control and outbound credentials must differ")
					}
					client, err := integrations.NewConfiguredClient(cfg.Service, cfg.Token, cfg.ControlToken, externalValueLimit)
					if err != nil {
						return nil, err
					}
					clients[name] = client
				}
			} else {
				plugin, err := registry.CreatePlugin(step.PluginID, step.Config)
				if err != nil {
					continue
				}
				p, ok := plugin.(*ExternalPlugin)
				if !ok {
					continue
				}
				p.ruleID, p.stepID = rule.ID, step.ID
				clients, err = p.configuredServices()
				if err != nil {
					return nil, err
				}
			}
			for name, client := range clients {
				binding := stepServiceBinding(rule.ID, step.ID, name)
				if _, exists := result[binding]; exists {
					result[binding] = nil
				} else {
					result[binding] = client
				}
			}
		}
	}
	return result, nil
}

// configuredBinding resolves only enabled rule steps using one package snapshot.
func (e *Engine) configuredBinding(binding string) *integrations.Snapshot {
	bindings, err := e.configuredBindings()
	if err != nil {
		return nil
	}
	return bindings[binding]
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

// HandleIntegrationBindingResolve returns the configured binding identifier for a pair of credentials.
func (e *Engine) HandleIntegrationBindingResolve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path != "/api/integrations/bindings/resolve" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		integrationError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	var input struct {
		ServiceToken string `json:"service_token"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || input.ServiceToken == "" || len(input.ServiceToken) > 4096 {
		integrationError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		integrationError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	control := bearerToken(r)
	if control == "" {
		integrationError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	bindings, err := e.configuredBindings()
	if err != nil {
		integrationError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	matched := ""
	count := 0
	for id, service := range bindings {
		if service == nil {
			continue
		}
		a, b := service.ControlCredential(), control
		controlMatch := a != "" && len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
		a, b = service.Credential(), input.ServiceToken
		serviceMatch := a != "" && len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
		if controlMatch && serviceMatch {
			matched = id
			count++
		}
	}
	if count == 0 {
		integrationError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if count != 1 {
		integrationError(w, http.StatusConflict, "ambiguous_binding")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		BindingID string `json:"binding_id"`
	}{matched})
}
