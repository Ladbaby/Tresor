// Tresor Web UI — Admin Dashboard

const API_BASE = '/api';

// Format display metadata — labels and badge classes used by rule/alias badges.
const FORMAT_LABELS = {
    openai: 'OpenAI',
    openai_responses: 'OpenAI Responses',
    anthropic: 'Anthropic',
    gemini: 'Gemini',
};
const FORMAT_BADGE_CLASS = {
    openai: 'format-openai',
    openai_responses: 'format-openai_responses',
    anthropic: 'format-anthropic',
    gemini: 'format-gemini',
};

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
 * ponytail: also handles SSE lifecycle for the Logs tab — was monkey-patched
 * onto window.activateTab before.
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

    if (tabId === 'logs') {
        if (!logActive) { logActive = true; connectLogStream(); }
    } else if (logActive) {
        logActive = false;
        disconnectLogStream();
    }
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
        headers,
        credentials: 'same-origin',
        ...options,
    });
    if (resp.status === 401 && authEnabled) {
        logout();
        return;
    }
    if (!resp.ok) {
        const err = await resp.json().catch(() => ({ error: resp.statusText }));
        throw new Error(err.error || resp.statusText);
    }
    return resp.json();
}

// ---- Plugin cache (for visual pipeline editor) ----
let cachedPlugins = null;

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
    // ponytail: derived state per render call. cachedPlugins below mirrors so
    // the pipeline editor can re-find metadata without refetch.
    try {
        cachedPlugins = await api('/plugins');
    } catch {
        cachedPlugins = [];
    }
    return cachedPlugins;
}

