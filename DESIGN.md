# Route Design System

This file is the single source of truth for how Route looks. It replaces the
current slate-and-cyan look of `web/dashboard/` with a Linear-style look:
near-black surfaces, hairline borders, small dense type, one indigo accent,
and color used only to carry meaning.

Read the whole file before you edit anything. Then do the work in the order
of section 13. Every value you need is in this file. Do not invent new
colors, sizes, radii, or shadows. If a value is missing, use the closest
token and add a note to section 15.

## 1. Scope

Files you will change:

| File | What changes |
| --- | --- |
| `web/dashboard/index.html` | New layout (topbar, map pane, right inspector). All existing `id` values stay. |
| `web/dashboard/style.css` | Full rewrite against the tokens in section 3. |
| `web/dashboard/app.js` | Map paint colors, marker classes, one connection-status hook. No logic changes. |
| `web/dashboard/sim.js` | Two lines that set `data-state` on the status pill. No logic changes. |
| `deploy/grafana/dashboards/route.json` | Series colors that match the dashboard palette. |

Files you must not change: anything under `internal/`, `cmd/`, `deploy/compose/`,
and any Go file. The dashboard talks to the server through `/live` (WebSocket)
and `/sim/*` (HTTP). Those contracts do not change.

## 2. Design principles

These come from the Linear reference screenshots. Apply them everywhere.

1. **Dark and quiet.** Surfaces are near-black. Separation comes from 1px
   borders and small steps in background lightness, not from shadows or glow.
2. **Color means something.** Gray is the default for all chrome. Color is for
   status (courier and order states), the single accent (primary action,
   focus, selection), and errors. Never use color for decoration.
3. **Small, dense, aligned.** Base text is 13px. Rows are 28 to 32px tall.
   Everything sits on a 4px grid.
4. **Labels are sentence case.** No uppercase labels, no letter-spaced caps.
   Linear writes "Scope", "Started", "Completed", not "SCOPE".
5. **Numbers are tabular.** Every live number uses `font-variant-numeric:
   tabular-nums` so values do not shake when they update four times a second.
6. **One look everywhere.** Every panel, input, button, chip, and legend row
   uses the same tokens. If two things look similar, they must use the same
   class.

## 3. Tokens

Put this block at the top of `style.css`, replacing the old `:root`. Every
other rule in the file must use these variables. A hex code outside this
block is a bug (the only exception is the select chevron data URI in 7.6).

```css
:root {
    /* Surfaces, darkest to lightest */
    --bg-app: #08090a;          /* page behind everything */
    --bg-surface: #0f1011;      /* inspector, topbar */
    --bg-elevated: #151618;     /* cards inside a surface, popovers */
    --bg-hover: rgba(255, 255, 255, 0.04);
    --bg-active: rgba(255, 255, 255, 0.07);
    --bg-input: #08090a;
    --bg-overlay: rgba(8, 9, 10, 0.7); /* --bg-app at 70%, for map chrome */

    /* Borders */
    --border-subtle: rgba(255, 255, 255, 0.06);  /* between sections */
    --border-default: rgba(255, 255, 255, 0.09); /* panel outlines */
    --border-control: #62666d;  /* input and button edges, 3:1 against --bg-input */

    /* Text */
    --text-primary: #f7f8f8;
    --text-secondary: #8a8f98;  /* labels, help text */
    --text-tertiary: #62666d;   /* separators, disabled, never body copy */
    --text-on-accent: #ffffff;

    /* Accent (Linear indigo) */
    --accent: #5e6ad2;
    --accent-hover: #5560c8;    /* darker, keeps white text above 4.5:1 */
    --accent-soft: rgba(94, 106, 210, 0.16);
    --focus-ring: #7c86e8;

    /* Status. These are the only colors on the map. */
    --status-idle: #4ea7fc;     /* courier idle */
    --status-offered: #f2c94c;  /* courier offered, pending offer line */
    --status-assigned: #4cb782; /* courier and order assigned, assignment line */
    --status-queued: #f2994a;   /* order queued */
    --status-error: #eb5757;    /* errors, offline */
    --status-neutral: #8a8f98;  /* idle simulator, unknown state */

    /* Surge ramp (sequential, one hue family so it never reads as a status) */
    --surge-low: #3b3f8f;
    --surge-mid: #6e56cf;
    --surge-high: #b38cf2;

    /* Type */
    --font-sans: 'Inter', -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif;
    --text-xs: 11px;
    --text-sm: 12px;
    --text-base: 13px;
    --text-md: 14px;
    --text-lg: 16px;
    --text-xl: 20px;
    --weight-regular: 400;
    --weight-medium: 500;
    --weight-semibold: 600;

    /* Spacing (4px grid) */
    --space-1: 4px;
    --space-2: 8px;
    --space-3: 12px;
    --space-4: 16px;
    --space-5: 20px;
    --space-6: 24px;
    --space-8: 32px;

    /* Radii */
    --radius-sm: 4px;   /* chips, swatches, order markers */
    --radius-md: 6px;   /* buttons, inputs, selects */
    --radius-lg: 8px;   /* cards, popovers */
    --radius-xl: 12px;  /* the workspace frame */
    --radius-full: 9999px;

    /* Sizes */
    --topbar-h: 44px;
    --inspector-w: 320px;
    --control-h: 30px;

    /* Elevation. Only popovers and the map attribution use a shadow. */
    --shadow-popover: 0 4px 24px rgba(0, 0, 0, 0.45);

    /* Motion */
    --ease: cubic-bezier(0.25, 0.1, 0.25, 1);
    --duration-fast: 120ms;
}
```

