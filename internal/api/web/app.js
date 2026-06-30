// Tresor Web UI — Admin Dashboard

const API_BASE = '/api';

// ---- Theme detection & toggle (session-only, no persistence) ----
(function initTheme() {
    const prefersDark = window.matchMedia('(prefers-color-scheme: dark)').matches;
    document.documentElement.setAttribute('data-theme', prefersDark ? 'dark' : 'light');

    const toggle = document.getElementById('theme-toggle');
    if (toggle) {
        toggle.addEventListener('click', function () {
            const current = document.documentElement.getAttribute('data-theme');
            const next = current === 'dark' ? 'light' : 'dark';
            document.documentElement.setAttribute('data-theme', next);
        });
    }

    // Update theme when browser preference changes while the page is open
    window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', function (e) {
        document.documentElement.setAttribute('data-theme', e.matches ? 'dark' : 'light');
    });
})();

// ---- Tab switching ----
/**
 * Activate a tab by its data-tab ID (e.g. "rules", "settings", "about").
 * Falls back to "downstreams" if the tab is not found.
 */
function activateTab(tabId) {
    tabId = tabId || 'downstreams';
    let tabBtn = document.querySelector('.tab[data-tab="' + tabId + '"]');
    if (!tabBtn) tabId = 'downstreams';
    document.querySelectorAll('.tab').forEach(t => t.classList.remove('active'));
    document.querySelectorAll('.tab-content').forEach(tc => tc.classList.remove('active'));
    tabBtn = document.querySelector('.tab[data-tab="' + tabId + '"]');
    tabBtn.classList.add('active');
    document.getElementById('tab-' + tabId).classList.add('active');
}

document.querySelectorAll('.tab').forEach(tab => {
    tab.addEventListener('click', () => {
        activateTab(tab.dataset.tab);
    });
});

// ---- Auth state ----
let authEnabled = false;

/**
 * Check whether the server requires authentication, then decide
 * whether to show the login screen or the dashboard.
 * Auth is cookie-based — the browser sends cookies automatically.
 */
async function checkAuth() {
    try {
        const status = await fetch(API_BASE + '/auth/status').then(r => r.json());
        authEnabled = !!status.auth_enabled;
    } catch {
        // If the status endpoint fails, assume no auth (server might be old version)
        authEnabled = false;
    }

    if (!authEnabled) {
        // No auth required — show dashboard immediately
        showDashboardWithDefaultTab();
        return;
    }

    // Auth is enabled: verify the cookie still works by hitting a protected endpoint
    try {
        const resp = await fetch(API_BASE + '/rules', {
            credentials: 'same-origin',
        });
        if (resp.ok) {
            showDashboardWithDefaultTab();
            return;
        }
    } catch {
        // Network error — show login anyway
    }

    // Cookie invalid or missing — show login
    showLogin();
}

function showLogin() {
    document.getElementById('login-view').classList.remove('hidden');
    document.getElementById('dashboard-view').classList.add('hidden');
}

function showDashboard() {
    document.getElementById('login-view').classList.add('hidden');
    document.getElementById('dashboard-view').classList.remove('hidden');
    // Load all tabs' data
    loadRules();
    loadDownstreams();
    loadPlugins();
    loadAliasGroups();
    loadSettings();
    initLogs();
    loadAbout();
}

/**
 * Show dashboard and activate the server-configured default tab.
 * Called after auth check or login succeeds.
 */
async function showDashboardWithDefaultTab() {
    showDashboard();
    try {
        const cfg = await api('/config');
        activateTab(cfg.default_tab || 'downstreams');
    } catch {
        activateTab('downstreams');
    }
}

function logout() {
    // Close SSE connection if active
    if (logSSE) { logSSE.close(); logSSE = null; }
    // Notify the backend to invalidate the session (clears the cookie)
    fetch(API_BASE + '/auth/logout', { method: 'POST', credentials: 'same-origin' }).catch(function () { });
    showLogin();
    // Clear the password field
    const pw = document.getElementById('login-password');
    if (pw) pw.value = '';
}

// ---- Login form handler ----
document.getElementById('login-form').addEventListener('submit', async function (e) {
    e.preventDefault();
    const errorEl = document.getElementById('login-error');
    errorEl.classList.add('hidden');
    errorEl.textContent = '';

    const password = document.getElementById('login-password').value;
    try {
        const resp = await fetch(API_BASE + '/auth/login', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ password: password }),
            credentials: 'same-origin',
        });
        if (!resp.ok) {
            const errData = await resp.json().catch(() => ({}));
            throw new Error(errData.error || 'Login failed');
        }
        // Server sets the auth cookie via Set-Cookie header (no token in response body)
        showDashboardWithDefaultTab();
    } catch (err) {
        errorEl.textContent = err.message || 'Invalid password';
        errorEl.classList.remove('hidden');
    }
});

// ---- Logout button ----
document.getElementById('btn-logout').addEventListener('click', logout);

// ---- API helpers ----
/**
 * Build headers for fetch() calls. Auth is handled via cookies
 * (sent automatically with credentials: 'same-origin').
 */
function buildAuthHeaders() {
    return { 'Content-Type': 'application/json' };
}

async function api(path, options = {}) {
    const url = API_BASE + path;
    const headers = { ...buildAuthHeaders(), ...options.headers };
    const resp = await fetch(url, {
        headers: headers,
        credentials: 'same-origin',
        ...options,
    });
    // If we get 401 and auth is enabled, the cookie may have been invalidated
    // (e.g., password changed). Try once more; if it still fails, logout.
    if (resp.status === 401 && authEnabled) {
        const resp2 = await fetch(url, {
            headers: headers,
            credentials: 'same-origin',
            ...options,
        });
        if (resp2.status === 401) {
            logout();
            return;
        }
        if (!resp2.ok) {
            const err = await resp2.json().catch(() => ({ error: resp2.statusText }));
            throw new Error(err.error || resp2.statusText);
        }
        return resp2.json();
    }
    if (!resp.ok) {
        const err = await resp.json().catch(() => ({ error: resp.statusText }));
        throw new Error(err.error || resp.statusText);
    }
    return resp.json();
}

// ---- Plugin cache (for visual pipeline editor) ----
let cachedPlugins = null;

// ---- Format icon helper ----
// Maps API format ids to the SVG icon file to display alongside the text label.
const FORMAT_ICONS = {
    openai: 'icons/openai-completions.svg',
    openai_responses: 'icons/openai-responses.svg',
    anthropic: 'icons/anthropic.svg',
    google: 'icons/google.svg',
};

function formatIconHTML(formatId) {
    const src = FORMAT_ICONS[formatId];
    if (!src) return '';
    return `<img class="format-icon icon-${esc(formatId)}" src="${esc(src)}" alt="" aria-hidden="true">`;
}

// ---- Model icon helper ----
// Returns an <img> tag pointing at /api/icons/{modelID}. The daemon looks up
// the model name against its pattern table, lazily fetches the matching SVG
// from a public CDN on first miss, and serves it back. If the model doesn't
// match any pattern the endpoint returns 404 and the onerror handler hides
// the broken image — so it's safe to call for any model ID.
function modelIconHTML(modelID) {
    if (!modelID) return '';
    return `<img class="model-icon" src="/api/icons/${encodeURIComponent(modelID)}" alt="" loading="lazy" onerror="this.style.display='none'">`;
}

async function fetchPlugins() {
    if (cachedPlugins) return cachedPlugins;
    try {
        cachedPlugins = await api('/plugins');
    } catch {
        cachedPlugins = [];
    }
    return cachedPlugins;
}

// ---- Downstream cache (shared across rules and aliases) ----
let cachedDownstreams = null;

async function fetchDownstreams() {
    if (cachedDownstreams) return cachedDownstreams;
    try {
        cachedDownstreams = await api('/downstreams');
    } catch {
        cachedDownstreams = [];
    }
    return cachedDownstreams;
}

// ---- Rules ----
async function loadRules() {
    const tbody = document.getElementById('rules-body');
    try {
        const rules = await api('/rules');
        tbody.innerHTML = rules.length === 0
            ? '<tr><td colspan="7" class="loading">No rules configured</td></tr>'
            : rules.map(r => {
                // Build match badges from the three optional match fields
                const badges = [];
                const inputFmts = r.match_format || [];
                const dsFmts = r.match_downstream_format || [];
                const dsIds = r.match_downstreams || [];
                const formatLabels = { openai: 'OpenAI', openai_responses: 'OpenAI Responses', anthropic: 'Anthropic' };

                // Input format badges (blue for openai, amber for anthropic)
                inputFmts.forEach(f => {
                    const cls = f === 'openai' ? 'format-openai' : f === 'openai_responses' ? 'format-openai_responses' : f === 'anthropic' ? 'format-anthropic' : 'format-unknown';
                    badges.push(`<span class="format-badge ${cls}">in:${formatIconHTML(f)}${esc(formatLabels[f] || f)}</span>`);
                });

                // Downstream format badges (prefixed to distinguish from input)
                dsFmts.forEach(f => {
                    const cls = f === 'openai' ? 'format-openai' : f === 'openai_responses' ? 'format-openai_responses' : f === 'anthropic' ? 'format-anthropic' : 'format-unknown';
                    badges.push(`<span class="format-badge ${cls}">out:${formatIconHTML(f)}${esc(formatLabels[f] || f)}</span>`);
                });

                // Downstream ID badges (grey)
                dsIds.forEach(id => {
                    const ds = (cachedDownstreams || []).find(d => d.id === id);
                    const label = ds ? ds.name : id;
                    badges.push(`<span class="badge">ds:${esc(label)}</span>`);
                });

                const matchCell = badges.length > 0
                    ? badges.join(' ')
                    : '<span class="format-badge format-unknown">any</span>';

                return `
                <tr>
                    <td><strong>${esc(r.name)}</strong></td>
                    <td><code>${esc(r.pattern_path)}</code></td>
                    <td><code>${esc(r.pattern_model) || '—'}</code></td>
                    <td>${matchCell}</td>
                    <td><span class="pipeline-steps">${esc(shortPipeline(r.pipeline_config))}</span></td>
                    <td><span class="status-badge ${r.is_enabled ? 'status-enabled' : 'status-disabled'}">${r.is_enabled ? 'ON' : 'OFF'}</span></td>
                    <td>
                        <button class="btn-small rule-edit-btn" data-action="edit-rule" data-id="${esc(r.id)}">Edit</button>
                        <button class="btn-small rule-toggle-btn" data-action="toggle-rule" data-id="${esc(r.id)}" data-enabled="${r.is_enabled ? '0' : '1'}">${r.is_enabled ? 'Disable' : 'Enable'}</button>
                        <button class="btn-danger rule-delete-btn" data-action="delete-rule" data-id="${esc(r.id)}">Delete</button>
                    </td>
                </tr>`;
            }).join('');
    } catch (err) {
        tbody.innerHTML = `<tr><td colspan="7" class="loading">Error: ${esc(err.message)}</td></tr>`;
    }
}

// ---- Visual Pipeline Editor ----

/**
 * Build the visual pipeline editor UI.
 * - Loads available plugins
 * - Renders existing steps or starts empty
 * - Each step has a plugin selector + dynamic config fields
 */
