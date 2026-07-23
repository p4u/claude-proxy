// Chart wrappers over vendored uPlot (window.uPlot). One axis per chart, legend
// for multi-series, thin marks, hover crosshair + tooltip. Palette validated via
// the dataviz validator (both modes PASS).

import { axisTime, compactNum } from "./format.js";

const SERIES_LIGHT = ["#2a78d6", "#eb6834", "#1baf7a", "#eda100", "#e87ba4", "#008300", "#4a3aa7", "#e34948"];
const SERIES_DARK = ["#3987e5", "#d95926", "#199e70", "#c98500", "#d55181", "#008300", "#9085e9", "#e66767"];

function isDark() {
  const t = document.documentElement.getAttribute("data-theme");
  if (t === "dark") return true;
  if (t === "light") return false;
  return window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches;
}

export function seriesColors() {
  return isDark() ? SERIES_DARK : SERIES_LIGHT;
}

function chrome() {
  const dark = isDark();
  return {
    axis: dark ? "#6b807e" : "#898781",
    grid: dark ? "#22312f" : "#e6e8e5",
    ink: dark ? "#eaf2f0" : "#0d1a1c",
    surface: dark ? "#121d21" : "#ffffff",
  };
}

// hexToRgba for translucent fills.
function fade(hex, a) {
  const h = hex.replace("#", "");
  const r = parseInt(h.slice(0, 2), 16);
  const g = parseInt(h.slice(2, 4), 16);
  const b = parseInt(h.slice(4, 6), 16);
  return `rgba(${r},${g},${b},${a})`;
}

// Shared tooltip plugin: crosshair value readout anchored near cursor.
function tooltipPlugin(fmt) {
  let tip, over;
  return {
    hooks: {
      init(u) {
        over = u.over;
        tip = document.createElement("div");
        tip.className = "u-tip";
        tip.style.display = "none";
        over.appendChild(tip);
        over.addEventListener("mouseleave", () => (tip.style.display = "none"));
      },
      setCursor(u) {
        const { idx, left, top } = u.cursor;
        if (idx == null || left < 0) {
          tip.style.display = "none";
          return;
        }
        const ts = u.data[0][idx];
        let rows = "";
        for (let i = 1; i < u.series.length; i++) {
          const s = u.series[i];
          if (s.show === false) continue;
          const v = u.data[i][idx];
          if (v == null) continue;
          rows += `<div class="u-tip__row"><span class="u-tip__sw" style="background:${s._color}"></span><span class="u-tip__lbl">${s.label}</span><span class="u-tip__val">${fmt(v)}</span></div>`;
        }
        if (!rows) {
          tip.style.display = "none";
          return;
        }
        const when = new Date(ts * 1000).toLocaleString(undefined, {
          month: "short",
          day: "numeric",
          hour: "2-digit",
          minute: "2-digit",
        });
        tip.innerHTML = `<div class="u-tip__time">${when}</div>${rows}`;
        tip.style.display = "block";
        const tw = tip.offsetWidth;
        const bx = u.bbox.width / devicePixelRatio;
        let x = left + 14;
        if (x + tw > bx) x = left - tw - 14;
        tip.style.transform = `translate(${Math.max(4, x)}px, ${Math.max(4, top - 10)}px)`;
      },
    },
  };
}

function baseAxes(spanSec) {
  const c = chrome();
  const font = '11px ui-monospace, "SF Mono", Menlo, monospace';
  return [
    {
      stroke: c.axis,
      grid: { stroke: c.grid, width: 1 },
      ticks: { stroke: c.grid, width: 1, size: 4 },
      font,
      values: (u, splits) => splits.map((s) => axisTime(s, spanSec)),
      space: 64,
    },
    {
      stroke: c.axis,
      grid: { stroke: c.grid, width: 1 },
      ticks: { show: false },
      font,
      size: 52,
      values: (u, splits) => splits.map((s) => compactNum(s)),
    },
  ];
}

