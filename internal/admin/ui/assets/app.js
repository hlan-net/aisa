// aisa admin dashboard client script
document.addEventListener('DOMContentLoaded', () => {
  setupTabs();
  setupActionDelegation();
  setupDocLinks();
  void loadOverview();
  const doc = new URLSearchParams(location.search).get('doc');
  if (doc) {
    showTab('docs');
    void openDoc(doc, location.hash.slice(1), 'none');
  }
  window.addEventListener('popstate', () => {
    const d = new URLSearchParams(location.search).get('doc');
    if (d) {
      showTab('docs');
      void openDoc(d, location.hash.slice(1), 'none');
    } else if (docsIndex && docsIndex.length > 0 && document.getElementById('tab-docs').classList.contains('active')) {
      void openDoc(docsIndex[0].path, '', 'replace');
    }
  });
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
    else if (action === 'edit-budget') openBudget(name);
  });
}

// Navigation tabs
function setupTabs() {
  document.querySelectorAll('.nav-tab').forEach(tab => {
    tab.addEventListener('click', () => {
      showTab(tab.dataset.tab);

      // Refresh data on tab switch
      if (tab.dataset.tab === 'overview') void loadOverview();
      if (tab.dataset.tab === 'consumers') void loadConsumers();
      if (tab.dataset.tab === 'quotas') void loadQuotas();
      if (tab.dataset.tab === 'backends') void loadBackends();
      if (tab.dataset.tab === 'docs') void loadDocs();
    });
  });
}

function showTab(name) {
  document.querySelectorAll('.nav-tab').forEach(t => t.classList.toggle('active', t.dataset.tab === name));
  document.querySelectorAll('.tab-content').forEach(c => c.classList.toggle('active', c.id === 'tab-' + name));
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
    const [consumers, quotas, budgetList] = await Promise.all([
      api('/consumers'),
      api('/quotas').catch(() => []),
      api('/budgets').catch(() => [])
    ]);
    budgetsByConsumer = Object.fromEntries(budgetList.map(b => [b.consumer, b]));

    // Populate quota dropdown in consumer modal
    const quotaSelect = document.getElementById('consumer-quota');
    quotaSelect.innerHTML = '<option value="">-- No Quota Limit --</option>' +
      quotas.map(q => `<option value="${escapeHTML(q.name)}">${escapeHTML(q.name)} (${formatTokens(q.tokens_per_hour)}/h)</option>`).join('');

    if (consumers.length === 0) {
      tbody.innerHTML = '<tr><td colspan="7" class="text-center" style="color:var(--text-muted); padding:24px;">No consumers declared.</td></tr>';
      return;
    }

    tbody.innerHTML = consumers.map(c => `
      <tr>
        <td><strong>${escapeHTML(c.name)}</strong></td>
        <td>${c.quota_profile ? `<span class="badge badge-accent">${escapeHTML(c.quota_profile)}</span>` : '<span style="color:var(--text-muted)">None</span>'}</td>
        <td>${renderBudget(budgetsByConsumer[c.name])}</td>
        <td>${c.namespace ? `${escapeHTML(c.namespace)} / <code>${escapeHTML(c.secret)}</code>` : '<span style="color:var(--text-muted)">-</span>'}</td>
        <td>${c.has_key ? '<span class="badge badge-ready">Key Present</span>' : '<span class="badge badge-warning">Missing Hash</span>'}</td>
        <td>${renderStatusBadge(c.status)}</td>
        <td class="text-right">
          ${c.status !== 'undeclared' ? `<button class="btn btn-outline" style="padding:3px 8px; font-size:11px;" data-action="edit-budget" data-name="${escapeHTML(c.name)}">Budget</button>` : ''}
          ${!c.namespace ? `<button class="btn btn-outline" style="padding:3px 8px; font-size:11px;" data-action="rotate-key" data-name="${escapeHTML(c.name)}">Rotate Key</button>` : ''}
          <button class="btn btn-danger" data-action="delete-consumer" data-name="${escapeHTML(c.name)}">Delete</button>
        </td>
      </tr>
    `).join('');
  } catch (err) {
    tbody.innerHTML = `<tr><td colspan="7" class="text-center" style="color:var(--danger)">Failed to load consumers: ${escapeHTML(err.message)}</td></tr>`;
  }
}