async function initPipelineEditor(steps) {
    const plugins = await fetchPlugins();
    const container = document.getElementById('pipeline-steps');
    container.innerHTML = '';

    // Always set up the "Add Step" button first
    const addBtn = document.getElementById('btn-add-step');
    addBtn.onclick = () => addPipelineStep(plugins);

    if (!steps || steps.length === 0) {
        container.innerHTML = '<div class="pipeline-empty">No transformers configured</div>';
        return;
    }

    steps.forEach((step, idx) => {
        createStepCard(container, step, plugins, idx);
    });
}

/**
 * Create a DOM card for a single pipeline step.
 */
function createStepCard(container, step, plugins, idx) {
    const pluginInfo = plugins.find(p => p.id === step.plugin_id);
    if (!pluginInfo) return;

    const card = document.createElement('div');
    card.className = 'pipeline-step-card';
    card.dataset.idx = idx;

    // Header: plugin selector + remove button
    const header = document.createElement('div');
    header.className = 'pipeline-step-header';

    const select = document.createElement('select');
    select.className = 'step-plugin-select';
    plugins.forEach(p => {
        const opt = document.createElement('option');
        opt.value = p.id;
        opt.textContent = `${p.id}`;
        if (p.id === step.plugin_id) opt.selected = true;
        select.appendChild(opt);
    });

    const removeBtn = document.createElement('button');
    removeBtn.className = 'pipeline-step-remove';
    removeBtn.textContent = 'Remove';
    removeBtn.onclick = () => {
        card.remove();
        reindexSteps();
        if (container.querySelectorAll('.pipeline-step-card').length === 0) {
            container.innerHTML = '<div class="pipeline-empty">No transformers configured</div>';
        }
    };

    header.appendChild(select);
    header.appendChild(removeBtn);
    card.appendChild(header);

    // Description line
    const descEl = document.createElement('div');
    descEl.className = 'pipeline-step-description';
    descEl.textContent = pluginInfo.description;
    card.appendChild(descEl);

    // Config section
    const configDiv = document.createElement('div');
    configDiv.className = 'pipeline-step-config';
    configDiv.id = 'step-config-' + idx;

    buildConfigUI(configDiv, pluginInfo, step.config || {}, plugins);

    // When plugin selector changes, rebuild config UI
    select.onchange = () => {
        const newPlugin = plugins.find(p => p.id === select.value);
        if (!newPlugin) return;
        select.textContent = '';
        plugins.forEach(p => {
            const opt = document.createElement('option');
            opt.value = p.id;
            opt.textContent = `${p.id}`;
            if (p.id === newPlugin.id) opt.selected = true;
            select.appendChild(opt);
        });
        descEl.textContent = newPlugin.description;
        configDiv.innerHTML = '';
        buildConfigUI(configDiv, newPlugin, {}, plugins);
    };

    card.appendChild(configDiv);
    container.appendChild(card);
}

/**
 * Build config input fields based on plugin schema.
 */
function buildConfigUI(container, pluginInfo, currentConfig, plugins) {
    const schema = pluginInfo.config_schema;
    if (!schema || !schema.properties || Object.keys(schema.properties).length === 0) {
        // No config needed (e.g. openai2anthropic, anthropic2openai)
        container.innerHTML = '<div style="color:#8b949e;font-size:0.75rem;font-style:italic">No configuration required</div>';
        return;
    }

    const required = schema.required || [];

    // Handle custom_header plugin specially (key-value map)
    if (pluginInfo.id === 'custom_header') {
        buildHeaderPairs(container, currentConfig);
        return;
    }

    // Generic: render one input per property
    Object.entries(schema.properties).forEach(([key, prop]) => {
        const row = document.createElement('div');
        row.className = 'config-row';

        const label = document.createElement('label');
        label.textContent = key + (required.includes(key) ? '*' : '');
        row.appendChild(label);

        const input = document.createElement('input');
        input.type = 'text';
        input.className = 'step-config-input';
        input.dataset.key = key;
        input.placeholder = prop.description || '';

        // Restore current value
        if (currentConfig[key] !== undefined) {
            input.value = String(currentConfig[key]);
        }

        row.appendChild(input);
        container.appendChild(row);
    });
}

/**
 * Build header key-value pairs editor for custom_header plugin.
 */
function buildHeaderPairs(container, currentConfig) {
    const headersObj = currentConfig.headers || {};
    const pairsContainer = document.createElement('div');
    pairsContainer.className = 'header-pairs-container';

    if (Object.keys(headersObj).length === 0) {
        pairsContainer.innerHTML = '<div style="color:#8b949e;font-size:0.75rem;font-style:italic">No headers configured</div>';
    } else {
        Object.entries(headersObj).forEach(([k, v]) => {
            addHeaderPairRow(pairsContainer, k, v);
        });
    }

    container.appendChild(pairsContainer);

    const addBtn = document.createElement('button');
    addBtn.className = 'btn-add-header';
    addBtn.textContent = '+ Add Header';
    addBtn.onclick = () => addHeaderPairRow(pairsContainer, '', '');
    container.appendChild(addBtn);
}

function addHeaderPairRow(container, key, value) {
    const row = document.createElement('div');
    row.className = 'header-pair-row';

    const keyInput = document.createElement('input');
    keyInput.placeholder = 'Header name';
    keyInput.className = 'header-key';
    keyInput.value = key;

    const valInput = document.createElement('input');
    valInput.placeholder = 'Value';
    valInput.className = 'header-value';
    valInput.value = value;

    const removeBtn = document.createElement('button');
    removeBtn.className = 'header-pair-remove';
    removeBtn.textContent = '×';
    removeBtn.onclick = () => row.remove();

    row.appendChild(keyInput);
    row.appendChild(valInput);
    row.appendChild(removeBtn);
    container.appendChild(row);
}

/**
 * Reindex step cards after removal.
 */
function reindexSteps() {
    document.querySelectorAll('.pipeline-step-card').forEach((card, idx) => {
        card.dataset.idx = idx;
        const configDiv = card.querySelector('[id^="step-config-"]');
        if (configDiv) {
            configDiv.id = 'step-config-' + idx;
        }
    });
}

/**
 * Add a new pipeline step (default plugin).
 */
function addPipelineStep(plugins) {
    const container = document.getElementById('pipeline-steps');
    // Remove empty message if present
    const empty = container.querySelector('.pipeline-empty');
    if (empty) empty.remove();

    const idx = container.querySelectorAll('.pipeline-step-card').length;
    const step = { plugin_id: plugins[0] ? plugins[0].id : 'custom_header', config: {} };
    createStepCard(container, step, plugins, idx);
}

/**
 * Serialize the visual pipeline editor back to JSON array.
 */
function serializePipeline() {
    const cards = document.querySelectorAll('.pipeline-step-card');
    if (cards.length === 0) return '[]';

    const steps = [];
    cards.forEach(card => {
        const select = card.querySelector('.step-plugin-select');
        const pluginId = select.value;
        const configDiv = card.querySelector('.pipeline-step-config');
        const pluginInfo = cachedPlugins.find(p => p.id === pluginId);

        // Skip steps with no valid plugin selected
        if (!pluginInfo || !pluginId) return;

        const config = {};

        // custom_header: collect header key-value pairs
        if (pluginInfo.id === 'custom_header') {
            const headers = {};
            const pairRows = configDiv.querySelectorAll('.header-pair-row');
            pairRows.forEach(row => {
                const k = row.querySelector('.header-key').value.trim();
                const v = row.querySelector('.header-value').value.trim();
                if (k) headers[k] = v;
            });
            if (Object.keys(headers).length > 0) config.headers = headers;
        } else {
            // Generic: collect input values
            const inputs = configDiv.querySelectorAll('.step-config-input');
            inputs.forEach(input => {
                if (input.value.trim()) {
                    config[input.dataset.key] = input.value.trim();
                }
            });
        }

        steps.push({ plugin_id: pluginId, config });
    });

    return JSON.stringify(steps);
}

/**
 * Parse existing pipeline JSON into step objects.
 */
function parsePipeline(configStr) {
    try {
        const parsed = JSON.parse(configStr || '[]');
        return Array.isArray(parsed) ? parsed : [];
    } catch {
        return [];
    }
}

// ---- Rule Modal ----

/**
 * Populate the downstream multi-select in the rule modal.
 * Uses the existing cachedDownstreams to build clickable tags.
 */
function populateRuleMatchDownstreams(containerEl, selectedIds) {
    containerEl.innerHTML = '';
    const dsList = cachedDownstreams || [];
    const sel = selectedIds || [];

    if (dsList.length === 0) {
        containerEl.innerHTML = '<div class="model-multi-empty">No downstreams available</div>';
        return;
    }

    const wrap = document.createElement('div');
    wrap.className = 'model-tag-wrap';

    dsList.forEach(d => {
        const tag = document.createElement('span');
        tag.className = 'model-tag';
        if (sel.includes(d.id)) tag.classList.add('selected');
        tag.dataset.id = d.id;

        const cb = document.createElement('input');
        cb.type = 'checkbox';
        cb.value = d.id;
        cb.style.display = 'none';
        cb.checked = sel.includes(d.id);
        tag.appendChild(cb);

        const icon = document.createElement('span');
        icon.className = 'model-tag-icon';
        icon.textContent = cb.checked ? '☑' : '☐';
        tag.appendChild(icon);

        const name = document.createElement('span');
        name.className = 'model-tag-name';
        name.textContent = d.name;
        tag.appendChild(name);

        tag.addEventListener('click', () => {
            cb.checked = !cb.checked;
            tag.classList.toggle('selected', cb.checked);
            icon.textContent = cb.checked ? '☑' : '☐';
        });

        wrap.appendChild(tag);
    });

    containerEl.appendChild(wrap);
}

/**
 * Collect checked downstream IDs from the multi-select.
 */
function getSelectedMatchDownstreams(containerEl) {
    const ids = [];
    containerEl.querySelectorAll('.model-tag.selected').forEach(tag => {
        ids.push(tag.dataset.id);
    });
    return ids;
}

/**
 * Collect checked format values from a checkbox group.
 */
function getCheckedFormats(containerEl) {
    const formats = [];
    containerEl.querySelectorAll('input[type="checkbox"]:checked').forEach(cb => {
        formats.push(cb.value);
    });
    return formats;
}

async function openRuleModal(rule) {
    document.getElementById('rule-modal-title').textContent = rule ? 'Edit Rule' : 'New Rule';
    document.getElementById('rule-id').value = rule ? rule.id : '';
    document.getElementById('rule-name').value = rule ? rule.name : '';
    document.getElementById('rule-path').value = rule ? rule.pattern_path : '*';
    document.getElementById('rule-model').value = rule ? (rule.pattern_model || '') : '';
    document.getElementById('rule-enabled').checked = rule ? rule.is_enabled : true;

    // Ensure downstreams cache is loaded for the multi-select
    await fetchDownstreams();

    // Populate match input format checkboxes
    const inputFmtContainer = document.getElementById('rule-match-input-formats');
    const inputFormats = rule ? (rule.match_format || []) : [];
    inputFmtContainer.querySelectorAll('input[type="checkbox"]').forEach(cb => {
        cb.checked = inputFormats.includes(cb.value);
    });

    // Populate match downstream format checkboxes
    const dsFmtContainer = document.getElementById('rule-match-downstream-formats');
    const dsFormats = rule ? (rule.match_downstream_format || []) : [];
    dsFmtContainer.querySelectorAll('input[type="checkbox"]').forEach(cb => {
        cb.checked = dsFormats.includes(cb.value);
    });

    // Populate match downstreams multi-select
    const dsContainer = document.getElementById('rule-match-downstreams');
    populateRuleMatchDownstreams(dsContainer, rule ? (rule.match_downstreams || []) : []);

    // Initialize visual pipeline editor
    const steps = rule ? parsePipeline(rule.pipeline_config) : [];
    await initPipelineEditor(steps);

    document.getElementById('rule-modal').classList.remove('hidden');
}

