# proxy-manager dashboard — design brief

## What you're redesigning

A self-hosted reverse-proxy / Docker-service control panel served by a single Go binary at a private LAN URL. The audience is a homelab or small-team operator who logs in, watches live system stats (CPU/MEM/DISK), inspects routes the proxy is serving, scales/deploys managed services (with canary + rollback), edits Cloudflare DNS records, manages dashboard users and API tokens, and reads per-target traffic metrics. The current UI is a competent but anonymous GitHub-dark dashboard. The aesthetic goal is a polished, opinionated operator console that feels alive (live data, clear health), legible at a glance (real hierarchy, real KPIs, real iconography), and trustworthy when it locks down destructive actions behind 2FA. Think "Linear meets Grafana for one person's homelab" — not a generic admin template.

## Hard constraints — read first

- **Single file, single string.** The entire UI is one Go raw-string literal `const dashboardHTML = ` ... `` in `dashboard/ui.go`. You output the full replacement file (about 900 lines), starting with `package main` then the const. No new Go files, no new symbols, no package change.
- **No external network assets.** No `<link rel="stylesheet">`, no `<script src="...">`, no CDN (jsdelivr, unpkg, fonts.googleapis, cloudflare, etc.), no web fonts. Use the existing system-font stack only.
- **No frameworks, no build step.** No React/Vue/Svelte/Solid/Preact/Alpine/htmx/jQuery. No Tailwind/Bootstrap/Pico. No TypeScript/JSX/SCSS. No `<script type="module">`. One inline `<style>` in `<head>`, one inline classic `<script>` before `</body>`.
- **Backticks are forbidden inside the Go raw string.** Go raw strings (`` `...` ``) cannot contain a literal backtick. The current file uses JS template literals (which require backticks) by closing the raw string, concatenating `"\`"`, then reopening: `` ` + "\`" + ` ``. Every JS template literal you introduce MUST use that same escape pattern. CSS or JS that would use a backtick (template literals, etc.) must either avoid the backtick or use this concat trick.
- **No server-side templating.** The Go binary serves the constant verbatim — no `html/template`, no `text/template`. `{{ }}`, `<%= %>`, and bare `${...}` outside a JS template literal render as literal text. All dynamic content is built client-side via JS string concatenation into `.innerHTML`. The existing `esc()` helper escapes `&<>"'` — wrap every user-controlled value with it.
- **Preserve every DOM id, form input name, onclick handler name, data-tab value, and CSS class listed in the DOM contract below.** Renaming any of them breaks the JS. See the DOM contract section.
- **Keep `<dialog>` as native `<dialog>` elements.** JS calls `.showModal()` and `.close()`, which only exist on `HTMLDialogElement`. Do not replace dialogs with custom overlay divs.
- **Keep tab containers using the `hidden` attribute, not a CSS class.** `switchTab` toggles `el.hidden` directly.
- **Routes tab must be the initial active tab.** The `data-tab="routes"` nav button must have `class="active"` on first paint. `#auth-screen` and `#main-screen` must both start with the `hidden` attribute set.
- **Auto-refresh is 5s.** `setInterval(renderActive, 5000)` and `setInterval(refreshStats, 5000)` wipe and rebuild `#tab-*` and `#sys-stats` via `innerHTML`. Anything stateful you add (open menus, focused inputs in the active tab) will be destroyed on each tick. Keep dynamic markup cheap. Do NOT add a competing refresher that ignores the `pendingActive` flag (which suppresses re-render during the TOTP-confirm step so the QR doesn't get wiped mid-flow).
- **Inline event handlers must remain global.** `onclick="logout()"` etc. require the handler to be a global function. Keep all JS at the top level of the single `<script>` block — no IIFE, no module scope.
- **Onclick single-quote escaping must be preserved.** Dynamic HTML uses the pattern `onclick="scaleSvc('` + esc(name) + `', 1)"` built via `'...onclick="scaleSvc(\\'' + esc(s.name) + '\\', 1)"'`. Keep this discipline anywhere you generate onclick attributes with string arguments.

## Current design tokens

| Token | Value | Role |
|---|---|---|
| `--bg` | `#0a0d12` | Page bg; also input/inset bg |
| `--surface` | `#11161d` | Primary card / nav / dialog surface |
| `--surface-2` | `#161c25` | Hover state for nav, default `.btn` bg, muted pill bg |
| `--border` | `#232a35` | Default 1px border (cards, inputs, table rows) |
| `--border-2` | `#2e3744` | Stronger border (default `.btn`, dialog, card hover) |
| `--text` | `#e6edf3` | Primary foreground |
| `--muted` | `#8b95a4` | Secondary text (labels, meta, table headers, inactive nav) |
| `--accent` | `#58a6ff` | Link color, h1 gradient endpoint, bar fill start |
| `--accent-2` | `#3b82f6` | Active tab bg, `.btn.primary` bg, input focus ring |
| `--green` | `#3fb950` | Success / healthy fg |
| `--green-bg` | `rgba(63,185,80,.12)` | Success pill/toast tint |
| `--red` | `#f85149` | Error / down fg, danger button hover bg |
| `--red-bg` | `rgba(248,81,73,.12)` | Error pill/toast tint |
| `--yellow` | `#f0c849` | Warning fg, note strong color |
| `--yellow-bg` | `rgba(240,200,73,.13)` | Warning pill/note tint |
| `--orange` | `#f0883e` | Inline `<code>` color |
| `--radius` | `10px` | Standard card / nav radius |
| `--shadow` | `0 1px 0 rgba(255,255,255,.02) inset, 0 8px 32px -12px rgba(0,0,0,.6)` | Card elevation |

**Body font**: `-apple-system, BlinkMacSystemFont, "Inter", "Segoe UI", system-ui, sans-serif` at 14px/1.5.
**Mono font**: `ui-monospace, "SF Mono", Menlo, monospace` (unify to one `--font-mono` token).

Known token gaps to fix in the redesign: hardcoded blues (`#82baff`, `#2563eb`, `#1f6feb` for the canary pill), hardcoded warn/bad bar gradient stops (`#fde68a`, `#fca5a5`), `.err` reusing `#fca5a5`, ad-hoc pill border alphas (`rgba(... ,.25–.3)`), `dialog::backdrop` using a one-off `rgba(5,8,12,.7)`. Add semantic tokens — at minimum: `--info`, `--info-bg`, `--green-border`, `--red-border`, `--yellow-border`, `--info-border`, `--font-mono`, `--radius-sm`, `--radius-md`, `--radius-lg`, `--radius-pill`, `--transition-fast` (~.12s), `--transition-slow` (~.4s), `--shadow-dialog`, `--shadow-toast`, `--focus-ring`.

## Component inventory

### Auth shell (visible only when not signed in)
- **First-time Setup** — phase 1: `form#form-setup` with username (`pattern [a-zA-Z0-9._-]{2,32}`) + password (min 8) → submit fills `#setup-result` with the pending-user block. Phase 2 (pendingUserBlock): QR `<img>`, otpauth URI link, `<details>` revealing TOTP secret in a `.totp-secret` block, `.note` warning, `form#form-confirm-setup` with one `.otp-input` (name=`code`).
- **Login** — `form#form-login` with username, password, and optional 6-digit code (`.otp-input`, name=`code`). 2FA is optional but required to elevate edit rights.
- Both rendered inside `.card.full-card` (max-width 520px, centered) inside `#auth-content` inside `#auth-screen`.

### Header / shell (visible on all main tabs)
- `<h1>` "Pi Dashboard" with gradient text.
- `#sys-stats` injecting three `.sysstat` blocks every 5s (CPU/MEM/DISK), each with an inline SVG icon (`ICON_CPU` / `ICON_MEM` / `ICON_DISK`), `.label`, `.val`, `.bar > span` progress. Modifier classes `.sysstat.warn` (>=75%) and `.sysstat.bad` (>=90%).
- `#status` meta line: "updated HH:MM:SS" or "error: ...".
- `#who` username; Logout `.btn` calling `logout()`.
- `<nav>` with 5 direct `<button>` children: `data-tab="routes|services|dns|users|stats"`, one with `.active`.

### Routes tab (`#tab-routes`)
- One `.card` per route group. `.card-head` = `<code>host</code> <code>path</code>` + optional `.tag` "strip" + meta line "N backend(s) · service: ...".
- Backends `<table>`: Health | Backend | Weight | Container | Last error. Health column uses `.pill ok | bad | muted`. Last error rendered in `.err`.
- Empty state: `<p class="empty">No routes registered.</p>`.

### Services tab (`#tab-services`)
- `.btn-row` at top with primary `+ New service` button (locked via `lockedAttr()` when not elevated).
- Empty state when none.
- Per-service `.card`:
  - `.card-head` = service name + optional `.pill.warn` "update available" + optional canary "live" badge (currently inline-styled blue — change to a real `.pill.info`).
  - Facts `<table>`: Image, Canary (when present), Previous (when present and no canary), Host (+optional path), Port, Replicas. The Replicas row holds `.replica-ctrl` = `[−] [number input id="rep-<serviceName>"] [+] [Apply]` + optional `.pill.warn` "singleton".
  - Right-aligned `.btn-row` of actions: either `Promote canary (.primary)` + `Discard canary`, or `Stage new version (.primary)` + `Replace` + optional `Rollback to <image>`, plus `Delete service (.danger)`.

### DNS tab (`#tab-dns`)
- Disabled state (no Cloudflare creds) shows an empty message.
- `.btn-row` with primary `+ New record` button + zone meta indicator.
- Single `.card` wrapping a `<table>`: Type | Name | Content | Proxied | Actions. Type as `.pill.muted`, Proxied as `.pill.ok` ("on") or `.pill.muted` ("off"), per-row `Edit (.btn)` + `Delete (.btn.danger)`.

### Users tab (`#tab-users`)
- `.btn-row` with primary `+ Add user`.
- Users `.card`: `<table>` Username | Created | Actions, with `.pill.ok` "you" marker and `Delete (.btn.danger)` for other users.
- "My API tokens" `.card`: primary `+ Generate token` button, empty state, or `<table>` Label | ID | Created | Last used | Actions (`Revoke (.btn.danger)`).
- `.note` at the bottom with the public `/api/health` URL.

### Stats tab (`#tab-stats`)
- Top `.card-head` "Stack health" + `.pill.ok` "all healthy" or `.pill.warn` "degraded" + meta summary.
- Per-target `.card`: name + health pill + target URL `<code>` in meta + 2-column facts `<table>` (Total requests, In flight, Uptime, Latency p95) + optional inline pill table "By status" (2xx ok / 3xx muted / 4xx warn / 5xx bad) + optional inline code table "By method" + optional 2-column "Top hosts" table (up to 10).
- Empty/error state when monitor is unreachable.

### Modals (all native `<dialog>`)
- `dlg-new-service` / `form-new-service` — fields: `name` (pattern `[a-z0-9-]+`), `image`, `host`, `port` (number), `replicas` (number, default 1), `unscalable` (checkbox, "Singleton"), `env` (KEY=VALUE per line). Cancel + `Create (.primary)`.
- `dlg-replace-service` / `form-replace-service` — dual-mode (mode stored in `form.dataset.mode`); title and submit text mutate. Fields: hidden `serviceName`, disabled `currentImage`, new `image` input, `env` override textarea. Cancel + `Replace (.primary)`.
- `dlg-new-dns` / `form-new-dns` — `type` select (CNAME/A/TXT), `name`, `content`, `proxied` checkbox (default checked). Cancel + `Create (.primary)`.
- `dlg-2fa` / `form-2fa` — one `.otp-input` (name=`code`). Cancel + `Confirm (.primary)`. Elevates session for 5 minutes.
- `dlg-add-user` with `#add-user-body` swapping between phase 1 (`form-add-user` username + password → "Generate 2FA secret") and phase 2 (pending user block + `form-confirm-adduser` → "Verify & save user").

### Shared components
- `.card` (+ `.card.full-card` for auth) + `.card-head`.
- `.pill` + `.pill.ok/.bad/.warn/.muted` + new `.pill.info` (blue) for canary live.
- `.tag` (blue square label, e.g. "strip").
- `.btn` + `.btn.primary` + `.btn.danger` + `:disabled` (via `lockedAttr()`).
- `.btn-row` (default left-aligned; right-align via inline style today — promote to utility class).
- `.replica-ctrl`.
- `<table>` in three patterns: facts (2-col label/value), list (header + rows), inline-pill (single row of pills).
- `.field` (text/number/select/checkbox/`.otp-input`).
- `<dialog>` with `.dialog-actions` footer.
- `.toast` + `.toast.ok/.err` (top-right, auto-dismiss 3.5s).
- `.note` (yellow callout).
- `.empty` (italic muted line — promote to a real empty-state component).
- `.err` (mono red inline).
- `.meta` (12px muted helper).
- `.sysstat` blocks (CPU/MEM/DISK).
- `.totp-secret` (large mono centered string).
- `.otp-input` (large mono centered input, 8px letter-spacing).
- `<details><summary>` with custom marker rotation on `[open]`.

## What's weak today — pick your battles

Severity tags: **[H]** high (must fix), **[M]** medium (should fix), **[L]** low (nice).

1. **[H] Stats tab has zero charts.** "Total requests / In flight / Uptime / Latency p95" are flat `<td>`s. By-status is a one-row pill table. Top hosts is a plain 2-column table. Add inline-SVG sparklines per target, a horizontal bar for top hosts, a stacked or segmented bar for status distribution, and big-number tiles for the four headline KPIs.
2. **[H] No hero numbers anywhere.** Type scale tops out at 18px (h1). A monitoring dashboard needs display-size KPIs (32–48px tabular-nums) for uptime, total requests, healthy-target count.
3. **[H] Health pills don't pop.** 10.5px tinted chips next to long URLs and long red error text. Add a leading state glyph (filled dot / X / triangle), bump weight, consider solid colored chips for `ok/bad`.
4. **[H] Routes backend table overflows horizontally.** No max-width on the URL/error columns; wide errors push the layout. Move to a two-line responsive row or make the error cell an expandable `<details>` row.
5. **[H] Services card hierarchy is muddy.** Image (the most-changed field) is a row in a facts table next to Port (static). Deploy actions Stage/Replace/Rollback look identical; only Delete is `danger`. Promote image to the headline; build a clear action zone (primary Stage, secondary Replace, tertiary text-link Rollback); move Delete to an overflow `(⋯)` menu or visually separate it.
6. **[H] Danger buttons aren't dangerous-looking at rest.** `.btn` and `.btn.danger` differ only in text color until hover. Add a red-tinted bg or red border at rest.
7. **[H] Locked-state UX is invisible.** Without 2FA elevation, `lockedAttr()` sets `disabled` + `title="Confirm 2FA to enable"` and the page goes half-grey with no recovery CTA. Add a persistent top banner: lock icon + "Edits locked — confirm 2FA" + primary Unlock button; add a small lock glyph on each locked button.
8. **[H] No icons except CPU/MEM/DISK.** Adopt a small inline-SVG icon set (Lucide/Heroicons-style) with `ICON_*` Go-string-friendly consts: nav (routes/services/dns/users/stats), actions (plus/trash/edit/refresh/lock/copy/check), status (check/x/alert). Highest-leverage visual upgrade.
9. **[H] Nav tabs are silent.** No icons, no badges, no count indicators. Add a tiny dot/count badge on tabs with actionable state (Services has 1 update, Stats degraded, Users has pending).
10. **[H] Header sysstats are decorative inline text.** 4px tall bar, mixed units (CPU %, MEM/DISK free bytes), no chrome. Promote to three small stat tiles with consistent unit framing (used / total + %), larger bars or radial gauges, hover tooltip with precise numbers.
11. **[H] `window.alert()` for token reveal.** A sensitive one-shot moment delivered via a native alert looks like an error message and forces manual select+copy. Build a styled `<dialog>` with a mono token block + Copy button + "I've saved it" primary dismiss.
12. **[H] `window.confirm()` for all destructive actions.** Native OS dialogs outside the design system. Build a generic `confirmDialog({title, body, danger:true})` helper returning a Promise; replace every `confirm()` and `prompt()` call. (Note: `editDNS` currently uses `window.prompt` — replace with an inline edit row or a real dialog matching `dlg-new-dns`.)
13. **[M] Body is hard-capped 1200px centered.** On large monitors the dashboard is a column with empty navy gutters. Move to a fluid layout (consider a left sidebar nav replacing the top tabs) and cap content per-region.
14. **[M] All cards look identical.** Same padding, border, radius, hover. Define `.card.kpi`, `.card.list`, `.card.detail`, `.card.action` variants with distinct internal hierarchy.
15. **[M] Empty states are dead ends.** Single italic muted line. Build a real `.empty-state`: large icon + headline + one-line explanation + contextual primary CTA inside the empty area.
16. **[M] DNS records are type-blind.** Type pill is always `.muted`. Color CNAME/A/TXT distinctly (e.g. A=blue/`info`, CNAME=purple — but blue is the only on-system option without expanding the palette; alternative is to use the type letter as a colored glyph instead).
17. **[M] Tokens and users share one tab with no separation.** Add a clear section divider (icon + heading + description) or split into a Users sub-nav.
18. **[M] Replica scaler has duplicate affordances.** `[−] [input] [+] [Apply]` — +/- commit immediately and Apply commits the typed value. Collapse to a single segmented stepper or a slider; for singletons replace the whole control with a locked icon + tooltip.
19. **[M] Forms in dialogs have no grouping.** `dlg-new-service` stacks 7 fields with identical density. Group into Identity / Networking / Runtime, lift singleton helper text under the checkbox, add a one-line preview at the bottom.
20. **[M] Toasts have no exit animation, no stacking, no progress bar, no icon.** Add slide-out, vertical-stack manager, thin time-to-dismiss bar, check/cross icon.
21. **[M] Stats subheaders use inline `style="font-size:13px;margin-top:1em"` overriding h2.** Move to a `.subhead` utility class.
22. **[M] Canary pill is inline-styled `#1f6feb`.** Define `.pill.info` and use it. Forbid inline color overrides going forward.
23. **[M] First-time setup has no progress indicator.** Two-phase flow with no "Step 1 of 2". Add a stepper and a real welcome moment.
24. **[M] Login's 2FA field is the same huge `.otp-input` as the elevation modal.** Dwarfs the username/password above it. Make it collapsible ("+ Add 2FA code for edit access") or use a smaller segmented OTP for the login form.
25. **[M] Dialogs are anonymous.** No close (×), no icon header, no contextual summary. Add a header band with icon + title + close button; subtle accent stripe by purpose (create=blue, danger=red).
26. **[M] 2FA modal could be a 6-cell segmented OTP** with auto-advance and auto-submit, plus a "Unlocks edits for 5 minutes" badge near the submit.
27. **[L] Tabular-nums missing on numeric `<td>`s.** Apply `font-variant-numeric: tabular-nums` to `td` (or a `.num` utility) so columns line up.
28. **[L] `<code>` overused.** Hostnames, usernames, container names, token IDs, paths all share one orange-on-dark mono style. Use a real identifier style (medium font, no border) for prominent identifiers; reserve the chip with border for copyable tokens.
29. **[L] Routes "strip" tag is jargon with no affordance.** Make it a tooltip-bearing icon (scissors) next to the path.
30. **[L] Health-endpoint note is buried.** Promote to a small info card next to "+ Generate token" with a Copy button on the URL.
31. **[L] No footer / version / build SHA / dashboard uptime.** Add a thin footer strip.
32. **[L] No skeleton/loading states.** On first load and every tab switch users see a blank section then a populated section. Add skeleton rows on first render and consider a "staging buffer" swap for the 5s refresh to reduce flicker — at minimum, set `min-height` on tab containers to prevent layout jumps.

## DOM contract — do not change

If you rename any of the following, the app breaks. The wiring is hardcoded in the same file's JS.

**IDs that JS reads or writes:**
`auth-screen`, `auth-content`, `main-screen`, `sys-stats`, `status`, `who`, `tab-routes`, `tab-services`, `tab-dns`, `tab-users`, `tab-stats`, `dlg-new-service`, `dlg-new-dns`, `dlg-replace-service`, `dlg-2fa`, `dlg-add-user`, `add-user-body`, `form-new-service`, `form-new-dns`, `form-replace-service`, `form-2fa`, `form-setup`, `form-login`, `form-add-user`, `setup-result`.

**Dynamically generated ID patterns:**
- `form-confirm-setup` and `form-confirm-adduser` (built as `form-confirm-${kind}`).
- `rep-<serviceName>` — per-service replica number input, read by inline `onclick` via `document.getElementById`.

**Form input `name=` attributes (read as `f.<name>.value` / `.checked`):**
- `username`, `password`, `code` (in setup / login / 2FA / confirm forms).
- `name`, `image`, `host`, `port`, `replicas`, `unscalable`, `env` (new-service).
- `type`, `name`, `content`, `proxied` (new-dns).
- `serviceName`, `currentImage`, `image`, `env` (replace-service).

**Inline `onclick=` handler names (must remain global functions on `window`):**
`logout()`, `scaleSvc(name, n)`, `promoteCanary(name)`, `discardCanary(name)`, `openStage(name, currentImage)`, `openReplace(name, currentImage)`, `rollback(name, prevImage)`, `deleteSvc(name)`, `editDNS(id, currentContent)`, `deleteDNS(id, name)`, `openAddUser()`, `deleteUser(name)`, `createToken()`, `deleteToken(id)`. Also literal calls in onclick strings: `document.getElementById('dlg-...').showModal()` and `.close()` — so the dialog IDs above must not change.

**Selectors and attributes JS relies on:**
- `nav button` — `querySelectorAll('nav button')` attaches click handlers to every `<button>` inside `<nav>`. Must be direct children buttons (or at least selectable by that exact selector).
- `data-tab="routes|services|dns|users|stats"` on each nav button (`b.dataset.tab` is passed to `switchTab`).
- `.active` class is added/removed on nav buttons by `switchTab`.

**CSS classes referenced from JS template strings (renaming changes nothing in CSS only — the JS will still write these strings, so you must keep CSS for them):**
`.toast`, `.toast.ok`, `.toast.err`, `.sysstat`, `.sysstat.warn`, `.sysstat.bad`, `.sysstat .ico`, `.sysstat .label`, `.sysstat .val`, `.sysstat .bar`, `.sysstat .bar > span`, `.pill`, `.pill.ok`, `.pill.bad`, `.pill.warn`, `.pill.muted`, `.tag`, `.btn`, `.btn.primary`, `.btn.danger`, `.btn-row`, `.replica-ctrl`, `.card`, `.card-head`, `.field`, `.dialog-actions`, `.full-card`, `.meta`, `.empty`, `.err`, `.note`, `.totp-secret`, `.otp-input`.

**Structural requirements:**
- `<dialog>` elements must remain real `<dialog>` (JS calls `.showModal()` / `.close()`).
- Tab containers (`#tab-*`) must toggle visibility via the `hidden` HTML attribute (`switchTab` sets `el.hidden = true/false`).
- `<nav>` must contain direct `<button>` children with `data-tab` matching `routes|services|dns|users|stats`.
- `#auth-screen` and `#main-screen` must both start with `hidden` set.
- `data-tab="routes"` button must have `class="active"` on initial paint.
- The Go const must remain named `dashboardHTML` and stay a single backtick-delimited raw string in `dashboard/ui.go`.

## Deliverable format

You output the **complete replacement contents** of `/Users/matthewcheng/Projects/proxy-manager/dashboard/ui.go` as one `Write` (not a diff). Approximate target: 900 lines (the current file is 910). The file must be valid Go:

```go
package main

const dashboardHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>Pi Dashboard</title>
  <style>
    /* all CSS here, one block */
  </style>
</head>
<body>
  <!-- markup -->
  <script>
    // all JS here, one classic script, no modules, no IIFE
    // global functions only (onclick handlers depend on window.<name>)
  </script>
</body>
</html>`
```

**Backtick handling.** Anywhere your JS needs a template literal, close the raw string, concatenate `"\`"`, reopen the raw string:

```go
const dashboardHTML = `...
  function setupView(){
    document.getElementById('auth-content').innerHTML = ` + "`" + `
      <div class="card full-card">
        <h2>First-time setup</h2>
        ...
      </div>
    ` + "`" + `;
  }
...`
```

Search the current file for `` + "\`" + `` to see every existing use (in `setupView`, `loginView`, `pendingUserBlock`, `openAddUser`). Match that style.

**Backslashes.** Inside the raw string, backslashes are literal — there is NO Go-level escaping. `\n` inside a JS string literal is interpreted by the JS engine at runtime, not by Go. Don't try to escape characters as if it were a regular Go string.

**Onclick string-argument escaping.** Use the existing pattern: `'...onclick="scaleSvc(\\'' + esc(s.name) + '\\', 1)"'`. Don't break service names with quotes by skipping `esc()`.

**No other files.** Do not introduce additional Go files, do not change `package main`, do not alter anything else under `dashboard/`.

## Suggested design direction

Pick exactly ONE. All three respect the constraints (single file, no fonts, system stack, dark theme).

### A) Linear-inspired neutral console (recommended for legibility)
Very neutral surfaces, restrained accent, generous whitespace, large hero numbers in tabular-nums. Hierarchy through type weight + size, not color.
- `--bg #0b0c0f`, `--surface #14161b`, `--surface-2 #1c1f26`, `--border #232730`, `--border-2 #2e333d`
- `--text #e8eaed`, `--muted #8a92a0`
- `--accent #7c8cff` (single indigo brand), `--accent-2 #5b6cf0` (primary actions)
- `--green #4ade80`, `--red #f87171`, `--yellow #facc15`, `--info #60a5fa`
- Radius scale: 6 / 10 / 14 / 999. Shadows: subtle inset highlight + soft drop. Letter-spacing tighter on display sizes (-0.02em on 32px+).
- Best fit for: readable Stats KPIs, calm operator console feel, minimal visual fatigue during long sessions.

### B) Vercel/Geist-inspired gradient-edge
Pitch-black canvas, glass-like cards with subtle gradient borders on hover, electric accent. More striking; risk is over-decoration.
- `--bg #000000`, `--surface #0a0a0a`, `--surface-2 #111111`, `--border #1f1f1f`, `--border-2 #2a2a2a`
- `--text #ededed`, `--muted #888888`
- `--accent #00d4ff` (cyan brand), `--accent-2 #0070f3` (primary actions)
- `--green #50e3c2`, `--red #ff4d4f`, `--yellow #f5a623`, `--info #0070f3`
- Cards: 1px gradient border via `background: linear-gradient(...) border-box; border: 1px solid transparent;`. Hero numbers in a subtle white-to-accent gradient.
- Best fit for: a portfolio-looking dashboard; eye-catching for screenshots.

### C) Tailscale-inspired warm minimal
Slightly warm-grey canvas, soft cards, no gradients, accent only on the primary CTA. Pragmatic, friendly, "open source tool that respects you."
- `--bg #16171a`, `--surface #1d1f23`, `--surface-2 #25282d`, `--border #2c3037`, `--border-2 #383d46`
- `--text #ececec`, `--muted #9aa0a8`
- `--accent #f59e0b` (warm amber brand), `--accent-2 #3b82f6` (primary actions stay blue for cross-cultural "go" semantics)
- `--green #34d399`, `--red #ef4444`, `--yellow #fbbf24`, `--info #60a5fa`
- Best fit for: long-term homelab tool that doesn't try to be cool. Pairs well with a left-sidebar nav.

If undecided, **pick A**. It's the safest aesthetic upgrade and the closest to "operator console" without resembling a CSS framework demo.

## Out of scope

- **No functional changes.** Don't rename, add, or remove any API endpoint, route, or fetch call. Don't change what `scaleSvc`, `promoteCanary`, etc. do — only re-style their triggers.
- **No JS behavior changes** beyond: replacing `alert()` and `confirm()`/`prompt()` with styled-dialog equivalents (keep the same external behavior — same prompts, same confirmations, same destructive guards); adding new helpers that don't change existing function signatures; building skeleton/loading states; segmenting the OTP input. If unsure whether a JS change is in scope, default to "keep it identical."
- **No new external dependencies** (network or otherwise).
- **No new Go files, no Go-side changes, no other files under `dashboard/` touched.**
- **No tab additions / removals** — still exactly 5 tabs with the same `data-tab` values in the same order (routes, services, dns, users, stats).
- **No removal of any current capability.** Locked-button state must still exist (now better-explained), 2FA elevation must still gate the same actions, the 5s/30s polling cadence stays, `pendingActive` suppression stays.
- **Auth flows stay two-phase** (form → pending QR → confirm code). Don't collapse them.

## How to verify your work

Run through this checklist mentally before finishing. The recipient cannot run the code, so you must self-verify against the contract.

- [ ] File starts with `package main\n\nconst dashboardHTML = \`` and ends with `` `\n ``. One Go const, one backtick-delimited raw string.
- [ ] No literal backticks inside the raw string. Every JS template literal uses the `` ` + "\`" + ` `` escape on both ends.
- [ ] No `<link rel="stylesheet">`, no `<script src=...>`, no `@import url(http...)`, no `url(http...)` in CSS, no `fetch('http...')` to a non-relative origin, no Google Fonts.
- [ ] One `<style>` block in `<head>`. One classic `<script>` block before `</script></body>`. No `type="module"`.
- [ ] All 25 must-preserve IDs are present in the markup (search the file for each `id="..."` from the DOM contract).
- [ ] All five `<dialog>` elements are real `<dialog>` tags (not `<div>`).
- [ ] `#auth-screen` and `#main-screen` both have the `hidden` attribute on initial paint.
- [ ] `<nav>` contains exactly five `<button>` direct children with `data-tab` of `routes|services|dns|users|stats` in that order, and the `routes` button has `class="active"` initially.
- [ ] Every dynamic-HTML CSS class from the contract still exists in CSS (`.toast`, `.toast.ok`, `.toast.err`, `.sysstat[.warn/.bad]`, `.sysstat .ico/.label/.val/.bar > span`, `.pill[.ok/.bad/.warn/.muted]`, `.tag`, `.btn[.primary/.danger]`, `.btn-row`, `.replica-ctrl`, `.card[.full-card]`, `.card-head`, `.field`, `.dialog-actions`, `.meta`, `.empty`, `.err`, `.note`, `.totp-secret`, `.otp-input`).
- [ ] Every form input name from the contract is preserved (`username`, `password`, `code`, `name`, `image`, `host`, `port`, `replicas`, `unscalable`, `env`, `type`, `content`, `proxied`, `serviceName`, `currentImage`).
- [ ] Every inline onclick handler name is still defined as a global function in the `<script>` block (`logout`, `scaleSvc`, `promoteCanary`, `discardCanary`, `openStage`, `openReplace`, `rollback`, `deleteSvc`, `editDNS`, `deleteDNS`, `openAddUser`, `deleteUser`, `createToken`, `deleteToken`).
- [ ] `esc()` helper still exists and is used on every interpolated user value in dynamic HTML.
- [ ] Onclick string arguments still use the `\\'` escape pattern around `esc(...)` values.
- [ ] `pendingActive` flag and its suppression in the 5s/30s pollers still exist and still gate `renderActive`.
- [ ] `switchTab` still toggles `el.hidden` (not a CSS class).
- [ ] `rep-<serviceName>` input id is still generated per service so the `[−]/[+]/Apply` handlers find it via `document.getElementById`.
- [ ] No `{{ }}`, `<%= %>`, or bare `${...}` outside a JS template literal anywhere in the file (would render as literal text).
- [ ] Add-user phase 2 still uses `pendingUserBlock`-style markup with QR `<img>`, otpauth link, `<details>` revealing the secret in `.totp-secret`, and `form-confirm-adduser` with `.otp-input` named `code`.
- [ ] First-time setup still renders inside `#auth-content` and still populates `#setup-result` with the pending-user block on submit.
- [ ] Toasts still mount to `document.body` (or wherever `toast()` currently mounts) and still get the classes `.toast.ok` / `.toast.err`.
- [ ] No JS console errors on first paint (mentally trace: nav buttons selectable, `tab-routes` rendered, no missing IDs referenced by handlers).
- [ ] File compiles as Go: no stray backticks inside the raw string, all `+ "\`" +` concatenations balanced (count must be even).

---

## Available data APIs (call these from the redesigned UI)

All endpoints below are exposed by the dashboard binary. The Stats tab and any
new per-target views should fetch data from these — do not invent new ones,
the backend is already wired. Auth is automatic via session cookie.

### Top-level

| Endpoint | Returns | Use in UI |
|---|---|---|
| `GET /api/health` | `{status, targets:[{name,health}], checked_at}` — no auth, sanitized | Status badge in header; external monitors |
| `GET /api/stats` | host CPU/MEM/DISK numbers | sysstat widget (already in header) |
| `GET /api/monitor/overview` | aggregate health + all targets w/ basic metrics | "Stack health" hero card on Stats tab |
| `GET /api/monitor/snapshot` | raw `{name → TargetState}` map incl. full last sample | debug / power-user view |

### Per-target detail (NEW — wire these into per-target pages)

| Endpoint | Returns |
|---|---|
| `GET /api/monitor/target/{name}` | full target detail: health, ever_reached, last_ok, fail_count, full metrics blob, **rate_per_sec_1m**, **rate_per_sec_5m**, **error_pct_recent** |
| `GET /api/monitor/target/{name}/hosts` | per-host breakdown sorted by traffic: `[{host, total, by_status, error_pct, errors}]` — feed top-N tables |
| `GET /api/monitor/target/{name}/errors` | error breakdown: `{total_errors, by_status, by_host, by_host_status}` |
| `GET /api/monitor/target/{name}/series?field=delta` | rolling time series (1h, 5s buckets) for sparklines |

Series fields accepted: `delta` (req/interval), `total` (cumulative), `in_flight`.

### Target health states

The `health` field on every target now takes one of FOUR values:

| State | Pill | Meaning | Show in overall health? |
|---|---|---|---|
| `up` | green | last scrape OK | yes |
| `flaky` | yellow | recent intermittent failures | yes |
| `down` | red | 3+ consecutive failures (was reachable before) | yes |
| `absent` | muted gray | never reached, probably not deployed (e.g. `edge` with profile off) | **no** — render as "not deployed" |

### Services / DNS / Users (unchanged, still wireable)

| Endpoint | Notes |
|---|---|
| `GET /api/services` | list managed services (with replicas, update_available, canary_image, previous_image, unscalable, etc.) |
| `GET /api/routes` | list routes from labels + static config |
| `GET /api/cf/records` | Cloudflare DNS records |
| `GET /api/users` | list users (no secrets) |
| `GET /api/users/tokens` | current user's API tokens (no raw value) |

Mutations (POST/PATCH/DELETE) all exist for these — see existing code; the
redesign just needs to keep the existing button click handlers intact, the
backend doesn't change.

### Suggested per-proxy detail view

A new screen the redesigner is free to add — drill-down from the Stats tab,
populated by the per-target endpoints above:

```
┌─ proxy ───────────────────────────────── [up] ─┐
│ rate: 12.3 req/s (1m) · 11.7 req/s (5m)        │
│ p95 latency: 47ms · 0 in-flight · 0.2% errors  │
│                                                 │
│  ▁▂▂▃▅▃▂▁▂▄▅▆█▆▄▂▁▂▃▂▁▂  (sparkline, /series) │
│                                                 │
│  Top hosts             req     errors  err%    │
│  badminton.polardev    5120    12      0.2%    │
│  market.polardev       3840    0       0.0%    │
│  …                                              │
└─────────────────────────────────────────────────┘
```

Drill-in: `/api/monitor/target/proxy` for the hero numbers, `/hosts` for the
table, `/series?field=delta` for the sparkline.

