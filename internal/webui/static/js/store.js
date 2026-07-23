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
  save();
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
