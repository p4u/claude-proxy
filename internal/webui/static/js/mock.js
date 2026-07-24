// Dev mock: intercepts fetch for /api/* and returns realistic sample data.
// Activated only when the page URL contains ?mock=1. No effect in production.

import { API_BASE } from "./api.js";

const CREDS = [
  { id: "cred_ax91", label: "max-personal", type: "max", weight: 5, status: "active" },
  { id: "cred_bt42", label: "team-eu", type: "team", weight: 5, status: "active" },
  { id: "cred_ck88", label: "pro-backup", type: "pro", weight: 1, status: "limited" },
  { id: "cred_dz17", label: "enterprise-01", type: "enterprise", weight: 5, status: "active" },
  { id: "cred_er05", label: "pro-old", type: "pro", weight: 1, status: "disabled" },
];
const USERS = [
  { id: "utok_alice", name: "alice", status: "active" },
  { id: "utok_bob", name: "bob", status: "active" },
  { id: "utok_carol", name: "carol", status: "disabled" },
  { id: "utok_ci", name: "ci-runner", status: "active" },
];

const now = Math.floor(Date.now() / 1000);
const rand = (seed) => {
  let x = Math.sin(seed) * 10000;
  return x - Math.floor(x);
};

function periodSpan(p) {
  return { "1h": 3600, "6h": 21600, "24h": 86400, "7d": 604800, "30d": 2592000 }[p] || 86400;
}

// Resolve the requested window from query params: a custom from/to range (unix
// seconds) overrides the preset period, mirroring the real backend contract.
function resolveWindow(q) {
  const from = q.get("from");
  const to = q.get("to");
  if (from != null && to != null) {
    const f = Number(from);
    const t = Number(to);
    const valid = !(isNaN(f) || isNaN(t) || f >= t || t - f > 90 * 86400);
    return { start: f, end: t, span: Math.max(1, t - f), custom: true, valid };
  }
  const span = periodSpan(q.get("period") || "24h");
  return { start: now - span, end: now, span, custom: false, valid: true };
}

function buckets(q, n) {
  const w = resolveWindow(q);
  const step = w.span / n;
  return Array.from({ length: n }, (_, i) => Math.round(w.start + step * (i + 1)));
}

function wave(n, base, amp, seed, floor = 0) {
  return Array.from({ length: n }, (_, i) => {
    const v = base + amp * (0.5 * Math.sin(i / 5 + seed) + 0.5 * rand(seed + i));
    return Math.max(floor, Math.round(v));
  });
}

function groupSeries(q, group) {
  const n = 60;
  const b = buckets(q, n);
  const src = group === "user" ? USERS.filter((u) => u.status === "active") : CREDS.filter((c) => c.status === "active");
  const series = src.map((s, i) => {
    const reqs = wave(n, 20 - i * 3, 40, i + 1, 0);
    const tin = reqs.map((r) => r * (900 + Math.round(rand(i + r) * 400)));
    const tout = reqs.map((r) => r * (300 + Math.round(rand(i + r + 9) * 200)));
    return {
      id: s.id,
      label: s.label || s.name,
      requests: reqs,
      errors: reqs.map((r) => (rand(r + i) > 0.9 ? Math.round(r * 0.1) : 0)),
      tokens_in: tin,
      tokens_out: tout,
    };
  });
  return { buckets: b, series };
}

// Aggregate (no grouping) token/request series over the window.
function totalsSeries(q) {
  const n = 60;
  const b = buckets(q, n);
  const requests = wave(n, 60, 90, 7, 0);
  return {
    buckets: b,
    requests,
    errors: requests.map((r, i) => (rand(r + i) > 0.85 ? Math.round(r * 0.08) : 0)),
    tokens: {
      input: requests.map((r) => r * (900 + Math.round(rand(r) * 400))),
      output: requests.map((r) => r * (280 + Math.round(rand(r + 3) * 180))),
      cache_read: requests.map((r) => r * (2600 + Math.round(rand(r + 5) * 1400))),
      cache_creation: requests.map((r) => r * (90 + Math.round(rand(r + 8) * 120))),
    },
  };
}