### 3.1 Why these colors

- The status colors are Linear's own issue-state hues: blue for "started",
  yellow for "in progress", green for "done", orange for "needs attention",
  red for "error". A courier with a pending offer is "in progress", so it is
  yellow. An assignment is "done", so it is green.
- The old surge ramp (yellow, orange, red) clashed with the offered and
  queued states. Surge now uses an indigo-to-violet ramp. It sits under the
  markers and never competes with them.
- `--border-control` and `--accent-hover` exist for contrast. Do not swap them
  for the lighter "Linear-looking" values. Inputs need a 3:1 edge, and white
  text on the primary button needs 4.5:1.

### 3.2 Contrast table (already checked, do not change the pairs)

| Foreground | Background | Use | Ratio |
| --- | --- | --- | --- |
| `--text-primary` | `--bg-surface` | body text | about 18:1 |
| `--text-secondary` | `--bg-surface` | labels, help | about 5.8:1 |
| `--text-tertiary` | `--bg-surface` | separators only | about 3.3:1 |
| `--text-on-accent` | `--accent` | primary button | about 4.7:1 |
| `--border-control` | `--bg-input` | input edge | about 3.4:1 |

Never put `--text-tertiary` on any text a person must read.

## 4. Typography

Load Inter from Google Fonts with weights 400, 500 and 600 only. Replace the
current font `<link>` with:

```html
<link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600&display=swap" rel="stylesheet">
```

Set on `body`:

```css
body {
    font-family: var(--font-sans);
    font-size: var(--text-base);
    line-height: 1.5;
    font-feature-settings: 'cv11', 'ss01'; /* Inter's straight-sided alternates, as Linear uses */
    -webkit-font-smoothing: antialiased;
    -moz-osx-font-smoothing: grayscale;
}
```

Type roles. Use only these.

| Role | Size | Weight | Color | Where |
| --- | --- | --- | --- | --- |
| Page title | `--text-md` | 500 | primary | breadcrumb current item |
| Section title | `--text-sm` | 500 | secondary | inspector section headers ("Live", "Simulator", "Legend") |
| Body | `--text-base` | 400 | primary | legend rows, select text |
| Label | `--text-sm` | 400 | secondary | stat labels, form labels |
| Help | `--text-sm` | 400 | secondary | help text under inputs |
| Stat value | `--text-lg` | 500 | primary | the eight live metrics, tabular nums |
| Chip | `--text-sm` | 500 | primary | status pill, connection pill |
| Button | `--text-base` | 500 | primary or on-accent | all buttons |

Rules:

- No `text-transform: uppercase` anywhere. Remove all of it.
- No `letter-spacing` except `-0.01em` on the stat values.
- Headings in the HTML stay semantic (`h1`, `h2`), but their look comes from
  the role table, not from browser defaults.

## 5. Layout

The new layout copies the Linear project page (reference 1): a thin top bar,
then one rounded workspace frame that holds the main content on the left and
an inspector panel on the right.

```
+------------------------------------------------------------------+
| [mark] Route  >  Live map                     (o) Connected      |  topbar, 44px, --bg-app
+------------------------------------------------------------------+
|  +-----------------------------------------------+-------------+ |
|  |                                               | Live        | |
|  |                                               |  stat grid  | |
|  |                 map (MapLibre)                +-------------+ |
|  |                                               | Simulator v | |
|  |                                               |  form       | |
|  |                                               +-------------+ |
|  |                                               | Legend      | |
|  |                                               |  rows       | |
|  +-----------------------------------------------+-------------+ |
+------------------------------------------------------------------+
   workspace frame: 8px gap to window edge, 12px radius, 1px border
   inspector: 320px wide, --bg-surface, border-left, scrolls on its own
```

