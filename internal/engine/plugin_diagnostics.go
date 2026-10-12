package engine

import (
	"errors"
	"tresor/internal/jsplugins"
)

// Hook failures remain part of request termination, not diagnostic telemetry.
func (p *ExternalPlugin) invocationFailure(hook string, err error) error {
	if err == nil {
		return nil
	}
	var fault *jsplugins.InvocationError
	if !errors.As(err, &fault) {
		err = &jsplugins.InvocationError{Code: "invalid_effect", Plugin: p.PluginName(), Hook: hook, Cause: err}
	}
	if p.ctx != nil {
		p.ctx.recordFailure(err)
	}
	return err
}
