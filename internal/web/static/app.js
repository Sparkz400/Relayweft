// Switchyard web UI: plain JavaScript, no build step, no external requests.
'use strict';
(function () {

// ---------- helpers ----------
const $ = (sel, el) => (el || document).querySelector(sel);
const $$ = (sel, el) => Array.from((el || document).querySelectorAll(sel));

// h builds an element. Strings become text nodes (never HTML).
function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  if (attrs) {
    for (const k in attrs) {
      const v = attrs[k];
      if (v == null || v === false) continue;
      if (k === 'class') el.className = v;
      else if (k === 'style') el.setAttribute('style', v);
      else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
      else if (k === 'dataset') Object.assign(el.dataset, v);
      else if (k in el && typeof v !== 'string') el[k] = v;
      else el.setAttribute(k, v === true ? '' : v);
    }
  }
  for (const kid of kids.flat(Infinity)) {
    if (kid == null || kid === false) continue;
    el.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return el;
}

// Static, trusted SVG icons.
const ICONS = {
  check: '<path d="M5 12.5l4.5 4.5L19 7.5"/>',
  x: '<path d="M6 6l12 12M18 6L6 18"/>',
  slash: '<circle cx="12" cy="12" r="8"/><path d="M6.5 17.5l11-11"/>',
  tool: '<path d="M14.7 6.3a4 4 0 0 0-5.4 5.4L4 17l3 3 5.3-5.3a4 4 0 0 0 5.4-5.4l-2.5 2.5-2.4-.6-.6-2.4z"/>',
  edit: '<path d="M4 20h4L19 9l-4-4L4 16z"/><path d="M13.5 6.5l4 4"/>',
  msg: '<path d="M4 5h16v11H9l-5 4z"/>',
  think: '<circle cx="12" cy="12" r="1"/><circle cx="6" cy="12" r="1"/><circle cx="18" cy="12" r="1"/>',
  route: '<path d="M4 7h11l-3-3M20 17H9l3 3"/>',
  review: '<path d="M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7S2 12 2 12z"/><circle cx="12" cy="12" r="3"/>',
  err: '<circle cx="12" cy="12" r="9"/><path d="M12 7v6M12 16.5v.5"/>',
  merge: '<circle cx="6" cy="6" r="2"/><circle cx="6" cy="18" r="2"/><circle cx="18" cy="12" r="2"/><path d="M6 8v8M8 6c6 0 6 6 8 6"/>',
  play: '<path d="M7 5l12 7-12 7z"/>',
  dot: '<circle cx="12" cy="12" r="2.5"/>',
  log: '<path d="M5 7h14M5 12h14M5 17h9"/>',
  flag: '<path d="M5 21V4M5 4h11l-2 4 2 4H5"/>',
  up: '<path d="M12 19V5M6 11l6-6 6 6"/>',
  down: '<path d="M12 5v14M6 13l6 6 6-6"/>',
  trash: '<path d="M4 7h16M9 7V4h6v3M6 7l1 13h10l1-13"/>',
  grip: '<circle cx="9" cy="6" r="1"/><circle cx="15" cy="6" r="1"/><circle cx="9" cy="12" r="1"/><circle cx="15" cy="12" r="1"/><circle cx="9" cy="18" r="1"/><circle cx="15" cy="18" r="1"/>',
  plus: '<path d="M12 5v14M5 12h14"/>',
  spark: '<path d="M12 3v4M12 17v4M3 12h4M17 12h4M6 6l2.5 2.5M15.5 15.5L18 18M6 18l2.5-2.5M15.5 8.5L18 6"/>',
  agents: '<circle cx="12" cy="5" r="2.5"/><circle cx="5" cy="19" r="2.5"/><circle cx="19" cy="19" r="2.5"/><path d="M12 7.5v4M12 11.5l-6 5M12 11.5l6 5"/>',
  bell: '<path d="M6 16V11a6 6 0 0 1 12 0v5l2 2H4z"/><path d="M10 21h4"/>',
  kill: '<rect x="6" y="6" width="12" height="12" rx="2"/>',
};
function icon(name, cls) {
  const s = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  s.setAttribute('viewBox', '0 0 24 24');
  if (cls) s.setAttribute('class', cls);
  s.innerHTML = ICONS[name] || '';
  return s;
}

function human(n) {
  n = Number(n) || 0;
  if (n >= 1e6) return (n / 1e6).toFixed(1) + 'M';
  if (n >= 1e4) return Math.round(n / 1e3) + 'k';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'k';
  return String(n);
}
const fresh = (t) => t ? (t.input || 0) - (t.cached || 0) + (t.output || 0) : 0;
const addTok = (a, b) => ({ input: (a.input || 0) + (b.input || 0), cached: (a.cached || 0) + (b.cached || 0), output: (a.output || 0) + (b.output || 0), cost_usd: (a.cost_usd || 0) + (b.cost_usd || 0) });
function dur(ms) {
  if (!(ms >= 0)) return '';
  const s = Math.floor(ms / 1000);
  if (s < 60) return s + 's';
  const m = Math.floor(s / 60);
  if (m < 60) return m + 'm ' + String(s % 60).padStart(2, '0') + 's';
  return Math.floor(m / 60) + 'h ' + String(m % 60).padStart(2, '0') + 'm';
}
function clock(ts) {
  const d = new Date(ts);
  if (isNaN(d)) return '';
  return d.toTimeString().slice(0, 8);
}
function when(ts) {
  const d = new Date(ts);
  if (isNaN(d) || d.getFullYear() < 2000) return '';
  const now = new Date();
  const same = d.toDateString() === now.toDateString();
  return same ? d.toTimeString().slice(0, 5) : d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' }) + ' ' + d.toTimeString().slice(0, 5);
}
const oneLine = (s, n) => { s = String(s || '').replace(/\s+/g, ' ').trim(); return n && s.length > n ? s.slice(0, n - 1) + '…' : s; };
const store = {
  get(k) { try { return localStorage.getItem(k); } catch (e) { return null; } },
  set(k, v) { try { localStorage.setItem(k, v); } catch (e) { /* private mode */ } },
};

const ROLE_COLORS = {
  planner: 'var(--main)', worker: '#5AA9E6', worker_high: '#7F8CFF', explorer: '#56D6C9', researcher: '#9BD35A',
  reviewer: 'var(--reviewer)', judge: 'var(--router)', explore: '#56D6C9', research: '#9BD35A', edit: '#5AA9E6', fix: '#5AA9E6',
};
const ROLES = ['planner', 'worker', 'worker_high', 'explorer', 'researcher', 'reviewer', 'judge'];
const PLAN_ROLES = ['', 'planner', 'worker', 'worker_high', 'explorer', 'researcher'];
const PLAN_KINDS = ['explore', 'research', 'edit'];

// ---------- session ----------
// The link sy prints carries a single-use bootstrap in the URL fragment
// (#b=...), which never reaches a server. The page trades it for a session
// secret, kept in this tab's sessionStorage (it survives a reload of this
// tab only). There are no cookies.
const SESSION_KEY = 'sy-session';
let SESSION = null;
const sess = {
  get() { try { return sessionStorage.getItem(SESSION_KEY); } catch (e) { return SESSION; } },
  set(v) { SESSION = v; try { sessionStorage.setItem(SESSION_KEY, v); } catch (e) { /* in memory only */ } },
  clear() { SESSION = null; try { sessionStorage.removeItem(SESSION_KEY); } catch (e) { /* ignore */ } },
};
async function startSession() {
  // Some openers (VS Code's openExternal) percent-encode the "=".
  const m = /(?:^#|&)b=([0-9a-f]+)/.exec((location.hash || '').replace(/%3D/gi, '='));
  if (location.hash) history.replaceState(null, '', location.pathname + location.search);
  if (m) {
    try {
      const res = await fetch('/api/session', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ bootstrap: m[1] }) });
      const data = await res.json().catch(() => ({}));
      if (res.ok && data.session) { sess.set(data.session); return true; }
      // A fresh link failed: an older session of this tab may still work.
      if (sess.get() && await sessionWorks()) return true;
      lock(data.error || 'This link does not work any more.');
      return false;
    } catch (e) {
      lock('sy is not reachable - is it still running?');
      return false;
    }
  }
  SESSION = sess.get();
  if (SESSION && await sessionWorks()) return true;
  lock(SESSION ? 'This page\'s session ended (sy was restarted).' : null);
  return false;
}
async function sessionWorks() {
  SESSION = sess.get();
  try {
    const r = await fetch('/api/state', { headers: { 'X-Switchyard-Session': SESSION } });
    return r.ok;
  } catch (e) { return false; }
}
function lock(why) {
  if (es) { es.close(); es = null; }
  sess.clear();
  if (why) { why = String(why); $('#locked-why').textContent = why.charAt(0).toUpperCase() + why.slice(1) + (/[.?!)]$/.test(why) ? '' : '.'); }
  $('#locked').hidden = false;
}

// ---------- API ----------
async function api(method, path, body) {
  const opt = { method, headers: { 'X-Switchyard-Session': SESSION || '' } };
  if (body !== undefined) {
    opt.headers['Content-Type'] = 'application/json';
    opt.body = JSON.stringify(body);
  }
  let res;
  try {
    res = await fetch(path, opt);
  } catch (e) {
    throw new Error('sy is not reachable - is it still running?');
  }
  let data = null;
  try { data = await res.json(); } catch (e) { /* not JSON */ }
  if (res.status === 401) { lock('This page\'s session ended (sy was restarted).'); throw new Error('no session'); }
  if (!res.ok) throw new Error((data && data.error) || res.status + ' ' + res.statusText);
  return data;
}
async function act(method, path, body, okLevel) {
  try {
    const r = await api(method, path, body);
    if (r && r.message) toast(r.message, okLevel || 'info');
    return r;
  } catch (e) {
    toast(e.message, 'error');
    return null;
  }
}

// ---------- toasts ----------
function toast(text, level) {
  const t = h('div', { class: 'toast ' + (level || 'info') }, h('i'), h('div', null, text));
  $('#toasts').append(t);
  while ($('#toasts').children.length > 4) $('#toasts').firstChild.remove();
  setTimeout(() => { t.classList.add('out'); setTimeout(() => t.remove(), 300); }, level === 'error' ? 6500 : 3800);
}

// ---------- state ----------
const S = {
  snap: null,
  timeOffset: 0,
  connected: false,
  everConnected: false,
  replaying: false,
  nodes: new Map(),
  order: [],
  reviewer: null,
  judge: null,
  decisions: [],
  log: [],
  taskStartTs: null,
  filter: store.get('sy-filter') || 'all',
  agentFilter: null,
  search: '',
  selected: null,
  follow: true,
  dismissed: new Set(),
  seenApprovals: new Set(),
  openApproval: null,
  drawer: null,
  dirtyGraph: true,
  autoPrefilled: false,
};

function resetTree() {
  S.nodes = new Map();
  S.order = [];
  S.nodes.set('main', newNode('main', 'planner', 'main agent'));
  S.reviewer = { id: 'reviewer', role: 'reviewer', status: 'queued', checkpoints: [], calls: 0, tokens: {}, last: '', provider: '', model: '', touched: false };
  S.judge = null;
  S.decisions = [];
  S.dirtyGraph = true;
}
function newNode(id, role, title) {
  return { id, role: role || '', kind: '', title: title || '', provider: '', model: '', status: 'queued', last: '', lastErr: false, tokens: {}, started: 0, ended: 0, attempts: 0, files: new Set(), merge: '', mergeOK: false, pulse: 0, isNew: true };
}
function node(id) {
  let n = S.nodes.get(id);
  if (!n) {
    n = newNode(id);
    S.nodes.set(id, n);
    if (id !== 'main' && id !== 'reviewer' && id !== 'judge') S.order.push(id);
  }
  return n;
}

const tsOf = (e) => Date.parse(e.ts) || Date.now();
function kindCat(k) {
  switch (k) {
    case 'message': case 'thinking': case 'done': return 'msg';
    case 'tool': return 'tool';
    case 'edit': return 'edit';
    case 'route': return 'route';
    case 'checkpoint': return 'review';
    case 'error': case 'limit': case 'provider': return 'error';
  }
  return 'sys';
}
function addLog(entry) {
  entry.cat = entry.cat || kindCat(entry.kind);
  S.log.push(entry);
  if (S.log.length > 4000) S.log.splice(0, S.log.length - 3500);
  if (!S.replaying) appendRow(entry);
}

