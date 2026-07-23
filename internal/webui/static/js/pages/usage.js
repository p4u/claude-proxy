import { api } from "../api.js";
import { el, clear, spinner, errorState, emptyState, statusBadge } from "../ui.js";
import { meter, chartFrame, legend, segmented, sectionHead } from "../components.js";
import { getPeriod, setPeriod, PERIODS } from "../store.js";
import { timeChart, seriesColors } from "../charts.js";
import { pct, relTime, countdown } from "../format.js";

export async function render(root) {
  clear(root);
  const period = getPeriod();
  const head = sectionHead(
    "Subscriptions",
    "Remote 5-hour and weekly quota utilization per credential. 100% is a hard ceiling — requests 429 on whichever window fills first.",
    [segmented(PERIODS, period, (p) => { setPeriod(p); render(root); }, "History period")]
  );
  const cards = el("div", { class: "grid grid--cards" }, spinner("Reading utilization…"));
  const histWrap = el("div", { class: "hist-wrap" });
  root.append(head, cards, histWrap);

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

  historyChart(histWrap, period);
}

function subCard(r) {
  const five = r.five_hour || {};
  const seven = r.seven_day || {};
  const sonnet = r.seven_day_sonnet || {};
  return el("div", { class: "card sub-card" }, [
    el("div", { class: "sub-card__head" }, [
      el("div", {}, [
        el("div", { class: "sub-card__name", text: r.label || r.credential_id }),
        el("div", { class: "sub-card__meta", text: [r.subscription_type, `weight ${r.weight}`].filter(Boolean).join(" · ") }),
      ]),
      statusBadge(r.status),
    ]),
    el("div", { class: "sub-card__meters" }, [
      meter({ label: "5-hour window", value: five.pct, resets: countdown(five.resets_at) }),
      meter({ label: "7-day window", value: seven.pct, resets: countdown(seven.resets_at) }),
      meter({ label: "7-day Sonnet", value: sonnet.pct, resets: countdown(sonnet.resets_at) }),
    ]),
    el("div", { class: "sub-card__foot", text: r.captured_at ? `snapshot ${relTime(tsOf(r.captured_at))}` : "no snapshot yet" }),
  ]);
}

function tsOf(v) {
  if (typeof v === "number") return v;
  const t = Date.parse(v);
  return isNaN(t) ? 0 : t / 1000;
}

async function historyChart(wrap, period) {
  clear(wrap);
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
      const d = await api.usageHistory(period);
      clear(frame.plot);
      const seriesRaw = d.series || [];
      if (!seriesRaw.length || !seriesRaw.some((s) => (s.points || []).length)) {
        frame.plot.append(emptyState("No history yet", "The poller records utilization every 10 minutes — check back after the next cycle."));
        return;
      }
      // Unify timestamps across credentials.
      const tsSet = new Set();
      seriesRaw.forEach((s) => (s.points || []).forEach((p) => tsSet.add(tsOf(p.ts))));
      const buckets = [...tsSet].sort((a, b) => a - b);
      const idx = new Map(buckets.map((t, i) => [t, i]));
      const series = seriesRaw.map((s) => {
        const vals = new Array(buckets.length).fill(null);
        (s.points || []).forEach((p) => {
          const i = idx.get(tsOf(p.ts));
          if (i != null) vals[i] = p[metric];
        });
        return { label: s.label || s.credential_id, values: vals };
      });
      const ch = timeChart(frame.plot, {
        buckets, series, mode: "line", fmt: (v) => pct(v), height: 260,
        yRange: [0, 100],
      });
      // Mark the 100% ceiling.
      markCeiling(frame.plot, ch.u);
      frame.legendSlot.append(legend(ch.legendItems));
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
  u.hooks = u.hooks || {};
  setTimeout(overlay, 0);
  const ro = new ResizeObserver(() => setTimeout(overlay, 0));
  ro.observe(container);
}
