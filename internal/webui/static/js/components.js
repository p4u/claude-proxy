// Reusable presentational blocks: stat tiles, meters, legends, chart frames, segmented toggles.

import { el } from "./ui.js";
import { compactNum, pct } from "./format.js";

export function statTile({ label, value, unit, sub, tone }) {
  return el("div", { class: "tile" + (tone ? ` tile--${tone}` : "") }, [
    el("div", { class: "tile__label", text: label }),
    el("div", { class: "tile__value" }, [
      el("span", { class: "tile__num", text: value }),
      unit ? el("span", { class: "tile__unit", text: unit }) : null,
    ]),
    sub ? el("div", { class: "tile__sub", text: sub }) : null,
  ]);
}

// Segmented control. options=[{value,label}]. onChange(value).
export function segmented(options, active, onChange, ariaLabel) {
  const root = el("div", { class: "seg", role: "tablist", "aria-label": ariaLabel || "options" });
  const btns = new Map();
  for (const o of options) {
    const b = el("button", {
      class: "seg__btn" + (o.value === active ? " is-active" : ""),
      role: "tab",
      "aria-selected": o.value === active ? "true" : "false",
      text: o.label,
      onClick: () => {
        if (o.value === active) return;
        active = o.value;
        for (const [v, node] of btns) {
          const on = v === active;
          node.classList.toggle("is-active", on);
          node.setAttribute("aria-selected", on ? "true" : "false");
        }
        onChange(o.value);
      },
    });
    btns.set(o.value, b);
    root.append(b);
  }
  return root;
}

// Legend row for a chart. items=[{label,color,dash}].
export function legend(items) {
  const root = el("div", { class: "legend" });
  for (const it of items) {
    root.append(
      el("span", { class: "legend__item" }, [
        el("span", {
          class: "legend__sw" + (it.dash ? " legend__sw--dash" : ""),
          style: `--sw:${it.color}`,
        }),
        el("span", { class: "legend__lbl", text: it.label }),
      ])
    );
  }
  return root;
}

// A titled chart frame with an optional toolbar (e.g. group-by toggle).
export function chartFrame({ title, eyebrow, toolbar }) {
  const plot = el("div", { class: "chart__plot" });
  const legendSlot = el("div", { class: "chart__legend" });
  const head = el("div", { class: "chart__head" }, [
    el("div", {}, [
      eyebrow ? el("div", { class: "eyebrow", text: eyebrow }) : null,
      el("h3", { class: "chart__title", text: title }),
    ]),
    toolbar ? el("div", { class: "chart__tools" }, toolbar) : null,
  ]);
  const root = el("div", { class: "card chart" }, [head, plot, legendSlot]);
  return { root, plot, legendSlot };
}

// Horizontal segmented utilization meter (the signature element): LED-style
// segments that fill to the current pct, with the 100% ceiling marked.
export function meter({ label, value, resets, segments = 20, tone }) {
  const v = Math.max(0, Math.min(100, value == null ? 0 : value));
  const filled = Math.round((v / 100) * segments);
  const t = tone || (v >= 90 ? "critical" : v >= 70 ? "warning" : "good");
  const bar = el("div", { class: `meter__bar meter__bar--${t}`, role: "meter", "aria-valuenow": v.toFixed(0), "aria-valuemin": "0", "aria-valuemax": "100", "aria-label": `${label} utilization ${v.toFixed(1)}%` });
  for (let i = 0; i < segments; i++) {
    bar.append(el("span", { class: "meter__seg" + (i < filled ? " is-on" : "") + (i === segments - 1 ? " meter__seg--ceil" : "") }));
  }
  return el("div", { class: "meter" }, [
    el("div", { class: "meter__top" }, [
      el("span", { class: "meter__label", text: label }),
      el("span", { class: `meter__pct meter__pct--${t}`, text: value == null ? "—" : pct(v) }),
    ]),
    bar,
    resets ? el("div", { class: "meter__resets", text: resets }) : null,
  ]);
}

export function sectionHead(title, sub, actions) {
  return el("div", { class: "sect-head" }, [
    el("div", {}, [
      el("h1", { class: "sect-title", text: title }),
      sub ? el("p", { class: "sect-sub", text: sub }) : null,
    ]),
    actions ? el("div", { class: "sect-actions" }, actions) : null,
  ]);
}