// fold applies one orchestrator event (mirrors the TUI's handleEvent).
function fold(e) {
  const k = e.kind;
  const ts = tsOf(e);
  switch (k) {
    case 'task_start':
      resetTree();
      S.taskStartTs = ts;
      S.nodes.get('main').title = e.text || '';
      addLog({ ts, kind: k, text: 'task: ' + (e.text || '') });
      return;
    case 'task_done': {
      for (const n of S.nodes.values()) {
        if (n.status === 'queued' || n.status === 'running') {
          if (n.id === 'main' && e.ok) continue;
          n.status = 'killed';
          if (!n.ended) n.ended = ts;
        }
      }
      if (S.reviewer.status === 'running') S.reviewer.status = 'killed';
      const main = S.nodes.get('main');
      if (e.ok) main.status = 'ok'; else if (main.status !== 'killed') main.status = 'failed';
      main.ended = ts;
      S.dirtyGraph = true;
      addLog({ ts, kind: k, text: e.text || '', ok: !!e.ok, cost: e.cost, tokens: e.tokens, took: S.taskStartTs ? ts - S.taskStartTs : 0 });
      if (!S.replaying) {
        const took = S.taskStartTs ? ts - S.taskStartTs : 0;
        if (took > 8000) notify(e.ok ? 'Switchyard: done' : 'Switchyard: failed', oneLine(e.text, 180));
      }
      return;
    }
    case 'phase':
      if (e.text === 'review' || e.text === 'fix') for (const id of S.order) { const n = S.nodes.get(id); if (n) n.pulse = Date.now(); }
      addLog({ ts, kind: k, text: e.text });
      S.dirtyGraph = true;
      return;
    case 'log':
      addLog({ ts, kind: k, text: e.text, agent: e.agent_id || '' });
      return;
    case 'provider':
      addLog({ ts, kind: 'limit', prov: e.provider, text: e.text });
      if (!S.replaying && e.until && Date.parse(e.until) > Date.now()) notify('Switchyard: ' + e.provider + ' hit its limit', e.text);
      return;
    case 'route': {
      const d = e.decision;
      if (!d) return;
      S.decisions.push({ d, agent: e.agent_id, at: ts });
      if (S.decisions.length > 30) S.decisions.shift();
      S.routerFlash = Date.now();
      S.dirtyGraph = true;
      const label = d.provider + ':' + d.model + (d.effort ? '@' + d.effort : '');
      addLog({ ts, kind: k, agent: e.agent_id, prov: d.provider, text: `${e.agent_id} → ${label}  [${d.rule} ${Number(d.confidence || 0).toFixed(2)}] ${d.reason || ''}${d.fallback ? ' (limit fallback)' : ''}` });
      return;
    }
    case 'checkpoint': {
      const r = S.reviewer;
      r.checkpoints.push({ name: String(e.text || '').split(':')[0], ok: !!e.ok, text: e.text });
      r.pulse = Date.now();
      r.touched = true;
      S.dirtyGraph = true;
      addLog({ ts, kind: k, agent: 'reviewer', prov: e.provider, ok: !!e.ok, text: (e.ok ? 'approved · ' : 'changes requested · ') + oneLine(e.text) });
      return;
    }
    case 'merge': {
      const n = node(e.agent_id);
      n.merge = e.text; n.mergeOK = !!e.ok; n.pulse = Date.now();
      S.dirtyGraph = true;
      addLog({ ts, kind: k, agent: e.agent_id, ok: !!e.ok, text: 'merge ' + (e.ok ? '✓ ' : '✗ ') + e.text });
      return;
    }
    case 'quota':
      return; // the state snapshot carries it
  }
  if (e.agent_id === 'reviewer') return reviewerEvent(e, ts);
  if (e.agent_id === 'judge') {
    if (!S.judge) S.judge = newNode('judge', 'judge', 'routing judge');
    return nodeEvent(S.judge, e, ts);
  }
  if (!e.agent_id) return;
  const n = node(e.agent_id);
  if (k === 'queued' && e.role && !n.kind && !e.provider) n.kind = e.role;
  nodeEvent(n, e, ts);
}

function nodeEvent(n, e, ts) {
  S.dirtyGraph = true;
  switch (e.kind) {
    case 'queued':
      if (e.text && n.id !== 'main') n.title = e.text;
      if (e.provider) {
        n.provider = e.provider; n.model = e.model; n.role = e.role || n.role;
        n.status = 'queued'; n.attempts++; n.pulse = Date.now();
      }
      return;
    case 'started':
      n.status = 'running'; n.started = ts; n.ended = 0;
      n.provider = e.provider; n.model = e.model;
      if (e.role) n.role = e.role;
      n.pulse = Date.now();
      break;
    case 'thinking': case 'tool': case 'message':
      if (e.text) { n.last = e.text; n.lastErr = false; }
      n.pulse = Date.now();
      break;
    case 'edit':
      n.files.add(e.text); n.last = 'edit ' + e.text; n.lastErr = false; n.pulse = Date.now();
      break;
    case 'usage':
      n.tokens = addTok(n.tokens, e.tokens || {});
      return;
    case 'error':
      n.last = e.text; n.lastErr = true; break;
    case 'limit':
      n.last = 'usage limit: ' + e.text; n.lastErr = true; break;
    case 'done':
      n.ended = ts;
      n.status = e.ok ? 'ok' : (/killed/.test(e.text || '') ? 'killed' : 'failed');
      if (e.text) { n.last = e.text; n.lastErr = !e.ok; }
      n.pulse = Date.now();
      break;
  }
  let text = e.text || '';
  if (e.kind === 'done' && !text.trim()) text = n.status;
  if (e.kind === 'thinking' && !text.trim()) return;
  addLog({ ts, kind: e.kind, agent: n.id, prov: e.provider, text, ok: e.kind === 'done' ? !!e.ok : undefined });
}

function reviewerEvent(e, ts) {
  const r = S.reviewer;
  r.touched = true;
  S.dirtyGraph = true;
  switch (e.kind) {
    case 'queued':
      if (e.provider) { r.provider = e.provider; r.model = e.model; r.status = 'queued'; r.pulse = Date.now(); }
      return;
    case 'started':
      r.status = 'running'; r.calls++; r.provider = e.provider; r.model = e.model; r.started = ts; r.pulse = Date.now();
      break;
    case 'usage':
      r.tokens = addTok(r.tokens, e.tokens || {});
      return;
    case 'done':
      r.status = e.ok ? 'ok' : 'failed'; r.ended = ts;
      break;
    case 'tool': case 'thinking': case 'message': case 'error': case 'limit':
      if (e.text) r.last = e.text;
      r.pulse = Date.now();
  }
  if (e.kind === 'message' || e.kind === 'thinking') return;
  addLog({ ts, kind: e.kind, agent: 'reviewer', prov: e.provider, text: e.text || '', ok: e.kind === 'done' ? !!e.ok : undefined });
}

// ---------- SSE ----------
let es = null;

// sy app exits soon after its window closes instead of waiting 30s; a
// reload says goodbye too but reconnects in time.
window.addEventListener('pagehide', (e) => {
  if (e.persisted || !SESSION) return;
  try {
    fetch('/api/bye', { method: 'POST', keepalive: true, headers: { 'X-Switchyard-Session': SESSION, 'Content-Type': 'application/json' }, body: '{}' }).catch(() => {});
  } catch (err) { /* best effort */ }
});
function connect() {
  if (es) es.close();
  es = new EventSource('/api/events?s=' + encodeURIComponent(SESSION || ''));
  es.addEventListener('state', (m) => { applySnap(JSON.parse(m.data)); });
  es.addEventListener('reset', () => {
    S.replaying = true;
    S.log = [];
    resetTree();
    S.taskStartTs = null;
  });
  es.addEventListener('ev', (m) => {
    try { fold(JSON.parse(m.data)); } catch (err) { console.error(err); }
  });
  es.addEventListener('synced', () => {
    S.replaying = false;
    // Replayed activity is history: no pulses for it.
    for (const n of S.nodes.values()) n.pulse = 0;
    if (S.reviewer) S.reviewer.pulse = 0;
    if (S.judge) S.judge.pulse = 0;
    S.routerFlash = 0;
    S.connected = true;
    S.everConnected = true;
    renderLog();
    renderGraph(true);
    renderBanner();
  });
  es.addEventListener('notice', (m) => {
    const n = JSON.parse(m.data);
    toast(n.text, n.level);
  });
  es.onerror = () => {
    S.connected = false;
    renderBanner();
    // EventSource hides the status: ask whether the session still works
    // (sy restarted = 401 = lock; sy gone = keep retrying).
    clearTimeout(S.probe);
    S.probe = setTimeout(async () => {
      try {
        const r = await fetch('/api/state', { headers: { 'X-Switchyard-Session': SESSION || '' } });
        if (r.status === 401) lock('This page\'s session ended (sy was restarted).');
      } catch (e) { /* sy is down; EventSource retries */ }
    }, 1000);
  };
}

function applySnap(s) {
  const prev = S.snap;
  S.snap = s;
  S.timeOffset = Date.now() - Date.parse(s.now);
  if (!prev || prev.running !== s.running) S.dirtyGraph = true;
  renderHeader();
  syncApprovals(prev);
  renderBanner();
  renderComposer();
  if (S.drawer === 'queue') openDrawer('queue', true);
  if (S.drawer === 'settings' && !document.activeElement.closest('#drawer')) openDrawer('settings', true);
  if (!S.autoPrefilled && s.demo && s.demo_task) {
    S.autoPrefilled = true;
    const p = $('#prompt');
    if (!p.value) { p.value = s.demo_task; autosize(); }
  }
  if (!prev) {
    document.title = (s.project ? s.project + ' · ' : '') + 'Switchyard';
  }
}

// ---------- header ----------
const PROV = {};
// Provider colors: the two built-in ones from the theme, the shipped
// presets by brand, any other from a palette (stable per name).
const PROV_COLORS = { gemini: '#4796E3', qwen: '#8B7CF6', deepseek: '#4D6BFE', ollama: '#C9C9C9' };
const PROV_PALETTE = ['#E5C07B', '#56B6C2', '#C678DD', '#98C379', '#E06C75'];
function provColor(p) {
  if (!p) return 'var(--muted)';
  if (p === 'codex' || p === 'claude') return `var(--${p})`;
  if (PROV_COLORS[p]) return PROV_COLORS[p];
  let x = 0;
  for (const ch of p) x = (x * 31 + ch.codePointAt(0)) | 0;
  return PROV_PALETTE[((x % PROV_PALETTE.length) + PROV_PALETTE.length) % PROV_PALETTE.length];
}
// provOrder lists a per-provider object's keys: codex, claude, then by name.
function provOrder(m) {
  const keys = Object.keys(m || {});
  const head = ['codex', 'claude'].filter((p) => keys.includes(p));
  return head.concat(keys.filter((p) => p !== 'codex' && p !== 'claude').sort());
}
function renderHeader() {
  const s = S.snap;
  if (!s) return;
  $('#project').textContent = s.project + (s.demo ? ' · demo' : '');
  $('#project').title = s.dir;
  const ph = $('#phase');
  let phase = s.paused ? 'paused' : (s.running ? (s.phase === 'done' ? 'finishing' : s.phase || 'running') : (s.phase === 'done' ? 'done' : 'idle'));
  if (s.cancelling) phase = 'cancelling';
  ph.textContent = String(phase).replace(/-/g, ' ');
  ph.dataset.phase = s.running ? (s.phase || '') : (s.phase === 'done' ? 'done' : 'idle');
  if (s.cancelling) ph.dataset.phase = 'cancelling';
  ph.dataset.active = s.running && !s.paused && !s.cancelling && s.phase !== 'review' && s.phase !== 'fix' ? '1' : '0';
  ph.dataset.ok = s.last && s.last.ok ? '1' : '0';
  ph.dataset.paused = s.paused ? '1' : '0';
  const tt = $('#tasktext');
  if (s.running) tt.textContent = s.task;
  else if (s.last) tt.textContent = (s.last.ok ? 'Finished: ' : 'Failed: ') + oneLine(s.last.text, 200);
  else tt.textContent = 'Ready for a task';
  tt.title = tt.textContent;

  const pv = $('#providers');
  const shown = provOrder(s.providers).filter((p) => !s.providers[p].disabled || s.providers[p].calls || s.providers[p].limited_until);
  for (const p of Object.keys(PROV)) if (!shown.includes(p)) { PROV[p].root.remove(); delete PROV[p]; }
  for (const p of shown) {
    const d = s.providers[p] || {};
    let val, pct, title;
    const limited = d.limited_until && Date.parse(d.limited_until) > Date.now() - S.timeOffset;
    if (limited) {
      val = 'limit · ' + new Date(Date.parse(d.limited_until) + S.timeOffset).toTimeString().slice(0, 5);
      pct = 1;
      title = p + ' is at its usage limit; its roles use the next provider until then';
    } else if (d.quota) {
      pct = d.quota.utilization;
      val = Math.round(pct * 100) + '%' + (d.quota.window ? ' · ' + d.quota.window.replace('five_hour', '5h').replace('seven_day', '7d') : '');
      title = p + ' subscription window used (as reported by the CLI)';
    } else {
      pct = d.share || 0;
      val = human(d.fresh) + ' tokens';
      title = `${p}: ${human(d.fresh)} fresh tokens in ${d.calls || 0} call(s) this session; the bar is its share of all tokens`;
    }
    let el = PROV[p];
    if (!el) {
      el = PROV[p] = { val: h('span', { class: 'prov-val' }), fill: h('span') };
      el.root = h('div', { class: 'prov', style: `--c: ${provColor(p)}` },
        h('div', { class: 'prov-top' }, h('span', { class: 'prov-name' }, h('i'), p), el.val), h('div', { class: 'bar' }, el.fill));
      pv.append(el.root);
    }
    el.root.classList.toggle('limited', !!limited);
    el.root.title = title;
    el.val.textContent = val;
    const w = Math.max(2, Math.min(100, pct * 100)).toFixed(1) + '%';
    requestAnimationFrame(() => { el.fill.style.width = w; });
  }
  const cost = $('#cost');
  cost.textContent = '';
  let tok = 0, usd = 0;
  for (const n of S.nodes.values()) { tok += fresh(n.tokens); usd += n.tokens.cost_usd || 0; }
  if (S.reviewer) { tok += fresh(S.reviewer.tokens); usd += S.reviewer.tokens.cost_usd || 0; }
  if (s.running || tok) {
    cost.append(h('b', null, human(tok) + ' tok' + (usd ? ' · $' + usd.toFixed(2) : '')), s.running ? 'this task' : 'last task');
    cost.title = usd ? 'Fresh tokens; $ is Claude\'s API-equivalent price (not billed on a subscription)' : 'Fresh tokens used';
  }
  renderBudget(s.budget);
  $('#btn-pause').classList.toggle('on', !!s.paused);
  $('#btn-pause').title = s.paused ? 'Resume dispatching' : 'Pause dispatching (running agents finish)';
  $('#btn-cancel').disabled = !s.running;
  const qb = $('#queue-badge');
  qb.hidden = !s.queue.length;
  qb.textContent = s.queue.length;
  $('#history-badge').hidden = !s.interrupted;
  tickElapsed();
}
// Budget status: "$0.42/$2 today" (and the running task's share).
function money(v) { return Number.isInteger(v) ? String(v) : v.toFixed(2); }
function renderBudget(b) {
  const el = $('#budget');
  const l = (b && b.limits) || {};
  let main = '', sub = '', worst = 0;
  const level = (used, max) => { worst = Math.max(worst, used / max); };
  if (l.day_usd > 0) { main = `$${b.day_usd.toFixed(2)}/$${money(l.day_usd)}`; sub = 'today'; level(b.day_usd, l.day_usd); }
  else if (l.day_tokens > 0) { main = `${human(b.day_tokens)}/${human(l.day_tokens)} tok`; sub = 'today'; level(b.day_tokens, l.day_tokens); }
  if (b && b.running) {
    let t = '';
    if (l.task_usd > 0) { t = `task $${b.task_usd.toFixed(2)}/$${money(l.task_usd)}`; level(b.task_usd, l.task_usd); }
    else if (l.task_tokens > 0) { t = `task ${human(b.task_tokens)}/${human(l.task_tokens)} tok`; level(b.task_tokens, l.task_tokens); }
    if (t && main) sub += ' · ' + t;
    else if (t) { main = t; sub = 'budget'; }
  }
  el.hidden = !main;
  if (!main) return;
  el.textContent = '';
  el.append(h('b', null, main), sub);
  el.dataset.level = worst >= 1 ? 'over' : (l.warn_at > 0 && worst >= l.warn_at ? 'warn' : '');
  el.title = 'Budget (config budget.*; 0 = off). $ is Claude\'s API-equivalent price; tokens are fresh tokens. At a limit you are asked whether the task goes on; queued and scheduled tasks stop.';
}
function tickElapsed() {
  const s = S.snap;
  const el = $('#elapsed');
  if (s && s.running && s.task_start) el.textContent = dur(Date.now() - S.timeOffset - Date.parse(s.task_start));
  else if (s && s.last && s.last.took) el.textContent = s.last.took;
  else el.textContent = '';
}

