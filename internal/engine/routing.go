package engine

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"tresor/internal/store"
)

// resolveModel reads the request body, extracts the model name, and resolves
// the target downstream via alias lookup or direct output_model_id matching.
// Always returns a modelResult (may be partial on error) for entry population.
func (e *Engine) resolveModel(r *http.Request) (*modelResult, *gatewayError) {
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		return &modelResult{body: body}, &gatewayError{http.StatusInternalServerError, "failed to read body", "failed to read body", "failed to read body", err}
	}

	model := extractModel(body, r.URL.Path)
	if model == "" {
		return &modelResult{body: body}, &gatewayError{http.StatusBadRequest, "request body missing model", "request body missing model", "missing model", nil}
	}

	// Model resolution follows a strict priority (most specific first):
	//   1. Non-regex alias — a deliberately-named override for an exact model
	//      name. Wins over everything.
	//   2. Downstream model — the model is listed exactly in a downstream's
	//      output_model_ids. Wins over catch-all regex aliases.
	//   3. Regex alias — an announced name of a regex group, or a match against
	//      a regex group's catch-all pattern (e.g. "gpt-.*"). The fallback.
	// This ordering means a concrete name (whether a downstream model or a
	// named alias) is never shadowed by a broad regex catch-all, matching
	// sub2api's "exact key beats wildcard" and pi's disjoint exact/glob paths.

	// Tier 1: non-regex (exact) alias.
	groups, groupErr := e.store.ListGroups()
	if groupErr != nil {
		return &modelResult{model: model, body: body}, &gatewayError{500, "alias group lookup error", "internal error", "alias lookup error", groupErr}
	}
	for _, g := range groups {
		if g.InputModelID == model && !g.IsRegex && (g.RouteMode == "load_balance" || g.RouteMode == "fallback") {
			if result, ge := e.resolveRoutedGroup(r, body, model, g); result != nil || ge != nil {
				return result, ge
			}
		}
	}
	alias, err := e.store.FindActiveNonRegexAlias(model)
	if err != nil {
		return &modelResult{model: model, body: body}, &gatewayError{http.StatusInternalServerError, fmt.Sprintf("error looking up alias for model %s", model), "internal error", "alias lookup error", err}
	}
	if alias != nil {
		ds, derr := e.store.GetDownstream(alias.DownstreamID)
		if derr != nil {
			return &modelResult{model: model, body: body}, &gatewayError{http.StatusBadGateway, fmt.Sprintf("error getting downstream %s for alias %s", alias.DownstreamID, alias.ID),
				fmt.Sprintf("alias %q references missing downstream %q", alias.ID, alias.DownstreamID), "alias downstream missing", derr}
		}
		// A disabled downstream behaves "as if deleted": ignore the alias and
		// fall through to the other tiers.
		if ds.IsEnabled {
			e.logger.Debug("alias match (exact): model %q → alias %q → downstream %q (%s)", model, alias.ID, ds.ID, alias.OutputModelID)
			return &modelResult{ds: ds, alias: alias, model: model, resolvedModel: alias.OutputModelID, body: rewriteModelInBody(body, alias.OutputModelID)}, nil
		}
		e.logger.Debug("exact alias match skipped: downstream %q is disabled", ds.ID)
	}

	// Tier 2: direct downstream by exact output_model_id.
	ds, err := e.store.FindDownstreamByOutputModel(model)
	if err != nil {
		return &modelResult{model: model, body: body}, &gatewayError{http.StatusInternalServerError, fmt.Sprintf("error looking up downstream for model %s", model), "internal error", "downstream lookup error", err}
	}
	if ds != nil {
		e.logger.Debug("direct resolution: model %q → downstream %q (%s)", model, ds.ID, ds.Name)
		return &modelResult{ds: ds, model: model, resolvedModel: model, body: body}, nil
	}

	// Tier 3: regex (catch-all) alias. Announced names precede pattern matches.
	for pass := 0; pass < 2; pass++ {
		for _, g := range groups {
			if !g.IsRegex {
				continue
			}
			matched := slices.Contains(g.AnnouncedNames, model)
			if pass == 1 {
				re, err := regexp.Compile(g.InputModelID)
				matched = err == nil && re.MatchString(model)
			}
			if !matched {
				continue
			}
			if g.RouteMode == "load_balance" || g.RouteMode == "fallback" {
				if result, ge := e.resolveRoutedGroup(r, body, model, g); result != nil || ge != nil {
					return result, ge
				}
			} else {
				for _, a := range g.Options {
					if a.IsActive {
						ds, err := e.store.GetDownstream(a.DownstreamID)
						if err == nil && ds.IsEnabled {
							return &modelResult{ds: ds, alias: &a, model: model, resolvedModel: a.OutputModelID, body: rewriteModelInBody(body, a.OutputModelID)}, nil
						}
					}
				}
			}
		}
	}
	alias, err = e.store.FindActiveRegexAlias(model)
	if err != nil {
		return &modelResult{model: model, body: body}, &gatewayError{http.StatusInternalServerError, fmt.Sprintf("error looking up regex alias for model %s", model), "internal error", "alias lookup error", err}
	}
	if alias != nil {
		rds, derr := e.store.GetDownstream(alias.DownstreamID)
		if derr != nil {
			return &modelResult{model: model, body: body}, &gatewayError{http.StatusBadGateway, fmt.Sprintf("error getting downstream %s for alias %s", alias.DownstreamID, alias.ID),
				fmt.Sprintf("alias %q references missing downstream %q", alias.ID, alias.DownstreamID), "alias downstream missing", derr}
		}
		if rds.IsEnabled {
			e.logger.Debug("alias match (regex %q): model %q → downstream %q (%s)", alias.InputModelID, model, rds.ID, alias.OutputModelID)
			return &modelResult{ds: rds, alias: alias, model: model, resolvedModel: alias.OutputModelID, body: rewriteModelInBody(body, alias.OutputModelID)}, nil
		}
		e.logger.Debug("regex alias match skipped: downstream %q is disabled", rds.ID)
	}

	msg := fmt.Sprintf("unknown model %q", model)
	e.logger.Debug("model %q did not match any alias or downstream output_model_ids", model)
	return &modelResult{model: model, body: body}, &gatewayError{http.StatusNotFound, msg, msg, "unknown model", nil}
}

