package main

const dashboardHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Pi Dashboard</title>
<style>
/* ============ TOKENS ============ */
:root{
  --bg:#000000;
  --surface:#0a0a0a;
  --surface-2:#111111;
  --surface-3:#161616;
  --border:#1f1f1f;
  --border-2:#2a2a2a;
  --text:#ededed;
  --muted:#888888;
  --muted-2:#5f5f5f;
  --accent:#00d4ff;
  --accent-2:#0070f3;
  --green:#50e3c2;      --green-bg:rgba(80,227,194,.10);  --green-border:rgba(80,227,194,.30);
  --red:#ff4d4f;        --red-bg:rgba(255,77,79,.10);     --red-border:rgba(255,77,79,.32);
  --yellow:#f5a623;     --yellow-bg:rgba(245,166,35,.12); --yellow-border:rgba(245,166,35,.30);
  --info:#0070f3;       --info-bg:rgba(0,112,243,.12);    --info-border:rgba(0,112,243,.35);
  --orange:#f5a623;
  --font:-apple-system,BlinkMacSystemFont,"Inter","Segoe UI",system-ui,sans-serif;
  --font-mono:ui-monospace,"SF Mono",Menlo,monospace;
  --radius-sm:6px; --radius-md:10px; --radius-lg:14px; --radius-pill:999px;
  --transition-fast:.12s ease; --transition-slow:.4s cubic-bezier(.16,1,.3,1);
  --shadow:0 1px 0 rgba(255,255,255,.02) inset,0 8px 32px -12px rgba(0,0,0,.7);
  --shadow-dialog:0 24px 80px -20px rgba(0,0,0,.85),0 0 0 1px var(--border-2);
  --shadow-toast:0 12px 40px -12px rgba(0,0,0,.7);
  --focus-ring:0 0 0 3px rgba(0,112,243,.35);
}
*{box-sizing:border-box}
[hidden]{display:none!important}
html,body{margin:0;padding:0}
body{
  background:var(--bg);
  color:var(--text);
  font-family:var(--font);
  font-size:14px;line-height:1.5;
  -webkit-font-smoothing:antialiased;
  min-height:100vh;
}
body::before{
  content:"";position:fixed;inset:0;pointer-events:none;z-index:0;
  background:
    radial-gradient(900px 500px at 78% -8%,rgba(0,112,243,.10),transparent 60%),
    radial-gradient(700px 480px at 4% 0%,rgba(0,212,255,.06),transparent 55%);
}
a{color:var(--accent);text-decoration:none}
a:hover{text-decoration:underline}
code{font-family:var(--font-mono);color:var(--orange)}
::selection{background:rgba(0,212,255,.25)}
.num,td.num,.tnum{font-variant-numeric:tabular-nums}