// ---------- banner ----------
function renderBanner() {
  const b = $('#banner');
  b.textContent = '';
  b.className = 'banner';
  const s = S.snap;
  if (S.everConnected && !S.connected) {
    b.classList.add('info');
    b.append(h('span', { class: 'dot' }), h('div', { class: 'grow' }, 'Lost the connection to sy - reconnecting… (if you closed sy, run `sy web` again)'));
    b.hidden = false;
    return;
  }
  if (!s) { b.hidden = true; return; }
  const waiting = (s.approvals || []).filter((a) => !S.openApproval || S.openApproval.id !== a.id);
  if (waiting.length && (!S.openApproval)) {
    const a = waiting[0];
    b.append(h('span', { class: 'dot' }),
      h('div', { class: 'grow' }, h('b', null, approvalTitle(a)),
        waiting.length > 1 ? h('span', { class: 'muted' }, ` · ${waiting.length - 1} more`) : ''),
      h('button', { class: 'btn primary sm', onclick: () => openApproval(a, true) }, 'Open'));
    b.hidden = false;
    return;
  }
  if (s.interrupted && !s.running && !S.dismissed.has('int:' + s.interrupted.id)) {
    const t = s.interrupted;
    b.classList.add('info');
    b.append(h('span', { class: 'dot', style: 'background:var(--warn)' }),
      h('div', { class: 'grow' }, h('b', null, 'A task was interrupted '), h('span', { class: 'muted' }, when(t.updated) + ' · '), oneLine(t.task, 120)),
      h('button', { class: 'btn primary sm', onclick: () => act('POST', '/api/resume', {}) }, 'Resume'),
      h('button', { class: 'btn ghost sm', onclick: () => { S.dismissed.add('int:' + t.id); renderBanner(); } }, 'Dismiss'));
    b.hidden = false;
    return;
  }
  b.hidden = true;
}

// ---------- agent graph ----------
const cards = new Map();
function statusEl(status) {
  const st = h('span', { class: 'st ' + status, title: status });
  if (status === 'ok') st.append(icon('check'));
  else if (status === 'failed') st.append(icon('x'));
  else if (status === 'killed') st.append(icon('slash'));
  return st;
}
function cardFor(key, build) {
  let c = cards.get(key);
  if (!c) { c = build(); cards.set(key, c); }
  return c;
}
function buildCard(n) {
  const el = h('div', { class: 'card enter', tabindex: '0', role: 'button' });
  const refs = {
    id: h('span', { class: 'card-id' }), role: h('span', { class: 'card-role' }),
    elapsed: h('span', { class: 'elapsed-t' }), st: h('span'), title: h('div', { class: 'card-title' }),
    route: h('span', { class: 'route' }), tok: h('span'), att: h('span'), last: h('div', { class: 'card-last' }),
    merge: h('div', { class: 'card-merge' }), chips: h('div', { class: 'chips' }),
  };
  const kill = h('button', { class: 'btn sm kill', title: 'Kill this agent', onclick: (ev) => { ev.stopPropagation(); act('POST', '/api/kill', { agent: n.id }, 'warn'); } }, icon('kill'), 'Kill');
  el.append(
    h('div', { class: 'card-top' }, refs.id, refs.role, h('span', { class: 'card-right' }, refs.elapsed, refs.st)),
    refs.title, h('div', { class: 'card-meta' }, refs.route, refs.tok, refs.att), refs.last, refs.merge, refs.chips, kill);
  el.addEventListener('click', () => selectAgent(n.id));
  el.addEventListener('keydown', (ev) => { if (ev.key === 'Enter' || ev.key === ' ') { ev.preventDefault(); selectAgent(n.id); } });
  setTimeout(() => el.classList.remove('enter'), 500);
  return { el, refs, kill, pulse: 0, status: '' };
}
function updateCard(c, n, opts) {
  const { el, refs } = c;
  el.classList.toggle('codex', n.provider === 'codex');
  el.classList.toggle('claude', n.provider === 'claude');
  if (n.provider && n.provider !== 'codex' && n.provider !== 'claude') el.style.setProperty('--c', provColor(n.provider));
  el.classList.toggle('sel', S.agentFilter === n.id);
  if (opts && opts.extra) el.classList.add(opts.extra);
  refs.id.textContent = opts && opts.name ? opts.name : n.id;
  const role = n.role || n.kind || '';
  refs.role.textContent = role.replace('_', ' ');
  refs.role.hidden = !role;
  refs.role.style.setProperty('--rc', ROLE_COLORS[role] || 'var(--muted)');
  refs.title.textContent = n.title && n.title !== n.id ? n.title : '';
  refs.title.hidden = !refs.title.textContent;
  refs.route.textContent = n.provider ? n.provider + ':' + (n.model || '?') : 'routing…';
  refs.route.hidden = !n.provider && n.status !== 'queued';
  const tk = fresh(n.tokens);
  refs.tok.textContent = tk ? human(tk) + ' tok' : '';
  refs.att.textContent = n.attempts > 1 ? 'attempt ' + n.attempts : (n.calls > 1 ? n.calls + ' calls' : '');
  refs.last.textContent = n.last ? oneLine(n.last, 160) : '';
  refs.last.hidden = !n.last;
  refs.last.classList.toggle('err', !!n.lastErr);
  refs.merge.textContent = n.merge ? (n.mergeOK ? '✓ merged ' : '✗ merge: ') + oneLine(n.merge, 90) : '';
  refs.merge.className = 'card-merge ' + (n.mergeOK ? 'ok' : 'bad');
  refs.merge.hidden = !n.merge;
  if (c.status !== n.status) {
    refs.st.replaceWith(refs.st = statusEl(n.status));
    c.status = n.status;
  }
  c.kill.hidden = n.status !== 'running';
  if (n.pulse && n.pulse !== c.pulse) {
    c.pulse = n.pulse;
    el.classList.remove('pulse');
    void el.offsetWidth;
    el.classList.add('pulse');
  }
  updateElapsed(c, n);
}
function updateElapsed(c, n) {
  let t = '';
  if (n.started) t = dur((n.ended || Date.now()) - n.started);
  c.refs.elapsed.textContent = t;
}

function mainView() {
  const main = S.nodes.get('main');
  const taskRunning = !!(S.snap && S.snap.running);
  return Object.assign({}, main, {
    status: taskRunning ? 'running' : main.status,
    started: S.taskStartTs || main.started,
    ended: taskRunning ? 0 : (main.ended || main.started),
  });
}

let graphBuilt = false;
const G = {};
function renderGraph(force) {
  if (!S.dirtyGraph && !force) return;
  S.dirtyGraph = false;
  const g = $('#graph');
  const main = S.nodes.get('main');
  const active = !!(S.snap && S.snap.running) || S.order.length > 0 || main.provider;
  if (!active) {
    if (!G.empty) {
      g.textContent = '';
      cards.clear();
      graphBuilt = false;
      G.empty = h('div', { class: 'empty' }, icon('agents', 'big'), h('h3', null, 'No agents yet'),
        h('p', null, 'Run a task: a planner splits it, the router picks Codex or Claude per step, and every agent shows up here live.'));
      g.append(G.empty);
    }
    $('#agent-count').textContent = '';
    return;
  }
  if (!graphBuilt) {
    g.textContent = '';
    G.empty = null;
    cards.clear();
    G.tree = h('div', { class: 'tree' });
    G.mainSlot = h('div', { class: 'node-main' });
    G.router = h('div', { class: 'router' });
    G.pill = h('div', { class: 'router-pill', title: 'Routing decisions - click to show them in the log', onclick: () => setFilter('route') });
    G.router.append(G.pill);
    G.kids = h('div', { class: 'kids' });
    G.tree.append(G.mainSlot, G.router, G.kids);
    G.revLabel = h('div', { class: 'section-label' }, 'Reviewer');
    G.revSlot = h('div');
    G.judgeLabel = h('div', { class: 'section-label' }, 'Judge');
    G.judgeSlot = h('div');
    g.append(G.tree, G.revLabel, G.revSlot, G.judgeLabel, G.judgeSlot);
    graphBuilt = true;
  }
  // Main.
  // The main card stands for the whole task: it spins while the task runs
  // and its clock is the task's.
  const mc = cardFor('main', () => buildCard(main));
  updateCard(mc, mainView(), { name: 'main' });
  mc.kill.hidden = true;
  if (mc.el.parentNode !== G.mainSlot) G.mainSlot.append(mc.el);
  // Router pill.
  G.pill.textContent = '';
  const last = S.decisions[S.decisions.length - 1];
  G.pill.append(icon('route'), h('span', null, 'router'),
    h('span', { class: 'd' }, last ? `${last.agent} → ${last.d.provider}:${last.d.model}${last.d.effort ? '@' + last.d.effort : ''}` : 'waiting for the plan'),
    S.decisions.length ? h('span', { class: 'd' }, '· ' + S.decisions.length) : '');
  if (S.routerFlash && S.routerFlash !== G.flashSeen) {
    G.flashSeen = S.routerFlash;
    G.router.classList.remove('flash'); void G.router.offsetWidth; G.router.classList.add('flash');
  }
  G.router.hidden = !S.order.length && !S.decisions.length;
  // Children, in order.
  const want = S.order.map((id) => S.nodes.get(id)).filter(Boolean);
  for (const [k, c] of cards) {
    if (k.startsWith('kid:') && !S.nodes.has(k.slice(4))) { c.el.parentNode && c.el.parentNode.remove(); cards.delete(k); }
  }
  want.forEach((n, i) => {
    const c = cardFor('kid:' + n.id, () => {
      const cc = buildCard(n);
      cc.wrap = h('div', { class: 'kid' }, cc.el);
      return cc;
    });
    updateCard(c, n);
    c.wrap.classList.toggle('flow', n.status === 'running');
    c.wrap.style.setProperty('--c', n.provider ? provColor(n.provider) : 'var(--codex)');
    if (G.kids.children[i] !== c.wrap) G.kids.insertBefore(c.wrap, G.kids.children[i] || null);
  });
  G.kids.hidden = !want.length;
  // Reviewer.
  const r = S.reviewer;
  G.revLabel.hidden = G.revSlot.hidden = !r.touched;
  if (r.touched) {
    const rc = cardFor('reviewer', () => buildCard(r));
    updateCard(rc, Object.assign({ title: r.checkpoints.length ? '' : 'checks the plan, repeated errors and the final diff', attempts: 0 }, r), { name: 'reviewer', extra: 'reviewer' });
    rc.refs.chips.textContent = '';
    for (const cp of r.checkpoints.slice(-6)) rc.refs.chips.append(h('span', { class: 'cp ' + (cp.ok ? 'ok' : 'bad'), title: cp.text }, (cp.ok ? '✓ ' : '✗ ') + cp.name));
    if (rc.el.parentNode !== G.revSlot) G.revSlot.append(rc.el);
  }
  G.judgeLabel.hidden = G.judgeSlot.hidden = !S.judge;
  if (S.judge) {
    const jc = cardFor('judge', () => buildCard(S.judge));
    updateCard(jc, S.judge, { name: 'judge', extra: 'judge' });
    if (jc.el.parentNode !== G.judgeSlot) G.judgeSlot.append(jc.el);
  }
  const running = want.filter((n) => n.status === 'running').length;
  $('#agent-count').textContent = want.length ? `${want.length} agent${want.length > 1 ? 's' : ''}${running ? ' · ' + running + ' running' : ''}` : '';
  // The header's token total follows the tree (debounced).
  if (S.snap && !S._costT) S._costT = setTimeout(() => { S._costT = null; renderHeader(); }, 400);
}

