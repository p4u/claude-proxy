import { api } from "../api.js";
import { el, clear, spinner, errorState, emptyState } from "../ui.js";
import { statTile, segmented, chartFrame, periodControl, sectionHead } from "../components.js";
import { getWindow, setWindowPeriod, setWindowCustom, windowLabel } from "../store.js";
import { timeChart } from "../charts.js";
import { compactNum, fullNum, ms, pct } from "../format.js";

const GROUP_OPTS = [
  { value: "user", label: "By user" },
  { value: "credential", label: "By credential" },
];

// Kill one chart before a per-group redraw. charts.js auto-destroys detached
// instances, so no module-level registry is needed here. Returns null so
// callers write: cur = killChart(cur).
function killChart(ch) {
  if (!ch) return null;
  ch.destroy();
  return null;
}

function onWindowChange(root, sel) {
  if (sel.mode === "custom") setWindowCustom(sel.from, sel.to);
  else setWindowPeriod(sel.period);
  render(root);
}

export async function render(root) {
  clear(root);
  const win = getWindow();

  const tilesWrap = el("div", { class: "tiles" }, spinner("Loading overview…"));
  const head = sectionHead(
    "Control room",
    "Live traffic across every multiplexed subscription.",
    [periodControl(win, (sel) => onWindowChange(root, sel))]
  );
  // Uniform 2-column grid; see dashboard.css for the minmax(0, 1fr) column rule.
  const chartsWrap = el("div", { class: "dash-charts" });
  root.append(head, tilesWrap, chartsWrap);

  // Overview tiles — follow the global window. Field names lost the `_24h`
  // suffix in v2; keep a fallback to the old names for older backends.
  try {
    const o = await api.overview(win);
    // Guard: the user may have navigated away while the request was in flight.
    if (!tilesWrap.isConnected) return;
    clear(tilesWrap);
    const wl = windowLabel(win);
    const requests = o.requests ?? o.requests_24h;
    const tin = o.tokens || o.tokens_24h || {};
    const errRate = o.error_rate ?? o.error_rate_24h ?? 0;
    const avgLat = o.avg_latency_ms ?? o.avg_latency_ms_24h;
    const cr = o.credentials || {};
    tilesWrap.append(
      statTile({ label: `Requests · ${wl}`, value: compactNum(requests), sub: fullNum(requests) + " total" }),
      statTile({ label: `Tokens in · ${wl}`, value: compactNum(tin.input), unit: "tok" }),
      statTile({ label: `Tokens out · ${wl}`, value: compactNum(tin.output), unit: "tok" }),
      statTile({ label: `Cache reads · ${wl}`, value: compactNum(tin.cache_read), unit: "tok" }),
      statTile({
        label: `Error rate · ${wl}`,
        value: pct(errRate * 100),
        tone: errRate > 0.05 ? "alert" : null,
      }),
      statTile({ label: `Avg latency · ${wl}`, value: fullNum(avgLat), unit: "ms" }),
      statTile({ label: "Active convs", value: fullNum(o.active_conversations) }),
      statTile({
        label: "Credentials",
        value: fullNum(cr.active),
        unit: `/ ${fullNum(cr.total)}`,
        sub: [cr.limited ? `${cr.limited} limited` : null, cr.errored ? `${cr.errored} errored` : null].filter(Boolean).join(" · ") || "all healthy",
        tone: cr.errored ? "alert" : null,
      })
    );
  } catch (e) {
    if (!tilesWrap.isConnected) return;
    clear(tilesWrap).append(errorState(e.message, () => render(root)));
  }

  // Charts: uniform 2-column grid, 6 panels in 3 rows.
  // Row 1 — aggregate totals.
  totalsTokensChart(chartsWrap, win);
  totalsRequestsChart(chartsWrap, win);
  // Row 2 — by user / credential (group selector retained).
  requestsChart(chartsWrap, win);
  tokensChart(chartsWrap, win);
  // Row 3 — responsiveness + new-conversation binding distribution.
  latencyChart(chartsWrap, win);
  selectionChart(chartsWrap, win);
}

// ---- Shared helpers ----

// Wrap chartFrame and tag the root with the page-scoped class so dashboard.css
// selectors apply without bleeding into other pages.
function newFrame(opts) {
  const frame = chartFrame(opts);
  frame.root.classList.add("dash-chart");
  return frame;
}

// Build a time chart into frame.plot and attach the interactive legend.
// Returns the chart handle or null when there is no data.
function mountChart(frame, buckets, series, mode, fmt, yRange) {
  if (!buckets || !buckets.length || !series.length) {
    frame.plot.append(emptyState("No data yet", "The request log fills as traffic flows through the proxy."));
    return null;
  }
  const ch = timeChart(frame.plot, { buckets, series, mode, fmt, height: 220, yRange });
  frame.legendSlot.append(ch.legendEl);
  return ch;
}

// ---- Totals (aggregate, no grouping) from /api/stats/totals ----

async function totalsTokensChart(wrap, win) {
  const frame = newFrame({ eyebrow: "totals", title: "Tokens by type" });
  wrap.append(frame.root);
  async function draw() {
    clear(frame.plot);
    clear(frame.legendSlot);
    frame.plot.append(spinner());
    try {
      const d = await api.statsTotals(win, 60);
      if (!frame.root.isConnected) return;
      clear(frame.plot);
      const t = d.tokens || {};
      // Ordered so stack colors map to palette 0..3 (blue/orange/green/amber).
      const series = [
        { label: "input", values: t.input },
        { label: "output", values: t.output },
        { label: "cache read", values: t.cache_read },
        { label: "cache creation", values: t.cache_creation },
      ].filter((s) => Array.isArray(s.values));
      mountChart(frame, d.buckets, series, "stack", compactNum);
    } catch (e) {
      if (!frame.root.isConnected) return;
      clear(frame.plot).append(errorState(e.message, draw));
    }
  }
  draw();
}

