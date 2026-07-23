// Tiny shared state: selected period + theme, persisted to localStorage.

const KEY = "cpui.prefs";
const load = () => {
  try {
    return JSON.parse(localStorage.getItem(KEY)) || {};
  } catch {
    return {};
  }
};
let prefs = load();
const save = () => localStorage.setItem(KEY, JSON.stringify(prefs));

export const PERIODS = [
  { value: "1h", label: "1H" },
  { value: "6h", label: "6H" },
  { value: "24h", label: "24H" },
  { value: "7d", label: "7D" },
  { value: "30d", label: "30D" },
];

export function getPeriod() {
  return prefs.period || "24h";
}
export function setPeriod(p) {
  prefs.period = p;
  prefs.mode = "period";
  save();
}

// ---- Time-window selection (preset period OR custom from/to range) ----
// Shape: {mode:"period"|"custom", period, from, to}. `from`/`to` are unix
// seconds. Pages pass this object straight to the api layer (winParams).
export const MAX_CUSTOM_SPAN = 90 * 86400; // backend rejects spans > 90d

export function getWindow() {
  const custom = prefs.mode === "custom" && prefs.from != null && prefs.to != null;
  return {
    mode: custom ? "custom" : "period",
    period: prefs.period || "24h",
    from: custom ? prefs.from : null,
    to: custom ? prefs.to : null,
  };
}
export function setWindowPeriod(p) {
  prefs.mode = "period";
  prefs.period = p;
  save();
}
export function setWindowCustom(from, to) {
  prefs.mode = "custom";
  prefs.from = from;
  prefs.to = to;
  save();
}
// Validate a custom range. Returns an error string, or null when valid.
export function validateRange(from, to) {
  if (from == null || to == null || isNaN(from) || isNaN(to)) return "Enter both a start and end time.";
  if (from >= to) return "Start must be before end.";
  if (to - from > MAX_CUSTOM_SPAN) return "Range must be 90 days or less.";
  return null;
}

export function getTheme() {
  return prefs.theme || "auto"; // auto | light | dark
}
export function setTheme(t) {
  prefs.theme = t;
  save();
  applyTheme();
}
export function applyTheme() {
  const t = getTheme();
  if (t === "auto") document.documentElement.removeAttribute("data-theme");
  else document.documentElement.setAttribute("data-theme", t);
}

// Period → seconds (for axis span hints).
export function periodSeconds(p) {
  return { "1h": 3600, "6h": 21600, "24h": 86400, "7d": 604800, "30d": 2592000 }[p] || 86400;
}