function selectAgent(id) {
  S.agentFilter = S.agentFilter === id ? null : id;
  renderAgentFilter();
  renderLog();
  S.dirtyGraph = true;
  renderGraph();
}
function renderAgentFilter() {
  const a = $('#agent-filter');
  a.textContent = '';
  a.hidden = !S.agentFilter;
  if (S.agentFilter) a.append('agent: ', h('b', null, S.agentFilter), h('button', { title: 'Show all agents', onclick: () => selectAgent(S.agentFilter) }, '×'));
}

// ---------- log ----------
const KIND_ICON = { message: 'msg', thinking: 'think', tool: 'tool', edit: 'edit', route: 'route', checkpoint: 'review', error: 'err', limit: 'err', merge: 'merge', started: 'play', done: 'check', phase: 'flag', task_start: 'flag', log: 'log', queued: 'dot', task_done: 'flag' };
function matches(e) {
  if (S.agentFilter && e.agent !== S.agentFilter) return false;
  if (S.filter !== 'all' && e.cat !== S.filter) {
    if (!(S.filter === 'review' && (e.agent === 'reviewer' || (e.kind === 'phase' && /review|fix/.test(e.text))))) return false;
  }
  if (S.search && !(String(e.text) + ' ' + (e.agent || '')).toLowerCase().includes(S.search)) return false;
  return true;
}
function rowEl(e, animate) {
  if (e.kind === 'task_done') {
    const per = [];
    if (e.cost && e.cost.per_provider) for (const p of provOrder(e.cost.per_provider)) { const u = e.cost.per_provider[p]; if (u && fresh(u)) per.push(p + ' ' + human(fresh(u))); }
    let line = per.length ? per.join(' · ') + ' fresh tokens' : '';
    if (e.cost && e.cost.cost_usd) line += ` · ≈$${e.cost.cost_usd.toFixed(2)} API-equivalent`;
    if (e.took) line += (line ? ' · ' : '') + dur(e.took);
    return h('div', { class: 'result' + (e.ok ? '' : ' bad') },
      h('h4', null, icon(e.ok ? 'check' : 'x'), e.ok ? 'Task finished' : 'Task did not finish'),
      h('p', null, e.text || ''), line ? h('div', { class: 'cost-line' }, line) : '');
  }
  const cls = ['row', 'k-' + e.kind];
  if (e.ok === true) cls.push('ok');
  if (e.ok === false) cls.push('bad');
  if (animate) cls.push('new');
  const who = h('span', { class: 'who ' + (e.prov || ''), title: e.agent ? 'Show only ' + e.agent : '' }, e.agent || 'sy');
  if (e.prov && e.prov !== 'codex' && e.prov !== 'claude') who.style.color = provColor(e.prov);
  if (e.agent) who.addEventListener('click', () => selectAgent(e.agent));
  let ic = KIND_ICON[e.kind] || 'dot';
  if ((e.kind === 'done' || e.kind === 'merge' || e.kind === 'checkpoint') && e.ok === false) ic = 'x';
  else if (e.kind === 'checkpoint' && e.ok) ic = 'check';
  return h('div', { class: cls.join(' ') }, h('span', { class: 'ts' }, clock(e.ts)), who, h('span', { class: 'k' }, icon(ic)), h('span', { class: 'tx' }, e.text));
}
function nearBottom(el) { return el.scrollHeight - el.scrollTop - el.clientHeight < 60; }
function renderLog() {
  const log = $('#log');
  log.textContent = '';
  const rows = S.log.filter(matches);
  if (!rows.length) {
    log.append(emptyLog());
    return;
  }
  const frag = document.createDocumentFragment();
  for (const e of rows.slice(-2500)) frag.append(rowEl(e, false));
  log.append(frag);
  log.scrollTop = log.scrollHeight;
  S.follow = true;
  $('#jump').hidden = true;
}
function appendRow(e) {
  if (!matches(e)) return;
  const log = $('#log');
  const empty = $('.log-empty', log);
  if (empty) empty.remove();
  const stick = S.follow || nearBottom(log);
  log.append(rowEl(e, true));
  while (log.childElementCount > 3000) log.firstElementChild.remove();
  if (stick) log.scrollTop = log.scrollHeight;
  else $('#jump').hidden = false;
}
function emptyLog() {
  const s = S.snap || {};
  const tips = [
    ['play', s.demo ? 'Run the demo task: fake agents plan, route, review and merge - nothing is touched.' : 'Type a task below and press Enter. A planner splits it; each step goes to the best model.', s.demo ? () => { $('#prompt').value = s.demo_task; autosize(); submit(); } : null],
    ['msg', 'Follow up with a finished agent: @parser make the error message clearer', null],
    ['edit', 'Turn on plan approval or change review in Settings to check work before it lands.', () => openDrawer('settings')],
  ];
  return h('div', { class: 'log-empty' },
    S.search || S.agentFilter || S.filter !== 'all' ? h('div', null, 'Nothing matches the filter.') :
      h('div', null, h('div', { style: 'font-size:15px;color:var(--text);font-weight:600' }, 'Ready when you are'),
        h('div', { class: 'tips' }, tips.map(([ic, t, fn]) => h('div', { class: 'tip' + (fn ? ' click' : ''), onclick: fn }, icon(ic), h('span', null, t))))));
}
function setFilter(f) {
  S.filter = f;
  store.set('sy-filter', f);
  $$('#filters .chip').forEach((c) => c.classList.toggle('on', c.dataset.filter === f));
  renderLog();
}

// ---------- composer ----------
const prompt = () => $('#prompt');
function autosize() {
  const p = prompt();
  p.style.height = 'auto';
  p.style.height = Math.min(220, p.scrollHeight) + 'px';
}
function renderComposer() {
  const s = S.snap;
  if (!s) return;
  $('#send-label').textContent = s.running ? 'Queue' : 'Run';
  const hints = $('#hints');
  hints.textContent = '';
  const add = (k, t) => hints.append(h('span', null, h('kbd', null, k), t));
  add('Enter', 'run');
  add('Shift+Enter', 'new line');
  add('@', 'follow up / tell an agent');
  if (s.running) add('Ctrl+X', 'cancel task');
  add('Esc', 'close panel');
  if (s.running) hints.append(h('span', null, 'A task typed now is queued and runs unattended afterwards.'));
}
async function submit() {
  const p = prompt();
  const text = p.value.trim();
  if (!text) return;
  askNotifyPermission();
  hideAC();
  const r = await act('POST', '/api/task', { text }, 'ok');
  if (r) {
    p.value = '';
    autosize();
    if (r.status === 'started') { S.agentFilter = null; renderAgentFilter(); }
  }
}

// @agent autocomplete
const AC = { items: [], sel: 0, open: false, cache: null, at: 0 };
async function sessions() {
  if (AC.cache && Date.now() - AC.at < 2000) return AC.cache;
  try { AC.cache = await api('GET', '/api/sessions'); AC.at = Date.now(); } catch (e) { AC.cache = []; }
  return AC.cache;
}
async function updateAC() {
  const p = prompt();
  const v = p.value.slice(0, p.selectionStart);
  const m = /^@([a-z0-9_-]*)$/.exec(v);
  if (!m || p.selectionStart !== p.value.length) return hideAC();
  const all = await sessions();
  const items = all.filter((s) => s.agent.startsWith(m[1]));
  if (!items.length) return hideAC();
  AC.items = items; AC.sel = Math.min(AC.sel, items.length - 1); AC.open = true;
  const box = $('#ac');
  box.textContent = '';
  box.append(h('div', { class: 'ac-head' }, 'Agents · Tab to complete'));
  items.forEach((s, i) => box.append(h('div', { class: 'ac-item' + (i === AC.sel ? ' sel' : ''), onmousedown: (ev) => { ev.preventDefault(); AC.sel = i; acceptAC(); } },
    h('span', { class: 'id', style: `color:var(--${s.provider || 'text'})` }, '@' + s.agent),
    h('span', { class: 'meta' }, s.running ? 'running - your message is delivered when its turn ends' : [s.role, s.provider && s.provider + ':' + s.model, oneLine(s.title, 60)].filter(Boolean).join(' · ')),
    s.running ? h('span', { class: 'live' }, 'live') : '')));
  box.hidden = false;
}
function hideAC() { AC.open = false; $('#ac').hidden = true; }
function acceptAC() {
  const s = AC.items[AC.sel];
  if (!s) return;
  prompt().value = '@' + s.agent + ' ';
  hideAC();
  prompt().focus();
}

// ---------- approvals ----------
function syncApprovals() {
  const list = (S.snap && S.snap.approvals) || [];
  for (const a of list) {
    if (!S.seenApprovals.has(a.id)) {
      S.seenApprovals.add(a.id);
      notify('Switchyard needs you', a.type === 'plan' ? 'Approve the plan: ' + oneLine(a.task, 120) : a.type === 'budget' ? 'Budget reached: ' + a.budget.text : 'Review the changes of ' + a.changes.step_id);
      if (!S.openApproval && !$('#modal').dataset.busy) openApproval(a);
    }
  }
  if (S.openApproval && !list.some((a) => a.id === S.openApproval.id)) {
    closeModal();
    const next = list[0];
    if (next) openApproval(next);
  }
}
function closeModal() {
  $('#modal').hidden = true;
  $('#modal-card').textContent = '';
  S.openApproval = null;
  renderBanner();
}
function hideApproval() {
  // Esc: hide; the request stays pending (the banner reopens it).
  $('#modal').hidden = true;
  S.openApproval = null;
  renderBanner();
}
function openApproval(a) {
  S.openApproval = { id: a.id };
  // Someone typing when it pops up must not approve by accident: take the
  // focus away and ignore Ctrl+Enter for a moment.
  S.modalOpenedAt = Date.now();
  if (document.activeElement && document.activeElement.blur) document.activeElement.blur();
  const card = $('#modal-card');
  card.textContent = '';
  card.className = 'modal-card';
  if (a.type === 'plan') planEditor(a, card);
  else if (a.type === 'budget') budgetPanel(a, card);
  else reviewPanel(a, card);
  $('#modal').hidden = false;
  renderBanner();
}

function approvalTitle(a) {
  if (a.type === 'plan') return 'A plan is waiting for your approval';
  if (a.type === 'budget') return 'Budget reached: ' + a.budget.text + ' - continue?';
  return `Changes of ${a.changes.step_id} are waiting for your review`;
}

// Budget question: go on past the limit (until the task ends) or stop.
function budgetPanel(a, card) {
  const b = a.budget;
  const err = h('div', { class: 'm-err' });
  async function answer(ok) {
    err.textContent = '';
    try {
      $('#modal').dataset.busy = '1';
      const r = await api('POST', `/api/approvals/${a.id}/budget`, { ok });
      toast(r.message, ok ? 'ok' : 'warn');
      closeModal();
    } catch (e) {
      err.textContent = e.message;
    } finally {
      delete $('#modal').dataset.busy;
    }
  }
  card.append(
    h('div', { class: 'm-head' },
      h('div', { class: 'm-kicker' }, h('span', { class: 'pip' }), 'Budget reached'),
      h('div', { class: 'm-title' }, oneLine(a.task, 160)),
      h('div', { class: 'm-sub' }, b.next ? 'Next: ' + b.next : '')),
    h('div', { class: 'm-body' },
      h('p', { class: 'budget-q' }, b.text + '.'),
      h('p', { class: 'muted small' }, 'Continue lets this task run past the limit until it ends. Stop ends it cleanly; finished work stays. To change the limit for good: ' + b.hint + '.')),
    h('div', { class: 'm-foot' }, err, h('span', { class: 'grow' }),
      h('button', { class: 'btn ghost', onclick: hideApproval, title: 'Hide (the question keeps waiting)' }, 'Later'),
      h('button', { class: 'btn danger', onclick: () => answer(false) }, 'Stop the task'),
      h('button', { class: 'btn primary', onclick: () => answer(true), title: 'Ctrl+Enter' }, icon('play'), 'Continue', h('kbd', null, 'Ctrl ↵'))));
  card._submit = () => answer(true);
}

