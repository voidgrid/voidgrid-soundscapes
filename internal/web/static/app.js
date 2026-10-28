'use strict';

// State comes from the server over /api/events (server-sent events) and is shown as it arrives. Changes go
// back as small requests (one layer at a time), so two browsers adjusting different layers do not
// overwrite each other.

const $ = (id) => document.getElementById(id);
const el = (tag, props = {}, ...kids) => {
  const e = Object.assign(document.createElement(tag), props);
  e.append(...kids);
  return e;
};

const state = { layers: [], soundscapes: [], loaded: false };

const SEND_EVERY = 80;   // ms between slider updates while dragging
const HOLD = 1500;       // ms a control keeps the value the user set before the server's value is shown again
const touched = new Map();   // layer id -> time of the last local edit
let rowSignature = '';
let rows = new Map();        // layer id -> row elements

const player = $('player'), listenBtn = $('listen');

// ---- talking to the server ----------------------------------------------------------------------

async function api(method, path, body) {
  const res = await fetch(path, {
    method,
    headers: body === undefined ? {} : { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (res.status === 204) return null;
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || `${res.status} ${res.statusText}`);
  return data;
}

let toastTimer;
function toast(msg) {
  const t = $('toast');
  t.textContent = msg;
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.hidden = true; }, 5000);
}

// Apply an answer from the server straight away, without waiting for the next event.
function adopt(view) {
  const i = state.soundscapes.findIndex((s) => s.id === view.id);
  if (i >= 0) state.soundscapes[i] = view; else state.soundscapes.push(view);
  render();
}

function connect() {
  const es = new EventSource('api/events');
  es.addEventListener('state', (e) => {
    const data = JSON.parse(e.data);
    state.layers = data.layers;
    state.soundscapes = data.soundscapes;
    state.loaded = true;
    setLive(true);
    render();
  });
  es.onerror = () => setLive(false);
}

function setLive(on) {
  for (const l of document.querySelectorAll('.live')) {
    l.textContent = on ? 'Live' : 'Reconnecting…';
    l.className = on ? 'live on' : 'live';
  }
}

// ---- helpers --------------------------------------------------------------------------------------

const pct = (level) => `${Math.round(level * 100)}%`;
const streamURL = (id) => `${location.origin}/stream/${encodeURIComponent(id)}`;
const plural = (n, one, many) => `${n} ${n === 1 ? one : many}`;