function editRule(id) {
    api('/rules/' + id).then(rule => openRuleModal(rule)).catch(err => alert(err.message));
}

async function toggleRule(id, enabled) {
    try {
        await api('/rules/' + id, {
            method: 'PUT',
            body: JSON.stringify({ is_enabled: enabled }),
        });
        loadRules();
    } catch (err) {
        alert(err.message);
    }
}

async function deleteRule(id) {
    if (!confirm('Delete this rule?')) return;
    try {
        await api('/rules/' + id, { method: 'DELETE' });
        loadRules();
    } catch (err) {
        alert(err.message);
    }
}

document.getElementById('btn-new-rule').addEventListener('click', async () => {
    await fetchPlugins();
    await fetchDownstreams();
    openRuleModal(null);
});

document.getElementById('rule-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const id = document.getElementById('rule-id').value;

    // Serialize visual pipeline to JSON for the hidden textarea
    document.getElementById('rule-pipeline').value = serializePipeline();

    // Collect match criteria
    const matchFormat = getCheckedFormats(document.getElementById('rule-match-input-formats'));
    const matchDownstreamFormat = getCheckedFormats(document.getElementById('rule-match-downstream-formats'));
    const matchDownstreams = getSelectedMatchDownstreams(document.getElementById('rule-match-downstreams'));

    const body = {
        name: document.getElementById('rule-name').value,
        pattern_path: document.getElementById('rule-path').value,
        pattern_model: document.getElementById('rule-model').value,
        match_format: matchFormat,
        match_downstream_format: matchDownstreamFormat,
        match_downstreams: matchDownstreams,
        pipeline_config: document.getElementById('rule-pipeline').value,
        is_enabled: document.getElementById('rule-enabled').checked,
    };
    try {
        if (id) {
            await api('/rules/' + id, {
                method: 'PUT',
                body: JSON.stringify(body),
            });
        } else {
            await api('/rules', {
                method: 'POST',
                body: JSON.stringify(body),
            });
        }
        document.getElementById('rule-modal').classList.add('hidden');
        loadRules();
    } catch (err) {
        alert('Error: ' + err.message);
    }
});

// ---- Downstreams ----
// Sidebar + detail-pane layout (cherry-studio style). The list on the left
// shows every configured downstream with a brand icon and an ON-style pill.
// The right pane is an INLINE EDITOR: every field (Name, URL, Key, Formats,
// Models) is editable in place and auto-saves on blur or on per-item action.
// ＋ Add Provider creates an empty downstream in the DB; if the user navigates
// away without typing anything into it, the empty stub is auto-deleted.
let _currentDownstreamId = null;

async function loadDownstreams() {
    try {
        const ds = await api('/downstreams');
        cachedDownstreams = ds;
        renderDownstreamSidebar(ds);
        if (_currentDownstreamId && ds.some(d => d.id === _currentDownstreamId)) {
            renderDownstreamDetail(ds.find(d => d.id === _currentDownstreamId));
        } else if (ds.length > 0) {
            _currentDownstreamId = ds[0].id;
            renderDownstreamDetail(ds[0]);
            selectSidebarItem(_currentDownstreamId);
        } else {
            _currentDownstreamId = null;
            document.getElementById('downstreams-detail').innerHTML =
                '<div class="downstreams-detail-empty">No providers yet. Click ＋ Add Provider.</div>';
        }
    } catch (err) {
        document.getElementById('downstreams-detail').innerHTML =
            '<div class="downstreams-detail-empty">Error loading downstreams: ' + esc(err.message) + '</div>';
    }
}

/**
 * Switch the active downstream in the detail pane. If the previously-selected
 * one is an empty stub (just-created and never edited), auto-delete it from
 * the DB so we don't leave orphan empty rows.
 */
async function selectDownstream(id) {
    const prevId = _currentDownstreamId;
    _currentDownstreamId = id;
    // Re-fetch the latest list and check whether `prevId` is an untouched stub.
    if (prevId && prevId !== id) {
        try {
            const list = await api('/downstreams');
            const prev = list.find(d => d.id === prevId);
            if (prev && isEmptyDownstream(prev)) {
                // Fire-and-forget cleanup
                api('/downstreams/' + encodeURIComponent(prevId), { method: 'DELETE' })
                    .catch(() => { /* best-effort */ });
            }
        } catch { /* ignore */ }
    }
    loadDownstreams();
}

function isEmptyDownstream(d) {
    // An "empty stub" is one where the user has not replaced the placeholder
    // values that createNewDownstream seeded (or is truly untouched). Used
    // by selectDownstream() to auto-clean orphan stubs.
    const placeholderName = d.name === 'New Provider';
    const placeholderUrl = d.base_url === 'https://' || d.base_url === '';
    return placeholderName && placeholderUrl && !d.api_key
        && (!d.api_formats || d.api_formats.length === 0)
        && (!d.output_model_ids || d.output_model_ids.length === 0);
}

function renderDownstreamSidebar(list) {
    const ul = document.getElementById('downstreams-list');
    if (!ul) return;
    const q = (document.getElementById('downstreams-search').value || '').toLowerCase().trim();
    const filtered = q ? list.filter(d => (d.name || '').toLowerCase().includes(q)) : list;
    if (filtered.length === 0) {
        ul.innerHTML = '<li style="cursor:default;color:var(--text-secondary);justify-content:center;padding:1rem 0.5rem;font-size:0.8rem;">No matches</li>';
        return;
    }
    ul.innerHTML = filtered.map(d => `
        <li data-id="${esc(d.id)}" class="${d.id === _currentDownstreamId ? 'selected' : ''}">
            ${modelIconHTML(d.name)}
            <span class="ds-name">${esc(d.name)}</span>
            <span class="ds-on-pill">ON</span>
        </li>`).join('');
    ul.querySelectorAll('li[data-id]').forEach(li => {
        li.onclick = () => {
            const id = li.dataset.id;
            const ds = list.find(d => d.id === id);
            if (!ds) return;
            selectSidebarItem(id);
            selectDownstream(id);
        };
    });
}

function selectSidebarItem(id) {
    document.querySelectorAll('#downstreams-list li').forEach(li => {
        li.classList.toggle('selected', li.dataset.id === id);
    });
}