let cachedDownstreams = null;
// ponytail: keep the last-known list around as a fallback only (e.g. when the
// aliases tab re-renders immediately after a CRUD). Full reload still happens
// each tab open via loadDownstreams() / loadAliasGroups().
async function fetchDownstreams() {
    try {
        cachedDownstreams = await api('/downstreams');
    } catch {
        cachedDownstreams = cachedDownstreams || [];
    }
    return cachedDownstreams;
}
// ---- Rules ----
async function loadRules() {
    const tbody = document.getElementById('rules-body');
    try {
        // Ensure downstream cache is populated so match badges can resolve
        // friendly names instead of falling back to raw IDs on a race with
        // loadDownstreams() (which also fires from showDashboard()).
        await fetchDownstreams();
        const rules = await api('/rules');
        tbody.innerHTML = rules.length === 0
            ? '<tr><td colspan="7" class="loading">No rules configured</td></tr>'
            : rules.map(r => {
                // Build match badges from the three optional match fields
                const badges = [];
                const inputFmts = r.match_format || [];
                const dsFmts = r.match_downstream_format || [];
                const dsIds = r.match_downstreams || [];

                inputFmts.forEach(f => {
                    badges.push(`<span class="format-badge ${FORMAT_BADGE_CLASS[f] || 'format-unknown'}">in:${esc(FORMAT_LABELS[f] || f)}</span>`);
                });

                dsFmts.forEach(f => {
                    badges.push(`<span class="format-badge ${FORMAT_BADGE_CLASS[f] || 'format-unknown'}">out:${esc(FORMAT_LABELS[f] || f)}</span>`);
                });

                // Downstream ID badges (grey). When an ID doesn't resolve to a
                // known downstream (e.g. it was deleted without a cascade
                // cleanup, or loaded from a stale YAML), flag it as missing so
                // the user can tell the rule has a dangling reference — the
                // engine never matches against unknown IDs anyway.
                dsIds.forEach(id => {
                    const ds = (cachedDownstreams || []).find(d => d.id === id);
                    if (ds) {
                        badges.push(`<span class="badge">ds:${esc(ds.name)}</span>`);
                    } else {
                        badges.push(`<span class="badge badge-missing" title="Downstream ${esc(id)} no longer exists — this rule will never match">ds:${esc(id)} (missing)</span>`);
                    }
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
// Sidebar + detail-pane layout. The right pane is an INLINE EDITOR; every field
// auto-saves on blur or on per-item action.
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
            _currentDownstreamId = id;
            renderDownstreamDetail(ds);
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
    // ponytail: `hasKey` is implicit — non-empty api_key means there's a saved key.
    const hasKey = !!(ds.api_key && ds.api_key.length > 0);

    document.getElementById('downstreams-detail').innerHTML = `
        <div class="detail-header">
            ${modelIconHTML(ds.name)}
            <input type="text" class="detail-edit-name" value="${esc(ds.name)}" placeholder="(unnamed provider)" autocomplete="off">
            <div class="header-actions">
                <button class="detail-header-delete" data-action="delete" title="Delete this downstream">🗑</button>
            </div>
        </div>
        <div class="detail-section">
            <label>API Formats</label>
            <div class="format-checkboxes">
                ${['openai', 'openai_responses', 'anthropic', 'gemini'].map(f => `
                    <label class="format-checkbox"><input type="checkbox" name="format" value="${f}"${formats.includes(f) ? ' checked' : ''}><img class="format-icon icon-${f}" src="icons/${f === 'openai' ? 'openai-completions' : f === 'openai_responses' ? 'openai-responses' : f}.svg" alt="" aria-hidden="true"> ${FORMAT_LABELS[f] || f}</label>
                `).join('')}
            </div>
        </div>
        <div class="detail-section">
            <label>API Key</label>
            <div class="detail-edit-key-wrap">
                <input type="password" class="detail-edit-key" value="" placeholder="${hasKey ? '•••••••••••••••• (saved — type to replace)' : 'sk-…'}" autocomplete="off">
                <button type="button" class="eye-toggle" data-action="toggle-key" title="Reveal / hide the saved key" aria-label="Toggle key visibility">👁</button>
            </div>
        </div>
        <div class="detail-section">
            <label>API Host</label>
            <div class="detail-row">
                <input type="url" class="detail-edit-url" value="${esc(ds.base_url || '')}" placeholder="https://api.example.com" autocomplete="off">
            </div>
        </div>
        <div class="detail-section">
            <label class="models-count-label">Models (${models.length})</label>
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
                <input type="text" class="add-model-input" placeholder="Type a model ID and press Enter" autocomplete="off">
                <button type="button" class="btn-small btn-primary" data-action="add-model-submit">Add</button>
                <button type="button" class="btn-small" data-action="add-model-cancel">Cancel</button>
            </div>
        </div>`;
    _currentDownstream = ds;
    // ponytail: stale-state guard — after re-render, re-bind the eye reveal flag
    // so the next reveal doesn't get blocked by a leftover `justRevealed` from a
    // prior render.
    _keyRevealed = false;
}

// ponytail: one delegated listener replaces per-render attach. Reads the active
// downstream from `_currentDownstream` and dispatches by [data-action].
let _currentDownstream = null;
let _keyRevealed = false;

// Helper: re-render the detail pane keeping the same selection.
function refreshDownstreamDetail() {
    if (_currentDownstream) renderDownstreamDetail(_currentDownstream);
}

(async function setupDownstreamsDelegation() {
    const root = document.getElementById('downstreams-detail');
    if (!root) return;

    // Auto-save text inputs on blur (delegated; works after any re-render).
    root.addEventListener('blur', async (e) => {
        if (!_currentDownstream) return;
        const t = e.target;
        if (t.classList.contains('detail-edit-name')) {
            const newName = t.value.trim();
            if (!newName) { t.value = _currentDownstream.name; showToast('Name cannot be empty'); return; }
            if (newName === _currentDownstream.name) return;
            const prev = _currentDownstream.name;
            await autoSaveDownstreamField(_currentDownstream.id, { name: newName }, (err) => {
                if (err) { t.value = prev; return; }
                _currentDownstream.name = newName;
                const li = document.querySelector('#downstreams-list li[data-id="' + _currentDownstream.id + '"] .ds-name');
                if (li) li.textContent = newName;
            });
        } else if (t.classList.contains('detail-edit-url')) {
            const newUrl = t.value.trim();
            if (!newUrl) { t.value = _currentDownstream.base_url || ''; showToast('Base URL cannot be empty'); return; }
            if (newUrl === (_currentDownstream.base_url || '')) return;
            const prev = _currentDownstream.base_url || '';
            await autoSaveDownstreamField(_currentDownstream.id, { base_url: newUrl }, (err) => {
                if (err) { t.value = prev; return; }
                _currentDownstream.base_url = newUrl;
            });
        } else if (t.classList.contains('detail-edit-key')) {
            // ponytail: empty → no-op. typing replaces the saved key on save.
            if (t.value === '') return;
            if (_currentDownstream.dataset && _currentDownstream.dataset.justRevealed === '1') {
                t.dataset.justRevealed = '';
                return;
            }
            const newKey = t.value;
            await autoSaveDownstreamField(_currentDownstream.id, { api_key: newKey }, (err) => {
                if (err) { t.value = ''; return; }
                _currentDownstream.api_key = newKey;
                t.value = '';
                t.type = 'password';
                const eye = root.querySelector('[data-action="toggle-key"]');
                if (eye) { eye.classList.remove('shown'); eye.textContent = '👁'; }
                _keyRevealed = false;
            });
        }
    }, true);

    // Enter on text inputs triggers blur (skip form submit).
    root.addEventListener('keydown', (e) => {
        if (e.key !== 'Enter') return;
        const t = e.target;
        if (t.matches('.detail-edit-name, .detail-edit-url, .detail-edit-key, .add-model-input')) {
            e.preventDefault();
            t.blur();
        }
    });

    // Clicks: format checkboxes, model remove, add-model cancel/submit/btn,
    // fetch-models, header delete, eye toggle.
    root.addEventListener('change', async (e) => {
        if (!_currentDownstream) return;
        const t = e.target;
        if (t.matches('.format-checkboxes input[type="checkbox"]')) {
            const checked = Array.from(root.querySelectorAll('.format-checkboxes input[type="checkbox"]:checked'), c => c.value);
            await autoSaveDownstreamField(_currentDownstream.id, { api_formats: checked }, (err) => {
                if (err) {
                    const target = _currentDownstream.api_formats || [];
                    root.querySelectorAll('.format-checkboxes input[type="checkbox"]').forEach(c => { c.checked = target.includes(c.value); });
                    return;
                }
            });
        }
    });

    root.addEventListener('click', async (e) => {
        if (!_currentDownstream) return;
        const btn = e.target.closest('[data-action]');
        if (!btn) return;
        const action = btn.dataset.action;
        const id = _currentDownstream.id;

        if (action === 'delete') { deleteDownstream(id); return; }
        if (action === 'fetch-models') { fetchDownstreamModels(id); return; }
        if (action === 'add-model') {
            const row = root.querySelector('[data-role="add-model-row"]');
            const input = root.querySelector('.add-model-input');
            row.style.display = '';
            input.focus();
            return;
        }
        if (action === 'add-model-cancel') {
            const input = root.querySelector('.add-model-input');
            input.value = '';
            root.querySelector('[data-role="add-model-row"]').style.display = 'none';
            return;
        }
        if (action === 'add-model-submit') {
            const input = root.querySelector('.add-model-input');
            const modelId = input.value.trim();
            if (!modelId) { showToast('Type a model ID first'); return; }
            try {
                const updated = await api('/downstreams/' + encodeURIComponent(id) + '/models', {
                    method: 'POST',
                    body: JSON.stringify({ model_id: modelId }),
                });
                _currentDownstream.output_model_ids = updated.output_model_ids || _currentDownstream.output_model_ids;
                refreshDownstreamDetail();
                selectSidebarItem(id);
                showToast('Added ' + modelId);
            } catch (err) {
                showToast('Add failed: ' + err.message);
            }
            return;
        }
        if (action === 'toggle-key') {
            const keyInput = root.querySelector('.detail-edit-key');
            if (_keyRevealed) {
                keyInput.type = 'password';
                btn.classList.remove('shown');
                btn.textContent = '👁';
                keyInput.value = '';
                keyInput.dataset.justRevealed = '';
                _keyRevealed = false;
                return;
            }
            if (!_currentDownstream.api_key) {
                showToast('No API key set yet — type one to add');
                keyInput.focus();
                return;
            }
            btn.disabled = true;
            const realKey = await revealApiKey(id);
            btn.disabled = false;
            if (!realKey) return;
            keyInput.dataset.justRevealed = '1';
            keyInput.type = 'text';
            keyInput.value = realKey;
            btn.classList.add('shown');
            btn.textContent = '🙈';
            _keyRevealed = true;
            return;
        }
        if (btn.classList.contains('model-id-remove')) {
            const li = btn.closest('li[data-model-id]');
            const modelId = li && li.dataset.modelId;
            if (!modelId) return;
            try {
                await api('/downstreams/' + encodeURIComponent(id) + '/models/' + encodeURIComponent(modelId), { method: 'DELETE' });
                _currentDownstream.output_model_ids = (_currentDownstream.output_model_ids || []).filter(m => m !== modelId);
                // ponytail: re-render is the simplest path to keep the empty-state
                // markup and the count label in sync with the new array length.
                refreshDownstreamDetail();
                showToast('Removed ' + modelId);
            } catch (err) {
                showToast('Delete failed: ' + err.message);
            }
        }
    });
})();

/**
 * Patch one or more fields on the downstream and PUT. On success, fires
 * the optional callback (for local-state updates like re-rendering sidebar text).
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

// ponytail: navigator.clipboard.writeText is universally available in modern
// browsers; the execCommand fallback path adds 20 lines for a vanishing niche.
async function copyText(text, msg) {
    if (!text) return;
    try {
        await navigator.clipboard.writeText(text);
        showToast(msg || 'Copied');
    } catch {
        showToast('Copy failed');
    }
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
                    if (_currentDownstream && _currentDownstream.id === dsId) {
                        renderDownstreamDetail(_currentDownstream);
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
 * and let the user fill in the fields inline.
 */
async function createNewDownstream() {
    try {
        // Backend rejects empty name/base_url, so seed placeholders the user must replace
        // before blur-save can run.
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
 * ponytail: cleanupModal kept for forward-compat — currently no modal sets
 * modal._fetchListeners; this returns nothing in the present code.
 */
function cleanupModal(modal) {
    const l = modal._fetchListeners;
    if (!l) return;
    if (l.url && l.handler) l.url.removeEventListener('input', l.handler);
    if (l.key && l.handler) l.key.removeEventListener('input', l.handler);
    delete modal._fetchListeners;
}

// ---- Utility ----
function esc(s) {
    if (s == null) return '';
    // ponytail: 5 chars of escape-mapping is enough; newlines never carry data.
    return String(s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
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
    // For regex groups, the pattern itself often names the brand (e.g. "claude.*"),
    // so we still pass the full pattern through the same /api/icons endpoint —
    // the server's pattern table resolves the brand from the leading segment.
    title.innerHTML = modelIconHTML(group.input_model_id) + esc(group.input_model_id) +
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

        // Downstream brand icon + name (bottom line, smaller). The icon is
        // resolved through the same /api/icons endpoint used elsewhere; when
        // no CDN slug matches, the endpoint returns the generic dummy icon
        // (internal/api/icons.go) so this slot is never blank.
        const dsLabel = document.createElement('div');
        dsLabel.className = 'option-downstream';
        const dsName = opt.downstream_name || opt.downstream_id;
        dsLabel.innerHTML = modelIconHTML(dsName) + esc(dsName);
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

        // Render the provider's model icon (CDN-backed SVG), hidden gracefully
        // on miss via onerror in modelIconHTML.
        const iconWrap = document.createElement('span');
        iconWrap.className = 'model-tag-model-icon';
        iconWrap.innerHTML = modelIconHTML(m);
        tag.appendChild(iconWrap);

        const checkGlyph = document.createElement('span');
        checkGlyph.className = 'model-tag-icon';
        checkGlyph.textContent = '☐';
        tag.appendChild(checkGlyph);

        const name = document.createElement('span');
        name.className = 'model-tag-name';
        name.textContent = m;
        tag.appendChild(name);

        tag.addEventListener('click', () => {
            cb.checked = !cb.checked;
            tag.classList.toggle('selected', cb.checked);
            checkGlyph.textContent = cb.checked ? '☑' : '☐';
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
        await Promise.all(models.map(m => api('/aliases', {
            method: 'POST',
            body: JSON.stringify({
                input_model_id: inputModelId,
                downstream_id: downstreamId,
                output_model_id: m,
                is_active: false,
            }),
        })));
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
        // ponytail: each insert sets `is_active = (i === 0)`, so DB writes must
        // be sequential; otherwise parallel inserts race on which row becomes
        // the active one. Promise.all would corrupt the active bit.
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
        // Populate the payload-capture checkbox
        const captureEl = document.getElementById('setting-capture-payloads');
        if (captureEl) {
            captureEl.checked = !!cfg.capture_payloads;
            capturePayloadsEnabled = captureEl.checked;
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
        // Include the payload-capture flag if the checkbox exists
        const captureEl = document.getElementById('setting-capture-payloads');
        if (captureEl) {
            body.capture_payloads = captureEl.checked;
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

function initLogs() {
    // Load recent entries from the REST API (for initial render)
    fetchLogs();
    setupLogInspect();
    setupInspectViewToggle();
}

// Delegated click handler for clickable log rows. initLogs runs once.
function setupLogInspect() {
    const container = document.getElementById('logs-stream');
    if (!container) return;
    container.addEventListener('click', function (ev) {
        const row = ev.target.closest('.log-entry.clickable');
        if (!row) return;
        const id = parseInt(row.dataset.id, 10);
        if (!isNaN(id)) openLogInspect(id);
    });
}

/**
 * Open the inspector modal for the given log id. Fetches
 * /api/logs/{id}/inspect and renders the captured raw request and
 * response bodies. Body rendering rules:
 *   - application/json (request or response): pretty-print with 2-space indent
 *   - text/event-stream: render verbatim, preserving the `data: ...\n\n` framing
 *   - anything else: render as plain text inside a <pre>
 * Truncation and missing-capture states are surfaced as banners.
 */
// State for the inspector modal. The view toggle re-renders from
// currentInspectData without re-fetching, so flipping between Raw and
// Parsed is instant and never hits the network.
let currentInspectData = null;
let currentInspectPath = '';
let currentInspectView = 'raw'; // 'raw' | 'parsed'
// Single global toggle: when true, text/thinking/tool_result text bodies
// in the Parsed view are rendered as markdown via marked+DOMPurify instead
// of as plain textContent. Default to true, since it is most user-friendly
let currentInspectMarkdown = true;

async function openLogInspect(id) {
    const modal = document.getElementById('log-inspect-modal');
    const metaEl = document.getElementById('log-inspect-meta');
    const statusEl = document.getElementById('log-inspect-status');
    const bodyEl = document.getElementById('log-inspect-body');
    if (!modal || !metaEl || !statusEl || !bodyEl) return;

    // Reset state for the new entry
    currentInspectData = null;
    currentInspectView = 'parsed';
    currentInspectMarkdown = true;
    setActiveInspectView('parsed');
    metaEl.innerHTML = '';
    statusEl.className = 'inspect-status';
    statusEl.style.display = 'none';
    bodyEl.innerHTML = '<div class="inspect-empty">Loading\u2026</div>';
    modal.classList.remove('hidden');

    let data;
    try {
        const resp = await fetch(API_BASE + '/logs/' + id + '/inspect', { credentials: 'same-origin' });
        if (resp.status === 404) {
            bodyEl.innerHTML = '';
            statusEl.textContent = 'No captured payload for this request. Either the inspector was disabled when the request came in, or the entry has aged out of the in-memory cache (most recent 100 only).';
            statusEl.className = 'inspect-status error';
            statusEl.style.display = 'block';
            return;
        }
        if (!resp.ok) {
            const text = await resp.text();
            throw new Error('HTTP ' + resp.status + ': ' + text);
        }
        data = await resp.json();
    } catch (err) {
        bodyEl.innerHTML = '';
        statusEl.textContent = 'Failed to fetch captured payload: ' + err.message;
        statusEl.className = 'inspect-status error';
        statusEl.style.display = 'block';
        return;
    }

    // Header meta
    metaEl.innerHTML = renderInspectMeta(data);
    currentInspectData = data;
    currentInspectPath = data.path || '';
    renderInspectBody();
}

function setupInspectViewToggle() {
    const raw = document.getElementById('inspect-view-raw');
    const parsed = document.getElementById('inspect-view-parsed');
    if (!raw || !parsed) return;
    raw.addEventListener('click', function () {
        setActiveInspectView('raw');
        currentInspectView = 'raw';
        renderInspectBody();
    });
    parsed.addEventListener('click', function () {
        setActiveInspectView('parsed');
        currentInspectView = 'parsed';
        renderInspectBody();
    });
}

function setActiveInspectView(view) {
    const raw = document.getElementById('inspect-view-raw');
    const parsed = document.getElementById('inspect-view-parsed');
    if (!raw || !parsed) return;
    if (view === 'raw') {
        raw.classList.add('active'); raw.setAttribute('aria-selected', 'true');
        parsed.classList.remove('active'); parsed.setAttribute('aria-selected', 'false');
    } else {
        parsed.classList.add('active'); parsed.setAttribute('aria-selected', 'true');
        raw.classList.remove('active'); raw.setAttribute('aria-selected', 'false');
    }
}

function renderInspectBody() {
    const bodyEl = document.getElementById('log-inspect-body');
    if (!bodyEl || !currentInspectData) return;
    bodyEl.innerHTML = '';
    if (currentInspectView === 'parsed') {
        bodyEl.appendChild(buildInspectMarkdownToggle());
        bodyEl.appendChild(renderInspectSectionParsed('Request (parsed)', currentInspectData.request, currentInspectPath, 'request'));
        bodyEl.appendChild(renderInspectSectionParsed('Response (parsed)', currentInspectData.response, currentInspectPath, 'response'));
    } else {
        bodyEl.appendChild(renderInspectSection('Raw client request (before plugins)', currentInspectData.request));
        bodyEl.appendChild(renderInspectSection('Raw downstream response (before plugins)', currentInspectData.response));
    }
}

// Build the small "Render as Markdown" checkbox that sits at the top of
// the Parsed view. When toggled, re-renders the body in place so the
// current view (already loaded payloads) is just re-painted; no
// re-fetch. The flag is global for the current modal — flipping it
// affects every text/thinking/tool_result block on both Request and
// Response.
function buildInspectMarkdownToggle() {
    const wrap = document.createElement('label');
    wrap.className = 'inspect-md-toggle';
    wrap.title = 'Render text/thinking/tool result bodies as Markdown (bold, code, lists, etc.). DOMPurify sanitizes the output.';

    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.id = 'inspect-md-toggle-cb';
    cb.checked = !!currentInspectMarkdown;
    cb.addEventListener('change', function () {
        currentInspectMarkdown = cb.checked;
        // Re-paint only the parsed children, not the toggle itself, so
        // the checkbox doesn't flicker out from under the user.
        const bodyEl = document.getElementById('log-inspect-body');
        if (!bodyEl) return;
        // Remove everything after the toggle and re-render.
        while (bodyEl.children.length > 1) bodyEl.removeChild(bodyEl.lastChild);
        bodyEl.appendChild(renderInspectSectionParsed('Request (parsed)', currentInspectData.request, currentInspectPath, 'request'));
        bodyEl.appendChild(renderInspectSectionParsed('Response (parsed)', currentInspectData.response, currentInspectPath, 'response'));
    });

    const span = document.createElement('span');
    span.className = 'inspect-md-toggle-label';
    span.textContent = 'Render text blocks as Markdown';

    wrap.appendChild(cb);
    wrap.appendChild(span);
    return wrap;
}

function renderInspectMeta(d) {
    // Downstream: prefer the human-readable name when present, fall back
    // to the ID. The engine populates downstream_name at capture time
    // (see engine.recordAndCapture).
    const downstream = d.downstream_name || d.downstream_id || '';
    const rows = [
        ['Method', d.method || ''],
        ['Path', d.path || ''],
        ['Status', d.status != null ? String(d.status) : ''],
        ['Model', d.model || ''],
        ['Resolved model', d.resolved_model || ''],
        ['Downstream', downstream],
        ['Client IP', d.client_ip || ''],
        ['Captured at', d.timestamp || ''],
    ];
    return rows
        .filter(function (r) { return r[1] && r[1] !== ''; })
        .map(function (r) {
            return '<div class="meta-key">' + esc(r[0]) + '</div>' +
                '<div class="meta-val">' + esc(r[1]) + '</div>';
        }).join('');
}

function renderInspectSection(title, body) {
    const wrap = document.createElement('div');
    wrap.className = 'inspect-section';

    const header = document.createElement('div');
    header.className = 'inspect-section-header';

    const titleEl = document.createElement('h4');
    titleEl.textContent = title;
    header.appendChild(titleEl);

    const badges = document.createElement('div');
    badges.className = 'badges';
    if (body && body.content_type) {
        const ct = document.createElement('span');
        ct.className = 'badge-ct';
        ct.textContent = body.content_type;
        badges.appendChild(ct);
    }
    if (body && body.truncated) {
        const tr = document.createElement('span');
        tr.className = 'badge-truncated';
        tr.textContent = 'truncated';
        badges.appendChild(tr);
    }
    const copyBtn = document.createElement('button');
    copyBtn.type = 'button';
    copyBtn.className = 'copy-btn';
    copyBtn.textContent = 'Copy';
    copyBtn.onclick = function () {
        const text = body && typeof body.body === 'string' ? body.body : '';
        if (!text) return;
        if (navigator.clipboard && navigator.clipboard.writeText) {
            navigator.clipboard.writeText(text).then(function () {
                copyBtn.textContent = 'Copied';
                copyBtn.classList.add('copied');
                setTimeout(function () {
                    copyBtn.textContent = 'Copy';
                    copyBtn.classList.remove('copied');
                }, 1200);
            }, function () {
                copyBtn.textContent = 'Copy failed';
                setTimeout(function () { copyBtn.textContent = 'Copy'; }, 1200);
            });
        } else {
            // Fallback for browsers without async clipboard
            const ta = document.createElement('textarea');
            ta.value = text;
            document.body.appendChild(ta);
            ta.select();
            try { document.execCommand('copy'); copyBtn.textContent = 'Copied'; } catch (e) { copyBtn.textContent = 'Copy failed'; }
            document.body.removeChild(ta);
            setTimeout(function () { copyBtn.textContent = 'Copy'; }, 1200);
        }
    };
    badges.appendChild(copyBtn);
    header.appendChild(badges);
    wrap.appendChild(header);

    if (!body || !body.body) {
        const empty = document.createElement('div');
        empty.className = 'inspect-empty';
        empty.textContent = '(no body captured)';
        wrap.appendChild(empty);
        return wrap;
    }

    const pre = document.createElement('pre');
    pre.className = 'inspect-pre';
    const ct = (body.content_type || '').toLowerCase();
    if (ct.indexOf('application/json') !== -1) {
        pre.classList.add('json');
        // Step 1: if Content-Type: application/json, parse the body and pretty-print
        pre.textContent = prettyPrintJSON(body.body);
    } else if (ct.indexOf('text/event-stream') !== -1) {
        // SSE: keep verbatim, monospace without wrapping so data: lines align
        pre.classList.add('sse');
        pre.textContent = body.body;
    } else {
        // Step 2: non-JSON / non-SSE — render as <pre> text
        pre.textContent = body.body;
    }
    wrap.appendChild(pre);
    return wrap;
}

function prettyPrintJSON(s) {
    try {
        return JSON.stringify(JSON.parse(s), null, 2);
    } catch (e) {
        // Not valid JSON — return raw. The server already stored the wire
        // bytes; if they aren't valid JSON, that's what the operator needs
        // to see.
        return s;
    }
}

/**
 * Build a parsed-section element. Reconstructs a streaming response into
 * one human-readable view (text + thinking + tool_use blocks) without
 * the wall of `data: {...}` lines that the Raw view produces.
 *
 * Falls back to the raw view when the body doesn't parse as JSON, when
 * it isn't a recognised format, or when reassembly produces nothing
 * (e.g. a non-streaming OpenAI Responses JSON body — still pretty-
 * printed but reported in the parsed tab as a single shape).
 */
function renderInspectSectionParsed(title, body, path, kind) {
    const wrap = document.createElement('div');
    wrap.className = 'inspect-section';

    const header = document.createElement('div');
    header.className = 'inspect-section-header';
    const h4 = document.createElement('h4');
    h4.textContent = title;
    header.appendChild(h4);
    // No copy button on the parsed view: the parsed body isn't a single
    // string of plain text the user would copy (it's a tree of message
    // blocks, usage stats, etc.). The raw view keeps its copy button
    // for users who want to paste the wire bytes somewhere.
    wrap.appendChild(header);

    if (!body || !body.body) {
        const empty = document.createElement('div');
        empty.className = 'inspect-empty';
        empty.textContent = '(no body captured)';
        wrap.appendChild(empty);
        return wrap;
    }

    const contentType = (body.content_type || '').toLowerCase();
    let isStreaming = contentType.indexOf('text/event-stream') !== -1;
    // Some servers lie about the content-type when streaming — they send
    // `application/json` while the body is still SSE. Detect by shape:
    // a body that begins with `data: ` and contains a blank line is SSE.
    if (!isStreaming && body.body && /^\s*data:\s/m.test(body.body) &&
        body.body.indexOf('\n\n') !== -1) {
        isStreaming = true;
    }

    let parsedView = null;
    let parseError = null;
    try {
        parsedView = buildParsedView(body.body, path, kind, isStreaming);
    } catch (e) {
        parseError = e && e.message ? e.message : String(e);
        if (e && e.stack) console.error('[inspect] parser threw: ' + title + ' error=' + parseError);
    }

    if (parsedView && parsedView.messages && parsedView.messages.length) {
        wrap.appendChild(renderParsedMessages(parsedView));
        return wrap;
    }
    if (isStreaming && parsedView && parsedView.complete === false) {
        // Mid-stream snapshot: render what we have, plus a note that the
        // final response may differ.
        const warn = document.createElement('div');
        warn.className = 'inspect-fallback';
        warn.textContent = 'Stream incomplete — showing partial reconstruction.';
        wrap.appendChild(warn);
        if (parsedView.messages && parsedView.messages.length) {
            wrap.appendChild(renderParsedMessages(parsedView));
        } else {
            wrap.appendChild(buildRawFallback(body));
        }
        return wrap;
    }
    if (parseError || !parsedView) {
        const note = document.createElement('div');
        note.className = 'inspect-fallback';
        note.textContent = 'Parser unavailable for this body'
            + (parseError ? (': ' + parseError) : '')
            + '. Showing raw.';
        wrap.appendChild(note);
        wrap.appendChild(buildRawFallback(body));
        return wrap;
    }
    // Reached the empty-messages path: the parser ran without error but
    // couldn't reconstruct any visible content. This happens when the
    // reassembler produced a snapshot with all-empty text fields, or the
    // body shape isn't one we recognise. Show the raw view so the
    // operator isn't stranded.
    const note = document.createElement('div');
    note.className = 'inspect-fallback';
    note.textContent = 'No parsed content reconstructed. Showing raw.';
    wrap.appendChild(note);
    wrap.appendChild(buildRawFallback(body));
    return wrap;
}

/**
 * Reconstruct a parsed view of a request or response body.
 * For a streaming body, run it through the SSE reassembler; for a
 * non-streaming JSON body, normalise once via the format-specific helpers.
 * Returns { messages: [...], usage?: {...}, complete: bool, format?: string }
 */
function buildParsedView(rawBody, path, kind, isStreaming) {
    // Try to detect the format even before parsing, so the error message
    // can hint "unknown format" rather than "JSON parse error".
    let preview = rawBody;
    // Pull out the first SSE data line for format sniffing if streaming.
    if (isStreaming) {
        const first = firstSseDataLine(rawBody);
        if (first) preview = first;
    }

    const format = (typeof detectRequestFormat === 'function')
        ? detectRequestFormat(path, (function () { try { return JSON.parse(preview); } catch (e) { return {}; } })())
        : null;

    if (!format) {
        return { messages: [], complete: false, format: null };
    }

    if (isStreaming) {
        const r = new SSEReassembler();
        r.feed(rawBody);
        const snapshot = r.reconstruct();
        if (!snapshot) return { messages: [], complete: false, format: format };
        const norm = (kind === 'response')
            ? normalizeResponse(snapshot, path)
            : null;
        if (norm) return { messages: [norm], usage: norm.usage || null, complete: true, format: format };
        // For requests, snapshots from the reassembler don't apply — fall
        // through to the non-streaming parser.
        return { messages: [], complete: false, format: format };
    }

    let parsed;
    try { parsed = JSON.parse(rawBody); }
    catch (e) { return { messages: [], complete: false, format: format, error: 'JSON parse failed' }; }

    if (kind === 'request') {
        const req = normalizeRequest(parsed, path);
        if (!req) return { messages: [], complete: true, format: format };
        const messages = [];
        if (req.system) {
            // req.system is now a list of {type:'text', text} blocks
            // (string form is wrapped upstream by normalizeAnthropicSystem
            // and the equivalent for chat_completions / responses). Each
            // block becomes one inspect-msg with role 'system', so a
            // Anthropic request with two system blocks renders as two
            // distinct system turns rather than one mashed-together
            // string.
            const sysBlocks = Array.isArray(req.system) ? req.system : null;
            if (sysBlocks && sysBlocks.length) {
                for (const sb of sysBlocks) {
                    messages.push({ role: 'system', content: [sb] });
                }
            } else if (typeof req.system === 'string') {
                messages.push({ role: 'system', content: [{ type: 'text', text: req.system }] });
            }
        }
        for (const m of (req.messages || [])) messages.push(m);
        return { messages: messages, complete: true, format: format };
    }

    // response
    const resp = normalizeResponse(parsed, path);
    if (!resp) return { messages: [], complete: true, format: format };
    return { messages: [resp], usage: resp.usage || null, complete: true, format: format };
}

function firstSseDataLine(s) {
    // Pull the first event's first non-empty data: line so format detection
    // can run on a single sample, which is enough to identify the four
    // supported protocols.
    const lines = s.split(/\r?\n/);
    for (let i = 0; i < lines.length; i++) {
        const line = lines[i];
        if (line.indexOf('data:') !== 0) continue;
        let rest = line.slice(5);
        if (rest.charAt(0) === ' ') rest = rest.slice(1);
        if (rest && rest !== '[DONE]') return rest;
    }
    return null;
}

function buildRawFallback(body) {
    const pre = document.createElement('pre');
    pre.className = 'inspect-pre';
    const ct = (body.content_type || '').toLowerCase();
    if (ct.indexOf('application/json') !== -1) {
        pre.classList.add('json');
        pre.textContent = prettyPrintJSON(body.body);
    } else if (ct.indexOf('text/event-stream') !== -1) {
        pre.classList.add('sse');
        pre.textContent = body.body;
    } else {
        pre.textContent = body.body;
    }
    return pre;
}

function renderParsedMessages(view) {
    const out = document.createElement('div');
    out.className = 'inspect-parsed';
    for (const m of view.messages) {
        out.appendChild(renderOneMessage(m));
    }
    if (view.usage) {
        const u = view.usage;
        const parts = [];
        if (u.input_tokens != null)      parts.push('<strong>' + u.input_tokens + '</strong> in');
        if (u.output_tokens != null)     parts.push('<strong>' + u.output_tokens + '</strong> out');
        if (u.cache_read_input_tokens)    parts.push('<strong>' + u.cache_read_input_tokens + '</strong> cached');
        if (parts.length) {
            const d = document.createElement('div');
            d.className = 'inspect-usage';
            d.innerHTML = parts.join(' \u00b7 ');
            out.appendChild(d);
        }
    }
    return out;
}

function renderOneMessage(m) {
    const wrap = document.createElement('div');
    const role = (m.role || 'user').toLowerCase();
    wrap.className = 'inspect-msg';

    const header = document.createElement('div');
    header.className = 'inspect-msg-header inspect-msg-role-' + role;
    header.textContent = role;
    wrap.appendChild(header);

    const body = document.createElement('div');
    body.className = 'inspect-msg-body';

    // Content can be: string, array of blocks, array of OpenAI-style
    // chat parts, image parts, etc. Normalise to a list of content blocks
    // we can render.
    const blocks = normaliseContentBlocks(m.content);
    if (!blocks.length) {
        const raw = m.raw;
        if (raw) {
            const pre = document.createElement('pre');
            pre.className = 'inspect-block-tool-args';
            pre.textContent = JSON.stringify(raw, null, 2);
            body.appendChild(pre);
        } else {
            body.textContent = '(empty)';
        }
    } else {
        for (const block of blocks) body.appendChild(renderContentBlock(block));
    }
    wrap.appendChild(body);
    return wrap;
}

// normaliseContentBlocks lives in content-normalise.js (loaded as a
// <script> before this file via index.html) so it can be unit-tested
// under Node without dragging in the DOM-heavy app.js. It's exposed
// as the global `normaliseContentBlocks` and called directly where
// needed.


function renderContentBlock(block) {
    const w = document.createElement('div');
    w.className = 'inspect-block';
    if (block.type === 'text') {
        w.appendChild(renderInspectText(block.text, 'text'));
    } else if (block.type === 'thinking') {
        // Thinking blocks are collapsible: collapsed by default. The
        // header row carries the toggle so the toggle target is the
        // whole label area, not just the bullet triangle.
        const header = document.createElement('button');
        header.type = 'button';
        header.className = 'inspect-block-thinking-header';
        header.setAttribute('aria-expanded', 'false');
        const arrow = document.createElement('span');
        arrow.className = 'inspect-block-thinking-arrow';
        arrow.textContent = '\u25B6'; // ▶ collapsed
        const label = document.createElement('span');
        label.className = 'inspect-block-thinking-label';
        label.textContent = 'Thinking';
        header.appendChild(arrow);
        header.appendChild(label);
        const body = document.createElement('div');
        body.className = 'inspect-block-thinking';
        body.appendChild(renderInspectText(block.thinking || block.text || '', 'thinking'));
        header.addEventListener('click', function () {
            const open = header.getAttribute('aria-expanded') === 'true';
            header.setAttribute('aria-expanded', open ? 'false' : 'true');
            body.classList.toggle('open', !open);
            arrow.textContent = open ? '\u25B6' : '\u25BC';
        });
        w.appendChild(header);
        w.appendChild(body);
    } else if (block.type === 'system_reminder') {
        // Injected-by-client block (Claude Code wraps context in
        // <system-reminder>, local commands in <local-command-caveat>,
        // etc.). Render as a small dimmed callout with a label, body
        // verbatim. Collapsed by default because the wrapped content is
        // often long and the user usually just wants to see the
        // surrounding user turn.
        w.className = 'inspect-block inspect-block-system-reminder';
        const header = document.createElement('button');
        header.type = 'button';
        header.className = 'inspect-block-system-reminder-header';
        header.setAttribute('aria-expanded', 'false');
        const arrow = document.createElement('span');
        arrow.className = 'inspect-block-system-reminder-arrow';
        arrow.textContent = '\u25B6';
        const label = document.createElement('span');
        label.className = 'inspect-block-system-reminder-label';
        label.textContent = block.label || 'Injected';
        const tagBadge = document.createElement('span');
        tagBadge.className = 'inspect-block-system-reminder-tag';
        tagBadge.textContent = '<' + (block.tag || 'injected') + '>';
        header.appendChild(arrow);
        header.appendChild(label);
        header.appendChild(tagBadge);
        const body = document.createElement('div');
        body.className = 'inspect-block-system-reminder-body';
        body.appendChild(renderInspectText(block.text || '', 'system_reminder'));
        header.addEventListener('click', function () {
            const open = header.getAttribute('aria-expanded') === 'true';
            header.setAttribute('aria-expanded', open ? 'false' : 'true');
            body.classList.toggle('open', !open);
            arrow.textContent = open ? '\u25B6' : '\u25BC';
        });
        w.appendChild(header);
        w.appendChild(body);
    } else if (block.type === 'tool_use') {
        const name = document.createElement('div');
        name.className = 'inspect-block-tool-name';
        name.textContent = block.name ? ('Tool call: ' + block.name) : 'Tool call';
        const args = document.createElement('pre');
        args.className = 'inspect-block-tool-args';
        const input = block.input;
        args.textContent = (input && typeof input === 'object')
            ? JSON.stringify(input, null, 2)
            : (typeof input === 'string' ? input : '');
        w.appendChild(name); w.appendChild(args);
    } else if (block.type === 'tool_result') {
        // Anthropic tool_result block: a labelled body with the
        // tool_use_id as the link back to the matching tool_use. The
        // body is either a string (most bash/file outputs) or an
        // array of blocks (when a tool returned images, e.g. a
        // screenshot from a headless browser).
        w.className = 'inspect-block inspect-block-tool-result'
            + (block.is_error ? ' inspect-block-tool-result-error' : '');
        const header = document.createElement('div');
        header.className = 'inspect-block-tool-result-header';
        const label = document.createElement('span');
        label.className = 'inspect-block-tool-result-label';
        label.textContent = block.is_error ? 'Tool result (error)' : 'Tool result';
        if (block.tool_use_id) {
            const id = document.createElement('span');
            id.className = 'inspect-block-tool-result-id';
            id.textContent = block.tool_use_id;
            header.appendChild(label);
            header.appendChild(id);
        } else {
            header.appendChild(label);
        }
        w.appendChild(header);
        const body = document.createElement('div');
        body.className = 'inspect-block-tool-result-body';
        // Empty results get a quiet "(empty)" marker so the operator
        // can tell the tool returned nothing from a missing block.
        if (block.content == null || block.content === '') {
            body.textContent = '(empty result)';
            body.classList.add('inspect-block-tool-result-empty');
        } else if (typeof block.content === 'string') {
            const pre = document.createElement('pre');
            pre.className = 'inspect-block-tool-result-pre';
            body.appendChild(renderInspectText(block.content, 'tool_result'));
        } else if (Array.isArray(block.content)) {
            for (const c of block.content) {
                if (c == null) continue;
                if (typeof c === 'string') {
                    body.appendChild(renderInspectText(c, 'tool_result'));
                    // Note: was a <pre> for monospace alignment, but markdown rendering
                    // expects block-flow elements. renderInspectText returns a <div>; we
                    // rely on .inspect-md styling to keep readability.
                } else if (typeof c === 'object') {
                    if (c.type === 'text' || c.text != null) {
                        body.appendChild(renderInspectText(c.text || '', 'tool_result'));
                    } else if (c.type === 'image' || c.type === 'input_image' || c.type === 'image_url') {
                        const imgWrap = document.createElement('div');
                        imgWrap.className = 'inspect-block-tool-result-image';
                        const url = (c.image_url && c.image_url.url) || c.url || c.source_url;
                        if (url) {
                            const img = document.createElement('img');
                            img.src = url;
                            img.alt = '(tool result image)';
                            imgWrap.appendChild(img);
                        } else {
                            imgWrap.textContent = '(image — no url)';
                        }
                        body.appendChild(imgWrap);
                    } else {
                        const pre = document.createElement('pre');
                        pre.className = 'inspect-block-tool-result-pre';
                        pre.textContent = JSON.stringify(c, null, 2);
                        body.appendChild(pre);
                    }
                }
            }
        } else {
            // Object content (rare). Dump as JSON for visibility.
            const pre = document.createElement('pre');
            pre.className = 'inspect-block-tool-result-pre';
            pre.textContent = JSON.stringify(block.content, null, 2);
            body.appendChild(pre);
        }
        w.appendChild(body);
    } else if (block.type === 'image') {
        const img = document.createElement('div');
        img.className = 'inspect-block-image';
        if (block.url) {
            const i = document.createElement('img');
            i.src = block.url;
            i.alt = '(image)';
            img.appendChild(i);
        } else {
            img.textContent = '(image)';
        }
        w.appendChild(img);
    } else {
        // raw / unknown
        const pre = document.createElement('pre');
        pre.className = 'inspect-block-tool-args';
        pre.textContent = JSON.stringify(block.value || block, null, 2);
        w.appendChild(pre);
    }
    return w;
}

// Render a free-text body. Default is plain textContent (preserves
// the historical behaviour where the operator sees the exact bytes the
// client sent). When the markdown toggle is on, pipe the text through
// marked -> DOMPurify so the operator sees the formatted result while
// staying safe against XSS in untrusted LLM output. CSS targets the
// returned element via `.inspect-md` for spacing overrides.
function renderInspectText(text, kind) {
    const s = text == null ? '' : String(text);
    if (!currentInspectMarkdown) {
        const pre = document.createElement('div');
        pre.className = 'inspect-text-plain';
        pre.textContent = s;
        return pre;
    }
    const wrap = document.createElement('div');
    wrap.className = 'inspect-md inspect-md-' + (kind || 'text');
    try {
        // marked v12 exposes marked.parse(); DOMPurify strips scripts,
        // on* handlers, and javascript:/data: URLs.
        const rawHtml = (window.marked && typeof window.marked.parse === 'function')
            ? window.marked.parse(s, { gfm: true, breaks: true })
            : null;
        const safe = (window.DOMPurify && typeof window.DOMPurify.sanitize === 'function')
            ? DOMPurify.sanitize(rawHtml != null ? rawHtml : esc(s), {
                USE_PROFILES: { html: true },
                ADD_ATTR: ['target', 'rel'],
            })
            : esc(s);
        wrap.innerHTML = safe;
        // Force links to open in a new tab so the inspector stays open.
        // DOMPurify strips href=javascript: but doesn't add rel/target.
        wrap.querySelectorAll('a').forEach(function (a) {
            a.setAttribute('target', '_blank');
            a.setAttribute('rel', 'noopener noreferrer');
        });
        return wrap;
    } catch (e) {
        // Library missing or threw — fall back to plain text so the
        // operator still sees the body.
        const pre = document.createElement('div');
        pre.className = 'inspect-text-plain';
        pre.textContent = s;
        return pre;
    }
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
 * ponytail: EventSource auto-reconnects; only reconnect manually when the
 * browser has given up (CLOSED) so we can rebuild ordering.
 */
function connectLogStream() {
    if (logSSE) return; // already connected

    const badge = document.getElementById('log-status-badge');
    const url = API_BASE + '/logs/stream';

    try { logSSE = new EventSource(url); }
    catch {
        if (badge) { badge.textContent = '✗ Offline'; badge.style.background = 'var(--color-danger)'; }
        return;
    }

    if (badge) { badge.textContent = '● Live'; badge.style.background = 'var(--color-success)'; }

    logPaused = false;
    const pauseBtn = document.getElementById('btn-pause-logs');
    if (pauseBtn) pauseBtn.textContent = '⏸ Pause';

    logSSE.addEventListener('log', function (e) {
        let entry;
        try { entry = JSON.parse(e.data); } catch { return; }
        if (logEntries.some(e2 => e2.id === entry.id)) return;
        // ponytail: unshift preserves recency; one-time race after a long
        // disconnect may put an older entry above newer ones for one slot.
        logEntries.unshift(entry);
        if (logEntries.length > 500) logEntries.pop();
        if (logPaused) return;
        prependLogEntry(entry, true);
    });

    logSSE.addEventListener('config', function (e) {
        let cfg;
        try { cfg = JSON.parse(e.data); } catch { return; }
        const indicator = document.getElementById('log-level-indicator');
        if (cfg.level && indicator) {
            indicator.textContent = 'Level: ' + cfg.level.charAt(0).toUpperCase() + cfg.level.slice(1);
            const colors = { debug: '#4a5568', info: '#1a5ab8', warn: '#d49e00', error: '#c53030' };
            indicator.style.background = colors[cfg.level] || colors.info;
            indicator.style.display = '';
        }
    });

    logSSE.addEventListener('error', function () {
        if (!logSSE) return;
        if (logSSE.readyState === EventSource.CLOSED) {
            logSSE.close();
            logSSE = null;
            fetchLogs();
            connectLogStream();
            return;
        }
        if (badge) { badge.textContent = '⟳ Reconnecting'; badge.style.background = '#d49e00'; }
    });
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
    // When the inspector is enabled, every log row is clickable. We do
    // this via a delegated click handler on the container (see
    // setupLogInspect) rather than per-row listeners, so streaming SSE
    // updates do not pile up listeners.
    if (capturePayloadsEnabled && entry.id !== undefined && entry.level !== 'debug') {
        // Only request-level entries (not debug system messages) get the
        // clickable class. Debug entries are router/rule/pipeline trace
        // messages, not full request/response captures — clicking them
        // would just open the modal with "not captured" noise.
        div.classList.add('clickable');
        div.title = 'Click to inspect raw request and response';
    }

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
    if (container.firstChild) container.insertBefore(div, container.firstChild);
    else container.appendChild(div);

    while (container.children.length > 500) container.removeChild(container.lastChild);

    if (isNew) setTimeout(() => div.classList.remove('new'), 600);
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
 * ponytail: ISO timestamps already contain HH:MM:SS at slice 11..19; numeric
 * timestamps still need the padStart dance.
 */
function formatTime(ts) {
    if (!ts) return '—';
    const d = new Date(typeof ts === 'number' ? ts : String(ts).slice(0, 19) + 'Z');
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