// Budgets
let budgetsByConsumer = {};
let budgetConsumer = '';

// renderBudget shows a consumer's budget and this month's use of it in the consumers table.
function renderBudget(b) {
  if (!b) return '<span style="color:var(--text-muted)">None</span>';
  if (b.status === 'invalid') return `<span class="badge badge-danger" title="${escapeHTML(b.error)}">Invalid</span>`;
  const limit = `${formatTokens(b.monthly_tokens)} / month`;
  if (!b.usage) return `<code>${limit}</code>`;
  const u = b.usage;
  let badge = '';
  if (u.exhausted) badge = ` <span class="badge badge-danger">${b.on_exhausted === 'downgrade' ? 'Downgraded' : 'Exhausted'}</span>`;
  else if (u.soft) badge = ' <span class="badge badge-warning">Soft limit</span>';
  return `<code>${formatTokens(u.used)} / ${formatTokens(b.monthly_tokens)}</code>${badge}`;
}

function openBudget(name) {
  budgetConsumer = name;
  const b = budgetsByConsumer[name];
  document.getElementById('budget-consumer').textContent = name;
  document.getElementById('form-budget').reset();
  const valid = b?.status === 'ready';
  if (valid) {
    document.getElementById('budget-tokens').value = b.monthly_tokens;
    document.getElementById('budget-soft').value = b.soft_ratio;
    document.getElementById('budget-on-exhausted').value = b.on_exhausted || 'reject';
    document.getElementById('budget-downgrade').value = b.downgrade_to || '';
  }
  toggleDowngradeInput();
  document.getElementById('budget-remove').classList.toggle('hidden', !b);
  document.getElementById('budget-usage').innerHTML = renderBudgetUsage(b);
  openModal('modal-budget');
}

function renderBudgetUsage(b) {
  if (!b) return '<p class="section-desc">No budget: the consumer\'s use of priced models is not bounded by a month.</p>';
  if (b.status === 'invalid') return `<div class="key-warning">The stored budget cannot be read, so requests to priced models are refused with 503: ${escapeHTML(b.error)}</div>`;
  if (!b.usage) return '<p class="section-desc">aisa counts no budgets (no Redis), so the month\'s use is not known.</p>';
  const u = b.usage;
  return `
    <div class="budget-stats">
      <div><div class="stat-label">Used in ${escapeHTML(u.period)}</div><div class="budget-value">${formatTokens(u.used)}</div></div>
      <div><div class="stat-label">Remaining</div><div class="budget-value">${formatTokens(u.remaining)}</div></div>
      <div><div class="stat-label">Soft limit</div><div class="budget-value">${formatTokens(b.soft_limit)}</div></div>
    </div>`;
}

function toggleDowngradeInput() {
  const downgrade = document.getElementById('budget-on-exhausted').value === 'downgrade';
  document.getElementById('budget-downgrade-container').classList.toggle('hidden', !downgrade);
  document.getElementById('budget-downgrade').required = downgrade;
}

async function handleSaveBudget(e) {
  e.preventDefault();
  const tokens = Number.parseInt(document.getElementById('budget-tokens').value, 10);
  const soft = document.getElementById('budget-soft').value.trim();
  const onExhausted = document.getElementById('budget-on-exhausted').value;
  const body = { monthly_tokens: tokens, on_exhausted: onExhausted };
  if (soft !== '') body.soft_ratio = Number.parseFloat(soft);
  if (onExhausted === 'downgrade') body.downgrade_to = document.getElementById('budget-downgrade').value.trim();
  try {
    await api(`/budgets/${encodeURIComponent(budgetConsumer)}`, { method: 'PUT', body: JSON.stringify(body) });
    closeModal('modal-budget');
    showToast(`Budget of '${budgetConsumer}' saved`, 'success');
    await loadConsumers();
  } catch (_err) {
    // API error notification is handled in api()
  }
}

