'use strict';
// Nube dashboard — plain JS, no build step, no external requests.

const S = {
  token: lsGet('nube.token') || '',
  data: null,
  view: (location.hash || '#overview').slice(1),
  ns: lsGet('nube.ns') || 'all',
  online: true,
  logAbort: null,
  eventFilter: '',
  eventType: 'all',
};
const $ = (sel, root = document) => root.querySelector(sel);
const VIEWS = {
  overview: 'Overview', nodes: 'Nodes', deployments: 'Deployments', statefulsets: 'StatefulSets', jobs: 'Jobs & CronJobs',
  pods: 'Pods', services: 'Services', ingresses: 'Ingress', volumes: 'Volumes', config: 'Config & Secrets',
  events: 'Events', tokens: 'Access tokens', cluster: 'Control Plane', apply: 'Apply YAML',
};
// Views with form inputs are rendered once, not on every poll.
const STATIC_VIEWS = new Set(['apply', 'tokens']);

function lsGet(k) { try { return localStorage.getItem(k); } catch { return null; } }
function lsSet(k, v) { try { v == null ? localStorage.removeItem(k) : localStorage.setItem(k, v); } catch {} }

// ------------------------------------------------------------ helpers

function esc(s) {
  return String(s ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}
function ago(t) {
  if (!t || t.startsWith('0001')) return '—';
  const s = Math.max(0, (Date.now() - new Date(t)) / 1000);
  if (s < 60) return Math.floor(s) + 's';
  if (s < 3600) return Math.floor(s / 60) + 'm';
  if (s < 172800) return Math.floor(s / 3600) + 'h' + (Math.floor(s / 60) % 60 ? Math.floor(s / 60) % 60 + 'm' : '');
  return Math.floor(s / 86400) + 'd';
}
function pct(a, b) { return b > 0 ? Math.round((a / b) * 100) : 0; }
function fmtCPU(m) { return m >= 1000 ? (m / 1000).toFixed(m % 1000 ? 1 : 0) + ' cores' : m + 'm'; }
function fmtMem(mi) { return mi >= 1024 ? (mi / 1024).toFixed(1) + ' GiB' : mi + ' MiB'; }
function inNS(o) { return S.ns === 'all' || o.metadata.namespace === S.ns; }
function labels(m) { return Object.entries(m || {}).map(([k, v]) => `<span class="tag">${esc(k)}=${esc(v)}</span>`).join(''); }

function chip(text, kind, pulse) {
  return `<span class="chip ${kind || ''} ${pulse ? 'pulse' : ''}">${esc(text)}</span>`;
}
// mainImage describes a pod template's image(s).
function mainImage(t) {
  const cs = t.containers && t.containers.length ? t.containers : [t];
  return (cs[0].image || '') + (cs.length > 1 ? ` (+${cs.length - 1})` : '');
}
function containerDots(p) {
  const cs = (p.status.containers || []).filter(c => !c.init);
  if (cs.length < 2) return '';
  return `<span class="replicas" title="${esc(cs.map(c => c.name + ': ' + c.state + (c.ready ? ', ready' : '')).join(', '))}">` +
    cs.map(c => `<i class="${c.ready ? 'r' : c.state === 'Running' ? 'p' : ''}"></i>`).join('') + '</span>';
}
function podChip(p) {
  const ph = p.status.phase;
  if (ph === 'Init') {
    const inits = (p.status.containers || []).filter(c => c.init);
    const done = inits.filter(c => c.state === 'Terminated' && c.exitCode === 0).length;
    return chip(`Init ${done}/${(p.spec.template.initContainers || []).length}`, 'info', true);
  }
  if (ph === 'Running') return p.status.ready ? chip('Running', 'ok') : chip('Unready', 'warn');
  if (ph === 'Succeeded') return chip('Completed', 'ok');
  if (ph === 'CrashLoop' || ph === 'Failed') return chip(ph === 'CrashLoop' ? 'CrashLoopBackOff' : 'Failed', 'bad');
  if (ph === 'Terminating') return chip('Terminating', '', true);
  if (ph === 'Pending' && p.status.message) return chip('Pending', 'warn');
  return chip(ph, 'info', true);
}
function deployChip(d) {
  const ph = d.status.phase || 'Progressing';
  return chip(ph, { Healthy: 'ok', Degraded: 'bad', Paused: 'warn' }[ph] || 'info', ph === 'Progressing');
}
function nodeChip(n) {
  if (n.status !== 'Ready') return chip('NotReady', 'bad');
  return n.cordoned ? chip('Cordoned', 'warn') : chip('Ready', 'ok');
}
function meter(label, used, req, total, fmt) {
  const u = pct(used, total), r = pct(req, total);
  const cls = u >= 90 ? 'crit' : u >= 75 ? 'hot' : '';
  return `<div class="meter">
    <div class="meter-top"><span>${label}</span><span><b>${fmt(used)}</b> used · ${fmt(req)} requested · ${fmt(total)}</span></div>
    <div class="bar ${cls}"><span class="req" style="width:${Math.min(r, 100)}%"></span><span class="use" style="width:${Math.min(u, 100)}%"></span></div>
  </div>`;
}
function miniBar(used, total) {
  const u = pct(used, total);
  return `<span class="bar mini-bar ${u >= 90 ? 'crit' : u >= 75 ? 'hot' : ''}"><span class="use" style="width:${Math.min(u, 100)}%"></span></span>${u}%`;
}
function toast(msg, err) {
  const el = document.createElement('div');
  el.className = 'toast' + (err ? ' err' : '');
  el.textContent = msg;
  $('#toasts').appendChild(el);
  setTimeout(() => el.remove(), err ? 6000 : 3500);
}
function card(title, body, right = '') {
  return `<div class="card"><div class="card-head"><h3>${title}</h3><div class="right">${right}</div></div>${body}</div>`;
}
function table(head, rows, empty) {
  if (!rows.length) return `<div class="empty">${empty}</div>`;
  return `<div class="table-wrap"><table><thead><tr>${head.map(h => `<th>${h}</th>`).join('')}</tr></thead><tbody>${rows.join('')}</tbody></table></div>`;
}

// ------------------------------------------------------------ API

async function api(path, opts = {}) {
  const headers = { Authorization: 'Bearer ' + S.token, ...(opts.headers || {}) };
  const res = await fetch(path, { ...opts, headers });
  if (res.status === 401) { logout('Token rejected by the server.'); throw new Error('unauthorized'); }
  if (opts.raw) return res;
  const text = await res.text();
  let body; try { body = JSON.parse(text); } catch { body = text; }
  if (!res.ok) throw new Error((body && body.error) || res.statusText);
  return body;
}
async function act(promise, okMsg) {
  try {
    const r = await promise;
    toast(okMsg || (r && r.status) || 'Done');
    refresh();
    return r;
  } catch (e) { toast(e.message, true); }
}
const post = (path, body) => api(path, { method: 'POST', body: JSON.stringify(body || {}), headers: { 'Content-Type': 'application/json' } });
const del = path => api(path, { method: 'DELETE' });
const objPath = (kind, ns, name) => `/api/v1/namespaces/${encodeURIComponent(ns)}/${kind}/${encodeURIComponent(name)}`;

// ------------------------------------------------------------ boot, auth, polling

function logout(msg) {
  S.token = ''; lsSet('nube.token', null);
  $('#app').classList.add('hidden'); $('#login').classList.remove('hidden');
  $('#login-error').textContent = msg || '';
}

$('#login-form').addEventListener('submit', async e => {
  e.preventDefault();
  S.token = $('#token-input').value.trim();
  try {
    await api('/api/v1/version');
    lsSet('nube.token', S.token);
    start();
  } catch (err) { if (err.message !== 'unauthorized') $('#login-error').textContent = err.message; }
});
$('#logout-btn').onclick = () => logout();
$('#theme-btn').onclick = () => {
  const t = document.documentElement.dataset.theme === 'light' ? 'dark' : 'light';
  document.documentElement.dataset.theme = t; lsSet('nube.theme', t);
};
$('#menu-btn').onclick = () => $('.sidebar').classList.toggle('open');
$('#ns').onchange = e => { S.ns = e.target.value; lsSet('nube.ns', S.ns); render(true); };
$('#nav').addEventListener('click', e => {
  const a = e.target.closest('a[data-view]');
  if (a) { location.hash = a.dataset.view; $('.sidebar').classList.remove('open'); }
});
window.addEventListener('hashchange', () => { S.view = location.hash.slice(1) || 'overview'; render(true); });
$('#modal-close').onclick = closeModal;
$('#modal').addEventListener('click', e => { if (e.target.id === 'modal') closeModal(); });
document.addEventListener('keydown', e => { if (e.key === 'Escape') closeModal(); });

if (lsGet('nube.theme')) document.documentElement.dataset.theme = lsGet('nube.theme');

let pollTimer;
function start() {
  $('#login').classList.add('hidden'); $('#app').classList.remove('hidden');
  refresh();
  clearInterval(pollTimer);
  pollTimer = setInterval(refresh, 2000);
}
async function refresh() {
  try {
    S.data = await api('/api/v1/overview');
    S.online = true;
    render(false);
  } catch (e) {
    S.online = false;
    $('#live').classList.add('off');
  }
}

function render(force) {
  if (!VIEWS[S.view]) S.view = 'overview';
  const d = S.data;
  document.querySelectorAll('#nav a').forEach(a => a.classList.toggle('active', a.dataset.view === S.view));
  $('#title').textContent = VIEWS[S.view];
  if (!d) return;
  $('#live').classList.toggle('off', !S.online);
  $('#version').textContent = 'Nube ' + d.version;
  const c = d.cluster;
  $('#leader-pill').textContent = (c.mode === 'ha' ? `HA · leader ${c.leader || '—'} · ${c.members?.length || 0} servers` : 'single server') +
    ` · ${d.me.name} (${d.me.role})`;
  document.body.dataset.role = d.me.role;
  const sel = $('#ns'), want = ['all', ...d.namespaces];
  if (sel.options.length !== want.length || [...sel.options].some((o, i) => o.value !== want[i])) {
    sel.innerHTML = want.map(n => `<option value="${esc(n)}">${n === 'all' ? 'All namespaces' : esc(n)}</option>`).join('');
  }
  sel.value = want.includes(S.ns) ? S.ns : 'all';
  if (!force && STATIC_VIEWS.has(S.view) && $('#view').dataset.view === S.view) return;
  // Keep scroll position and focused filter field across live re-renders.
  const focused = document.activeElement && document.activeElement.id;
  const caret = focused && document.activeElement.selectionStart;
  $('#view').innerHTML = renderers[S.view](d);
  $('#view').dataset.view = S.view;
  if (focused && $('#' + focused)) { const el = $('#' + focused); el.focus(); try { el.setSelectionRange(caret, caret); } catch {} }
  if (renderers[S.view + 'Mounted']) renderers[S.view + 'Mounted']();
}

// ------------------------------------------------------------ views

const renderers = {
  overview(d) {
    const nodes = d.nodes, ready = nodes.filter(n => n.status === 'Ready');
    const deps = d.deployments.filter(inNS), healthy = deps.filter(x => x.status.phase === 'Healthy');
    const pods = d.pods.filter(inNS), running = pods.filter(p => p.status.phase === 'Running' && p.status.ready);
    const failing = pods.filter(p => ['CrashLoop', 'Failed'].includes(p.status.phase));
    const svcs = d.services.filter(inNS);
    const cap = nodes.reduce((a, n) => ({ cpu: a.cpu + n.capacity.cpu, mem: a.mem + n.capacity.memory }), { cpu: 0, mem: 0 });
    const req = nodes.reduce((a, n) => ({ cpu: a.cpu + n.allocated.cpu, mem: a.mem + n.allocated.memory }), { cpu: 0, mem: 0 });
    const use = ready.reduce((a, n) => ({ cpu: a.cpu + n.usage.cpu, mem: a.mem + n.usage.memory }), { cpu: 0, mem: 0 });
    const kpi = (label, value, total, sub, link) =>
      `<a class="card kpi" href="#${link}"><span class="label">${label}</span><span class="value">${value}<small> / ${total}</small></span><span class="sub">${sub}</span></a>`;
    const warn = d.events.filter(e => e.type === 'Warning').slice(-6).reverse();
    return `
      <div class="grid kpis">
        ${kpi('Nodes ready', ready.length, nodes.length, nodes.length ? `${nodes.filter(n => n.cordoned).length} cordoned` : 'no nodes joined yet', 'nodes')}
        ${kpi('Deployments healthy', healthy.length, deps.length, `${deps.filter(x => x.status.phase === 'Degraded').length} degraded`, 'deployments')}
        ${kpi('Pods running', running.length, pods.length, failing.length ? `<span style="color:var(--bad)">${failing.length} failing</span>` : 'all good', 'pods')}
        ${kpi('Services', svcs.filter(s => s.status.endpoints?.length).length, svcs.length, 'with ready endpoints', 'services')}
      </div>
      <div class="grid two">
        ${card('Cluster capacity', `<div class="card-body grid">
          ${meter('CPU', use.cpu, req.cpu, cap.cpu, fmtCPU)}
          ${meter('Memory', use.mem, req.mem, cap.mem, fmtMem)}
          <div class="muted small">Dark segment = live usage, light segment = requested by pods.</div>
        </div>`)}
        ${card('Recent warnings', warn.length ? `<div class="event-list">${warn.map(eventRow).join('')}</div>` : '<div class="empty">No warnings. 🎉</div>', '<a href="#events" class="small">All events →</a>')}
      </div>
      ${card('Deployments', table(['Name', 'Replicas', 'Status', 'Image', 'Updated'], deps.map(x => `
        <tr><td class="name"><a data-describe="deployments|${esc(x.metadata.namespace)}|${esc(x.metadata.name)}">${esc(x.metadata.name)}</a><div class="sub">${esc(x.metadata.namespace)}</div></td>
        <td>${x.status.readyReplicas}/${x.spec.replicas} ${replicaBlocks(x, d)}</td><td>${deployChip(x)}</td>
        <td class="mono trunc">${esc(mainImage(x.spec.template))}</td><td>${ago(x.metadata.updatedAt)}</td></tr>`), 'No deployments yet — use <a href="#apply">Apply YAML</a> to create one.'))}
      ${card('Nodes', `<div class="card-body nodes-grid">${nodes.map(nodeCard).join('') || '<div class="empty">No nodes. Start an agent: <code>nube agent --server URL --token TOKEN</code></div>'}</div>`)}
    `;
  },

  nodes(d) {
    return card('Nodes', `<div class="card-body nodes-grid">${d.nodes.map(n => nodeCard(n, true)).join('') ||
      '<div class="empty">No nodes have joined. On each machine run <code>nube agent --server http://SERVER:7070 --token TOKEN</code>.</div>'}</div>`);
  },

  deployments(d) {
    const deps = d.deployments.filter(inNS);
    return card(`Deployments <span class="muted">(${deps.length})</span>`, table(
      ['Name', 'Replicas', 'Status', 'Image', 'Rev', 'Scale', ''],
      deps.map(x => {
        const ns = esc(x.metadata.namespace), n = esc(x.metadata.name), id = `${ns}|${n}`;
        const auto = x.spec.autoscale;
        return `<tr>
          <td class="name"><a data-describe="deployments|${id}">${n}</a><div class="sub">${ns}${auto ? ` · autoscale ${auto.min}–${auto.max} @ ${auto.targetCPUPercent}% (now ${x.status.cpuPercent}%)` : ''}</div></td>
          <td>${x.status.readyReplicas}/${x.spec.replicas} ${replicaBlocks(x, d)}</td>
          <td>${deployChip(x)}${x.status.message ? `<div class="sub trunc" title="${esc(x.status.message)}">${esc(x.status.message)}</div>` : ''}</td>
          <td class="mono trunc" title="${esc(mainImage(x.spec.template))}">${esc(mainImage(x.spec.template))}</td>
          <td>${x.status.revision}</td>
          <td>${auto ? '<span class="muted small">auto</span>' : `<span class="scale"><button data-scale="${id}|-1" title="Scale down">−</button><span>${x.spec.replicas}</span><button data-scale="${id}|1" title="Scale up">+</button></span>`}</td>
          <td class="actions btn-row" style="justify-content:flex-end">
            <button class="btn small" data-restart="${id}">Restart</button>
            <button class="btn small" data-history="${id}">History</button>
            <button class="btn small danger" data-delete="deployments|${id}">Delete</button>
          </td></tr>`;
      }), 'No deployments in this namespace.'));
  },

  pods(d) {
    const pods = d.pods.filter(inNS);
    return card(`Pods <span class="muted">(${pods.length})</span>`, table(
      ['Name', 'Status', 'Restarts', 'Node', 'Pod IP', 'CPU / Mem', 'Age', ''],
      pods.map(p => {
        const id = `${esc(p.metadata.namespace)}|${esc(p.metadata.name)}`;
        return `<tr>
          <td class="name"><a data-describe="pods|${id}">${esc(p.metadata.name)}</a><div class="sub">${esc(p.metadata.namespace)}${p.spec.owner ? ' · ' + esc((p.spec.ownerKind || 'Deployment').toLowerCase()) + '/' + esc(p.spec.owner) : ''}</div></td>
          <td>${podChip(p)} ${containerDots(p)}${p.status.message ? `<div class="sub trunc" title="${esc(p.status.message)}">${esc(p.status.message)}</div>` : ''}</td>
          <td>${p.status.restarts > 0 ? `<b style="color:var(--warn)">${p.status.restarts}</b>` : 0}</td>
          <td>${esc(p.spec.nodeName || '—')}</td>
          <td class="mono">${esc(p.status.podIP || '—')}</td>
          <td class="mono">${p.status.cpu}m · ${p.status.memory}Mi</td>
          <td>${ago(p.metadata.createdAt)}</td>
          <td class="actions btn-row" style="justify-content:flex-end">
            <button class="btn small" data-logs="${id}" ${p.status.containerID ? '' : 'disabled'}>Logs</button>
            <button class="btn small danger" data-delete="pods|${id}">Delete</button>
          </td></tr>`;
      }), 'No pods in this namespace.'));
  },

  services(d) {
    const svcs = d.services.filter(inNS);
    return card(`Services <span class="muted">(${svcs.length})</span>`, table(
      ['Name', 'External port', 'In-cluster address', 'Endpoints', 'Load balancer', ''],
      svcs.map(s => {
        const id = `${esc(s.metadata.namespace)}|${esc(s.metadata.name)}`;
        const eps = s.status.endpoints || [];
        return `<tr>
          <td class="name"><a data-describe="services|${id}">${esc(s.metadata.name)}</a><div class="sub">${labels(s.spec.selector)}</div></td>
          <td class="mono">${s.spec.internal || s.spec.headless ? `<span class="tag">${s.spec.headless ? 'headless' : 'internal'}</span>` :
            s.spec.ports.map(p => `:${p.port} → ${p.targetPort}${p.protocol === 'UDP' ? ' <span class="tag">udp</span>' : ''}`).join('<br>')}</td>
          <td class="mono small">${esc(s.metadata.name)}.${esc(s.metadata.namespace)}.svc.nube:${s.spec.ports[0].port}</td>
          <td>${eps.length ? chip(eps.length + ' ready', 'ok') : chip('none', 'warn')}<div class="sub mono">${eps.slice(0, 3).map(esc).join('<br>')}${eps.length > 3 ? '<br>…' : ''}</div></td>
          <td>${s.spec.internal || s.spec.headless ? '<span class="muted small">cluster only</span>' : s.status.listening ? chip('listening', 'ok') : chip('not listening', 'bad')}<div class="sub">${s.spec.internal || s.spec.headless ? '' : s.status.connections + ' connections'}${s.status.message ? ' · ' + esc(s.status.message) : ''}</div></td>
          <td class="actions"><button class="btn small danger" data-delete="services|${id}">Delete</button></td></tr>`;
      }), 'No services. A service exposes ready pods on a port of every control-plane server.'));
  },


  statefulsets(d) {
    const list = d.statefulSets.filter(inNS);
    return card(`StatefulSets <span class="muted">(${list.length})</span>`, table(
      ['Name', 'Replicas', 'Status', 'Image', 'Stable names', 'Scale', ''],
      list.map(x => {
        const ns = esc(x.metadata.namespace), n = esc(x.metadata.name), id = `${ns}|${n}`;
        const svc = x.spec.serviceName;
        const names = Array.from({ length: Math.min(x.spec.replicas, 3) }, (_, i) => `${n}-${i}${svc ? '.' + esc(svc) : ''}`).join('<br>');
        return `<tr>
          <td class="name"><a data-describe="statefulsets|${id}">${n}</a><div class="sub">${ns} · ${esc(x.spec.podManagementPolicy)}</div></td>
          <td>${x.status.readyReplicas}/${x.spec.replicas}</td>
          <td>${deployChip(x)}${x.status.message ? `<div class="sub trunc" title="${esc(x.status.message)}">${esc(x.status.message)}</div>` : ''}</td>
          <td class="mono trunc">${esc(mainImage(x.spec.template))}</td>
          <td class="mono small">${names}${x.spec.replicas > 3 ? '<br>…' : ''}</td>
          <td><span class="scale"><button data-sts-scale="${id}|-1" title="Scale down">−</button><span>${x.spec.replicas}</span><button data-sts-scale="${id}|1" title="Scale up">+</button></span></td>
          <td class="actions btn-row" style="justify-content:flex-end">
            <button class="btn small" data-sts-restart="${id}">Restart</button>
            <button class="btn small danger" data-delete="statefulsets|${id}">Delete</button>
          </td></tr>`;
      }), 'No statefulsets. Use them for databases: stable names (db-0, db-1) and a volume per replica.'));
  },

  jobs(d) {
    const crons = d.cronJobs.filter(inNS), jobs = d.jobs.filter(inNS).slice().reverse();
    const jobChip = j => chip(j.status.phase, { Complete: 'ok', Failed: 'bad' }[j.status.phase] || 'info', j.status.phase === 'Running');
    const set = t => t && !t.startsWith('0001');
    return `
      ${card(`CronJobs <span class="muted">(${crons.length})</span>`, table(['Name', 'Schedule', 'Last run', 'Next run', 'Active', ''],
        crons.map(c => {
          const id = `${esc(c.metadata.namespace)}|${esc(c.metadata.name)}`;
          return `<tr><td class="name"><a data-describe="cronjobs|${id}">${esc(c.metadata.name)}</a><div class="sub">${esc(c.metadata.namespace)} · ${esc(c.spec.concurrencyPolicy)}</div></td>
            <td class="mono" style="white-space:nowrap">${esc(c.spec.schedule)}${c.spec.suspend ? ' ' + chip('suspended', 'warn') : ''}</td>
            <td>${set(c.status.lastScheduleTime) ? ago(c.status.lastScheduleTime) + ' ago' : '—'}</td>
            <td>${c.spec.suspend || !set(c.status.nextScheduleTime) ? '—' : new Date(c.status.nextScheduleTime).toLocaleString()}</td>
            <td>${(c.status.active || []).length}</td>
            <td class="actions btn-row" style="justify-content:flex-end">
              <button class="btn small" data-cron="${id}|trigger">Run now</button>
              <button class="btn small" data-cron="${id}|${c.spec.suspend ? 'resume' : 'suspend'}">${c.spec.suspend ? 'Resume' : 'Suspend'}</button>
              <button class="btn small danger" data-delete="cronjobs|${id}">Delete</button>
            </td></tr>`;
        }), 'No cronjobs.'))}
      ${card(`Jobs <span class="muted">(${jobs.length})</span>`, table(['Name', 'Status', 'Completions', 'Failed', 'Duration', 'Age', ''],
        jobs.map(j => {
          const id = `${esc(j.metadata.namespace)}|${esc(j.metadata.name)}`;
          const start = set(j.status.startTime) ? new Date(j.status.startTime) : null;
          const end = set(j.status.completionTime) ? new Date(j.status.completionTime) : new Date();
          const dur = start ? Math.round((end - start) / 1000) + 's' : '—';
          const from = j.metadata.labels && j.metadata.labels['nube/cronjob'];
          return `<tr><td class="name"><a data-describe="jobs|${id}">${esc(j.metadata.name)}</a><div class="sub">${esc(j.metadata.namespace)}${from ? ' · from cronjob/' + esc(from) : ''}</div></td>
            <td>${jobChip(j)}${j.status.message ? `<div class="sub trunc" title="${esc(j.status.message)}">${esc(j.status.message)}</div>` : ''}</td>
            <td>${j.status.succeeded}/${j.spec.completions}</td><td>${j.status.failed || 0}</td><td>${dur}</td><td>${ago(j.metadata.createdAt)}</td>
            <td class="actions"><button class="btn small danger" data-delete="jobs|${id}">Delete</button></td></tr>`;
        }), 'No jobs. A Job runs pods to completion; a CronJob creates Jobs on a schedule.'))}`;
  },

  ingresses(d) {
    const list = d.ingresses.filter(inNS);
    return card(`Ingress <span class="muted">(${list.length})</span>`, table(['Name', 'Routes', 'TLS', 'Requests', 'Status', ''],
      list.map(ing => {
        const id = `${esc(ing.metadata.namespace)}|${esc(ing.metadata.name)}`;
        const routes = ing.spec.rules.flatMap(r => r.paths.map(p =>
          `<div class="mono small">${esc(r.host || '*')}${esc(p.path)}${p.pathType === 'Exact' ? ' (exact)' : ''} → ${esc(p.backend.service)}:${p.backend.port}</div>`)).join('');
        const tls = (ing.spec.tls || []).flatMap(t => t.hosts.map(h => `<span class="tag">${esc(h)}${t.secretName ? '' : ' (self-signed)'}</span>`)).join('') || '—';
        return `<tr><td class="name"><a data-describe="ingresses|${id}">${esc(ing.metadata.name)}</a><div class="sub">${esc(ing.metadata.namespace)}</div></td>
          <td>${routes}</td><td>${tls}</td><td>${ing.status.requests || 0}</td>
          <td>${ing.status.message ? chip('check', 'warn') + `<div class="sub">${esc(ing.status.message)}</div>` : chip('ok', 'ok')}</td>
          <td class="actions"><button class="btn small danger" data-delete="ingresses|${id}">Delete</button></td></tr>`;
      }), 'No ingress. Ingress routes HTTP(S) by host name and path to services, on ports 80/443 of the servers.'));
  },

  volumes(d) {
    const vols = d.volumes.filter(inNS);
    return card(`Volumes <span class="muted">(${vols.length})</span>`, table(
      ['Name', 'Type', 'Size', 'Usage', 'Status', 'Node', 'Used by', 'Source', ''],
      vols.map(v => {
        const id = `${esc(v.metadata.namespace)}|${esc(v.metadata.name)}`;
        const src = v.spec.type === 'nfs' ? `${v.spec.nfs.server}:${v.spec.nfs.path}` : v.spec.type === 'hostPath' ? v.spec.hostPath :
          (v.status.enforced ? 'ext4 image nube_' : 'docker volume nube_') + v.metadata.namespace + '_' + v.metadata.name;
        const usage = v.status.capacityMiB > 0 ? `${miniBar(v.status.usedMiB, v.status.capacityMiB)}<div class="sub">${fmtMem(v.status.usedMiB)} of ${fmtMem(v.status.capacityMiB)}</div>`
          : `<span class="muted small">${v.status.message ? esc(v.status.message) : '—'}</span>`;
        return `<tr>
          <td class="name"><a data-describe="volumes|${id}">${esc(v.metadata.name)}</a><div class="sub">${esc(v.metadata.namespace)}</div></td>
          <td><span class="tag">${esc(v.spec.type)}</span></td>
          <td>${esc(v.spec.size || '—')}${v.status.enforced ? ' <span class="tag" title="hard limit enforced by the filesystem">enforced</span>' : ''}</td>
          <td>${usage}</td>
          <td>${v.status.phase === 'Bound' ? chip('Bound', 'ok') : chip(v.status.phase || 'Available', 'info')}</td>
          <td>${esc(v.status.nodeName || (v.spec.type === 'local' ? 'unbound' : 'any'))}</td>
          <td>${(v.status.usedBy || []).map(esc).join('<br>') || '—'}</td>
          <td class="mono small">${esc(src)}</td>
          <td class="actions"><button class="btn small danger" data-delete="volumes|${id}">Delete</button></td></tr>`;
      }), 'No volumes. Create one with <code>kind: Volume</code> (local, nfs or hostPath).'));
  },

  config(d) {
    const cms = d.configMaps.filter(inNS), secs = d.secrets.filter(inNS);
    const rows = (list, kind) => list.map(c => {
      const id = `${esc(c.metadata.namespace)}|${esc(c.metadata.name)}`;
      return `<tr><td class="name"><a data-describe="${kind}|${id}">${esc(c.metadata.name)}</a><div class="sub">${esc(c.metadata.namespace)}</div></td>
        <td>${Object.keys(c.data || {}).map(k => `<span class="tag">${esc(k)}</span>`).join('')}</td><td>${ago(c.metadata.updatedAt)}</td>
        <td class="actions"><button class="btn small danger" data-delete="${kind}|${id}">Delete</button></td></tr>`;
    });
    return `<div class="grid two">
      ${card('ConfigMaps', table(['Name', 'Keys', 'Updated', ''], rows(cms, 'configmaps'), 'No config maps.'))}
      ${card('Secrets', table(['Name', 'Keys', 'Updated', ''], rows(secs, 'secrets'), 'No secrets. Values are never shown in the dashboard.'))}
    </div>`;
  },

  events(d) {
    const q = S.eventFilter.toLowerCase();
    const evs = d.events.slice().reverse().filter(e =>
      (S.eventType === 'all' || e.type === S.eventType) &&
      (S.ns === 'all' || !e.namespace || e.namespace === S.ns) &&
      (!q || `${e.kind} ${e.name} ${e.reason} ${e.message}`.toLowerCase().includes(q)));
    return card(`Events <span class="muted">(${evs.length})</span>`,
      evs.length ? `<div class="event-list">${evs.map(eventRow).join('')}</div>` : '<div class="empty">No events match.</div>',
      `<div class="btn-row"><input id="ev-filter" placeholder="Filter…" value="${esc(S.eventFilter)}">
       <select id="ev-type">${['all', 'Normal', 'Warning'].map(t => `<option ${t === S.eventType ? 'selected' : ''}>${t}</option>`).join('')}</select></div>`);
  },
  eventsMounted() {
    $('#ev-filter').oninput = e => { S.eventFilter = e.target.value; render(true); };
    $('#ev-type').onchange = e => { S.eventType = e.target.value; render(true); };
  },

  tokens(d) {
    if (d.me.role !== 'admin') return card('Access tokens', '<div class="empty">Only admins can manage tokens.</div>');
    return `<div class="grid two">
      ${card('Tokens', '<div id="token-list" class="empty">Loading…</div>')}
      ${card('Create token', `<div class="card-body grid">
        <input id="tok-name" placeholder="name, e.g. ci-deployer">
        <select id="tok-role"><option value="edit">edit — change workloads &amp; config</option><option value="view">view — read only</option>
          <option value="node">node — for agents joining the cluster</option><option value="admin">admin — everything</option></select>
        <input id="tok-ns" placeholder="namespaces (comma separated, empty = all)">
        <div class="btn-row"><button class="btn primary" id="tok-create">Create</button></div>
        <div id="tok-result"></div></div>`)}
    </div>`;
  },
  tokensMounted() {
    if (S.data.me.role !== 'admin') return;
    const load = async () => {
      const r = await api('/api/v1/tokens');
      const el = $('#token-list');
      if (!el) return;
      el.outerHTML = `<div id="token-list">${table(['Name', 'Role', 'Namespaces', 'Created', ''], r.items.map(t =>
        `<tr><td class="name">${esc(t.name)}</td><td>${chip(t.role, { admin: 'bad', edit: 'warn', view: 'info', node: 'ok' }[t.role])}</td>
         <td>${(t.namespaces || []).map(n => `<span class="tag">${esc(n)}</span>`).join('') || 'all'}</td><td>${ago(t.createdAt)} ago</td>
         <td class="actions"><button class="btn small danger" data-token-delete="${esc(t.name)}">Revoke</button></td></tr>`),
        'No tokens yet. The cluster admin token (in /var/lib/nube/token) always works.')}</div>`;
    };
    S.reloadTokens = load;
    load().catch(e => toast(e.message, true));
    $('#tok-create').onclick = async () => {
      const ns = $('#tok-ns').value.split(',').map(x => x.trim()).filter(Boolean);
      try {
        const t = await post('/api/v1/tokens', { name: $('#tok-name').value.trim(), role: $('#tok-role').value, namespaces: ns });
        $('#tok-result').innerHTML = `<div class="small">Copy it now, it will not be shown again:</div><pre class="code">${esc(t.secret)}</pre>`;
        load();
      } catch (e) { toast(e.message, true); }
    };
  },

  cluster(d) {
    const c = d.cluster;
    const info = `<div class="card-body"><dl class="kv">
      <dt>Mode</dt><dd>${c.mode === 'ha' ? chip('High availability (Raft)', 'ok') : chip('Single server', 'warn')}</dd>
      <dt>Leader</dt><dd>${esc(c.leader || '—')}</dd>
      <dt>Answered by</dt><dd>${esc(c.serverID)} (${esc(c.state)})</dd>
      ${c.mode === 'ha' ? `<dt>Raft term</dt><dd>${c.term}</dd>` : ''}
      <dt>API TLS</dt><dd>${c.tls ? chip('on', 'ok') + ` <a href="/ca.crt" download>download CA</a>` : chip('off', 'warn')}</dd>
      ${c.caHash ? `<dt>CA hash</dt><dd class="mono small" style="word-break:break-all">${esc(c.caHash)}</dd>` : ''}
      <dt>Server uptime</dt><dd>${ago(d.startedAt)}</dd>
    </dl></div>`;
    const members = c.mode === 'ha' ? table(['Server', 'Role', 'Raft address', 'API', 'Voter'], (c.members || []).map(m =>
      `<tr><td class="name">${esc(m.id)}</td><td>${m.leader ? chip('leader', 'ok') : chip('follower', 'info')}</td>
       <td class="mono">${esc(m.raftAddr)}</td><td class="mono">${esc(m.apiAddr || '—')}</td><td>${m.voter ? 'yes' : 'no'}</td></tr>`), 'No members')
      : `<div class="card-body muted">Run three servers with <code>--ha</code> to survive the loss of any one of them.
         The first uses <code>--bootstrap</code>, the others <code>--join http://FIRST:7070</code>. See the README.</div>`;
    return `<div class="grid two">${card('Control plane', info)}${card('Members', members)}</div>`;
  },

  apply() {
    return `<div class="apply-grid">
      ${card('Manifest', `<div class="card-body grid">
        <textarea id="yaml" rows="24" spellcheck="false" placeholder="Paste YAML (multiple documents separated by ---)"></textarea>
        <div class="btn-row"><button class="btn primary" id="apply-btn">Apply</button><button class="btn danger" id="delete-btn">Delete these resources</button></div>
        <div id="apply-result"></div></div>`)}
      ${card('Templates', `<div class="card-body templates">${Object.keys(TEMPLATES).map(k => `<button class="btn" data-tpl="${esc(k)}">${esc(k)}</button>`).join('')}
        <p class="muted small">Templates fill the editor; nothing is created until you press Apply.</p></div>`)}
    </div>`;
  },
  applyMounted() {
    const ta = $('#yaml');
    ta.value = lsGet('nube.draft') || '';
    ta.oninput = () => lsSet('nube.draft', ta.value);
    const run = async path => {
      try {
        const r = await api(path, { method: 'POST', body: ta.value, headers: { 'Content-Type': 'application/yaml' } });
        $('#apply-result').innerHTML = r.results.map(l => `<div class="result-line">✓ ${esc(l)}</div>`).join('');
        refresh();
      } catch (e) { $('#apply-result').innerHTML = `<div class="error-text">✗ ${esc(e.message)}</div>`; }
    };
    $('#apply-btn').onclick = () => run('/api/v1/apply');
    $('#delete-btn').onclick = () => confirm('Delete every resource in this manifest?') && run('/api/v1/delete');
    document.querySelectorAll('[data-tpl]').forEach(b => b.onclick = () => { ta.value = TEMPLATES[b.dataset.tpl]; lsSet('nube.draft', ta.value); });
  },
};

function replicaBlocks(dep, d) {
  const pods = d.pods.filter(p => p.spec.owner === dep.metadata.name && p.metadata.namespace === dep.metadata.namespace && p.status.phase !== 'Terminating');
  const n = Math.max(dep.spec.replicas, pods.length);
  if (n > 12) return '';
  let html = '';
  for (let i = 0; i < n; i++) {
    const p = pods[i];
    html += `<i class="${p ? (p.status.ready ? 'r' : 'p') : ''}"></i>`;
  }
  return `<span class="replicas">${html}</span>`;
}

function nodeCard(n, full) {
  const id = esc(n.name);
  const net = n.networkMode && n.networkMode !== 'none'
    ? (n.networkReady ? chip(n.networkMode, 'ok') : chip(n.networkMode + ' down', 'bad'))
    : chip('no overlay', '');
  return `<div class="card node-card">
    <div class="top"><b>${id}</b><span class="muted small">${esc(n.hostIP)}</span>${nodeChip(n)}</div>
    ${meter('CPU', n.usage.cpu, n.allocated.cpu, n.capacity.cpu, fmtCPU)}
    ${meter('Memory', n.usage.memory, n.allocated.memory, n.capacity.memory, fmtMem)}
    <dl class="kv">
      <dt>Pods</dt><dd>${n.podCount}</dd>
      <dt>Network</dt><dd>${net} <span class="mono small muted">${esc(n.podCIDR || '')}</span>${n.networkError ? `<div class="small" style="color:var(--bad)">${esc(n.networkError)}</div>` : ''}</dd>
      ${full ? `<dt>OS</dt><dd>${esc(n.info.os)} · ${esc(n.info.arch)}</dd>
      <dt>Kernel</dt><dd>${esc(n.info.kernel)}</dd>
      <dt>Runtime</dt><dd>docker ${esc(n.info.runtimeVersion)}</dd>
      <dt>Agent</dt><dd>${esc(n.info.agentVersion)}</dd>
      <dt>Heartbeat</dt><dd>${ago(n.lastHeartbeat)} ago</dd>
      <dt>Labels</dt><dd>${labels(n.labels)}</dd>` : `<dt>Heartbeat</dt><dd>${ago(n.lastHeartbeat)} ago</dd>`}
    </dl>
    ${full ? `<div class="btn-row">
      ${n.cordoned ? `<button class="btn small" data-node="${id}|uncordon">Uncordon</button>` : `<button class="btn small" data-node="${id}|cordon">Cordon</button>`}
      <button class="btn small" data-node="${id}|drain">Drain</button>
      <button class="btn small danger" data-node="${id}|remove">Remove</button>
    </div>` : ''}
  </div>`;
}

function eventRow(e) {
  const obj = `${e.kind.toLowerCase()}/${e.name}`;
  return `<div class="event ${esc(e.type)}"><span class="t">${ago(e.time)} ago</span><span class="dot"></span>
    <div><span class="obj">${esc(obj)}</span>${e.namespace && e.namespace !== 'default' ? ` <span class="tag">${esc(e.namespace)}</span>` : ''}
    <span class="reason">${esc(e.reason)}</span><span>${esc(e.message)}</span></div></div>`;
}

// ------------------------------------------------------------ actions (event delegation)

$('#view').addEventListener('click', async e => {
  const t = e.target.closest('button, a');
  if (!t) return;
  const ds = t.dataset;
  if (ds.scale) {
    const [ns, name, delta] = ds.scale.split('|');
    const dep = S.data.deployments.find(x => x.metadata.namespace === ns && x.metadata.name === name);
    const n = Math.max(0, dep.spec.replicas + Number(delta));
    act(post(objPath('deployments', ns, name) + '/scale', { replicas: n }));
  } else if (ds.restart) {
    const [ns, name] = ds.restart.split('|');
    act(post(objPath('deployments', ns, name) + '/restart'));
  } else if (ds.history) {
    showHistory(...ds.history.split('|'));
  } else if (ds.delete) {
    const [kind, ns, name] = ds.delete.split('|');
    if (confirm(`Delete ${kind.replace(/s$/, '')} "${name}"?`)) act(del(objPath(kind, ns, name)));
  } else if (ds.node) {
    const [name, action] = ds.node.split('|');
    if (action === 'remove') {
      if (confirm(`Remove node "${name}" from the cluster? Its pods are rescheduled.`)) act(del('/api/v1/nodes/' + encodeURIComponent(name)));
    } else if (action !== 'drain' || confirm(`Drain "${name}"? All its pods will move to other nodes.`)) {
      act(post(`/api/v1/nodes/${encodeURIComponent(name)}/${action}`));
    }
  } else if (ds.stsScale) {
    const [ns, name, delta] = ds.stsScale.split('|');
    const ss = findObj('statefulsets', ns, name);
    act(post(objPath('statefulsets', ns, name) + '/scale', { replicas: Math.max(0, ss.spec.replicas + Number(delta)) }));
  } else if (ds.stsRestart) {
    const [ns, name] = ds.stsRestart.split('|');
    act(post(objPath('statefulsets', ns, name) + '/restart'));
  } else if (ds.cron) {
    const [ns, name, action] = ds.cron.split('|');
    act(post(objPath('cronjobs', ns, name) + '/' + action));
  } else if (ds.tokenDelete) {
    if (confirm(`Revoke token "${ds.tokenDelete}"? Anything using it stops working.`)) {
      await act(del('/api/v1/tokens/' + encodeURIComponent(ds.tokenDelete)));
      if (S.reloadTokens) S.reloadTokens();
    }
  } else if (ds.logs) {
    showLogs(...ds.logs.split('|'));
  } else if (ds.describe) {
    showDescribe(...ds.describe.split('|'));
  }
});

// ------------------------------------------------------------ modals

function openModal(title, body, actions = '') {
  $('#modal-title').textContent = title;
  $('#modal-body').innerHTML = body;
  $('#modal-actions').innerHTML = actions;
  $('#modal').classList.remove('hidden');
}
function closeModal() {
  if (S.logAbort) { S.logAbort.abort(); S.logAbort = null; }
  $('#modal').classList.add('hidden');
}

const KINDS = {
  deployments: ['deployments', 'Deployment'], statefulsets: ['statefulSets', 'StatefulSet'], jobs: ['jobs', 'Job'],
  cronjobs: ['cronJobs', 'CronJob'], pods: ['pods', 'Pod'], services: ['services', 'Service'], ingresses: ['ingresses', 'Ingress'],
  volumes: ['volumes', 'Volume'], configmaps: ['configMaps', 'ConfigMap'], secrets: ['secrets', 'Secret'],
};
function findObj(kind, ns, name) {
  return (S.data[KINDS[kind][0]] || []).find(o => o.metadata.namespace === ns && o.metadata.name === name);
}

function showDescribe(kind, ns, name) {
  const obj = findObj(kind, ns, name);
  if (!obj) return;
  const singular = KINDS[kind][1];
  const owner = ['deployments', 'statefulsets', 'jobs'].includes(kind);
  const evs = S.data.events.filter(e => (e.kind === singular && e.name === name) ||
    (owner && e.kind === 'Pod' && e.name.startsWith(name + '-'))).slice(-15).reverse();
  const ctrs = kind === 'pods' && (obj.status.containers || []).length ? `<h3 class="small muted">CONTAINERS</h3>` + table(
    ['Name', 'State', 'Ready', 'Restarts', 'Image', 'Message'], obj.status.containers.map(c => `<tr>
      <td class="name">${esc(c.name)}${c.init ? ' <span class="tag">init</span>' : ''}</td>
      <td>${chip(c.state, c.state === 'Running' ? 'ok' : c.state === 'Terminated' && c.exitCode === 0 ? 'info' : c.state === 'Terminated' ? 'bad' : 'warn')}</td>
      <td>${c.ready ? 'yes' : 'no'}</td><td>${c.restarts}</td><td class="mono small">${esc(c.image || '')}</td><td class="small">${esc(c.message || '')}</td></tr>`), '') : '';
  openModal(`${singular} ${ns}/${name}`,
    `${evs.length ? `<h3 class="small muted">RECENT EVENTS</h3><div class="card event-list" style="margin-bottom:14px">${evs.map(eventRow).join('')}</div>` : ''}
     ${ctrs}<h3 class="small muted">OBJECT</h3><pre class="code">${esc(JSON.stringify(obj, null, 2))}</pre>`,
    kind === 'pods' && obj.status.containerID ? `<button class="btn small" id="m-logs">Logs</button>` : '');
  const b = $('#m-logs');
  if (b) b.onclick = () => showLogs(ns, name);
}

function showHistory(ns, name) {
  const dep = findObj('deployments', ns, name);
  if (!dep) return;
  const rows = (dep.status.history || []).slice().reverse().map(h => `<tr>
    <td><b>${h.revision}</b></td><td class="mono">${esc(mainImage(h.template))}</td><td>${ago(h.createdAt)} ago</td>
    <td>${h.hash === dep.status.currentHash ? chip('current', 'ok') : `<button class="btn small" data-rollback="${h.revision}">Roll back</button>`}</td></tr>`);
  openModal(`Rollout history · ${name}`, table(['Revision', 'Image', 'Created', ''], rows, 'No history yet.'));
  document.querySelectorAll('[data-rollback]').forEach(b => b.onclick = async () => {
    await act(post(objPath('deployments', ns, name) + '/rollback', { revision: Number(b.dataset.rollback) }));
    closeModal();
  });
}

async function showLogs(ns, name) {
  if (S.logAbort) S.logAbort.abort();
  const pod = findObj('pods', ns, name);
  const ctrs = ((pod && pod.status.containers) || []).filter(c => c.containerID);
  const picker = ctrs.length > 1 ? `<select id="log-container">${ctrs.map(c =>
    `<option value="${esc(c.name)}">${esc(c.name)}${c.init ? ' (init)' : ''}</option>`).join('')}</select>` : '';
  if (ctrs.length > 1) {
    const firstApp = ctrs.find(c => !c.init);
    if (firstApp) setTimeout(() => { $('#log-container').value = firstApp.name; }, 0);
  }
  openModal(`Logs · ${ns}/${name}`, `<pre class="logs" id="logs">Connecting…</pre>`,
    `${picker}<label class="small muted"><input type="checkbox" id="log-follow" checked> follow</label>
     <button class="btn small" id="log-reload">Reload</button>`);
  const pre = $('#logs');
  const load = async () => {
    if (S.logAbort) S.logAbort.abort();
    const ctrl = new AbortController();
    S.logAbort = ctrl;
    pre.textContent = '';
    const follow = $('#log-follow').checked ? '1' : '';
    const ct = $('#log-container') ? '&container=' + encodeURIComponent($('#log-container').value) : '';
    try {
      const res = await fetch(`${objPath('pods', ns, name)}/logs?tail=500&follow=${follow}${ct}`,
        { headers: { Authorization: 'Bearer ' + S.token }, signal: ctrl.signal });
      if (!res.ok) { pre.textContent = (await res.json().catch(() => ({}))).error || res.statusText; return; }
      const reader = res.body.getReader();
      const dec = new TextDecoder();
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        const atBottom = pre.scrollHeight - pre.scrollTop - pre.clientHeight < 40;
        pre.textContent += dec.decode(value, { stream: true });
        if (pre.textContent.length > 400000) pre.textContent = pre.textContent.slice(-300000);
        if (atBottom) pre.scrollTop = pre.scrollHeight;
      }
      if (!pre.textContent) pre.textContent = '(no output)';
    } catch (e) {
      if (e.name !== 'AbortError') pre.textContent += '\n[' + e.message + ']';
    }
  };
  $('#log-reload').onclick = load;
  $('#log-follow').onchange = load;
  if ($('#log-container')) $('#log-container').onchange = load;
  setTimeout(load, 0);
}

