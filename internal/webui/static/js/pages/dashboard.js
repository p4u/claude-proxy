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
  const chartsWrap = el("div", { class: "grid grid--charts" });
  root.append(head, tilesWrap, chartsWrap);

  // Overview tiles — follow the global window. Field names lost the `_24h`
  // suffix in v2; keep a fallback to the old names for older backends.
  try {
    const o = await api.overview(win);
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
    clear(tilesWrap).append(errorState(e.message, () => render(root)));
  }

  // Charts
  totalsTokensChart(chartsWrap, win);
  totalsRequestsChart(chartsWrap, win);
  requestsChart(chartsWrap, win);
  tokensChart(chartsWrap, win);
  latencyChart(chartsWrap, win);
}

// ---- Totals (aggregate, no grouping) from /api/stats/totals ----
// Two charts, one y-scale each: tokens stacked by type (wide) + requests line.
async function totalsTokensChart(wrap, win) {
  const frame = chartFrame({ eyebrow: "totals", title: "Tokens by type" });
  frame.root.classList.add("chart--wide");
  wrap.append(frame.root);
  async function draw() {
    clear(frame.plot);
    clear(frame.legendSlot);
    frame.plot.append(spinner());
    try {
      const d = await api.statsTotals(win, 60);
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
      clear(frame.plot).append(errorState(e.message, draw));
    }
  }
  draw();
}

async function totalsRequestsChart(wrap, win) {
  const frame = chartFrame({ eyebrow: "totals", title: "Requests & errors" });
  wrap.append(frame.root);
  async function draw() {
    clear(frame.plot);
    clear(frame.legendSlot);
    frame.plot.append(spinner());
    try {
      const d = await api.statsTotals(win, 60);
      clear(frame.plot);
      if (!d.buckets || !d.buckets.length) {
        frame.plot.append(emptyState("No data yet", "The request log fills as traffic flows through the proxy."));
        return;
      }
      const series = [{ label: "requests", values: d.requests }];
      if (Array.isArray(d.errors)) series.push({ label: "errors", values: d.errors, dash: [5, 4] });
      const ch = timeChart(frame.plot, { buckets: d.buckets, series, mode: "line", fmt: compactNum, fill: true, height: 240 });
      frame.legendSlot.append(ch.legendEl);
    } catch (e) {
      clear(frame.plot).append(errorState(e.message, draw));
    }
  }
  draw();
}

function mountChart(frame, buckets, series, mode, fmt, yRange) {
  if (!buckets || !buckets.length || !series.length) {
    frame.plot.append(emptyState("No data yet", "The request log fills as traffic flows through the proxy."));
    return;
  }
  const ch = timeChart(frame.plot, { buckets, series, mode, fmt, height: 240, yRange });
  frame.legendSlot.append(ch.legendEl);
}

async function requestsChart(wrap, win) {
  let group = "user";
  const frame = chartFrame({
    eyebrow: "throughput",
    title: "Requests over time",
    toolbar: [segmented(GROUP_OPTS, group, (g) => { group = g; draw(); }, "Group requests by")],
  });
  wrap.append(frame.root);
  async function draw() {
    clear(frame.plot);
    clear(frame.legendSlot);
    frame.plot.append(spinner());
    try {
      const d = await api.statsRequests(win, 60, group);
      clear(frame.plot);
      const series = (d.series || []).map((s) => ({ label: s.label, values: s.requests }));
      mountChart(frame, d.buckets, series, "stack", compactNum);
    } catch (e) {
      clear(frame.plot).append(errorState(e.message, draw));
    }
  }
  draw();
}

async function tokensChart(wrap, win) {
  let group = "user";
  const frame = chartFrame({
    eyebrow: "consumption",
    title: "Tokens over time",
    toolbar: [segmented(GROUP_OPTS, group, (g) => { group = g; draw(); }, "Group tokens by")],
  });
  wrap.append(frame.root);
  async function draw() {
    clear(frame.plot);
    clear(frame.legendSlot);
    frame.plot.append(spinner());
    try {
      const d = await api.statsTokens(win, 60, group);
      clear(frame.plot);
      const series = (d.series || []).map((s) => ({
        label: s.label,
        values: (s.tokens_in || []).map((v, i) => v + ((s.tokens_out && s.tokens_out[i]) || 0)),
      }));
      mountChart(frame, d.buckets, series, "stack", compactNum);
    } catch (e) {
      clear(frame.plot).append(errorState(e.message, draw));
    }
  }
  draw();
}

async function latencyChart(wrap, win) {
  const frame = chartFrame({ eyebrow: "responsiveness", title: "Latency (avg / p95)" });
  frame.root.classList.add("chart--wide");
  wrap.append(frame.root);
  async function draw() {
    clear(frame.plot);
    clear(frame.legendSlot);
    frame.plot.append(spinner());
    try {
      const d = await api.statsLatency(win, 60);
      clear(frame.plot);
      if (!d.buckets || !d.buckets.length) {
        frame.plot.append(emptyState("No data yet", "Latency is recorded per forwarded request."));
        return;
      }
      const series = [
        { label: "avg", values: d.avg_ms },
        { label: "p95", values: d.p95_ms, dash: [5, 4] },
      ];
      const ch = timeChart(frame.plot, { buckets: d.buckets, series, mode: "line", fmt: ms, fill: false, height: 240 });
      frame.legendSlot.append(ch.legendEl);
    } catch (e) {
      clear(frame.plot).append(errorState(e.message, draw));
    }
  }
  draw();
}