### 5.1 Target HTML

Replace the body of `index.html` with this structure. Keep every `id` exactly
as written: `app.js` and `sim.js` look them up by id. Keep the MapLibre
`<script>` and `<link>` tags in `<head>` as they are (same version, same
integrity hashes). Change `<meta name="theme-color">` to `#08090a`.

```html
<body>
<div class="app">
    <header class="topbar">
        <div class="topbar-left">
            <span class="brand-mark" aria-hidden="true"></span>
            <nav class="breadcrumb" aria-label="Breadcrumb">
                <span class="crumb">Route</span>
                <svg class="crumb-sep" width="12" height="12" viewBox="0 0 12 12" aria-hidden="true"><path d="M4.5 2.5 8 6l-3.5 3.5" fill="none" stroke="currentColor" stroke-width="1.25" stroke-linecap="round" stroke-linejoin="round"/></svg>
                <h1 class="crumb crumb-current">Live map</h1>
            </nav>
        </div>
        <div class="topbar-right">
            <span class="pill" id="conn" data-state="connecting" role="status" aria-live="polite">
                <span class="pill-dot" aria-hidden="true"></span>
                <span id="conn-label">Connecting</span>
            </span>
        </div>
    </header>

    <div class="workspace">
        <main class="map-pane">
            <div id="map" role="region" aria-label="Live map of couriers and orders"></div>
        </main>

        <aside class="inspector" aria-label="Live metrics and controls">
            <section class="section" aria-labelledby="live-title">
                <h2 class="section-title" id="live-title">Live</h2>
                <dl class="stat-grid" id="metrics-panel">
                    <div class="stat" title="Orders accepted per second, last 10 seconds">
                        <dt>Orders/s</dt>
                        <dd id="val-orders-s">-</dd>
                    </div>
                    <div class="stat" title="Order created to first offer sent, last 10 seconds">
                        <dt>Match p50 / p99</dt>
                        <dd id="val-latency">-</dd>
                    </div>
                    <div class="stat" title="Orders assigned out of orders assigned or given up on, last 10 seconds">
                        <dt>Match rate</dt>
                        <dd id="val-match-rate">-</dd>
                    </div>
                    <div class="stat" title="How much shorter batched pickups are than greedy matching, last batch">
                        <dt>Batch gain</dt>
                        <dd id="val-batch-gain">-</dd>
                    </div>
                    <div class="stat" title="Location pings dropped under load, per second">
                        <dt>Pings shed/s</dt>
                        <dd id="val-shed">-</dd>
                    </div>
                    <div class="stat" title="Orders waiting for the matcher">
                        <dt>Queue depth</dt>
                        <dd id="val-queue">-</dd>
                    </div>
                    <div class="stat">
                        <dt>Active couriers</dt>
                        <dd id="val-couriers">0</dd>
                    </div>
                    <div class="stat">
                        <dt>Peak surge</dt>
                        <dd id="val-surge">1.0x</dd>
                    </div>
                </dl>
            </section>

            <!-- Shown only when routed has the /sim routes (see sim.js). -->
            <details class="section section-collapsible" id="sim-controls" hidden>
                <summary class="section-summary">
                    <svg class="chevron" width="12" height="12" viewBox="0 0 12 12" aria-hidden="true"><path d="M4.5 2.5 8 6l-3.5 3.5" fill="none" stroke="currentColor" stroke-width="1.25" stroke-linecap="round" stroke-linejoin="round"/></svg>
                    <span class="section-title">Simulator</span>
                    <span class="pill" id="sim-state" data-state="checking">checking</span>
                </summary>
                <div class="form">
                    <div class="field">
                        <label for="sim-token">API token</label>
                        <input class="input" id="sim-token" name="api-token" type="password" autocomplete="off" spellcheck="false" aria-describedby="sim-token-help sim-token-error">
                        <p class="help" id="sim-token-help">Needed to start, stop or change the rate. Kept in this tab only.</p>
                        <p class="field-error" id="sim-token-error" aria-live="polite"></p>
                    </div>

                    <div class="field">
                        <label for="sim-profile">Scenario</label>
                        <select class="input select" id="sim-profile" disabled></select>
                    </div>

                    <div class="button-row">
                        <button type="button" class="btn btn-primary" id="sim-start" disabled>Start</button>
                        <button type="button" class="btn btn-secondary" id="sim-stop" disabled>Stop</button>
                    </div>

                    <div class="field">
                        <label for="sim-rate" class="label-row">
                            <span>Order rate</span>
                            <span><output id="sim-rate-out" for="sim-rate">0</output> per second</span>
                        </label>
                        <input class="range" id="sim-rate" type="range" min="0" max="600" step="5" value="0" disabled>
                    </div>

                    <p class="message" id="sim-message" role="status" aria-live="polite"></p>
                </div>
            </details>

            <section class="section" aria-labelledby="legend-title">
                <h2 class="section-title" id="legend-title">Legend</h2>
                <ul class="legend">
                    <li><span class="key key-dot status-idle"></span>Courier, idle</li>
                    <li><span class="key key-dot status-offered"></span>Courier, offered</li>
                    <li><span class="key key-dot status-assigned"></span>Courier, assigned</li>
                    <li><span class="key key-square status-queued"></span>Order, queued</li>
                    <li><span class="key key-square status-assigned"></span>Order, assigned</li>
                    <li><span class="key key-line status-assigned"></span>Assignment</li>
                    <li><span class="key key-line key-line-dashed status-offered"></span>Pending offer</li>
                    <li><span class="key key-swatch surge-low"></span>Surge 1.2x to 1.5x</li>
                    <li><span class="key key-swatch surge-mid"></span>Surge 2x</li>
                    <li><span class="key key-swatch surge-high"></span>Surge 3x</li>
                </ul>
            </section>
        </aside>
    </div>
</div>

<script src="app.js"></script>
<script src="sim.js"></script>
</body>
```