async function handleRemoveBudget() {
  if (!confirm(`Remove the budget of '${budgetConsumer}'? Its use of priced models will no longer be bounded by a month.`)) {
    return;
  }
  try {
    await api(`/budgets/${encodeURIComponent(budgetConsumer)}`, { method: 'DELETE' });
    closeModal('modal-budget');
    showToast(`Budget of '${budgetConsumer}' removed`, 'success');
    await loadConsumers();
  } catch (_err) {
    // API error notification is handled in api()
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

// Documentation
let docsIndex = null;
let currentDoc = '';
let docRequest = 0;

// loadDocsIndex fills the document list once; it does not depend on any document loading.
async function loadDocsIndex() {
  if (docsIndex) return docsIndex;
  const nav = document.getElementById('docs-nav');
  try {
    docsIndex = await api('/docs');
  } catch (err) {
    nav.innerHTML = `<div style="color:var(--danger)">Failed to load documentation: ${escapeHTML(err.message)}</div>`;
    return null;
  }
  const sections = {};
  docsIndex.forEach(d => {
    sections[d.section] = sections[d.section] || [];
    sections[d.section].push(d);
  });
  nav.innerHTML = Object.entries(sections).map(([section, docs]) => `
    <div class="docs-section">${escapeHTML(section)}</div>
    ${docs.map(d => `<a href="?doc=${encodeURI(d.path)}" class="docs-link${d.path === currentDoc ? ' active' : ''}" data-doc="${escapeHTML(d.path)}">${escapeHTML(d.title)}</a>`).join('')}
  `).join('') || '<div style="color:var(--text-muted)">No documentation embedded.</div>';
  return docsIndex;
}

// loadDocs runs when the Documentation tab is chosen: without a document in the URL it opens the
// first one and records it there, so its heading links, a reload and a bookmark stay in the tab.
async function loadDocs() {
  const index = await loadDocsIndex();
  if (!currentDoc && index && index.length > 0) {
    await openDoc(index[0].path, '', 'replace');
  }
}

// openDoc shows the document at path. mode is 'push' for a link the reader followed,
// 'replace' to record a document opened on the reader's behalf, and anything else to leave the
// URL as it is (it already names the document).
async function openDoc(path, fragment, mode) {
  const content = document.getElementById('doc-content');
  const request = ++docRequest;
  void loadDocsIndex();
  let page;
  try {
    page = await api('/docs/' + encodeURI(path));
  } catch (err) {
    if (request === docRequest) {
      content.innerHTML = `<div style="color:var(--danger)">Failed to load ${escapeHTML(path)}: ${escapeHTML(err.message)}</div>`;
    }
    return;
  }
  // A document asked for later wins over this one, whichever answer arrives first.
  if (request !== docRequest) return;
  currentDoc = page.path;
  // The HTML is rendered by aisa from the Markdown embedded in its binary, with raw HTML omitted.
  content.innerHTML = page.html;
  content.querySelectorAll('a[href^="http"]').forEach(a => {
    a.target = '_blank';
    a.rel = 'noopener noreferrer';
  });
  const url = '?doc=' + encodeURI(page.path) + (fragment ? '#' + fragment : '');
  if (mode === 'push') history.pushState(null, '', url);
  else if (mode === 'replace') history.replaceState(null, '', url);
  document.querySelectorAll('.docs-link').forEach(l => l.classList.toggle('active', l.dataset.doc === currentDoc));
  const anchor = fragment ? document.getElementById(fragment) : null;
  if (anchor) anchor.scrollIntoView();
  else window.scrollTo(0, 0);
}

// Links between documents (?doc=<path>#<heading>) open in the documentation tab without a reload.
function setupDocLinks() {
  document.addEventListener('click', (e) => {
    const a = e.target.closest('#tab-docs a[href^="?doc="]');
    if (!a || e.ctrlKey || e.metaKey || e.shiftKey) return;
    e.preventDefault();
    const [path, fragment = ''] = a.getAttribute('href').slice('?doc='.length).split('#');
    void openDoc(decodeURI(path), fragment, 'push');
  });
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
