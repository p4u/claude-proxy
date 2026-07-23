// Formatting helpers — compact numbers, latency, percentages, timestamps, countdowns.

export function compactNum(n) {
  if (n == null || isNaN(n)) return "—";
  const abs = Math.abs(n);
  if (abs < 1000) return String(Math.round(n));
  const units = [
    { v: 1e12, s: "T" },
    { v: 1e9, s: "B" },
    { v: 1e6, s: "M" },
    { v: 1e3, s: "K" },
  ];
  for (const u of units) {
    if (abs >= u.v) {
      const val = n / u.v;
      const str = val >= 100 ? val.toFixed(0) : val.toFixed(1).replace(/\.0$/, "");
      return str + u.s;
    }
  }
  return String(n);
}

export function fullNum(n) {
  if (n == null || isNaN(n)) return "—";
  return Math.round(n).toLocaleString("en-US");
}

export function ms(n) {
  if (n == null || isNaN(n)) return "—";
  if (n >= 10000) return (n / 1000).toFixed(1) + "s";
  return Math.round(n) + " ms";
}

export function pct(n, digits = 1) {
  if (n == null || isNaN(n)) return "—";
  return n.toFixed(digits) + "%";
}

export function bytes(n) {
  if (n == null || isNaN(n)) return "—";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return (v >= 100 || i === 0 ? v.toFixed(0) : v.toFixed(1)) + " " + units[i];
}

// Local timestamp, short.
export function localTime(tsSec) {
  if (!tsSec) return "—";
  const d = new Date(tsSec * 1000);
  return d.toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

export function relTime(tsSec) {
  if (!tsSec) return "never";
  const diff = Date.now() / 1000 - tsSec;
  if (diff < 0) return "just now";
  if (diff < 60) return Math.floor(diff) + "s ago";
  if (diff < 3600) return Math.floor(diff / 60) + "m ago";
  if (diff < 86400) return Math.floor(diff / 3600) + "h ago";
  return Math.floor(diff / 86400) + "d ago";
}

// Countdown to a future ISO/epoch: "resets in 2h 14m".
export function countdown(target) {
  if (!target) return "—";
  const t = typeof target === "number" ? target * 1000 : Date.parse(target);
  if (isNaN(t)) return "—";
  let diff = Math.floor((t - Date.now()) / 1000);
  if (diff <= 0) return "resetting…";
  const d = Math.floor(diff / 86400);
  diff %= 86400;
  const h = Math.floor(diff / 3600);
  diff %= 3600;
  const m = Math.floor(diff / 60);
  if (d > 0) return `resets in ${d}d ${h}h`;
  if (h > 0) return `resets in ${h}h ${m}m`;
  if (m > 0) return `resets in ${m}m`;
  return "resets in <1m";
}

// Format a bucket ts for an axis given the total span (seconds).
export function axisTime(tsSec, spanSec) {
  const d = new Date(tsSec * 1000);
  if (spanSec <= 6 * 3600) {
    return d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
  }
  if (spanSec <= 26 * 3600) {
    return d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
  }
  return d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
}
