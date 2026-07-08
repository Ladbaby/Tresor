// sse-reassembler.test.js — Node-runnable sanity tests for the
// inspector's SSE reassembler + format normalizer.
//
// The reassembler is loaded inside a browser; we shim the globals it
// expects, then exercise the four supported formats against canned
// streams and assert the reconstructed snapshot shape.
//
// Run with: node internal/api/web/sse-reassembler.test.js
// Exit 0 on pass; non-zero with a failure summary on fail.

'use strict';

const fs = require('fs');
const path = require('path');

const source = fs.readFileSync(path.join(__dirname, 'sse-reassembler.js'), 'utf8');
// The browser IIFE has a 'window' guard; under Node it falls through to
// globalThis because `window` is undefined. globals assigned are picked
// up by tests below.
const window = {};
// eslint-disable-next-line no-new-func
new Function('window', source)(window);

// wrapper-detect.js is loaded as a script in the browser (before app.js).
// Under Node we eval it in the same way. The IIFE attaches
// `detectInjectedWrapper` to globalThis.
const wrapperSource = fs.readFileSync(path.join(__dirname, 'wrapper-detect.js'), 'utf8');
new Function(wrapperSource)();

// content-normalise.js is loaded after wrapper-detect.js (because it
// depends on the global `detectInjectedWrapper`). Order matters here.
const normaliseSource = fs.readFileSync(path.join(__dirname, 'content-normalise.js'), 'utf8');
new Function(normaliseSource)();

const SSEReassembler = window.SSEReassembler;
const normalizeRequest = window.normalizeRequest;
const normalizeResponse = window.normalizeResponse;
const detectRequestFormat = window.detectRequestFormat;

if (typeof SSEReassembler !== 'function') {
    console.error('FAIL: SSEReassembler not exported'); process.exit(1);
}
if (typeof normalizeRequest !== 'function') {
    console.error('FAIL: normalizeRequest not exported'); process.exit(1);
}

let failed = 0;
let passed = 0;
function check(name, cond, detail) {
    if (cond) { passed++; }
    else {
        failed++;
        console.error('FAIL:', name, detail ? ('\n  ' + detail) : '');
    }
}
function eq(name, got, want) {
    check(name + ' === ' + JSON.stringify(want),
        JSON.stringify(got) === JSON.stringify(want),
        'got ' + JSON.stringify(got));
}

// ---------- Chat Completions streaming ----------

{
    const stream =
        'data: {"id":"chat-1","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"}}]}\n' +
        '\n' +
        'data: {"id":"chat-1","model":"gpt-4o","choices":[{"index":0,"delta":{"content":" world"}}]}\n' +
        '\n' +
        'data: {"choices":[{"index":0,"finish_reason":"stop"}]}\n' +
        '\n' +
        'data: [DONE]\n';
    const r = new SSEReassembler();
    r.feed(stream);
    const snap = r.reconstruct();
    check('chat: snapshot exists', snap != null);
    eq('chat: assistant content accumulated',
        snap && snap.choices[0].message.content, 'Hello world');
    eq('chat: finish_reason captured',
        snap && snap.choices[0].finish_reason, 'stop');
    eq('chat: events count (no [DONE])', r.events.length, 3);
    // Anthropic-shaped mirror
    const text = snap && snap.content && snap.content.find(b => b.type === 'text');
    check('chat: content[] mirror has merged text', text && text.text === 'Hello world');
}

// ---------- Chat Completions with tool_call streaming ----------

{
    const stream =
        'data: {"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"get_"}}]}}]}\n' +
        '\n' +
        'data: {"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"weather"}}]}}]}\n' +
        '\n' +
        'data: {"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\\"city\\""}}]}}]}\n' +
        '\n' +
        'data: {"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":":\\"NYC\\"}"}}]}}]}\n' +
        '\n' +
        'data: {"choices":[{"finish_reason":"tool_calls"}]}\n' +
        '\n' +
        'data: [DONE]\n';
    const r = new SSEReassembler();
    r.feed(stream);
    const snap = r.reconstruct();
    check('chat-tools: snapshot exists', snap != null);
    const tc = snap && snap.choices[0].message.tool_calls && snap.choices[0].message.tool_calls[0];
    eq('chat-tools: name assembled', tc && tc.function.name, 'get_weather');
    eq('chat-tools: args assembled', tc && tc.function.arguments, '{"city":"NYC"}');
    eq('chat-tools: finish_reason', snap && snap.choices[0].finish_reason, 'tool_calls');
    // Mirror should have a tool_use block with parsed input
    const tu = snap && snap.content && snap.content.find(b => b.type === 'tool_use');
    check('chat-tools: tool_use mirror present', tu != null);
    check('chat-tools: tool_use input parsed',
        tu && tu.input && tu.input.city === 'NYC',
        'got ' + JSON.stringify(tu && tu.input));
}