// Plan approval editor.
function planEditor(a, card) {
  const plan = JSON.parse(JSON.stringify(a.plan));
  plan.subtasks = (plan.subtasks || []).map((s) => Object.assign({ files: [], depends_on: [] }, s, { files: s.files || [], depends_on: s.depends_on || [] }));
  const list = h('div', { class: 'steps' });
  const err = h('div', { class: 'm-err' });
  const count = h('span');
  let dragFrom = -1;
  // Dry-run estimate (a.estimate when the orchestrator made one): a chip per
  // step and the total with budget warnings, re-estimated after edits.
  let est = a.estimate || null;
  const estBox = h('div', { class: 'plan-est' });
  const chips = new Map();
  let estTimer = 0;
  function estChip(st) {
    const se = est && (est.steps || []).find((x) => x.step_id === st.id);
    if (!se) return ['', ''];
    let text = `${human(se.tokens.mid)} · ${dur(se.seconds.mid * 1000)}`;
    if (se.usd.high > 0) text += ` · $${se.usd.mid.toFixed(2)}`;
    const src = se.samples ? `${se.source}, ${se.samples} runs` : se.source;
    return [text + (se.source === 'no history' ? ' ?' : ''),
      `${se.role} on ${se.route}: ${human(se.tokens.low)}-${human(se.tokens.high)} tokens, ${dur(se.seconds.low * 1000)}-${dur(se.seconds.high * 1000)}` +
      (se.usd.high > 0 ? `, $${se.usd.low.toFixed(2)}-$${se.usd.high.toFixed(2)}` : '') + ` (median, 25th-75th percentile; ${src})`];
  }
  function renderEstimate() {
    estBox.textContent = '';
    estBox.hidden = !est;
    if (!est) return;
    for (const [id, chip] of chips) {
      const st = plan.subtasks.find((s) => s.id === id);
      const [text, title] = st ? estChip(st) : ['', ''];
      chip.textContent = text;
      chip.title = title;
      chip.hidden = !text;
    }
    const t = est;
    let total = `~${human(t.tokens.mid)} tokens (${human(t.tokens.low)}-${human(t.tokens.high)}) · ~${dur(t.seconds.mid * 1000)} (${dur(t.seconds.low * 1000)}-${dur(t.seconds.high * 1000)})`;
    if (t.usd.high > 0) total += ` · ≈$${t.usd.mid.toFixed(2)} ($${t.usd.low.toFixed(2)}-$${t.usd.high.toFixed(2)}) API-equivalent`;
    const review = (t.steps || []).some((s) => s.step_id === 'review-final');
    estBox.append(h('div', { class: 'est-total' }, h('b', null, review ? 'Estimate incl. final review: ' : 'Estimate: '), total),
      t.no_history ? h('div', { class: 'faint small' }, `${t.no_history} of ${(t.steps || []).length} steps have no history yet: fixed defaults (marked ?).`) : '',
      (t.warnings || []).map((w) => h('div', { class: 'est-warn' }, icon('err'), w)));
  }
  function reestimate() {
    if (!est) return;
    clearTimeout(estTimer);
    estTimer = setTimeout(async () => {
      try {
        est = await api('POST', `/api/approvals/${a.id}/estimate`, { plan });
        renderEstimate();
      } catch (e) { /* the request ended: the modal closes */ }
    }, 250);
  }

  function renderSteps(focusIdx) {
    list.textContent = '';
    chips.clear();
    count.textContent = `${plan.subtasks.length} subtask${plan.subtasks.length === 1 ? '' : 's'} · nothing has run yet`;
    if (!plan.subtasks.length) list.append(h('div', { class: 'empty-list' }, 'No subtasks left - add one, or cancel the task.'));
    plan.subtasks.forEach((st, i) => {
      const color = ROLE_COLORS[st.role || st.kind] || 'var(--muted)';
      const title = h('input', { class: 'in title-in', value: st.title || '', placeholder: 'Title', oninput: (e) => { st.title = e.target.value; } });
      const kind = h('select', { class: 'sel-in', title: 'Kind: explore and research are read-only', onchange: (e) => { st.kind = e.target.value; renderSteps(); reestimate(); } },
        PLAN_KINDS.map((k) => h('option', { value: k, selected: st.kind === k }, k)));
      const role = h('select', { class: 'sel-in', title: 'Pin to a role (auto lets the router decide)', onchange: (e) => { st.role = e.target.value; reestimate(); } },
        PLAN_ROLES.map((r) => h('option', { value: r, selected: (st.role || '') === r }, r ? r.replace('_', ' ') : 'auto (router)')));
      // Multi-repo task: the repo the step works in ('' = the project folder, plan.repos[0]).
      const repo = plan.repos && plan.repos.length ? h('select', { class: 'sel-in', title: 'Repo this subtask works in', onchange: (e) => { st.repo = e.target.value; } },
        plan.repos.map((r, j) => h('option', { value: j ? r : '', selected: (st.repo || '') === (j ? r : '') }, 'in ' + r))) : '';
      const ta = h('textarea', { class: 'ta', rows: '1', placeholder: 'What this agent should do', oninput: (e) => { st.prompt = e.target.value; grow(e.target); } });
      ta.value = st.prompt || '';
      const others = plan.subtasks.filter((o) => o !== st);
      const deps = h('div', { class: 'deps' }, others.length ? 'runs after' : '',
        others.map((o) => {
          const on = st.depends_on.includes(o.id);
          return h('label', { class: 'dep' + (on ? ' on' : '') }, h('input', { type: 'checkbox', checked: on, onchange: (e) => {
            st.depends_on = e.target.checked ? st.depends_on.concat(o.id) : st.depends_on.filter((d) => d !== o.id);
            e.target.parentNode.classList.toggle('on', e.target.checked);
            reestimate();
          } }), o.id);
        }),
        others.length && !st.depends_on.length ? h('span', { class: 'faint' }, '(nothing - starts right away)') : '');
      const tools = h('span', { class: 'tools' },
        h('button', { class: 'btn icon ghost sm', title: 'Move up', disabled: i === 0, onclick: () => move(i, i - 1) }, icon('up')),
        h('button', { class: 'btn icon ghost sm', title: 'Move down', disabled: i === plan.subtasks.length - 1, onclick: () => move(i, i + 1) }, icon('down')),
        h('button', { class: 'btn icon ghost sm danger', title: 'Delete this subtask', onclick: () => del(i) }, icon('trash')));
      const grip = h('div', { class: 'grip', title: 'Drag to reorder' }, icon('grip'), h('span', { class: 'n' }, String(i + 1)));
      const chip = h('span', { class: 'est-chip', hidden: true });
      chips.set(st.id, chip);
      const el = h('div', { class: 'step', style: `--rc:${color}` }, grip,
        h('div', null, h('div', { class: 'step-row' }, h('span', { class: 'step-id' }, st.id), title, kind, role, repo, chip, tools), ta, deps));
      grip.addEventListener('mousedown', () => { el.draggable = true; });
      el.addEventListener('dragstart', (e) => { dragFrom = i; el.classList.add('dragging'); e.dataTransfer.effectAllowed = 'move'; e.dataTransfer.setData('text/plain', String(i)); });
      el.addEventListener('dragend', () => { el.draggable = false; el.classList.remove('dragging'); $$('.step', list).forEach((s) => s.classList.remove('drop-before', 'drop-after')); });
      el.addEventListener('dragover', (e) => {
        if (dragFrom < 0) return;
        e.preventDefault();
        const r = el.getBoundingClientRect();
        const after = e.clientY > r.top + r.height / 2;
        el.classList.toggle('drop-after', after);
        el.classList.toggle('drop-before', !after);
      });
      el.addEventListener('dragleave', () => el.classList.remove('drop-before', 'drop-after'));
      el.addEventListener('drop', (e) => {
        e.preventDefault();
        const r = el.getBoundingClientRect();
        let to = e.clientY > r.top + r.height / 2 ? i + 1 : i;
        if (dragFrom < to) to--;
        const from = dragFrom;
        dragFrom = -1;
        if (from >= 0 && from !== to) move(from, to);
      });
      list.append(el);
      requestAnimationFrame(() => grow(ta));
      if (focusIdx === i) setTimeout(() => title.focus(), 30);
    });
    renderEstimate();
  }
  function grow(ta) { ta.style.height = 'auto'; ta.style.height = Math.min(260, ta.scrollHeight + 2) + 'px'; }
  function move(from, to) {
    if (to < 0 || to >= plan.subtasks.length) return;
    const [x] = plan.subtasks.splice(from, 1);
    plan.subtasks.splice(to, 0, x);
    renderSteps();
    reestimate();
  }
  function del(i) {
    const gone = plan.subtasks[i].id;
    plan.subtasks.splice(i, 1);
    for (const st of plan.subtasks) st.depends_on = st.depends_on.filter((d) => d !== gone);
    renderSteps();
    reestimate();
  }
  function addStep() {
    let n = plan.subtasks.length + 1;
    while (plan.subtasks.some((s) => s.id === 'step-' + n)) n++;
    plan.subtasks.push({ id: 'step-' + n, title: '', kind: 'edit', prompt: '', files: [], depends_on: [], role: '' });
    renderSteps(plan.subtasks.length - 1);
    reestimate();
  }
  async function answer(ok) {
    err.textContent = '';
    if (ok && !plan.subtasks.length) { err.textContent = 'The plan has no subtasks left - add one or cancel the task.'; return; }
    try {
      $('#modal').dataset.busy = '1';
      const r = await api('POST', `/api/approvals/${a.id}/plan`, { ok, plan });
      toast(r.message, ok ? 'ok' : 'warn');
      closeModal();
    } catch (e) {
      err.textContent = e.message;
    } finally {
      delete $('#modal').dataset.busy;
    }
  }
  renderSteps();
  card.append(
    h('div', { class: 'm-head' },
      h('div', { class: 'm-kicker' }, h('span', { class: 'pip' }), 'Approve the plan'),
      h('div', { class: 'm-title' }, oneLine(a.task, 160)),
      h('div', { class: 'm-sub' }, count)),
    h('div', { class: 'm-body' }, plan.summary ? h('div', { class: 'plan-summary' }, plan.summary) : '', estBox, list,
      h('button', { class: 'btn sm', style: 'margin-top:10px', onclick: addStep }, icon('plus'), 'Add subtask')),
    h('div', { class: 'm-foot' }, err, h('span', { class: 'grow' }),
      h('button', { class: 'btn ghost', onclick: hideApproval, title: 'Hide (the plan keeps waiting)' }, 'Later'),
      h('button', { class: 'btn danger', onclick: () => answer(false) }, 'Cancel task'),
      h('button', { class: 'btn primary', onclick: () => answer(true), title: 'Ctrl+Enter' }, icon('play'), 'Approve & run', h('kbd', null, 'Ctrl ↵'))));
  card._submit = () => answer(true);
}