async function totalsRequestsChart(wrap, win) {
  const frame = newFrame({ eyebrow: "totals", title: "Requests & errors" });
  wrap.append(frame.root);
  async function draw() {
    clear(frame.plot);
    clear(frame.legendSlot);
    frame.plot.append(spinner());
    try {
      const d = await api.statsTotals(win, 60);
      if (!frame.root.isConnected) return;
      clear(frame.plot);
      if (!d.buckets || !d.buckets.length) {
        frame.plot.append(emptyState("No data yet", "The request log fills as traffic flows through the proxy."));
        return;
      }
      const series = [{ label: "requests", values: d.requests }];
      if (Array.isArray(d.errors)) series.push({ label: "errors", values: d.errors, dash: [5, 4] });
      const ch = timeChart(frame.plot, { buckets: d.buckets, series, mode: "line", fmt: compactNum, fill: true, height: 220 });
      frame.legendSlot.append(ch.legendEl);
    } catch (e) {
      if (!frame.root.isConnected) return;
      clear(frame.plot).append(errorState(e.message, draw));
    }
  }
  draw();
}

// ---- By user / credential (group selector) ----

async function requestsChart(wrap, win) {
  let group = "user";
  let cur = null;
  // Sequence counter: incremented before each fetch. A result whose counter no
  // longer matches the current value belongs to a superseded toggle and is
  // dropped, preventing rapid group switches from overwriting each other.
  let seq = 0;
  const frame = newFrame({
    eyebrow: "throughput",
    title: "Requests over time",
    toolbar: [segmented(GROUP_OPTS, group, (g) => { group = g; draw(); }, "Group requests by")],
  });
  wrap.append(frame.root);
  async function draw() {
    cur = killChart(cur);
    clear(frame.plot);
    clear(frame.legendSlot);
    frame.plot.append(spinner());
    const mySeq = ++seq;
    try {
      const d = await api.statsRequests(win, 60, group);
      if (!frame.root.isConnected || seq !== mySeq) return;
      clear(frame.plot);
      const series = (d.series || []).map((s) => ({ label: s.label, values: s.requests }));
      cur = mountChart(frame, d.buckets, series, "stack", compactNum);
    } catch (e) {
      if (!frame.root.isConnected || seq !== mySeq) return;
      clear(frame.plot).append(errorState(e.message, draw));
    }
  }
  draw();
}

async function tokensChart(wrap, win) {
  let group = "user";
  let cur = null;
  let seq = 0;
  const frame = newFrame({
    eyebrow: "consumption",
    title: "Tokens over time",
    toolbar: [segmented(GROUP_OPTS, group, (g) => { group = g; draw(); }, "Group tokens by")],
  });
  wrap.append(frame.root);
  async function draw() {
    cur = killChart(cur);
    clear(frame.plot);
    clear(frame.legendSlot);
    frame.plot.append(spinner());
    const mySeq = ++seq;
    try {
      const d = await api.statsTokens(win, 60, group);
      if (!frame.root.isConnected || seq !== mySeq) return;
      clear(frame.plot);
      const series = (d.series || []).map((s) => ({
        label: s.label,
        values: (s.tokens_in || []).map((v, i) => v + ((s.tokens_out && s.tokens_out[i]) || 0)),
      }));
      cur = mountChart(frame, d.buckets, series, "stack", compactNum);
    } catch (e) {
      if (!frame.root.isConnected || seq !== mySeq) return;
      clear(frame.plot).append(errorState(e.message, draw));
    }
  }
  draw();
}

// ---- Latency ----

async function latencyChart(wrap, win) {
  const frame = newFrame({ eyebrow: "responsiveness", title: "Latency (avg / p95)" });
  wrap.append(frame.root);
  async function draw() {
    clear(frame.plot);
    clear(frame.legendSlot);
    frame.plot.append(spinner());
    try {
      const d = await api.statsLatency(win, 60);
      if (!frame.root.isConnected) return;
      clear(frame.plot);
      if (!d.buckets || !d.buckets.length) {
        frame.plot.append(emptyState("No data yet", "Latency is recorded per forwarded request."));
        return;
      }
      const series = [
        { label: "avg", values: d.avg_ms },
        { label: "p95", values: d.p95_ms, dash: [5, 4] },
      ];
      const ch = timeChart(frame.plot, { buckets: d.buckets, series, mode: "line", fmt: ms, fill: false, height: 220 });
      frame.legendSlot.append(ch.legendEl);
    } catch (e) {
      if (!frame.root.isConnected) return;
      clear(frame.plot).append(errorState(e.message, draw));
    }
  }
  draw();
}

// ---- New-session picks — 6th panel (/api/stats/selection) ----
// Each "pick" is a new conversation binding: the pool selects a credential for
// the first request in a conversation, creating a sticky assignment. This panel
// shows how new bindings are distributed across credentials over time, making
// the weighted-random selection algorithm's behaviour visible to the operator.
async function selectionChart(wrap, win) {
  const frame = newFrame({ eyebrow: "routing", title: "New-session picks" });
  wrap.append(frame.root);
  async function draw() {
    clear(frame.plot);
    clear(frame.legendSlot);
    frame.plot.append(spinner());
    try {
      const d = await api.statsSelection(win, 60);
      if (!frame.root.isConnected) return;
      clear(frame.plot);
      const series = (d.series || []).map((s) => ({ label: s.label, values: s.picks }));
      mountChart(frame, d.buckets, series, "stack", compactNum);
    } catch (e) {
      if (!frame.root.isConnected) return;
      clear(frame.plot).append(errorState(e.message, draw));
    }
  }
  draw();
}