// #/ is the list of soundscapes; #/s/<id> is one soundscape's mixer. Back and forward work, and so do bookmarks.
function route() {
  const m = location.hash.match(/^#\/s\/(.+)$/);
  return m ? decodeURIComponent(m[1]) : null;
}
const navigate = (id) => { location.hash = id ? `#/s/${encodeURIComponent(id)}` : '#/'; };
const current = () => state.soundscapes.find((s) => s.id === route()) || null;

// ---- rendering --------------------------------------------------------------------------------------

function render() {
  const id = route();
  if (id && state.loaded && !current()) { navigate(null); return; }   // deleted, or a bad link
  $('listView').hidden = !!id;
  $('mixView').hidden = !id;
  if (id) {
    if (state.loaded) { renderMixer(current()); }
  } else {
    renderCards();
  }
  syncListen();
}

function renderCards() {
  const cards = $('cards');
  if (!state.loaded) { cards.replaceChildren(); return; }
  if (!state.soundscapes.length) {
    cards.replaceChildren(el('p', { className: 'empty', textContent: 'No soundscapes yet. A soundscape is a named mix of your sounds, with its own stream URL. Make one to start.' }));
    return;
  }
  cards.replaceChildren(...state.soundscapes.map((s) => {
    const on = s.layers.filter((l) => l.enabled).length;
    const meta = el('span', { textContent: `${plural(s.layers.length, 'sound', 'sounds')}, ${on} on` });
    const kids = [el('strong', { textContent: s.name }), meta];
    if (s.listeners) kids.push(el('span', { className: 'on-air', textContent: ` · ${s.listeners} listening` }));
    return el('a', { className: 'card', href: `#/s/${encodeURIComponent(s.id)}` }, ...kids);
  }));
}

function renderMixer(s) {
  $('title').textContent = s.name;
  document.title = `${s.name} · Soundscapes`;

  // stream URL and listeners
  const info = $('info');
  if (info.dataset.id !== s.id) {
    info.dataset.id = s.id;
    const url = el('input', { type: 'text', readOnly: true, value: streamURL(s.id) });
    url.setAttribute('aria-label', 'Stream URL');
    url.addEventListener('focus', () => url.select());
    const copy = el('button', { type: 'button', textContent: 'Copy URL' });
    copy.addEventListener('click', async () => {
      try { await navigator.clipboard.writeText(url.value); } catch (_) { url.select(); document.execCommand('copy'); }
      copy.textContent = 'Copied';
      setTimeout(() => { copy.textContent = 'Copy URL'; }, 1200);
    });
    info.replaceChildren(url, copy, el('span', { id: 'listeners' }));
  }
  $('listeners').textContent = s.listeners === 0 ? 'nobody listening' : `${s.listeners} listening`;

  // Only the sounds in this soundscape get a row, in the order they were added.
  const byId = new Map(state.layers.map((l) => [l.id, l]));
  const sig = JSON.stringify([s.id, s.layers.map((l) => { const lib = byId.get(l.id); return [l.id, lib ? lib.name : null, lib ? lib.category : null, lib ? lib.error || '' : null]; })]);
  if (sig !== rowSignature) {
    rowSignature = sig;
    rows = new Map();
    $('layers').replaceChildren(...s.layers.map((l) => buildRow(l.id, byId.get(l.id))));
  }
  for (const spec of s.layers) { const r = rows.get(spec.id); if (r) updateRow(r, spec); }
  renderAdd(s, byId);
}

// The Add menu lists the sounds that are in the library and not yet in this soundscape.
function renderAdd(s, byId) {
  const used = new Set(s.layers.map((l) => l.id));
  const free = state.layers.filter((l) => !used.has(l.id) && !l.error);
  const sel = $('add');
  const sig = JSON.stringify(free.map((l) => [l.id, l.name, l.category]));
  if (sel.dataset.sig === sig) return;
  sel.dataset.sig = sig;
  const groups = new Map();
  for (const l of free) {
    const g = l.category || 'other';
    if (!groups.has(g)) groups.set(g, []);
    groups.get(g).push(l);
  }
  const names = [...groups.keys()].sort((a, b) => (a === 'other' ? 1 : b === 'other' ? -1 : a.localeCompare(b)));
  let hint = '+ Add a sound…';
  if (!free.length) hint = state.layers.length ? 'Every sound is already in this soundscape' : 'No sounds found in the audio folder';
  sel.replaceChildren(el('option', { value: '', textContent: hint }),
    ...names.map((g) => el('optgroup', { label: g },
      ...groups.get(g).sort((a, b) => a.name.localeCompare(b.name)).map((l) => el('option', { value: l.id, textContent: l.name })))));
  sel.value = '';
  sel.disabled = !free.length;
}

function buildRow(id, layer) {
  const root = el('div', { className: 'layer' });
  const r = { root, id, layer };
  const rm = el('button', { type: 'button', className: 'remove', textContent: '✕' });
  rm.setAttribute('aria-label', `Remove ${layer ? layer.name : id} from this soundscape`);
  rm.title = 'Remove from this soundscape';
  rm.addEventListener('click', () => patch({ id, remove: true }));
  const note = el('span', { className: 'note', hidden: true });
  r.note = note;
  rows.set(id, r);

  if (!layer) {   // saved in the soundscape but no longer in the audio folder
    r.missing = true;
    root.append(el('span'), el('span', { className: 'name', textContent: id, title: id }), el('span'), rm, note);
    note.textContent = 'This file is no longer in the audio folder.';
    note.className = 'note problem';
    note.hidden = false;
    root.classList.add('bad');
    return root;
  }

  const name = el('span', { className: 'name', textContent: layer.name, title: layer.id });
  if (layer.category) name.append(el('small', { textContent: layer.category }));
  const toggle = el('input', { type: 'checkbox' });
  toggle.setAttribute('aria-label', `${layer.name} on or off`);
  const slider = el('input', { type: 'range', min: 0, max: 100, step: 1, className: 'slider' });
  slider.setAttribute('aria-label', `${layer.name} level`);
  const label = el('span', { className: 'pct' });
  Object.assign(r, { toggle, slider, label });

  toggle.addEventListener('change', () => {
    touched.set(id, Date.now());
    patch({ id, enabled: toggle.checked });
  });
  let timer = null;
  slider.addEventListener('input', () => {
    touched.set(id, Date.now());
    label.textContent = `${slider.value}%`;
    if (timer) return;
    timer = setTimeout(() => {          // at most one request per SEND_EVERY while dragging; the last value always goes
      timer = null;
      patch({ id, level: slider.value / 100 });
    }, SEND_EVERY);
  });

  root.append(el('label', { className: 'toggle' }, toggle, el('span')), name, label, rm, slider, note);
  if (layer.error) root.classList.add('bad');
  return root;
}

function updateRow(r, spec) {
  if (r.missing) return;
  const bad = r.layer.error;
  const holding = Date.now() - (touched.get(r.id) || 0) < HOLD || (document.activeElement === r.slider && r.slider.matches(':active'));
  if (!holding) {
    r.toggle.checked = spec.enabled;
    r.slider.value = Math.round(spec.level * 100);
    r.label.textContent = `${r.slider.value}%`;
  }
  r.toggle.disabled = r.slider.disabled = !!bad;
  let text = '', kind = '';
  if (bad) { text = `Unusable: ${bad}`; kind = 'problem'; }
  else if (spec.enabled) {
    if (spec.state === 'loading') { text = 'Loading… silent until ready'; kind = 'loading'; }
    else if (spec.state === 'error') { text = `Could not load: ${spec.error || 'unknown error'}`; kind = 'problem'; }
    else if (spec.state === 'unusable' || spec.state === 'missing') { text = spec.error || spec.state; kind = 'problem'; }
  }
  r.note.textContent = text;
  r.note.className = `note ${kind}`;
  r.note.hidden = !text;
}

// ---- changing things --------------------------------------------------------------------------------

async function patch(body) {
  const s = current();
  if (!s) return;
  try {
    adopt(await api('PATCH', `api/soundscapes/${encodeURIComponent(s.id)}/layers`, body));
  } catch (err) {
    touched.delete(body.id);
    toast(err.message);
  }
}

function ask({ title, text = '', value = null, ok = 'OK', danger = false }) {
  const dlg = $('dialog'), input = $('dialogInput'), form = $('dialogForm');
  $('dialogTitle').textContent = title;
  $('dialogText').textContent = text;
  $('dialogText').hidden = !text;
  input.hidden = value === null;
  input.value = value ?? '';
  $('dialogOk').textContent = ok;
  $('dialogOk').className = danger ? 'danger' : 'primary';
  return new Promise((resolve) => {
    const done = (result) => { dlg.close(); cleanup(); resolve(result); };
    const submit = (e) => { e.preventDefault(); done(value === null ? true : input.value.trim()); };
    const cancel = () => done(null);
    const cleanup = () => { form.removeEventListener('submit', submit); $('dialogCancel').removeEventListener('click', cancel); dlg.removeEventListener('cancel', cancel); };
    form.addEventListener('submit', submit);
    $('dialogCancel').addEventListener('click', cancel);
    dlg.addEventListener('cancel', cancel);
    dlg.showModal();
    if (value !== null) { input.focus(); input.select(); }
  });
}

async function createSoundscape() {
  const name = await ask({ title: 'New soundscape', value: '', ok: 'Create' });
  if (!name) return;
  try {
    const v = await api('POST', 'api/soundscapes', { name });
    adopt(v); navigate(v.id);
  } catch (err) { toast(err.message); }
}

$('new').addEventListener('click', createSoundscape);

$('rename').addEventListener('click', async () => {
  const s = current();
  if (!s) return;
  const name = await ask({ title: 'Rename soundscape', text: 'The stream URL does not change.', value: s.name, ok: 'Rename' });
  if (!name || name === s.name) return;
  try { adopt(await api('PUT', `api/soundscapes/${encodeURIComponent(s.id)}`, { name })); } catch (err) { toast(err.message); }
});

$('duplicate').addEventListener('click', async () => {
  const s = current();
  if (!s) return;
  const name = await ask({ title: 'Duplicate soundscape', value: `${s.name} copy`, ok: 'Duplicate' });
  if (!name) return;
  try {
    const v = await api('POST', `api/soundscapes/${encodeURIComponent(s.id)}/duplicate`, { name });
    adopt(v); navigate(v.id);
  } catch (err) { toast(err.message); }
});

$('delete').addEventListener('click', async () => {
  const s = current();
  if (!s) return;
  const extra = s.listeners ? ` ${s.listeners} listening will be disconnected.` : '';
  if (!await ask({ title: `Delete “${s.name}”?`, text: `Its mix and its stream URL are removed.${extra}`, ok: 'Delete', danger: true })) return;
  try {
    await api('DELETE', `api/soundscapes/${encodeURIComponent(s.id)}`);
    state.soundscapes = state.soundscapes.filter((x) => x.id !== s.id);
    navigate(null);
  } catch (err) { toast(err.message); }
});

// Choosing a sound in the Add menu puts it in the soundscape, switched on at half level.
$('add').addEventListener('change', (e) => {
  const id = e.target.value;
  e.target.value = '';
  if (id) patch({ id, enabled: true });
});

window.addEventListener('hashchange', () => { rowSignature = ''; render(); });

// ---- listening in this browser --------------------------------------------------------------------

let listening = null;   // soundscape id being played here, or null

function syncListen() {
  if (listening && (!current() || current().id !== listening)) stopListening();   // left the soundscape, or it was deleted
  listenBtn.textContent = listening ? 'Stop listening' : 'Listen';
  listenBtn.setAttribute('aria-pressed', String(!!listening));
}

function startListening() {
  const s = current();
  if (!s) return;
  listening = s.id;
  player.src = `stream/${encodeURIComponent(s.id)}`;
  player.play().catch((err) => { toast(`Cannot play: ${err.message}`); stopListening(); syncListen(); });
}

function stopListening() {
  listening = null;
  player.pause();
  player.removeAttribute('src');
  player.load();   // drops the connection, so the server stops counting this browser as a listener
}

listenBtn.addEventListener('click', () => { if (listening) stopListening(); else startListening(); syncListen(); });
player.addEventListener('error', () => { if (listening) { toast('The stream stopped.'); stopListening(); syncListen(); } });

connect();
