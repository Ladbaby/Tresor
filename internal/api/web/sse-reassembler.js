// sse-reassembler.js — Port of claude-tap's SSEReassembler to JavaScript.
//
// Streams the four major LLM API formats into a single reconstructed
// response object the inspector's "Parsed" view can render without
// showing the user a wall of `data: {...}` lines:
//
//   - OpenAI Chat Completions: bare `data: {choices[].delta.content}`
//     frames, with `delta.tool_calls[].function.arguments` accumulated
//     token-by-token. Termination on `data: [DONE]`.
//   - OpenAI Responses: explicit event types (response.created,
//     response.output_item.added, response.output_text.delta,
//     response.output_item.done, response.completed, response.error).
//   - Anthropic Messages: explicit event types (message_start,
//     content_block_start, content_block_delta, content_block_stop,
//     message_delta, message_stop).
//   - Gemini streamGenerateContent: bare `data: {candidates[].content.parts[]}`
//     frames with text accumulated by position.
//
// Why port to JS rather than do it in Go? The engine already has the
// format-agnostic responsibility of forwarding bytes. Parsing belongs
// alongside rendering — both live in the Web UI and the renderer can
// react to a single canonical shape. The Go side stays oblivious to
// vendor formats.
//
// The reassembler also exposes normalizeRequest(body, format) which
// converts any of the four formats' request bodies into a canonical
// message list: {system?: string, messages: [{role, content, ...}]}.

