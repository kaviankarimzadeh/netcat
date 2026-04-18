const $ = (sel) => document.querySelector(sel);
const results = $('#results');
const summary = $('#summary');
const statTargets = $('#statTargets');
const statOk = $('#statOk');
const statFail = $('#statFail');
const statLatency = $('#statLatency');
const runBtn = $('#runBtn');
const clusterChips = $('#clusterChips');
const themeToggle = $('#themeToggle');

// -------- Theme toggle ----------------------------------------------------
function applyTheme(theme) {
  document.documentElement.setAttribute('data-theme', theme);
  try { localStorage.setItem('netcat:theme', theme); } catch (_) {}
  if (themeToggle) {
    themeToggle.setAttribute('aria-pressed', theme === 'light' ? 'true' : 'false');
    themeToggle.title = theme === 'light' ? 'Switch to dark theme' : 'Switch to light theme';
  }
}
if (themeToggle) {
  applyTheme(document.documentElement.getAttribute('data-theme') || 'dark');
  themeToggle.addEventListener('click', () => {
    const next = document.documentElement.getAttribute('data-theme') === 'light' ? 'dark' : 'light';
    applyTheme(next);
  });
}

let currentSource = null;
let latencyAcc = 0;
let latencyCount = 0;

async function loadClusters() {
  try {
    const r = await fetch('/api/clusters');
    const data = await r.json();
    clusterChips.innerHTML = '';
    for (const c of data) {
      const chip = document.createElement('span');
      chip.className = 'chip';
      const mode = c.mode ? ` · ${c.mode}` : '';
      chip.innerHTML = `<span class="pulse"></span>${escapeHtml(c.name)} <span class="chip-meta">· ${c.error ? '!' : c.nodes + ' nodes'}${mode}</span>`;
      if (c.error) chip.title = c.error;
      clusterChips.appendChild(chip);
    }
  } catch (e) {
    console.error(e);
  }
}

function escapeHtml(s) {
  return String(s ?? '').replace(/[&<>"']/g, (c) => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  })[c]);
}

function getClusterCard(name) {
  let card = document.querySelector(`[data-cluster="${CSS.escape(name)}"]`);
  if (card) return card;
  card = document.createElement('div');
  card.className = 'cluster-card';
  card.dataset.cluster = name;
  card.innerHTML = `
    <div class="cluster-header">
      <div class="flex items-center gap-3">
        <div class="h-2.5 w-2.5 rounded-full bg-indigo-400 shadow-[0_0_10px_rgba(129,140,248,0.7)]"></div>
        <div class="font-semibold tracking-tight">${escapeHtml(name)}</div>
      </div>
      <div class="flex items-center gap-2 text-xs text-slate-400">
        <span data-counter="ok"  class="badge badge-ok">0 ok</span>
        <span data-counter="bad" class="badge badge-fail">0 fail</span>
      </div>
    </div>
    <div data-rows></div>`;
  results.appendChild(card);
  return card;
}

function addResult(res) {
  const card = getClusterCard(res.cluster);
  const rows = card.querySelector('[data-rows]');
  const row = document.createElement('div');
  row.className = 'node-row';
  row.dataset.ok = res.ok ? '1' : '0';
  const latency = res.ok ? `${res.latency_ms.toFixed(1)} ms` : '—';
  const detail = res.ok
    ? `<span class="row-resolved">${escapeHtml(res.resolved_ip || '')}</span>`
    : `<span class="row-fail">${escapeHtml(res.error || 'failed')}</span>`;
  row.innerHTML = `
    <div class="dot ${res.ok ? 'ok' : 'bad'}"></div>
    <div>
      <div class="row-node">${escapeHtml(res.node)}</div>
      <div class="row-meta">${escapeHtml(res.pod)} · ${escapeHtml(res.proto)}</div>
    </div>
    <div>${detail}</div>
    <div class="row-latency">${latency}</div>`;
  // Keep failures pinned to the top: bad rows go before the first ok row,
  // ok rows get appended to the end.
  if (res.ok) {
    rows.appendChild(row);
  } else {
    const firstOk = rows.querySelector('[data-ok="1"]');
    rows.insertBefore(row, firstOk);
  }

  const counterSel = res.ok ? '[data-counter="ok"]' : '[data-counter="bad"]';
  const counter = card.querySelector(counterSel);
  const n = parseInt(counter.textContent, 10) + 1;
  counter.textContent = `${n} ${res.ok ? 'ok' : 'fail'}`;

  if (res.ok) {
    statOk.textContent = parseInt(statOk.textContent, 10) + 1;
    latencyAcc += res.latency_ms;
    latencyCount++;
    statLatency.textContent = (latencyAcc / latencyCount).toFixed(1) + ' ms';
  } else {
    statFail.textContent = parseInt(statFail.textContent, 10) + 1;
  }
}

function addClusterError(c) {
  const card = getClusterCard(c.cluster);
  const rows = card.querySelector('[data-rows]');
  const row = document.createElement('div');
  row.className = 'node-row';
  row.dataset.ok = '0';
  row.innerHTML = `
    <div class="dot bad"></div>
    <div class="row-fail">cluster unavailable</div>
    <div class="row-resolved">${escapeHtml(c.error)}</div>
    <div></div>`;
  rows.insertBefore(row, rows.firstChild);
}

function resetResults() {
  results.innerHTML = '';
  statTargets.textContent = '0';
  statOk.textContent = '0';
  statFail.textContent = '0';
  statLatency.textContent = '—';
  latencyAcc = 0;
  latencyCount = 0;
  summary.classList.remove('hidden');
  summary.classList.add('grid');
}

function run(host, port, proto) {
  if (currentSource) currentSource.close();
  resetResults();
  runBtn.disabled = true;
  runBtn.textContent = 'Probing…';

  const qs = new URLSearchParams({ host, port, proto });
  const es = new EventSource('/api/check?' + qs.toString());
  currentSource = es;

  es.addEventListener('start', () => {});
  es.addEventListener('cluster_ready', (e) => {
    const d = JSON.parse(e.data);
    statTargets.textContent = parseInt(statTargets.textContent, 10) + d.nodes;
    getClusterCard(d.cluster);
  });
  es.addEventListener('cluster_error', (e) => addClusterError(JSON.parse(e.data)));
  es.addEventListener('result', (e) => addResult(JSON.parse(e.data)));
  es.addEventListener('done', () => {
    es.close();
    currentSource = null;
    runBtn.disabled = false;
    runBtn.textContent = 'Probe';
  });
  es.onerror = () => {
    es.close();
    currentSource = null;
    runBtn.disabled = false;
    runBtn.textContent = 'Probe';
  };
}

document.getElementById('probeForm').addEventListener('submit', (e) => {
  e.preventDefault();
  const host = $('#host').value.trim();
  const port = $('#port').value;
  const proto = $('#proto').value;
  if (!host || !port) return;
  run(host, port, proto);
});

loadClusters();
