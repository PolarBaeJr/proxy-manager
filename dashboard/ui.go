package main

const dashboardHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Pi Dashboard</title>
<style>
  :root {
    --bg:        #0a0d12;
    --surface:   #11161d;
    --surface-2: #161c25;
    --border:    #232a35;
    --border-2:  #2e3744;
    --text:      #e6edf3;
    --muted:     #8b95a4;
    --accent:    #58a6ff;
    --accent-2:  #3b82f6;
    --green:     #3fb950;
    --green-bg:  rgba(63,185,80,.12);
    --red:       #f85149;
    --red-bg:    rgba(248,81,73,.12);
    --yellow:    #f0c849;
    --yellow-bg: rgba(240,200,73,.13);
    --orange:    #f0883e;
    --radius:    10px;
    --shadow:    0 1px 0 rgba(255,255,255,.02) inset, 0 8px 32px -12px rgba(0,0,0,.6);
  }
  * { box-sizing: border-box; }
  html, body { background: var(--bg); }
  body {
    font: 14px/1.5 -apple-system, BlinkMacSystemFont, "Inter", "Segoe UI", system-ui, sans-serif;
    max-width: 1200px; margin: 0 auto; padding: 1.2em 1.6em;
    color: var(--text); -webkit-font-smoothing: antialiased;
  }
  h1 {
    margin: 0; font-size: 18px; font-weight: 600; letter-spacing: -.01em;
    background: linear-gradient(180deg, #c5d3e0 0%, #58a6ff 120%);
    -webkit-background-clip: text; background-clip: text; color: transparent;
  }
  h2 { color: var(--text); margin: 0 0 .4em; font-size: 16px; font-weight: 600; letter-spacing: -.005em; }
  .meta { color: var(--muted); font-size: 12px; }
  a { color: var(--accent); text-decoration: none; transition: color .12s; }
  a:hover { color: #82baff; }

  header {
    display: flex; justify-content: space-between; align-items: center;
    border-bottom: 1px solid var(--border); padding-bottom: .9em; margin-bottom: 1.5em;
    gap: 1em; flex-wrap: wrap;
  }
  .header-left  { display: flex; align-items: center; gap: 1.2em; }
  .header-right { display: flex; gap: .9em; align-items: center; }

  .sysstat { display: inline-flex; align-items: center; gap: 6px; color: var(--text); font-size: 12px; }
  .sysstat .ico { color: var(--muted); display: inline-flex; }
  .sysstat .ico svg { width: 14px; height: 14px; }
  .sysstat .label { color: var(--muted); font-weight: 500; letter-spacing: .03em; }
  .sysstat .val { font-variant-numeric: tabular-nums; }
  .sysstat .bar {
    display: inline-block; width: 56px; height: 4px;
    background: var(--border); border-radius: 999px; overflow: hidden; margin-left: 2px;
  }
  .sysstat .bar > span {
    display: block; height: 100%;
    background: linear-gradient(90deg, var(--accent), #82baff);
    transition: width .4s ease;
  }
  .sysstat.warn .bar > span { background: linear-gradient(90deg, var(--yellow), #fde68a); }
  .sysstat.bad  .bar > span { background: linear-gradient(90deg, var(--red),    #fca5a5); }

  nav {
    display: inline-flex; gap: 2px; margin-bottom: 1.5em;
    background: var(--surface); padding: 4px; border-radius: var(--radius);
    border: 1px solid var(--border);
  }
  nav button {
    background: transparent; border: none; color: var(--muted);
    padding: 7px 14px; border-radius: 7px; cursor: pointer; font: inherit; font-size: 13px;
    font-weight: 500; transition: all .12s;
  }
  nav button:hover { color: var(--text); background: var(--surface-2); }
  nav button.active { background: var(--accent-2); color: white; }

  .card {
    background: var(--surface); border: 1px solid var(--border); border-radius: var(--radius);
    padding: 1.1em 1.3em; margin-bottom: 14px; overflow-x: auto;
    box-shadow: var(--shadow); transition: border-color .15s;
  }
  .card:hover { border-color: var(--border-2); }
  .card-head { display: flex; justify-content: space-between; align-items: center; gap: 1em; }
  .card-head h2 { flex: 1; display: flex; align-items: center; gap: .5em; flex-wrap: wrap; }
  .card td code { white-space: nowrap; }

  table { width: 100%; border-collapse: collapse; margin-top: .4em; }
  th, td { padding: 8px 10px; text-align: left; border-bottom: 1px solid var(--border); vertical-align: middle; }
  tr:last-child > td { border-bottom: none; }
  th {
    color: var(--muted); font-weight: 500; font-size: 10.5px;
    text-transform: uppercase; letter-spacing: .08em;
  }
  code {
    background: var(--bg); padding: 2px 7px; border-radius: 5px;
    color: var(--orange); font-size: 12.5px; font-family: ui-monospace, "SF Mono", Menlo, monospace;
    border: 1px solid var(--border); word-break: break-all;
  }

  .pill {
    display: inline-flex; align-items: center; padding: 2px 9px; border-radius: 999px;
    font-size: 10.5px; font-weight: 600; letter-spacing: .02em; line-height: 1.6;
    border: 1px solid transparent;
  }
  .pill.ok    { background: var(--green-bg);  color: var(--green);  border-color: rgba(63,185,80,.3); }
  .pill.bad   { background: var(--red-bg);    color: var(--red);    border-color: rgba(248,81,73,.3); }
  .pill.warn  { background: var(--yellow-bg); color: var(--yellow); border-color: rgba(240,200,73,.3); }
  .pill.muted { background: var(--surface-2); color: var(--muted);  border-color: var(--border); }
  .tag { background: rgba(59,130,246,.15); color: #82baff; padding: 1px 7px; border-radius: 5px; font-size: 10.5px; font-weight: 600; border: 1px solid rgba(59,130,246,.3); }

  .btn {
    background: var(--surface-2); border: 1px solid var(--border-2); color: var(--text);
    padding: 6px 13px; border-radius: 6px; cursor: pointer; font: inherit; font-size: 12.5px;
    font-weight: 500; transition: all .12s; line-height: 1.3;
  }
  .btn:hover { background: #1f2733; border-color: #3b4554; }
  .btn:active { transform: translateY(1px); }
  .btn.primary { background: var(--accent-2); border-color: var(--accent-2); color: white; }
  .btn.primary:hover { background: #2563eb; border-color: #2563eb; }
  .btn.danger { background: var(--surface-2); color: var(--red); border-color: var(--border-2); }
  .btn.danger:hover { background: var(--red); color: white; border-color: var(--red); }
  .btn:disabled { opacity: .45; cursor: not-allowed; }
  .btn:disabled:hover { background: var(--surface-2); border-color: var(--border-2); }

  .btn-row { display: flex; gap: 6px; align-items: center; }
  .replica-ctrl { display: inline-flex; align-items: center; gap: 6px; }
  .replica-ctrl input {
    width: 56px; background: var(--bg); border: 1px solid var(--border); color: var(--text);
    padding: 4px 6px; border-radius: 5px; text-align: center; font: inherit;
    font-variant-numeric: tabular-nums;
  }

  dialog {
    background: var(--surface); color: var(--text);
    border: 1px solid var(--border-2); border-radius: 14px;
    padding: 1.8em 1.6em; min-width: 440px; max-width: 90vw;
    box-shadow: 0 24px 70px -20px rgba(0,0,0,.7);
  }
  dialog::backdrop { background: rgba(5,8,12,.7); backdrop-filter: blur(4px); }
  dialog h2 { margin-top: 0; font-size: 17px; }
  .field { margin-bottom: 1em; }
  .field label { display: block; color: var(--muted); font-size: 11.5px; margin-bottom: 5px; font-weight: 500; letter-spacing: .02em; }
  .field input, .field select {
    width: 100%; background: var(--bg); border: 1px solid var(--border);
    color: var(--text); padding: 8px 11px; border-radius: 7px; font: inherit;
    transition: border-color .12s, box-shadow .12s;
  }
  .field input:focus, .field select:focus {
    outline: none; border-color: var(--accent-2);
    box-shadow: 0 0 0 3px rgba(59,130,246,.15);
  }
  .dialog-actions { display: flex; justify-content: flex-end; gap: 8px; margin-top: 1.3em; }

  .err   { color: #fca5a5; font-size: 11.5px; font-family: ui-monospace, monospace; }
  .empty { color: var(--muted); font-style: italic; padding: 2em 0; text-align: center; }

  .toast {
    position: fixed; top: 1.2em; right: 1.2em; padding: 11px 18px;
    border-radius: 8px; font-size: 13px; z-index: 100; font-weight: 500;
    border: 1px solid; box-shadow: 0 8px 30px -8px rgba(0,0,0,.6);
    animation: toastIn .18s ease-out;
  }
  @keyframes toastIn { from { transform: translateY(-8px); opacity: 0 } to { transform: none; opacity: 1 } }
  .toast.ok  { background: var(--green-bg);  color: var(--green);  border-color: rgba(63,185,80,.3); }
  .toast.err { background: var(--red-bg);    color: var(--red);    border-color: rgba(248,81,73,.3); }

  .totp-secret {
    background: var(--bg); padding: 1em; border-radius: 8px; border: 1px solid var(--border);
    font-family: ui-monospace, "SF Mono", Menlo, monospace; font-size: 15px;
    text-align: center; letter-spacing: 2px; margin: .8em 0; word-break: break-all;
  }
  .otp-input { font-size: 24px !important; text-align: center; letter-spacing: 8px; font-family: ui-monospace, monospace; }
  .full-card { max-width: 520px; margin: 6em auto; }
  .note {
    background: var(--yellow-bg); border: 1px solid rgba(240,200,73,.25); border-radius: 8px;
    padding: .7em 1em; margin: .9em 0; font-size: 12.5px; color: var(--text);
  }
  .note strong { color: var(--yellow); }

  details > summary { list-style: none; }
  details > summary::-webkit-details-marker { display: none; }
  details > summary::before { content: '▸ '; display: inline-block; transition: transform .12s; }
  details[open] > summary::before { transform: rotate(90deg); }
</style>
</head>
<body>

<div id="auth-screen" hidden>
  <header><h1>Pi Dashboard</h1></header>
  <div class="card full-card"><div id="auth-content"></div></div>
</div>

<div id="main-screen" hidden>
  <header>
    <div class="header-left">
      <h1>Pi Dashboard</h1>
      <div id="sys-stats" class="meta"></div>
    </div>
    <div class="header-right">
      <span class="meta" id="status">loading…</span>
      <span class="meta">user: <strong id="who">?</strong></span>
      <button class="btn" onclick="logout()">Logout</button>
    </div>
  </header>

  <nav>
    <button data-tab="routes" class="active">Routes</button>
    <button data-tab="services">Services</button>
    <button data-tab="dns">DNS</button>
    <button data-tab="users">Users</button>
    <button data-tab="stats">Stats</button>
  </nav>

  <section id="tab-routes"></section>
  <section id="tab-services" hidden></section>
  <section id="tab-dns" hidden></section>
  <section id="tab-users" hidden></section>
  <section id="tab-stats" hidden></section>
</div>

<dialog id="dlg-new-service">
  <h2>Create service</h2>
  <form id="form-new-service">
    <div class="field"><label>Name (unique)</label><input name="name" required pattern="[a-z0-9-]+" placeholder="myapp"></div>
    <div class="field"><label>Image</label><input name="image" required placeholder="nginx:latest"></div>
    <div class="field"><label>Hostname</label><input name="host" required placeholder="myapp.pi.local"></div>
    <div class="field"><label>Internal port</label><input name="port" type="number" required placeholder="80"></div>
    <div class="field"><label>Replicas</label><input name="replicas" type="number" value="1" min="1"></div>
    <div class="field"><label><input type="checkbox" name="unscalable"> Singleton (database, bot, etc — disables scale buttons)</label></div>
    <div class="field"><label>Env (KEY=VALUE per line)</label><input name="env" placeholder="DATABASE_URL=postgres://..."></div>
    <div class="dialog-actions">
      <button type="button" class="btn" onclick="document.getElementById('dlg-new-service').close()">Cancel</button>
      <button type="submit" class="btn primary">Create</button>
    </div>
  </form>
</dialog>

<dialog id="dlg-new-dns">
  <h2>New DNS record</h2>
  <form id="form-new-dns">
    <div class="field"><label>Type</label><select name="type"><option>CNAME</option><option>A</option><option>TXT</option></select></div>
    <div class="field"><label>Name</label><input name="name" required placeholder="myapp"></div>
    <div class="field"><label>Content (target)</label><input name="content" required placeholder="tunnel.example.com"></div>
    <div class="field"><label><input type="checkbox" name="proxied" checked> Proxy through Cloudflare</label></div>
    <div class="dialog-actions">
      <button type="button" class="btn" onclick="document.getElementById('dlg-new-dns').close()">Cancel</button>
      <button type="submit" class="btn primary">Create</button>
    </div>
  </form>
</dialog>

<dialog id="dlg-replace-service">
  <h2>Replace service</h2>
  <p class="meta">Spins up new replicas with a new image, waits, then removes the old. Env is copied from the existing template unless you override it.</p>
  <form id="form-replace-service">
    <input type="hidden" name="serviceName">
    <div class="field"><label>Current image</label><input name="currentImage" disabled></div>
    <div class="field"><label>New image</label><input name="image" required placeholder="nginx:1.27"></div>
    <div class="field"><label>Env override (KEY=VALUE per line — leave blank to keep current)</label><input name="env"></div>
    <div class="dialog-actions">
      <button type="button" class="btn" onclick="document.getElementById('dlg-replace-service').close()">Cancel</button>
      <button type="submit" class="btn primary">Replace</button>
    </div>
  </form>
</dialog>

<dialog id="dlg-2fa">
  <h2>Confirm with 2FA</h2>
  <p class="meta">Enter your current 6-digit code to unlock edits for 5 minutes.</p>
  <form id="form-2fa">
    <div class="field"><input name="code" inputmode="numeric" pattern="[0-9]{6}" maxlength="6" required class="otp-input" autocomplete="one-time-code"></div>
    <div class="dialog-actions">
      <button type="button" class="btn" onclick="document.getElementById('dlg-2fa').close()">Cancel</button>
      <button type="submit" class="btn primary">Confirm</button>
    </div>
  </form>
</dialog>

<!-- Add-user modal (two-phase: form → confirm code) -->
<dialog id="dlg-add-user">
  <h2>Add user</h2>
  <div id="add-user-body"></div>
</dialog>

<script>
const $ = (s, r=document) => r.querySelector(s);
const $$ = (s, r=document) => [...r.querySelectorAll(s)];
const fmtTime = () => new Date().toLocaleTimeString();

let authState = { setup_complete: false, authenticated: false, elevated_until: 0, username: '', now: 0 };
let pendingActive = false; // true while a TOTP-confirm screen is on-screen; freezes auto-refresh

function isElevated() { return authState.authenticated && authState.elevated_until > (Date.now() / 1000); }

function toast(msg, kind='ok') {
  const t = document.createElement('div');
  t.className = 'toast ' + kind;
  t.textContent = msg;
  document.body.appendChild(t);
  setTimeout(() => t.remove(), 3500);
}

async function api(path, opts={}) {
  const r = await fetch(path, { headers: {'Content-Type':'application/json'}, ...opts });
  if (r.status === 401) { await refreshAuth(); throw new Error('Session expired — sign in again.'); }
  if (r.status === 403) { needs2FA(); throw new Error('2FA required'); }
  if (!r.ok) {
    const txt = await r.text();
    let msg = txt;
    try { msg = JSON.parse(txt).error || txt; } catch {}
    throw new Error(msg);
  }
  return r.status === 204 ? null : r.json();
}

// ---- Auth bootstrap ----
async function refreshAuth() {
  const r = await fetch('/api/auth/status');
  const next = await r.json();
  // If a pending-confirm screen is on display, just update state silently —
  // don't re-render and wipe out the QR + input the user is in the middle of.
  if (pendingActive && !next.setup_complete) { authState = next; return; }
  authState = next;
  renderAuthOrMain();
}
function renderAuthOrMain() {
  if (!authState.setup_complete) { showAuth(setupView()); return; }
  if (!authState.authenticated)   { showAuth(loginView()); return; }
  $('#auth-screen').hidden = true;
  $('#main-screen').hidden = false;
  $('#who').textContent = authState.username;
  renderActive();
}
function showAuth(html) {
  $('#auth-screen').hidden = false;
  $('#main-screen').hidden = true;
  $('#auth-content').innerHTML = html;
  wireAuthForms();
}

function setupView() {
  return ` + "`" + `
    <h2>First-time setup</h2>
    <p class="meta">Pick a username and password. You'll then receive a 2FA secret and need to verify a code before the user is saved.</p>
    <form id="form-setup">
      <div class="field"><label>Username</label><input name="username" required pattern="[a-zA-Z0-9._-]{2,32}" autofocus></div>
      <div class="field"><label>Password (8+ chars)</label><input name="password" type="password" minlength="8" required></div>
      <div class="dialog-actions"><button class="btn primary" type="submit">Begin setup</button></div>
    </form>
    <div id="setup-result"></div>
  ` + "`" + `;
}

function loginView() {
  return ` + "`" + `
    <h2>Sign in</h2>
    <form id="form-login">
      <div class="field"><label>Username</label><input name="username" required autofocus></div>
      <div class="field"><label>Password</label><input name="password" type="password" required></div>
      <div class="field"><label>2FA code (optional — required for edits)</label><input name="code" inputmode="numeric" pattern="[0-9]{6}" maxlength="6" class="otp-input" placeholder="123456"></div>
      <div class="dialog-actions"><button class="btn primary" type="submit">Sign in</button></div>
    </form>
  ` + "`" + `;
}

function wireAuthForms() {
  const setup = $('#form-setup');
  if (setup) setup.onsubmit = async (e) => {
    e.preventDefault();
    try {
      const out = await api('/api/auth/setup', { method:'POST', body: JSON.stringify({
        username: setup.username.value.trim(), password: setup.password.value,
      })});
      pendingActive = true;
      $('#setup-result').innerHTML = pendingUserBlock(out, 'setup');
      wirePendingConfirm('setup', out.username);
    } catch (e) { toast(e.message, 'err'); }
  };

  const login = $('#form-login');
  if (login) login.onsubmit = async (e) => {
    e.preventDefault();
    try {
      await api('/api/auth/login', { method:'POST', body: JSON.stringify({
        username: login.username.value.trim(), password: login.password.value, code: login.code.value || '',
      })});
      await refreshAuth();
    } catch (e) { toast(e.message, 'err'); }
  };
}

// Shared pending-user block used by both setup view and add-user dialog.
function pendingUserBlock(p, kind) {
  return ` + "`" + `
    <div class="note">
      <strong>⚠ Not saved yet.</strong> Add this to an authenticator app, then verify a code below.
      Pending for ${kind === 'setup' ? '10 minutes' : 'the new user'} — closing this window cancels it.
    </div>
    <p class="meta">Account: <code>${esc(p.username)}</code></p>
    ${p.qr_data_url ? '<div style="text-align:center;margin:.6em 0"><img src="' + p.qr_data_url + '" alt="Scan this with your authenticator app" style="width:220px;height:220px;background:white;padding:10px;border-radius:8px"></div>' : ''}
    <p style="text-align:center"><a href="${esc(p.otpauth_uri)}" target="_blank">Or open in authenticator app →</a></p>
    <details style="margin-top:.5em"><summary class="meta" style="cursor:pointer">Can't scan? Type the secret manually</summary>
      <div class="totp-secret">${esc(p.totp_secret)}</div>
    </details>
    <form id="form-confirm-${kind}" style="margin-top: 1em">
      <div class="field"><label>Enter current 6-digit code</label><input name="code" inputmode="numeric" pattern="[0-9]{6}" maxlength="6" required class="otp-input" autocomplete="one-time-code"></div>
      <div class="dialog-actions">
        <button class="btn primary" type="submit">Verify & save user</button>
      </div>
    </form>
  ` + "`" + `;
}

function wirePendingConfirm(kind, username) {
  const f = $('#form-confirm-' + kind);
  if (!f) return;
  f.onsubmit = async (e) => {
    e.preventDefault();
    const path = kind === 'setup' ? '/api/auth/setup/confirm' : '/api/users/confirm';
    try {
      await api(path, { method:'POST', body: JSON.stringify({ username, code: f.code.value.trim() })});
      toast('User ' + username + ' confirmed and saved.');
      pendingActive = false;
      if (kind === 'setup') {
        await refreshAuth();
      } else {
        $('#dlg-add-user').close();
        renderActive();
      }
    } catch (e) { toast(e.message, 'err'); }
  };
}

async function logout() {
  await fetch('/api/auth/logout', { method:'POST' });
  await refreshAuth();
}

// ---- 2FA modal ----
function needs2FA() { document.getElementById('dlg-2fa').showModal(); }
function promptElevate() { document.getElementById('dlg-2fa').showModal(); }
$('#form-2fa').onsubmit = async (e) => {
  e.preventDefault();
  try {
    await api('/api/auth/verify-2fa', { method:'POST', body: JSON.stringify({ code: e.target.code.value.trim() })});
    toast('Unlocked for 5 minutes');
    document.getElementById('dlg-2fa').close();
    e.target.reset();
    await refreshAuth();
  } catch (e) { toast(e.message, 'err'); }
};

// ---- Tabs ----
const TABS = ['routes', 'services', 'dns', 'users', 'stats'];
let activeTab = 'routes';
$$('nav button').forEach(b => b.onclick = () => switchTab(b.dataset.tab));
function switchTab(t) {
  activeTab = t;
  $$('nav button').forEach(b => b.classList.toggle('active', b.dataset.tab === t));
  TABS.forEach(x => $('#tab-' + x).hidden = x !== t);
  renderActive();
}

async function renderActive() {
  if (!authState.authenticated) return;
  try {
    if (activeTab === 'routes') await renderRoutes();
    else if (activeTab === 'services') await renderServices();
    else if (activeTab === 'dns') await renderDNS();
    else if (activeTab === 'users') await renderUsers();
    else if (activeTab === 'stats') await renderStats();
    $('#status').textContent = 'updated ' + fmtTime();
  } catch (e) {
    $('#status').textContent = 'error: ' + e.message;
  }
}

function lockedAttr() { return isElevated() ? '' : 'disabled title="Confirm 2FA to enable"'; }

// ---- Routes ----
async function renderRoutes() {
  const groups = await api('/api/routes');
  const el = $('#tab-routes');
  if (!groups.length) { el.innerHTML = '<p class="empty">No routes registered.</p>'; return; }
  let html = '';
  for (const g of groups) {
    html += '<div class="card"><div class="card-head"><h2><code>' + esc(g.host) + '</code>';
    if (g.path) html += ' <code>' + esc(g.path) + '</code>' + (g.strip ? ' <span class="tag">strip</span>' : '');
    html += '</h2><span class="meta">' + g.backends.length + ' backend(s)' + (g.service ? ' · service: ' + esc(g.service) : '') + '</span></div>';
    html += '<table><tr><th>Health</th><th>Backend</th><th>Weight</th><th>Container</th><th>Last error</th></tr>';
    for (const b of g.backends) {
      const healthPill = b.healthy === true  ? '<span class="pill ok">healthy</span>'
                       : b.healthy === false ? '<span class="pill bad">down</span>'
                       : '<span class="pill muted">—</span>';
      html += '<tr><td>' + healthPill + '</td>';
      html += '<td><code>' + esc(b.url) + '</code></td><td>' + b.weight + '</td><td>' + esc(b.container || '') + '</td>';
      html += '<td class="err">' + esc(b.last_error || '') + '</td></tr>';
    }
    html += '</table></div>';
  }
  el.innerHTML = html;
}

// ---- Services ----
async function renderServices() {
  const svcs = await api('/api/services');
  const el = $('#tab-services');
  let html = '<div class="btn-row" style="margin-bottom:1em"><button class="btn primary" ' + lockedAttr() + ' onclick="document.getElementById(\'dlg-new-service\').showModal()">+ New service</button></div>';
  if (!svcs.length) html += '<p class="empty">No managed services yet.</p>';
  for (const s of svcs) {
    html += '<div class="card"><div class="card-head"><h2>' + esc(s.name);
    if (s.update_available) html += ' <span class="pill warn" title="Newer image available in registry">↑ update available</span>';
    if (s.canary_image)     html += ' <span class="pill" style="background:#1f6feb;color:white">canary live</span>';
    html += '</h2></div>';
    html += '<table><tr><th>Image</th><td><code>' + esc(s.image) + '</code></td></tr>';
    if (s.canary_image) {
      html += '<tr><th>Canary</th><td><code>' + esc(s.canary_image) + '</code> · ' + s.canary_replicas + ' replica(s) (traffic round-robins both)</td></tr>';
    }
    if (s.previous_image && !s.canary_image) {
      html += '<tr><th>Previous</th><td><code>' + esc(s.previous_image) + '</code></td></tr>';
    }
    html += '<tr><th>Host</th><td><code>' + esc(s.host) + '</code>' + (s.path ? ' <code>' + esc(s.path) + '</code>' : '') + '</td></tr>';
    html += '<tr><th>Port</th><td>' + s.port + '</td></tr>';
    const scaleDisabled = s.unscalable ? 'disabled title="Singleton — replica count locked at 1"' : lockedAttr();
    html += '<tr><th>Replicas</th><td><span class="replica-ctrl">';
    html += '<button class="btn" ' + scaleDisabled + ' onclick="scaleSvc(\'' + esc(s.name) + '\', ' + (s.replicas - 1) + ')">−</button>';
    html += '<input type="number" min="0" value="' + s.replicas + '" id="rep-' + esc(s.name) + '"' + (s.unscalable ? ' disabled' : '') + '>';
    html += '<button class="btn" ' + scaleDisabled + ' onclick="scaleSvc(\'' + esc(s.name) + '\', ' + (s.replicas + 1) + ')">+</button>';
    html += '<button class="btn" ' + scaleDisabled + ' onclick="scaleSvc(\'' + esc(s.name) + '\', +document.getElementById(\'rep-' + esc(s.name) + '\').value)">Apply</button>';
    if (s.unscalable) html += ' <span class="pill warn">singleton</span>';
    html += '</span></td></tr></table>';
    html += '<div class="btn-row" style="margin-top:.8em;justify-content:flex-end;gap:.5em;flex-wrap:wrap">';
    if (s.canary_image) {
      html += '<button class="btn primary" ' + lockedAttr() + ' onclick="promoteCanary(\'' + esc(s.name) + '\')">Promote canary</button>';
      html += '<button class="btn" ' + lockedAttr() + ' onclick="discardCanary(\'' + esc(s.name) + '\')">Discard canary</button>';
    } else {
      html += '<button class="btn" ' + lockedAttr() + ' onclick="openStage(\'' + esc(s.name) + '\', \'' + esc(s.image) + '\')">Stage new version</button>';
      html += '<button class="btn" ' + lockedAttr() + ' onclick="openReplace(\'' + esc(s.name) + '\', \'' + esc(s.image) + '\')">Replace</button>';
      if (s.previous_image) {
        html += '<button class="btn" ' + lockedAttr() + ' onclick="rollback(\'' + esc(s.name) + '\', \'' + esc(s.previous_image) + '\')" title="One-click rollback">Rollback to ' + esc(s.previous_image) + '</button>';
      }
    }
    html += '<button class="btn danger" ' + lockedAttr() + ' onclick="deleteSvc(\'' + esc(s.name) + '\')">Delete service</button>';
    html += '</div></div>';
  }
  el.innerHTML = html;
}

async function scaleSvc(name, n) {
  if (n < 0) return;
  try {
    await api('/api/services/' + encodeURIComponent(name) + '/scale', { method:'POST', body: JSON.stringify({replicas: n}) });
    toast('scaled ' + name + ' → ' + n);
    renderActive();
  } catch (e) { toast(e.message, 'err'); }
}
function openReplace(name, currentImage) {
  const f = $('#form-replace-service');
  f.serviceName.value = name;
  f.currentImage.value = currentImage;
  f.image.value = '';
  f.env.value = '';
  f.dataset.mode = 'replace';
  $('#dlg-replace-service').querySelector('h2').textContent = 'Replace service';
  $('#dlg-replace-service').showModal();
}

function openStage(name, currentImage) {
  const f = $('#form-replace-service');
  f.serviceName.value = name;
  f.currentImage.value = currentImage;
  f.image.value = '';
  f.env.value = '';
  f.dataset.mode = 'stage';
  $('#dlg-replace-service').querySelector('h2').textContent = 'Stage new version (canary)';
  $('#dlg-replace-service').showModal();
}

async function promoteCanary(name) {
  if (!confirm('Promote canary to live? Old replicas will be removed.')) return;
  try { await api('/api/services/' + encodeURIComponent(name) + '/promote', { method:'POST' });
    toast('promoted ' + name); renderActive();
  } catch (e) { toast(e.message, 'err'); }
}
async function discardCanary(name) {
  if (!confirm('Discard canary? Live continues unchanged.')) return;
  try { await api('/api/services/' + encodeURIComponent(name) + '/canary', { method:'DELETE' });
    toast('discarded canary for ' + name); renderActive();
  } catch (e) { toast(e.message, 'err'); }
}
async function rollback(name, prevImage) {
  if (!confirm('Replace ' + name + ' with ' + prevImage + '?')) return;
  try { await api('/api/services/' + encodeURIComponent(name) + '/replace', {
      method:'POST', body: JSON.stringify({ image: prevImage }),
    });
    toast('rolled back ' + name); renderActive();
  } catch (e) { toast(e.message, 'err'); }
}

$('#form-replace-service').onsubmit = async (e) => {
  e.preventDefault();
  const f = e.target;
  const env = {};
  if (f.env.value.trim()) {
    for (const line of f.env.value.split('\n')) {
      const [k, ...rest] = line.split('=');
      if (k && rest.length) env[k.trim()] = rest.join('=').trim();
    }
  }
  const mode = f.dataset.mode === 'stage' ? 'stage' : 'replace';
  try {
    await api('/api/services/' + encodeURIComponent(f.serviceName.value) + '/' + mode, {
      method: 'POST',
      body: JSON.stringify({ image: f.image.value, env: Object.keys(env).length ? env : null }),
    });
    toast((mode === 'stage' ? 'staged ' : 'replaced ') + f.serviceName.value + ' → ' + f.image.value);
    $('#dlg-replace-service').close();
    f.dataset.mode = 'replace';
    $('#dlg-replace-service').querySelector('h2').textContent = 'Replace service';
    renderActive();
  } catch (e) { toast(e.message, 'err'); }
};

async function deleteSvc(name) {
  if (!confirm('Delete service "' + name + '" and all its containers?')) return;
  try {
    await api('/api/services/' + encodeURIComponent(name), { method:'DELETE' });
    toast('deleted ' + name);
    renderActive();
  } catch (e) { toast(e.message, 'err'); }
}

$('#form-new-service').onsubmit = async (e) => {
  e.preventDefault();
  const f = e.target;
  const env = {};
  for (const line of (f.env.value || '').split('\n')) {
    const [k, ...rest] = line.split('=');
    if (k && rest.length) env[k.trim()] = rest.join('=').trim();
  }
  try {
    await api('/api/services', { method:'POST', body: JSON.stringify({
      name: f.name.value, image: f.image.value, host: f.host.value,
      port: +f.port.value, replicas: +f.replicas.value,
      unscalable: f.unscalable.checked, env,
    })});
    toast('created ' + f.name.value);
    $('#dlg-new-service').close();
    f.reset();
    renderActive();
  } catch (e) { toast(e.message, 'err'); }
};

// ---- DNS ----
async function renderDNS() {
  const status = await api('/api/cf/enabled');
  const el = $('#tab-dns');
  if (!status.enabled) {
    el.innerHTML = '<p class="empty">Cloudflare not configured. Set <code>CLOUDFLARE_API_TOKEN</code> + <code>CLOUDFLARE_ZONE_ID</code>.</p>';
    return;
  }
  const recs = await api('/api/cf/records');
  let html = '<div class="btn-row" style="margin-bottom:1em">';
  html += '<button class="btn primary" ' + lockedAttr() + ' onclick="document.getElementById(\'dlg-new-dns\').showModal()">+ New record</button>';
  if (status.domain) html += '<span class="meta" style="margin-left:1em">zone: <code>' + esc(status.domain) + '</code></span>';
  html += '</div>';
  html += '<div class="card"><table><tr><th>Type</th><th>Name</th><th>Content</th><th>Proxied</th><th style="text-align:right">Actions</th></tr>';
  for (const r of recs) {
    html += '<tr><td><span class="pill muted">' + esc(r.type) + '</span></td>';
    html += '<td><code>' + esc(r.name) + '</code></td><td><code>' + esc(r.content) + '</code></td>';
    html += '<td>' + (r.proxied ? '<span class="pill ok">on</span>' : '<span class="pill muted">off</span>') + '</td>';
    html += '<td style="text-align:right"><div class="btn-row" style="justify-content:flex-end">';
    html += '<button class="btn" ' + lockedAttr() + ' onclick="editDNS(\'' + r.id + '\', \'' + esc(r.content) + '\')">Edit</button>';
    html += '<button class="btn danger" ' + lockedAttr() + ' onclick="deleteDNS(\'' + r.id + '\', \'' + esc(r.name) + '\')">Delete</button>';
    html += '</div></td></tr>';
  }
  html += '</table></div>';
  el.innerHTML = html;
}

async function editDNS(id, currentContent) {
  const v = prompt('New content:', currentContent);
  if (v === null || v === currentContent) return;
  try {
    await api('/api/cf/records/' + id, { method:'PATCH', body: JSON.stringify({content: v}) });
    toast('updated'); renderActive();
  } catch (e) { toast(e.message, 'err'); }
}
async function deleteDNS(id, name) {
  if (!confirm('Delete DNS record "' + name + '"?')) return;
  try {
    await api('/api/cf/records/' + id, { method:'DELETE' });
    toast('deleted'); renderActive();
  } catch (e) { toast(e.message, 'err'); }
}

$('#form-new-dns').onsubmit = async (e) => {
  e.preventDefault();
  const f = e.target;
  try {
    await api('/api/cf/records', { method:'POST', body: JSON.stringify({
      type: f.type.value, name: f.name.value, content: f.content.value, proxied: f.proxied.checked,
    })});
    toast('created'); $('#dlg-new-dns').close(); f.reset(); renderActive();
  } catch (e) { toast(e.message, 'err'); }
};

// ---- Users ----
async function renderUsers() {
  const users = await api('/api/users');
  const el = $('#tab-users');
  let html = '<div class="btn-row" style="margin-bottom:1em"><button class="btn primary" ' + lockedAttr() + ' onclick="openAddUser()">+ Add user</button></div>';
  html += '<div class="card"><table><tr><th>Username</th><th>Created</th><th style="text-align:right">Actions</th></tr>';
  for (const u of users) {
    const isMe = u.username === authState.username;
    html += '<tr><td><code>' + esc(u.username) + '</code>' + (isMe ? ' <span class="pill ok">you</span>' : '') + '</td>';
    html += '<td class="meta">' + new Date(u.created_at * 1000).toLocaleString() + '</td>';
    html += '<td style="text-align:right">';
    if (!isMe) html += '<button class="btn danger" ' + lockedAttr() + ' onclick="deleteUser(\'' + esc(u.username) + '\')">Delete</button>';
    html += '</td></tr>';
  }
  html += '</table></div>';
  el.innerHTML = html;
}

function openAddUser() {
  $('#add-user-body').innerHTML = ` + "`" + `
    <form id="form-add-user">
      <div class="field"><label>Username</label><input name="username" required pattern="[a-zA-Z0-9._-]{2,32}" autofocus></div>
      <div class="field"><label>Initial password (8+ chars)</label><input name="password" type="password" minlength="8" required></div>
      <div class="dialog-actions">
        <button type="button" class="btn" onclick="document.getElementById('dlg-add-user').close()">Cancel</button>
        <button type="submit" class="btn primary">Generate 2FA secret</button>
      </div>
    </form>
  ` + "`" + `;
  document.getElementById('dlg-add-user').showModal();
  $('#form-add-user').onsubmit = async (e) => {
    e.preventDefault();
    const f = e.target;
    try {
      const out = await api('/api/users', { method:'POST', body: JSON.stringify({
        username: f.username.value.trim(), password: f.password.value,
      })});
      pendingActive = true;
      $('#add-user-body').innerHTML = pendingUserBlock(out, 'adduser');
      wirePendingConfirm('adduser', out.username);
    } catch (e) { toast(e.message, 'err'); }
  };
}

async function deleteUser(name) {
  if (!confirm('Delete user "' + name + '"? Their session will end immediately.')) return;
  try {
    await api('/api/users/' + encodeURIComponent(name), { method:'DELETE' });
    toast('deleted ' + name); renderActive();
  } catch (e) { toast(e.message, 'err'); }
}

// ---- Stats (from monitor binary) ----
async function renderStats() {
  const el = $('#tab-stats');
  let overview;
  try {
    overview = await api('/api/monitor/overview');
  } catch (e) {
    el.innerHTML = '<p class="empty">Monitor unavailable — make sure the <code>monitor</code> container is running.</p>';
    return;
  }
  const pill = overview.health === 'up'
    ? '<span class="pill ok">all healthy</span>'
    : '<span class="pill warn">degraded</span>';
  let html = '<div class="card-head" style="margin-bottom:1em"><h2>Stack health ' + pill + '</h2>'
    + '<span class="meta">' + (overview.targets || []).length + ' target(s) · ' + (overview.total_requests || 0).toLocaleString() + ' lifetime requests</span></div>';

  for (const t of (overview.targets || [])) {
    const tpill = t.health === 'up' ? 'pill ok' : t.health === 'flaky' ? 'pill warn' : 'pill bad';
    html += '<div class="card"><div class="card-head"><h2>' + esc(t.name) + ' <span class="' + tpill + '">' + esc(t.health) + '</span></h2>';
    html += '<span class="meta"><code>' + esc(t.url) + '</code></span></div>';
    html += '<table>';
    html += '<tr><th>Total requests</th><td>' + (t.total || 0).toLocaleString() + '</td>';
    html += '<th>In flight</th><td>' + (t.in_flight || 0) + '</td></tr>';
    html += '<tr><th>Uptime</th><td>' + fmtUptime(t.uptime_seconds || 0) + '</td>';
    html += '<th>Latency p95</th><td>' + (t.latency_ms ? t.latency_ms.p95.toFixed(1) + ' ms' : '—') + '</td></tr>';
    html += '</table>';

    if (t.by_status) {
      html += '<h2 style="font-size:13px;margin-top:1em">By status</h2><table><tr>';
      const codes = Object.keys(t.by_status).sort();
      for (const c of codes) {
        const cls = c[0] === '2' ? 'pill ok' : c[0] === '3' ? 'pill muted' : c[0] === '4' ? 'pill warn' : 'pill bad';
        html += '<td><span class="' + cls + '">' + c + '</span> &nbsp;' + t.by_status[c].toLocaleString() + '</td>';
      }
      html += '</tr></table>';
    }
    if (t.by_method) {
      html += '<h2 style="font-size:13px;margin-top:1em">By method</h2><table><tr>';
      for (const [k, v] of Object.entries(t.by_method)) {
        html += '<td><code>' + esc(k) + '</code> &nbsp;' + v.toLocaleString() + '</td>';
      }
      html += '</tr></table>';
    }
    if (t.by_host) {
      const entries = Object.entries(t.by_host).sort((a,b) => b[1] - a[1]);
      html += '<h2 style="font-size:13px;margin-top:1em">Top hosts</h2><table>';
      for (const [h, n] of entries.slice(0, 10)) {
        html += '<tr><td><code>' + esc(h) + '</code></td><td style="text-align:right">' + n.toLocaleString() + '</td></tr>';
      }
      html += '</table>';
    }
    html += '</div>';
  }
  el.innerHTML = html;
}

function fmtUptime(s) {
  if (s < 60) return s + 's';
  if (s < 3600) return Math.floor(s/60) + 'm ' + (s%60) + 's';
  if (s < 86400) return Math.floor(s/3600) + 'h ' + Math.floor((s%3600)/60) + 'm';
  return Math.floor(s/86400) + 'd ' + Math.floor((s%86400)/3600) + 'h';
}

function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
}

function fmtBytes(n) {
  if (!n) return '—';
  if (n >= 1e12) return (n/1e12).toFixed(1) + ' TB';
  if (n >= 1e9)  return (n/1e9).toFixed(1) + ' GB';
  if (n >= 1e6)  return (n/1e6).toFixed(0) + ' MB';
  return n + ' B';
}
const ICON_CPU  = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><rect x="6" y="6" width="12" height="12" rx="1.5"/><path d="M9 3v3M15 3v3M9 18v3M15 18v3M3 9h3M3 15h3M18 9h3M18 15h3"/><rect x="9.5" y="9.5" width="5" height="5" rx=".5"/></svg>';
const ICON_MEM  = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><rect x="2" y="7" width="20" height="10" rx="1.5"/><path d="M6 11v2M9 11v2M12 11v2M15 11v2M18 11v2M6 17v3M18 17v3"/></svg>';
const ICON_DISK = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><ellipse cx="12" cy="6" rx="9" ry="3"/><path d="M3 6v6c0 1.66 4.03 3 9 3s9-1.34 9-3V6M3 12v6c0 1.66 4.03 3 9 3s9-1.34 9-3v-6"/></svg>';

function statClass(pct) { return pct >= 90 ? ' bad' : pct >= 75 ? ' warn' : ''; }

async function refreshStats() {
  if (!authState.authenticated) { $('#sys-stats').textContent = ''; return; }
  try {
    const s = await (await fetch('/api/stats')).json();
    const cpuPct = Math.max(0, Math.min(100, s.cpu_pct));
    const memPct = s.mem_total ? Math.round(100 * s.mem_used / s.mem_total) : 0;
    const diskPct = s.disk_total ? Math.round(100 * s.disk_used / s.disk_total) : 0;
    $('#sys-stats').innerHTML =
      '<span class="sysstat' + statClass(cpuPct)  + '" title="CPU usage"><span class="ico">' + ICON_CPU + '</span><span class="label">CPU</span> <span class="val">' + cpuPct.toFixed(0)  + '%</span> <span class="bar"><span style="width:' + cpuPct  + '%"></span></span></span>'
    + '<span class="sysstat' + statClass(memPct)  + '" title="Memory used / total" style="margin-left:16px"><span class="ico">' + ICON_MEM + '</span><span class="label">MEM</span> <span class="val">' + fmtBytes(s.mem_free)  + '</span> <span class="bar"><span style="width:' + memPct  + '%"></span></span></span>'
    + '<span class="sysstat' + statClass(diskPct) + '" title="Disk used / total"   style="margin-left:16px"><span class="ico">' + ICON_DISK + '</span><span class="label">DISK</span> <span class="val">' + fmtBytes(s.disk_free) + '</span> <span class="bar"><span style="width:' + diskPct + '%"></span></span></span>';
  } catch (e) { /* silent */ }
}

setInterval(() => { refreshAuth().catch(()=>{}); }, 30000);
setInterval(renderActive, 5000);
setInterval(refreshStats, 5000);
refreshAuth();
refreshStats();
</script>
</body>
</html>`