// ------------------------------------------------------------ templates

const TEMPLATES = {
  'Sidecar + init container': `kind: Deployment
metadata:
  name: blog
spec:
  replicas: 2
  template:
    emptyDirs:
      - { name: html, sizeLimit: 64Mi }
    initContainers:
      - name: setup
        image: busybox:1.36
        command: ["sh", "-c", "echo '<h1>hello from the init container</h1>' > /html/index.html"]
        volumes: [{ emptyDir: html, containerPath: /html }]
    containers:
      - name: web
        image: nginx:alpine
        ports: [{ containerPort: 80 }]
        volumes: [{ emptyDir: html, containerPath: /usr/share/nginx/html }]
      - name: logger              # shares localhost with "web"
        image: busybox:1.36
        command: ["sh", "-c", "while true; do wget -qO- http://localhost/ >/dev/null && echo ok; sleep 10; done"]
`,
  'StatefulSet (database)': `kind: Service
metadata:
  name: db
spec:
  headless: true            # db-0.db, db-1.db resolve to the pods
  ports: [{ port: 6379 }]
---
kind: StatefulSet
metadata:
  name: db
spec:
  replicas: 2
  serviceName: db
  template:
    image: redis:7-alpine
    ports: [{ containerPort: 6379 }]
    volumes: [{ volume: data, containerPath: /data }]
  volumeClaimTemplates:
    - name: data             # creates data-db-0, data-db-1
      spec: { type: local, size: 1Gi }
`,
  'Job + CronJob': `kind: Job
metadata:
  name: migrate
spec:
  completions: 1
  backoffLimit: 3
  template:
    image: busybox:1.36
    command: ["sh", "-c", "echo migrating; sleep 3; echo done"]
---
kind: CronJob
metadata:
  name: nightly-report
spec:
  schedule: "0 2 * * *"      # every day at 02:00
  concurrencyPolicy: Forbid
  jobTemplate:
    template:
      image: busybox:1.36
      command: ["sh", "-c", "date; echo report"]
`,
  'Ingress (HTTP/HTTPS)': `kind: Ingress
metadata:
  name: web
spec:
  redirectHTTP: true
  tls:
    - hosts: [app.example.com]
      secretName: app-tls   # Secret of type tls; omit for self-signed
  rules:
    - host: app.example.com
      paths:
        - { path: /, backend: { service: web, port: 8080 } }
`,
  'UDP service': `kind: Deployment
metadata:
  name: echo-udp
spec:
  template:
    image: alpine/socat
    args: ["-v", "UDP-RECVFROM:9000,fork", "EXEC:cat"]
    ports: [{ containerPort: 9000, protocol: UDP }]
---
kind: Service
metadata:
  name: echo-udp
spec:
  ports: [{ port: 9000, protocol: UDP }]
`,
  'Web app + service': `kind: Deployment
metadata:
  name: web
spec:
  replicas: 3
  template:
    image: nginx:alpine
    ports:
      - containerPort: 80
    resources:
      cpu: 100m
      memory: 64Mi
    healthCheck:
      httpGet: { path: /, port: 80 }
      periodSeconds: 5
---
kind: Service
metadata:
  name: web
spec:
  selector: { app: web }
  ports:
    - port: 8080
      targetPort: 80
`,
  'Autoscaled API': `kind: Deployment
metadata:
  name: api
spec:
  replicas: 2
  autoscale: { min: 2, max: 8, targetCPUPercent: 70 }
  strategy: { maxSurge: 1, maxUnavailable: 0 }
  template:
    image: nginx:alpine
    ports: [{ containerPort: 80 }]
    resources: { cpu: 250m, memory: 128Mi }
`,
  'Redis with volume': `kind: Volume
metadata:
  name: redis-data
spec:
  type: local
  size: 1Gi
---
kind: Deployment
metadata:
  name: redis
spec:
  replicas: 1
  strategy: { maxSurge: 0, maxUnavailable: 1 }
  template:
    image: redis:7-alpine
    args: ["redis-server", "--appendonly", "yes"]
    ports: [{ containerPort: 6379 }]
    volumes:
      - volume: redis-data
        containerPath: /data
    healthCheck:
      tcpSocket: { port: 6379 }
---
kind: Service
metadata:
  name: redis
spec:
  ports: [{ port: 6379 }]
`,
  'Postgres + secret': `kind: Secret
metadata:
  name: pg-auth
stringData:
  POSTGRES_PASSWORD: change-me
---
kind: Volume
metadata:
  name: pg-data
spec:
  type: local
  size: 5Gi
---
kind: Deployment
metadata:
  name: postgres
spec:
  replicas: 1
  strategy: { maxSurge: 0, maxUnavailable: 1 }
  template:
    image: postgres:17-alpine
    envFrom: [{ secret: pg-auth }]
    env: { PGDATA: /var/lib/postgresql/data/pgdata }
    ports: [{ containerPort: 5432 }]
    resources: { cpu: 500m, memory: 512Mi }
    volumes:
      - volume: pg-data
        containerPath: /var/lib/postgresql/data
    healthCheck:
      tcpSocket: { port: 5432 }
      initialDelaySeconds: 5
`,
  'ConfigMap': `kind: ConfigMap
metadata:
  name: app-config
data:
  LOG_LEVEL: info
  FEATURE_X: "true"
`,
  'NFS volume': `kind: Volume
metadata:
  name: shared
spec:
  type: nfs
  nfs:
    server: 192.168.1.10
    path: /exports/shared
    options: nfsvers=4.1
`,
};

// ------------------------------------------------------------ go

if (S.token) start(); else logout();