(function (global) {
    'use strict';

    // ---------- helpers ----------

    function isDict(v) {
        return v && typeof v === 'object' && !Array.isArray(v);
    }

    function deepCopy(v) {
        // JSON round-trip is fine: all four formats are JSON-serialisable.
        return v === undefined ? undefined : JSON.parse(JSON.stringify(v));
    }

    function normalizeUsage(u) {
        // Map every shape to {input_tokens, output_tokens} so the
        // inspector has a single field to render regardless of format.
        if (!isDict(u)) return null;
        // Anthropic: input_tokens / output_tokens.
        // OpenAI: prompt_tokens / completion_tokens (plus cached_tokens).
        // Gemini: promptTokenCount / candidatesTokenCount / cachedContentTokenCount.
        const input = u.input_tokens != null ? u.input_tokens
                   : u.prompt_tokens != null ? u.prompt_tokens
                   : u.promptTokenCount != null ? u.promptTokenCount
                   : null;
        const output = u.output_tokens != null ? u.output_tokens
                    : u.completion_tokens != null ? u.completion_tokens
                    : u.candidatesTokenCount != null ? u.candidatesTokenCount
                    : null;
        const cached = u.cache_read_input_tokens != null ? u.cache_read_input_tokens
                    : u.cached_tokens != null ? u.cached_tokens
                    : u.cachedContentTokenCount != null ? u.cachedContentTokenCount
                    : 0;
        if (input == null && output == null) return null;
        const out = { input_tokens: input || 0, output_tokens: output || 0, cache_read_input_tokens: cached };
        // Preserve the raw input so message_delta events that report only
        // one side don't clobber the side already known from message_start.
        if (input != null) out.input_tokens = input;
        if (output != null) out.output_tokens = output;
        if (cached) out.cache_read_input_tokens = cached;
        return out;
    }

    // ---------- SSE parser ----------

    // SSEReassembler: feed it raw bytes; pull out the reconstructed response.
    // Format is auto-detected from the first non-empty event's shape.
    class SSEReassembler {
        constructor() {
            this._buf = '';
            this._currentEvent = null;
            this._currentData = [];
            this._snapshot = null;
            this.events = [];
            // Format detection cache. Once we see the first data line we
            // lock in the format, because each protocol uses different
            // frame structure.
            this._format = null;
        }

        feed(chunk) {
            this._buf += chunk;
            let nl;
            // Process one line at a time. The buf can hold partial
            // frames between calls; we keep what's left over.
            while ((nl = this._buf.indexOf('\n')) !== -1) {
                const line = this._buf.slice(0, nl);
                this._buf = this._buf.slice(nl + 1);
                this._feedLine(line.replace(/\r$/, ''));
            }
        }

        _feedLine(line) {
            if (line.startsWith('event:')) {
                this._currentEvent = line.slice(6).trim();
                this._currentData = [];
                return;
            }
            if (line.startsWith('data:')) {
                // data: prefix can be followed by a single space (per the
                // SSE spec) or not, depending on the implementation. We
                // trim the leading space if present.
                let rest = line.slice(5);
                if (rest.startsWith(' ')) rest = rest.slice(1);
                this._currentData.push(rest);
                return;
            }
            if (line === '') {
                if (this._currentEvent !== null || this._currentData.length > 0) {
                    this._emitEvent();
                }
            }
            // Comment lines (starting with ':') are ignored per SSE spec.
        }

        _emitEvent() {
            const raw = this._currentData.join('\n');
            // OpenAI Chat Completions terminator: "data: [DONE]". Skip
            // these so they don't pollute the events list.
            if (raw === '[DONE]' && this._currentEvent === null) {
                this._currentEvent = null;
                this._currentData = [];
                return;
            }

            let data;
            try {
                data = JSON.parse(raw);
            } catch (e) {
                data = raw;
            }

            const eventType = this._currentEvent || 'message';
            this.events.push({ event: eventType, data: data });
            this._accumulate(eventType, data);

            this._currentEvent = null;
            this._currentData = [];
        }

        _accumulate(eventType, data) {
            if (!isDict(data)) return;
            if (this._format === null) this._detectFormat(eventType, data);
            switch (this._format) {
                case 'chat_completions': this._accumulateChat(data); break;
                case 'responses':        this._accumulateResponses(eventType, data); break;
                case 'anthropic':        this._accumulateAnthropic(eventType, data); break;
                case 'gemini':           this._accumulateGemini(data); break;
                default:
                    // Unknown format: keep going, but reconstruction stops.
                    break;
            }
        }

        _detectFormat(eventType, data) {
            // Anthropic events: message_start, content_block_start, etc.
            if (['message_start', 'content_block_start', 'content_block_delta',
                 'content_block_stop', 'message_delta', 'message_stop'].indexOf(eventType) !== -1) {
                this._format = 'anthropic';
                return;
            }
            // OpenAI Responses events: response.created, response.output_text.delta, etc.
            if (['response.created', 'response.output_item.added',
                 'response.output_item.done', 'response.output_text.delta',
                 'response.completed', 'response.incomplete', 'response.failed',
                 'response.error'].indexOf(eventType) !== -1) {
                this._format = 'responses';
                return;
            }
            // OpenAI Chat Completions: data frame has `choices` and `delta`.
            if (data.choices && Array.isArray(data.choices) && data.choices[0] && data.choices[0].delta) {
                this._format = 'chat_completions';
                return;
            }
            // Gemini: candidates or usageMetadata at the top level.
            if (data.candidates || data.usageMetadata) {
                this._format = 'gemini';
                return;
            }
            // Don't lock yet — keep accumulating until we see a recognisable
            // shape. Unrecognised streams fall through to the "raw" view.
        }

        // ---- Chat Completions ----

        _accumulateChat(data) {
            const choices = data.choices || [];
            const usage = data.usage;
            if (choices.length === 0) {
                if (isDict(usage) && this._snapshot) this._mergeUsage(usage);
                return;
            }
            const choice = choices[0] || {};
            const delta = choice.delta || {};
            const finishReason = choice.finish_reason;
            const choiceUsage = choice.usage;

            if (!this._snapshot) {
                this._snapshot = {
                    id: data.id || '',
                    object: 'chat.completion',
                    model: data.model || '',
                    choices: [{
                        index: 0,
                        message: { role: delta.role || 'assistant', content: '' },
                        finish_reason: null,
                    }],
                    // Anthropic-shape mirror so the viewer's render path
                    // can stay format-agnostic.
                    content: [],
                };
            }

            const msg = this._snapshot.choices[0].message;
            if (typeof delta.role === 'string' && delta.role) msg.role = delta.role;

            if (typeof delta.content === 'string' && delta.content) {
                msg.content = (msg.content || '') + delta.content;
                this._appendTextBlock(this._snapshot.content, delta.content);
            }
            // Reasoning: o1/o3 / DeepSeek-R1 style `reasoning_content`.
            for (const key of ['reasoning_content', 'reasoning']) {
                if (typeof delta[key] === 'string' && delta[key]) {
                    msg[key] = (msg[key] || '') + delta[key];
                    this._appendThinkingBlock(this._snapshot.content, delta[key]);
                }
            }
            // Tool calls: each carries an index, an id, a function name,
            // and an arguments string. The arguments stream token by token
            // and must be concatenated, not replaced.
            for (const tcDelta of (delta.tool_calls || [])) {
                if (!isDict(tcDelta)) continue;
                const idx = tcDelta.index || 0;
                msg.tool_calls = msg.tool_calls || [];
                while (msg.tool_calls.length <= idx) {
                    msg.tool_calls.push({ id: '', type: 'function', function: { name: '', arguments: '' } });
                }
                const tc = msg.tool_calls[idx];
                if (typeof tcDelta.id === 'string') tc.id = tcDelta.id;
                if (typeof tcDelta.type === 'string') tc.type = tcDelta.type;
                const fnDelta = tcDelta.function || {};
                if (typeof fnDelta.name === 'string' && fnDelta.name) {
                    tc.function.name = (tc.function.name || '') + fnDelta.name;
                }
                if (typeof fnDelta.arguments === 'string' && fnDelta.arguments) {
                    tc.function.arguments = (tc.function.arguments || '') + fnDelta.arguments;
                }
                this._mirrorToolCallToContent(this._snapshot.content, idx, tc);
            }

            if (finishReason) this._snapshot.choices[0].finish_reason = finishReason;
            if (isDict(usage)) this._mergeUsage(usage);
            if (isDict(choiceUsage)) this._mergeUsage(choiceUsage);
        }

        _appendTextBlock(blocks, text) {
            const last = blocks[blocks.length - 1];
            if (last && last.type === 'text') last.text += text;
            else blocks.push({ type: 'text', text: text });
        }

        _appendThinkingBlock(blocks, text) {
            const last = blocks[blocks.length - 1];
            if (last && last.type === 'thinking') last.thinking += text;
            else blocks.unshift({ type: 'thinking', thinking: text });
        }

        _mirrorToolCallToContent(blocks, idx, tc) {
            // Find the tool_use block at slot (offset = 1 thinking + 1 text) + idx.
            const offset = blocks.filter(b => b.type === 'thinking').length
                         + blocks.filter(b => b.type === 'text').length
                         + idx;
            while (blocks.length <= offset) {
                blocks.push({ type: 'tool_use', id: '', name: '', input: {} });
            }
            const block = blocks[offset];
            if (tc.id) block.id = tc.id;
            if (tc.function && tc.function.name) block.name = tc.function.name;
            const argsStr = (tc.function && tc.function.arguments) || '';
            if (argsStr) {
                try { block.input = JSON.parse(argsStr); }
                catch (e) { /* still streaming, leave the previously parsed input */ }
            }
        }

        _mergeUsage(usage) {
            const normalised = normalizeUsage(usage);
            if (!normalised) return;
            this._snapshot.usage = Object.assign({}, this._snapshot.usage || {}, normalised);
        }

        // ---- OpenAI Responses ----

        _accumulateResponses(eventType, data) {
            if (eventType === 'response.created' || eventType === 'response.in_progress') {
                const response = data.response;
                if (isDict(response)) this._snapshot = deepCopy(response);
                return;
            }
            if (eventType === 'response.output_item.added' || eventType === 'response.output_item.done') {
                this._accumulateResponsesOutputItem(data);
                return;
            }
            if (eventType === 'response.output_text.delta') {
                this._accumulateResponsesOutputText(data);
                return;
            }
            if (eventType === 'response.completed' || eventType === 'response.done' || eventType === 'response.incomplete' || eventType === 'response.failed') {
                this._mergeResponsesTerminal(data);
                return;
            }
            if (eventType === 'response.error') {
                this._recordResponsesError(data);
                return;
            }
        }

        _ensureResponsesOutput() {
            if (!this._snapshot) this._snapshot = { output: [] };
            if (!Array.isArray(this._snapshot.output)) this._snapshot.output = [];
            return this._snapshot.output;
        }

        _accumulateResponsesOutputItem(data) {
            const item = data.item;
            if (!isDict(item)) return;
            const output = this._ensureResponsesOutput();
            const idx = (typeof data.output_index === 'number' && data.output_index >= 0) ? data.output_index : output.length;
            while (output.length <= idx) output.push({});
            output[idx] = deepCopy(item);
        }

        _accumulateResponsesOutputText(data) {
            if (typeof data.delta !== 'string' || !data.delta) return;
            const output = this._ensureResponsesOutput();
            const idx = (typeof data.output_index === 'number' && data.output_index >= 0) ? data.output_index : output.length - 1;
            if (idx < 0 || idx >= output.length) return;
            const item = output[idx];
            if (!isDict(item)) return;
            if (!Array.isArray(item.content)) item.content = [];
            let part = item.content.find(c => isDict(c) && c.type === 'output_text');
            if (!part) {
                part = { type: 'output_text', text: '' };
                item.content.push(part);
            }
            part.text = (part.text || '') + data.delta;
        }

        _mergeResponsesTerminal(data) {
            const response = data.response;
            if (!isDict(response)) {
                this._snapshot = deepCopy(data);
                return;
            }
            const merged = deepCopy(response);
            const accumulated = (this._snapshot && Array.isArray(this._snapshot.output)) ? this._snapshot.output : null;
            if ((!merged.output || merged.output.length === 0) && accumulated && accumulated.length) {
                merged.output = accumulated;
            }
            this._snapshot = merged;
        }

        _recordResponsesError(data) {
            if (!this._snapshot) this._snapshot = { output: [] };
            const error = {};
            for (const k of ['code', 'message', 'param']) {
                if (data[k] !== undefined) error[k] = data[k];
            }
            this._snapshot.error = (Object.keys(error).length) ? error : deepCopy(data);
            if (['queued', 'in_progress', undefined, null, ''].indexOf(this._snapshot.status) !== -1) {
                this._snapshot.status = 'failed';
            }
        }

        // ---- Anthropic ----

        _accumulateAnthropic(eventType, data) {
            if (eventType === 'message_start') {
                if (isDict(data.message)) this._snapshot = deepCopy(data.message);
                return;
            }
            if (eventType === 'content_block_start') {
                if (!this._snapshot) this._snapshot = { content: [] };
                if (!Array.isArray(this._snapshot.content)) this._snapshot.content = [];
                const idx = (typeof data.index === 'number') ? data.index : this._snapshot.content.length;
                const block = isDict(data.content_block) ? deepCopy(data.content_block) : {};
                while (this._snapshot.content.length <= idx) this._snapshot.content.push({});
                this._snapshot.content[idx] = block;
                return;
            }
            if (eventType === 'content_block_delta') {
                if (!this._snapshot) return;
                if (!Array.isArray(this._snapshot.content)) this._snapshot.content = [];
                const idx = (typeof data.index === 'number' && data.index >= 0) ? data.index : 0;
                const delta = data.delta || {};
                while (this._snapshot.content.length <= idx) this._snapshot.content.push({});
                const block = this._snapshot.content[idx];
                if (!isDict(block)) return;
                if (delta.type === 'text_delta' && typeof delta.text === 'string') {
                    block.text = (block.text || '') + delta.text;
                    block.type = block.type || 'text';
                } else if (delta.type === 'thinking_delta' && typeof delta.thinking === 'string') {
                    block.thinking = (block.thinking || '') + delta.thinking;
                    block.type = block.type || 'thinking';
                } else if (delta.type === 'input_json_delta' && typeof delta.partial_json === 'string') {
                    block._partial_json = (block._partial_json || '') + delta.partial_json;
                    block.type = block.type || 'tool_use';
                } else if (delta.type === 'signature_delta' && typeof delta.signature === 'string') {
                    block.signature = (block.signature || '') + delta.signature;
                }
                return;
            }
            if (eventType === 'content_block_stop') {
                if (!this._snapshot || !Array.isArray(this._snapshot.content)) return;
                const idx = (typeof data.index === 'number' && data.index >= 0) ? data.index : 0;
                const block = this._snapshot.content[idx];
                if (isDict(block) && typeof block._partial_json === 'string') {
                    try { block.input = JSON.parse(block._partial_json); }
                    catch (e) { /* keep partial */ }
                    delete block._partial_json;
                }
                return;
            }
            if (eventType === 'message_delta') {
                if (!this._snapshot) return;
                const delta = data.delta || {};
                for (const k of Object.keys(delta)) this._snapshot[k] = delta[k];
                if (isDict(data.usage)) {
                    if (!isDict(this._snapshot.usage)) this._snapshot.usage = {};
                    const norm = normalizeUsage(data.usage);
                    if (norm) {
                        // Merge usage as sparsely as possible:
                        //  - The new usage's known fields overwrite the snapshot's
                        //  - We only overwrite existing non-zero values when the
                        //    new usage reports a non-zero value for the same side.
                        //    This prevents message_delta's output-only update from
                        //    resetting input_tokens to 0.
                        const merged = Object.assign({}, this._snapshot.usage || {}, norm);
                        for (const k of ['input_tokens', 'output_tokens', 'cache_read_input_tokens']) {
                            if ((merged[k] === 0 || merged[k] == null) &&
                                this._snapshot.usage &&
                                this._snapshot.usage[k] != null &&
                                this._snapshot.usage[k] !== 0) {
                                merged[k] = this._snapshot.usage[k];
                            }
                        }
                        this._snapshot.usage = merged;
                    }
                }
                return;
            }
            // message_stop, ping, etc.: no-op for the snapshot.
        }

        // ---- Gemini ----

        _accumulateGemini(data) {
            if (!this._snapshot || !Array.isArray(this._snapshot.candidates)) {
                this._snapshot = { candidates: [] };
            }
            // Copy non-candidates/non-usageMetadata fields.
            for (const k of Object.keys(data)) {
                if (k === 'candidates' || k === 'usageMetadata') continue;
                this._snapshot[k] = deepCopy(data[k]);
            }
            const candidates = data.candidates;
            if (Array.isArray(candidates)) {
                for (let pos = 0; pos < candidates.length; pos++) {
                    if (isDict(candidates[pos])) this._mergeGeminiCandidate(pos, candidates[pos]);
                }
            }
            if (isDict(data.usageMetadata)) {
                this._snapshot.usageMetadata = deepCopy(data.usageMetadata);
                this._snapshot.usage = normalizeUsage(data.usageMetadata);
            }
            this._snapshot.content = this._geminiContentBlocks();
        }

        _mergeGeminiCandidate(position, candidate) {
            const idx = (typeof candidate.index === 'number' && candidate.index >= 0) ? candidate.index : position;
            const candidates = this._snapshot.candidates;
            while (candidates.length <= idx) candidates.push({});
            const target = candidates[idx];
            if (!isDict(target)) { candidates[idx] = {}; }
            const tgt = candidates[idx];
            for (const k of Object.keys(candidate)) {
                if (k === 'content' && isDict(candidate.content)) {
                    this._mergeGeminiCandidateContent(tgt, candidate.content);
                } else {
                    tgt[k] = deepCopy(candidate[k]);
                }
            }
        }

        _mergeGeminiCandidateContent(candidate, incoming) {
            if (!isDict(candidate.content)) candidate.content = {};
            const content = candidate.content;
            for (const k of Object.keys(incoming)) {
                if (k === 'parts') continue;
                content[k] = deepCopy(incoming[k]);
            }
            if (!Array.isArray(content.parts)) content.parts = [];
            for (const part of (incoming.parts || [])) {
                if (isDict(part)) this._appendGeminiPart(content.parts, part);
            }
        }

        _appendGeminiPart(parts, part) {
            // Plain text parts get concatenated onto the previous text part
            // when they share the same `thought` flag — that's how Gemini
            // streams assistant prose.
            if (isDict(part) && typeof part.text === 'string' &&
                Object.keys(part).every(k => k === 'text' || k === 'thought')) {
                const prev = parts[parts.length - 1];
                if (isDict(prev) && typeof prev.text === 'string' &&
                    Object.keys(prev).every(k => k === 'text' || k === 'thought') &&
                    !!prev.thought === !!part.thought) {
                    prev.text += part.text;
                    return;
                }
            }
            parts.push(deepCopy(part));
        }

        _geminiContentBlocks() {
            const content = [];
            for (const candidate of (this._snapshot.candidates || [])) {
                if (!isDict(candidate)) continue;
                const candContent = candidate.content;
                if (!isDict(candContent)) continue;
                for (const part of (candContent.parts || [])) {
                    if (!isDict(part)) continue;
                    if (typeof part.text === 'string' && part.text.trim()) {
                        if (part.thought === true) {
                            this._appendMergeable(content, { type: 'thinking', thinking: part.text });
                        } else {
                            this._appendMergeable(content, { type: 'text', text: part.text });
                        }
                    }
                    if (isDict(part.functionCall)) {
                        content.push({
                            type: 'tool_use',
                            id: part.functionCall.id || '',
                            name: part.functionCall.name || 'tool_use',
                            input: isDict(part.functionCall.args) ? part.functionCall.args : {},
                        });
                    }
                }
            }
            return content;
        }

        _appendMergeable(blocks, block) {
            const prev = blocks[blocks.length - 1];
            if (isDict(prev) && prev.type === block.type) {
                if (block.type === 'text') prev.text = (prev.text || '') + (block.text || '');
                else if (block.type === 'thinking') prev.thinking = (prev.thinking || '') + (block.thinking || '');
                else { blocks.push(block); }
                return;
            }
            blocks.push(block);
        }

        // ---- output ----

        reconstruct() {
            return this._snapshot ? deepCopy(this._snapshot) : null;
        }
    }

    // ---------- Request normaliser ----------

    // Convert any of the four request formats into a canonical message
    // list. Returns {system?: string, messages: [{role, content}]}.
    // `path` is the URL path so we can pick the right format detector
    // without sniffing the body.
    function normalizeRequest(body, path) {
        if (typeof body === 'string') {
            try { body = JSON.parse(body); }
            catch (e) { return null; }
        }
        if (!isDict(body)) return null;
        const format = detectRequestFormat(path, body);
        switch (format) {
            case 'chat_completions': return normalizeChatRequest(body);
            case 'responses':        return normalizeResponsesRequest(body);
            case 'anthropic':        return normalizeAnthropicRequest(body);
            case 'gemini':           return normalizeGeminiRequest(body);
            default:                 return null;
        }
    }

    function detectRequestFormat(path, body) {
        // Body-shape takes priority over URL path because:
        //   (a) For RESPONSE bodies: the URL path is the client's path
        //       (e.g. /v1/responses) but the body is whatever the
        //       downstream produced. When an auto-translator rewrites
        //       requests/responses between formats, the URL may say
        //       /v1/responses but the body is Chat Completions-shape
        //       (choices[]), and vice versa.
        //   (b) For REQUEST bodies: content shape is what we actually
        //       need to normalise.

        // Anthropic and Gemini: URL path is unambiguous (their SDKs only
        // post to those endpoints), so we let path win here.
        if (path && path.indexOf('/v1/messages') !== -1) return 'anthropic';
        if (path && path.indexOf('/v1beta/') === 0) return 'gemini';

        // Body-shape recognition first.
        if (Array.isArray(body.messages)) return 'chat_completions';
        if (typeof body.system === 'string' && Array.isArray(body.messages)) return 'anthropic';
        if (Array.isArray(body.contents)) return 'gemini';
        if (Array.isArray(body.input)) return 'responses';
        // Response-shape recognition: the four formats' response bodies don't
        // carry `messages`/`contents`/`input` — they hold their canonical
        // shape (OpenAI `choices`/`output`, Anthropic `content`+`role`,
        // Gemini `candidates`). We accept those here so the parser can reach
        // the format-specific normalisers downstream.
        if (Array.isArray(body.choices)) return 'chat_completions';
        if (Array.isArray(body.candidates)) return 'gemini';
        if (Array.isArray(body.output) && isDict(body) && body.object === 'response') return 'responses';
        if (Array.isArray(body.content) && (body.role === 'assistant' || body.role === 'user' || body.role === 'system')) return 'anthropic';

        // Path as last fallback: when neither path nor body reveals the
        // format, fall through to whatever the URL hints at.
        if (path && path.indexOf('/v1/responses') !== -1) return 'responses';

        return null;
    }

    function normalizeChatRequest(body) {
        // OpenAI Chat uses messages with role="system" rather than a
        // top-level system field. Lift system-role messages out of the
        // conversation into the canonical `system` slot — that's how
        // Anthropic and Gemini expose it, and lets the inspector render
        // system instructions in a distinct colour.
        //
        // The lifted system content is normalised to a list of
        // {type:'text', text} blocks so the renderer doesn't have to
        // JSON.stringify it (which would dump the whole array as a
        // single wrapped text block).
        const messages = [];
        const systemBlocks = [];
        for (const m of (body.messages || [])) {
            if (m && m.role === 'system') {
                appendSystemContent(systemBlocks, m.content);
                continue;
            }
            messages.push({ role: m.role || 'user', content: m.content });
        }
        return {
            system: systemBlocks.length ? systemBlocks : null,
            messages: messages,
        };
    }

    // appendSystemContent pushes one or more text blocks from a system
    // message's content into `out`. The content can be a string, an
    // array of {type:'text', text} blocks (OpenAI Chat), or a single
    // object (rare). Anything else is coerced via String() so the
    // operator still sees the system instructions in the inspector.
    function appendSystemContent(out, content) {
        if (content == null) return;
        if (typeof content === 'string') {
            if (content) out.push({ type: 'text', text: content });
            return;
        }
        if (Array.isArray(content)) {
            for (const b of content) {
                if (b == null) continue;
                if (typeof b === 'string') {
                    if (b) out.push({ type: 'text', text: b });
                } else if (typeof b === 'object') {
                    out.push({ type: 'text', text: b.text || '' });
                }
            }
            return;
        }
        if (typeof content === 'object') {
            out.push({ type: 'text', text: content.text || '' });
            return;
        }
        out.push({ type: 'text', text: String(content) });
    }

    function normalizeResponsesRequest(body) {
        // `instructions` is documented as a string but the OpenAI
        // Responses API can also accept a list of structured blocks
        // (matching the chat completion content shape). Normalise to a
        // list of {type:'text', text} so the renderer doesn't have to.
        const out = { system: normalizeInstructionsField(body.instructions), messages: [] };
        for (const item of (body.input || [])) {
            if (!isDict(item)) continue;
            // Three well-known input item types: 'message' (text/image),
            // 'function_call' / 'function_call_output' (tool interactions),
            // or just an item with a `role` and `content` (no type field —
            // common in client SDKs that omit it).
            const t = item.type;
            if (t == null || t === 'message') {
                // Default to message handling when type is missing so
                // {role, content} payloads from the Responses API still
                // flow through. extractResponsesContent takes care of
                // string and array shapes.
                out.messages.push({
                    role: item.role || 'user',
                    content: extractResponsesContent(item.content),
                });
            } else if (t === 'function_call' || t === 'function_call_output') {
                // Tool calls/results appear as their own items. Roll them
                // into a single assistant message in the canonical list.
                out.messages.push({ role: 'tool', raw: item });
            } else {
                out.messages.push({ role: item.role || 'user', raw: item });
            }
        }
        return out;
    }

    function extractResponsesContent(content) {
        if (typeof content === 'string') return content;
        if (!Array.isArray(content)) return content;
        return content.map(p => {
            if (!isDict(p)) return p;
            if (p.type === 'input_text' || p.type === 'output_text') return { type: 'text', text: p.text || '' };
            if (p.type === 'input_image' || p.type === 'image') return { type: 'image', source: p.source || p };
            return p;
        });
    }

    function normalizeAnthropicRequest(body) {
        return {
            // Anthropic `system` can be either a string or an array of
            // structured blocks (each {type:'text', text, cache_control?}).
            // Normalise to an array of plain text blocks so the renderer
            // doesn't have to special-case both shapes — and so a request
            // that arrived as the array form renders as readable text
            // blocks instead of a `JSON.stringify` dump starting with `[{`.
            // We drop `cache_control` here because the renderer doesn't
            // surface it (it's a prompt-cache marker, not content) and
            // because some clients (notably Claude Code) attach
            // cache_control to system blocks between turns, which would
            // otherwise produce duplicate-looking blocks.
            system: normalizeAnthropicSystem(body.system),
            messages: (body.messages || []).map(m => ({
                role: m.role || 'user',
                content: m.content, // string or array
            })),
        };
    }

    function normalizeAnthropicSystem(system) {
        if (system == null) return null;
        if (typeof system === 'string') {
            return [{ type: 'text', text: system }];
        }
        if (Array.isArray(system)) {
            return system
                .filter(b => b && typeof b === 'object')
                .map(b => ({ type: 'text', text: b.text || '' }));
        }
        // Unknown shape: fall through to a single-block render rather
        // than dropping the system prompt entirely.
        return [{ type: 'text', text: String(system) }];
    }

    // normalizeInstructionsField is the Responses-API equivalent of
    // normalizeAnthropicSystem. `instructions` is documented as a
    // string but in practice some clients post an array of structured
    // blocks. We always return either null or an array of {type:'text',
    // text} so the renderer's `Array.isArray(req.system)` branch
    // always handles things consistently.
    function normalizeInstructionsField(instructions) {
        if (instructions == null) return null;
        if (typeof instructions === 'string') {
            return instructions ? [{ type: 'text', text: instructions }] : null;
        }
        if (Array.isArray(instructions)) {
            const out = [];
            for (const b of instructions) {
                if (b == null) continue;
                if (typeof b === 'string') {
                    if (b) out.push({ type: 'text', text: b });
                } else if (typeof b === 'object') {
                    out.push({ type: 'text', text: b.text || '' });
                }
            }
            return out.length ? out : null;
        }
        return [{ type: 'text', text: String(instructions) }];
    }

    function normalizeGeminiRequest(body) {
        return {
            system: (body.systemInstruction && body.systemInstruction.parts) ?
                body.systemInstruction.parts.map(p => p.text || '').join('\n') : null,
            messages: (body.contents || []).map(c => ({
                role: c.role === 'model' ? 'assistant' : (c.role || 'user'),
                content: c.parts || [],
            })),
        };
    }

    // Convert a reconstructed response into a canonical {role, content: [...]}.
    function normalizeResponse(body, path) {
        if (typeof body === 'string') {
            try { body = JSON.parse(body); }
            catch (e) { return null; }
        }
        if (!isDict(body)) return null;
        const format = detectRequestFormat(path, body);
        // Reuse the format detector — it works for both directions because
        // the response shape mirrors the request shape.
        switch (format) {
            case 'chat_completions': return normalizeChatResponse(body);
            case 'responses':        return normalizeResponsesResponse(body);
            case 'anthropic':        return normalizeAnthropicResponse(body);
            case 'gemini':           return normalizeGeminiResponse(body);
            default:                 return null;
        }
    }

    function normalizeChatResponse(body) {
        const choice = (body.choices || [])[0];
        if (!choice) return null;
        const msg = choice.message || {};
        const blocks = [];
        if (typeof msg.content === 'string' && msg.content) blocks.push({ type: 'text', text: msg.content });
        if (typeof msg.reasoning_content === 'string' && msg.reasoning_content) {
            blocks.unshift({ type: 'thinking', thinking: msg.reasoning_content });
        }
        if (Array.isArray(msg.tool_calls)) {
            for (const tc of msg.tool_calls) {
                if (!isDict(tc)) continue;
                let input = {};
                if (typeof tc.function && typeof tc.function.arguments === 'string') {
                    try { input = JSON.parse(tc.function.arguments); }
                    catch (e) { input = tc.function.arguments; }
                }
                blocks.push({ type: 'tool_use', id: tc.id || '', name: (tc.function && tc.function.name) || 'tool_use', input });
            }
        }
        return {
            role: 'assistant',
            content: blocks,
            finish_reason: choice.finish_reason || null,
            usage: normalizeUsage(body.usage),
        };
    }

    function normalizeResponsesResponse(body) {
        const blocks = [];
        for (const item of (body.output || [])) {
            if (!isDict(item)) continue;
            if (item.type === 'message' && Array.isArray(item.content)) {
                for (const p of item.content) {
                    if (!isDict(p)) continue;
                    if (p.type === 'output_text') blocks.push({ type: 'text', text: p.text || '' });
                }
            } else if (item.type === 'function_call' || item.type === 'tool_call') {
                let input = {};
                if (typeof item.arguments === 'string') {
                    try { input = JSON.parse(item.arguments); }
                    catch (e) { input = item.arguments; }
                } else if (isDict(item.arguments)) {
                    input = item.arguments;
                }
                blocks.push({ type: 'tool_use', id: item.id || item.call_id || '', name: item.name || 'tool_use', input });
            }
        }
        return {
            role: 'assistant',
            content: blocks,
            status: body.status || null,
            usage: normalizeUsage(body.usage),
        };
    }

    function normalizeAnthropicResponse(body) {
        return {
            role: 'assistant',
            content: Array.isArray(body.content) ? body.content : [],
            stop_reason: body.stop_reason || null,
            usage: normalizeUsage(body.usage),
        };
    }

    function normalizeGeminiResponse(body) {
        // Build a content-array from candidates[].content.parts[].
        const blocks = [];
        for (const candidate of (body.candidates || [])) {
            if (!isDict(candidate)) continue;
            const parts = (candidate.content && candidate.content.parts) || [];
            for (const part of parts) {
                if (!isDict(part)) continue;
                if (typeof part.text === 'string' && part.text) {
                    if (part.thought === true) blocks.push({ type: 'thinking', thinking: part.text });
                    else blocks.push({ type: 'text', text: part.text });
                }
                if (isDict(part.functionCall)) {
                    blocks.push({
                        type: 'tool_use',
                        id: part.functionCall.id || '',
                        name: part.functionCall.name || 'tool_use',
                        input: isDict(part.functionCall.args) ? part.functionCall.args : {},
                    });
                }
            }
        }
        return {
            role: 'assistant',
            content: blocks,
            finish_reason: (body.candidates || [])[0] ? ((body.candidates[0].finishReason) || null) : null,
            usage: normalizeUsage(body.usageMetadata),
        };
    }

    // ---------- exports ----------

    global.SSEReassembler = SSEReassembler;
    global.normalizeRequest = normalizeRequest;
    global.normalizeResponse = normalizeResponse;
    global.detectRequestFormat = detectRequestFormat;

})(typeof window !== 'undefined' ? window : globalThis);