// ---------- Chat Completions with reasoning_content (o1 style) ----------

{
    const stream =
        'data: {"choices":[{"delta":{"role":"assistant","reasoning_content":"I think"}}]}\n' +
        '\n' +
        'data: {"choices":[{"delta":{"reasoning_content":" therefore..."}}]}\n' +
        '\n' +
        'data: {"choices":[{"delta":{"content":"Final answer"}}]}\n' +
        '\n' +
        'data: [DONE]\n';
    const r = new SSEReassembler();
    r.feed(stream);
    const snap = r.reconstruct();
    check('o1: snapshot exists', snap != null);
    eq('o1: reasoning_content assembled',
        snap && snap.choices[0].message.reasoning_content, 'I think therefore...');
    // Mirror should place thinking BEFORE text
    const blocks = snap && snap.content;
    check('o1: thinking block precedes text',
        blocks && blocks[0] && blocks[0].type === 'thinking' && blocks[0].thinking === 'I think therefore...'
        && blocks[1] && blocks[1].type === 'text' && blocks[1].text === 'Final answer',
        'blocks: ' + JSON.stringify(blocks));
}

// ---------- Anthropic streaming ----------

{
    const stream =
        'event: message_start\n' +
        'data: {"message":{"id":"msg_1","role":"assistant","content":[],"model":"claude-3","usage":{"input_tokens":12,"output_tokens":0}}}\n' +
        '\n' +
        'event: content_block_start\n' +
        'data: {"index":0,"content_block":{"type":"text","text":""}}\n' +
        '\n' +
        'event: content_block_delta\n' +
        'data: {"index":0,"delta":{"type":"text_delta","text":"Hi"}}\n' +
        '\n' +
        'event: content_block_delta\n' +
        'data: {"index":0,"delta":{"type":"text_delta","text":" there"}}\n' +
        '\n' +
        'event: content_block_stop\n' +
        'data: {"index":0}\n' +
        '\n' +
        'event: message_delta\n' +
        'data: {"delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}\n' +
        '\n' +
        'event: message_stop\n' +
        'data: {}\n' +
        '\n';
    const r = new SSEReassembler();
    r.feed(stream);
    const snap = r.reconstruct();
    check('anthropic: snapshot exists', snap != null);
    eq('anthropic: role', snap && snap.role, 'assistant');
    const block0 = snap && snap.content && snap.content[0];
    eq('anthropic: text delta merged', block0 && block0.text, 'Hi there');
    eq('anthropic: stop_reason', snap && snap.stop_reason, 'end_turn');
    eq('anthropic: output_tokens', snap && snap.usage && snap.usage.output_tokens, 2);
    eq('anthropic: input_tokens', snap && snap.usage && snap.usage.input_tokens, 12);
}

// ---------- Anthropic streaming with tool_use (input_json_delta) ----------

{
    const stream =
        'event: message_start\ndata: {"message":{"id":"msg_2","role":"assistant","content":[],"model":"claude-3"}}\n\n' +
        'event: content_block_start\ndata: {"index":0,"content_block":{"type":"tool_use","id":"tool_1","name":"calc","input":{}}}\n\n' +
        'event: content_block_delta\ndata: {"index":0,"delta":{"type":"input_json_delta","partial_json":"{\\"x\\":1"}}\n\n' +
        'event: content_block_delta\ndata: {"index":0,"delta":{"type":"input_json_delta","partial_json":",\\"y\\":2}"}}\n\n' +
        'event: content_block_stop\ndata: {"index":0}\n\n' +
        'event: message_stop\ndata: {}\n\n';
    const r = new SSEReassembler();
    r.feed(stream);
    const snap = r.reconstruct();
    const block0 = snap && snap.content && snap.content[0];
    check('anthropic-tool: tool_use block present', block0 && block0.type === 'tool_use');
    eq('anthropic-tool: name', block0 && block0.name, 'calc');
    eq('anthropic-tool: id', block0 && block0.id, 'tool_1');
    check('anthropic-tool: input parsed',
        block0 && block0.input && block0.input.x === 1 && block0.input.y === 2,
        'got ' + JSON.stringify(block0 && block0.input));
}

// ---------- OpenAI Responses streaming ----------