// Change review.
function reviewPanel(a, card) {
  const cs = a.changes;
  card.classList.add('wide');
  const inc = cs.files.map(() => true);
  const hunks = cs.files.map((f) => new Set((f.hunks || []).map((_, i) => i)));
  let sel = 0;
  const filesEl = h('div', { class: 'files' });
  const diffEl = h('div', { class: 'diff-wrap' });
  const applyBtn = h('button', { class: 'btn primary' });
  const fb = h('textarea', { class: 'ta feedback', rows: '1', placeholder: 'Feedback for the agent…', title: 'Sends the agent back to work with this message instead of applying' });
  const fbBtn = h('button', { class: 'btn claude', disabled: true, onclick: () => send({ feedback: fb.value.trim() }) }, icon('msg'), 'Send back');
  const err = h('div', { class: 'm-err' });
  fb.addEventListener('input', () => { fbBtn.disabled = !fb.value.trim(); });

  const fileOn = (i) => inc[i] && (!cs.files[i].splittable || hunks[i].size > 0);
  function renderFiles() {
    filesEl.textContent = '';
    const n = cs.files.filter((_, i) => fileOn(i)).length;
    let add = 0, delc = 0;
    cs.files.forEach((f) => { add += f.added; delc += f.deleted; });
    filesEl.append(h('div', { class: 'files-head' }, h('span', { class: 'grow' }, `${n} of ${cs.files.length} files · `, h('span', { class: 'plus' }, '+' + add), ' ', h('span', { class: 'minus' }, '-' + delc)),
      h('button', { class: 'btn ghost sm', onclick: () => { inc.fill(true); cs.files.forEach((f, i) => { hunks[i] = new Set((f.hunks || []).map((_, j) => j)); }); renderAll(); } }, 'All'),
      h('button', { class: 'btn ghost sm', onclick: () => { inc.fill(false); renderAll(); } }, 'None')));
    cs.files.forEach((f, i) => {
      const partial = f.splittable && inc[i] && hunks[i].size > 0 && hunks[i].size < f.hunks.length;
      const cb = h('input', { type: 'checkbox', checked: fileOn(i), title: 'Apply this file', onclick: (e) => e.stopPropagation(), onchange: (e) => {
        inc[i] = e.target.checked;
        if (inc[i] && f.splittable && !hunks[i].size) hunks[i] = new Set(f.hunks.map((_, j) => j));
        renderAll();
      } });
      cb.indeterminate = partial;
      filesEl.append(h('div', { class: 'file' + (i === sel ? ' sel' : '') + (fileOn(i) ? '' : ' off'), title: f.path, onclick: () => { sel = i; renderAll(); } },
        cb, h('span', { class: 'fs ' + f.status }, f.status), h('span', { class: 'path' }, '‎' + f.path),
        h('span', { class: 'counts' }, f.binary ? h('span', { class: 'muted' }, 'bin') : [h('span', { class: 'plus' }, '+' + f.added), ' ', h('span', { class: 'minus' }, '-' + f.deleted)],
          '')));
    });
    const k = cs.files.filter((_, i) => fileOn(i)).length;
    applyBtn.textContent = '';
    applyBtn.append(icon('check'), k ? `Apply ${k} file${k > 1 ? 's' : ''}` : 'Apply nothing', h('kbd', null, 'Ctrl ↵'));
  }
  function renderDiff() {
    diffEl.textContent = '';
    const f = cs.files[sel];
    if (!f) return;
    const lang = langOf(f.path);
    diffEl.append(h('div', { class: 'diff-head' }, h('span', { class: 'fs ' + f.status }, f.status), f.path,
      f.splittable ? h('span', { class: 'muted small', style: 'margin-left:auto;font-family:var(--font)' }, `${hunks[f.hunks ? sel : 0].size} of ${f.hunks.length} hunks selected`) : ''));
    const body = h('div', { class: 'diff' });
    if (f.binary) body.append(h('div', { class: 'log-empty' }, 'Binary file - no diff to show.'));
    else if (!String(f.patch || '').trim()) body.append(h('div', { class: 'log-empty' }, 'No diff text.'));
    else {
      const parts = splitPatch(f.patch);
      parts.hunks.forEach((hk, j) => {
        const on = !f.splittable || hunks[sel].has(j);
        const head = h('div', { class: 'hunk-h' });
        if (f.splittable) {
          head.append(h('label', null, h('input', { type: 'checkbox', checked: hunks[sel].has(j), onchange: (e) => {
            if (e.target.checked) hunks[sel].add(j); else hunks[sel].delete(j);
            inc[sel] = hunks[sel].size > 0;
            renderAll(true);
          } }), 'hunk ' + (j + 1)));
        }
        head.append(h('span', { class: 'at' }, hk.header));
        const box = h('div', { class: 'hunk' + (on && fileOn(sel) ? '' : ' off') }, head);
        let [o, n] = hunkStart(hk.header);
        for (const line of hk.lines) {
          const c = line[0];
          let cls = 'dl', ln1 = '', ln2 = '';
          if (c === '+') { cls += ' add'; ln2 = n++; }
          else if (c === '-') { cls += ' del'; ln1 = o++; }
          else if (c === '\\') { cls += ' meta'; }
          else { ln1 = o++; ln2 = n++; }
          box.append(h('div', { class: cls }, h('span', { class: 'ln' }, ln1), h('span', { class: 'ln' }, ln2), h('span', { class: 'sg' }, c === ' ' ? '' : c), h('span', { class: 'code' }, highlight(line.slice(1), lang))));
        }
        body.append(box);
      });
      if (!parts.hunks.length) body.append(h('pre', { class: 'raw', style: 'margin:14px' }, f.patch));
    }
    diffEl.append(body);
  }
  function renderAll(keepScroll) {
    const st = diffEl.scrollTop;
    renderFiles();
    renderDiff();
    if (keepScroll) diffEl.scrollTop = st;
  }
  async function send(dec) {
    err.textContent = '';
    try {
      $('#modal').dataset.busy = '1';
      const r = await api('POST', `/api/approvals/${a.id}/changes`, dec);
      toast(cs.step_id + ': ' + r.message, dec.feedback ? 'info' : (dec.apply && dec.apply.length ? 'ok' : 'warn'));
      closeModal();
    } catch (e) {
      err.textContent = e.message;
    } finally {
      delete $('#modal').dataset.busy;
    }
  }
  function applySel() {
    const apply = [], hk = {};
    cs.files.forEach((f, i) => {
      if (!fileOn(i)) return;
      apply.push(f.path);
      if (f.splittable && hunks[i].size < f.hunks.length) hk[f.path] = Array.from(hunks[i]).sort((x, y) => x - y);
    });
    if (!apply.length && !confirm('Nothing is selected. Reject all of ' + cs.step_id + '\'s changes? (They are kept on a branch.)')) return;
    send({ apply, hunks: hk });
  }
  applyBtn.addEventListener('click', applySel);
  renderAll();
  card.append(
    h('div', { class: 'm-head' },
      h('div', { class: 'm-kicker' }, h('span', { class: 'pip' }), 'Review changes',
        cs.round > 1 ? h('span', { style: 'color:var(--warn)' }, ` · round ${cs.round} (after your feedback)`) : ''),
      h('div', { class: 'm-title' }, h('span', { class: 'mono', style: 'color:var(--muted)' }, cs.step_id + '  '), cs.title || ''),
      cs.summary ? h('div', { class: 'm-sub' }, 'agent: ' + cs.summary) : ''),
    h('div', { class: 'm-body', style: 'padding:0' }, h('div', { class: 'review' }, filesEl, diffEl)),
    h('div', { class: 'm-foot' }, h('div', { class: 'grow', style: 'display:flex;gap:8px;align-items:flex-end' }, fb, fbBtn),
      err,
      h('button', { class: 'btn ghost', onclick: hideApproval, title: 'Hide (the review keeps waiting)' }, 'Later'),
      h('button', { class: 'btn danger', onclick: () => { if (confirm('Reject all of ' + cs.step_id + '\'s changes? (They are kept on a branch.)')) send({ apply: [] }); } }, 'Reject all'),
      applyBtn));
  card._submit = applySel;
}

function splitPatch(patch) {
  const lines = String(patch).replace(/\n$/, '').split('\n');
  const out = { meta: [], hunks: [] };
  let cur = null;
  for (const l of lines) {
    if (l.startsWith('@@')) { cur = { header: l, lines: [] }; out.hunks.push(cur); continue; }
    if (!cur) { out.meta.push(l); continue; }
    cur.lines.push(l.length ? l : ' ');
  }
  return out;
}
function hunkStart(header) {
  const m = /^@@ -(\d+)(?:,\d+)? \+(\d+)/.exec(header);
  return m ? [Number(m[1]), Number(m[2])] : [0, 0];
}
const KW = {
  go: 'break case chan const continue default defer else fallthrough for func go goto if import interface map package range return select struct switch type var nil true false err',
  js: 'async await break case catch class const continue default delete do else export extends false finally for from function if import in instanceof let new null return super switch this throw true try typeof undefined var void while yield',
  py: 'and as assert async await break class continue def del elif else except False finally for from global if import in is lambda None nonlocal not or pass raise return self True try while with yield',
  rs: 'as async await break const continue crate else enum extern false fn for if impl in let loop match mod move mut pub ref return self Self static struct super trait true type unsafe use where while',
  c: 'auto break case char const continue default do double else enum extern float for goto if int long register return short signed sizeof static struct switch typedef union unsigned void volatile while class public private protected new delete this namespace template bool true false null',
  sh: 'if then else elif fi for while do done case esac function in return export local echo',
};
const KWSET = {};
for (const k in KW) KWSET[k] = new Set(KW[k].split(' '));
function langOf(path) {
  const ext = (path.split('.').pop() || '').toLowerCase();
  if (ext === 'go') return 'go';
  if (['js', 'mjs', 'cjs', 'ts', 'tsx', 'jsx', 'java', 'kt', 'swift', 'cs', 'dart'].includes(ext)) return 'js';
  if (['py', 'pyi'].includes(ext)) return 'py';
  if (ext === 'rs') return 'rs';
  if (['c', 'h', 'cc', 'cpp', 'hpp'].includes(ext)) return 'c';
  if (['sh', 'bash', 'zsh', 'ps1'].includes(ext)) return 'sh';
  return '';
}
const TOKEN_RE = /(\/\/.*$|#.*$|\/\*.*?\*\/|"(?:[^"\\]|\\.)*"?|'(?:[^'\\]|\\.)*'?|`[^`]*`?|\b\d[\d_.xXa-fA-F]*\b|\b[A-Za-z_][A-Za-z0-9_]*\b)/g;
function highlight(code, lang) {
  if (!lang) return code;
  const frag = document.createDocumentFragment();
  const kws = KWSET[lang];
  let last = 0;
  TOKEN_RE.lastIndex = 0;
  let m;
  while ((m = TOKEN_RE.exec(code))) {
    const t = m[0];
    let cls = '';
    if (t.startsWith('//') || t.startsWith('/*')) cls = lang === 'py' || lang === 'sh' ? '' : 'tk-com';
    else if (t[0] === '#') cls = lang === 'py' || lang === 'sh' ? 'tk-com' : '';
    else if (t[0] === '"' || t[0] === "'" || t[0] === '`') cls = 'tk-str';
    else if (/^\d/.test(t)) cls = 'tk-num';
    else if (kws.has(t)) cls = 'tk-kw';
    else if (code[m.index + t.length] === '(') cls = 'tk-fn';
    if (!cls) continue;
    if (m.index > last) frag.append(code.slice(last, m.index));
    frag.append(h('span', { class: cls }, t));
    last = m.index + t.length;
    if (cls === 'tk-com') { last = code.length; frag.lastChild.textContent = code.slice(m.index); break; }
  }
  if (last < code.length) frag.append(code.slice(last));
  return frag;
}

// ---------- drawers ----------
const DRAWERS = {
  models: { title: 'Models & routes', render: drawModels, wide: true },
  queue: { title: 'Queue', render: drawQueue, narrow: true },
  history: { title: 'History', render: drawHistory },
  stats: { title: 'Stats & tuning', render: drawStats },
  settings: { title: 'Settings', render: drawSettings, narrow: true },
};
function openDrawer(name, refresh) {
  const d = DRAWERS[name];
  if (!d) return;
  if (refresh && S.drawer !== name) return;
  S.drawer = name;
  $('#drawer').classList.toggle('narrow', !!d.narrow);
  $('#drawer').classList.toggle('wide', !!d.wide);
  $('#drawer-title').textContent = d.title;
  $$('.nav').forEach((b) => b.classList.toggle('active', b.dataset.panel === name));
  const body = $('#drawer-body');
  const st = body.scrollTop;
  if (!refresh) body.textContent = '';
  $('#drawer').hidden = false;
  $('#scrim').hidden = false;
  Promise.resolve(d.render(body, refresh)).then(() => { if (refresh) body.scrollTop = st; });
}
function closeDrawer() {
  S.drawer = null;
  $('#drawer').hidden = true;
  $('#scrim').hidden = true;
  $$('.nav').forEach((b) => b.classList.remove('active'));
}

async function drawModels(body) {
  let v;
  try { v = await api('GET', '/api/routes'); } catch (e) { body.textContent = e.message; return; }
  render(v);
  function render(v) {
    body.textContent = '';
    const save = h('button', { class: 'btn ' + (v.dirty ? 'primary' : ''), onclick: async () => { const r = await act('POST', '/api/config/save', {}, 'ok'); if (r) { v.dirty = false; render(v); } } }, v.dirty ? 'Save to config' : 'Saved');
    body.append(h('div', { class: 'toolbar' },
      h('div', { class: 'grow muted small' }, 'Any model for any job. Changes apply to the next agent. ', h('br'), h('code', null, v.path)),
      h('label', { class: 'small muted' }, 'all roles prefer ',
        h('select', { class: 'sel-in', onchange: async (e) => { if (!e.target.value) return; const r = await set({ role: 'all', prefer: e.target.value }); if (r) render(r); } },
          h('option', { value: '' }, '…'), v.prefer_options.map((p) => h('option', { value: p }, p)))),
      save));
    const order = v.provider_order || ['codex', 'claude'];
    const provs = order.filter((p) => S.showAllProviders || !v.providers[p].disabled);
    const hidden = order.length - provs.length;
    const tbl = h('table', { class: 'tbl' });
    tbl.append(h('tr', null, h('th', null, 'Role'), h('th', null, 'Prefer'),
      provs.map((p) => [h('th', null, h('span', { class: 'prov-head', style: `--c:${provColor(p)}` }, h('i'), (v.providers[p].label || p) + ' model' + (v.providers[p].disabled ? ' (off)' : ''))), h('th', null, 'Effort')]),
      h('th', null, 'Now uses')));
    for (const r of v.roles) {
      const prefer = h('select', { class: 'sel-in', onchange: async (e) => { const n = await set({ role: r.role, prefer: e.target.value }); if (n) render(n); } },
        v.prefer_options.map((p) => h('option', { value: p, selected: r.prefer === p }, p)));
      const now = r.now || {};
      tbl.append(h('tr', null,
        h('td', null, h('span', { class: 'rolecell', style: `--rc:${ROLE_COLORS[r.role]}` }, h('i'), r.role.replace('_', ' '))),
        h('td', null, prefer),
        provs.map((p) => [h('td', null, modelSel(v, r, p)), h('td', null, effortSel(v, r, p))]),
        h('td', null, h('span', { class: 'now', style: `--c:${provColor(now.provider)}` }, now.provider ? `${now.provider}:${now.model}${now.effort ? '@' + now.effort : ''}` : '-'),
          now.fallback ? h('div', { class: 'small', style: 'color:var(--warn)' }, 'limit fallback') : '')));
    }
    body.append(tbl);
    if (hidden || S.showAllProviders) {
      body.append(h('label', { class: 'small muted', style: 'display:flex;gap:6px;align-items:center;margin-top:8px' },
        h('input', { type: 'checkbox', checked: !!S.showAllProviders, onchange: (e) => { S.showAllProviders = e.target.checked; render(v); } }),
        `show disabled providers (${order.filter((p) => v.providers[p].disabled).join(', ')}) - turn one on with disabled: false in the config`));
    }
    body.append(h('h3', null, 'How routing works'),
      h('div', { class: 'muted small', style: 'display:grid;gap:6px' },
        h('div', null, h('b', null, 'prefer'), ': a provider = that one · other = not the planner\'s (good for review) · auto = whichever has used fewer tokens this session.'),
        h('div', null, 'At a usage limit the role\'s route on the next provider (routing.provider_order) is used automatically.'),
        h('div', null, 'Tip: ', h('code', null, 'sy models --refresh'), ' reads the current Codex catalog from ', h('code', null, 'codex debug models'), '.')));
  }
  function modelSel(v, r, prov) {
    const cur = ((r.routes || r)[prov] || {}).model || '';
    const cat = v.providers[prov].models || [];
    const opts = cat.map((m) => h('option', { value: m.id, selected: m.id === cur }, m.id + (m.tier ? '  · ' + m.tier : '')));
    if (cur && !cat.some((m) => m.id === cur)) opts.unshift(h('option', { value: cur, selected: true }, cur + '  · custom'));
    opts.push(h('option', { value: '__custom' }, 'custom…'), h('option', { value: '', selected: !cur }, '(none)'));
    const sel = h('select', { class: 'sel-in select-' + prov, style: `border-color: color-mix(in srgb, ${provColor(prov)} 30%, var(--border))`, onchange: async (e) => {
      let val = e.target.value;
      if (val === '__custom') {
        val = window.prompt(`Model id for ${r.role} on ${prov} (any id the CLI accepts):`, cur);
        if (val == null) { e.target.value = cur; return; }
      }
      const n = await set({ role: r.role, provider: prov, model: val.trim() });
      if (n) render(n); else e.target.value = cur;
    } }, opts);
    return sel;
  }
  function effortSel(v, r, prov) {
    const cur = ((r.routes || r)[prov] || {}).effort || '';
    return h('select', { class: 'sel-in select-' + prov, style: `border-color: color-mix(in srgb, ${provColor(prov)} 30%, var(--border))`, onchange: async (e) => { const n = await set({ role: r.role, provider: prov, effort: e.target.value }); if (n) render(n); else e.target.value = cur; } },
      h('option', { value: '', selected: !cur }, 'default'), (v.providers[prov].efforts || []).map((x) => h('option', { value: x, selected: x === cur }, x)));
  }
  async function set(body) {
    try { return await api('POST', '/api/routes', body); } catch (e) { toast(e.message, 'error'); return null; }
  }
}

function drawQueue(body) {
  const q = (S.snap && S.snap.queue) || [];
  const ae = document.activeElement;
  const refocus = ae && body.contains(ae) && ae.dataset && ae.dataset.sched ? ae.dataset.sched : '';
  body.textContent = '';
  body.append(h('div', { class: 'toolbar' }, h('div', { class: 'grow muted small' }, 'Tasks typed while one runs wait here and run one after another, unattended (no approvals). Scheduled tasks start at their time, after any running task.'),
    q.length ? h('button', { class: 'btn sm danger', onclick: () => act('POST', '/api/queue/clear', {}, 'warn') }, 'Clear all') : ''));
  // The drawer re-renders on every state update: keep what is typed.
  const sf = S.sched || (S.sched = { when: '', what: '' });
  const when = h('input', { class: 'in when', placeholder: '02:30 · in 2h · reset claude', value: sf.when, dataset: { sched: 'when' }, title: 'When: HH:MM (today or tomorrow), "2026-10-04 02:30", in 2h, or reset <provider>|any (when that usage limit resets)' });
  const what = h('input', { class: 'in what', placeholder: 'Task to run then…', value: sf.what, dataset: { sched: 'what' } });
  for (const [el, k] of [[when, 'when'], [what, 'what']]) el.addEventListener('input', () => { sf[k] = el.value; });
  const go = async () => {
    if (!sf.when.trim() || !sf.what.trim()) { toast('Give a time and a task', 'warn'); return; }
    try {
      const r = await api('POST', '/api/schedule', { when: sf.when, text: sf.what });
      toast(r.message, 'ok');
      sf.what = '';
      what.value = '';
    } catch (e) { toast(e.message, 'err'); }
  };
  what.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); go(); } });
  body.append(h('div', { class: 'sched-form' }, when, what, h('button', { class: 'btn sm primary', onclick: go }, 'Schedule')));
  if (refocus) {
    const el = refocus === 'when' ? when : what;
    el.focus();
    el.setSelectionRange(el.value.length, el.value.length);
  }
  if (!q.length) { body.append(h('div', { class: 'empty-list' }, 'The queue is empty.')); return; }
  const list = h('div', { class: 'list' });
  q.forEach((j, i) => list.append(h('div', { class: 'item', style: `animation-delay:${i * 30}ms` },
    h('div', { class: 't' }, j.label),
    h('div', { class: 'meta' }, h('span', { class: 'pill' }, j.at ? 'scheduled' : j.kind),
      j.at ? 'at ' + new Date(Date.parse(j.at) + S.timeOffset).toLocaleString([], { weekday: 'short', hour: '2-digit', minute: '2-digit' }) : '#' + (i + 1) + ' in line'),
    h('div', { class: 'side' }, h('button', { class: 'btn sm', onclick: () => act('POST', '/api/queue/remove', { id: j.id }) }, icon('trash'), 'Remove')))));
  body.append(list);
}

