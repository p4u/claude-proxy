import { api } from "../api.js";
import { el, clear, spinner, errorState, emptyState, statusBadge } from "../ui.js";
import { meter, chartFrame, periodControl, segmented, sectionHead } from "../components.js";
import { getWindow, setWindowPeriod, setWindowCustom } from "../store.js";
import { timeChart } from "../charts.js";
import { pct, relTime, countdown } from "../format.js";

export async function render(root) {
  clear(root);
  // The page header no longer carries the period picker: cards always show the
  // latest snapshot, so the picker is scoped down to the charts section below.
  const head = sectionHead(
    "Subscriptions",
    "Remote 5-hour and weekly quota utilization per credential. 100% is a hard ceiling — requests 429 on whichever window fills first.",
  );
  const cards = el("div", { class: "grid grid--cards" }, spinner("Reading utilization…"));
  const chartsWrap = el("div", { class: "usage-charts" });
  root.append(head, cards, chartsWrap);

  try {
    const rows = await api.usageCurrent();
    clear(cards);
    if (!rows || !rows.length) {
      cards.append(emptyState("No subscriptions yet", "Import a credential to start multiplexing. The usage poller records a snapshot every 10 minutes."));
    } else {
      for (const r of rows) cards.append(subCard(r));
    }
  } catch (e) {
    clear(cards).append(errorState(e.message, () => render(root)));
  }

  renderCharts(chartsWrap);
}

// Charts section: a scoped period picker driving the History + Selection charts.
function renderCharts(wrap) {
  clear(wrap);
  const win = getWindow();
  const picker = el("div", { class: "usage-charts__head" }, [
    el("div", {}, [
      el("div", { class: "eyebrow", text: "time window" }),
      el("p", { class: "usage-charts__note", text: "Scopes the history and selection charts below. Cards above always reflect the latest snapshot." }),
    ]),
    periodControl(win, (sel) => {
      if (sel.mode === "custom") setWindowCustom(sel.from, sel.to);
      else setWindowPeriod(sel.period);
      renderCharts(wrap);
    }),
  ]);
  const grid = el("div", { class: "grid grid--charts" });
  wrap.append(picker, grid);
  historyChart(grid, win);
  selectionChart(grid, win);
}

function subCard(r) {
  const five = r.five_hour || {};
  const seven = r.seven_day || {};
  const sonnet = r.seven_day_sonnet || {};
  const sel = r.selection || null;
  return el("div", { class: "card sub-card" }, [
    el("div", { class: "sub-card__head" }, [
      el("div", {}, [
        el("div", { class: "sub-card__name", text: r.label || r.credential_id }),
        el("div", { class: "sub-card__meta", text: [r.subscription_type, `weight ${r.weight}`].filter(Boolean).join(" · ") }),
      ]),
      el("div", { class: "sub-card__badges" }, [
        sel && sel.saturated ? el("span", { class: "badge badge--critical", title: "Excluded from new sessions" }, [
          el("span", { class: "badge__dot" }), el("span", { text: "saturated" }),
        ]) : null,
        statusBadge(r.status),
      ]),
    ]),
    el("div", { class: "sub-card__meters" }, [
      meter({ label: "5-hour window", value: five.pct, resets: countdown(five.resets_at) }),
      meter({ label: "7-day window", value: seven.pct, resets: countdown(seven.resets_at) }),
      meter({ label: "7-day Sonnet", value: sonnet.pct, resets: countdown(sonnet.resets_at) }),
    ]),
    sel ? selectionRow(sel) : null,
    el("div", { class: "sub-card__foot", text: r.captured_at ? `snapshot ${relTime(tsOf(r.captured_at))}` : "no snapshot yet" }),
  ]);
}

// Selection metric row: pool share + raw score (weight × room_5h × room_7d^1.5).
function selectionRow(sel) {
  const share = sel.share_pct == null ? "—" : pct(sel.share_pct, sel.share_pct >= 10 ? 0 : 1);
  const saturated = !!sel.saturated;
  return el("div", { class: "sub-card__sel" + (saturated ? " sub-card__sel--sat" : "") }, [
    el("span", { class: "sub-card__sel-lbl", text: saturated ? "excluded from new sessions" : "selection share" }),
    el("span", { class: "sub-card__sel-val", text: saturated ? "0%" : share }),
    sel.score != null ? el("span", { class: "sub-card__sel-score", text: `score ${sel.score.toFixed(2)}` }) : null,
  ]);
}