{
    const stream =
        'event: response.created\ndata: {"response":{"id":"resp_1","status":"in_progress","output":[]}}\n\n' +
        'event: response.output_item.added\ndata: {"output_index":0,"item":{"type":"message","role":"assistant","content":[]}}\n\n' +
        'event: response.output_text.delta\ndata: {"output_index":0,"delta":"Hello"}\n\n' +
        'event: response.output_text.delta\ndata: {"output_index":0,"delta":" world"}\n\n' +
        'event: response.completed\ndata: {"response":{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":3,"output_tokens":2}}}\n\n';
    const r = new SSEReassembler();
    r.feed(stream);
    const snap = r.reconstruct();
    check('responses: snapshot exists', snap != null);
    check('responses: in-progress output preserved',
        snap && snap.output && snap.output[0] && Array.isArray(snap.output[0].content),
        'snap=' + JSON.stringify(snap));
    const txt = snap && snap.output && snap.output[0] && snap.output[0].content
        && snap.output[0].content.find(c => c.type === 'output_text');
    eq('responses: text deltas merged', txt && txt.text, 'Hello world');
}

// ---------- Gemini streaming ----------

{
    const stream =
        'data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"Hi"}]}}]}\n\n' +
        'data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":" there"}]}}]}\n\n' +
        'data: {"candidates":[{"index":0,"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2}}\n\n';
    const r = new SSEReassembler();
    r.feed(stream);
    const snap = r.reconstruct();
    check('gemini: snapshot exists', snap != null);
    const candidates = snap && snap.candidates;
    check('gemini: one candidate', candidates && candidates.length === 1,
        'candidates=' + JSON.stringify(candidates));
    const merged = candidates && candidates[0] && candidates[0].content
        && candidates[0].content.parts
        && candidates[0].content.parts.map(p => p.text).join('');
    eq('gemini: parts merged', merged, 'Hi there');
    eq('gemini: finishReason preserved',
        candidates && candidates[0] && candidates[0].finishReason, 'STOP');
    check('gemini: usage normalised',
        snap && snap.usage && snap.usage.input_tokens === 5 && snap.usage.output_tokens === 2,
        'usage=' + JSON.stringify(snap && snap.usage));
}

// ---------- Multi-byte chunk boundary ----------

{
    // Split a single SSE event across two chunks so we exercise the
    // partial-line buffer in the parser.
    const evt1 =
        'event: message_start\n' +
        'data: {"message":{"id":"m","role":"assistant","content":[]}}\n\n' +
        'event: content_block_delta\n' +
        'data: {"index":0,"delta":{"type":"text_delta","text":"abc"}}\n\n';
    const r = new SSEReassembler();
    const mid = 7; // split inside "data: {"message"
    r.feed(evt1.slice(0, mid));
    r.feed(evt1.slice(mid));
    const snap = r.reconstruct();
    const block0 = snap && snap.content && snap.content[0];
    eq('chunk-boundary: text across chunk boundary',
        block0 && block0.text, 'abc');
}

// ---------- Request normalisation ----------

{
    const req = normalizeRequest({
        model: 'gpt-4o',
        messages: [
            { role: 'system', content: 'You are helpful.' },
            { role: 'user', content: 'Hi' },
        ],
    }, '/v1/chat/completions');
    // System-role messages are lifted into the top-level `system` slot;
    // messages array should contain only the user turn here. The system
    // field is normalised to a list of {type:'text', text} blocks so the
    // renderer can treat every format's system field the same way.
    check('norm-req: shape', req && req.messages && req.messages.length === 1,
        'messages=' + JSON.stringify(req && req.messages));
    check('norm-req: system extracted as blocks',
        req && Array.isArray(req.system) && req.system.length === 1 &&
        req.system[0].type === 'text' && req.system[0].text === 'You are helpful.',
        'system=' + JSON.stringify(req && req.system));
    eq('norm-req: user message preserved',
        req && req.messages && req.messages[0] && req.messages[0].content, 'Hi');
}

// System field as a structured array of blocks (OpenAI Chat Completions
// accepts this shape, and Anthropic also accepts it). Both should be
// normalised to [{type:'text', text:'...'}] so the renderer doesn't
// JSON.stringify the array.
{
    const req = normalizeRequest({
        messages: [
            { role: 'system', content: [
                { type: 'text', text: 'You are helpful.' },
                { type: 'text', text: 'Be concise.' },
            ]},
            { role: 'user', content: 'Hi' },
        ],
    }, '/v1/chat/completions');
    check('norm-req-chat-array: system flattened',
        req && Array.isArray(req.system) && req.system.length === 2 &&
        req.system[0].text === 'You are helpful.' && req.system[1].text === 'Be concise.',
        'system=' + JSON.stringify(req && req.system));
}