function renderDownstreamDetail(ds) {
    const formats = ds.api_formats || [];
    const models = ds.output_model_ids || [];
    // "***" is the masked form returned by /api/downstreams without ?reveal=1.
    // Treat any non-empty value as "has a key" so the masked placeholder renders
    // and the Reveal button is offered.
    const hasKey = !!(ds.api_key && ds.api_key.length > 0);

    document.getElementById('downstreams-detail').innerHTML = `
        <div class="detail-header">
            ${modelIconHTML(ds.name)}
            <input type="text" class="detail-edit-name" id="edit-name-${esc(ds.id)}" value="${esc(ds.name)}" placeholder="(unnamed provider)" autocomplete="off">
            <div class="header-actions">
                <button class="detail-header-delete" data-action="delete" title="Delete this downstream">🗑</button>
            </div>
        </div>
        <div class="detail-section">
            <label>API Formats</label>
            <div class="format-checkboxes" id="edit-formats-${esc(ds.id)}">
                <label class="format-checkbox"><input type="checkbox" name="format" value="openai"${formats.includes('openai') ? ' checked' : ''}><img class="format-icon icon-openai" src="icons/openai-completions.svg" alt="" aria-hidden="true"> OpenAI</label>
                <label class="format-checkbox"><input type="checkbox" name="format" value="openai_responses"${formats.includes('openai_responses') ? ' checked' : ''}><img class="format-icon icon-openai_responses" src="icons/openai-responses.svg" alt="" aria-hidden="true"> OpenAI Responses</label>
                <label class="format-checkbox"><input type="checkbox" name="format" value="anthropic"${formats.includes('anthropic') ? ' checked' : ''}><img class="format-icon icon-anthropic" src="icons/anthropic.svg" alt="" aria-hidden="true"> Anthropic</label>
            </div>
        </div>
        <div class="detail-section">
            <label>API Key</label>
            <div class="detail-edit-key-wrap">
                <input type="password" class="detail-edit-key" id="edit-key-${esc(ds.id)}" value="" placeholder="${hasKey ? '•••••••••••••••• (saved — type to replace)' : 'sk-…'}" autocomplete="off">
                <button type="button" class="eye-toggle" data-action="toggle-key" title="Reveal / hide the saved key" aria-label="Toggle key visibility">👁</button>
            </div>
        </div>
        <div class="detail-section">
            <label>API Host</label>
            <div class="detail-row">
                <input type="text" class="detail-edit-url" id="edit-url-${esc(ds.id)}" value="${esc(ds.base_url || '')}" placeholder="https://api.example.com/v1" autocomplete="off">
            </div>
        </div>
        <div class="detail-section">
            <label>Models (${models.length})</label>
            <div class="detail-edit-models-actions">
                <button type="button" class="btn-small" data-action="add-model">＋ Add Model</button>
                <button type="button" class="btn-small" data-action="fetch-models">⟳ Fetch Models</button>
            </div>
            ${models.length > 0
                ? '<ul class="detail-models-list">' + models.map(m =>
                    '<li data-model-id="' + esc(m) + '">' + modelIconHTML(m) + '<span class="model-name">' + esc(m) + '</span><button type="button" class="model-id-remove" title="Remove model" aria-label="Remove ' + esc(m) + '">×</button></li>'
                  ).join('') + '</ul>'
                : '<div class="detail-empty-models">No models yet. Click ＋ Add Model or ⟳ Fetch Models.</div>'}
            <div class="detail-edit-add-model-row" data-role="add-model-row" style="display:none;">
                <input type="text" id="add-model-input-${esc(ds.id)}" placeholder="Type a model ID and press Enter" autocomplete="off">
                <button type="button" class="btn-small btn-primary" data-action="add-model-submit">Add</button>
                <button type="button" class="btn-small" data-action="add-model-cancel">Cancel</button>
            </div>
        </div>`;

    const root = document.getElementById('downstreams-detail');
    const id = ds.id;

    // ---- Auto-save on blur for Name / URL ----
    const nameInput = root.querySelector('.detail-edit-name');
    nameInput.addEventListener('blur', () => {
        const newName = nameInput.value.trim();
        if (newName === ds.name) return;
        if (!newName) {
            // Don't allow empty — revert.
            nameInput.value = ds.name;
            showToast('Name cannot be empty');
            return;
        }
        autoSaveDownstreamField(id, { name: newName }, err => {
            if (err) {
                nameInput.value = ds.name;
                return;
            }
            ds.name = newName;
            // Update sidebar item text + cachedDownstreams
            const cached = (cachedDownstreams || []).find(d => d.id === id);
            if (cached) cached.name = newName;
            const li = document.querySelector('#downstreams-list li[data-id="' + id + '"] .ds-name');
            if (li) li.textContent = newName;
        });
    });
    // Pressing Enter also triggers blur, but prevent form-style submission
    nameInput.addEventListener('keydown', e => { if (e.key === 'Enter') { e.preventDefault(); nameInput.blur(); } });

    const urlInput = root.querySelector('.detail-edit-url');
    urlInput.addEventListener('blur', () => {
        const newUrl = urlInput.value.trim();
        if (newUrl === (ds.base_url || '')) return;
        if (!newUrl) {
            urlInput.value = ds.base_url || '';
            showToast('Base URL cannot be empty');
            return;
        }
        autoSaveDownstreamField(id, { base_url: newUrl }, err => {
            if (err) {
                urlInput.value = ds.base_url || '';
                return;
            }
            ds.base_url = newUrl;
            const cached = (cachedDownstreams || []).find(d => d.id === id);
            if (cached) cached.base_url = newUrl;
        });
    });
    urlInput.addEventListener('keydown', e => { if (e.key === 'Enter') { e.preventDefault(); urlInput.blur(); } });

    // ---- API Key: on blur, if the input has any value, save it (replaces the saved key).
    //       If the input is empty, no save (keeps existing key untouched).
    const keyInput = root.querySelector('.detail-edit-key');
    const eyeBtn = root.querySelector('[data-action="toggle-key"]');
    let keyRevealed = false;
    keyInput.addEventListener('blur', () => {
        // If the eye toggle put the key into the input for editing purposes, blur
        // should NOT treat that as a save-replace. We detect this by checking
        // whether the input's value matches the just-revealed key.
        if (keyInput.dataset.justRevealed === '1') {
            keyInput.dataset.justRevealed = '';
            return;
        }
        const newKey = keyInput.value;
        if (newKey === '') {
            // Empty input → revert to placeholder, don't change saved key.
            return;
        }
        if (newKey === ds.api_key || (hasKey && newKey === '')) return;
        // PUT with explicit api_key replaces the saved key.
        autoSaveDownstreamField(id, { api_key: newKey }, err => {
            if (err) {
                keyInput.value = '';
                return;
            }
            ds.api_key = newKey;
            keyInput.value = '';
            // Re-mask on save (the input was just for typing a replacement)
            keyInput.type = 'password';
            eyeBtn.classList.remove('shown');
            eyeBtn.textContent = '👁';
            keyRevealed = false;
        });
    });
    keyInput.addEventListener('keydown', e => { if (e.key === 'Enter') { e.preventDefault(); keyInput.blur(); } });

    // ---- Eye toggle: on first reveal fetch the actual key; subsequent toggles just flip type. ----
    eyeBtn.onclick = async () => {
        if (keyRevealed) {
            // Hide it
            keyInput.type = 'password';
            eyeBtn.classList.remove('shown');
            eyeBtn.textContent = '👁';
            keyInput.value = '';
            keyInput.dataset.justRevealed = '';
            keyRevealed = false;
            return;
        }
        if (!hasKey) {
            showToast('No API key set yet — type one to add');
            keyInput.focus();
            return;
        }
        eyeBtn.disabled = true;
        const realKey = await revealApiKey(id);
        eyeBtn.disabled = false;
        if (!realKey) return;
        keyInput.dataset.justRevealed = '1';
        keyInput.type = 'text';
        keyInput.value = realKey;
        eyeBtn.classList.add('shown');
        eyeBtn.textContent = '🙈';
        keyRevealed = true;
    };

    // ---- Formats: auto-save on change ----
    root.querySelectorAll('#edit-formats-' + id + ' input[type="checkbox"]').forEach(cb => {
        cb.addEventListener('change', () => {
            const checked = [];
            root.querySelectorAll('#edit-formats-' + id + ' input[type="checkbox"]:checked').forEach(c => checked.push(c.value));
            autoSaveDownstreamField(id, { api_formats: checked }, err => {
                if (err) {
                    // Revert: re-read from ds
                    const targetVal = ds.api_formats || [];
                    root.querySelectorAll('#edit-formats-' + id + ' input[type="checkbox"]').forEach(c => {
                        c.checked = targetVal.includes(c.value);
                    });
                    return;
                }
            });
        });
    });

    // ---- Per-model × delete: hits DELETE /api/downstreams/{id}/models/{model_id} ----
    root.querySelectorAll('.detail-models-list li[data-model-id]').forEach(li => {
        const removeBtn = li.querySelector('.model-id-remove');
        removeBtn.onclick = async () => {
            const modelId = li.dataset.modelId;
            try {
                await api('/downstreams/' + encodeURIComponent(id) + '/models/' + encodeURIComponent(modelId), { method: 'DELETE' });
                li.remove();
                ds.output_model_ids = (ds.output_model_ids || []).filter(m => m !== modelId);
                const cached = (cachedDownstreams || []).find(d => d.id === id);
                if (cached) cached.output_model_ids = ds.output_model_ids;
                // Update the count label
                const label = root.querySelector('.detail-section:nth-of-type(4) > label');
                if (label) label.textContent = 'Models (' + ds.output_model_ids.length + ')';
                if (ds.output_model_ids.length === 0) {
                    const list = root.querySelector('.detail-models-list');
                    if (list) list.remove();
                    const empty = document.createElement('div');
                    empty.className = 'detail-empty-models';
                    empty.textContent = 'No models yet. Click ＋ Add Model or ⟳ Fetch Models.';
                    const addRow = root.querySelector('[data-role="add-model-row"]');
                    root.querySelector('.detail-edit-models-actions').after(empty, addRow);
                }
                showToast('Removed ' + modelId);
            } catch (err) {
                showToast('Delete failed: ' + err.message);
            }
        };
    });

    // ---- + Add Model: toggles the inline input row ----
    const addModelBtn = root.querySelector('[data-action="add-model"]');
    const addModelRow = root.querySelector('[data-role="add-model-row"]');
    const addModelInput = root.querySelector('#add-model-input-' + id);
    addModelBtn.onclick = () => {
        addModelRow.style.display = '';
        addModelInput.focus();
    };
    root.querySelector('[data-action="add-model-cancel"]').onclick = () => {
        addModelInput.value = '';
        addModelRow.style.display = 'none';
    };
    async function submitAddModel() {
        const modelId = addModelInput.value.trim();
        if (!modelId) {
            showToast('Type a model ID first');
            return;
        }
        try {
            const updated = await api('/downstreams/' + encodeURIComponent(id) + '/models', {
                method: 'POST',
                body: JSON.stringify({ model_id: modelId }),
            });
            ds.output_model_ids = updated.output_model_ids || [];
            // Re-render the models section to pick up the new chip and count
            renderDownstreamDetail(ds);
            selectSidebarItem(id);
            showToast('Added ' + modelId);
        } catch (err) {
            showToast('Add failed: ' + err.message);
        }
    }
    addModelInput.addEventListener('keydown', e => { if (e.key === 'Enter') { e.preventDefault(); submitAddModel(); } });
    root.querySelector('[data-action="add-model-submit"]').onclick = submitAddModel;

    // ---- Fetch Models ----
    root.querySelector('[data-action="fetch-models"]').onclick = () => fetchDownstreamModels(id);

    // ---- Header Delete ----
    root.querySelector('[data-action="delete"]').onclick = () => deleteDownstream(id);
}

/**
 * Patch one or more fields on the downstream and PUT. On success, fires
 * the optional callback (for local-state updates like re-rendering sidebar text).
 * On error, shows a toast and reverts nothing — the caller is responsible for
 * reverting its own UI state (the input value).
 */
async function autoSaveDownstreamField(id, patch, cb) {
    try {
        const updated = await api('/downstreams/' + encodeURIComponent(id), {
            method: 'PUT',
            body: JSON.stringify(patch),
        });
        showToast('Saved');
        if (cb) cb(null, updated);
    } catch (err) {
        showToast('Save failed: ' + err.message);
        if (cb) cb(err);
    }
}

async function revealApiKey(id) {
    try {
        const full = await api('/downstreams/' + encodeURIComponent(id) + '?reveal=1');
        return full.api_key || '';
    } catch (err) {
        showToast('Reveal failed: ' + err.message);
        return '';
    }
}

function copyText(text, msg) {
    if (!text) return;
    if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(
            () => showToast(msg || 'Copied'),
            () => fallbackCopy(text, msg)
        );
    } else {
        fallbackCopy(text, msg);
    }
}

function fallbackCopy(text, msg) {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand('copy'); showToast(msg || 'Copied'); }
    catch { showToast('Copy failed'); }
    document.body.removeChild(ta);
}

let _toastTimer = null;
function showToast(msg) {
    let el = document.getElementById('app-toast');
    if (!el) {
        el = document.createElement('div');
        el.id = 'app-toast';
        el.className = 'toast';
        document.body.appendChild(el);
    }
    el.textContent = msg;
    el.classList.add('visible');
    if (_toastTimer) clearTimeout(_toastTimer);
    _toastTimer = setTimeout(() => el.classList.remove('visible'), 1800);
}

// addModelIdRow used to live here for the old edit modal; the inline editor
// builds model chips directly in renderDownstreamDetail. The helper is gone.

/**
 * Fetch the upstream provider's model list and show the picker popup.
 * Uses POST /api/downstreams/{id}/fetch-models (the ID variant) so the server
 * can use the saved API key server-side. The ID variant has been refactored
 * to NOT auto-append — it returns {"model_ids":[...]} without persisting.
 * The popup's "＋ Add" buttons commit individually via
 * POST /api/downstreams/{id}/models. Closing the popup leaves the DB
 * unchanged for any models the user did not explicitly add.
 */
async function fetchDownstreamModels(id) {
    const btn = document.querySelector('#downstreams-detail [data-action="fetch-models"]');
    const origText = btn ? btn.textContent : '⟳ Fetch Models';
    if (btn) { btn.textContent = 'Fetching…'; btn.disabled = true; }
    try {
        const resp = await api('/downstreams/' + encodeURIComponent(id) + '/fetch-models', {
            method: 'POST',
        });
        const modelIds = resp.model_ids || resp.output_model_ids || [];
        showFetchModelsPopup(id, modelIds);
    } catch (err) {
        let msg = err.message || String(err);
        if (msg.startsWith('fetch failed: ')) {
            msg = msg.substring('fetch failed: '.length);
        }
        showToast('Fetch failed: ' + msg);
    } finally {
        if (btn) { btn.textContent = origText; btn.disabled = false; }
    }
}

/**
 * Show the fetch-models picker. Each "+ Add" commits IMMEDIATELY via
 * POST /api/downstreams/{id}/models and re-renders the detail pane so the
 * new chip appears right away. Closing the popup (× or background) commits
 * nothing further.
 */