Notes on the markup:

- The `dt`/`dd` pairs are wrapped in `div`s. That is valid HTML inside `dl`.
- `#metrics-panel` moved from a `div` to the `dl`. Nothing in JS reads it as a
  `div`, so this is safe.
- The old `.legend h4` is gone. The legend is a list now.
- The old `metric-wide` class is gone. The grid handles width.

### 5.2 Layout CSS

```css
html { color-scheme: dark; }

*, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }

body {
    /* plus the font rules from section 4 */
    background: var(--bg-app);
    color: var(--text-primary);
    overflow: hidden;
}

.app {
    display: grid;
    grid-template-rows: var(--topbar-h) 1fr;
    height: 100vh;
    height: 100dvh;
}

.workspace {
    display: grid;
    grid-template-columns: 1fr var(--inspector-w);
    min-height: 0; /* lets the inspector scroll instead of growing the page */
    margin: 0 var(--space-2) var(--space-2);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-xl);
    overflow: hidden; /* clips the map to the rounded corners */
    background: var(--bg-surface);
}

.map-pane {
    position: relative;
    min-width: 0;
    min-height: 0;
}

#map { position: absolute; inset: 0; }

.inspector {
    overflow-y: auto;
    border-left: 1px solid var(--border-default);
    background: var(--bg-surface);
}
```

## 6. Topbar

Matches the breadcrumb bar in reference 1 ("Projects > Website").

```css
.topbar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
    padding: 0 var(--space-4);
    background: var(--bg-app);
}

.topbar-left,
.topbar-right {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    min-width: 0;
}

/* The brand mark: a 16px rounded square in the accent color with a
   lighter diagonal. No image file, no logo font. */
.brand-mark {
    width: 16px;
    height: 16px;
    border-radius: var(--radius-sm);
    background: linear-gradient(135deg, var(--focus-ring), var(--accent));
    flex: none;
}

.breadcrumb {
    display: flex;
    align-items: center;
    gap: var(--space-1);
    min-width: 0;
}

.crumb {
    font-size: var(--text-md);
    font-weight: var(--weight-medium);
    color: var(--text-secondary);
    white-space: nowrap;
}

.crumb-current { color: var(--text-primary); }

.crumb-sep { color: var(--text-tertiary); flex: none; }
```

## 7. Components

Each component below has exactly one class. Do not make variants that are not
listed.

### 7.1 Inspector section

```css
.section {
    padding: var(--space-4);
    border-bottom: 1px solid var(--border-subtle);
}

.section:last-child { border-bottom: 0; }

.section-title {
    display: block;
    margin-bottom: var(--space-3);
    font-size: var(--text-sm);
    font-weight: var(--weight-medium);
    color: var(--text-secondary);
}
```

### 7.2 Collapsible section (the simulator)

The `details` element keeps its native open and close behavior. `sim.js`
already opens it on wide screens and closes it on narrow ones.

