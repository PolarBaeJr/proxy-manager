package main

// enrollPage is the passkey-management page, served verbatim via io.WriteString
// (NOT Fprintf) so its inline JS can contain literal % without doubling. All
// server-side gating is done in handlePasskeysPage (ssoUser cookie); the page
// itself carries no server-injected values. No backticks or template literals.
const enrollPage = `<!doctype html><html lang=en><meta charset=utf-8>
<meta name=viewport content="width=device-width,initial-scale=1">
<title>Passkeys</title>
<style>
  :root{color-scheme:dark}
  html,body{height:100%;margin:0}
  body{background:#0a0a0a;color:#e6e6e6;font:15px/1.55 -apple-system,BlinkMacSystemFont,"Inter",system-ui,sans-serif;display:flex;align-items:center;justify-content:center;padding:24px}
  .box{width:100%;max-width:420px}
  .code{font:600 12px/1 ui-monospace,SFMono-Regular,Menlo,monospace;letter-spacing:.12em;color:#6a6a6a;margin-bottom:14px;text-transform:uppercase}
  h1{margin:0 0 18px;font-size:20px;font-weight:600;letter-spacing:-.015em;color:#fafafa}
  .row{display:flex;align-items:center;justify-content:space-between;gap:10px;padding:10px 12px;border:1px solid #262626;border-radius:8px;background:#141414;margin-bottom:8px}
  .row .lbl{font-size:14px;color:#e6e6e6}
  .row .meta{font-size:12px;color:#6a6a6a}
  .row button{padding:6px 10px;border:1px solid #3f1d1d;border-radius:6px;background:#1a0f0f;color:#f87171;font:inherit;font-size:12px;cursor:pointer}
  .add{width:100%;padding:10px;border:0;border-radius:8px;background:#fafafa;color:#0a0a0a;font-weight:600;font-size:14px;font-family:inherit;cursor:pointer;margin-top:6px}
  .add:hover{background:#e2e2e2}
  .msg{margin:0 0 14px;font-size:13px;color:#f87171;min-height:1em}
  .msg.ok{color:#4ade80}
  .empty{color:#8a8a8a;font-size:14px;margin:0 0 8px}
  a{color:#e6e6e6}
</style>
<div class=box>
  <div class=code>proxy-manager</div>
  <h1>Passkeys</h1>
  <p class=msg id=msg></p>
  <div id=list></div>
  <button class=add id=add>Add passkey</button>
  <p style="margin-top:16px;font-size:13px"><a href="/login?ok=1">Back</a></p>
</div>
<script>
function b64uToBuf(s) {
  if (typeof s !== 'string') return s;
  s = s.replace(/-/g, '+').replace(/_/g, '/');
  while (s.length % 4) s += '=';
  const bin = atob(s);
  const buf = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) buf[i] = bin.charCodeAt(i);
  return buf.buffer;
}
function bufToB64u(buf) {
  const bytes = new Uint8Array(buf);
  let bin = '';
  for (let i = 0; i < bytes.byteLength; i++) bin += String.fromCharCode(bytes[i]);
  return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}
function decodeCreateOptions(o) {
  o.challenge = b64uToBuf(o.challenge);
  if (o.user && o.user.id) o.user.id = b64uToBuf(o.user.id);
  if (Array.isArray(o.excludeCredentials)) o.excludeCredentials.forEach(function(c){ c.id = b64uToBuf(c.id); });
  return o;
}
function encodeCreateResponse(cred) {
  return {
    id: cred.id,
    rawId: bufToB64u(cred.rawId),
    type: cred.type,
    clientExtensionResults: cred.getClientExtensionResults ? cred.getClientExtensionResults() : {},
    response: {
      attestationObject: bufToB64u(cred.response.attestationObject),
      clientDataJSON:    bufToB64u(cred.response.clientDataJSON),
      transports:        cred.response.getTransports ? cred.response.getTransports() : [],
    },
  };
}
function esc(s) {
  return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
}
function setMsg(text, ok) {
  const m = document.getElementById('msg');
  m.textContent = text || '';
  m.className = ok ? 'msg ok' : 'msg';
}
async function refresh() {
  const resp = await fetch('/passkeys/list');
  if (!resp.ok) { setMsg('Could not load passkeys.', false); return; }
  const items = await resp.json();
  const list = document.getElementById('list');
  if (!items || items.length === 0) {
    list.innerHTML = '<p class=empty>No passkeys yet.</p>';
    return;
  }
  let html = '';
  items.forEach(function(it){
    const when = it.created_at ? new Date(it.created_at * 1000).toLocaleDateString() : '';
    html += '<div class=row><div><div class=lbl>' + esc(it.label || 'Passkey') + '</div>'
          + '<div class=meta>Added ' + esc(when) + '</div></div>'
          + '<button data-id="' + esc(it.id) + '">Remove</button></div>';
  });
  list.innerHTML = html;
  list.querySelectorAll('button[data-id]').forEach(function(btn){
    btn.addEventListener('click', function(){ removePasskey(btn.getAttribute('data-id')); });
  });
}
async function removePasskey(id) {
  const resp = await fetch('/passkeys/item?id=' + encodeURIComponent(id), { method: 'DELETE' });
  if (!resp.ok) { setMsg('Remove failed.', false); return; }
  setMsg('Passkey removed.', true);
  refresh();
}
async function passkeyRegister(label) {
  const beginResp = await fetch('/passkey/register/begin', { method: 'POST' });
  if (!beginResp.ok) throw new Error('begin failed');
  const begin = await beginResp.json();
  const opts = decodeCreateOptions(begin.options.publicKey);
  const cred = await navigator.credentials.create({ publicKey: opts });
  if (!cred) throw new Error('no credential returned');
  const finishResp = await fetch('/passkey/register/finish?ceremony=' + encodeURIComponent(begin.ceremony)
            + '&label=' + encodeURIComponent(label || 'Passkey'), {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(encodeCreateResponse(cred)),
  });
  if (!finishResp.ok) throw new Error(await finishResp.text());
}
document.getElementById('add').addEventListener('click', function(){
  if (!window.PublicKeyCredential) { setMsg('This browser does not support passkeys.', false); return; }
  const label = prompt('Name this passkey', 'Passkey') || 'Passkey';
  passkeyRegister(label).then(function(){
    setMsg('Passkey added.', true);
    refresh();
  }).catch(function(e){ setMsg('Could not add passkey: ' + e.message, false); });
});
refresh();
</script>
`