{
    // Multiple system messages in chat_completions are preserved as a
    // list of blocks (rather than concatenated into one string). This
    // matches what the operator would want to see: the system prompt is
    // often delivered in pieces and the pieces may have different
    // metadata (cache_control) attached to each.
    const req = normalizeRequest({
        messages: [
            { role: 'system', content: 'First.' },
            { role: 'system', content: 'Second.' },
            { role: 'user', content: 'Hi' },
        ],
    }, '/v1/chat/completions');
    check('norm-req-chat-multi: multiple system blocks',
        req && Array.isArray(req.system) && req.system.length === 2 &&
        req.system[0].text === 'First.' && req.system[1].text === 'Second.',
        'system=' + JSON.stringify(req && req.system));
}

{
    // Anthropic system field as an array of {type:'text', text, cache_control}
    // blocks. cache_control is dropped because the renderer doesn't
    // surface it (it's a prompt-cache marker, not content) and
    // including it would produce duplicate-looking blocks across turns.
    const req = normalizeRequest({
        system: [
            { type: 'text', text: 'You are Claude Code.', cache_control: { type: 'ephemeral' } },
            { type: 'text', text: 'Be concise.', cache_control: { type: 'ephemeral' } },
        ],
        messages: [{ role: 'user', content: 'hi' }],
    }, '/v1/messages');
    check('norm-req-anthropic-array: system flattened, cache_control dropped',
        req && Array.isArray(req.system) && req.system.length === 2 &&
        req.system[0].text === 'You are Claude Code.' &&
        req.system[0].cache_control === undefined &&
        req.system[1].text === 'Be concise.',
        'system=' + JSON.stringify(req && req.system));
}

{
    // Anthropic system as a plain string (the simpler request shape).
    const req = normalizeRequest({
        system: 'You are Claude Code.',
        messages: [{ role: 'user', content: 'hi' }],
    }, '/v1/messages');
    check('norm-req-anthropic-string: wrapped in single block',
        req && Array.isArray(req.system) && req.system.length === 1 &&
        req.system[0].text === 'You are Claude Code.',
        'system=' + JSON.stringify(req && req.system));
}

{
    const req = normalizeRequest({
        system: 'helpful',
        messages: [{ role: 'user', content: 'hi' }],
    }, '/v1/messages');
    check('norm-req-anthropic: detected', req != null);
    check('norm-req-anthropic: system is wrapped block array',
        req && Array.isArray(req.system) && req.system.length === 1 &&
        req.system[0].text === 'helpful',
        'system=' + JSON.stringify(req && req.system));
}

// OpenAI Responses API request — input items do NOT carry an explicit
// `type:'message'` field. The normaliser must default to message handling
// when type is missing, otherwise the parser silently drops the user turn
// and the request side of the inspector appears empty.
{
    const body = {
        model: 'qwen3.5:9b-mtp',
        input: [{ role: 'user', content: [{ type: 'input_text', text: 'hi' }] }],
        store: false,
        stream: true,
    };
    const req = normalizeRequest(body, '/v1/responses');
    check('norm-req-responses: detected', req != null);
    check('norm-req-responses: input not dropped',
        req && req.messages && req.messages.length === 1,
        'messages=' + JSON.stringify(req && req.messages));
    eq('norm-req-responses: user content preserved',
        req && req.messages && req.messages[0] &&
        req.messages[0].content && req.messages[0].content[0] &&
        req.messages[0].content[0].text, 'hi');
}

// ---------- Response normalization across all 4 formats ----------
//
// These mirror what the inspector's buildParsedView does: feed a raw
// captured body (streaming or non-streaming) through detectRequestFormat
// and (for streams) SSEReassembler, then call normalizeResponse on the
// reconstructed snapshot. The bug we're guarding against is the parser
// silently producing an empty content array for any response shape.

// --- OpenAI Chat Completions: streaming SSE response ---

{
    const stream =
        'data: {"id":"c","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"How"}}]}\n\n' +
        'data: {"id":"c","model":"gpt-4o","choices":[{"index":0,"delta":{"content":" can"}}]}\n\n' +
        'data: {"id":"c","model":"gpt-4o","choices":[{"index":0,"delta":{"content":" I help you today?"},"finish_reason":"stop"}]}\n\n' +
        'data: [DONE]\n';
    const r = new SSEReassembler();
    r.feed(stream);
    const snap = r.reconstruct();
    const norm = normalizeResponse(snap, '/v1/chat/completions');
    check('norm-resp-chat-stream: not null', norm != null);
    eq('norm-resp-chat-stream: role', norm && norm.role, 'assistant');
    const t = norm && norm.content && norm.content[0];
    eq('norm-resp-chat-stream: text assembled', t && t.text, 'How can I help you today?');
    eq('norm-resp-chat-stream: finish_reason', norm && norm.finish_reason, 'stop');
}

// --- OpenAI Chat Completions: non-streaming JSON response ---

