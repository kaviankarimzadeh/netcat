const $ = (sel) => document.querySelector(sel);
const results = $('#results');
const summary = $('#summary');
const statTargets = $('#statTargets');
const statOk = $('#statOk');
const statFail = $('#statFail');
const statLatency = $('#statLatency');
const runBtn = $('#runBtn');
const clusterChips = $('#clusterChips');

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
      chip.innerHTML = `<span class="pulse"></span>${escapeHtml(c.name)} <span style="color:#64748b">· ${c.error ? '!' : c.nodes + ' nodes'}${mode}</span>`;
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
        <span data-counter="ok"  class="badge" style="color:#34d399">0 ok</span>
        <span data-counter="bad" class="badge" style="color:#fb7185">0 fail</span>
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
  const latency = res.ok ? `${res.latency_ms.toFixed(1)} ms` : '—';
  const detail = res.ok
    ? `<span style="color:#94a3b8">${escapeHtml(res.resolved_ip || '')}</span>`
    : `<span style="color:#fb7185">${escapeHtml(res.error || 'failed')}</span>`;
  row.innerHTML = `
    <div class="dot ${res.ok ? 'ok' : 'bad'}"></div>
    <div>
      <div style="color:#e2e8f0">${escapeHtml(res.node)}</div>
      <div style="color:#64748b; font-size:0.72rem;">${escapeHtml(res.pod)} · ${escapeHtml(res.proto)}</div>
    </div>
    <div>${detail}</div>
    <div style="color:#cbd5e1">${latency}</div>`;
  rows.appendChild(row);

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
  row.innerHTML = `
    <div class="dot bad"></div>
    <div style="color:#fb7185">cluster unavailable</div>
    <div style="color:#94a3b8">${escapeHtml(c.error)}</div>
    <div></div>`;
  rows.appendChild(row);
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
