// aisa admin dashboard client script
document.addEventListener('DOMContentLoaded', () => {
  setupTabs();
  setupActionDelegation();
  window.addEventListener('hashchange', routeHash);
  routeHash();
  void loadOverview();
});

function setupActionDelegation() {
  document.addEventListener('click', (e) => {
    const btn = e.target.closest('button[data-action]');
    if (!btn) return;
    const action = btn.dataset.action;
    const name = btn.dataset.name;
    if (action === 'rotate-key') void rotateKey(name);
    else if (action === 'delete-consumer') void deleteConsumer(name);
    else if (action === 'delete-quota') void deleteQuota(name);
  });
}

// Navigation tabs
function setupTabs() {
  document.querySelectorAll('.nav-tab').forEach(tab => {
    tab.addEventListener('click', () => {
      const name = tab.dataset.tab;
      if (name === 'docs') {
        // The document view follows the URL's hash, so a document has a link of its own.
        const hash = '#docs/' + (currentDoc || 'README.md');
        if (location.hash === hash) routeHash();
        else location.hash = hash;
        return;
      }
      if (location.hash) history.replaceState(null, '', location.pathname + location.search);
      showTab(name);

      // Refresh data on tab switch
      if (name === 'overview') void loadOverview();
      if (name === 'consumers') void loadConsumers();
      if (name === 'quotas') void loadQuotas();
      if (name === 'backends') void loadBackends();
    });
  });
}

function showTab(name) {
  document.querySelectorAll('.nav-tab').forEach(t => t.classList.toggle('active', t.dataset.tab === name));
  document.querySelectorAll('.tab-content').forEach(c => c.classList.toggle('active', c.id === 'tab-' + name));
}

// Documentation: #docs/<path>[#<heading>] shows a document of the repository, rendered by aisa.
let docsIndex = null;
let currentDoc = null;

function routeHash() {
  if (!location.hash.startsWith('#docs/')) return false;
  const rest = decodeURIComponent(location.hash.slice('#docs/'.length));
  const i = rest.indexOf('#');
  const path = i < 0 ? rest : rest.slice(0, i);
  const heading = i < 0 ? '' : rest.slice(i + 1);
  showTab('docs');
  void openDoc(path || 'README.md', heading);
  return true;
}

async function loadDocsIndex() {
  if (docsIndex) return docsIndex;
  const nav = document.getElementById('admin-docs-nav');
  try {
    const data = await api('/docs');
    docsIndex = data.docs || [];
  } catch (err) {
    nav.innerHTML = `<div style="color:var(--danger)">Failed to load documents: ${escapeHTML(err.message)}</div>`;
    throw err;
  }
  const sections = new Map();
  docsIndex.forEach(d => {
    const key = d.section || '';
    if (!sections.has(key)) sections.set(key, []);
    sections.get(key).push(d);
  });
  nav.innerHTML = [...sections].map(([section, docs]) => `
    ${section ? `<div class="docs-section">${escapeHTML(section)}</div>` : ''}
    ${docs.map(d => `<a class="docs-link" href="#docs/${escapeHTML(d.path)}" data-path="${escapeHTML(d.path)}">${escapeHTML(d.title)}</a>`).join('')}
  `).join('') || '<div style="color:var(--text-muted)">No documents in this build.</div>';
  return docsIndex;
}

async function openDoc(path, heading) {
  const article = document.getElementById('admin-docs-article');
  try {
    await loadDocsIndex();
    if (path !== currentDoc) {
      article.innerHTML = '<div style="color:var(--text-muted)">Loading...</div>';
      const doc = await api('/docs/' + path.split('/').map(encodeURIComponent).join('/'));
      // The HTML is rendered by aisa from the documents built into it, without their raw HTML.
      article.innerHTML = doc.html;
      currentDoc = path;
      document.querySelectorAll('.docs-link').forEach(a => a.classList.toggle('active', a.dataset.path === path));
    }
    const target = heading ? document.getElementById('doc-' + heading) : null;
    if (target) target.scrollIntoView();
    else window.scrollTo(0, 0);
  } catch (err) {
    article.innerHTML = `<div style="color:var(--danger)">Failed to load ${escapeHTML(path)}: ${escapeHTML(err.message)}</div>`;
  }
}