{
    const body = {
        id: 'c', model: 'gpt-4o',
        choices: [{ index: 0, message: { role: 'assistant', content: 'Hello there.' }, finish_reason: 'stop' }],
    };
    const norm = normalizeResponse(body, '/v1/chat/completions');
    check('norm-resp-chat-json: not null', norm != null);
    const t = norm && norm.content && norm.content[0];
    eq('norm-resp-chat-json: text', t && t.text, 'Hello there.');
}

// --- Anthropic: streaming SSE response ---

{
    const stream =
        'event: message_start\ndata: {"message":{"id":"m","role":"assistant","content":[],"model":"claude-3","usage":{"input_tokens":7,"output_tokens":0}}}\n\n' +
        'event: content_block_start\ndata: {"index":0,"content_block":{"type":"text","text":""}}\n\n' +
        'event: content_block_delta\ndata: {"index":0,"delta":{"type":"text_delta","text":"I am Claude."}}\n\n' +
        'event: content_block_stop\ndata: {"index":0}\n\n' +
        'event: message_delta\ndata: {"delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}\n\n' +
        'event: message_stop\ndata: {}\n\n';
    const r = new SSEReassembler();
    r.feed(stream);
    const snap = r.reconstruct();
    const norm = normalizeResponse(snap, '/v1/messages');
    check('norm-resp-anthropic-stream: not null', norm != null);
    const t = norm && norm.content && norm.content[0];
    eq('norm-resp-anthropic-stream: text', t && t.text, 'I am Claude.');
    eq('norm-resp-anthropic-stream: stop_reason', norm && norm.stop_reason, 'end_turn');
    eq('norm-resp-anthropic-stream: usage preserved',
        norm && norm.usage && norm.usage.input_tokens, 7);
}

// --- Anthropic: non-streaming JSON response ---

{
    const body = {
        id: 'm', role: 'assistant', model: 'claude-3',
        content: [{ type: 'text', text: 'Hi from Claude.' }],
        stop_reason: 'end_turn',
        usage: { input_tokens: 5, output_tokens: 4 },
    };
    const norm = normalizeResponse(body, '/v1/messages');
    check('norm-resp-anthropic-json: not null', norm != null);
    const t = norm && norm.content && norm.content[0];
    eq('norm-resp-anthropic-json: text', t && t.text, 'Hi from Claude.');
}

// --- OpenAI Responses: streaming SSE response ---

{
    const stream =
        'event: response.created\ndata: {"response":{"id":"r","object":"response","status":"in_progress","output":[]}}\n\n' +
        'event: response.output_item.added\ndata: {"output_index":0,"item":{"type":"message","role":"assistant","content":[]}}\n\n' +
        'event: response.output_text.delta\ndata: {"output_index":0,"delta":"Sure"}\n\n' +
        'event: response.output_text.delta\ndata: {"output_index":0,"delta":" thing."}\n\n' +
        'event: response.completed\ndata: {"response":{"id":"r","object":"response","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":2}}}\n\n';
    const r = new SSEReassembler();
    r.feed(stream);
    const snap = r.reconstruct();
    const norm = normalizeResponse(snap, '/v1/responses');
    check('norm-resp-responses-stream: not null', norm != null);
    const t = norm && norm.content && norm.content[0];
    eq('norm-resp-responses-stream: text', t && t.text, 'Sure thing.');
    eq('norm-resp-responses-stream: status', norm && norm.status, 'completed');
}

// --- OpenAI Responses: non-streaming JSON response ---

{
    const body = {
        id: 'r', object: 'response', status: 'completed',
        output: [{ type: 'message', role: 'assistant', content: [{ type: 'output_text', text: 'Done.' }] }],
        usage: { input_tokens: 1, output_tokens: 1 },
    };
    const norm = normalizeResponse(body, '/v1/responses');
    check('norm-resp-responses-json: not null', norm != null);
    const t = norm && norm.content && norm.content[0];
    eq('norm-resp-responses-json: text', t && t.text, 'Done.');
}

// --- Gemini: streaming SSE response ---

{
    const stream =
        'data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"Hello"}]}}]}\n\n' +
        'data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":" there."}]}}]}\n\n' +
        'data: {"candidates":[{"index":0,"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":2}}\n\n';
    const r = new SSEReassembler();
    r.feed(stream);
    const snap = r.reconstruct();
    const norm = normalizeResponse(snap, '/v1beta/models/x:streamGenerateContent');
    check('norm-resp-gemini-stream: not null', norm != null);
    const t = norm && norm.content && norm.content[0];
    eq('norm-resp-gemini-stream: text', t && t.text, 'Hello there.');
    eq('norm-resp-gemini-stream: usage',
        norm && norm.usage && norm.usage.output_tokens, 2);
}

