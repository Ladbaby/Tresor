// Tresor Web UI — Admin Dashboard

const API_BASE = '/api';

// ---- Theme detection & toggle (session-only, no persistence) ----
(function initTheme() {
    var prefersDark = window.matchMedia('(prefers-color-scheme: dark)').matches;
    document.documentElement.setAttribute('data-theme', prefersDark ? 'dark' : 'light');

    var toggle = document.getElementById('theme-toggle');
    if (toggle) {
        toggle.addEventListener('click', function () {
            var current = document.documentElement.getAttribute('data-theme');
            var next = current === 'dark' ? 'light' : 'dark';
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
    var tabBtn = document.querySelector('.tab[data-tab="' + tabId + '"]');
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
var authEnabled = false;
var authToken = sessionStorage.getItem('tresor_token') || null;

/**
 * Check whether the server requires authentication, then decide
 * whether to show the login screen or the dashboard.
 */
async function checkAuth() {
    try {
        var status = await fetch(API_BASE + '/auth/status').then(r => r.json());
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

    // Auth is enabled: if we have a stored token, try using it
    if (authToken) {
        // Verify the token still works by hitting a protected endpoint
        try {
            await api('/health');
            showDashboardWithDefaultTab();
            return;
        } catch {
            // Token expired or invalid — fall through to login
            authToken = null;
            sessionStorage.removeItem('tresor_token');
        }
    }

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
        var cfg = await api('/config');
        activateTab(cfg.default_tab || 'downstreams');
    } catch {
        activateTab('downstreams');
    }
}

function logout() {
    authToken = null;
    sessionStorage.removeItem('tresor_token');
    showLogin();
    // Clear the password field
    var pw = document.getElementById('login-password');
    if (pw) pw.value = '';
}

// ---- Login form handler ----
document.getElementById('login-form').addEventListener('submit', async function (e) {
    e.preventDefault();
    var errorEl = document.getElementById('login-error');
    errorEl.classList.add('hidden');
    errorEl.textContent = '';

    var password = document.getElementById('login-password').value;
    try {
        var resp = await fetch(API_BASE + '/auth/login', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ password: password }),
        });
        if (!resp.ok) {
            var errData = await resp.json().catch(() => ({}));
            throw new Error(errData.error || 'Login failed');
        }
        // Store the password as the bearer token for this session
        authToken = password;
        sessionStorage.setItem('tresor_token', authToken);
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
 * Build authorization headers for fetch() calls.
 */
function buildAuthHeaders() {
    var headers = { 'Content-Type': 'application/json' };
    if (authToken) {
        headers['Authorization'] = 'Bearer ' + authToken;
    }
    return headers;
}

async function api(path, options = {}) {
    const url = API_BASE + path;
    const headers = buildAuthHeaders();
    const resp = await fetch(url, {
        headers: { ...headers, ...options.headers },
        ...options,
    });
    // If we get 401 and auth is enabled, clear token and show login
    if (resp.status === 401 && authEnabled) {
        logout();
    }
    if (!resp.ok) {
        const err = await resp.json().catch(() => ({ error: resp.statusText }));
        throw new Error(err.error || resp.statusText);
    }
    return resp.json();
}

// ---- Plugin cache (for visual pipeline editor) ----
let cachedPlugins = null;

async function fetchPlugins() {
    if (cachedPlugins) return cachedPlugins;
    try {
        cachedPlugins = await api('/plugins');
    } catch {
        cachedPlugins = [];
    }
    return cachedPlugins;
}

// ---- Rules ----
async function loadRules() {
    const tbody = document.getElementById('rules-body');
    try {
        const rules = await api('/rules');
        tbody.innerHTML = rules.length === 0
            ? '<tr><td colspan="7" class="loading">No rules configured</td></tr>'
            : rules.map(r => `
                <tr>
                    <td><strong>${esc(r.name)}</strong></td>
                    <td><code>${esc(r.pattern_path)}</code></td>
                    <td><code>${esc(r.pattern_model) || '—'}</code></td>
                    <td><code>${esc(r.active_downstream) || '—'}</code></td>
                    <td><span class="pipeline-steps">${esc(shortPipeline(r.pipeline_config))}</span></td>
                    <td><span class="status-badge ${r.is_enabled ? 'status-enabled' : 'status-disabled'}">${r.is_enabled ? 'ON' : 'OFF'}</span></td>
                    <td>
                        <button class="btn-small" onclick="editRule('${r.id}')">Edit</button>
                        <button class="btn-small" onclick="toggleRule('${r.id}', ${!r.is_enabled})">${r.is_enabled ? 'Disable' : 'Enable'}</button>
                        <button class="btn-danger" onclick="deleteRule('${r.id}')">Delete</button>
                    </td>
                </tr>
            `).join('');
    } catch (err) {
        tbody.innerHTML = `<tr><td colspan="7" class="loading">Error: ${esc(err.message)}</td></tr>`;
    }
}

async function loadDownstreamsForSelect() {
    try {
        const ds = await api('/downstreams');
        const select = document.getElementById('rule-downstream');
        select.innerHTML = '<option value="">— None —</option>' +
            ds.map(d => `<option value="${esc(d.id)}">${esc(d.name)}</option>`).join('');
        return ds;
    } catch {
        return [];
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
async function openRuleModal(rule) {
    document.getElementById('rule-modal-title').textContent = rule ? 'Edit Rule' : 'New Rule';
    document.getElementById('rule-id').value = rule ? rule.id : '';
    document.getElementById('rule-name').value = rule ? rule.name : '';
    document.getElementById('rule-path').value = rule ? rule.pattern_path : '*';
    document.getElementById('rule-model').value = rule ? (rule.pattern_model || '') : '';
    document.getElementById('rule-enabled').checked = rule ? rule.is_enabled : true;

    await loadDownstreamsForSelect().then(ds => {
        document.getElementById('rule-downstream').value = rule ? (rule.active_downstream || '') : '';
    });

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

document.getElementById('btn-new-rule').addEventListener('click', () => {
    fetchPlugins().then(() => openRuleModal(null));
});

document.getElementById('rule-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const id = document.getElementById('rule-id').value;

    // Serialize visual pipeline to JSON for the hidden textarea
    document.getElementById('rule-pipeline').value = serializePipeline();

    const body = {
        name: document.getElementById('rule-name').value,
        pattern_path: document.getElementById('rule-path').value,
        pattern_model: document.getElementById('rule-model').value,
        active_downstream: document.getElementById('rule-downstream').value,
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
async function loadDownstreams() {
    const tbody = document.getElementById('downstreams-body');
    try {
        const ds = await api('/downstreams');
        tbody.innerHTML = ds.length === 0
            ? '<tr><td colspan="6" class="loading">No downstreams configured</td></tr>'
            : ds.map(d => {
                const models = (d.output_model_ids || []).map(m => esc(m));
                const formats = d.api_formats || [];
                const formatBadges = formats.map(f => {
                    const fClass = f === 'openai' ? 'format-openai' : f === 'anthropic' ? 'format-anthropic' : 'format-unknown';
                    return `<span class="format-badge ${fClass}">${esc(f.charAt(0).toUpperCase() + f.slice(1))}</span>`;
                }).join(' ');
                const formatCell = formatBadges || '<span class="format-badge format-unknown">—</span>';
                return `
                <tr>
                    <td><strong>${esc(d.name)}</strong></td>
                    <td>${formatCell}</td>
                    <td><code>${esc(d.base_url)}</code></td>
                    <td><code>${esc(maskKey(d.api_key))}</code></td>
                    <td>
                        <span class="model-badge">${(d.output_model_ids || []).length}</span>
                        ${models.length > 0 ? '<ul class="model-list">' + models.map(m => '<li>' + m + '</li>').join('') + '</ul>' : '<em class="text-muted">none</em>'}
                    </td>
                    <td>
                        <button class="btn-small" onclick="editDownstream('${d.id}')">Edit</button>
                        <button class="btn-danger" onclick="deleteDownstream('${d.id}')">Delete</button>
                    </td>
                </tr>`;
            }).join('');
    } catch (err) {
        tbody.innerHTML = `<tr><td colspan="6" class="loading">Error: ${esc(err.message)}</td></tr>`;
    }
}

function maskKey(key) {
    if (!key || key.length === 0) return '';
    if (key.length <= 4) return '****';
    return key.substring(0, 2) + '*'.repeat(key.length - 4) + key.substring(key.length - 2);
}

function openDownstreamModal(ds) {
    document.getElementById('downstream-modal-title').textContent = ds ? 'Edit Downstream' : 'New Downstream';
    document.getElementById('downstream-id').value = ds ? ds.id : '';
    document.getElementById('downstream-name').value = ds ? ds.name : '';
    document.getElementById('downstream-url').value = ds ? ds.base_url : '';
    var keyInput = document.getElementById('downstream-key');
    keyInput.value = '';

    if (ds && ds.api_key && ds.api_key.length > 0) {
        keyInput.placeholder = '(current key is set — leave blank to keep it, or enter a new key)';
    } else {
        keyInput.placeholder = 'sk-...';
    }

    // Set format checkboxes based on ds.api_formats array
    const formats = ds ? (ds.api_formats || []) : [];
    document.querySelectorAll('#format-checkboxes input[type="checkbox"]').forEach(cb => {
        cb.checked = formats.includes(cb.value);
    });

    // Populate model IDs list
    const modelContainer = document.getElementById('model-ids-container');
    modelContainer.innerHTML = '';
    const modelIds = ds ? (ds.output_model_ids || []) : [];
    modelIds.forEach(m => {
        addModelIdRow(modelContainer, m);
    });

    // Show/hide fetch button (only for existing downstreams)
    document.getElementById('btn-fetch-models').style.display = ds ? '' : 'none';

    document.getElementById('downstream-modal').classList.remove('hidden');
}

function addModelIdRow(container, modelId) {
    const row = document.createElement('div');
    row.className = 'model-id-row';
    const input = document.createElement('input');
    input.type = 'text';
    input.className = 'model-id-input';
    input.value = modelId;
    input.placeholder = 'Model ID';
    row.appendChild(input);
    if (modelId) {
        const removeBtn = document.createElement('button');
        removeBtn.className = 'model-id-remove';
        removeBtn.textContent = '×';
        removeBtn.title = 'Remove model';
        removeBtn.onclick = () => row.remove();
        row.appendChild(removeBtn);
    }
    container.appendChild(row);
}

function addNewModelId() {
    const container = document.getElementById('model-ids-container');
    addModelIdRow(container, '');
    container.querySelector('.model-id-input:last-of-type').focus();
}

async function fetchDownstreamModels(id) {
    const btn = document.getElementById('btn-fetch-models');
    const origText = btn.textContent;
    btn.textContent = 'Fetching...';
    btn.disabled = true;
    try {
        const resp = await api('/downstreams/' + id + '/fetch-models', { method: 'POST' });
        const container = document.getElementById('model-ids-container');
        // Clear and repopulate with fetched models
        container.innerHTML = '';
        (resp.output_model_ids || []).forEach(m => {
            addModelIdRow(container, m);
        });
    } catch (err) {
        var msg = err.message || String(err);
        // Strip the "fetch failed: " prefix from the server response for cleaner display
        if (msg.startsWith('fetch failed: ')) {
            msg = msg.substring('fetch failed: '.length);
        }
        alert('Failed to fetch models:\n\n' + msg);
    }
    btn.textContent = origText;
    btn.disabled = false;
}

function editDownstream(id) {
    api('/downstreams/' + id).then(ds => openDownstreamModal(ds)).catch(err => alert(err.message));
}

async function deleteDownstream(id) {
    if (!confirm('Delete this downstream?')) return;
    try {
        await api('/downstreams/' + id, { method: 'DELETE' });
        loadDownstreams();
    } catch (err) {
        alert(err.message);
    }
}

document.getElementById('btn-new-downstream').addEventListener('click', () => openDownstreamModal(null));

document.getElementById('btn-add-model-id').addEventListener('click', () => addNewModelId());

document.getElementById('btn-fetch-models').addEventListener('click', () => {
    const id = document.getElementById('downstream-id').value;
    if (id) fetchDownstreamModels(id);
});

document.getElementById('downstream-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const id = document.getElementById('downstream-id').value;
    const body = {
        name: document.getElementById('downstream-name').value,
        base_url: document.getElementById('downstream-url').value,
        api_key: document.getElementById('downstream-key').value,
    };

    // Collect checked formats
    const formats = [];
    document.querySelectorAll('#format-checkboxes input[type="checkbox"]:checked').forEach(cb => {
        formats.push(cb.value);
    });
    body.api_formats = formats;

    // Collect model IDs from the editor
    const modelInputs = document.querySelectorAll('#model-ids-container .model-id-input');
    const modelIds = [];
    modelInputs.forEach(input => {
        const v = input.value.trim();
        if (v) modelIds.push(v);
    });
    body.output_model_ids = modelIds;

    try {
        if (id) {
            await api('/downstreams/' + id, {
                method: 'PUT',
                body: JSON.stringify(body),
            });
        } else {
            await api('/downstreams', {
                method: 'POST',
                body: JSON.stringify(body),
            });
        }
        document.getElementById('downstream-modal').classList.add('hidden');
        loadDownstreams();
    } catch (err) {
        alert('Error: ' + err.message);
    }
});

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
        btn.closest('.modal').classList.add('hidden');
    });
});

// Close modal on background click
document.querySelectorAll('.modal').forEach(m => {
    m.addEventListener('click', (e) => {
        if (e.target === m) m.classList.add('hidden');
    });
});

// ---- Utility ----
function esc(s) {
    if (s == null) return '';
    const div = document.createElement('div');
    div.textContent = String(s);
    return div.innerHTML;
}

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

let cachedDownstreams = null;

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

    // Header row
    const header = document.createElement('div');
    header.className = 'alias-group-header';

    const title = document.createElement('div');
    title.className = 'alias-group-title';
    title.innerHTML = `<span class="group-icon">🎯</span> ${esc(group.input_model_id)}`;

    const actions = document.createElement('div');
    actions.className = 'alias-group-actions';

    // Show active label if one exists
    if (group.active_id) {
        const activeOpt = group.options.find(o => o.id === group.active_id);
        if (activeOpt) {
            const badge = document.createElement('span');
            badge.className = 'protocol-badge';
            badge.style.background = '#1b4123';
            badge.style.color = '#3fb950';
            badge.textContent = `Active: ${esc(activeOpt.downstream_name)} / ${esc(activeOpt.output_model_id)}`;
            actions.appendChild(badge);
        } else {
            // Stale active_id — no matching option (e.g. active alias was deleted)
            const warning = document.createElement('span');
            warning.className = 'protocol-badge';
            warning.style.background = '#5d2600';
            warning.style.color = '#d29922';
            warning.textContent = 'No active mapping';
            actions.appendChild(warning);
        }
    }

    // Add option button in header area
    const addBtn = document.createElement('button');
    addBtn.className = 'btn-small';
    addBtn.textContent = '+ Option';
    addBtn.onclick = () => openAliasOptionModal(group.input_model_id);
    actions.appendChild(addBtn);

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

        // Downstream name line
        const dsLabel = document.createElement('div');
        dsLabel.className = 'option-downstream';
        dsLabel.textContent = opt.downstream_name || opt.downstream_id;
        btn.appendChild(dsLabel);

        // Output model line
        const modelLabel = document.createElement('div');
        modelLabel.className = 'option-model';
        modelLabel.textContent = opt.output_model_id;
        btn.appendChild(modelLabel);

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
 * Build a tag-based multi-select UI for output model IDs.
 * Populated from the given downstream's known output_model_ids.
 * If no models are known, shows a hint and reveals the custom text input.
 */
function populateOutputModelSelect(containerEl, downstreamId) {
    containerEl.innerHTML = '';
    const ds = (cachedDownstreams || []).find(d => d.id === downstreamId);
    const models = ds ? (ds.output_model_ids || []) : [];

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

    // Wire up downstream change handler to populate models
    const downstreamSelect = document.getElementById('alias-downstream');
    downstreamSelect.onchange = () => {
        populateOutputModelSelect(outputContainer, downstreamSelect.value);
    };

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

    document.getElementById('new-group-input-model').value = '';
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
        for (let i = 0; i < models.length; i++) {
            await api('/aliases', {
                method: 'POST',
                body: JSON.stringify({
                    input_model_id: inputModelId,
                    downstream_id: downstreamId,
                    output_model_id: models[i],
                    is_active: i === 0,
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
        var defaultTabEl = document.getElementById('default-tab');
        if (defaultTabEl) {
            defaultTabEl.value = cfg.default_tab || 'downstreams';
        }
        // Populate log level selector
        var logLevelEl = document.getElementById('setting-log-level');
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
    var container = document.getElementById('proxy-api-keys-container');
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
    var row = document.createElement('div');
    row.className = 'api-key-row';

    var input = document.createElement('input');
    input.type = 'text';
    input.className = 'api-key-input';
    input.placeholder = 'API key';
    input.value = value;

    var removeBtn = document.createElement('button');
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
    var container = document.getElementById('proxy-api-keys-container');
    // Remove empty-state message if present
    var empty = container.querySelector('.api-key-empty');
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
    var keyInputs = document.querySelectorAll('#proxy-api-keys-container .api-key-input');
    var proxyAPIKeys = [];
    keyInputs.forEach(function (input) {
        var v = input.value.trim();
        if (v) proxyAPIKeys.push(v);
    });

    // Collect admin password
    var newPassword = document.getElementById('admin-password').value;
    var confirmPassword = document.getElementById('admin-password-confirm').value;
    var clearPassword = document.getElementById('clear-password').checked;

    // Validate password inputs
    if (clearPassword) {
        newPassword = '';
    } else if (newPassword && newPassword !== confirmPassword) {
        statusEl.textContent = 'Passwords do not match.';
        statusEl.className = 'settings-status error';
        return;
    }

    try {
        var body = {
            proxy_mode: document.getElementById('proxy-mode').value,
            proxy_api_keys: proxyAPIKeys,
            default_tab: document.getElementById('default-tab').value,
        };
        // Include log level if the selector exists
        var logLevelEl = document.getElementById('setting-log-level');
        if (logLevelEl) {
            body.log_level = logLevelEl.value;
        }
        // Only send admin_password if the user entered something or wants to clear it
        if (newPassword || clearPassword) {
            body.admin_password = newPassword;
        }
        var resp = await api('/config', {
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

var logSSE = null;          // current EventSource connection
var logEntries = [];        // in-memory log entries (mirrors server buffer)
var logActive = false;      // whether the Logs tab is currently visible

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
        var url = API_BASE + '/logs';
        if (authToken) {
            url += '?token=' + encodeURIComponent(authToken);
        }
        var data = await fetch(url).then(function (r) { return r.json(); });
        logEntries = data || [];
        renderLogTable(logEntries);
    } catch (err) {
        // If the endpoint doesn't exist (old server), skip logs
    }
}

/**
 * Connect to the SSE log stream. Called when the Logs tab becomes active.
 */
function connectLogStream() {
    if (logSSE) return; // already connected

    var indicator = document.getElementById('log-level-indicator');
    var badge = document.getElementById('log-status-badge');

    // EventSource doesn't support custom headers, so pass auth as query param
    var url = API_BASE + '/logs/stream';
    if (authToken) {
        url += '?token=' + encodeURIComponent(authToken);
    }

    try {
        logSSE = new EventSource(url);
    } catch {
        if (badge) { badge.textContent = '✗ Offline'; badge.style.background = 'var(--color-danger)'; }
        return;
    }

    if (badge) { badge.textContent = '● Live'; badge.style.background = 'var(--color-success)'; }

    logSSE.addEventListener('log', function (e) {
        var entry;
        try { entry = JSON.parse(e.data); } catch { return; }
        // Append to in-memory array and render as new row
        logEntries.push(entry);
        if (logEntries.length > 500) logEntries.shift();
        appendLogRow(entry, true);
    });

    logSSE.addEventListener('config', function (e) {
        var cfg;
        try { cfg = JSON.parse(e.data); } catch { return; }
        if (cfg.level && indicator) {
            indicator.textContent = 'Level: ' + cfg.level.charAt(0).toUpperCase() + cfg.level.slice(1);
            // Color-code the indicator by level
            var colors = { debug: '#6c757d', info: '#0d6efd', warn: '#ffc107', error: '#dc3545' };
            indicator.style.background = colors[cfg.level] || 'var(--color-primary)';
            indicator.style.display = '';
        }
    });

    logSSE.addEventListener('error', function () {
        if (badge) { badge.textContent = '✗ Offline'; badge.style.background = 'var(--color-danger)'; }
    });

    // When tab becomes inactive, close SSE to save resources
    var section = document.getElementById('tab-logs');
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
    var badge = document.getElementById('log-status-badge');
    if (badge) { badge.textContent = '○ Disconnected'; badge.style.background = '#6c757d'; }
}

/**
 * Render the full log table from an array of entries.
 */
function renderLogTable(entries) {
    var tbody = document.getElementById('logs-table-body');
    if (!tbody) return;
    tbody.innerHTML = '';
    entries.forEach(function (entry) {
        appendLogRow(entry, false);
    });
}

/**
 * Append a single log entry as a table row.
 */
function appendLogRow(entry, isNew) {
    var tbody = document.getElementById('logs-table-body');
    if (!tbody) return;

    var tr = document.createElement('tr');
    tr.className = 'log-entry';
    if (isNew) tr.classList.add('new');
    if (entry.error || (entry.status >= 500)) tr.classList.add('log-error');
    else if (entry.status >= 400) tr.classList.add('log-warn');

    // Time
    var td = document.createElement('td');
    td.textContent = formatTime(entry.timestamp);
    tr.appendChild(td);

    // Method
    td = document.createElement('td');
    td.textContent = entry.method || '';
    tr.appendChild(td);

    // Path
    td = document.createElement('td');
    td.textContent = entry.path || '';
    tr.appendChild(td);

    // Model (show resolved downstream model, fall back to input model)
    td = document.createElement('td');
    td.innerHTML = '<code>' + esc(entry.resolved_model || entry.model || '—') + '</code>';
    tr.appendChild(td);

    // Downstream
    td = document.createElement('td');
    td.textContent = entry.downstream_name || entry.downstream_id || '—';
    tr.appendChild(td);

    // Alias
    td = document.createElement('td');
    if (entry.alias_group) {
        td.innerHTML = '<span class="badge">' + esc(entry.alias_group) + '</span>';
    } else {
        td.textContent = '—';
    }
    tr.appendChild(td);

    // Status
    td = document.createElement('td');
    var statusClass = 'status-ok';
    if (entry.status >= 500) statusClass = 'status-error';
    else if (entry.status >= 400) statusClass = 'status-warn';
    td.innerHTML = '<span class="' + statusClass + '">' + entry.status + '</span>';
    tr.appendChild(td);

    // Duration
    td = document.createElement('td');
    td.textContent = formatDuration(entry.duration);
    tr.appendChild(td);

    // Error
    td = document.createElement('td');
    if (entry.error) {
        td.innerHTML = '<span style="color:var(--color-danger);">' + esc(entry.error) + '</span>';
    } else {
        td.textContent = '—';
    }
    tr.appendChild(td);

    tbody.appendChild(tr);

    // Remove old rows if exceeding 500
    while (tbody.children.length > 500) {
        tbody.removeChild(tbody.firstChild);
    }

    // Remove flash class after animation completes
    if (isNew) {
        setTimeout(function () { tr.classList.remove('new'); }, 600);
    }
}

/**
 * Clear all log entries from the table and memory.
 */
window.clearLogEntries = function () {
    var tbody = document.getElementById('logs-table-body');
    if (tbody) tbody.innerHTML = '';
    logEntries = [];
};

/**
 * Format a timestamp to a readable time string.
 */
function formatTime(ts) {
    if (!ts) return '—';
    var d;
    if (typeof ts === 'number') {
        d = new Date(ts);
    } else {
        d = new Date(ts);
    }
    if (isNaN(d.getTime())) return String(ts);
    return String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0');
}

/**
 * Format a duration in milliseconds to a readable string.
 */
function formatDuration(ms) {
    if (ms == null || ms === 0) return '—';
    if (ms < 1000) return Math.round(ms) + 'ms';
    if (ms < 60000) return (ms / 1000).toFixed(1) + 's';
    var mins = Math.floor(ms / 60000);
    var secs = ((ms % 60000) / 1000).toFixed(1);
    return mins + 'm' + secs + 's';
}

// Intercept tab switching to manage SSE connection lifecycle.
(function () {
    var originalActivateTab = window.activateTab;
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
        var data = await fetch(API_BASE + '/version').then(r => r.json());
        document.getElementById('about-version').textContent = data.version || 'unknown';
        document.getElementById('about-build-time').textContent = data.build_time || '—';
    } catch {
        document.getElementById('about-version').textContent = 'unknown';
        document.getElementById('about-build-time').textContent = '—';
    }
}

// ---- Init ----
checkAuth();