/* ============ LAYOUT ============ */
.wrap{position:relative;z-index:1;max-width:1320px;margin:0 auto;padding:0 28px}
header.app{padding:26px 0 0}
.topline{display:flex;align-items:flex-start;justify-content:space-between;gap:24px;flex-wrap:wrap}
.brand{display:flex;align-items:center;gap:13px}
.brand .logo{
  width:38px;height:38px;border-radius:11px;flex:none;display:grid;place-items:center;
  background:linear-gradient(145deg,var(--accent),var(--accent-2));
  box-shadow:0 0 24px -6px rgba(0,140,255,.6);
}
.brand .logo svg{width:21px;height:21px;color:#04121f}
h1{
  margin:0;font-size:21px;font-weight:700;letter-spacing:-.02em;line-height:1.1;
  background:linear-gradient(95deg,#fff 10%,var(--accent) 130%);
  -webkit-background-clip:text;background-clip:text;color:transparent;
}
.brand .sub{font-size:12px;color:var(--muted);margin-top:1px}
.idbox{display:flex;align-items:center;gap:14px}
#who{display:flex;align-items:center;gap:8px;font-size:13px;color:var(--text)}
#who .avatar{width:26px;height:26px;border-radius:50%;background:var(--surface-2);border:1px solid var(--border-2);
  display:grid;place-items:center;font-size:11px;font-weight:700;color:var(--accent)}

/* sysstats */
#sys-stats{display:flex;gap:12px;margin:22px 0 0;flex-wrap:wrap}
.sysstat{
  flex:1;min-width:190px;display:flex;align-items:center;gap:13px;
  background:var(--surface);border:1px solid var(--border);border-radius:var(--radius-md);
  padding:13px 15px;position:relative;overflow:hidden;transition:border-color var(--transition-fast);
}
.sysstat:hover{border-color:var(--border-2)}
.sysstat .ico{width:34px;height:34px;border-radius:9px;flex:none;display:grid;place-items:center;
  background:var(--surface-2);color:var(--accent);border:1px solid var(--border)}
.sysstat .ico svg{width:18px;height:18px}
.sysstat .body{flex:1;min-width:0}
.sysstat .label{font-size:10.5px;letter-spacing:.09em;text-transform:uppercase;color:var(--muted);font-weight:600}
.sysstat .val{font-size:19px;font-weight:700;letter-spacing:-.01em;font-variant-numeric:tabular-nums;margin:1px 0 7px;display:flex;align-items:baseline;gap:5px}
.sysstat .val small{font-size:11.5px;font-weight:500;color:var(--muted)}
.sysstat .bar{height:6px;border-radius:var(--radius-pill);background:var(--surface-3);overflow:hidden}
.sysstat .bar>span{display:block;height:100%;border-radius:inherit;
  background:linear-gradient(90deg,var(--accent),var(--accent-2));transition:width var(--transition-slow)}
.sysstat.warn .ico{color:var(--yellow)} .sysstat.warn .bar>span{background:linear-gradient(90deg,var(--yellow),#e08c12)}
.sysstat.bad .ico{color:var(--red)} .sysstat.bad .bar>span{background:linear-gradient(90deg,var(--red),#c0282b)}
#status{font-size:12px;color:var(--muted);margin:14px 0 0;display:flex;align-items:center;gap:7px}
#status .dot{width:7px;height:7px;border-radius:50%;background:var(--green);box-shadow:0 0 8px var(--green)}
#status.err .dot{background:var(--red);box-shadow:0 0 8px var(--red)}

/* nav */
nav{display:flex;gap:5px;margin:20px 0 0;border-bottom:1px solid var(--border);padding-bottom:0;flex-wrap:wrap}
nav button{
  appearance:none;background:transparent;border:0;color:var(--muted);cursor:pointer;
  font-family:inherit;font-size:13.5px;font-weight:550;
  display:flex;align-items:center;gap:8px;padding:11px 15px;border-radius:var(--radius-sm) var(--radius-sm) 0 0;
  position:relative;transition:color var(--transition-fast);margin-bottom:-1px;
}
nav button svg{width:16px;height:16px;opacity:.85}
nav button:hover{color:var(--text)}
nav button.active{color:var(--text)}
nav button.active::after{content:"";position:absolute;left:12px;right:12px;bottom:0;height:2px;border-radius:2px;
  background:linear-gradient(90deg,var(--accent),var(--accent-2));box-shadow:0 0 10px -1px var(--accent)}
nav button .badge{font-size:10px;font-weight:700;min-width:16px;height:16px;padding:0 5px;border-radius:var(--radius-pill);
  display:grid;place-items:center;background:var(--info-bg);color:var(--info);border:1px solid var(--info-border)}
nav button .badge.warn{background:var(--yellow-bg);color:var(--yellow);border-color:var(--yellow-border)}
nav button .badge.dot{min-width:7px;width:7px;height:7px;padding:0;background:var(--yellow);border:0;box-shadow:0 0 7px var(--yellow)}

/* locked banner */
#lock-banner{
  display:flex;align-items:center;gap:13px;margin:18px 0 0;padding:12px 16px;
  border:1px solid var(--yellow-border);background:linear-gradient(90deg,var(--yellow-bg),transparent);
  border-radius:var(--radius-md);
}
#lock-banner .lk{width:30px;height:30px;border-radius:8px;flex:none;display:grid;place-items:center;
  background:var(--yellow-bg);color:var(--yellow);border:1px solid var(--yellow-border)}
#lock-banner .lk svg{width:16px;height:16px}
#lock-banner .txt{flex:1}
#lock-banner b{font-weight:650;color:var(--text)}
#lock-banner .sub{font-size:12px;color:var(--muted)}
#lock-banner.hide{display:none}

main{padding:24px 0 64px}
.tabpane{min-height:440px}
@keyframes fade{from{opacity:.4;transform:translateY(5px)}to{opacity:1;transform:none}}
.tabpane:not([hidden]){animation:fade var(--transition-slow) both}

/* ============ CARDS ============ */
.card{
  position:relative;background:var(--surface);border:1px solid var(--border);
  border-radius:var(--radius-lg);padding:18px;box-shadow:var(--shadow);margin:0 0 16px;
  transition:border-color var(--transition-fast),transform var(--transition-fast);
}
.card::before{content:"";position:absolute;inset:0;border-radius:inherit;padding:1px;pointer-events:none;
  background:linear-gradient(135deg,var(--accent),transparent 42%);
  -webkit-mask:linear-gradient(#000 0 0) content-box,linear-gradient(#000 0 0);
  -webkit-mask-composite:xor;mask-composite:exclude;opacity:0;transition:opacity var(--transition-slow)}
.card:hover::before{opacity:.7}
.card-head{display:flex;align-items:center;gap:10px;flex-wrap:wrap;margin:0 0 14px}
.card-head h2,.card-head .ttl{margin:0;font-size:15px;font-weight:650;letter-spacing:-.01em;display:flex;align-items:center;gap:9px}
.card-head .spacer{flex:1}
.card.kpi{padding:16px 18px}
.grid{display:grid;gap:16px}
.grid.k4{grid-template-columns:repeat(4,1fr)}
.grid.k2{grid-template-columns:repeat(2,1fr)}
@media(max-width:880px){.grid.k4{grid-template-columns:repeat(2,1fr)}.grid.k2{grid-template-columns:1fr}}
.subhead{font-size:11px;letter-spacing:.08em;text-transform:uppercase;color:var(--muted);font-weight:600;margin:20px 0 10px;display:flex;align-items:center;gap:8px}
.subhead svg{width:14px;height:14px;opacity:.8}
.divider{height:1px;background:var(--border);margin:22px 0}

/* identifier */
.ident{font-family:var(--font-mono);font-weight:550;color:var(--text);font-size:13px}
.ident.dim{color:var(--muted)}

/* KPI tile */
.kpi-num{font-size:38px;font-weight:750;letter-spacing:-.03em;line-height:1;font-variant-numeric:tabular-nums;
  background:linear-gradient(180deg,#fff,#9aa6b2);-webkit-background-clip:text;background-clip:text;color:transparent}
.kpi-num.accent{background:linear-gradient(150deg,#fff,var(--accent));-webkit-background-clip:text;background-clip:text}
.kpi-num small{font-size:15px;font-weight:600;-webkit-text-fill-color:var(--muted)}
.kpi-label{font-size:11px;letter-spacing:.07em;text-transform:uppercase;color:var(--muted);font-weight:600;margin-bottom:11px;display:flex;align-items:center;gap:7px}
.kpi-label svg{width:14px;height:14px}
.kpi-foot{font-size:11.5px;color:var(--muted);margin-top:9px;display:flex;align-items:center;gap:6px}
.kpi-foot svg{width:13px;height:13px;flex:none}
.kpi-foot .up{color:var(--green);display:inline-flex;align-items:center;gap:5px} .kpi-foot .down{color:var(--red);display:inline-flex;align-items:center;gap:5px}
.kpi-foot .up svg,.kpi-foot .down svg,.kpi-foot span svg{width:13px;height:13px}

/* ============ PILLS ============ */
.pill{display:inline-flex;align-items:center;gap:5px;font-size:11px;font-weight:650;line-height:1;
  padding:4px 9px 4px 8px;border-radius:var(--radius-pill);border:1px solid transparent;white-space:nowrap;letter-spacing:.01em}
.pill svg{width:11px;height:11px}
.pill .gl{width:6px;height:6px;border-radius:50%;flex:none}
.pill.ok{color:var(--green);background:var(--green-bg);border-color:var(--green-border)} .pill.ok .gl{background:var(--green);box-shadow:0 0 6px var(--green)}
.pill.bad{color:var(--red);background:var(--red-bg);border-color:var(--red-border)} .pill.bad .gl{background:var(--red);box-shadow:0 0 6px var(--red)}
.pill.warn{color:var(--yellow);background:var(--yellow-bg);border-color:var(--yellow-border)} .pill.warn .gl{background:var(--yellow);box-shadow:0 0 6px var(--yellow)}
.pill.info{color:#5eb4ff;background:var(--info-bg);border-color:var(--info-border)} .pill.info .gl{background:var(--info)}
.pill.muted{color:var(--muted);background:var(--surface-2);border-color:var(--border-2)} .pill.muted .gl{background:var(--muted-2)}
.tag{display:inline-flex;align-items:center;gap:4px;font-size:10.5px;font-weight:650;color:#5eb4ff;background:var(--info-bg);
  border:1px solid var(--info-border);border-radius:var(--radius-sm);padding:2px 7px;cursor:default}
.tag svg{width:11px;height:11px}

/* ============ BUTTONS ============ */
.btn{
  appearance:none;font-family:inherit;font-size:13px;font-weight:600;cursor:pointer;
  display:inline-flex;align-items:center;gap:7px;padding:8px 13px;border-radius:var(--radius-sm);
  background:var(--surface-2);color:var(--text);border:1px solid var(--border-2);
  transition:background var(--transition-fast),border-color var(--transition-fast),transform var(--transition-fast),opacity var(--transition-fast);
}
.btn svg{width:15px;height:15px;opacity:.9}
.btn:hover{background:var(--surface-3);border-color:#363636}
.btn:active{transform:translateY(1px)}
.btn:focus-visible{outline:none;box-shadow:var(--focus-ring)}
.btn.primary{background:linear-gradient(180deg,var(--accent-2),#0059c2);border-color:#0a63d6;color:#fff;
  box-shadow:0 1px 0 rgba(255,255,255,.14) inset,0 6px 18px -8px rgba(0,112,243,.7)}
.btn.primary:hover{background:linear-gradient(180deg,#1a82ff,#0062cf)}
.btn.danger{background:var(--red-bg);border-color:var(--red-border);color:#ff7173}
.btn.danger:hover{background:rgba(255,77,79,.18);border-color:var(--red);color:#fff}
.btn.ghost{background:transparent;border-color:transparent;color:var(--muted)}
.btn.ghost:hover{background:var(--surface-2);color:var(--text)}
.btn.sm{padding:5px 9px;font-size:12px}
.btn.icon{padding:7px;border-radius:var(--radius-sm)}
.btn:disabled{opacity:.45;cursor:not-allowed}
.btn:disabled .lock{display:inline-flex}
.btn .lock{display:none;width:12px;height:12px;opacity:.8}
.btn-row{display:flex;align-items:center;gap:9px;flex-wrap:wrap}
.btn-row.end{justify-content:flex-end}
.btn-row.top{margin:0 0 16px}
.linkbtn{background:none;border:0;color:var(--accent);font:inherit;font-size:12.5px;font-weight:600;cursor:pointer;padding:4px 2px;display:inline-flex;align-items:center;gap:5px}
.linkbtn:hover{text-decoration:underline}
.linkbtn svg{width:13px;height:13px}

/* ============ TABLES ============ */
table{width:100%;border-collapse:collapse;font-size:13px}
th{text-align:left;font-size:10.5px;letter-spacing:.07em;text-transform:uppercase;color:var(--muted);font-weight:600;
  padding:0 12px 9px;border-bottom:1px solid var(--border)}
td{padding:11px 12px;border-bottom:1px solid var(--border);vertical-align:middle;font-variant-numeric:tabular-nums}
tr:last-child td{border-bottom:0}
tbody tr{transition:background var(--transition-fast)}
tbody tr:hover{background:rgba(255,255,255,.018)}
table.facts td{border:0;padding:7px 0}
table.facts td:first-child{color:var(--muted);width:42%;font-variant-numeric:normal}
table.facts td:last-child{text-align:right;font-weight:550}
.err{font-family:var(--font-mono);color:#ff7173;font-size:12px}
.col-clip{max-width:280px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
details.errd summary{cursor:pointer;color:var(--muted);font-size:12px;list-style:none;display:inline-flex;align-items:center;gap:5px}
details.errd summary::-webkit-details-marker{display:none}
details.errd[open] summary{color:#ff7173}

/* ============ CHARTS ============ */
.spark{display:block;width:100%;height:46px}
.statusbar{display:flex;height:9px;border-radius:var(--radius-pill);overflow:hidden;background:var(--surface-3);margin:4px 0}
.statusbar i{display:block;height:100%}
.legend{display:flex;gap:14px;flex-wrap:wrap;margin-top:9px;font-size:11.5px;color:var(--muted)}
.legend span{display:inline-flex;align-items:center;gap:5px;font-variant-numeric:tabular-nums}
.legend .sw{width:9px;height:9px;border-radius:2px;flex:none}
.hostbar{display:grid;grid-template-columns:1fr;gap:9px}
.hostrow{display:grid;grid-template-columns:160px 1fr 64px 56px;gap:12px;align-items:center;font-size:12.5px}
.hostrow .nm{font-family:var(--font-mono);font-size:12px;color:var(--text);overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.hostrow .track{height:7px;border-radius:var(--radius-pill);background:var(--surface-3);overflow:hidden}
.hostrow .track>i{display:block;height:100%;border-radius:inherit;background:linear-gradient(90deg,var(--accent),var(--accent-2))}
.hostrow .rq{text-align:right;font-variant-numeric:tabular-nums;color:var(--muted)}
.hostrow .ep{text-align:right;font-variant-numeric:tabular-nums;font-size:11.5px}
.hostrow .ep.bad{color:var(--red)} .hostrow .ep.ok{color:var(--muted-2)}
@media(max-width:680px){.hostrow{grid-template-columns:110px 1fr 50px}.hostrow .ep{display:none}}

/* replica control */
.replica-ctrl{display:inline-flex;align-items:center;gap:0;border:1px solid var(--border-2);border-radius:var(--radius-sm);overflow:hidden;background:var(--surface-2)}
.replica-ctrl button{appearance:none;border:0;background:transparent;color:var(--text);font:inherit;font-size:15px;width:30px;height:30px;cursor:pointer;transition:background var(--transition-fast)}
.replica-ctrl button:hover:not(:disabled){background:var(--surface-3)}
.replica-ctrl button:disabled{opacity:.4;cursor:not-allowed}
.replica-ctrl input{width:44px;height:30px;text-align:center;background:var(--bg);border:0;border-left:1px solid var(--border);border-right:1px solid var(--border);color:var(--text);font:inherit;font-variant-numeric:tabular-nums}
.replica-ctrl input:focus{outline:none}
.replica-ctrl .apply{width:auto;padding:0 11px;font-size:12px;font-weight:600;color:var(--accent);border-left:1px solid var(--border)}
.singleton-lock{display:inline-flex;align-items:center;gap:6px;color:var(--muted);font-size:12px}
.singleton-lock svg{width:13px;height:13px}

/* service action zone */
.svc-head{display:flex;align-items:flex-start;gap:14px;margin-bottom:4px}
.svc-head .ic{width:40px;height:40px;border-radius:10px;flex:none;display:grid;place-items:center;background:var(--surface-2);border:1px solid var(--border);color:var(--accent)}
.svc-head .ic svg{width:20px;height:20px}
.svc-name{font-size:16px;font-weight:650;letter-spacing:-.01em;display:flex;align-items:center;gap:8px;flex-wrap:wrap}
.svc-img{font-family:var(--font-mono);font-size:12.5px;color:var(--muted);margin-top:2px;display:inline-flex;align-items:center;gap:6px}
.svc-img b{color:var(--text);font-weight:550}
.svc-img svg{width:13px;height:13px}
.actionzone{display:flex;align-items:center;gap:9px;flex-wrap:wrap;margin-top:14px;padding-top:14px;border-top:1px solid var(--border)}
.actionzone .sep{flex:1}
.menu{position:relative}
.menu-pop{position:absolute;right:0;top:calc(100% + 6px);background:var(--surface-2);border:1px solid var(--border-2);border-radius:var(--radius-md);
  box-shadow:var(--shadow-dialog);min-width:170px;padding:5px;z-index:30;display:none}
.menu-pop.open{display:block}
.menu-pop button{display:flex;width:100%;align-items:center;gap:9px;padding:8px 10px;border:0;background:none;color:var(--text);
  font:inherit;font-size:13px;border-radius:var(--radius-sm);cursor:pointer;text-align:left}
.menu-pop button:hover{background:var(--surface-3)}
.menu-pop button.danger{color:#ff7173} .menu-pop button.danger:hover{background:var(--red-bg)}
.menu-pop button svg{width:14px;height:14px}

/* empty state */
.empty{font-style:italic;color:var(--muted)}
.empty-state{text-align:center;padding:46px 24px;display:flex;flex-direction:column;align-items:center;gap:6px}
.empty-state .es-ic{width:52px;height:52px;border-radius:14px;display:grid;place-items:center;margin-bottom:8px;
  background:var(--surface-2);border:1px solid var(--border);color:var(--muted)}
.empty-state .es-ic svg{width:24px;height:24px}
.empty-state h3{margin:0;font-size:15px;font-weight:600;color:var(--text)}
.empty-state p{margin:0;font-size:13px;color:var(--muted);max-width:340px}
.empty-state .btn{margin-top:12px}

/* note */
.note{display:flex;gap:10px;align-items:flex-start;border:1px solid var(--yellow-border);background:var(--yellow-bg);
  border-radius:var(--radius-md);padding:12px 14px;font-size:12.5px;color:var(--text)}
.note svg{width:16px;height:16px;color:var(--yellow);flex:none;margin-top:1px}
.note strong{color:var(--yellow)}
.note.info{border-color:var(--info-border);background:var(--info-bg)}
.note.info svg{color:var(--info)}
.meta{font-size:12px;color:var(--muted)}
.copybar{display:flex;align-items:center;gap:8px;background:var(--bg);border:1px solid var(--border);border-radius:var(--radius-sm);padding:7px 9px;margin-top:8px}
.copybar code{flex:1;font-size:12px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;color:var(--text)}

/* ============ FIELDS / FORMS ============ */
.field{margin:0 0 14px}
.field>label{display:block;font-size:12px;font-weight:600;color:var(--text);margin-bottom:6px}
.field .hint{font-size:11.5px;color:var(--muted);margin-top:5px;font-weight:400}
.field input[type=text],.field input[type=password],.field input[type=number],.field input:not([type]),.field select,.field textarea{
  width:100%;background:var(--bg);border:1px solid var(--border);border-radius:var(--radius-sm);
  color:var(--text);font:inherit;font-size:13.5px;padding:9px 11px;transition:border-color var(--transition-fast),box-shadow var(--transition-fast)}
.field textarea{font-family:var(--font-mono);font-size:12.5px;resize:vertical;min-height:74px}
.field input:focus,.field select:focus,.field textarea:focus{outline:none;border-color:var(--accent-2);box-shadow:var(--focus-ring)}
.field input:disabled{opacity:.6;background:var(--surface)}
.field.check{display:flex;align-items:flex-start;gap:10px}
.field.check input[type=checkbox]{width:17px;height:17px;margin-top:1px;accent-color:var(--accent-2);padding:0}
.field.check label{margin:0}
.field-group{border:1px solid var(--border);border-radius:var(--radius-md);padding:14px 14px 2px;margin:0 0 14px;background:var(--surface)}
.field-group>.gl-title{font-size:10.5px;letter-spacing:.08em;text-transform:uppercase;color:var(--muted);font-weight:650;margin:0 0 12px;display:flex;align-items:center;gap:7px}
.field-group>.gl-title svg{width:13px;height:13px}
.field-row{display:grid;grid-template-columns:1fr 1fr;gap:12px}
@media(max-width:520px){.field-row{grid-template-columns:1fr}}

/* OTP */
.otp-input{width:100%;text-align:center;letter-spacing:8px;font-family:var(--font-mono);font-size:22px;font-weight:600;
  background:var(--bg);border:1px solid var(--border);border-radius:var(--radius-sm);color:var(--text);padding:12px}
.otp-input:focus{outline:none;border-color:var(--accent-2);box-shadow:var(--focus-ring)}

/* ============ AUTH ============ */
#auth-screen{position:relative;z-index:1;min-height:100vh;display:grid;place-items:center;padding:30px}
.full-card{max-width:520px;width:100%;margin:0;padding:30px}
.full-card .brand{justify-content:center;margin-bottom:6px}
.stepper{display:flex;align-items:center;gap:9px;justify-content:center;margin:4px 0 22px}
.stepper .st{display:flex;align-items:center;gap:8px;color:var(--muted);font-size:12px;font-weight:600}
.stepper .st .n{width:22px;height:22px;border-radius:50%;display:grid;place-items:center;font-size:11px;
  background:var(--surface-2);border:1px solid var(--border-2);color:var(--muted)}
.stepper .st.active .n{background:linear-gradient(145deg,var(--accent),var(--accent-2));color:#04121f;border-color:transparent}
.stepper .st.active{color:var(--text)}
.stepper .ln{width:34px;height:1px;background:var(--border-2)}
.totp-secret{font-family:var(--font-mono);font-size:18px;font-weight:600;text-align:center;letter-spacing:2px;
  background:var(--bg);border:1px solid var(--border);border-radius:var(--radius-sm);padding:13px;color:var(--accent);word-break:break-all}
.qr-wrap{display:grid;place-items:center;padding:14px;background:#fff;border-radius:var(--radius-md);width:fit-content;margin:0 auto 16px}
.qr-wrap img{display:block;width:172px;height:172px;image-rendering:pixelated}
details.secret{margin:14px 0}
details.secret summary{cursor:pointer;font-size:13px;color:var(--accent);list-style:none;display:inline-flex;align-items:center;gap:6px}
details.secret summary::-webkit-details-marker{display:none}
details.secret summary .chev{transition:transform var(--transition-fast);display:inline-flex}
details.secret[open] summary .chev{transform:rotate(90deg)}
.collapse-2fa{margin-top:6px}
.collapse-2fa summary{cursor:pointer;font-size:12.5px;color:var(--accent);list-style:none;display:inline-flex;align-items:center;gap:6px}
.collapse-2fa summary::-webkit-details-marker{display:none}

/* ============ DIALOG ============ */
dialog{
  border:0;padding:0;background:transparent;color:var(--text);max-width:560px;width:calc(100% - 36px);
  border-radius:var(--radius-lg);box-shadow:var(--shadow-dialog);overflow:visible;
}
dialog::backdrop{background:rgba(0,0,0,.66);backdrop-filter:blur(3px)}
.dlg{background:var(--surface);border-radius:var(--radius-lg);overflow:hidden;border:1px solid var(--border-2)}
.dlg-head{display:flex;align-items:center;gap:12px;padding:17px 18px;border-bottom:1px solid var(--border);position:relative}
.dlg-head::before{content:"";position:absolute;left:0;top:0;bottom:0;width:3px;background:var(--accent-2)}
.dlg-head.danger::before{background:var(--red)}
.dlg-head .di{width:34px;height:34px;border-radius:9px;flex:none;display:grid;place-items:center;background:var(--info-bg);color:var(--info);border:1px solid var(--info-border)}
.dlg-head.danger .di{background:var(--red-bg);color:var(--red);border-color:var(--red-border)}
.dlg-head .di svg{width:17px;height:17px}
.dlg-head h3{margin:0;font-size:15px;font-weight:650}
.dlg-head .dsub{font-size:12px;color:var(--muted);margin-top:1px}
.dlg-head .x{margin-left:auto;background:none;border:0;color:var(--muted);cursor:pointer;width:30px;height:30px;border-radius:var(--radius-sm);display:grid;place-items:center}
.dlg-head .x:hover{background:var(--surface-2);color:var(--text)}
.dlg-head .x svg{width:16px;height:16px}
.dlg-body{padding:18px;max-height:min(70vh,640px);overflow:auto}
.dialog-actions{display:flex;justify-content:flex-end;gap:9px;padding:15px 18px;border-top:1px solid var(--border);background:var(--surface-2)}
.token-block{font-family:var(--font-mono);font-size:13px;background:var(--bg);border:1px solid var(--border);border-radius:var(--radius-sm);
  padding:13px;word-break:break-all;color:var(--accent);line-height:1.55}

/* ============ TOASTS ============ */
#toasts{position:fixed;top:18px;right:18px;z-index:200;display:flex;flex-direction:column;gap:10px;width:340px;max-width:calc(100vw - 36px)}
.toast{position:relative;display:flex;align-items:flex-start;gap:11px;background:var(--surface-2);border:1px solid var(--border-2);
  border-radius:var(--radius-md);padding:13px 14px;box-shadow:var(--shadow-toast);overflow:hidden;
  animation:toastIn var(--transition-slow)}
.toast.out{animation:toastOut .3s ease forwards}
@keyframes toastIn{from{opacity:0;transform:translateX(28px) scale(.96)}to{opacity:1;transform:none}}
@keyframes toastOut{to{opacity:0;transform:translateX(28px) scale(.96)}}
.toast .ti{width:22px;height:22px;border-radius:6px;flex:none;display:grid;place-items:center}
.toast .ti svg{width:14px;height:14px}
.toast.ok .ti{background:var(--green-bg);color:var(--green)}
.toast.err .ti{background:var(--red-bg);color:var(--red)}
.toast .msg{flex:1;font-size:13px;line-height:1.4;color:var(--text)}
.toast .prog{position:absolute;left:0;bottom:0;height:2px;background:var(--accent)}
.toast.ok .prog{background:var(--green)} .toast.err .prog{background:var(--red)}
@keyframes progshrink{from{width:100%}to{width:0%}}

footer.app{border-top:1px solid var(--border);margin-top:30px;padding:16px 0;display:flex;gap:16px;align-items:center;
  font-size:11.5px;color:var(--muted-2);flex-wrap:wrap}
footer.app .dotsep{width:3px;height:3px;border-radius:50%;background:var(--muted-2)}
footer.app code{color:var(--muted)}
</style>
</head>
<body>

<!-- ============ AUTH SCREEN ============ -->
<div id="auth-screen" hidden>
  <div class="card full-card">
    <div class="brand">
      <div class="logo" id="auth-logo-slot"></div>
      <div>
        <h1>Pi Dashboard</h1>
        <div class="sub">homelab edge &amp; service control</div>
      </div>
    </div>
    <div id="auth-content"></div>
  </div>
</div>

<!-- ============ MAIN SCREEN ============ -->
<div id="main-screen" hidden>
  <header class="app">
    <div class="wrap">
      <div class="topline">
        <div class="brand">
          <div class="logo" id="main-logo-slot"></div>
          <div>
            <h1>Pi Dashboard</h1>
            <div class="sub">homelab edge &amp; service control</div>
          </div>
        </div>
        <div class="idbox">
          <div id="who"></div>
          <button class="btn" onclick="logout()" id="logout-btn">Logout</button>
        </div>
      </div>
      <div id="sys-stats"></div>
      <div id="status">loading…</div>
      <nav>
        <button class="active" data-tab="routes"></button>
        <button data-tab="services"></button>
        <button data-tab="dns"></button>
        <button data-tab="users"></button>
        <button data-tab="stats"></button>
      </nav>
      <div id="lock-banner" class="hide"></div>
    </div>
  </header>
  <main>
    <div class="wrap">
      <section id="tab-routes" class="tabpane"></section>
      <section id="tab-services" class="tabpane" hidden></section>
      <section id="tab-dns" class="tabpane" hidden></section>
      <section id="tab-users" class="tabpane" hidden></section>
      <section id="tab-stats" class="tabpane" hidden></section>
    </div>
    <div class="wrap">
      <footer class="app">
        <span>Pi Dashboard <code>v2.4.0</code></span><span class="dotsep"></span>
        <span><a href="/api/health">/api/health</a></span>
      </footer>
    </div>
  </main>
</div>

<!-- ============ DIALOGS ============ -->
<dialog id="dlg-new-service"></dialog>
<dialog id="dlg-replace-service"></dialog>
<dialog id="dlg-new-dns"></dialog>
<dialog id="dlg-2fa"></dialog>
<dialog id="dlg-add-user"><div class="dlg"><div id="add-user-body"></div></div></dialog>
<dialog id="dlg-token-reveal"></dialog>

<div id="toasts"></div>

<script>
/* ===================================================================
   Pi Dashboard — client. Wire-compatible with the existing api.go.
   Visuals are the pitch-black / Vercel/Geist refresh; behavior, auth
   flow, polling, and contract are preserved from the prior ui.go.
   =================================================================== */

const $ = (s, r=document) => r.querySelector(s);
const $$ = (s, r=document) => [...r.querySelectorAll(s)];
const fmtTime = () => new Date().toLocaleTimeString();

let authState = { setup_complete: false, authenticated: false, elevated_until: 0, username: '', now: 0 };
let pendingActive = false; // true while a TOTP-confirm screen is on-screen; freezes auto-refresh
let statsDetail = null;

function isElevated() { return authState.authenticated && authState.elevated_until > (Date.now() / 1000); }

/* ---------- icons (inline SVG, shared stroke set) ---------- */
function svg(p, o){ o=o||''; return '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" '+o+'>'+p+'</svg>'; }
const I = {
  logo:    svg('<path d="M12 3l9 5-9 5-9-5z"/><path d="M3 12l9 5 9-5M3 16l9 5 9-5"/>'),
  layers:  svg('<path d="M12 3l9 5-9 5-9-5z"/><path d="M3 12l9 5 9-5M3 16l9 5 9-5"/>'),
  routes:  svg('<path d="M5 4v12a3 3 0 0 0 3 3h8"/><circle cx="5" cy="4" r="1.6"/><path d="M16 16l3 3-3 3"/>'),
  services:svg('<rect x="3" y="4" width="18" height="6" rx="1.5"/><rect x="3" y="14" width="18" height="6" rx="1.5"/><path d="M7 7h.01M7 17h.01"/>'),
  dns:     svg('<circle cx="12" cy="12" r="9"/><path d="M3 12h18M12 3c3 3 3 15 0 18M12 3c-3 3-3 15 0 18"/>'),
  users:   svg('<circle cx="9" cy="8" r="3.2"/><path d="M3.5 20a5.5 5.5 0 0 1 11 0"/><path d="M16 5.2a3.2 3.2 0 0 1 0 6M16.5 14.4a5.5 5.5 0 0 1 4 5.6"/>'),
  stats:   svg('<path d="M4 19V5M4 19h16"/><path d="M8 16l3-4 3 2 4-6"/>'),
  cpu:     svg('<rect x="7" y="7" width="10" height="10" rx="1.5"/><path d="M9 3v3M12 3v3M15 3v3M9 18v3M12 18v3M15 18v3M3 9h3M3 12h3M3 15h3M18 9h3M18 12h3M18 15h3"/>'),
  mem:     svg('<rect x="3" y="7" width="18" height="10" rx="1.5"/><path d="M7 7v-2M11 7v-2M15 7v-2M7 21v-4M11 21v-4M15 21v-4"/>'),
  disk:    svg('<ellipse cx="12" cy="6" rx="8" ry="3"/><path d="M4 6v12c0 1.7 3.6 3 8 3s8-1.3 8-3V6M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3"/>'),
  plus:    svg('<path d="M12 5v14M5 12h14"/>'),
  trash:   svg('<path d="M4 7h16M9 7V5a1 1 0 0 1 1-1h4a1 1 0 0 1 1 1v2M6 7l1 13a1 1 0 0 0 1 1h8a1 1 0 0 0 1-1l1-13"/>'),
  edit:    svg('<path d="M4 20h4L19 9a2 2 0 0 0-3-3L5 17z"/><path d="M14 7l3 3"/>'),
  lock:    svg('<rect x="5" y="11" width="14" height="9" rx="2"/><path d="M8 11V8a4 4 0 0 1 8 0v3"/>'),
  unlock:  svg('<rect x="5" y="11" width="14" height="9" rx="2"/><path d="M8 11V8a4 4 0 0 1 7.5-1.8"/>'),
  copy:    svg('<rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h8"/>'),
  check:   svg('<path d="M4 12.5l5 5L20 6"/>'),
  x:       svg('<path d="M6 6l12 12M18 6L6 18"/>'),
  alert:   svg('<path d="M12 4l9 16H3z"/><path d="M12 10v4M12 17h.01"/>'),
  arrowup: svg('<path d="M12 19V5M6 11l6-6 6 6"/>'),
  arrowdown:svg('<path d="M12 5v14M6 13l6 6 6-6"/>'),
  dots:    svg('<circle cx="6" cy="12" r="1.6"/><circle cx="12" cy="12" r="1.6"/><circle cx="18" cy="12" r="1.6"/>'),
  rocket:  svg('<path d="M5 15c-1 1-1.5 4-1.5 4s3-.5 4-1.5M14 4c4 1 6 3 7 7-2 2-5 4-9 5l-3-3c1-4 3-7 5-9z"/><circle cx="14.5" cy="9.5" r="1.4"/>'),
  swap:    svg('<path d="M4 8h13l-3-3M20 16H7l3 3"/>'),
  rewind:  svg('<path d="M11 18L4 12l7-6zM20 18l-7-6 7-6z"/>'),
  chevron: svg('<path d="M9 6l6 6-6 6"/>'),
  scissors:svg('<circle cx="6" cy="6" r="2.2"/><circle cx="6" cy="18" r="2.2"/><path d="M8 8l12 10M8 16L20 6"/>'),
  key:     svg('<circle cx="8" cy="14" r="4"/><path d="M11 11l8-8M16 6l2 2M14 8l2 2"/>'),
  globe:   svg('<circle cx="12" cy="12" r="9"/><path d="M3 12h18M12 3c3 3 3 15 0 18M12 3c-3 3-3 15 0 18"/>'),
  clock:   svg('<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/>'),
  activity:svg('<path d="M3 12h4l3 8 4-16 3 8h4"/>'),
  bolt:    svg('<path d="M13 3L4 14h7l-1 7 9-11h-7z"/>'),
  shield:  svg('<path d="M12 3l8 3v6c0 4-3 7-8 9-5-2-8-5-8-9V6z"/><path d="M9 12l2 2 4-4"/>'),
  link:    svg('<path d="M10 14a4 4 0 0 0 6 .5l2-2a4 4 0 0 0-6-6l-1 1M14 10a4 4 0 0 0-6-.5l-2 2a4 4 0 0 0 6 6l1-1"/>'),
};
const NAVMETA = { routes:'Routes', services:'Services', dns:'DNS', users:'Users', stats:'Stats' };
const NAVICON = { routes:I.routes, services:I.services, dns:I.dns, users:I.users, stats:I.stats };

/* ---------- toast (kind, msg) with icon + progshrink bar ---------- */
function toast(msg, kind='ok') {
  const wrap = document.getElementById('toasts');
  if (!wrap) return;
  const t = document.createElement('div');
  t.className = 'toast ' + (kind === 'err' ? 'err' : 'ok');
  t.innerHTML = '<div class="ti">' + (kind === 'err' ? I.x : I.check) + '</div>'
              + '<div class="msg">' + esc(msg) + '</div>'
              + '<div class="prog" style="animation:progshrink 3.5s linear forwards"></div>';
  wrap.appendChild(t);
  setTimeout(() => { t.classList.add('out'); setTimeout(() => t.remove(), 300); }, 3500);
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

/* ---------- Auth bootstrap ---------- */
async function refreshAuth() {
  const r = await fetch('/api/auth/status');
  const next = await r.json();
  // Freeze re-render while a pending-confirm screen is up (preserves QR + TOTP input).
  if (pendingActive && !next.setup_complete) { authState = next; return; }
  authState = next;
  renderAuthOrMain();
}
function renderAuthOrMain() {
  if (!authState.setup_complete) { showAuth(setupView()); return; }
  if (!authState.authenticated)   { showAuth(loginView()); return; }
  $('#auth-screen').hidden = true;
  $('#main-screen').hidden = false;
  renderHeader();
  renderActive();
}
function showAuth(html) {
  $('#auth-screen').hidden = false;
  $('#main-screen').hidden = true;
  $('#auth-logo-slot').innerHTML = I.layers;
  $('#auth-content').innerHTML = html;
  wireAuthForms();
}

function setupView() {
  return '<div class="stepper"><div class="st active"><span class="n">1</span>Credentials</div>'
       + '<div class="ln"></div><div class="st"><span class="n">2</span>Enroll 2FA</div></div>'
       + '<p class="meta" style="margin:0 0 14px">First-time setup. Pick a username and password, then verify a 2FA code before the account is saved.</p>'
       + '<form id="form-setup">'
       + '<div class="field"><label>Admin username</label><input name="username" required pattern="[a-zA-Z0-9._-]{2,32}" placeholder="admin" autofocus></div>'
       + '<div class="field"><label>Password</label><input name="password" type="password" minlength="8" required><div class="hint">Minimum 8 characters.</div></div>'
       + '<button class="btn primary" type="submit" style="width:100%;justify-content:center">' + I.shield + 'Create account &amp; enroll 2FA</button>'
       + '</form>'
       + '<div id="setup-result"></div>';
}

function loginView() {
  return '<p class="meta" style="margin:0 0 14px;text-align:center">Sign in to your homelab.</p>'
       + '<form id="form-login">'
       + '<div class="field"><label>Username</label><input name="username" required autofocus></div>'
       + '<div class="field"><label>Password</label><input name="password" type="password" required></div>'
       + '<details class="collapse-2fa"><summary>' + I.shield + ' Add 2FA code for edit access</summary>'
       + '<div class="field" style="margin-top:10px"><input class="otp-input" name="code" inputmode="numeric" pattern="[0-9]{6}" maxlength="6" placeholder="••••••" style="font-size:18px;letter-spacing:6px;padding:9px"></div>'
       + '</details>'
       + '<button class="btn primary" type="submit" style="width:100%;justify-content:center;margin-top:16px">' + I.unlock + 'Sign in</button>'
       + '</form>';
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
      $('#setup-result').innerHTML = '<div class="divider"></div>' + pendingUserBlock(out, 'setup');
      wirePendingConfirm('setup', out.username);
    } catch (e) { toast(e.message, 'err'); }
  };

  const login = $('#form-login');
  if (login) login.onsubmit = async (e) => {
    e.preventDefault();
    try {
      await api('/api/auth/login', { method:'POST', body: JSON.stringify({
        username: login.username.value.trim(), password: login.password.value, code: (login.code && login.code.value) || '',
      })});
      await refreshAuth();
    } catch (e) { toast(e.message, 'err'); }
  };
}

// Shared pending-user block used by both setup view and add-user dialog.
function pendingUserBlock(p, kind) {
  const stepper = '<div class="stepper"><div class="st active"><span class="n">✓</span>Credentials</div>'
                + '<div class="ln"></div><div class="st active"><span class="n">2</span>Enroll 2FA</div></div>';
  const qr = p.qr_data_url
    ? '<div class="qr-wrap"><img src="' + p.qr_data_url + '" alt="Scan with your authenticator"></div>'
    : '';
  return stepper
    + '<div class="note">' + I.alert + '<div><strong>Save this now</strong> — the secret is shown only once. '
    + 'Pending for ' + (kind === 'setup' ? '10 minutes' : 'the new user') + '; closing this window cancels it.</div></div>'
    + '<p class="meta" style="margin:12px 0 6px;text-align:center">Account: <span class="ident">' + esc(p.username) + '</span></p>'
    + qr
    + '<p style="text-align:center;margin:4px 0 0"><a href="' + esc(p.otpauth_uri) + '" target="_blank">Or open in authenticator app →</a></p>'
    + '<details class="secret"><summary>' + I.key + '<span class="chev">' + I.chevron + '</span> Reveal setup key</summary>'
    + '<div class="totp-secret" style="margin-top:10px">' + esc(p.totp_secret) + '</div></details>'
    + '<form id="form-confirm-' + kind + '" style="margin-top: 6px">'
    + '<div class="field"><label>Enter the 6-digit code to confirm</label><input class="otp-input" name="code" inputmode="numeric" pattern="[0-9]{6}" maxlength="6" required autocomplete="one-time-code" placeholder="••••••"></div>'
    + '<button class="btn primary" type="submit" style="width:100%;justify-content:center">' + I.check + 'Verify &amp; save user</button>'
    + '</form>';
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

/* ---------- 2FA modal ---------- */
function needs2FA() { document.getElementById('dlg-2fa').showModal(); }

/* ---------- Header (logo, who, nav, lock banner) ---------- */
function renderHeader() {
  $('#main-logo-slot').innerHTML = I.layers;
  const u = authState.username || '?';
  $('#who').innerHTML = '<span class="avatar">' + esc(u.charAt(0).toUpperCase()) + '</span><span>' + esc(u) + '</span>';
  $('#logout-btn').innerHTML = I.x + 'Logout';
  $$('nav button').forEach(b => {
    const k = b.dataset.tab;
    b.innerHTML = NAVICON[k] + '<span>' + NAVMETA[k] + '</span>';
  });
  renderLockBanner();
}
function renderLockBanner() {
  const el = $('#lock-banner');
  if (!el) return;
  if (isElevated()) { el.className = 'hide'; el.innerHTML = ''; return; }
  el.className = '';
  el.innerHTML = '<div class="lk">' + I.lock + '</div>'
               + '<div class="txt"><b>Edits locked</b> <span class="sub">— confirm 2FA to scale, deploy, and edit DNS or users.</span></div>'
               + '<button class="btn primary" onclick="document.getElementById(\'dlg-2fa\').showModal()">' + I.unlock + 'Unlock edits</button>';
}

function lockedAttr() { return isElevated() ? '' : 'disabled title="Confirm 2FA to enable"'; }
function lk() { return isElevated() ? '' : '<span class="lock">' + I.lock + '</span>'; }

/* ---------- Tabs ---------- */
const TABS = ['routes', 'services', 'dns', 'users', 'stats'];
let activeTab = 'routes';
$$('nav button').forEach(b => b.onclick = () => switchTab(b.dataset.tab));
function switchTab(t) {
  activeTab = t;
  statsDetail = null;
  $$('nav button').forEach(b => b.classList.toggle('active', b.dataset.tab === t));
  TABS.forEach(x => $('#tab-' + x).hidden = x !== t);
  renderActive();
}

async function renderActive() {
  if (!authState.authenticated) return;
  if (pendingActive) return;
  try {
    if (activeTab === 'routes') await renderRoutes();
    else if (activeTab === 'services') await renderServices();
    else if (activeTab === 'dns') await renderDNS();
    else if (activeTab === 'users') await renderUsers();
    else if (activeTab === 'stats') await renderStats();
    setStatusOK();
  } catch (e) {
    setStatusErr(e.message);
  }
}
function setStatusOK() {
  const s = $('#status');
  s.className = '';
  s.innerHTML = '<span class="dot"></span>updated ' + fmtTime() + ' · auto-refresh 5s';
}
function setStatusErr(msg) {
  const s = $('#status');
  s.className = 'err';
  s.innerHTML = '<span class="dot"></span>error: ' + esc(msg);
}

/* ---------- HEALTH PILL ---------- */
function healthPill(state) {
  const map = {
    up:     ['ok',    'Healthy',     I.check],
    flaky:  ['warn',  'Flaky',       I.alert],
    down:   ['bad',   'Down',        I.x],
    absent: ['muted', 'Not deployed',''],
    unknown:['muted', '—',           ''],
  };
  const m = map[state] || map.unknown;
  return '<span class="pill ' + m[0] + '">' + (m[2] || '<span class="gl"></span>') + m[1] + '</span>';
}

/* ---------- KPI helpers ---------- */
function kpi(icon, label, val, foot, accent) {
  return '<div class="card kpi"><div class="kpi-label">' + icon + label + '</div>'
       + '<div class="kpi-num' + (accent ? ' accent' : '') + '">' + val + '</div>'
       + (foot ? '<div class="kpi-foot">' + foot + '</div>' : '') + '</div>';
}
function kpiSm(label, val) {
  return '<div><div class="kpi-label" style="margin-bottom:4px">' + label + '</div>'
       + '<div style="font-size:19px;font-weight:700;font-variant-numeric:tabular-nums;letter-spacing:-.01em">' + val + '</div></div>';
}
function emptyState(icon, title, body) {
  return '<div class="card"><div class="empty-state"><div class="es-ic">' + icon + '</div>'
       + '<h3>' + esc(title) + '</h3><p>' + esc(body) + '</p></div></div>';
}

/* ---------- Charts ---------- */
function sparkline(data, w, h, color) {
  w = w || 300; h = h || 46; color = color || 'var(--accent)';
  if (!data || !data.length) return '';
  const max = Math.max.apply(null, data) || 1, min = Math.min.apply(null, data);
  const pad = 2, iw = w - pad * 2, ih = h - pad * 2;
  const pts = data.map((v, i) => { const x = pad + i / (data.length - 1) * iw; const y = pad + ih - ((v - min) / ((max - min) || 1)) * ih; return [x, y]; });
  const line = pts.map((p, i) => (i ? 'L' : 'M') + p[0].toFixed(1) + ' ' + p[1].toFixed(1)).join(' ');
  const area = line + ' L' + pts[pts.length - 1][0].toFixed(1) + ' ' + (h - pad) + ' L' + pad + ' ' + (h - pad) + ' Z';
  const id = 'sg' + Math.random().toString(36).slice(2, 7);
  return '<svg class="spark" viewBox="0 0 ' + w + ' ' + h + '" preserveAspectRatio="none">'
       + '<defs><linearGradient id="' + id + '" x1="0" y1="0" x2="0" y2="1">'
       + '<stop offset="0" stop-color="' + color + '" stop-opacity=".28"/><stop offset="1" stop-color="' + color + '" stop-opacity="0"/></linearGradient></defs>'
       + '<path d="' + area + '" fill="url(#' + id + ')"/>'
       + '<path d="' + line + '" fill="none" stroke="' + color + '" stroke-width="1.6" stroke-linejoin="round"/></svg>';
}
const STC = { '2xx':'var(--green)', '3xx':'#5eb4ff', '4xx':'var(--yellow)', '5xx':'var(--red)' };
function statusBarFromCodes(byCode) {
  const buckets = { '2xx':0, '3xx':0, '4xx':0, '5xx':0 };
  for (const [code, n] of Object.entries(byCode || {})) {
    const c = String(code);
    const k = c[0] === '2' ? '2xx' : c[0] === '3' ? '3xx' : c[0] === '4' ? '4xx' : c[0] === '5' ? '5xx' : null;
    if (k) buckets[k] += n;
  }
  const keys = ['2xx','3xx','4xx','5xx'];
  const total = keys.reduce((a,k) => a + buckets[k], 0) || 1;
  const bar = keys.map(k => { const p = buckets[k] / total * 100; return p <= 0 ? '' : '<i style="width:' + p + '%;background:' + STC[k] + '"></i>'; }).join('');
  const leg = keys.map(k => '<span><i class="sw" style="background:' + STC[k] + '"></i>' + k + ' ' + fmt(buckets[k]) + '</span>').join('');
  return '<div class="statusbar">' + bar + '</div><div class="legend">' + leg + '</div>';
}
function hostBarsFromObj(byHost) {
  const entries = Object.entries(byHost || {}).sort((a,b) => b[1] - a[1]).slice(0, 10);
  if (!entries.length) return '<p class="empty">No host traffic recorded.</p>';
  const max = entries[0][1] || 1;
  return '<div class="hostbar">' + entries.map(([host, n]) => {
    const w = Math.max(3, n / max * 100);
    return '<div class="hostrow"><span class="nm" title="' + esc(host) + '">' + esc(host) + '</span>'
         + '<div class="track"><i style="width:' + w + '%"></i></div>'
         + '<span class="rq">' + fmt(n) + '</span>'
         + '<span class="ep ok">—</span></div>';
  }).join('') + '</div>';
}

/* ---------- Routes ---------- */
async function renderRoutes() {
  const groups = await api('/api/routes');
  const el = $('#tab-routes');
  if (!groups.length) { el.innerHTML = emptyState(I.routes, 'No routes registered', 'Routes appear here as the proxy discovers Docker labels or static config entries.'); return; }
  let html = '<div class="subhead">' + I.routes + groups.length + ' active route' + (groups.length === 1 ? '' : 's') + '</div>';
  for (const g of groups) {
    const head = '<code>' + esc(g.host) + '</code>'
      + (g.path ? ' <code>' + esc(g.path) + '</code>' : '')
      + (g.strip ? ' <span class="tag" title="Strip path prefix before proxying">' + I.scissors + 'strip</span>' : '');
    const meta = g.backends.length + ' backend' + (g.backends.length === 1 ? '' : 's')
      + (g.service ? ' · service: <span class="ident dim">' + esc(g.service) + '</span>' : '');
    let rows = '';
    for (const b of g.backends) {
      const state = b.healthy === true ? 'up' : b.healthy === false ? 'down' : 'unknown';
      const err = b.last_error
        ? '<details class="errd"><summary>' + I.alert + 'error</summary><div class="err" style="margin-top:6px">' + esc(b.last_error) + '</div></details>'
        : '<span class="meta">—</span>';
      rows += '<tr><td>' + healthPill(state) + '</td>'
           +  '<td><span class="ident">' + esc(b.url) + '</span></td>'
           +  '<td class="num">' + b.weight + '</td>'
           +  '<td><span class="ident dim">' + esc(b.container || '') + '</span></td>'
           +  '<td style="max-width:260px">' + err + '</td></tr>';
    }
    html += '<div class="card"><div class="card-head"><div class="ttl">' + head + '</div>'
         +  '<div class="spacer"></div><div class="meta">' + meta + '</div></div>'
         +  '<table><thead><tr><th>Health</th><th>Backend</th><th>Weight</th><th>Container</th><th>Last error</th></tr></thead>'
         +  '<tbody>' + rows + '</tbody></table></div>';
  }
  el.innerHTML = html;
}

/* ---------- Services ---------- */
async function renderServices() {
  const svcs = await api('/api/services');
  const el = $('#tab-services');
  let html = '<div class="btn-row top">'
    + '<button class="btn primary" ' + lockedAttr() + ' onclick="document.getElementById(\'dlg-new-service\').showModal()">' + I.plus + 'New service' + lk() + '</button>'
    + '<span class="meta">' + svcs.length + ' managed service' + (svcs.length === 1 ? '' : 's') + '</span></div>';
  if (!svcs.length) { el.innerHTML = html + emptyState(I.services, 'No managed services', 'Deploy a container and the proxy will scale, canary, and roll it back from here.'); return; }
  for (const s of svcs) {
    const sn = esc(s.name);
    const canary = !!s.canary_image;
    let badges = '';
    if (s.update_available) badges += ' <span class="pill warn">' + I.arrowup + 'update available</span>';
    if (canary)             badges += ' <span class="pill info"><span class="gl"></span>canary live</span>';
    let facts = '<table class="facts">';
    facts += '<tr><td>Host</td><td><span class="ident">' + esc(s.host) + (s.path ? esc(s.path) : '') + '</span></td></tr>';
    if (canary)              facts += '<tr><td>Canary</td><td><span class="ident" style="color:#5eb4ff">' + esc(s.canary_image) + '</span> <span class="meta">· ' + s.canary_replicas + ' replica' + (s.canary_replicas === 1 ? '' : 's') + '</span></td></tr>';
    else if (s.previous_image) facts += '<tr><td>Previous</td><td><span class="ident dim">' + esc(s.previous_image) + '</span></td></tr>';
    facts += '<tr><td>Port</td><td class="num">' + s.port + '</td></tr>';
    facts += '<tr><td>Replicas</td><td>' + replicaCtrl(s) + '</td></tr>';
    facts += '</table>';

    let actions;
    if (canary) {
      actions = '<button class="btn primary" ' + lockedAttr() + ' onclick="promoteCanary(\'' + sn + '\')">' + I.check + 'Promote canary' + lk() + '</button>'
              + '<button class="btn" ' + lockedAttr() + ' onclick="discardCanary(\'' + sn + '\')">' + I.x + 'Discard' + lk() + '</button>';
    } else {
      actions = '<button class="btn primary" ' + lockedAttr() + ' onclick="openStage(\'' + sn + '\', \'' + esc(s.image) + '\')">' + I.rocket + 'Stage new version' + lk() + '</button>'
              + '<button class="btn" ' + lockedAttr() + ' onclick="openReplace(\'' + sn + '\', \'' + esc(s.image) + '\')">' + I.swap + 'Replace' + lk() + '</button>'
              + (s.previous_image ? '<button class="linkbtn" ' + lockedAttr() + ' onclick="rollback(\'' + sn + '\', \'' + esc(s.previous_image) + '\')">' + I.rewind + 'Rollback</button>' : '');
    }
    const menu = '<div class="menu"><button class="btn icon" onclick="toggleMenu(event,\'m-' + sn + '\')">' + I.dots + '</button>'
               + '<div class="menu-pop" id="m-' + sn + '"><button class="danger" ' + lockedAttr() + ' onclick="deleteSvc(\'' + sn + '\')">' + I.trash + 'Delete service</button></div></div>';

    html += '<div class="card">'
         +  '<div class="svc-head"><div class="ic">' + I.services + '</div>'
         +  '<div style="flex:1;min-width:0"><div class="svc-name">' + esc(s.name) + badges + '</div>'
         +  '<div class="svc-img">' + I.layers + '<b>' + esc(s.image) + '</b></div></div></div>'
         +  facts
         +  '<div class="actionzone">' + actions + '<div class="sep"></div>' + menu + '</div>'
         +  '</div>';
  }
  el.innerHTML = html;
}

function replicaCtrl(s) {
  if (s.unscalable) return '<span class="singleton-lock">' + I.lock + 'Singleton <span class="pill muted" style="margin-left:4px">fixed at 1</span></span>';
  const sn = esc(s.name);
  const dis = lockedAttr();
  return '<span class="replica-ctrl">'
       + '<button ' + dis + ' onclick="scaleSvc(\'' + sn + '\', ' + (s.replicas - 1) + ')">−</button>'
       + '<input type="number" min="0" value="' + s.replicas + '" id="rep-' + sn + '"' + (isElevated() ? '' : ' disabled') + '>'
       + '<button ' + dis + ' onclick="scaleSvc(\'' + sn + '\', ' + (s.replicas + 1) + ')">+</button>'
       + '<button class="apply" ' + dis + ' onclick="scaleSvc(\'' + sn + '\', +document.getElementById(\'rep-' + sn + '\').value)">Apply</button>'
       + '</span>';
}

function toggleMenu(e, id) {
  e.stopPropagation();
  const m = document.getElementById(id);
  const was = m.classList.contains('open');
  document.querySelectorAll('.menu-pop.open').forEach(x => x.classList.remove('open'));
  if (!was) m.classList.add('open');
}
document.addEventListener('click', () => { document.querySelectorAll('.menu-pop.open').forEach(x => x.classList.remove('open')); });

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
  $('#dlg-replace-service').querySelector('h3').textContent = 'Replace service';
  const sub = $('#dlg-replace-service').querySelector('.dsub');
  if (sub) sub.textContent = 'Swap the running image for ' + name;
  $('#dlg-replace-service').showModal();
}
function openStage(name, currentImage) {
  const f = $('#form-replace-service');
  f.serviceName.value = name;
  f.currentImage.value = currentImage;
  f.image.value = '';
  f.env.value = '';
  f.dataset.mode = 'stage';
  $('#dlg-replace-service').querySelector('h3').textContent = 'Stage new version (canary)';
  const sub = $('#dlg-replace-service').querySelector('.dsub');
  if (sub) sub.textContent = 'Deploy a canary alongside ' + name + ' for promotion';
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

async function deleteSvc(name) {
  if (!confirm('Delete service "' + name + '" and all its containers?')) return;
  try {
    await api('/api/services/' + encodeURIComponent(name), { method:'DELETE' });
    toast('deleted ' + name);
    renderActive();
  } catch (e) { toast(e.message, 'err'); }
}

/* ---------- DNS ---------- */
async function renderDNS() {
  const status = await api('/api/cf/enabled');
  const el = $('#tab-dns');
  if (!status.enabled) {
    el.innerHTML = emptyState(I.dns, 'Cloudflare not configured', 'Set CLOUDFLARE_API_TOKEN and CLOUDFLARE_ZONE_ID to manage DNS records from the dashboard.');
    return;
  }
  const recs = await api('/api/cf/records');
  let html = '<div class="btn-row top">'
    + '<button class="btn primary" ' + lockedAttr() + ' onclick="document.getElementById(\'dlg-new-dns\').showModal()">' + I.plus + 'New record' + lk() + '</button>'
    + (status.domain ? '<span class="pill muted">' + I.globe + 'zone ' + esc(status.domain) + '</span>' : '')
    + '<span class="meta">' + recs.length + ' record' + (recs.length === 1 ? '' : 's') + '</span></div>';
  const typeColor = { A:'#5eb4ff', AAAA:'#5eb4ff', CNAME:'var(--accent)', TXT:'var(--muted)', MX:'var(--yellow)' };
  let rows = '';
  for (const r of recs) {
    const tc = typeColor[r.type] || 'var(--muted)';
    rows += '<tr><td><span class="pill muted" style="color:' + tc + ';border-color:' + tc + '33"><b style="font-family:var(--font-mono)">' + esc(r.type) + '</b></span></td>'
         +  '<td><span class="ident">' + esc(r.name) + '</span></td>'
         +  '<td class="col-clip"><span class="ident dim">' + esc(r.content) + '</span></td>'
         +  '<td>' + (r.proxied ? '<span class="pill ok"><span class="gl"></span>proxied</span>' : '<span class="pill muted"><span class="gl"></span>dns only</span>') + '</td>'
         +  '<td><div class="btn-row" style="justify-content:flex-end">'
         +    '<button class="btn sm" ' + lockedAttr() + ' onclick="editDNS(\'' + esc(r.id) + '\', \'' + esc(r.content) + '\')">' + I.edit + 'Edit</button>'
         +    '<button class="btn sm danger" ' + lockedAttr() + ' onclick="deleteDNS(\'' + esc(r.id) + '\', \'' + esc(r.name) + '\')">' + I.trash + '</button>'
         +  '</div></td></tr>';
  }
  html += '<div class="card"><table><thead><tr><th>Type</th><th>Name</th><th>Content</th><th>Proxied</th><th style="text-align:right">Actions</th></tr></thead>'
       +  '<tbody>' + rows + '</tbody></table></div>';
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

/* ---------- Users + Tokens ---------- */
async function renderUsers() {
  const [users, myTokens] = await Promise.all([
    api('/api/users'),
    api('/api/users/tokens').catch(() => []),
  ]);
  const el = $('#tab-users');
  let html = '<div class="subhead">' + I.users + 'Dashboard users</div>';
  html += '<div class="btn-row top"><button class="btn primary" ' + lockedAttr() + ' onclick="openAddUser()">' + I.plus + 'Add user' + lk() + '</button></div>';
  html += '<div class="card"><table><thead><tr><th>Username</th><th>Created</th><th style="text-align:right">Actions</th></tr></thead><tbody>';
  for (const u of users) {
    const isMe = u.username === authState.username;
    html += '<tr><td><span class="ident">' + esc(u.username) + '</span>' + (isMe ? ' <span class="pill ok"><span class="gl"></span>you</span>' : '') + '</td>'
         +  '<td class="meta">' + new Date(u.created_at * 1000).toLocaleString() + '</td>'
         +  '<td style="text-align:right">'
         +    (isMe ? '<span class="meta">—</span>' : '<button class="btn sm danger" ' + lockedAttr() + ' onclick="deleteUser(\'' + esc(u.username) + '\')">' + I.trash + 'Delete</button>')
         +  '</td></tr>';
  }
  html += '</tbody></table></div>';

  html += '<div class="divider"></div>';
  html += '<div class="subhead">' + I.key + 'My API tokens</div>';
  html += '<div class="btn-row top"><button class="btn primary" ' + lockedAttr() + ' onclick="createToken()">' + I.plus + 'Generate token' + lk() + '</button>'
       +  '<span class="meta">Pass as <code>Authorization: Bearer pmt_…</code></span></div>';
  if (!myTokens.length) {
    html += '<div class="card"><div class="empty-state"><div class="es-ic">' + I.key + '</div>'
         +  '<h3>No tokens yet</h3><p>Create one to call this API from scripts (Uptime Kuma, cron jobs, anything).</p></div></div>';
  } else {
    html += '<div class="card"><table><thead><tr><th>Label</th><th>ID</th><th>Created</th><th>Last used</th><th style="text-align:right">Actions</th></tr></thead><tbody>';
    for (const t of myTokens) {
      html += '<tr><td><span class="ident">' + esc(t.label) + '</span></td>'
           +  '<td><span class="ident dim">' + esc(t.id) + '…</span></td>'
           +  '<td class="meta">' + new Date(t.created_at * 1000).toLocaleString() + '</td>'
           +  '<td class="meta">' + (t.last_used_at ? new Date(t.last_used_at * 1000).toLocaleString() : '—') + '</td>'
           +  '<td style="text-align:right"><button class="btn sm danger" ' + lockedAttr() + ' onclick="deleteToken(\'' + esc(t.id) + '\')">' + I.trash + 'Revoke</button></td></tr>';
    }
    html += '</tbody></table></div>';
  }
  html += '<div class="note info">' + I.activity + '<div><strong>Public health endpoint</strong> — no auth, returns up/degraded/down. Good for Uptime Kuma.'
       +  '<div class="copybar"><code id="health-url">' + window.location.origin + '/api/health</code>'
       +  '<button class="btn sm" onclick="copyText(document.getElementById(\'health-url\').textContent,this)">' + I.copy + 'Copy</button></div></div></div>';

  el.innerHTML = html;
}

function copyText(t, btn) {
  if (navigator.clipboard) navigator.clipboard.writeText(t);
  if (btn) { const o = btn.innerHTML; btn.innerHTML = I.check + 'Copied'; setTimeout(() => { btn.innerHTML = o; }, 1400); }
  toast('Copied to clipboard.');
}

async function createToken() {
  const label = prompt('Token label (e.g. "uptime-kuma", "deploy-script"):', '');
  if (label === null) return;
  try {
    const out = await api('/api/users/tokens', { method:'POST', body: JSON.stringify({ label })});
    tokenReveal(out.token);
    renderActive();
  } catch (e) { toast(e.message, 'err'); }
}
function tokenReveal(raw) {
  const d = document.getElementById('dlg-token-reveal');
  d.innerHTML = '<div class="dlg"><div class="dlg-head"><div class="di">' + I.key + '</div>'
    + '<div><h3>API token created</h3><div class="dsub">Copy it now — it will not be shown again</div></div>'
    + '<button class="x" type="button" onclick="document.getElementById(\'dlg-token-reveal\').close()">' + I.x + '</button></div>'
    + '<div class="dlg-body">'
    + '<div class="note">' + I.alert + '<div><strong>Save this now</strong> — the secret is shown only once. Store it somewhere safe.</div></div>'
    + '<div class="token-block" id="tr-raw" style="margin-top:14px">' + esc(raw) + '</div>'
    + '<button class="btn" style="margin-top:12px" onclick="copyText(document.getElementById(\'tr-raw\').textContent,this)">' + I.copy + 'Copy token</button>'
    + '</div><div class="dialog-actions"><button class="btn primary" onclick="document.getElementById(\'dlg-token-reveal\').close()">' + I.check + 'I\'ve saved it</button></div></div>';
  d.showModal();
}
async function deleteToken(id) {
  if (!confirm('Revoke this token? Anything using it will stop working.')) return;
  try {
    await api('/api/users/tokens/' + encodeURIComponent(id), { method:'DELETE' });
    toast('revoked'); renderActive();
  } catch (e) { toast(e.message, 'err'); }
}

function openAddUser() {
  pendingActive = false;
  $('#add-user-body').innerHTML = '<div class="dlg-head"><div class="di">' + I.users + '</div>'
    + '<div><h3>Add user</h3><div class="dsub">Step 1 of 2 — credentials</div></div>'
    + '<button class="x" type="button" onclick="document.getElementById(\'dlg-add-user\').close()">' + I.x + '</button></div>'
    + '<form id="form-add-user"><div class="dlg-body">'
    + '<div class="field"><label>Username</label><input name="username" required pattern="[a-zA-Z0-9._-]{2,32}" autofocus></div>'
    + '<div class="field"><label>Initial password (8+ chars)</label><input name="password" type="password" minlength="8" required></div>'
    + '</div><div class="dialog-actions">'
    + '<button type="button" class="btn" onclick="document.getElementById(\'dlg-add-user\').close()">Cancel</button>'
    + '<button type="submit" class="btn primary">' + I.shield + 'Generate 2FA secret</button>'
    + '</div></form>';
  document.getElementById('dlg-add-user').showModal();
  $('#form-add-user').onsubmit = async (e) => {
    e.preventDefault();
    const f = e.target;
    try {
      const out = await api('/api/users', { method:'POST', body: JSON.stringify({
        username: f.username.value.trim(), password: f.password.value,
      })});
      pendingActive = true;
      $('#add-user-body').innerHTML = '<div class="dlg-head"><div class="di">' + I.shield + '</div>'
        + '<div><h3>Scan to enroll</h3><div class="dsub">Step 2 of 2 — confirm 2FA</div></div>'
        + '<button class="x" type="button" onclick="document.getElementById(\'dlg-add-user\').close()">' + I.x + '</button></div>'
        + '<div class="dlg-body">' + pendingUserBlock(out, 'adduser') + '</div>';
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

/* ---------- Stats (monitor binary) ---------- */
async function renderStats() {
  if (statsDetail) { return renderStatsDetail(statsDetail); }
  const el = $('#tab-stats');
  let overview, certs;
  try {
    [overview, certs] = await Promise.all([
      api('/api/monitor/overview'),
      api('/api/monitor/certs').catch(() => ({ enabled: false, certs: [] })),
    ]);
  } catch (e) {
    el.innerHTML = emptyState(I.activity, 'Monitor unreachable', 'The traffic monitor is not responding. Make sure the monitor container is running. Health and metrics will resume when it recovers.');
    return;
  }
  const targets = overview.targets || [];
  const live    = targets.filter(t => t.health !== 'absent');
  const absent  = targets.filter(t => t.health === 'absent');
  const healthy = targets.filter(t => t.health === 'up').length;
  const degraded= targets.filter(t => t.health === 'flaky' || t.health === 'down').length;
  const inFlight= live.reduce((a,t) => a + (t.in_flight || 0), 0);
  const p95s    = live.map(t => (t.latency_ms && t.latency_ms.p95) ? t.latency_ms.p95 : 0).filter(v => v > 0);
  const avgP95  = p95s.length ? Math.round(p95s.reduce((a,b) => a+b, 0) / p95s.length) : 0;
  const totErrors = live.reduce((a,t) => {
    const by = t.by_status || {};
    let n = 0;
    for (const [c, v] of Object.entries(by)) {
      const f = String(c)[0];
      if (f === '4' || f === '5') n += v;
    }
    return a + n;
  }, 0);
  const totalReqs = overview.total_requests || live.reduce((a,t) => a + (t.total || 0), 0);
  const errPct = totalReqs > 0 ? (totErrors / totalReqs) * 100 : 0;

  const stackPill = overview.health === 'up'
    ? '<span class="pill ok"><span class="gl"></span>all healthy</span>'
    : '<span class="pill warn">' + I.alert + 'degraded</span>';

  const hero = '<div class="grid k4">'
    + kpi(I.shield, 'Healthy targets', '<span class="num">' + healthy + '</span><small>/ ' + live.length + '</small>',
        overview.health === 'up'
          ? '<span class="up">' + I.check + 'all systems normal</span>'
          : '<span class="down">' + I.alert + degraded + ' degraded</span>',
        true)
    + kpi(I.activity, 'Total requests', '<span class="num">' + fmt(totalReqs) + '</span>', '<span>' + I.clock + 'lifetime</span>')
    + kpi(I.bolt,     'In flight',      '<span class="num">' + inFlight + '</span>',       '<span>across all targets</span>')
    + kpi(I.clock,    'Avg p95 latency','<span class="num">' + avgP95 + '</span><small>ms</small>',
        '<span class="' + (errPct > 1 ? 'down' : 'up') + '">' + pct(errPct) + ' error rate</span>')
    + '</div>';

  let html = '<div class="card"><div class="card-head"><div class="ttl">' + I.layers + 'Stack health</div>'
           + stackPill + '<div class="spacer"></div>'
           + '<div class="meta">' + healthy + ' up · ' + degraded + ' degraded' + (absent.length ? ' · ' + absent.length + ' not deployed' : '') + '</div></div>'
           + hero + '</div>';

  html += '<div class="subhead">' + I.globe + 'Targets</div>';
  for (const t of targets) {
    const isAbsent = t.health === 'absent';
    const click = isAbsent ? '' : 'onclick="openTarget(\'' + esc(t.name) + '\')" style="cursor:pointer"';
    const p95 = (t.latency_ms && t.latency_ms.p95 != null) ? t.latency_ms.p95.toFixed(1) : '—';
    const total = t.total || 0;
    const tErr  = (() => { const by = t.by_status || {}; let n = 0; for (const [c, v] of Object.entries(by)) { const f = String(c)[0]; if (f === '4' || f === '5') n += v; } return n; })();
    const tErrPct = total > 0 ? (tErr / total * 100) : 0;
    const nums = isAbsent ? '' : '<div class="grid k4" style="margin:10px 0 6px">'
        + kpiSm('Requests', fmt(total))
        + kpiSm('In flight', String(t.in_flight || 0))
        + kpiSm('p95',     p95 + ' <small>ms</small>')
        + kpiSm('Errors',  pct(tErrPct))
        + '</div>';
    const detail = isAbsent
      ? '<div class="meta" style="padding:10px 0 0">Not deployed — no metrics.</div>'
      : (t.by_status ? '<div style="margin-top:8px">' + statusBarFromCodes(t.by_status) + '</div>' : '');
    html += '<div class="card" ' + click + '><div class="card-head"><div class="ttl">' + I.globe + '<span class="ident" style="font-size:14px">' + esc(t.name) + '</span></div>'
         +  healthPill(t.health) + '<div class="spacer"></div>'
         +  '<div class="meta"><span class="ident dim">' + esc(t.url || '') + '</span></div></div>'
         +  nums + detail
         +  '</div>';
  }

  // ---- TLS certs (probed) ----
  if (certs && certs.enabled) {
    const worstPill = certs.worst_status === 'ok'
      ? '<span class="pill ok"><span class="gl"></span>all good</span>'
      : certs.worst_status === 'warning'
      ? '<span class="pill warn">' + I.alert + 'renew soon</span>'
      : '<span class="pill bad">' + I.x + esc(certs.worst_status || 'issue') + '</span>';
    html += '<div class="card"><div class="card-head"><div class="ttl">' + I.shield + 'TLS certs</div>'
         +  worstPill + '<div class="spacer"></div>'
         +  '<div class="meta">' + (certs.certs || []).length + ' probed</div></div>'
         +  '<table><thead><tr><th>Host</th><th>Issuer</th><th>Expires</th><th>Days left</th><th>Status</th></tr></thead><tbody>';
    for (const c of (certs.certs || [])) {
      const cls = c.status === 'ok' ? 'ok'
        : c.status === 'warning' ? 'warn'
        : (c.status === 'critical' || c.status === 'expired') ? 'bad'
        : 'muted';
      html += '<tr><td><span class="ident">' + esc(c.host) + '</span></td>'
           +  '<td class="meta">' + esc((c.issuer || '—').slice(0, 60)) + '</td>'
           +  '<td class="meta">' + (c.not_after ? new Date(c.not_after).toLocaleDateString() : '—') + '</td>'
           +  '<td>' + (c.days_left != null ? c.days_left : '—') + '</td>'
           +  '<td><span class="pill ' + cls + '">' + esc(c.status) + '</span>'
           +    (c.error ? ' <span class="err">' + esc((c.error || '').slice(0, 80)) + '</span>' : '')
           +  '</td></tr>';
    }
    html += '</tbody></table></div>';
  }

  el.innerHTML = html;
}
function openTarget(n) { statsDetail = n; renderActive(); }
function closeTarget() { statsDetail = null; renderActive(); }
function degradedColor(h) { return h === 'down' ? 'var(--red)' : h === 'flaky' ? 'var(--yellow)' : 'var(--accent)'; }

async function renderStatsDetail(name) {
  const el = $('#tab-stats');
  let t, hosts, series;
  try {
    [t, hosts, series] = await Promise.all([
      api('/api/monitor/target/' + encodeURIComponent(name)),
      api('/api/monitor/target/' + encodeURIComponent(name) + '/hosts').catch(() => []),
      api('/api/monitor/series?target=' + encodeURIComponent(name) + '&field=delta').catch(() => []),
    ]);
  } catch (e) {
    el.innerHTML = '<button class="linkbtn" style="margin-bottom:14px" onclick="closeTarget()">' + I.rewind + 'Back to targets</button>'
                 + emptyState(I.activity, 'Target unavailable', 'The monitor could not return details for "' + name + '". It may have just gone away.');
    return;
  }
  const m       = t.metrics || {};
  const rate1   = +(t.rate_per_sec_1m || 0);
  const rate5   = +(t.rate_per_sec_5m || 0);
  const p95     = (m.latency_ms && m.latency_ms.p95 != null) ? m.latency_ms.p95 : 0;
  const total   = m.total || 0;
  const inFlight= m.in_flight || 0;
  const errPct  = +(t.error_pct_recent || 0);
  const byMethod= m.by_method || {};
  const byStatus= m.by_status || {};
  const color   = degradedColor(t.health);
  const sparkData = (series || []).map(p => +p.v || 0);

  const hero = '<div class="card"><div class="card-head">'
    + '<div class="ttl">' + I.globe + '<span class="ident" style="font-size:16px">' + esc(t.name) + '</span></div>'
    + healthPill(t.health) + '<div class="spacer"></div>'
    + '<div class="meta">rate ' + rate1.toFixed(1) + ' req/s (1m) · ' + rate5.toFixed(1) + ' req/s (5m)</div></div>'
    + '<div class="grid k4">'
    + kpi(I.activity, 'Total requests', '<span class="num">' + fmt(total) + '</span>', '<span>' + I.clock + 'lifetime</span>')
    + kpi(I.bolt,     'In flight',      '<span class="num">' + inFlight + '</span>',     '<span>concurrent</span>')
    + kpi(I.clock,    'p95 latency',    '<span class="num">' + (p95 ? p95.toFixed(0) : '—') + '</span><small>ms</small>', '<span>tail response time</span>')
    + kpi(I.shield,   'Error rate',     '<span class="num">' + pct(errPct) + '</span>',
        '<span class="' + (errPct > 1 ? 'down' : 'up') + '">' + (errPct > 1 ? 'above' : 'within') + ' target</span>')
    + '</div>'
    + (sparkData.length
        ? '<div class="subhead" style="margin-top:18px">' + I.activity + 'Request rate · recent samples</div>'
          + sparkline(sparkData, 1100, 90, color)
        : '')
    + '</div>';

  const methodRows = Object.keys(byMethod).length
    ? Object.entries(byMethod).sort((a,b) => b[1] - a[1]).map(([k, v]) =>
        '<tr><td><span class="pill muted"><b style="font-family:var(--font-mono)">' + esc(k) + '</b></span></td>'
      + '<td class="num" style="text-align:right">' + fmt(v) + '</td></tr>').join('')
    : '<tr><td colspan="2"><span class="empty">No method breakdown.</span></td></tr>';

  const dist = '<div class="grid k2">'
    + '<div class="card"><div class="card-head"><div class="ttl">' + I.layers + 'Status distribution</div></div>'
    +   (Object.keys(byStatus).length ? statusBarFromCodes(byStatus) : '<p class="empty">No status data.</p>')
    + '</div>'
    + '<div class="card"><div class="card-head"><div class="ttl">' + I.swap + 'By method</div></div>'
    +   '<table><tbody>' + methodRows + '</tbody></table>'
    + '</div></div>';

  let topHostsHtml;
  if (hosts && hosts.length) {
    const max = hosts.reduce((a, h) => Math.max(a, +h.total || 0), 0) || 1;
    const rows = hosts.slice(0, 10).map(h => {
      const tot = +h.total || 0;
      const ep  = +h.error_pct || 0;
      const w   = Math.max(3, tot / max * 100);
      const epCls = ep > 0 ? 'bad' : 'ok';
      const epTxt = ep > 0 ? ep.toFixed(1) + '%' : '—';
      return '<div class="hostrow"><span class="nm" title="' + esc(h.host) + '">' + esc(h.host) + '</span>'
           + '<div class="track"><i style="width:' + w + '%"></i></div>'
           + '<span class="rq">' + fmt(tot) + '</span>'
           + '<span class="ep ' + epCls + '">' + epTxt + '</span></div>';
    }).join('');
    topHostsHtml = '<div class="card"><div class="card-head"><div class="ttl">' + I.globe + 'Top hosts</div>'
                 + '<div class="spacer"></div><div class="meta">by request volume</div></div>'
                 + '<div class="hostrow" style="color:var(--muted);font-size:10.5px;letter-spacing:.06em;text-transform:uppercase;margin-bottom:8px">'
                 +   '<span>Host</span><span></span><span class="rq">Req</span><span class="ep">Err</span></div>'
                 + '<div class="hostbar">' + rows + '</div></div>';
  } else {
    topHostsHtml = '';
  }

  el.innerHTML = '<button class="linkbtn" style="margin-bottom:14px" onclick="closeTarget()">' + I.rewind + 'Back to targets</button>'
               + hero + dist + topHostsHtml;
}

/* ---------- Build static dialog content once on load ---------- */
function buildDialogs() {
  // New service
  $('#dlg-new-service').innerHTML =
    '<div class="dlg"><div class="dlg-head"><div class="di">' + I.rocket + '</div>'
    + '<div><h3>New service</h3><div class="dsub">Deploy a managed container behind the proxy</div></div>'
    + '<button class="x" type="button" onclick="document.getElementById(\'dlg-new-service\').close()">' + I.x + '</button></div>'
    + '<form id="form-new-service"><div class="dlg-body">'
    + '<div class="field-group"><div class="gl-title">' + I.layers + 'Identity</div>'
    +   '<div class="field"><label>Service name</label><input name="name" pattern="[a-z0-9-]+" placeholder="myapp" required><div class="hint">Lowercase letters, numbers, dashes.</div></div>'
    +   '<div class="field"><label>Image</label><input name="image" placeholder="ghcr.io/org/app:tag" required></div></div>'
    + '<div class="field-group"><div class="gl-title">' + I.globe + 'Networking</div>'
    +   '<div class="field"><label>Hostname</label><input name="host" placeholder="app.polardev.org" required></div>'
    +   '<div class="field"><label>Container port</label><input name="port" type="number" placeholder="3000" required></div></div>'
    + '<div class="field-group"><div class="gl-title">' + I.cpu + 'Runtime</div>'
    +   '<div class="field-row"><div class="field"><label>Replicas</label><input name="replicas" type="number" min="0" value="1"></div>'
    +   '<div class="field check" style="align-items:center;padding-top:24px"><input type="checkbox" name="unscalable" id="nsv-single"><label for="nsv-single">Singleton (db, bot)</label></div></div>'
    +   '<div class="hint" style="margin:-6px 0 12px">Singletons run exactly one replica and cannot be scaled.</div>'
    +   '<div class="field"><label>Environment (KEY=VALUE per line)</label><textarea name="env" placeholder="DATABASE_URL=postgres://...&#10;PORT=3000"></textarea></div></div>'
    + '</div><div class="dialog-actions">'
    +   '<button type="button" class="btn" onclick="document.getElementById(\'dlg-new-service\').close()">Cancel</button>'
    +   '<button type="submit" class="btn primary">' + I.check + 'Create</button>'
    + '</div></form></div>';

  // Replace / stage
  $('#dlg-replace-service').innerHTML =
    '<div class="dlg"><div class="dlg-head"><div class="di">' + I.swap + '</div>'
    + '<div><h3>Replace service</h3><div class="dsub">Spins up new replicas, waits, then removes the old. Env is copied unless overridden.</div></div>'
    + '<button class="x" type="button" onclick="document.getElementById(\'dlg-replace-service\').close()">' + I.x + '</button></div>'
    + '<form id="form-replace-service" data-mode="replace"><div class="dlg-body">'
    + '<input type="hidden" name="serviceName">'
    + '<div class="field"><label>Current image</label><input name="currentImage" disabled></div>'
    + '<div class="field"><label>New image</label><input name="image" placeholder="ghcr.io/org/app:tag" required></div>'
    + '<div class="field"><label>Env override <span class="hint" style="display:inline">(KEY=VALUE per line — leave blank to keep current)</span></label><textarea name="env"></textarea></div>'
    + '</div><div class="dialog-actions">'
    +   '<button type="button" class="btn" onclick="document.getElementById(\'dlg-replace-service\').close()">Cancel</button>'
    +   '<button type="submit" class="btn primary">' + I.check + 'Replace</button>'
    + '</div></form></div>';

  // New DNS
  $('#dlg-new-dns').innerHTML =
    '<div class="dlg"><div class="dlg-head"><div class="di">' + I.dns + '</div>'
    + '<div><h3>New DNS record</h3><div class="dsub">Create a record in the Cloudflare zone</div></div>'
    + '<button class="x" type="button" onclick="document.getElementById(\'dlg-new-dns\').close()">' + I.x + '</button></div>'
    + '<form id="form-new-dns"><div class="dlg-body">'
    + '<div class="field-row">'
    +   '<div class="field"><label>Type</label><select name="type"><option>CNAME</option><option>A</option><option>AAAA</option><option>TXT</option><option>MX</option></select></div>'
    +   '<div class="field"><label>Name</label><input name="name" placeholder="subdomain" required></div>'
    + '</div>'
    + '<div class="field"><label>Content (target)</label><input name="content" placeholder="tunnel.example.com" required></div>'
    + '<div class="field check"><input type="checkbox" name="proxied" id="dns-proxied" checked><label for="dns-proxied">Proxy through Cloudflare<div class="hint">Orange-cloud: hides origin IP and adds CDN/TLS.</div></label></div>'
    + '</div><div class="dialog-actions">'
    +   '<button type="button" class="btn" onclick="document.getElementById(\'dlg-new-dns\').close()">Cancel</button>'
    +   '<button type="submit" class="btn primary">' + I.check + 'Create</button>'
    + '</div></form></div>';

  // 2FA
  $('#dlg-2fa').innerHTML =
    '<div class="dlg"><div class="dlg-head"><div class="di">' + I.shield + '</div>'
    + '<div><h3>Confirm with 2FA</h3><div class="dsub">Unlocks edit access for 5 minutes</div></div>'
    + '<button class="x" type="button" onclick="document.getElementById(\'dlg-2fa\').close()">' + I.x + '</button></div>'
    + '<form id="form-2fa"><div class="dlg-body">'
    + '<p class="meta" style="margin:0 0 14px">Enter the 6-digit code from your authenticator app.</p>'
    + '<div class="field"><input name="code" inputmode="numeric" pattern="[0-9]{6}" maxlength="6" required class="otp-input" autocomplete="one-time-code" placeholder="••••••"></div>'
    + '</div><div class="dialog-actions">'
    +   '<button type="button" class="btn" onclick="document.getElementById(\'dlg-2fa\').close()">Cancel</button>'
    +   '<button type="submit" class="btn primary">' + I.check + 'Confirm</button>'
    + '</div></form></div>';

  wireDialogForms();
}

function wireDialogForms() {
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
      $('#dlg-replace-service').querySelector('h3').textContent = 'Replace service';
      const sub = $('#dlg-replace-service').querySelector('.dsub');
      if (sub) sub.textContent = 'Spins up new replicas, waits, then removes the old. Env is copied unless overridden.';
      renderActive();
    } catch (e) { toast(e.message, 'err'); }
  };

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

  $('#form-new-dns').onsubmit = async (e) => {
    e.preventDefault();
    const f = e.target;
    try {
      await api('/api/cf/records', { method:'POST', body: JSON.stringify({
        type: f.type.value, name: f.name.value, content: f.content.value, proxied: f.proxied.checked,
      })});
      toast('created'); $('#dlg-new-dns').close(); f.reset(); f.proxied.checked = true; renderActive();
    } catch (e) { toast(e.message, 'err'); }
  };
}

/* ---------- sys-stats (CPU / Mem / Disk) ---------- */
function statClass(pct) { return pct >= 90 ? ' bad' : pct >= 75 ? ' warn' : ''; }
function tileMarkup(icon, label, valHtml, p) {
  const cls = statClass(p);
  const w = Math.min(100, p).toFixed(1);
  return '<div class="sysstat' + cls + '" title="' + esc(label + ': ' + (p.toFixed ? p.toFixed(1) : p) + '%') + '">'
       + '<div class="ico">' + icon + '</div>'
       + '<div class="body"><div class="label">' + label + '</div>'
       + '<div class="val">' + valHtml + '</div>'
       + '<div class="bar"><span style="width:' + w + '%"></span></div></div></div>';
}
async function refreshStats() {
  if (!authState.authenticated) { $('#sys-stats').textContent = ''; return; }
  try {
    const s = await (await fetch('/api/stats')).json();
    const cpuPct  = Math.max(0, Math.min(100, s.cpu_pct || 0));
    const memPct  = s.mem_total  ? 100 * s.mem_used  / s.mem_total  : 0;
    const diskPct = s.disk_total ? 100 * s.disk_used / s.disk_total : 0;
    const memGB   = (s.mem_used  / 1073741824).toFixed(1);
    const memTotG = (s.mem_total / 1073741824).toFixed(1);
    const diskGB  = (s.disk_used  / 1073741824).toFixed(1);
    const diskTotT= (s.disk_total / 1099511627776).toFixed(1);
    $('#sys-stats').innerHTML =
        tileMarkup(I.cpu,  'CPU',    '<span class="num">' + cpuPct.toFixed(0) + '</span><small>% load</small>', cpuPct)
      + tileMarkup(I.mem,  'Memory', '<span class="num">' + memGB + '</span><small>/ ' + memTotG + ' GB used · ' + fmtBytes(s.mem_free) + ' free</small>', memPct)
      + tileMarkup(I.disk, 'Disk',   '<span class="num">' + diskGB + '</span><small>/ ' + diskTotT + ' TB used · ' + fmtBytes(s.disk_free) + ' free</small>', diskPct);
  } catch (e) { /* silent */ }
}

/* ---------- utilities ---------- */
function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
}
function fmt(n) {
  n = +n || 0;
  if (n >= 1e9) return (n/1e9).toFixed(1) + 'B';
  if (n >= 1e6) return (n/1e6).toFixed(1) + 'M';
  if (n >= 1e3) return (n/1e3).toFixed(1) + 'k';
  return String(n);
}
function pct(n) { return (Math.round(n * 10) / 10) + '%'; }
function fmtUptime(s) {
  if (s < 60) return s + 's';
  if (s < 3600) return Math.floor(s/60) + 'm ' + (s%60) + 's';
  if (s < 86400) return Math.floor(s/3600) + 'h ' + Math.floor((s%3600)/60) + 'm';
  return Math.floor(s/86400) + 'd ' + Math.floor((s%86400)/3600) + 'h';
}
function fmtBytes(n) {
  if (!n) return '—';
  if (n >= 1e12) return (n/1e12).toFixed(1) + ' TB';
  if (n >= 1e9)  return (n/1e9).toFixed(1) + ' GB';
  if (n >= 1e6)  return (n/1e6).toFixed(0) + ' MB';
  return n + ' B';
}

/* ---------- boot ---------- */
buildDialogs();
setInterval(() => { refreshAuth().catch(()=>{}); }, 30000);
setInterval(renderActive, 5000);
setInterval(refreshStats, 5000);
refreshAuth();
refreshStats();
</script>
</body>
</html>`
