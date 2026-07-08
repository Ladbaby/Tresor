// content-normalise.js — converts a message content field (string,
// array of parts, or single object) into a uniform list of content
// blocks the renderer can dispatch on. Pure function, no DOM access.
//
// Loaded as a <script> before app.js (see index.html) so app.js can
// call the global normaliseContentBlocks. Tested directly under Node
// from sse-reassembler.test.js so the normaliser can be exercised
// without pulling in the DOM-heavy app.js.
//
// `detectInjectedWrapper` is provided by wrapper-detect.js, which is
// loaded first. We call it through a global indirection so this file
// doesn't have a hard dependency on the wrapper module.

'use strict';

(function () {
    const diw = (typeof detectInjectedWrapper === 'function')
        ? detectInjectedWrapper
        : function (b) { return b; };

    function normaliseContentBlocks(content) {
        if (content == null) return [];
        if (typeof content === 'string') {
            // A bare string could in theory wrap a system-reminder
            // (some older clients concatenate the wrapper into a
            // single text block). Treat it like a one-block content
            // array.
            return content ? [diw({ type: 'text', text: content })] : [];
        }
        if (Array.isArray(content)) {
            // Two shapes appear:
            //   - OpenAI Chat: parts can be strings (rare) or {type, text,...}.
            //   - Anthropic/OpenAI Responses: parts are objects {type, text, ...}.
            //   - Gemini: parts are objects {text} | {functionCall}.
            const out = [];
            for (const p of content) {
                if (p == null) continue;
                if (typeof p === 'string') {
                    out.push(diw({ type: 'text', text: p }));
                    continue;
                }
                if (typeof p !== 'object') continue;
                if (p.type === 'thinking' || p.type === 'thinking_delta' || p.thinking != null) {
                    out.push({ type: 'thinking', thinking: p.thinking || p.text || '' });
                } else if (p.type === 'tool_use' || p.functionCall || p.tool_call || p.type === 'function_call') {
                    const fn = p.functionCall || p;
                    out.push({
                        type: 'tool_use',
                        id: p.id || '',
                        name: (fn && (fn.name || fn.function)) || 'tool_use',
                        input: fn && (fn.args || fn.input || fn.arguments) || {},
                    });
                } else if (p.type === 'tool_result') {
                    // Anthropic tool_result block. Content can be a
                    // string (most cases — bash output, file contents)
                    // or an array of structured blocks (text + image
                    // when a tool returned a screenshot, for example).
                    // Normalise to a dedicated block type so the
                    // renderer can show a labelled "Tool result (id)"
                    // header instead of a JSON dump. cache_control is
                    // dropped because it's a prompt-cache marker, not
                    // content (same reason as system blocks).
                    out.push({
                        type: 'tool_result',
                        tool_use_id: p.tool_use_id || '',
                        is_error: !!p.is_error,
                        content: p.content,
                    });
                } else if (p.type === 'image' || p.type === 'input_image' || p.type === 'image_url') {
                    const url = (p.image_url && p.image_url.url) || p.url || p.source_url;
                    out.push({ type: 'image', url: url });
                } else if (p.type === 'text' || p.text != null) {
                    out.push(diw({ type: 'text', text: p.text || '' }));
                } else {
                    out.push({ type: 'raw', value: p });
                }
            }
            return out;
        }
        // Single object — treat as a single block in its declared shape.
        return [content];
    }

    // Export pattern matches sse-reassembler.js: browser attaches to
    // window, Node attaches to globalThis.
    if (typeof window !== 'undefined') {
        window.normaliseContentBlocks = normaliseContentBlocks;
    } else if (typeof globalThis !== 'undefined') {
        globalThis.normaliseContentBlocks = normaliseContentBlocks;
    }
})();