```css
.section-collapsible[hidden] { display: none; }

.section-summary {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    margin: calc(var(--space-1) * -1) calc(var(--space-2) * -1);
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius-md);
    cursor: pointer;
    list-style: none;
    touch-action: manipulation;
    transition: background-color var(--duration-fast) var(--ease);
}

.section-summary::-webkit-details-marker { display: none; }

.section-summary:hover { background: var(--bg-hover); }

.section-summary .section-title { margin: 0; }

.section-summary .pill { margin-left: auto; }

.chevron {
    color: var(--text-secondary);
    flex: none;
    transition: transform var(--duration-fast) var(--ease);
}

.section-collapsible[open] .chevron { transform: rotate(90deg); }

.section-collapsible[open] > .form { margin-top: var(--space-4); }
```

### 7.3 Stat grid

Matches the "Scope / Started / Completed" block in reference 1. Two columns,
label on top, value below.

```css
.stat-grid {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: var(--space-4) var(--space-3);
}

.stat { min-width: 0; }

.stat dt {
    font-size: var(--text-sm);
    color: var(--text-secondary);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
}

.stat dd {
    margin-top: 2px;
    font-size: var(--text-lg);
    font-weight: var(--weight-medium);
    letter-spacing: -0.01em;
    line-height: 1.3;
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
}
```

Do not add sparklines, icons, colored squares, or trend arrows to the stats.
The values are live already.

### 7.4 Pill (status chip)

Used for the connection state in the topbar and the simulator state. A
6px dot plus a short word, on a faint fill. Matches the "In Progress" and
"High" property chips in reference 1.

```css
.pill {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    max-width: 100%;
    height: 22px;
    padding: 0 var(--space-2);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-full);
    background: var(--bg-hover);
    font-size: var(--text-sm);
    font-weight: var(--weight-medium);
    color: var(--text-primary);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
}

/* The simulator pill has no inner dot element (sim.js sets its text),
   so its dot is a pseudo-element. */
.pill-dot,
#sim-state::before {
    content: '';
    width: 6px;
    height: 6px;
    border-radius: var(--radius-full);
    background: var(--status-neutral);
    flex: none;
}

.pill[data-state="connected"] .pill-dot,
#sim-state[data-state="running"]::before { background: var(--status-assigned); }

.pill[data-state="connecting"] .pill-dot,
#sim-state[data-state="checking"]::before { background: var(--status-offered); }

.pill[data-state="disconnected"] .pill-dot,
#sim-state[data-state="offline"]::before { background: var(--status-error); }

/* idle stays --status-neutral */
```

### 7.5 Buttons

Two kinds only: primary (one per view, the main action) and secondary.

```css
.btn {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    height: var(--control-h);
    padding: 0 var(--space-3);
    font: inherit;
    font-size: var(--text-base);
    font-weight: var(--weight-medium);
    border: 1px solid transparent;
    border-radius: var(--radius-md);
    cursor: pointer;
    touch-action: manipulation;
    transition: background-color var(--duration-fast) var(--ease),
                border-color var(--duration-fast) var(--ease);
}

.btn-primary {
    color: var(--text-on-accent);
    background: var(--accent);
}

.btn-primary:hover:not(:disabled) { background: var(--accent-hover); }

.btn-secondary {
    color: var(--text-primary);
    background: var(--bg-hover);
    border-color: var(--border-control);
}

.btn-secondary:hover:not(:disabled) { background: var(--bg-active); }

.btn:disabled {
    opacity: 0.45;
    cursor: not-allowed;
}

.button-row {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: var(--space-2);
}
```

No `transform` on press. Linear buttons do not move.

### 7.6 Form fields

```css
.form {
    display: grid;
    gap: var(--space-4);
}

.field {
    display: grid;
    gap: 6px;
}

.field label,
.label-row {
    font-size: var(--text-sm);
    color: var(--text-secondary);
}

.label-row {
    display: flex;
    justify-content: space-between;
    gap: var(--space-2);
}

.label-row output {
    color: var(--text-primary);
    font-weight: var(--weight-medium);
    font-variant-numeric: tabular-nums;
}

.input {
    width: 100%;
    height: var(--control-h);
    padding: 0 var(--space-2);
    font: inherit;
    font-size: var(--text-base);
    color: var(--text-primary);
    background: var(--bg-input);
    border: 1px solid var(--border-control);
    border-radius: var(--radius-md);
    touch-action: manipulation;
    transition: border-color var(--duration-fast) var(--ease);
}

.input:hover:not(:disabled) { border-color: var(--text-secondary); }

.input[aria-invalid="true"] { border-color: var(--status-error); }

.select {
    appearance: none;
    padding-right: 28px;
    /* The one allowed raw color: SVG data URIs cannot read CSS variables.
       #8a8f98 is --text-secondary. */
    background-image: url("data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' width='12' height='12' viewBox='0 0 12 12'%3E%3Cpath d='M3 4.5 6 7.5l3-3' fill='none' stroke='%238a8f98' stroke-width='1.25' stroke-linecap='round' stroke-linejoin='round'/%3E%3C/svg%3E");
    background-repeat: no-repeat;
    background-position: right 8px center;
}

.range {
    width: 100%;
    accent-color: var(--accent);
    touch-action: manipulation;
}

.input:disabled,
.range:disabled {
    opacity: 0.45;
    cursor: not-allowed;
}

.help {
    font-size: var(--text-sm);
    color: var(--text-secondary);
}

.field-error {
    font-size: var(--text-sm);
    color: var(--status-error);
}

.field-error:empty { display: none; }

.message {
    min-height: 18px;
    font-size: var(--text-sm);
    color: var(--text-secondary);
}

.message:empty { min-height: 0; }
```