async function drawHistory(body) {
  const all = store.get('sy-hist-all') === '1';
  let rows;
  try { rows = await api('GET', '/api/history' + (all ? '?all=1' : '')); } catch (e) { body.textContent = e.message; return; }
  body.textContent = '';
  body.append(h('div', { class: 'toolbar' },
    h('div', { class: 'grow muted small' }, 'Every task saves its plan and finished steps, so an interrupted one can continue where it stopped.'),
    h('label', { class: 'small muted', style: 'display:flex;gap:6px;align-items:center' }, h('input', { type: 'checkbox', checked: all, onchange: (e) => { store.set('sy-hist-all', e.target.checked ? '1' : '0'); drawHistory(body); } }), 'all projects')));
  if (!rows.length) { body.append(h('div', { class: 'empty-list' }, S.snap && S.snap.demo ? 'Demo runs keep no history.' : 'No tasks recorded for this folder yet.')); return; }
  const running = S.snap && S.snap.running;
  const list = h('div', { class: 'list' });
  rows.forEach((t, i) => {
    const canResume = t.status === 'interrupted' || t.status === 'failed' || t.status === 'cancelled';
    list.append(h('div', { class: 'item', style: `animation-delay:${Math.min(i, 12) * 25}ms` },
      h('div', { class: 't', title: t.task }, t.task),
      h('div', { class: 'meta' }, h('span', { class: 'pill ' + t.status }, t.status), when(t.created),
        t.steps ? `${t.done}/${t.steps} steps` : '', t.cost ? h('span', null, t.cost) : '', !t.here ? h('code', { class: 'small' }, t.dir) : '',
        h('code', { class: 'faint small' }, t.id)),
      h('div', { class: 'side' }, canResume ? h('button', { class: 'btn sm ' + (t.status === 'interrupted' ? 'primary' : ''), disabled: running,
        title: running ? 'A task is running' : (t.status === 'interrupted' ? 'Continue: finished steps are skipped' : 'Run the steps that did not succeed'),
        onclick: async () => { const r = await act('POST', '/api/resume', { id: t.id }, 'ok'); if (r) closeDrawer(); } }, icon('play'), t.status === 'interrupted' ? 'Resume' : 'Retry unfinished') : '')));
  });
  body.append(list);
}

async function drawStats(body) {
  const here = store.get('sy-stats-here') === '1';
  const since = store.get('sy-stats-since') || '7d';
  let v;
  try { v = await api('GET', `/api/stats?since=${encodeURIComponent(since)}${here ? '&here=1' : ''}`); } catch (e) { body.textContent = e.message; return; }
  body.textContent = '';
  body.append(h('div', { class: 'toolbar' },
    h('div', { class: 'grow' }, h('select', { class: 'sel-in', onchange: (e) => { store.set('sy-stats-since', e.target.value); drawStats(body); } },
      [['24h', 'last 24 hours'], ['7d', 'last 7 days'], ['30d', 'last 30 days'], ['all', 'all time']].map(([k, l]) => h('option', { value: k, selected: since === k }, l)))),
    h('label', { class: 'small muted', style: 'display:flex;gap:6px;align-items:center' }, h('input', { type: 'checkbox', checked: here, onchange: (e) => { store.set('sy-stats-here', e.target.checked ? '1' : '0'); drawStats(body); } }), 'this project only')));
  const st = v.stats;
  let ok = 0;
  (st.Modes || []).forEach((m) => { ok += m.OK; });
  body.append(h('div', { class: 'kpis' },
    kpi(v.tasks, 'tasks'), kpi(v.tasks ? Math.round(ok / v.tasks * 100) + '%' : '-', 'succeeded'),
    ...(() => {
      const ps = provOrder(v.totals).filter((p) => v.totals[p]);
      const used = ps.length ? ps : ['codex', 'claude'];
      return [kpi(used.map((p) => human(v.totals[p] || 0)).join(' / '), used.join(' / ') + ' tokens')];
    })(), kpi(v.usd ? '$' + v.usd.toFixed(2) : '-', 'API-equivalent')));
  // Days.
  const days = st.Days || [];
  if (days.length) {
    const per = (d) => d.Providers || { codex: d.Codex, claude: d.Claude };
    const total = (d) => Object.values(per(d)).reduce((a, b) => a + (b || 0), 0);
    const maxT = Math.max(1, ...days.map(total));
    const seen = {};
    days.forEach((d) => Object.keys(per(d)).forEach((p) => { seen[p] = 1; }));
    const ps = provOrder(Object.keys(seen).length ? seen : { codex: 1, claude: 1 });
    body.append(h('h3', null, 'Per day'), h('div', { class: 'legend' }, ps.map((p) => h('span', null, h('i', { style: `background:${provColor(p)}` }), p))));
    const box = h('div', { class: 'days' });
    for (const d of days) {
      box.append(h('div', { class: 'day' }, h('span', { class: 'muted mono' }, d.Date),
        h('div', { class: 'bars', title: ps.map((p) => `${p} ${human(per(d)[p] || 0)}`).join(' · ') },
          ps.map((p) => h('span', { style: `width:${(per(d)[p] || 0) / maxT * 100}%;background:${provColor(p)}` }))),
        h('span', { class: 'muted small', style: 'text-align:right' }, `${d.OK}/${d.Tasks} ok${d.USD ? ' · $' + d.USD.toFixed(2) : ''}`)));
    }
    body.append(box);
  }
  // Suggestions.
  body.append(h('h3', null, 'Routing suggestions'));
  if (!v.suggestions.length) {
    body.append(h('div', { class: 'empty-list' }, v.tasks < v.min_tasks ? `Only ${v.tasks} task(s) logged - suggestions need about ${v.min_tasks}.` : `No suggestions from ${v.tasks} tasks: the current routing looks fine.`));
  } else {
    if (v.tasks < v.min_tasks) body.append(h('div', { class: 'muted small', style: 'margin-bottom:8px' }, `Few tasks logged - treat these as hints until ~${v.min_tasks} tasks.`));
    const list = h('div', { class: 'list' });
    for (const s of v.suggestions) {
      list.append(h('div', { class: 'sug ' + s.Severity }, h('div', { class: 't' }, s.Title), h('div', { class: 'd' }, s.Detail),
        (s.Commands || []).length ? h('div', { class: 'cmds' }, s.Commands.map((c) => [h('code', null, c), applyable(c) ? h('button', { class: 'btn sm', onclick: () => applyCmd(c) }, 'Apply') : ''])) : ''));
    }
    body.append(list);
  }
  // Routes.
  if ((st.Routes || []).length) {
    body.append(h('h3', null, 'Usage per model'));
    const tbl = h('table', { class: 'tbl' }, h('tr', null, ['Route', 'Calls', 'OK', 'Fail', 'Limit', 'Fresh in', 'Out', 'Avg'].map((x, i) => h('th', { class: i ? 'num' : '' }, x))));
    for (const r of st.Routes) {
      tbl.append(h('tr', null, h('td', null, h('span', { class: 'now', style: `--c:var(--${r.Provider})` }, r.Key)),
        h('td', { class: 'num' }, r.Calls), h('td', { class: 'num' }, r.OK), h('td', { class: 'num' }, r.Failed), h('td', { class: 'num' }, r.LimitHits),
        h('td', { class: 'num' }, human((r.Tokens.input || 0) - (r.Tokens.cached || 0))), h('td', { class: 'num' }, human(r.Tokens.output)),
        h('td', { class: 'num' }, r.Calls ? dur(r.Duration / 1e6 / r.Calls) : '-')));
    }
    body.append(tbl);
  }
  body.append(h('details', null, h('summary', null, 'Full report (sy stats)'), h('pre', { class: 'raw' }, v.text)),
    h('div', { class: 'muted small', style: 'margin-top:10px' }, 'logs: ', h('code', null, v.log_dir)));
  function kpi(val, label) { return h('div', { class: 'kpi' }, h('div', { class: 'v' }, String(val)), h('div', { class: 'l' }, label)); }
}
function applyable(c) { return /^\/(route|prefer) \S+ \S+$/.test(c) || /^\/(judge|review|parallel) (on|off)$/.test(c); }
async function applyCmd(c) {
  const f = c.split(/\s+/);
  if (f[0] === '/route') {
    const [prov, model, effort] = f[2].split(':');
    await act('POST', '/api/routes', { role: f[1], provider: prov, model, effort: effort || '' }, 'ok');
  } else if (f[0] === '/prefer') {
    await act('POST', '/api/routes', { role: f[1], prefer: f[2] }, 'ok');
  } else {
    await act('POST', '/api/settings', { [f[0].slice(1)]: f[1] === 'on' }, 'ok');
  }
  toast('Applied for this session - save in Models to keep it', 'info');
}