function showFetchModelsPopup(dsId, modelIds) {
    const list = document.getElementById('fetch-models-list');
    list.innerHTML = '';

    // Collect existing model IDs from the cached downstream
    const ds = (cachedDownstreams || []).find(d => d.id === dsId);
    const existingIds = new Set(ds ? (ds.output_model_ids || []) : []);

    if (modelIds.length === 0) {
        list.innerHTML = '<div style="padding:1rem;text-align:center;color:var(--text-muted);">No models returned by the provider.</div>';
        document.getElementById('fetch-models-modal').classList.remove('hidden');
        return;
    }

    modelIds.forEach(function (m) {
        const row = document.createElement('div');
        row.className = 'fetch-model-row';

        const name = document.createElement('span');
        name.className = 'fetch-model-name';
        name.innerHTML = modelIconHTML(m) + esc(m);
        row.appendChild(name);

        const addBtn = document.createElement('button');
        addBtn.className = 'fetch-model-add';

        function setAddedUI() {
            addBtn.textContent = 'Added';
            addBtn.classList.add('added');
            addBtn.disabled = true;
        }
        function setNotAddedUI() {
            addBtn.textContent = '+ Add';
            addBtn.classList.remove('added');
            addBtn.disabled = false;
        }

        if (existingIds.has(m)) {
            setAddedUI();
        } else {
            setNotAddedUI();
            addBtn.onclick = async function () {
                addBtn.disabled = true;
                addBtn.textContent = '…';
                try {
                    const updated = await api('/downstreams/' + encodeURIComponent(dsId) + '/models', {
                        method: 'POST',
                        body: JSON.stringify({ model_id: m }),
                    });
                    // Update local cache + ds reference so re-opening the popup
                    // shows the new model as "Added".
                    if (ds && updated && updated.output_model_ids) {
                        ds.output_model_ids = updated.output_model_ids;
                    }
                    const cached = (cachedDownstreams || []).find(d => d.id === dsId);
                    if (cached && updated && updated.output_model_ids) {
                        cached.output_model_ids = updated.output_model_ids;
                    }
                    setAddedUI();
                    // Reflect the new chip in the detail pane if it's still selected.
                    if (_currentDownstreamId === dsId && ds) {
                        renderDownstreamDetail(ds);
                        selectSidebarItem(dsId);
                    }
                } catch (err) {
                    setNotAddedUI();
                    showToast('Add failed: ' + err.message);
                }
            };
        }

        row.appendChild(addBtn);
        list.appendChild(row);
    });

    document.getElementById('fetch-models-modal').classList.remove('hidden');
}

// Also close the fetch popup when clicking outside it (handled by .modal background click)

async function deleteDownstream(id) {
    if (!confirm('Delete this downstream?')) return;
    try {
        await api('/downstreams/' + id, { method: 'DELETE' });
        loadDownstreams();
    } catch (err) {
        alert(err.message);
    }
}

document.getElementById('btn-add-downstream').addEventListener('click', () => createNewDownstream());
document.getElementById('downstreams-search').addEventListener('input', () => {
    // Re-render the sidebar using the cached list, filtered by the search box.
    if (cachedDownstreams) renderDownstreamSidebar(cachedDownstreams);
});

/**
 * Create a new empty downstream in the DB, switch to it in the detail pane,
 * and let the user fill in the fields inline. If the user navigates away
 * (via the sidebar) without typing anything, the stub is auto-deleted by
 * selectDownstream()'s cleanup hook.
 */
async function createNewDownstream() {
    try {
        // The backend rejects empty name/base_url, so we create with placeholders
        // the user is expected to replace. The selectDownstream() cleanup hook
        // auto-deletes this stub if the user navigates away without typing.
        const ds = await api('/downstreams', {
            method: 'POST',
            body: JSON.stringify({
                name: 'New Provider',
                base_url: 'https://',
                api_key: '',
                api_formats: [],
                output_model_ids: [],
            }),
        });
        // Refresh the list and switch the selection to the new one.
        const list = await api('/downstreams');
        cachedDownstreams = list;
        _currentDownstreamId = ds.id;
        renderDownstreamSidebar(list);
        selectSidebarItem(ds.id);
        renderDownstreamDetail(ds);
        // Focus the name input and select all so the user can type to replace.
        const nameInput = document.querySelector('.detail-edit-name');
        if (nameInput) { nameInput.focus(); nameInput.select(); }
    } catch (err) {
        showToast('Create failed: ' + err.message);
    }
}

// (The old btn-fetch-models listener and downstream-form submit handler are gone
//  with the edit modal. Fetch is now wired per-row in renderDownstreamDetail.)

// ---- Plugins ----
async function loadPlugins() {
    const tbody = document.getElementById('plugins-body');
    try {
        const plugins = await api('/plugins');
        cachedPlugins = plugins;
        tbody.innerHTML = plugins.length === 0
            ? '<tr><td colspan="3" class="loading">No plugins registered</td></tr>'
            : plugins.map(p => `
                <tr>
                    <td><code>${esc(p.id)}</code></td>
                    <td>${esc(p.description)}</td>
                    <td><pre class="schema-hint">${esc(formatSchema(p.config_schema))}</pre></td>
                </tr>
            `).join('');
    } catch (err) {
        tbody.innerHTML = `<tr><td colspan="3" class="loading">Error: ${esc(err.message)}</td></tr>`;
    }
}

function formatSchema(schema) {
    if (!schema) return '(none)';
    if (schema.type === 'object' && schema.properties) {
        const entries = Object.entries(schema.properties);
        if (entries.length === 0) return '(no config)';
        const lines = entries.map(([key, val]) => {
            const req = schema.required && schema.required.includes(key) ? ' (required)' : '';
            return `${key}: ${val.type || 'any'}${req}`;
        });
        return lines.join('\n');
    }
    return JSON.stringify(schema, null, 2);
}

// ---- Modal close handlers ----
document.querySelectorAll('.modal .close').forEach(btn => {
    btn.addEventListener('click', () => {
        const modal = btn.closest('.modal');
        cleanupModal(modal);
        modal.classList.add('hidden');
    });
});

// Close modal on background click
document.querySelectorAll('.modal').forEach(m => {
    m.addEventListener('click', (e) => {
        if (e.target === m) {
            cleanupModal(m);
            m.classList.add('hidden');
        }
    });
});

/**
 * Clean up dynamic event listeners attached when opening a modal.
 */
function cleanupModal(modal) {
    if (modal._fetchListeners) {
        const l = modal._fetchListeners;
        if (l.url && l.handler) l.url.removeEventListener('input', l.handler);
        if (l.key && l.handler) l.key.removeEventListener('input', l.handler);
        delete modal._fetchListeners;
    }
}

// ---- Utility ----
function esc(s) {
    if (s == null) return '';
    return String(s).replace(/[&<>"']/g, function(c) {
        return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c];
    });
}

// ---- Help Tooltip Texts ----
const tooltipTexts = {
    'alias.regex_badge': "This group uses a regex pattern — matches any incoming model name the regex accepts. (e.g., claude-opus.* matches both claude-opus-4-7 and claude-opus-4-8)",
};

function makeHelpIcon(tooltip) {
    const icon = document.createElement('span');
    icon.className = 'help-icon';
    icon.tabIndex = 0;
    icon.setAttribute('role', 'button');
    icon.setAttribute('aria-label', 'Help');
    icon.setAttribute('aria-describedby', 'help-popover');
    icon.dataset.tooltip = tooltip;
    icon.textContent = '?';
    return icon;
}

(function setupHelpPopover() {
    const popover = document.createElement('div');
    popover.id = 'help-popover';
    popover.setAttribute('role', 'tooltip');
    document.body.appendChild(popover);

    let activeIcon = null;
    let pinned = false;

    function place(icon) {
        const r = icon.getBoundingClientRect();
        popover.textContent = icon.dataset.tooltip;
        popover.classList.remove('anchor-right', 'anchor-above');
        popover.style.top = '0px';
        popover.style.left = '0px';

        requestAnimationFrame(() => {
            const pr = popover.getBoundingClientRect();
            let top = r.bottom + window.scrollY + 6;
            let left = r.left + window.scrollX;

            if (r.left + pr.width > window.innerWidth - 8) {
                left = r.right + window.scrollX - pr.width;
                popover.classList.add('anchor-right');
            }
            if (r.bottom + pr.height > window.innerHeight - 8) {
                top = r.top + window.scrollY - pr.height - 6;
                left = r.left + window.scrollX + r.width / 2 - pr.width / 2;
                popover.classList.add('anchor-above');
            }
            popover.style.top = top + 'px';
            popover.style.left = left + 'px';
        });
    }

    function show(icon) {
        activeIcon = icon;
        place(icon);
        popover.classList.add('visible');
    }
    function hide() {
        if (pinned) return;
        activeIcon = null;
        popover.classList.remove('visible');
    }

    document.addEventListener('mouseenter', e => {
        const icon = e.target.closest && e.target.closest('.help-icon');
        if (icon) { pinned = false; show(icon); }
    }, true);
    document.addEventListener('mouseleave', e => {
        const icon = e.target.closest && e.target.closest('.help-icon');
        if (icon) hide();
    }, true);
    document.addEventListener('focusin', e => {
        const icon = e.target.closest && e.target.closest('.help-icon');
        if (icon) { pinned = false; show(icon); }
    });
    document.addEventListener('focusout', e => {
        const icon = e.target.closest && e.target.closest('.help-icon');
        if (icon) hide();
    });
    document.addEventListener('click', e => {
        const icon = e.target.closest && e.target.closest('.help-icon');
        if (icon) {
            e.preventDefault();
            if (pinned && activeIcon === icon) {
                pinned = false; hide();
            } else {
                pinned = true; show(icon);
            }
            return;
        }
        if (pinned) { pinned = false; hide(); }
    });
    document.addEventListener('keydown', e => {
        if (e.key === 'Escape' && (pinned || popover.classList.contains('visible'))) {
            pinned = false;
            hide();
            if (activeIcon) activeIcon.focus();
        }
    });
    window.addEventListener('scroll', () => { if (!pinned) hide(); }, true);
    window.addEventListener('resize', hide);
})();

// Event delegation for rule table buttons (replaces inline onclick)
document.getElementById('rules-body').addEventListener('click', function (e) {
    const btn = e.target.closest('.rule-edit-btn, .rule-toggle-btn, .rule-delete-btn');
    if (!btn) return;
    const action = btn.dataset.action;
    const id = btn.dataset.id;
    if (action === 'edit-rule') editRule(id);
    else if (action === 'toggle-rule') toggleRule(id, btn.dataset.enabled === '1');
    else if (action === 'delete-rule') deleteRule(id);
});

// Downstream row edit/delete is now wired directly in renderDownstreamDetail()
// (see above). The old #downstreams-body table delegate is gone with the
// table layout — the new layout puts the actions in the detail pane.

function shortPipeline(config) {
    try {
        const steps = JSON.parse(config || '[]');
        if (!steps.length) return '(no pipeline)';
        return steps.map(s => s.plugin_id).join(' → ');
    } catch {
        return '(invalid)';
    }
}

// ---- Aliases ----

async function loadDownstreamsForAliasSelect() {
    if (cachedDownstreams) return cachedDownstreams;
    try {
        cachedDownstreams = await api('/downstreams');
    } catch {
        cachedDownstreams = [];
    }
    return cachedDownstreams;
}

// Populate downstream <select> elements from cache
function populateDownstreamSelect(selectEl) {
    const ds = cachedDownstreams || [];
    selectEl.innerHTML = '<option value="">— Select —</option>' +
        ds.map(d => `<option value="${esc(d.id)}">${esc(d.name)}</option>`).join('');
}

/**
 * Load and render alias groups from the API.
 */