`.message` shows both success text ("Started demo.") and errors, so it uses
the neutral secondary color. Errors that need red already go to
`.field-error`.

### 7.7 Focus

One focus style for every interactive element. Put it once, near the end of
the file.

```css
:where(a, button, input, select, summary):focus-visible {
    outline: 2px solid var(--focus-ring);
    outline-offset: 2px;
}

:where(a, button, input, select, summary):focus:not(:focus-visible) {
    outline: none;
}
```

### 7.8 Legend

Matches the list rows in reference 3: one line per item, a small leading
glyph, then the label.

```css
.legend {
    list-style: none;
    display: grid;
    gap: 2px;
}

.legend li {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    height: 28px;
    font-size: var(--text-base);
    color: var(--text-primary);
}

.key {
    flex: none;
    width: 16px;
    display: inline-flex;
    justify-content: center;
}

.key-dot::before,
.key-square::before {
    content: '';
    width: 10px;
    height: 10px;
    background: currentColor;
    box-shadow: 0 0 0 1.5px var(--bg-app);
}

.key-dot::before { border-radius: var(--radius-full); }
.key-square::before { border-radius: 3px; }

.key-line::before {
    content: '';
    width: 16px;
    height: 0;
    border-top: 2px solid currentColor;
}

.key-line-dashed::before { border-top-style: dashed; }

.key-swatch::before {
    content: '';
    width: 12px;
    height: 12px;
    border-radius: var(--radius-sm);
    background: currentColor;
    opacity: 0.6; /* matches the map fill opacity in section 8.2 */
}

/* Color helpers. The glyphs above draw in currentColor. */
.status-idle { color: var(--status-idle); }
.status-offered { color: var(--status-offered); }
.status-assigned { color: var(--status-assigned); }
.status-queued { color: var(--status-queued); }
.surge-low { color: var(--surge-low); }
.surge-mid { color: var(--surge-mid); }
.surge-high { color: var(--surge-high); }
```

No glow (`box-shadow: 0 0 8px currentColor`) anywhere. Remove it.

## 8. Map

### 8.1 Markers

Markers are flat circles (couriers) and rounded squares (orders) with a thin
dark ring that separates them from the basemap. No white borders, no glow.

```css
.marker-courier,
.marker-order {
    width: 10px;
    height: 10px;
    background: currentColor;
    box-shadow: 0 0 0 1.5px var(--bg-app);
    transition: color var(--duration-fast) var(--ease);
}

.marker-courier { border-radius: var(--radius-full); }
.marker-order { border-radius: 3px; }
```

The marker color comes from the `status-*` helper classes in 7.8. In
`app.js`, change the class names that are assigned to marker elements. This
is a rename only. Do not change when they are assigned.

| Old class string in `app.js` | New class string |
| --- | --- |
| `'marker-courier courier-idle'` | `'marker-courier status-idle'` |
| `'marker-courier courier-assigned'` | `'marker-courier status-assigned'` |
| `'marker-courier courier-offered'` | `'marker-courier status-offered'` |
| `'marker-order order-assigned'` | `'marker-order status-assigned'` |
| `'marker-order order-queued'` | `'marker-order status-queued'` |

After this, delete the old `.courier-*` and `.order-*` rules from the CSS.

### 8.2 Map layers

MapLibre paint properties cannot read CSS variables. Read them once at load
time so the map and the CSS can never drift apart. Add this near the top of
`app.js`, after the `map` is created:

```js
// Map paint colors come from the CSS tokens so the map and the legend match.
const css = getComputedStyle(document.documentElement);
const token = (name) => css.getPropertyValue(name).trim();
```