function drawSettings(body) {
  const s = S.snap;
  if (!s) return;
  const st = s.settings;
  body.textContent = '';
  const toggle = (key, title, desc) => h('div', { class: 'setting' }, h('div', { class: 'txt' }, h('b', null, title), h('span', null, desc)),
    h('label', { class: 'switch' }, h('input', { type: 'checkbox', checked: !!st[key], onchange: (e) => act('POST', '/api/settings', { [key]: e.target.checked }) }), h('span')));
  body.append(h('h3', null, 'Checks before work lands'),
    toggle('approve_plan', 'Approve the plan', 'Show the plan for editing before any agent runs.'),
    toggle('review_changes', 'Review changes', 'Show each agent\'s diff (per file and hunk) before it lands in your tree.'),
    toggle('review', 'Reviewer checkpoints', 'A reviewer checks the plan, repeated errors and the final diff.'),
    h('h3', null, 'Agents'),
    toggle('parallel', 'Run in parallel', 'Independent subtasks run at the same time (in git worktrees when they write).'),
    h('div', { class: 'setting' }, h('div', { class: 'txt' }, h('b', null, 'Max parallel agents'), h('span', null, 'Upper bound; the load gate can hold agents on a busy machine.')),
      h('input', { class: 'in', type: 'number', min: '1', max: '64', value: String(st.max_threads), style: 'width:80px', onchange: (e) => act('POST', '/api/settings', { max_threads: Number(e.target.value) }) })),
    toggle('judge', 'LLM judge', 'Ask a small model when the routing rules are unsure.'));
  // Verify commands.
  const cmds = st.verify.slice();
  const vbox = h('div', { class: 'verify' });
  const renderV = () => {
    vbox.textContent = '';
    cmds.forEach((c, i) => vbox.append(h('div', { class: 'verify-row' },
      h('input', { class: 'in', value: c, onchange: (e) => { cmds[i] = e.target.value; saveV(); } }),
      h('button', { class: 'btn icon sm', title: 'Remove', onclick: () => { cmds.splice(i, 1); saveV(); renderV(); } }, icon('trash')))));
    const add = h('input', { class: 'in', placeholder: 'e.g. go test ./...', onkeydown: (e) => { if (e.key === 'Enter' && add.value.trim()) { cmds.push(add.value.trim()); saveV(); renderV(); } } });
    vbox.append(h('div', { class: 'verify-row' }, add, h('button', { class: 'btn sm', onclick: () => { if (add.value.trim()) { cmds.push(add.value.trim()); saveV(); renderV(); } } }, icon('plus'), 'Add')));
  };
  const saveV = () => act('POST', '/api/settings', { verify: cmds });
  renderV();
  body.append(h('h3', null, 'Verify commands'), h('div', { class: 'muted small', style: 'margin-bottom:8px' }, 'Your repo\'s checks: agents may run them, and sy runs them before the final review.'), vbox);
  // Notifications & look.
  const perm = 'Notification' in window ? Notification.permission : 'unsupported';
  body.append(h('h3', null, 'Notifications & look'),
    h('div', { class: 'setting' }, h('div', { class: 'txt' }, h('b', null, 'Browser notifications'), h('span', null, perm === 'granted' ? 'On: you are told when sy needs you or a task ends while this tab is in the background.' : perm === 'denied' ? 'Blocked in the browser settings for this page.' : 'Get told when sy needs an approval or a task ends.')),
      perm === 'default' ? h('button', { class: 'btn sm', onclick: () => Notification.requestPermission().then(() => drawSettings(body)) }, icon('bell'), 'Enable') : h('span', { class: 'muted small' }, perm)),
    toggle('notify', 'Desktop notifications when no page is open', 'sy itself notifies (Windows toast, macOS, notify-send) when it needs you or a long task ends.'),
    h('div', { class: 'setting' }, h('div', { class: 'txt' }, h('b', null, 'Theme')),
      h('select', { class: 'sel-in', onchange: (e) => setTheme(e.target.value) }, ['system', 'dark', 'light'].map((t) => h('option', { value: t, selected: (store.get('sy-theme') || 'system') === t }, t)))));
  // Providers.
  body.append(h('h3', null, 'Providers'));
  for (const p of provOrder(s.providers)) {
    const d = s.providers[p] || {};
    const limited = d.limited_until && Date.parse(d.limited_until) > Date.now() - S.timeOffset;
    body.append(h('div', { class: 'setting' }, h('div', { class: 'txt' }, h('b', { style: `color:${provColor(p)}` }, p),
      h('span', null, d.disabled ? 'disabled (providers.' + p + '.disabled in the config)' : `${d.calls || 0} calls · ${human(d.fresh)} fresh tokens · ${d.limit_hits || 0} limit hits` + (limited ? ' · at its limit' : ''))),
      d.disabled ? '' : limited ? h('button', { class: 'btn sm', onclick: () => act('POST', '/api/limit', { provider: p, action: 'reset' }) }, 'Mark available')
        : h('button', { class: 'btn sm ghost', onclick: () => act('POST', '/api/limit', { provider: p, action: 'set' }) }, 'Mark at limit')));
  }
  if (s.demo) {
    body.append(h('h3', null, 'Demo'), h('div', { class: 'setting' }, h('div', { class: 'txt' }, h('b', null, 'Sample change review'), h('span', null, 'Demo runs have no git repo, so try the review panel with a sample diff.')),
      h('button', { class: 'btn sm', onclick: () => { closeDrawer(); act('POST', '/api/demo/review', {}); } }, 'Open')));
  }
  body.append(h('h3', null, 'About'), h('div', { class: 'kv' },
    h('div', null, 'project'), h('div', null, h('code', null, s.dir)),
    h('div', null, 'config'), h('div', null, h('code', null, s.config_path), s.dirty ? h('span', { style: 'color:var(--warn)' }, ' · unsaved changes ') : '',
      s.dirty ? h('button', { class: 'btn sm', onclick: () => act('POST', '/api/config/save', {}, 'ok') }, 'Save') : ''),
    s.session_log ? [h('div', null, 'session log'), h('div', null, h('code', null, s.session_log))] : '',
    h('div', null, 'version'), h('div', null, s.version || 'dev')));
}

// ---------- theme & notifications ----------
function setTheme(t) {
  store.set('sy-theme', t === 'system' ? '' : t);
  if (t === 'system') document.documentElement.removeAttribute('data-theme');
  else document.documentElement.setAttribute('data-theme', t);
}
function currentTheme() {
  const a = document.documentElement.getAttribute('data-theme');
  if (a) return a;
  return window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark';
}
function askNotifyPermission() {
  if (!('Notification' in window) || Notification.permission !== 'default' || store.get('sy-notify-asked')) return;
  store.set('sy-notify-asked', '1');
  try { Notification.requestPermission(); } catch (e) { /* old browsers */ }
}
function notify(title, body) {
  if (!('Notification' in window) || Notification.permission !== 'granted') return;
  if (!document.hidden && document.hasFocus()) return;
  try {
    const n = new Notification(title, { body, tag: 'switchyard-' + title });
    n.onclick = () => { window.focus(); n.close(); };
  } catch (e) { /* ignore */ }
}

// ---------- wiring ----------
function wire() {
  $('#btn-theme').addEventListener('click', () => setTheme(currentTheme() === 'dark' ? 'light' : 'dark'));
  $('#btn-pause').addEventListener('click', () => act('POST', '/api/pause', { paused: !(S.snap && S.snap.paused) }));
  $('#btn-cancel').addEventListener('click', () => act('POST', '/api/cancel', {}, 'warn'));
  $$('.nav').forEach((b) => b.addEventListener('click', () => (S.drawer === b.dataset.panel ? closeDrawer() : openDrawer(b.dataset.panel))));
  $('#drawer-close').addEventListener('click', closeDrawer);
  $('#scrim').addEventListener('click', closeDrawer);
  $$('#filters .chip').forEach((c) => c.addEventListener('click', () => setFilter(c.dataset.filter)));
  let st;
  $('#search').addEventListener('input', (e) => { clearTimeout(st); st = setTimeout(() => { S.search = e.target.value.trim().toLowerCase(); renderLog(); }, 120); });
  const log = $('#log');
  log.addEventListener('scroll', () => { S.follow = nearBottom(log); if (S.follow) $('#jump').hidden = true; });
  $('#jump').addEventListener('click', () => { log.scrollTop = log.scrollHeight; S.follow = true; $('#jump').hidden = true; });
  $('#btn-send').addEventListener('click', submit);
  const p = prompt();
  p.addEventListener('input', () => { autosize(); updateAC(); });
  p.addEventListener('keydown', (e) => {
    if (AC.open) {
      if (e.key === 'ArrowDown') { e.preventDefault(); AC.sel = (AC.sel + 1) % AC.items.length; updateAC(); return; }
      if (e.key === 'ArrowUp') { e.preventDefault(); AC.sel = (AC.sel - 1 + AC.items.length) % AC.items.length; updateAC(); return; }
      if (e.key === 'Tab' || (e.key === 'Enter' && !e.shiftKey && !e.ctrlKey)) { e.preventDefault(); acceptAC(); return; }
      if (e.key === 'Escape') { e.preventDefault(); hideAC(); return; }
    } else if (e.key === 'Tab' && /^@[a-z0-9_-]*$/.test(p.value)) {
      e.preventDefault(); updateAC(); return;
    }
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); submit(); }
  });
  p.addEventListener('blur', () => setTimeout(hideAC, 150));
  document.addEventListener('keydown', (e) => {
    const inField = e.target.closest && e.target.closest('input, textarea, select');
    if (e.key === 'Escape') {
      if (AC.open) return hideAC();
      if (!$('#modal').hidden) { e.preventDefault(); return hideApproval(); }
      if (S.drawer) { e.preventDefault(); return closeDrawer(); }
      if (S.agentFilter) return selectAgent(S.agentFilter);
    }
    if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') {
      const card = $('#modal-card');
      if (!$('#modal').hidden && card._submit) { e.preventDefault(); if (Date.now() - (S.modalOpenedAt || 0) > 800) card._submit(); return; }
      if (e.target === p) { e.preventDefault(); return submit(); }
    }
    if (e.ctrlKey && !e.metaKey && !e.altKey && (e.key === 'x' || e.key === 'X')) {
      const sel = e.target.selectionStart !== undefined && e.target.selectionStart !== e.target.selectionEnd;
      if (inField && sel) return; // a normal cut
      if (S.snap && S.snap.running) { e.preventDefault(); act('POST', '/api/cancel', {}, 'warn'); }
    }
    if (e.key === '/' && !inField && $('#modal').hidden) { e.preventDefault(); p.focus(); }
  });
  window.addEventListener('focus', () => { S.dirtyGraph = true; });
  setInterval(() => {
    tickElapsed();
    for (const [k, c] of cards) {
      const id = k.startsWith('kid:') ? k.slice(4) : k;
      const n = id === 'reviewer' ? S.reviewer : id === 'judge' ? S.judge : id === 'main' ? mainView() : S.nodes.get(id);
      if (n && n.status === 'running') updateElapsed(c, n);
    }
  }, 1000);
  const loop = () => { if (!S.replaying) renderGraph(); requestAnimationFrame(loop); };
  requestAnimationFrame(loop);
}

resetTree();
wire();
renderLog();
renderGraph(true);
setFilter(S.filter);
startSession().then((ok) => { if (ok) { connect(); p0(); } });
function p0() { prompt().focus(); autosize(); }
})();