// buildPipeline constructs the transformation pipeline from matching rules and
// adds auto-translation transformers when input format differs from downstream format.
// Returns the pipeline and the list of matching rules (for logging).
func (e *Engine) buildPipeline(path, model string, inputFormat string, ds *store.Downstream, alias *store.Alias) (Pipeline, []store.Rule, *gatewayError) {
	registry := e.registry
	if snapshots, ok := registry.(interface{ Snapshot() PluginRegistry }); ok {
		registry = snapshots.Snapshot()
	}
	return e.buildPipelineWithRegistry(path, model, inputFormat, ds, alias, registry)
}
func (e *Engine) buildPipelineWithRegistry(path, model string, inputFormat string, ds *store.Downstream, alias *store.Alias, registry PluginRegistry) (Pipeline, []store.Rule, *gatewayError) {
	// Build the candidate model list for pattern_models matching.
	// A rule's pattern_models match if it equals ANY of these strings
	// (exact match). This makes rules useful whether the user keys them
	// on the raw incoming model, a downstream output_model_id, the alias
	// input_model_id, the regex pattern of a regex alias, or one of the
	// announced names of a regex alias.
	candidateModels := []string{model}
	if ds != nil {
		candidateModels = append(candidateModels, ds.OutputModelIDs...)
	}
	if alias != nil {
		candidateModels = append(candidateModels, alias.OutputModelID)
		if alias.IsRegex {
			candidateModels = append(candidateModels, alias.InputModelID) // the regex pattern itself
			candidateModels = append(candidateModels, alias.AnnouncedNames...)
		}
	}
	// De-duplicate and drop empty strings.
	seen := make(map[string]struct{}, len(candidateModels))
	uniq := candidateModels[:0]
	for _, m := range candidateModels {
		if m == "" {
			continue
		}
		if _, ok := seen[m]; ok {
			continue
		}
		seen[m] = struct{}{}
		uniq = append(uniq, m)
	}
	candidateModels = uniq

	// Find all matching rules (path+model+format filters)
	rules, err := e.store.FindMatchingRulesWithCandidates(path, candidateModels, inputFormat, ds.ID, ds.ApiFormats)
	if err != nil {
		return Pipeline{}, nil, &gatewayError{http.StatusInternalServerError, fmt.Sprintf("error finding rules: %v", err), "internal error", "rule lookup error", err}
	}

	// Concatenate all matching rules' pipelines (in priority order)
	var pipeline Pipeline
	for _, rule := range rules {
		p, err := ParsePipelineConfig(rule.PipelineConfig, registry)
		if err != nil {
			return Pipeline{}, nil, &gatewayError{http.StatusInternalServerError, fmt.Sprintf("error building pipeline for rule %s: %v", rule.ID, err), "pipeline error", "pipeline error", err}
		}
		for _, external := range p.ExternalSteps {
			external.ruleID = rule.ID
		}
		pipeline.UpstreamSteps = append(pipeline.UpstreamSteps, p.UpstreamSteps...)
		pipeline.ExternalSteps = append(pipeline.ExternalSteps, p.ExternalSteps...)
		pipeline.RequestSteps = append(pipeline.RequestSteps, p.RequestSteps...)
		pipeline.ResponseSteps = append(pipeline.ResponseSteps, p.ResponseSteps...)
		pipeline.StreamResponseSteps = append(pipeline.StreamResponseSteps, p.StreamResponseSteps...)
	}

	if len(rules) > 0 {
		e.logger.Debug("matched %d rule(s) for %s %s: %v", len(rules), path, model, func() []string {
			ids := make([]string, len(rules))
			for i, r := range rules {
				ids[i] = r.ID
			}
			return ids
		}())
	} else {
		e.logger.Debug("no rules matched for %s %s", path, model)
	}

	// Auto-translation: compare input format with downstream formats
	if inputFormat != "" && len(ds.ApiFormats) > 0 && !slices.Contains(ds.ApiFormats, inputFormat) {
		var pluginID string
		switch inputFormat {
		case "openai":
			switch {
			case slices.Contains(ds.ApiFormats, "openai_responses"):
				pluginID = "openai2responses"
			case slices.Contains(ds.ApiFormats, "gemini"):
				pluginID = "openai2gemini"
			default:
				pluginID = "openai2anthropic"
			}
		case "anthropic":
			switch {
			case slices.Contains(ds.ApiFormats, "openai_responses"):
				pluginID = "anthropic2responses"
			case slices.Contains(ds.ApiFormats, "gemini"):
				pluginID = "anthropic2gemini"
			default:
				pluginID = "anthropic2openai"
			}
		case "openai_responses":
			if slices.Contains(ds.ApiFormats, "openai") {
				pluginID = "responses2openai"
			} else if slices.Contains(ds.ApiFormats, "anthropic") {
				pluginID = "responses2anthropic"
			} else if slices.Contains(ds.ApiFormats, "gemini") {
				pluginID = "responses2openai"
			}
		case "gemini":
			switch {
			case slices.Contains(ds.ApiFormats, "openai"):
				pluginID = "gemini2openai"
			case slices.Contains(ds.ApiFormats, "anthropic"):
				pluginID = "gemini2anthropic"
			case slices.Contains(ds.ApiFormats, "openai_responses"):
				pluginID = "gemini2responses"
			}
		}
		if pluginID != "" {
			ids := []string{pluginID}
			if inputFormat == "openai_responses" && pluginID == "responses2openai" && !slices.Contains(ds.ApiFormats, "openai") {
				ids = append(ids, "openai2gemini")
			}
			// Walk backwards so requests are prepended in forward order, while
			// buffered/SSE responses are appended in inverse conversion order.
			for i := len(ids) - 1; i >= 0; i-- {
				pluginID := ids[i]
				transformer, err := registry.CreatePlugin(pluginID, nil)
				if err != nil {
					log.Printf("Error creating auto-translation plugin %s: %v", pluginID, err)
				} else {
					name := transformerTypeName(transformer)
					if reqT, ok := transformer.(RequestTransformer); ok && !pluginInList[RequestTransformer](pipeline.RequestSteps, name) {
						pipeline.RequestSteps = append([]RequestTransformer{reqT}, pipeline.RequestSteps...)
					}
					if respT, ok := transformer.(ResponseTransformer); ok && !pluginInList[ResponseTransformer](pipeline.ResponseSteps, name) {
						pipeline.ResponseSteps = append(pipeline.ResponseSteps, respT)
					}
					if streamT, ok := transformer.(StreamResponseTransformer); ok && !pluginInList[StreamResponseTransformer](pipeline.StreamResponseSteps, name) {
						pipeline.StreamResponseSteps = append(pipeline.StreamResponseSteps, streamT)
					}
					e.logger.Debug("auto-translating %s → downstream %s (formats: %v)", inputFormat, ds.ID, ds.ApiFormats)
				}
			}
		}
	}

	return pipeline, rules, nil
}

// pluginInList checks if a transformer with the given type name is already in the pipeline.
func pluginInList[T any](transformers []T, typeName string) bool {
	for _, t := range transformers {
		if transformerTypeName(t) == typeName {
			return true
		}
	}
	return false
}

func transformerTypeName(t interface{}) string {
	if namer, ok := t.(PluginNamer); ok {
		return namer.PluginName()
	}
	typ := reflect.TypeOf(t)
	if typ == nil {
		return ""
	}
	if typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}
	return typ.Name()
}