Then change the paint properties inside `map.on('load', ...)`:

| Layer | Property | New value |
| --- | --- | --- |
| `surge-fill` | `fill-color` | `['interpolate', ['linear'], ['get', 'multiplier'], 1.2, token('--surge-low'), 2, token('--surge-mid'), 3, token('--surge-high')]` |
| `surge-fill` | `fill-opacity` | `0.6` (the new ramp is darker, so it needs more opacity) |
| `surge-outline` | `line-color` | `token('--surge-mid')` |
| `surge-outline` | `line-opacity` | `0.5` |
| `assignment-lines` | `line-color` | `token('--status-assigned')` |
| `assignment-lines` | `line-width` | `1.5` |
| `pending-offer-lines` | `line-color` | `token('--status-offered')` |
| `pending-offer-lines` | `line-width` | `1.5` |
| `pending-offer-lines` | `line-dasharray` | `[3, 3]` |

Keep the basemap style URL (Carto `dark-matter-gl-style`), the center, the
zoom, and the pitch. Dark Matter already matches the palette.

### 8.3 MapLibre chrome

The attribution box and any controls must use the same tokens.

```css
.maplibregl-ctrl-attrib {
    background: var(--bg-overlay) !important;
    color: var(--text-secondary);
    font-size: var(--text-xs);
    border-radius: var(--radius-sm) 0 0 0;
}

.maplibregl-ctrl-attrib a { color: var(--text-secondary); }

.maplibregl-ctrl-attrib a:hover { color: var(--text-primary); }

.maplibregl-ctrl-attrib-button { filter: invert(1); }
```

The `!important` is needed
because MapLibre's own stylesheet loads first with a white background.

## 9. Small JS hooks

These are the only behavior-adjacent edits. Each one sets an attribute that
the CSS reads. Nothing else in the JS changes.

### 9.1 Connection pill (`app.js`)

The old header said "Live" even when the socket was down. Now the pill shows
the real state. Change `connect()` to:

```js
const connEl = document.getElementById('conn');
const connLabel = document.getElementById('conn-label');
function setConn(state, label) {
    connEl.dataset.state = state;
    connLabel.textContent = label;
}

function connect() {
    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    const ws = new WebSocket(`${protocol}//${window.location.host}/live`);

    ws.onopen = () => setConn('connected', 'Live');
    ws.onmessage = (event) => {
        try {
            updateDashboard(JSON.parse(event.data));
        } catch (e) {
            console.error('failed to parse message', e);
        }
    };
    ws.onclose = () => {
        setConn('disconnected', 'Reconnecting');
        setTimeout(connect, 1000);
    };
    ws.onerror = () => ws.close();
}
connect();
```

### 9.2 Simulator pill (`sim.js`)

Add one line in each of two functions:

- In `showOffline()`, after `stateEl.textContent = 'offline';` add
  `stateEl.dataset.state = 'offline';`
- In `showStatus(st)`, after the `stateEl.textContent = ...` line add
  `stateEl.dataset.state = running ? 'running' : 'idle';`

The HTML already starts it at `data-state="checking"`.

## 10. Responsive

Breakpoint: 767px and below is "narrow". Use the same breakpoint as
`sim.js` (`min-width: 768px`).

On narrow screens the frame goes away and the inspector stacks under the map.
The page scrolls.

```css
@media (max-width: 767px) {
    body { overflow: auto; }

    .app {
        height: auto;
        min-height: 100dvh;
    }

    .workspace {
        grid-template-columns: 1fr;
        margin: 0;
        border: 0;
        border-top: 1px solid var(--border-default);
        border-radius: 0;
        overflow: visible;
    }

    .map-pane {
        height: 55vh;
        min-height: 320px;
    }

    .inspector {
        overflow: visible;
        border-left: 0;
        border-top: 1px solid var(--border-default);
    }

    /* 44px touch targets */
    .btn,
    .input,
    .range,
    .section-summary {
        min-height: 44px;
    }
}
```

At 768px to 1023px the layout stays two-column. Check that the stat labels
truncate with an ellipsis instead of wrapping.

## 11. Motion and accessibility

- Only `background-color`, `border-color`, `color` and the chevron
  `transform` animate. Duration is `--duration-fast`. Nothing animates on load.
- Respect reduced motion:

```css
@media (prefers-reduced-motion: reduce) {
    *, *::before, *::after {
        transition: none !important;
    }
}
```

- Keep every `aria-*`, `role`, `title`, `for`, and `autocomplete` attribute
  from the old markup. The section 5.1 markup already has them.
- The topbar pill is `role="status"` with `aria-live="polite"`, so a screen
  reader hears "Reconnecting" and "Live".
- Never remove a focus outline without the `:focus-visible` replacement in 7.7.

## 12. Grafana (same palette, second surface)

Grafana at `http://localhost:3001` is the other place a person looks at
Route. Make its series colors match the dashboard so the two read as one
product.