const DB = {
  "/overview": (q) => {
    // Follows the selected window: scale totals vs a 24h baseline.
    const scale = Math.max(0.1, resolveWindow(q).span / 86400);
    const k = (v) => Math.round(v * scale);
    return {
      requests: k(18432),
      tokens: {
        input: k(42_800_000), output: k(9_650_000),
        cache_read: k(128_400_000), cache_creation: k(3_200_000),
      },
      active_conversations: 47,
      credentials: { total: 5, active: 3, limited: 1, errored: 0 },
      users_total: 4,
      avg_latency_ms: 842,
      error_rate: 0.021,
    };
  },
  "/stats/requests": (q) => groupSeries(q, q.get("group_by") || "user"),
  "/stats/tokens": (q) => groupSeries(q, q.get("group_by") || "user"),
  "/stats/totals": (q) => totalsSeries(q),
  "/stats/latency": (q) => {
    const n = 60;
    const b = buckets(q, n);
    const avg = wave(n, 700, 500, 3, 120);
    return { buckets: b, avg_ms: avg, p95_ms: avg.map((v) => Math.round(v * 1.9 + 200)) };
  },
  "/stats/users": (q) => {
    // Scale scalar totals by the selected window vs a 24h baseline so custom
    // ranges and presets visibly move the numbers.
    const scale = Math.max(0.15, resolveWindow(q).span / 86400);
    const k = (v) => Math.round(v * scale);
    return USERS.map((u, i) => ({
      id: u.id, name: u.name, requests: k([8200, 5400, 1200, 3600][i]), ok: k([8050, 5310, 1180, 3550][i]),
      errors: k([150, 90, 20, 50][i]), tokens_in: k([19_000_000, 12_000_000, 2_400_000, 8_900_000][i]),
      tokens_out: k([4_200_000, 2_800_000, 600_000, 1_900_000][i]), cache_read: k([40e6, 26e6, 5e6, 18e6][i]),
      cache_creation: k([1.1e6, 0.7e6, 0.2e6, 0.5e6][i]), bytes_sent: k([22e6, 14e6, 3e6, 9e6][i]),
      bytes_received: k([180e6, 120e6, 22e6, 78e6][i]), avg_latency_ms: [812, 903, 640, 1120][i],
      conversations: [12, 9, 3, 7][i],
    }));
  },
  "/usage/current": () => {
    const fives = [72, 41, 100, 18, 0];
    const sevens = [58, 63, 92, 22, 0];
    const sonnets = [44, 51, 78, 15, 0];
    // Score mirrors the pool: weight × room_5h × room_7d^1.5. Disabled creds and
    // saturated snapshots (≥100% on either window) are excluded from the share.
    const rows = CREDS.map((c, i) => {
      const five = fives[i], seven = sevens[i];
      const room5 = Math.max(0, 1 - five / 100);
      const room7 = Math.max(0, 1 - seven / 100);
      const saturated = five >= 100 || seven >= 100;
      const active = c.status !== "disabled" && !saturated;
      const score = active ? c.weight * room5 * Math.pow(room7, 1.5) : 0;
      return { c, i, five, seven, sonnet: sonnets[i], room5, room7, saturated, score };
    });
    const sum = rows.reduce((a, r) => a + r.score, 0) || 1;
    return rows.map(({ c, i, five, seven, sonnet, room5, room7, saturated, score }) => ({
      credential_id: c.id, label: c.label, subscription_type: c.type, status: c.status, weight: c.weight,
      five_hour: { pct: five, resets_at: now + (3600 * (1 + i)) },
      seven_day: { pct: seven, resets_at: now + (86400 * (2 + i)) },
      seven_day_sonnet: { pct: sonnet, resets_at: now + (86400 * (2 + i)) },
      captured_at: now - 300 - i * 90,
      selection: {
        room_5h: room5, room_7d: room7, score,
        share_pct: score > 0 ? (score / sum) * 100 : 0,
        saturated,
      },
    }));
  },
  "/usage/history": (q) => {
    // Aligned grid: shared buckets, one value per bucket per series, null gaps.
    const n = 48;
    const b = buckets(q, n);
    const active = CREDS.filter((c) => c.status !== "disabled");
    const series = active.map((c, i) => {
      const five = wave(n, 30 + i * 12, 55, i + 2).map((v) => Math.min(100, v));
      const seven = wave(n, 25 + i * 10, 45, i + 5).map((v) => Math.min(100, v));
      const sonnet = wave(n, 18 + i * 8, 40, i + 8).map((v) => Math.min(100, v));
      // Simulate missing snapshots (a fresh import mid-window) as nulls.
      const nulls = (arr) => arr.map((v, j) => (i === active.length - 1 && j < n / 3 ? null : v));
      return {
        credential_id: c.id, label: c.label,
        five_hour_pct: nulls(five),
        seven_day_pct: nulls(seven),
        seven_day_sonnet_pct: nulls(sonnet),
      };
    });
    return { buckets: b, series };
  },
  "/stats/selection": (q) => {
    const n = 60;
    const b = buckets(q, n);
    const active = CREDS.filter((c) => c.status !== "disabled" && c.status !== "limited");
    const series = active.map((c, i) => ({
      credential_id: c.id, label: c.label,
      picks: wave(n, 6 - i, 10, i + 4, 0),
    }));
    const totalPicks = series.map((s) => s.picks.reduce((a, v) => a + v, 0));
    const grand = totalPicks.reduce((a, v) => a + v, 0) || 1;
    const totals = active.map((c, i) => ({
      credential_id: c.id, label: c.label, picks: totalPicks[i],
      share_pct: (totalPicks[i] / grand) * 100,
    }));
    return { buckets: b, series, totals };
  },
  "/credentials": () =>
    CREDS.map((c, i) => ({
      id: c.id, label: c.label, subscription_type: c.type, status: c.status, weight: c.weight,
      request_count: [8200, 5400, 1200, 3600, 40][i], last_request_at: c.status === "disabled" ? null : now - 60 * (i + 1),
      expires_at: now + 3600 * (5 - i), created_at: now - 86400 * (30 - i * 4),
    })),
  "/users": () =>
    USERS.map((u, i) => ({
      id: u.id, name: u.name, status: u.status, created_at: now - 86400 * (20 - i * 3),
      last_used_at: u.status === "disabled" ? now - 86400 * 4 : now - 120 * (i + 1),
    })),
  "/conversations": () =>
    Array.from({ length: 12 }, (_, i) => ({
      key: "conv_" + (1000 + i), credential_id: CREDS[i % 3].id, credential_label: CREDS[i % 3].label,
      last_seen: now - 60 * i, requests: 3 + (i % 7),
    })),
};

