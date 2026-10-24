package jsplugins

import (
	"fmt"
	"tresor/internal/integrations"
)

// ServiceDeclaration references top-level parameter fields, never global settings.
type ServiceDeclaration struct {
	BaseURLParam      string                            `json:"base_url_param"`
	TokenParam        string                            `json:"token_param,omitempty"`
	ControlTokenParam string                            `json:"control_token_param,omitempty"`
	Operations        map[integrations.Operation]string `json:"operations"`
}

func validateServiceDeclarations(m Manifest) error {
	if len(m.Services) > 16 {
		return fmt.Errorf("too many service declarations")
	}
	if len(m.Services) > 0 && m.APIVersion != 3 {
		return fmt.Errorf("services require API V3")
	}
	props, _ := m.ConfigSchema["properties"].(map[string]any)
	for name, decl := range m.Services {
		if !identity.MatchString(name) {
			return fmt.Errorf("invalid local service name")
		}
		if decl.BaseURLParam == "" {
			return fmt.Errorf("service base_url_param required")
		}
		for _, field := range []string{decl.BaseURLParam, decl.TokenParam, decl.ControlTokenParam} {
			if field == "" {
				continue
			}
			s, ok := props[field].(map[string]any)
			if !ok || s["type"] != "string" {
				return fmt.Errorf("service parameter must reference string property")
			}
			if field != decl.BaseURLParam && s["secret"] != true && s["format"] != "password" {
				return fmt.Errorf("credential parameter must be marked secret or password")
			}
		}
		service := integrations.Service{BaseURL: "https://example.invalid", Operations: decl.Operations}
		if err := service.Validate(); err != nil {
			return fmt.Errorf("invalid service declaration: %w", err)
		}
		for op := range decl.Operations {
			if !contains(m.Permissions, "service."+string(op)) {
				return fmt.Errorf("service operation missing manifest capability")
			}
		}
	}
	return nil
}

// ServiceConfigs resolves this instance's detached configuration into host-only connections.
func (i *Instance) ServiceConfigs() map[string]ServiceConnection {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.serviceConfigs()
}
func (i *Instance) serviceConfigs() map[string]ServiceConnection {
	result := map[string]ServiceConnection{}
	for name, decl := range i.pkg.Manifest.Services {
		base, _ := i.config[decl.BaseURLParam].(string)
		token, _ := i.config[decl.TokenParam].(string)
		control, _ := i.config[decl.ControlTokenParam].(string)
		ops := map[integrations.Operation]string{}
		for op, path := range decl.Operations {
			ops[op] = path
		}
		result[name] = ServiceConnection{Service: integrations.Service{BaseURL: base, Operations: ops}, Token: token, ControlToken: control}
	}
	return result
}

type ServiceConnection struct {
	Service             integrations.Service
	Token, ControlToken string
}
