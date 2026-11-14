package plugins

// Token accounting translation between the four wire formats Tresor speaks.
//
// The formats disagree about what the prompt-token number means, and getting the
// direction wrong silently corrupts billing displays:
//
//   - Chat Completions / Responses API: prompt_tokens (input_tokens) is the TOTAL
//     prompt size, cache hits included. Cached tokens are reported separately
//     under prompt_tokens_details.cached_tokens / input_tokens_details.cached_tokens
//     as a subset of that total.
//   - Anthropic: input_tokens is only the FRESH prompt; cache reads
//     (cache_read_input_tokens) and cache writes (cache_creation_input_tokens) are
//     reported alongside it and are not part of input_tokens.
//   - Gemini: promptTokenCount is the total, with cachedContentTokenCount as the
//     cached subset.
//
// So OpenAI->Anthropic must SUBTRACT the cached portion, and Anthropic->OpenAI
// must ADD it back. Every helper below states which convention it produces.
//
// These helpers are deliberately separate from engine/cache.go's usageFromMap,
// which is a permissive scraper for the Logs tab and normalises everything to
// Anthropic semantics.

// intPtr returns a pointer to v, for the optional detail counters.
func intPtr(v int) *int { return &v }

// openAIUsageToResponses converts a Chat Completions usage block into the
// Responses API `usage` object.
//
// Responses semantics match Chat Completions here: input_tokens is the TOTAL
// prompt size (cache included), so prompt_tokens is copied across unchanged.
// Detail counters are emitted only when the upstream actually reported them.
// A nil u yields a zero-valued usage object, which strict clients require to be
// present rather than absent.
func openAIUsageToResponses(u *openAIChunkUsage) map[string]any {
	usage := map[string]any{
		"input_tokens":  0,
		"output_tokens": 0,
		"total_tokens":  0,
	}
	if u == nil {
		return usage
	}
	usage["input_tokens"] = u.PromptTokens
	usage["output_tokens"] = u.CompletionTokens
	usage["total_tokens"] = u.TotalTokens
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens != nil {
		usage["input_tokens_details"] = map[string]any{
			"cached_tokens": *u.PromptTokensDetails.CachedTokens,
		}
	}
	if u.CompletionTokensDetails != nil && u.CompletionTokensDetails.ReasoningTokens != nil {
		usage["output_tokens_details"] = map[string]any{
			"reasoning_tokens": *u.CompletionTokensDetails.ReasoningTokens,
		}
	}
	return usage
}

// openAIUsageToGemini converts a Chat Completions usage block into Gemini's
// usageMetadata object. Gemini, like OpenAI, counts the whole prompt in
// promptTokenCount, so the cached count is carried as a subset.
// Returns nil when there is nothing to report, so no empty usageMetadata is
// invented for a response that never had one.
func openAIUsageToGemini(u *openAIChunkUsage) *geminiUsageMetadata {
	if u == nil {
		return nil
	}
	meta := &geminiUsageMetadata{
		PromptTokenCount:     u.PromptTokens,
		CandidatesTokenCount: u.CompletionTokens,
		TotalTokenCount:      u.TotalTokens,
	}
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens != nil {
		meta.CachedContentTokenCount = *u.PromptTokensDetails.CachedTokens
	}
	return meta
}

// openAIUsageToAnthropic converts a Chat Completions usage block into Anthropic's
// fresh-input convention. The cached portion is subtracted from prompt_tokens so
// input_tokens counts only newly processed tokens, and it is returned separately
// so the caller can emit cache_read_input_tokens.
//
// A malformed upstream (cached > total) is passed through as-is rather than
// producing a negative count — preserving the reported total is safer than
// inventing a nonsensical one.
func openAIUsageToAnthropic(u *openAIChunkUsage) (inputTokens, outputTokens, cacheReadTokens int) {
	if u == nil {
		return 0, 0, 0
	}
	inputTokens = u.PromptTokens
	outputTokens = u.CompletionTokens
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens != nil {
		cached := *u.PromptTokensDetails.CachedTokens
		cacheReadTokens = cached
		if cached >= 0 && cached <= inputTokens {
			inputTokens -= cached
		}
	}
	return inputTokens, outputTokens, cacheReadTokens
}

// anthropicUsageToOpenAI converts Anthropic's fresh-input usage into Chat
// Completions totals by adding the cached portion back into prompt_tokens, so the
// result follows the OpenAI convention that the prompt total includes cache hits.
// Returns nil when the upstream reported nothing, leaving usage absent rather
// than fabricating zeroes.
func anthropicUsageToOpenAI(inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens int) *openAIChunkUsage {
	if inputTokens == 0 && outputTokens == 0 && cacheReadTokens == 0 && cacheCreationTokens == 0 {
		return nil
	}
	total := inputTokens + cacheReadTokens
	usage := &openAIChunkUsage{
		PromptTokens:     total,
		CompletionTokens: outputTokens,
		TotalTokens:      total + outputTokens,
	}
	if cacheReadTokens > 0 || cacheCreationTokens > 0 {
		usage.PromptTokensDetails = &openAITokenDetails{CachedTokens: intPtr(cacheReadTokens)}
	}
	return usage
}

// geminiUsageToAnthropic converts Gemini's usageMetadata into Anthropic's
// fresh-input convention. Gemini's promptTokenCount includes cache hits, so the
// cached count is subtracted to produce input_tokens and returned separately as
// cacheReadTokens. A nil meta yields all zeroes.
func geminiUsageToAnthropic(meta *geminiUsageMetadata) (inputTokens, outputTokens, cacheReadTokens int) {
	if meta == nil {
		return 0, 0, 0
	}
	inputTokens = meta.PromptTokenCount
	outputTokens = meta.CandidatesTokenCount
	if meta.CachedContentTokenCount > 0 {
		cacheReadTokens = meta.CachedContentTokenCount
		if meta.CachedContentTokenCount <= inputTokens {
			inputTokens -= meta.CachedContentTokenCount
		}
	}
	return inputTokens, outputTokens, cacheReadTokens
}

// geminiUsageToOpenAI converts Gemini's usageMetadata into Chat Completions
// totals. Gemini's promptTokenCount already includes cache hits, matching the
// OpenAI convention, so no arithmetic is needed.
// Returns nil when there is no metadata, keeping usage absent rather than zeroed.
func geminiUsageToOpenAI(meta *geminiUsageMetadata) *openAIChunkUsage {
	if meta == nil {
		return nil
	}
	usage := &openAIChunkUsage{
		PromptTokens:     meta.PromptTokenCount,
		CompletionTokens: meta.CandidatesTokenCount,
		TotalTokens:      meta.TotalTokenCount,
	}
	if meta.CachedContentTokenCount > 0 {
		usage.PromptTokensDetails = &openAITokenDetails{
			CachedTokens: intPtr(meta.CachedContentTokenCount),
		}
	}
	return usage
}