function json(body, status = 200) {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

const realFetch = window.fetch.bind(window);
window.fetch = async (input, init = {}) => {
  const url = typeof input === "string" ? input : input.url;
  if (!url.includes(API_BASE)) return realFetch(input, init);
  const u = new URL(url, location.origin);
  const path = u.pathname.replace(API_BASE, "");
  const method = (init.method || "GET").toUpperCase();

  await new Promise((r) => setTimeout(r, 120 + Math.random() * 220));

  const anon = new URLSearchParams(location.search).get("anon") === "1";
  if (path === "/session") return json({ authenticated: !anon });
  if (path === "/login") return json({ authenticated: true });
  if (path === "/logout") return json({ authenticated: false });

  // Mutations: acknowledge.
  if (method !== "GET") {
    if (path === "/users" && method === "POST") return json({ id: "utok_new", name: JSON.parse(init.body || "{}").name || "new", token: "cpu_" + Math.random().toString(36).slice(2) + Math.random().toString(36).slice(2) });
    if (path.endsWith("/rotate")) return json({ token: "cpu_" + Math.random().toString(36).slice(2) + Math.random().toString(36).slice(2) });
    return json({ ok: true });
  }

  // Mirror the backend's 400 on invalid custom windows (from>=to or span >90d).
  if (u.searchParams.has("from") && u.searchParams.has("to")) {
    const w = resolveWindow(u.searchParams);
    if (!w.valid) return json({ error: "invalid range: from must be before to and span ≤ 90 days" }, 400);
  }

  // Dynamic route: per-user recent prompts.
  const pm = path.match(/^\/users\/([^/]+)\/prompts$/);
  if (pm) {
    const limit = Math.min(parseInt(u.searchParams.get("limit"), 10) || 50, 200);
    // carol is disabled with no traffic → exercise the empty state.
    if (pm[1] === "utok_carol") return json([]);
    const models = ["claude-opus-4-8", "claude-sonnet-4-5", "claude-haiku-4-5"];
    const samples = [
      "Refactor the pool selection to prefer the least-saturated credential.",
      "Why does my SSE stream cut off after ~30s behind the proxy?",
      "Summarize the diff in internal/webui/static and flag any contract drift.",
      "Write a table-driven test for winParams covering custom windows.",
      "Explain the 4-priority conversation key derivation with an example.\nInclude the fallback hashing case.",
      "<script>alert('xss')</script> — make sure this renders as literal text.",
    ];
    const rows = Array.from({ length: Math.min(limit, 18) }, (_, i) => ({
      ts: now - i * 640 - Math.round(rand(i + 1) * 300),
      conv_id: "conv_" + (2000 + i),
      model: models[i % models.length],
      prompt: samples[i % samples.length],
    }));
    return json(rows);
  }

  const handler = DB[path];
  if (handler) return json(handler(u.searchParams));
  return json({ error: "mock: no route for " + path }, 404);
};

console.info("[claude-proxy] mock mode active — /api/* served from sample data");