async function loadAliasGroups() {
    const container = document.getElementById('alias-groups-container');
    try {
        // Refresh downstream cache for labels
        cachedDownstreams = await api('/downstreams');

        const groups = await api('/aliases');
        container.innerHTML = '';

        if (groups.length === 0) {
            container.innerHTML = `
                <div class="alias-empty-state">
                    <div class="empty-icon">🔀</div>
                    <p>No alias groups configured</p>
                    <p class="empty-hint">Create a new group to map input models to output models.</p>
                </div>`;
            return;
        }

        groups.forEach(g => renderAliasGroup(container, g));

        // Attach drag-and-drop handlers to all rendered cards
        const cards = container.querySelectorAll('.alias-group-card');
        cards.forEach(function (card) {
            setupDraggable(card, container);
        });
    } catch (err) {
        container.innerHTML = `<div class="alias-empty-state"><p>Error: ${esc(err.message)}</p></div>`;
    }
}

/**
 * Render a single alias group card.
 */
function renderAliasGroup(container, group) {
    const card = document.createElement('div');
    card.className = 'alias-group-card';
    card.dataset.inputModel = group.input_model_id;

    // Header row (drag handle)
    const header = document.createElement('div');
    header.className = 'alias-group-header';
    header.draggable = true;

    const title = document.createElement('div');
    title.className = 'alias-group-title';
    const isRegexGroup = group.is_regex;
    title.innerHTML = modelIconHTML(isRegexGroup ? '' : group.input_model_id) + esc(group.input_model_id) +
        (isRegexGroup ? '<span class="badge" style="background:#6e256d;color:#e879f9;margin-left:0.4rem;font-size:0.7rem;">regex</span>' : '');
    if (isRegexGroup) {
        title.appendChild(makeHelpIcon(tooltipTexts['alias.regex_badge']));
    }

    const actions = document.createElement('div');
    actions.className = 'alias-group-actions';

    const addBtnWrap = document.createElement('span');
    addBtnWrap.style.cssText = 'display:inline-flex;align-items:center;gap:0.3rem;';

    // Add option button in header area
    const addBtn = document.createElement('button');
    addBtn.className = 'btn-small';
    addBtn.textContent = '+ Option';
    addBtn.onclick = () => openAliasOptionModal(group.input_model_id);
    addBtnWrap.appendChild(addBtn);
    actions.appendChild(addBtnWrap);

    // Delete group button
    const deleteGroupBtn = document.createElement('button');
    deleteGroupBtn.className = 'btn-danger btn-small';
    deleteGroupBtn.textContent = 'Delete Group';
    deleteGroupBtn.onclick = () => deleteAliasGroup(group.input_model_id);
    actions.appendChild(deleteGroupBtn);

    header.appendChild(title);
    header.appendChild(actions);
    card.appendChild(header);

    // Options grid
    const grid = document.createElement('div');
    grid.className = 'alias-options-grid';

    group.options.forEach(opt => {
        const btn = document.createElement('div');
        btn.className = 'alias-option-btn' + (opt.is_active ? ' active' : '');
        btn.dataset.aliasId = opt.id;

        // Output model line (top, larger) with model icon
        const modelLabel = document.createElement('div');
        modelLabel.className = 'option-model';
        modelLabel.innerHTML = modelIconHTML(opt.output_model_id) + esc(opt.output_model_id);
        btn.appendChild(modelLabel);

        // Downstream name line with format icons (bottom, smaller)
        const dsLabel = document.createElement('div');
        dsLabel.className = 'option-downstream';
        const formats = Array.isArray(opt.api_formats) ? opt.api_formats : [];
        const icons = formats.map(formatIconHTML).join('');
        dsLabel.innerHTML = icons + esc(opt.downstream_name || opt.downstream_id);
        btn.appendChild(dsLabel);

        // Remove button (top-right corner)
        const removeBtn = document.createElement('button');
        removeBtn.className = 'option-remove';
        removeBtn.textContent = '×';
        removeBtn.title = `Delete this option`;
        removeBtn.onclick = (e) => {
            e.stopPropagation();
            deleteAlias(opt.id, group.input_model_id);
        };
        btn.appendChild(removeBtn);

        // Click to activate (hot-switch)
        if (!opt.is_active) {
            btn.onclick = () => activateAlias(opt.id);
        }

        grid.appendChild(btn);
    });

    card.appendChild(grid);
    container.appendChild(card);
}

/**
 * Hot-switch: activate a specific alias option.
 */
async function activateAlias(id) {
    try {
        await api('/aliases/' + id + '/activate', { method: 'PUT' });
        loadAliasGroups();
    } catch (err) {
        alert('Error activating alias: ' + err.message);
    }
}

/**
 * Delete an alias option.
 */
async function deleteAlias(id, inputModelId) {
    if (!confirm('Delete this alias option?')) return;
    try {
        await api('/aliases/' + id, { method: 'DELETE' });
        loadAliasGroups();
    } catch (err) {
        alert('Error: ' + err.message);
    }
}

/**
 * Delete an entire alias group (all options for a given input model).
 */
async function deleteAliasGroup(inputModelId) {
    if (!confirm('Delete this entire alias group ("' + inputModelId + '") and all its options?\nThis cannot be undone.')) return;
    try {
        await api('/aliases/group/' + encodeURIComponent(inputModelId), { method: 'DELETE' });
        loadAliasGroups();
    } catch (err) {
        alert('Error: ' + err.message);
    }
}

/**
 * Set up HTML5 drag-and-drop on an alias group card.
 * Drag handle is the header area (avoids conflicts with buttons and option buttons).
 */
function setupDraggable(card, container) {
    const header = card.querySelector('.alias-group-header');
    if (!header) return;

    header.addEventListener('dragstart', function (e) {
        e.dataTransfer.effectAllowed = 'move';
        e.dataTransfer.setData('text/plain', card.dataset.inputModel);
        card.classList.add('dragging');
    });

    header.addEventListener('dragend', function () {
        card.classList.remove('dragging');
        // Clean up all drag-over states
        container.querySelectorAll('.alias-group-card.drag-over').forEach(function (c) {
            c.classList.remove('drag-over');
        });
    });

    // Drop handlers go on the card (so the whole card surface accepts drops)
    card.addEventListener('dragover', function (e) {
        e.preventDefault(); // Allow drop
        e.dataTransfer.dropEffect = 'move';
        if (card !== document.querySelector('.alias-group-card.dragging')) {
            card.classList.add('drag-over');
        }
    });

    card.addEventListener('dragleave', function () {
        card.classList.remove('drag-over');
    });

    card.addEventListener('drop', function (e) {
        e.preventDefault();
        card.classList.remove('drag-over');

        const draggedId = e.dataTransfer.getData('text/plain');
        if (draggedId === card.dataset.inputModel) return;

        // Determine new order from DOM
        const draggedCard = container.querySelector('.alias-group-card[data-input-model="' + escapedAttr(draggedId) + '"]');
        if (!draggedCard) return;

        // Dragged card takes drop target's position; drop target shifts toward dragged's original position.
        const cards = Array.from(container.querySelectorAll('.alias-group-card'));
        const draggedIdx = cards.indexOf(draggedCard);
        const dropIdx = cards.indexOf(card);

        if (draggedIdx < 0 || dropIdx < 0) return;

        // Remove dragged card
        container.removeChild(draggedCard);

        // Insert dragged card at drop target's position
        if (draggedIdx < dropIdx) {
            // Forward drag: A->C. After removal, card shifted to dropIdx-1.
            // insert before card's nextSibling (or append if last) to place dragged
            // at card's original position, pushing card down.
            // Ex: A,B,C drag A to C -> remove A -> [B,C] -> insertBefore(A, C.nextSibling=null) -> appendChild(A) -> [B,C,A]
            if (card.nextSibling) {
                container.insertBefore(draggedCard, card.nextSibling);
            } else {
                container.appendChild(draggedCard);
            }
        } else {
            // Backward drag: C->A. After removal, card stays at dropIdx.
            // insert before card to place dragged at card's position, pushing card down.
            // Ex: A,B,C drag C to A -> remove C -> [A,B] -> insertBefore(C,A) -> [C,A,B]
            container.insertBefore(draggedCard, card);
        }

        // Collect new order from DOM and send to server
        const newCards = Array.from(container.querySelectorAll('.alias-group-card'));
        const order = newCards.map(function (c) { return c.dataset.inputModel; });
        reorderAliasGroups(order);
    });
}

/**
 * Escape a value for use in an attribute selector.
 */
function escapedAttr(val) {
    return val.replace(/"/g, '\\"');
}

/**
 * Send the new group ordering to the server.
 */
async function reorderAliasGroups(order) {
    try {
        await api('/aliases/reorder', {
            method: 'POST',
            body: JSON.stringify({ order: order })
        });
        loadAliasGroups();
    } catch (err) {
        alert('Error reordering groups: ' + err.message);
        loadAliasGroups(); // Re-render to restore original order
    }
}

/**
 * Build a tag-based multi-select UI for output model IDs.
 * Populated from the given downstream's known output_model_ids.
 * If no models are known, shows a hint and reveals the custom text input.
 */
function populateOutputModelSelect(containerEl, downstreamId, excludeModels) {
    containerEl.innerHTML = '';
    const ds = (cachedDownstreams || []).find(d => d.id === downstreamId);
    let models = ds ? (ds.output_model_ids || []) : [];

    // Filter out models already in the alias group
    if (excludeModels && excludeModels.length > 0) {
        const ex = excludeModels.map(m => m.toLowerCase());
        models = models.filter(m => !ex.includes(m.toLowerCase()));
    }

    // Hide custom input by default
    const customInput = containerEl.closest('label').querySelector('.custom-model-input');

    if (models.length === 0) {
        containerEl.innerHTML = '<div class="model-multi-empty">No known models — type one below or fetch from provider</div>';
        if (customInput) customInput.style.display = '';
        return;
    }

    if (customInput) customInput.style.display = 'none';

    // Wrap tags in a flex container for wrapping
    const wrap = document.createElement('div');
    wrap.className = 'model-tag-wrap';

    models.forEach(m => {
        const tag = document.createElement('span');
        tag.className = 'model-tag';
        tag.dataset.model = m;

        const cb = document.createElement('input');
        cb.type = 'checkbox';
        cb.value = m;
        cb.style.display = 'none';
        tag.appendChild(cb);

        const icon = document.createElement('span');
        icon.className = 'model-tag-icon';
        icon.textContent = '☐';
        tag.appendChild(icon);

        const name = document.createElement('span');
        name.className = 'model-tag-name';
        name.textContent = m;
        tag.appendChild(name);

        tag.addEventListener('click', () => {
            cb.checked = !cb.checked;
            tag.classList.toggle('selected', cb.checked);
            icon.textContent = cb.checked ? '☑' : '☐';
        });

        wrap.appendChild(tag);
    });

    containerEl.appendChild(wrap);
}

/**
 * Collect selected output model IDs from the tag-based multi-select + custom input.
 */
function getSelectedOutputModels(containerEl, customInputEl) {
    const models = [];
    containerEl.querySelectorAll('.model-tag.selected').forEach(tag => {
        models.push(tag.dataset.model);
    });
    // Include custom text if provided
    if (customInputEl && customInputEl.value.trim()) {
        models.push(customInputEl.value.trim());
    }
    return models;
}

/**
 * Open the "Add Alias Option" modal for a given group.
 */
async function openAliasOptionModal(inputModelId) {
    document.getElementById('alias-input-model').value = inputModelId;
    document.getElementById('alias-group-label').textContent = inputModelId;

    await loadDownstreamsForAliasSelect();
    populateDownstreamSelect(document.getElementById('alias-downstream'));

    // Clear output model select and custom input
    const outputContainer = document.getElementById('alias-output-models');
    outputContainer.innerHTML = '';
    const customInput = document.getElementById('alias-output-model-custom');
    customInput.value = '';
    customInput.style.display = 'none';

    // Fetch alias groups to determine which models are already in this group
    let excludeModels = [];
    try {
        const groups = await api('/aliases');
        const group = groups.find(function (g) { return g.input_model_id === inputModelId; });
        if (group) {
            excludeModels = group.options.map(function (o) { return o.output_model_id; });
        }
    } catch {
        // If fetch fails, show all models (no exclusions)
    }

    // Wire up downstream change handler to populate models
    const downstreamSelect = document.getElementById('alias-downstream');
    downstreamSelect.onchange = () => {
        populateOutputModelSelect(outputContainer, downstreamSelect.value, excludeModels);
    };

    // If a downstream is already selected (pre-populated), populate immediately
    if (downstreamSelect.value) {
        populateOutputModelSelect(outputContainer, downstreamSelect.value, excludeModels);
    }

    document.getElementById('alias-modal').classList.remove('hidden');
}

// Alias option form submit — creates one alias per selected output model
document.getElementById('alias-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const inputModelId = document.getElementById('alias-input-model').value;
    const downstreamId = document.getElementById('alias-downstream').value;
    const models = getSelectedOutputModels(
        document.getElementById('alias-output-models'),
        document.getElementById('alias-output-model-custom')
    );

    if (!downstreamId || models.length === 0) {
        alert('Select a downstream and at least one output model.');
        return;
    }

    try {
        
        for (const model of models) {
            await api('/aliases', {
                method: 'POST',
                body: JSON.stringify({
                    input_model_id: inputModelId,
                    downstream_id: downstreamId,
                    output_model_id: model,
                    is_active: false,
                    
                }),
            });
        }
        document.getElementById('alias-modal').classList.add('hidden');
        loadAliasGroups();
    } catch (err) {
        alert('Error: ' + err.message);
    }
});