// Build a responsive uPlot into container. spec:
//   { buckets:[tsSec], series:[{label,values}], mode:'line'|'stack', fmt, height }
export function timeChart(container, spec) {
  const uPlot = window.uPlot;
  const colors = seriesColors();
  const c = chrome();
  const fmt = spec.fmt || ((v) => compactNum(v));
  const span = spec.buckets.length > 1 ? spec.buckets[spec.buckets.length - 1] - spec.buckets[0] : 3600;

  let data, uSeries;
  const legendItems = [];

  if (spec.mode === "stack") {
    // Cumulative arrays; draw top→bottom so lower series paint on top (solid fill to 0).
    const n = spec.buckets.length;
    const cum = spec.series.map(() => new Array(n).fill(0));
    for (let b = 0; b < n; b++) {
      let acc = 0;
      for (let s = 0; s < spec.series.length; s++) {
        acc += spec.series[s].values[b] || 0;
        cum[s][b] = acc;
      }
    }
    data = [spec.buckets];
    uSeries = [{}];
    const order = spec.series.map((_, i) => i).reverse();
    for (const i of order) {
      data.push(cum[i]);
      const col = colors[i % colors.length];
      uSeries.push({
        label: spec.series[i].label,
        _color: col,
        _raw: spec.series[i].values,
        stroke: col,
        fill: fade(col, isDark() ? 0.55 : 0.7),
        width: 1,
        points: { show: false },
      });
    }
    spec.series.forEach((s, i) => legendItems.push({ label: s.label, color: colors[i % colors.length] }));
  } else {
    data = [spec.buckets];
    uSeries = [{}];
    spec.series.forEach((s, i) => {
      const col = s.color || colors[i % colors.length];
      data.push(s.values);
      uSeries.push({
        label: s.label,
        _color: col,
        stroke: col,
        width: 2,
        fill: spec.fill ? fade(col, 0.12) : undefined,
        points: { show: false },
        dash: s.dash || undefined,
      });
      legendItems.push({ label: s.label, color: col, dash: s.dash });
    });
  }

  const stackFmt = spec.mode === "stack"
    ? (v) => v // tooltip overridden below to use raw
    : fmt;

  // For stacked, tooltip should show raw contribution not cumulative.
  const tip = spec.mode === "stack"
    ? {
        hooks: {
          init(u) {
            const t = document.createElement("div");
            t.className = "u-tip";
            t.style.display = "none";
            u.over.appendChild(t);
            u.over.addEventListener("mouseleave", () => (t.style.display = "none"));
            u._tip = t;
          },
          setCursor(u) {
            const t = u._tip;
            const { idx, left, top } = u.cursor;
            if (idx == null || left < 0) {
              t.style.display = "none";
              return;
            }
            let rows = "";
            for (let i = 1; i < u.series.length; i++) {
              const s = u.series[i];
              if (s.show === false) continue;
              const v = s._raw[idx];
              if (v == null) continue;
              rows += `<div class="u-tip__row"><span class="u-tip__sw" style="background:${s._color}"></span><span class="u-tip__lbl">${s.label}</span><span class="u-tip__val">${fmt(v)}</span></div>`;
            }
            const when = new Date(u.data[0][idx] * 1000).toLocaleString(undefined, {
              month: "short", day: "numeric", hour: "2-digit", minute: "2-digit",
            });
            t.innerHTML = `<div class="u-tip__time">${when}</div>${rows}`;
            t.style.display = "block";
            const tw = t.offsetWidth;
            const bx = u.bbox.width / devicePixelRatio;
            let x = left + 14;
            if (x + tw > bx) x = left - tw - 14;
            t.style.transform = `translate(${Math.max(4, x)}px, ${Math.max(4, top - 10)}px)`;
          },
        },
      }
    : tooltipPlugin(fmt);

  const opts = {
    width: container.clientWidth || 600,
    height: spec.height || 240,
    padding: [10, 8, 4, 4],
    cursor: {
      y: false,
      points: { size: 7, width: 2, stroke: (u, i) => u.series[i]._color, fill: c.surface },
      focus: { prox: 24 },
    },
    legend: { show: false },
    scales: { x: { time: false }, y: { range: spec.yRange } },
    axes: baseAxes(span),
    series: uSeries,
    plugins: [tip],
  };

  const u = new uPlot(opts, data, container);
  const ro = new ResizeObserver(() => {
    u.setSize({ width: container.clientWidth, height: spec.height || 240 });
  });
  ro.observe(container);
  return { u, legendItems, destroy: () => { ro.disconnect(); u.destroy(); } };
}