// --- Gemini: non-streaming JSON response ---

{
    const body = {
        candidates: [{
            content: { role: 'model', parts: [{ text: 'Hi from Gemini.' }] },
            finishReason: 'STOP',
        }],
        usageMetadata: { promptTokenCount: 1, candidatesTokenCount: 4 },
    };
    const norm = normalizeResponse(body, '/v1beta/models/x:generateContent');
    check('norm-resp-gemini-json: not null', norm != null);
    const t = norm && norm.content && norm.content[0];
    eq('norm-resp-gemini-json: text', t && t.text, 'Hi from Gemini.');
}

// --- Reasoning-only OpenAI Chat stream (Qwen3.5 / DeepSeek-R1 / o1 style) ---
//
// Real-world: many reasoning models stream only `reasoning_content` deltas
// during their thinking phase, then a separate stream of `content` deltas
// for the final answer. The first event typically carries both
// `role:'assistant'` and `content:null`, then reasoning chunks arrive, then
// content chunks. This was the regression case where the parsed view
// appeared empty.

{
    const stream =
        'data: {"choices":[{"finish_reason":null,"index":0,"delta":{"role":"assistant","content":null}}]}\n\n' +
        'data: {"choices":[{"finish_reason":null,"index":0,"delta":{"reasoning_content":"Thinking"}}]}\n\n' +
        'data: {"choices":[{"finish_reason":null,"index":0,"delta":{"reasoning_content":" Process"}}]}\n\n' +
        'data: {"choices":[{"finish_reason":null,"index":0,"delta":{"reasoning_content":":"}}]}\n\n' +
        'data: {"choices":[{"finish_reason":null,"index":0,"delta":{"content":"Hello!"}}]}\n\n' +
        'data: {"choices":[{"finish_reason":null,"index":0,"delta":{"content":" world"}}]}\n\n' +
        'data: {"choices":[{"finish_reason":"stop","index":0,"delta":{}}]}\n\n' +
        'data: [DONE]\n';
    const r = new SSEReassembler();
    r.feed(stream);
    const snap = r.reconstruct();
    eq('reasoning: message.content', snap.choices[0].message.content, 'Hello! world');
    eq('reasoning: message.reasoning_content',
        snap.choices[0].message.reasoning_content, 'Thinking Process:');
    const norm = normalizeResponse(snap, '/v1/chat/completions');
    check('reasoning: not null', norm != null);
    check('reasoning: has blocks', norm && norm.content && norm.content.length >= 1,
        'blocks=' + JSON.stringify(norm && norm.content));
    // First block should be thinking (placed before text in the mirror).
    eq('reasoning: thinking first', norm.content[0].type, 'thinking');
    eq('reasoning: thinking text', norm.content[0].thinking, 'Thinking Process:');
    if (norm.content.length >= 2) {
        eq('reasoning: text second', norm.content[1].type, 'text');
        eq('reasoning: text content', norm.content[1].text, 'Hello! world');
    }
}

// --- Reasoning-only stream with NO content (only thinking, then stop) ---
// Edge case: model only produced thinking, never a final content answer.
// The parsed view should still render the thinking block.

{
    const stream =
        'data: {"choices":[{"finish_reason":null,"index":0,"delta":{"role":"assistant","content":null}}]}\n\n' +
        'data: {"choices":[{"finish_reason":null,"index":0,"delta":{"reasoning_content":"Just thinking..."}}]}\n\n' +
        'data: {"choices":[{"finish_reason":"stop","index":0,"delta":{}}]}\n\n' +
        'data: [DONE]\n';
    const r = new SSEReassembler();
    r.feed(stream);
    const snap = r.reconstruct();
    const norm = normalizeResponse(snap, '/v1/chat/completions');
    check('reasoning-only: thinking block present',
        norm && norm.content && norm.content.length === 1 &&
        norm.content[0].type === 'thinking' &&
        norm.content[0].thinking === 'Just thinking...',
        'blocks=' + JSON.stringify(norm && norm.content));
}

// ---------- Format detection ----------
//
// The path is a *hint*; the body's shape is authoritative. We test the
// realistic auto-translation scenarios where the URL path and body shape
// don't match (because an upstream client posts to /v1/responses but the
// downstream is Chat Completions-shaped, or vice versa).