// New alias group button
document.getElementById('btn-new-alias-group').addEventListener('click', async () => {
    await loadDownstreamsForAliasSelect();
    populateDownstreamSelect(document.getElementById('new-group-downstream'));

    // Clear output model select and custom input
    const outputContainer = document.getElementById('new-group-output-models');
    outputContainer.innerHTML = '';
    const customInput = document.getElementById('new-group-output-model-custom');
    customInput.value = '';
    customInput.style.display = 'none';

    // Wire up downstream change handler to populate models
    const downstreamSelect = document.getElementById('new-group-downstream');
    downstreamSelect.onchange = () => {
        populateOutputModelSelect(outputContainer, downstreamSelect.value);
    };

    // Reset fields
    document.getElementById('new-group-input-model').value = '';
    document.getElementById('new-group-is-regex').checked = false;
    document.getElementById('new-group-modal').classList.remove('hidden');
});

// New alias group form submit — creates one alias per selected output model
// The first created option is set active (brand-new group has no existing active).
document.getElementById('new-group-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const inputModelId = document.getElementById('new-group-input-model').value.trim();
    const downstreamId = document.getElementById('new-group-downstream').value;
    const models = getSelectedOutputModels(
        document.getElementById('new-group-output-models'),
        document.getElementById('new-group-output-model-custom')
    );

    if (!inputModelId || !downstreamId || models.length === 0) {
        alert('Input model ID, downstream, and at least one output model are required.');
        return;
    }

    try {
        const isRegex = document.getElementById('new-group-is-regex').checked;
        for (let i = 0; i < models.length; i++) {
            await api('/aliases', {
                method: 'POST',
                body: JSON.stringify({
                    input_model_id: inputModelId,
                    downstream_id: downstreamId,
                    output_model_id: models[i],
                    is_active: i === 0,
                    is_regex: isRegex,
                }),
            });
        }
        document.getElementById('new-group-modal').classList.add('hidden');
        loadAliasGroups();
    } catch (err) {
        alert('Error: ' + err.message);
    }
});

// ---- Settings ----

const proxyModeHelpTexts = {
    auto: 'Auto-detect: Windows system proxy → env vars → direct',
    env: 'Use HTTP_PROXY/HTTPS_PROXY environment variables only',
    windows: 'Windows system proxy first, then env vars as fallback',
    none: 'Connect directly — no proxy used',
};

async function loadSettings() {
    const statusEl = document.getElementById('settings-status');
    try {
        const cfg = await api('/config');
        document.getElementById('proxy-mode').value = cfg.proxy_mode || 'auto';
        const helpText = proxyModeHelpTexts[cfg.proxy_mode] || '';
        document.getElementById('proxy-mode-help').textContent = helpText;

        // Render proxy API keys list
        renderProxyAPIKeys(cfg.proxy_api_keys || []);

        // Show clear-password checkbox if a password is currently set
        if (cfg.admin_password_set) {
            document.getElementById('clear-password-row').style.display = '';
        } else {
            document.getElementById('clear-password-row').style.display = 'none';
        }
        // Clear password input fields on load
        document.getElementById('admin-password').value = '';
        document.getElementById('admin-password-confirm').value = '';
        document.getElementById('clear-password').checked = false;

        // Populate default tab selector
        const defaultTabEl = document.getElementById('default-tab');
        if (defaultTabEl) {
            defaultTabEl.value = cfg.default_tab || 'downstreams';
        }
        // Populate log level selector
        const logLevelEl = document.getElementById('setting-log-level');
        if (logLevelEl) {
            logLevelEl.value = cfg.log_level || 'info';
        }
    } catch (err) {
        statusEl.textContent = 'Failed to load settings: ' + err.message;
        statusEl.className = 'settings-status error';
    }
}

/**
 * Render the proxy API keys editor in the Settings tab.
 */
function renderProxyAPIKeys(keys) {
    const container = document.getElementById('proxy-api-keys-container');
    container.innerHTML = '';
    if (keys.length === 0) {
        container.innerHTML = '<div class="api-key-empty">No API keys configured — all traffic is allowed.</div>';
        return;
    }
    keys.forEach(function (key) {
        addProxyKeyRow(container, key);
    });
}

/**
 * Add a single API key input row with a remove button.
 */
function addProxyKeyRow(container, value) {
    const row = document.createElement('div');
    row.className = 'api-key-row';

    const input = document.createElement('input');
    input.type = 'text';
    input.className = 'api-key-input';
    input.placeholder = 'API key';
    input.value = value;

    const removeBtn = document.createElement('button');
    removeBtn.className = 'api-key-remove';
    removeBtn.textContent = '×';
    removeBtn.title = 'Remove key';
    removeBtn.onclick = function () { row.remove(); };

    row.appendChild(input);
    row.appendChild(removeBtn);
    container.appendChild(row);
}

// "Add API Key" button
document.getElementById('btn-add-proxy-key').addEventListener('click', function () {
    const container = document.getElementById('proxy-api-keys-container');
    // Remove empty-state message if present
    const empty = container.querySelector('.api-key-empty');
    if (empty) empty.remove();
    addProxyKeyRow(container, '');
    container.querySelector('.api-key-input:last-of-type').focus();
});

// Update help text when dropdown changes
document.getElementById('proxy-mode').addEventListener('change', function () {
    document.getElementById('proxy-mode-help').textContent = proxyModeHelpTexts[this.value] || '';
});

// Save settings button
document.getElementById('btn-save-settings').addEventListener('click', async () => {
    const statusEl = document.getElementById('settings-status');
    statusEl.textContent = 'Saving...';
    statusEl.className = 'settings-status';

    // Collect proxy API keys from the editor
    const keyInputs = document.querySelectorAll('#proxy-api-keys-container .api-key-input');
    const proxyAPIKeys = [];
    keyInputs.forEach(function (input) {
        const v = input.value.trim();
        if (v) proxyAPIKeys.push(v);
    });

    // Collect admin password
    const newPassword = document.getElementById('admin-password').value;
    const confirmPassword = document.getElementById('admin-password-confirm').value;
    const clearPassword = document.getElementById('clear-password').checked;

    // Validate password inputs
    if (clearPassword) {
        newPassword = '';
    } else if (newPassword && newPassword !== confirmPassword) {
        statusEl.textContent = 'Passwords do not match.';
        statusEl.className = 'settings-status error';
        return;
    }

    try {
        const body = {
            proxy_mode: document.getElementById('proxy-mode').value,
            proxy_api_keys: proxyAPIKeys,
            default_tab: document.getElementById('default-tab').value,
        };
        // Include log level if the selector exists
        const logLevelEl = document.getElementById('setting-log-level');
        if (logLevelEl) {
            body.log_level = logLevelEl.value;
        }
        // Only send admin_password if the user entered something or wants to clear it
        if (newPassword || clearPassword) {
            body.admin_password = newPassword;
        }
        const resp = await api('/config', {
            method: 'PUT',
            body: JSON.stringify(body),
        });

        // If the password was changed (not cleared), log out the current session
        // so the user must log in with the new password.
        if (newPassword && !clearPassword) {
            statusEl.textContent = 'Settings saved. Logging out — please sign in with your new password.';
            statusEl.className = 'settings-status success';
            // Brief pause so the user can see the success message
            setTimeout(function () {
                logout();
            }, 1500);
            return;
        }

        statusEl.textContent = 'Settings saved — proxy mode and auth keys updated live.';
        statusEl.className = 'settings-status success';
        // Reload settings to refresh UI state
        loadSettings();
    } catch (err) {
        statusEl.textContent = 'Failed to save: ' + err.message;
        statusEl.className = 'settings-status error';
    }
});

// ---- Logs ----

let logSSE = null;          // current EventSource connection
let logEntries = [];        // in-memory log entries (mirrors server buffer)
let logActive = false;      // whether the Logs tab is currently visible
let logPaused = false;      // whether log rendering is paused

/**
 * Initialize the Logs tab. Called once on dashboard load.
 * Sets up SSE connection management and loads initial entries.
 */
function initLogs() {
    // Load recent entries from the REST API (for initial render)
    fetchLogs();
}

/**
 * Fetch recent log entries via REST API (used for initial load).
 */
async function fetchLogs() {
    try {
        logEntries = await api('/logs');
        renderLogStream(logEntries);
    } catch (err) {
        // If the endpoint doesn't exist (old server) or auth fails, skip logs
    }
}

/**
 * Connect to the SSE log stream. Called when the Logs tab becomes active.
 */
