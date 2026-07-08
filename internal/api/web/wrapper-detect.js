// wrapper-detect.js — detects text blocks wrapped in Claude Code-style
// injected XML tags (e.g. <system-reminder>, <local-command-caveat>).
// Loaded by the Web UI BEFORE app.js (see index.html) so app.js can
// call the global detectInjectedWrapper. Tested directly under Node
// from sse-reassembler.test.js.
//
// Kept in its own file so it can be unit-tested without pulling in the
// rest of app.js (which is heavily DOM-coupled and doesn't load under
// plain Node).

'use strict';

(function () {
    // Known Claude Code-injected wrapper tags. Anything else passes
    // through unchanged so we don't mangle legitimate user text that
    // happens to contain XML-like syntax.
    const KNOWN_INJECTED_TAGS = {
        'system-reminder':         'System reminder',
        'local-command-caveat':    'Local command caveat',
        'command-name':            'Command name',
        'command-message':         'Command message',
        'command-args':            'Command args',
        'local-command-stdout':    'Local command stdout',
        'user-message-subcontent': 'User message subcontent',
        'environment_context':     'Environment context',
    };

    // Match a single XML tag that wraps the whole block. We look for a
    // top-level opening tag, allow some leading whitespace, and require
    // the matching closing tag at the end of the trimmed text. This
    // intentionally does NOT match partial wraps (e.g. the user's text
    // contains a literal "<system-reminder>" string) — only blocks that
    // are entirely a wrapper.
    const INJECTED_RE = /^\s*<([a-zA-Z][\w-]*)>([\s\S]*)<\/\1>\s*$/;

    function detectInjectedWrapper(block) {
        if (!block || block.type !== 'text' || typeof block.text !== 'string') {
            return block;
        }
        const m = block.text.match(INJECTED_RE);
        if (!m) return block;
        const tag = m[1].toLowerCase();
        const label = KNOWN_INJECTED_TAGS[tag];
        if (!label) return block;
        return {
            type: 'system_reminder',
            tag: tag,
            label: label,
            text: m[2],
        };
    }

    // Export pattern matches sse-reassembler.js: browser attaches to
    // window, Node attaches to globalThis.
    if (typeof window !== 'undefined') {
        window.detectInjectedWrapper = detectInjectedWrapper;
    } else if (typeof globalThis !== 'undefined') {
        globalThis.detectInjectedWrapper = detectInjectedWrapper;
    }
})();