{
    eq('detect: chat request body+chat path',
        detectRequestFormat('/v1/chat/completions', { messages: [] }), 'chat_completions');
    eq('detect: anthropic path wins (URL is unambiguous)',
        detectRequestFormat('/v1/messages', { messages: [], system: '' }), 'anthropic');
    eq('detect: responses path+responses body',
        detectRequestFormat('/v1/responses', { input: [] }), 'responses');
    eq('detect: gemini path wins',
        detectRequestFormat('/v1beta/models/x:streamGenerateContent', { contents: [] }), 'gemini');
    // Auto-translation reality: client posts to /v1/responses, downstream
    // returns Chat Completions-shape (choices[]). Body shape must win.
    eq('detect: /v1/responses + Chat Completions body',
        detectRequestFormat('/v1/responses', { choices: [{ delta: {} }] }), 'chat_completions');
    // Reverse: client posts to /v1/chat/completions, downstream returns
    // Responses-shape (output[]/object:'response').
    eq('detect: /v1/chat/completions + Responses body',
        detectRequestFormat('/v1/chat/completions',
            { object: 'response', output: [{ type: 'message' }] }), 'responses');
}

// ---------- Reasoning-only stream with NO content (only thinking, then stop) ----------
// Edge case: model only produced thinking, never a final content answer.
// The parsed view should still render the thinking block.

{
    const stream =
        'data: {"choices":[{"finish_reason":null,"index":0,"delta":{"role":"assistant","content":null}}]}\n\n' +
        'data: {"choices":[{"finish_reason":null,"index":0,"delta":{"reasoning_content":"Just thinking..."}}]}\n\n' +
        'data: {"choices":[{"finish_reason":"stop","index":0,"delta":{}}]}\n\n' +
        'data: [DONE]\n';
    const r = new SSEReassembler();
    r.feed(stream);
    const snap = r.reconstruct();
    const norm = normalizeResponse(snap, '/v1/chat/completions');
    check('reasoning-only: thinking block present',
        norm && norm.content && norm.content.length === 1 &&
        norm.content[0].type === 'thinking' &&
        norm.content[0].thinking === 'Just thinking...',
        'blocks=' + JSON.stringify(norm && norm.content));
}

// ---------- Injected XML wrapper detection (Claude Code) ----------
//
// Claude Code's user messages sometimes contain content blocks whose
// text is wrapped in <system-reminder>...</system-reminder> (or
// similar wrapper tags). The inspector's normaliser tags those blocks
// with type='system_reminder' so the renderer can show them as a
// dimmed callout. We do NOT filter the wrapped content out — the
// inspector's job is to show what was actually sent, and a malicious
// client could use a system-reminder to influence the operator's
// reading of the trace.
{
    const block = { type: 'text', text: '<system-reminder>As you answer the user\'s questions, you can use the following context:\n\n# foo</system-reminder>' };
    const out = detectInjectedWrapper(block);
    eq('wrapper: system-reminder type', out.type, 'system_reminder');
    eq('wrapper: system-reminder tag', out.tag, 'system-reminder');
    eq('wrapper: system-reminder label', out.label, 'System reminder');
    eq('wrapper: system-reminder inner text', out.text, "As you answer the user's questions, you can use the following context:\n\n# foo");
}
{
    // local-command-caveat is the tag Claude Code wraps around /local
    // command instructions. Same treatment.
    const out = detectInjectedWrapper({ type: 'text', text: '<local-command-caveat>caveat body here</local-command-caveat>' });
    eq('wrapper: local-command-caveat', out.type, 'system_reminder');
    eq('wrapper: local-command-caveat label', out.label, 'Local command caveat');
}
{
    // Unknown wrapper tag (e.g. a tag the user typed in chat) must NOT
    // be wrapped. Only the whitelist is touched.
    const original = { type: 'text', text: '<my-custom-tag>hello</my-custom-tag>' };
    const out = detectInjectedWrapper(original);
    eq('wrapper: unknown tag passes through', out, original);
}
{
    // Partial wrap (text starts with a tag but doesn't end with the
    // matching close) must NOT be wrapped — only full wraps are tagged.
    const original = { type: 'text', text: '<system-reminder>incomplete' };
    const out = detectInjectedWrapper(original);
    eq('wrapper: partial wrap passes through', out, original);
}
{
    // Plain text passes through untouched.
    const original = { type: 'text', text: 'hi' };
    const out = detectInjectedWrapper(original);
    eq('wrapper: plain text passes through', out, original);
}
{
    // Non-text blocks (image, tool_use) must not be touched.
    const img = { type: 'image', url: 'http://x' };
    const tu = { type: 'tool_use', name: 'f' };
    eq('wrapper: image untouched', detectInjectedWrapper(img), img);
    eq('wrapper: tool_use untouched', detectInjectedWrapper(tu), tu);
}
{
    // The exact scenario the user reported: a Claude Code turn where
    // the system-reminder is the first text block and the actual user
    // text is the second. Both blocks survive normaliseContentBlocks;
    // only the first is re-tagged.
    const blocks = [
        { type: 'text', text: '<system-reminder>As you answer the user\'s questions, you can use the following context: # TodoWrite, # Read, # Bash</system-reminder>' },
        { type: 'text', text: 'hi' },
    ];
    const normalised = blocks.map(detectInjectedWrapper);
    eq('scenario: reminder block tagged', normalised[0].type, 'system_reminder');
    eq('scenario: user text untouched', normalised[1].type, 'text');
    eq('scenario: user text preserved', normalised[1].text, 'hi');
}

