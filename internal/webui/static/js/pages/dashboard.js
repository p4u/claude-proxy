import { api } from "../api.js";
import { el, clear, spinner, errorState, emptyState } from "../ui.js";
import { statTile, segmented, chartFrame, legend, sectionHead } from "../components.js";
import { getPeriod, setPeriod, PERIODS } from "../store.js";
import { timeChart } from "../charts.js";
import { compactNum, fullNum, ms, pct } from "../format.js";

const GROUP_OPTS = [
  { value: "user", label: "By user" },
  { value: "credential", label: "By credential" },
];

export async function render(root) {
  clear(root);
  const period = getPeriod();

  const tilesWrap = el("div", { class: "tiles" }, spinner("Loading overview…"));
  const head = sectionHead(
    "Control room",
    "Live traffic across every multiplexed subscription.",
    [segmented(PERIODS, period, (p) => { setPeriod(p); render(root); }, "Time period")]
  );
  const chartsWrap = el("div", { class: "grid grid--charts" });
  root.append(head, tilesWrap, chartsWrap);

  // Overview tiles
  try {
    const o = await api.overview();
    clear(tilesWrap);
    const tin = o.tokens_24h || {};
    const cr = o.credentials || {};
    tilesWrap.append(
      statTile({ label: "Requests · 24h", value: compactNum(o.requests_24h), sub: fullNum(o.requests_24h) + " total" }),
      statTile({ label: "Tokens in · 24h", value: compactNum(tin.input), unit: "tok" }),
      statTile({ label: "Tokens out · 24h", value: compactNum(tin.output), unit: "tok" }),
      statTile({ label: "Cache reads · 24h", value: compactNum(tin.cache_read), unit: "tok" }),
      statTile({
        label: "Error rate · 24h",
        value: pct((o.error_rate_24h || 0) * 100),
        tone: (o.error_rate_24h || 0) > 0.05 ? "alert" : null,
      }),
      statTile({ label: "Avg latency · 24h", value: fullNum(o.avg_latency_ms_24h), unit: "ms" }),
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
  requestsChart(chartsWrap, period);
  tokensChart(chartsWrap, period);
  latencyChart(chartsWrap, period);
}

function mountChart(frame, buckets, series, mode, fmt, yRange) {
  if (!buckets || !buckets.length || !series.length) {
    frame.plot.append(emptyState("No data yet", "The request log fills as traffic flows through the proxy."));
    return;
  }
  const ch = timeChart(frame.plot, { buckets, series, mode, fmt, height: 240, yRange });
  frame.legendSlot.append(legend(ch.legendItems));
}

async function requestsChart(wrap, period) {
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
      const d = await api.statsRequests(period, 60, group);
      clear(frame.plot);
      const series = (d.series || []).map((s) => ({ label: s.label, values: s.requests }));
      mountChart(frame, d.buckets, series, "stack", compactNum);
    } catch (e) {
      clear(frame.plot).append(errorState(e.message, draw));
    }
  }
  draw();
}

async function tokensChart(wrap, period) {
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
      const d = await api.statsTokens(period, 60, group);
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

async function latencyChart(wrap, period) {
  const frame = chartFrame({ eyebrow: "responsiveness", title: "Latency (avg / p95)" });
  frame.root.classList.add("chart--wide");
  wrap.append(frame.root);
  async function draw() {
    clear(frame.plot);
    clear(frame.legendSlot);
    frame.plot.append(spinner());
    try {
      const d = await api.statsLatency(period, 60);
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
      frame.legendSlot.append(legend(ch.legendItems));
    } catch (e) {
      clear(frame.plot).append(errorState(e.message, draw));
    }
  }
  draw();
}