function connectLogStream() {
    if (logSSE) return; // already connected

    const indicator = document.getElementById('log-level-indicator');
    const badge = document.getElementById('log-status-badge');

    // SSE connections automatically send cookies, so auth is handled via the auth cookie
    const url = API_BASE + '/logs/stream';

    try {
        logSSE = new EventSource(url);
    } catch {
        if (badge) { badge.textContent = '✗ Offline'; badge.style.background = 'var(--color-danger)'; }
        return;
    }

    if (badge) { badge.textContent = '● Live'; badge.style.background = 'var(--color-success)'; }

    // Reset pause state on reconnect
    logPaused = false;
    const pauseBtn = document.getElementById('btn-pause-logs');
    if (pauseBtn) pauseBtn.textContent = '⏸ Pause';

    logSSE.addEventListener('log', function (e) {
        let entry;
        try { entry = JSON.parse(e.data); } catch { return; }
        // Skip if already in memory (e.g. from REST fetch)
        if (logEntries.some(function (e2) { return e2.id === entry.id; })) return;
        // Prepend to in-memory array (newest first)
        logEntries.unshift(entry);
        if (logEntries.length > 500) logEntries.pop();
        if (logPaused) return; // skip rendering while paused
        prependLogEntry(entry, true);
    });

    logSSE.addEventListener('config', function (e) {
        let cfg;
        try { cfg = JSON.parse(e.data); } catch { return; }
        if (cfg.level && indicator) {
            indicator.textContent = 'Level: ' + cfg.level.charAt(0).toUpperCase() + cfg.level.slice(1);
            // Color-code the indicator by level
            const colors = { debug: '#4a5568', info: '#1a5ab8', warn: '#d49e00', error: '#c53030' };
            indicator.style.background = colors[cfg.level] || colors.info;
            indicator.style.display = '';
        }
    });

    logSSE.addEventListener('error', function () {
        if (badge) { badge.textContent = '✗ Offline'; badge.style.background = 'var(--color-danger)'; }
    });

    // When tab becomes inactive, close SSE to save resources
    const section = document.getElementById('tab-logs');
    if (section) {
        section.addEventListener('inactive', function () {
            disconnectLogStream();
        });
    }
}

/**
 * Disconnect the SSE log stream. Called when the Logs tab becomes inactive.
 */
function disconnectLogStream() {
    if (logSSE) {
        logSSE.close();
        logSSE = null;
    }
    logPaused = false;
    const badge = document.getElementById('log-status-badge');
    if (badge) { badge.textContent = '◝ Disconnected'; badge.style.background = '#6c757d'; }
    const pauseBtn = document.getElementById('btn-pause-logs');
    if (pauseBtn) pauseBtn.textContent = '⏸ Pause';
}

/**
 * Render the full log stream from an array of entries (newest first).
 */
function renderLogStream(entries) {
    const container = document.getElementById('logs-stream');
    if (!container) return;
    container.innerHTML = '';
    entries.forEach(function (entry) {
        const div = buildLogEntry(entry, false);
        container.appendChild(div);
    });
}

/**
 * Highlight quoted identifiers in a debug log message so the same model/alias
 * distinctions used in request lines apply to system messages too.
 *
 * Debug messages use printf-style %q quoting: `model "X"`, `alias "Y"`, etc.
 * We tokenize the message by alternating plain text and "quoted" runs, then
 * assign a class based on the preceding keyword (model/alias/downstream).
 */
function highlightDebugMessage(message) {
    if (!message) return '';
    let out = '';
    let i = 0;
    let lastKeyword = '';
    while (i < message.length) {
        const ch = message[i];
        if (ch === '"') {
            // Find closing quote (no escaping in these messages)
            let j = i + 1;
            while (j < message.length && message[j] !== '"') j++;
            const value = message.slice(i + 1, j);
            const cls = debugClassForKeyword(lastKeyword);
            out += cls
                ? '<span class="' + cls + '">' + esc(value) + '</span>'
                : esc(value);
            i = j + 1;
            lastKeyword = '';
        } else {
            // Accumulate plain text up to the next quote, capturing the last
            // word before the quote so we can classify the quoted value.
            let j = i;
            while (j < message.length && message[j] !== '"') j++;
            const segment = message.slice(i, j);
            out += esc(segment);
            const wordMatch = segment.match(/(\w+)\s*$/);
            if (wordMatch) lastKeyword = wordMatch[1].toLowerCase();
            i = j;
        }
    }
    return out;
}

function debugClassForKeyword(keyword) {
    if (keyword === 'model') return 'log-model-input';
    if (keyword === 'alias') return 'log-alias';
    if (keyword === 'downstream') return 'log-downstream';
    return '';
}

/**
 * Build a log entry DOM element (a <div> with timestamp, level badge, message).
 */
function buildLogEntry(entry, isNew) {
    const div = document.createElement('div');
    div.className = 'log-entry';
    div.dataset.id = entry.id;
    if (isNew) div.classList.add('new');

    // Timestamp
    const timeSpan = document.createElement('span');
    timeSpan.className = 'log-time';
    timeSpan.textContent = formatTime(entry.timestamp);
    div.appendChild(timeSpan);

    // Level badge
    const levelBadge = document.createElement('span');
    const level = entry.level || 'info';
    levelBadge.className = 'log-level log-level-' + level;
    levelBadge.textContent = level;
    div.appendChild(levelBadge);

    // Message body
    const msgDiv = document.createElement('div');
    msgDiv.className = 'log-msg';

    if (entry.level === 'debug') {
        div.classList.add('log-debug');
        msgDiv.innerHTML = '&#9670; ' + highlightDebugMessage(entry.message || '');
    } else {
        div.classList.add('log-request');

        const parts = [];

        // Method + Path
        if (entry.method || entry.path) {
            parts.push('<span class="log-method-path">' + esc((entry.method || '') + ' ' + (entry.path || '')).trim() + '</span>');
        }

        // Model chain: input-model -> resolved-model
        if (entry.model) {
            let modelPart = '<span class="log-model-input">' + esc(entry.model) + '</span>';
            if (entry.resolved_model && entry.resolved_model !== entry.model) {
                modelPart += ' <span class="log-arrow">&rarr;</span> <span class="log-model-resolved">' + esc(entry.resolved_model) + '</span>';
            }
            parts.push(modelPart);
        }

        // Downstream
        if (entry.downstream_name) {
            parts.push('<span class="log-downstream">' + esc(entry.downstream_name) + '</span>');
        } else if (entry.downstream_id) {
            parts.push('<span class="log-downstream">' + esc(entry.downstream_id) + '</span>');
        }

        // Alias group
        if (entry.alias_group) {
            parts.push('via <span class="log-alias">' + esc(entry.alias_group) + '</span>');
        }

        // Status code with color class
        let statusClass = 'log-status-ok';
        if (entry.status >= 500) {
            statusClass = 'log-status-error';
            div.classList.add('log-error');
        } else if (entry.status >= 400) {
            statusClass = 'log-status-warn';
            div.classList.add('log-warn');
        }
        parts.push('<span class="' + statusClass + '">' + entry.status + '</span>');

        // Duration
        const dur = formatDuration(entry.duration);
        if (dur && dur !== '') {
            parts.push(dur);
        }

        // Error
        if (entry.error) {
            parts.push('<span style="color:var(--danger-hover);">ERR: ' + esc(entry.error) + '</span>');
        }

        msgDiv.innerHTML = parts.join(' ');
    }

    div.appendChild(msgDiv);
    return div;
}

/**
 * Prepend a log entry (newest at top).
 */
function prependLogEntry(entry, isNew) {
    const container = document.getElementById('logs-stream');
    if (!container) return;

    const div = buildLogEntry(entry, isNew);
    if (container.firstChild) {
        container.insertBefore(div, container.firstChild);
    } else {
        container.appendChild(div);
    }

    // Remove oldest entries from bottom if exceeding 500
    while (container.children.length > 500) {
        container.removeChild(container.lastChild);
    }

    // Remove flash class after animation completes
    if (isNew) {
        setTimeout(function () { div.classList.remove('new'); }, 600);
    }
}

/**
 * Clear all log entries from the stream and memory.
 */
window.clearLogEntries = function () {
    const container = document.getElementById('logs-stream');
    if (container) container.innerHTML = '';
    logEntries = [];
};

/**
 * Filter log entries by search text.
 */
function applyLogFilter(query) {
    const container = document.getElementById('logs-stream');
    if (!container) return;
    const q = query.toLowerCase();

    container.querySelectorAll('.log-entry').forEach(function (div) {
        const id = parseInt(div.dataset.id, 10);
        const entry = logEntries.find(function (e) { return e.id === id; });
        if (!entry) { div.style.display = 'none'; return; }

        const text = [entry.model, entry.resolved_model, entry.path, entry.downstream_name,
            entry.downstream_id, entry.error, entry.alias_group, entry.method,
            String(entry.status), entry.message, entry.level].join(' ').toLowerCase();
        div.style.display = (q === '' || text.indexOf(q) !== -1) ? '' : 'none';
    });
}

/**
 * Log filter input handler.
 */
document.getElementById('log-filter-input').addEventListener('input', function () {
    applyLogFilter(this.value);
});

/**
 * Toggle pause/resume of log rendering.
 * Keeps the SSE connection alive — only stops rendering new rows.
 */
window.toggleLogPause = function () {
    logPaused = !logPaused;
    const btn = document.getElementById('btn-pause-logs');
    const badge = document.getElementById('log-status-badge');
    if (btn) {
        btn.textContent = logPaused ? '▶ Resume' : '⏸ Pause';
    }
    if (badge) {
        if (logPaused) {
            badge.textContent = '⏸ Paused';
            badge.style.background = '#d49e00';
        } else if (logSSE && logSSE.readyState === EventSource.OPEN) {
            badge.textContent = '● Live';
            badge.style.background = 'var(--color-success)';
        } else {
            badge.textContent = '✗ Offline';
            badge.style.background = 'var(--color-danger)';
        }
    }
    // When resuming, send a visual cue
    if (!logPaused && badge) {
        badge.style.transition = 'color 0.3s';
        badge.textContent = '● Live';
        setTimeout(function () { badge.style.transition = ''; }, 300);
    }
};

/**
 * Format a timestamp to a readable time string.
 */
function formatTime(ts) {
    if (!ts) return '—';
    let d;
    if (typeof ts === 'number') {
        d = new Date(ts);
    } else {
        d = new Date(ts);
    }
    if (isNaN(d.getTime())) return String(ts);
    return String(d.getHours()).padStart(2, '0') + ':'
        + String(d.getMinutes()).padStart(2, '0') + ':'
        + String(d.getSeconds()).padStart(2, '0');
}

/**
 * Format a duration in milliseconds to a readable string.
 */
function formatDuration(ms) {
    if (ms == null || ms === 0) return '';
    if (ms < 1000) return Math.round(ms) + 'ms';
    if (ms < 60000) return (ms / 1000).toFixed(1) + 's';
    const mins = Math.floor(ms / 60000);
    const secs = ((ms % 60000) / 1000).toFixed(1);
    return mins + 'm' + secs + 's';
}

// Intercept tab switching to manage SSE connection lifecycle.
(function () {
    const originalActivateTab = window.activateTab;
    window.activateTab = function (tabId) {
        // If leaving the Logs tab, disconnect SSE
        if (!logActive && tabId !== 'logs') {
            // was already inactive, nothing to do
        } else if (logActive && tabId !== 'logs') {
            // Leaving logs tab
            logActive = false;
            disconnectLogStream();
        }
        // If entering the Logs tab, connect SSE
        if (tabId === 'logs' && !logActive) {
            logActive = true;
            connectLogStream();
        }
        originalActivateTab(tabId);
    };
})();

// ---- About ----

async function loadAbout() {
    try {
        const data = await fetch(API_BASE + '/version').then(r => r.json());
        document.getElementById('about-version').textContent = data.version || 'unknown';
        document.getElementById('about-build-time').textContent = data.build_time || '—';
    } catch {
        document.getElementById('about-version').textContent = 'unknown';
        document.getElementById('about-build-time').textContent = '—';
    }
}

// ---- Init ----
checkAuth();