// API helper
async function api(path, options = {}) {
  try {
    const headers = { 'Content-Type': 'application/json', ...options.headers };
    const res = await fetch('/admin/api' + path, {
      ...options,
      headers
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      throw new Error(data.error || `HTTP ${res.status}`);
    }
    return data;
  } catch (err) {
    showToast(err.message, 'error');
    throw err;
  }
}

// Toast notification
function showToast(msg, type = 'info') {
  const toast = document.getElementById('toast');
  toast.textContent = msg;
  toast.className = `toast toast-${type}`;
  setTimeout(() => {
    toast.className = 'toast hidden';
  }, 4000);
}

// Modal management
function openModal(id) {
  const modal = document.getElementById(id);
  if (modal) modal.classList.remove('hidden');
}

function closeModal(id) {
  const modal = document.getElementById(id);
  if (modal) modal.classList.add('hidden');
}

// Overview
async function loadOverview() {
  try {
    const data = await api('/overview');
    document.getElementById('version-tag').textContent = data.version || 'v0.2.0';
    const statusDot = document.getElementById('system-status-indicator');
    const statusText = document.getElementById('system-status-text');

    if (data.ready) {
      statusDot.className = 'status-dot dot-green';
      statusText.textContent = 'Ready (All systems ok)';
    } else {
      statusDot.className = 'status-dot dot-red';
      statusText.textContent = 'Degraded';
    }

    // Counts
    document.getElementById('stat-consumers').textContent = data.counts.consumers;
    document.getElementById('stat-keys').textContent = data.counts.keys;
    document.getElementById('stat-quotas').textContent = data.counts.quotas;
    document.getElementById('stat-backends').textContent = data.counts.backends;
    document.getElementById('stat-models').textContent = data.counts.models;

    // Checks
    const checksList = document.getElementById('readiness-list');
    checksList.innerHTML = Object.entries(data.checks || {}).map(([name, status]) => `
      <div style="display:flex; justify-content:space-between; margin-bottom:8px;">
        <span><strong>${escapeHTML(name)}</strong></span>
        <span class="badge ${status === 'ok' ? 'badge-ready' : 'badge-danger'}">${escapeHTML(status)}</span>
      </div>
    `).join('') || '<div style="color:var(--text-muted)">No checks reported</div>';
  } catch (err) {
    console.error('Failed to load overview:', err);
    document.getElementById('system-status-indicator').className = 'status-dot dot-red';
    document.getElementById('system-status-text').textContent = 'Disconnected';
  }
}

// Consumers
async function loadConsumers() {
  const tbody = document.getElementById('consumers-table-body');
  try {
    const [consumers, quotas] = await Promise.all([
      api('/consumers'),
      api('/quotas').catch(() => [])
    ]);

    // Populate quota dropdown in consumer modal
    const quotaSelect = document.getElementById('consumer-quota');
    quotaSelect.innerHTML = '<option value="">-- No Quota Limit --</option>' +
      quotas.map(q => `<option value="${escapeHTML(q.name)}">${escapeHTML(q.name)} (${formatTokens(q.tokens_per_hour)}/h)</option>`).join('');

    if (consumers.length === 0) {
      tbody.innerHTML = '<tr><td colspan="6" class="text-center" style="color:var(--text-muted); padding:24px;">No consumers declared.</td></tr>';
      return;
    }

    tbody.innerHTML = consumers.map(c => `
      <tr>
        <td><strong>${escapeHTML(c.name)}</strong></td>
        <td>${c.quota_profile ? `<span class="badge badge-accent">${escapeHTML(c.quota_profile)}</span>` : '<span style="color:var(--text-muted)">None</span>'}</td>
        <td>${c.namespace ? `${escapeHTML(c.namespace)} / <code>${escapeHTML(c.secret)}</code>` : '<span style="color:var(--text-muted)">-</span>'}</td>
        <td>${c.has_key ? '<span class="badge badge-ready">Key Present</span>' : '<span class="badge badge-warning">Missing Hash</span>'}</td>
        <td>${renderStatusBadge(c.status)}</td>
        <td class="text-right">
          ${!c.namespace ? `<button class="btn btn-outline" style="padding:3px 8px; font-size:11px;" data-action="rotate-key" data-name="${escapeHTML(c.name)}">Rotate Key</button>` : ''}
          <button class="btn btn-danger" data-action="delete-consumer" data-name="${escapeHTML(c.name)}">Delete</button>
        </td>
      </tr>
    `).join('');
  } catch (err) {
    tbody.innerHTML = `<tr><td colspan="6" class="text-center" style="color:var(--danger)">Failed to load consumers: ${escapeHTML(err.message)}</td></tr>`;
  }
}

function renderStatusBadge(status) {
  if (status === 'ready') return '<span class="badge badge-ready">Ready</span>';
  if (status === 'missing_key') return '<span class="badge badge-warning">No Key in Vault</span>';
  if (status === 'undeclared') return '<span class="badge badge-danger">Undeclared</span>';
  if (status === 'invalid') return '<span class="badge badge-danger">Invalid JSON</span>';
  return `<span class="badge badge-muted">${escapeHTML(status)}</span>`;
}

function toggleCustomKeyInput() {
  const genKey = document.getElementById('consumer-gen-key').checked;
  const customContainer = document.getElementById('custom-key-container');
  if (genKey) {
    customContainer.classList.add('hidden');
  } else {
    customContainer.classList.remove('hidden');
  }
}

async function handleCreateConsumer(e) {
  e.preventDefault();
  const name = document.getElementById('consumer-name').value.trim();
  const quota = document.getElementById('consumer-quota').value;
  const ns = document.getElementById('consumer-namespace').value.trim();
  const sec = document.getElementById('consumer-secret').value.trim();
  const genKey = document.getElementById('consumer-gen-key').checked;
  const customKey = document.getElementById('consumer-custom-key').value.trim();

  try {
    const res = await api('/consumers', {
      method: 'POST',
      body: JSON.stringify({
        name,
        quota_profile: quota,
        namespace: ns,
        secret: sec,
        generate_key: genKey,
        custom_key: genKey ? '' : customKey
      })
    });

    closeModal('modal-add-consumer');
    document.getElementById('form-add-consumer').reset();
    showToast(`Consumer '${name}' created!`, 'success');
    await loadConsumers();

    if (res.plaintext_key) {
      document.getElementById('revealed-key-text').textContent = res.plaintext_key;
      openModal('modal-key-reveal');
    }
  } catch (_err) {
    // API error notification is handled in api()
  }
}

async function rotateKey(name) {
  if (!confirm(`Generate and rotate a new API key for '${name}'? The existing key will be superseded.`)) {
    return;
  }
  try {
    const res = await api(`/consumers/${encodeURIComponent(name)}/rotate-key`, { method: 'POST' });
    showToast(`Key rotated for '${name}'`, 'success');
    await loadConsumers();
    if (res.plaintext_key) {
      document.getElementById('revealed-key-text').textContent = res.plaintext_key;
      openModal('modal-key-reveal');
    }
  } catch (_err) {
    // API error notification is handled in api()
  }
}

async function deleteConsumer(name) {
  if (!confirm(`Are you sure you want to delete consumer '${name}'? All access for this consumer will cease immediately.`)) {
    return;
  }
  try {
    await api(`/consumers/${encodeURIComponent(name)}`, { method: 'DELETE' });
    showToast(`Consumer '${name}' deleted`, 'success');
    await loadConsumers();
  } catch (_err) {
    // API error notification is handled in api()
  }
}

function copyRevealedKey() {
  const text = document.getElementById('revealed-key-text').textContent;
  void navigator.clipboard.writeText(text).then(() => {
    showToast('API Key copied to clipboard!', 'success');
  }).catch(() => {
    showToast('Failed to copy to clipboard', 'error');
  });
}

// Quotas
async function loadQuotas() {
  const tbody = document.getElementById('quotas-table-body');
  try {
    const quotas = await api('/quotas');
    if (quotas.length === 0) {
      tbody.innerHTML = '<tr><td colspan="3" class="text-center" style="color:var(--text-muted); padding:24px;">No quota profiles found.</td></tr>';
      return;
    }

    tbody.innerHTML = quotas.map(q => {
      const isInvalid = q.status === 'invalid';
      const limitDisplay = isInvalid
        ? '<span class="badge badge-danger">invalid</span>'
        : `<code>${formatTokens(q.tokens_per_hour)} tokens / hour</code>`;
      return `
      <tr>
        <td><strong>${escapeHTML(q.name)}</strong></td>
        <td>${limitDisplay}</td>
        <td class="text-right">
          <button class="btn btn-danger" data-action="delete-quota" data-name="${escapeHTML(q.name)}">Delete</button>
        </td>
      </tr>
    `;
    }).join('');
  } catch (err) {
    tbody.innerHTML = `<tr><td colspan="3" class="text-center" style="color:var(--danger)">Failed to load quotas: ${escapeHTML(err.message)}</td></tr>`;
  }
}

function setQuotaPreset(val) {
  document.getElementById('quota-tokens').value = val;
}

async function handleCreateQuota(e) {
  e.preventDefault();
  const name = document.getElementById('quota-name').value.trim();
  const tokens = Number.parseInt(document.getElementById('quota-tokens').value, 10);
  if (Number.isNaN(tokens) || tokens < 0) {
    showToast('Tokens per hour must be a non-negative number', 'error');
    return;
  }

  try {
    await api('/quotas', {
      method: 'POST',
      body: JSON.stringify({ name, tokens_per_hour: tokens })
    });
    closeModal('modal-add-quota');
    document.getElementById('form-add-quota').reset();
    showToast(`Quota profile '${name}' saved`, 'success');
    await loadQuotas();
  } catch (_err) {
    // API error notification is handled in api()
  }
}

async function deleteQuota(name) {
  if (!confirm(`Delete quota profile '${name}'? Consumers using it will receive 503 until reconfigured.`)) {
    return;
  }
  try {
    await api(`/quotas/${encodeURIComponent(name)}`, { method: 'DELETE' });
    showToast(`Quota profile '${name}' deleted`, 'success');
    await loadQuotas();
  } catch (_err) {
    // API error notification is handled in api()
  }
}

// Backends & Models
async function loadBackends() {
  const tbody = document.getElementById('backends-table-body');
  const modelsContainer = document.getElementById('models-list');
  try {
    const [backends, models] = await Promise.all([
      api('/backends'),
      api('/models')
    ]);

    if (backends.length === 0) {
      tbody.innerHTML = '<tr><td colspan="6" class="text-center" style="color:var(--text-muted); padding:24px;">No backends discovered in Consul.</td></tr>';
    } else {
      tbody.innerHTML = backends.map(b => `
        <tr>
          <td><strong>${escapeHTML(b.id)}</strong></td>
          <td><code>${escapeHTML(b.address)}:${escapeHTML(b.port)}</code></td>
          <td><span class="badge badge-accent">${escapeHTML(b.provider || 'openai-compatible')}</span></td>
          <td>${b.models.map(m => `<span class="badge badge-muted" style="margin-right:4px;">${escapeHTML(m)}</span>`).join('')}</td>
          <td>${escapeHTML(b.priority || '1')} / ${escapeHTML(b.fail_policy || 'closed')}</td>
          <td>${b.status === 'passing' ? '<span class="badge badge-ready">Passing</span>' : `<span class="badge badge-danger">${escapeHTML(b.status)}</span>`}</td>
        </tr>
      `).join('');
    }

    if (models.length === 0) {
      modelsContainer.innerHTML = '<div style="color:var(--text-muted);">No active models found in healthy backends.</div>';
    } else {
      modelsContainer.innerHTML = models.map(m => `
        <div class="model-card">
          <div class="model-title">${escapeHTML(m.name)}</div>
          <div class="model-backends">Provided by: ${m.backends.map(b => `<code>${escapeHTML(b)}</code>`).join(', ')}</div>
        </div>
      `).join('');
    }
  } catch (err) {
    tbody.innerHTML = `<tr><td colspan="6" class="text-center" style="color:var(--danger)">Failed to load backends: ${escapeHTML(err.message)}</td></tr>`;
    modelsContainer.innerHTML = `<div style="color:var(--danger)">Failed to load models: ${escapeHTML(err.message)}</div>`;
  }
}

// Formatters
function formatTokens(n) {
  if (!n) return '0';
  return Number(n).toLocaleString();
}

function escapeHTML(str) {
  if (!str) return '';
  return String(str)
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;')
    .replaceAll("'", '&#039;');
}