// ---------- tool_result normalisation ----------
//
// tool_result is the Anthropic user-turn block that carries the
// output of a previous tool_use. The user's actual file
// (claude_code_request.txt) has one of these — a string payload
// (the contents of setup.sh) tagged with tool_use_id. The
// normaliser must turn it into a structured block so the renderer
// can show "Tool result (id)" + the body, instead of dumping the
// whole object as a JSON <pre>.
{
    const blocks = normaliseContentBlocks([
        {
            type: 'tool_result',
            tool_use_id: 'abc123',
            content: '1\t#!/usr/bin/env bash\n2\t# Tresor — setup\n',
            cache_control: { type: 'ephemeral' },
        },
    ]);
    eq('tool_result: count', blocks.length, 1);
    eq('tool_result: type', blocks[0].type, 'tool_result');
    eq('tool_result: tool_use_id preserved', blocks[0].tool_use_id, 'abc123');
    check('tool_result: content preserved as string',
        typeof blocks[0].content === 'string' &&
        blocks[0].content.indexOf('Tresor') !== -1,
        'content=' + JSON.stringify(blocks[0].content).slice(0, 80));
    check('tool_result: cache_control dropped (prompt-cache marker, not content)',
        blocks[0].cache_control === undefined,
        'block=' + JSON.stringify(blocks[0]));
    eq('tool_result: is_error defaults false', blocks[0].is_error, false);
}
{
    // is_error: true must be coerced to a boolean.
    const blocks = normaliseContentBlocks([
        { type: 'tool_result', tool_use_id: 'x', content: 'oops', is_error: 1 },
    ]);
    eq('tool_result: is_error coerced to bool', blocks[0].is_error, true);
}
{
    // Array-of-blocks content (text + image) survives unchanged.
    const blocks = normaliseContentBlocks([
        {
            type: 'tool_result',
            tool_use_id: 'screenshot',
            content: [
                { type: 'text', text: 'captured 1280x720' },
                { type: 'image', source: { type: 'base64', media_type: 'image/png', data: '...' } },
            ],
        },
    ]);
    eq('tool_result-array: count', blocks.length, 1);
    eq('tool_result-array: type', blocks[0].type, 'tool_result');
    check('tool_result-array: content array preserved',
        Array.isArray(blocks[0].content) && blocks[0].content.length === 2 &&
        blocks[0].content[0].text === 'captured 1280x720' &&
        blocks[0].content[1].source && blocks[0].content[1].source.media_type === 'image/png',
        'content=' + JSON.stringify(blocks[0].content).slice(0, 100));
}
{
    // Empty content should still produce a tool_result block (the
    // renderer shows "(empty result)" rather than dropping the
    // entry).
    const blocks = normaliseContentBlocks([
        { type: 'tool_result', tool_use_id: 'empty', content: '' },
    ]);
    eq('tool_result-empty: count', blocks.length, 1);
    eq('tool_result-empty: type', blocks[0].type, 'tool_result');
    eq('tool_result-empty: content is empty string', blocks[0].content, '');
}
{
    // The exact user scenario: a Claude Code user-turn whose
    // content is a tool_result carrying a long bash file as
    // string. Must not fall through to {type:'raw'}.
    const userContent = [
        { type: 'tool_result', tool_use_id: '124JP', content: '#!/usr/bin/env bash\nset -e' },
    ];
    const blocks = normaliseContentBlocks(userContent);
    eq('user-scenario: tool_result NOT raw',
        blocks[0].type === 'tool_result' && blocks[0].type !== 'raw', true);
    eq('user-scenario: content preserved',
        typeof blocks[0].content === 'string' && blocks[0].content.indexOf('bash') !== -1, true);
}
{
    // Mixed content: text + tool_result in the same user message
    // should both be normalised correctly, in order.
    const blocks = normaliseContentBlocks([
        { type: 'text', text: 'hi' },
        { type: 'tool_result', tool_use_id: 'x', content: 'result' },
    ]);
    eq('mixed: first block text', blocks[0].type, 'text');
    eq('mixed: second block tool_result', blocks[1].type, 'tool_result');
    eq('mixed: order preserved', blocks[1].content, 'result');
}

// ---------- summarise ----------

console.log(`\n${passed} passed, ${failed} failed`);
process.exit(failed ? 1 : 0);