In `deploy/grafana/dashboards/route.json`:

For each panel below, set `fieldConfig.defaults.color` to
   `{ "mode": "fixed", "fixedColor": "<hex>" }` for single-series panels, or
   add one override per series for multi-series panels:

```json
{
  "matcher": { "id": "byName", "options": "<series legend name>" },
  "properties": [{ "id": "color", "value": { "mode": "fixed", "fixedColor": "<hex>" } }]
}
```

| Panel title | Series | Hex |
| --- | --- | --- |
| Orders per second | all | `#5e6ad2` (accent) |
| Match latency | p50 | `#4ea7fc` |
| Match latency | p99 | `#f2c94c` |
| Match rate | all | `#4cb782` |
| Queue depth | all | `#f2994a` |
| Offer outcomes | accepted | `#4cb782` |
| Offer outcomes | rejected, expired | `#eb5757`, `#f2994a` |
| Batched against greedy | batched | `#5e6ad2` |
| Batched against greedy | greedy | `#8a8f98` |
| Claim conflicts | all | `#eb5757` |
| Everything else | first series | `#5e6ad2`, then `#4ea7fc`, `#4cb782`, `#f2c94c`, `#f2994a`, `#8a8f98` in order |

Read each panel's `targets[].legendFormat` to get the exact series names for
the `byName` matcher. If a name does not match one in the table, use the
"Everything else" order. Do not change queries, units, or thresholds.

## 13. Work order and checks

Do the steps in this order. Check each one before the next.

1. Rewrite `index.html` per 5.1. Open it and confirm no `id` from the old file
   is missing: `val-orders-s`, `val-latency`, `val-match-rate`,
   `val-batch-gain`, `val-shed`, `val-queue`, `val-couriers`, `val-surge`,
   `metrics-panel`, `map`, `sim-controls`, `sim-state`, `sim-token`,
   `sim-token-help`, `sim-token-error`, `sim-profile`, `sim-start`,
   `sim-stop`, `sim-rate`, `sim-rate-out`, `sim-message`.
2. Rewrite `style.css` from sections 3 to 11, in that order. Then search the
   file for `#` followed by a hex digit. The only hits allowed are inside
   `:root` and the select data URI. Search for `uppercase` and
   `letter-spacing`: only the `-0.01em` on `.stat dd` may remain.
3. Edit `app.js` per 8.1, 8.2 and 9.1.
4. Edit `sim.js` per 9.2.
5. Edit the Grafana JSON per 12. Validate it parses (for example
   `python -m json.tool deploy/grafana/dashboards/route.json`).
6. Run it. The static files are copied into the `routed` image, so rebuild:
   `make up`, then `make sim` (or `make sim-control` to see the simulator
   panel). Open `http://localhost:8080`.
7. Visual checks at 1440px, 1024px and 375px wide:
   - The workspace frame has an 8px gap to the window edge and rounded
     corners on desktop, none on mobile.
   - The topbar pill reads "Live" with a green dot. Stop `routed`
     (`docker-compose -f deploy/compose/docker-compose.yml stop routed`) and
     it turns red and reads "Reconnecting". Start it again.
   - Courier and order markers use the four status colors, and they match the
     legend glyphs exactly.
   - Surge hexagons are indigo to violet, not yellow to red.
   - Tab through the simulator panel. Every control shows the indigo focus
     ring.
   - The stat values do not shift sideways as they update.
8. Open Grafana at `http://localhost:3001` and confirm the series colors.

## 14. Do not

- Do not add a CSS framework, Tailwind, an icon library, or a JS framework.
  This is three static files served by Go's `http.FileServer`.
- Do not add a left sidebar. Route has one view. A sidebar with one link is
  decoration.
- Do not add light mode. The map basemap is dark only.
- Do not use gradients except on `.brand-mark`.
- Do not add shadows except `--shadow-popover` (reserved, unused today) and
  the 1.5px marker ring.
- Do not use glassmorphism (`backdrop-filter: blur`). Remove it.
- Do not put emoji in the UI.
- Do not change any copy except the strings this file gives (breadcrumb,
  section titles, legend labels, pill labels).

## 15. Open notes

Add anything here that this file did not cover, with the value you chose and
why, so the next pass can fold it into the tokens.