function tsOf(v) {
  if (typeof v === "number") return v;
  const t = Date.parse(v);
  return isNaN(t) ? 0 : t / 1000;
}

// History chart — consumes the aligned-grid shape:
// {buckets:[ts...], series:[{credential_id,label,five_hour_pct:[...], ...}]}.
// One value per bucket per series, null where a credential has no snapshot;
// uPlot renders the gaps natively.
async function historyChart(wrap, win) {
  let metric = "five_hour_pct";
  const metricOpts = [
    { value: "five_hour_pct", label: "5-hour" },
    { value: "seven_day_pct", label: "7-day" },
    { value: "seven_day_sonnet_pct", label: "Sonnet" },
  ];
  const frame = chartFrame({
    eyebrow: "history",
    title: "Utilization history",
    toolbar: [segmented(metricOpts, metric, (m) => { metric = m; draw(); }, "Metric")],
  });
  frame.root.classList.add("chart--wide");
  wrap.append(frame.root);

  async function draw() {
    clear(frame.plot);
    clear(frame.legendSlot);
    frame.plot.append(spinner());
    try {
      const d = await api.usageHistory(win);
      clear(frame.plot);
      const buckets = d.buckets || [];
      const seriesRaw = d.series || [];
      const hasData = buckets.length && seriesRaw.some((s) => (s[metric] || []).some((v) => v != null));
      if (!hasData) {
        frame.plot.append(emptyState("No history yet", "The poller records utilization every 10 minutes — check back after the next cycle."));
        return;
      }
      const series = seriesRaw.map((s) => ({
        label: s.label || s.credential_id,
        values: s[metric] || [],
      }));
      const ch = timeChart(frame.plot, {
        buckets, series, mode: "line", fmt: (v) => pct(v), height: 260,
        yRange: [0, 100],
      });
      markCeiling(frame.plot, ch.u);
      frame.legendSlot.append(ch.legendEl);
    } catch (e) {
      clear(frame.plot).append(errorState(e.message, draw));
    }
  }
  draw();
}

// Selection chart — how often each credential was picked for NEW conversations,
// stacked over time. Legend labels carry each credential's total share_pct.
async function selectionChart(wrap, win) {
  const frame = chartFrame({ eyebrow: "selection", title: "New-session picks by credential" });
  frame.root.classList.add("chart--wide");
  wrap.append(frame.root);

  async function draw() {
    clear(frame.plot);
    clear(frame.legendSlot);
    frame.plot.append(spinner());
    try {
      const d = await api.statsSelection(win, 60);
      clear(frame.plot);
      const buckets = d.buckets || [];
      const seriesRaw = d.series || [];
      const hasData = buckets.length && seriesRaw.some((s) => (s.picks || []).some((v) => v));
      if (!hasData) {
        frame.plot.append(emptyState("No picks yet", "Selection is recorded when a new conversation binds to a credential."));
        return;
      }
      const shareById = new Map((d.totals || []).map((t) => [t.credential_id, t.share_pct]));
      const series = seriesRaw.map((s) => {
        const share = shareById.get(s.credential_id);
        const base = s.label || s.credential_id;
        return {
          label: share == null ? base : `${base} · ${pct(share, share >= 10 ? 0 : 1)}`,
          values: s.picks || [],
        };
      });
      const ch = timeChart(frame.plot, { buckets, series, mode: "stack", fmt: (v) => String(v), height: 260 });
      frame.legendSlot.append(ch.legendEl);
    } catch (e) {
      clear(frame.plot).append(errorState(e.message, draw));
    }
  }
  draw();
}

// Draw a dashed 100% ceiling line + label over the plot area.
function markCeiling(container, u) {
  const overlay = () => {
    const old = container.querySelector(".ceiling");
    if (old) old.remove();
    if (!u.valToPos) return;
    const y = u.valToPos(100, "y", false);
    const line = el("div", { class: "ceiling", text: "100% ceiling", style: `top:${y}px` });
    container.querySelector(".u-over")?.appendChild(line);
  };
  setTimeout(overlay, 0);
  const ro = new ResizeObserver(() => setTimeout(overlay, 0));
  ro.observe(container);
}
