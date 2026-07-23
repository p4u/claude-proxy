// Dev mock: intercepts fetch for /ui/api/* and returns realistic sample data.
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

function buckets(period, n) {
  const span = periodSpan(period);
  const step = span / n;
  const start = now - span;
  return Array.from({ length: n }, (_, i) => Math.round(start + step * (i + 1)));
}

function wave(n, base, amp, seed, floor = 0) {
  return Array.from({ length: n }, (_, i) => {
    const v = base + amp * (0.5 * Math.sin(i / 5 + seed) + 0.5 * rand(seed + i));
    return Math.max(floor, Math.round(v));
  });
}

function groupSeries(period, group, kind) {
  const n = 60;
  const b = buckets(period, n);
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

const DB = {
  "/overview": () => ({
    requests_24h: 18432,
    tokens_24h: { input: 42_800_000, output: 9_650_000, cache_read: 128_400_000, cache_creation: 3_200_000 },
    active_conversations: 47,
    credentials: { total: 5, active: 3, limited: 1, errored: 0 },
    users_total: 4,
    avg_latency_ms_24h: 842,
    error_rate_24h: 0.021,
  }),
  "/stats/requests": (q) => {
    const g = q.get("group_by") || "user";
    return groupSeries(q.get("period") || "24h", g);
  },
  "/stats/tokens": (q) => {
    const g = q.get("group_by") || "user";
    return groupSeries(q.get("period") || "24h", g);
  },
  "/stats/latency": (q) => {
    const n = 60;
    const b = buckets(q.get("period") || "24h", n);
    const avg = wave(n, 700, 500, 3, 120);
    return { buckets: b, avg_ms: avg, p95_ms: avg.map((v) => Math.round(v * 1.9 + 200)) };
  },
  "/stats/users": () =>
    USERS.map((u, i) => ({
      id: u.id, name: u.name, requests: [8200, 5400, 1200, 3600][i], ok: [8050, 5310, 1180, 3550][i],
      errors: [150, 90, 20, 50][i], tokens_in: [19_000_000, 12_000_000, 2_400_000, 8_900_000][i],
      tokens_out: [4_200_000, 2_800_000, 600_000, 1_900_000][i], cache_read: [40e6, 26e6, 5e6, 18e6][i],
      cache_creation: [1.1e6, 0.7e6, 0.2e6, 0.5e6][i], bytes_sent: [22e6, 14e6, 3e6, 9e6][i],
      bytes_received: [180e6, 120e6, 22e6, 78e6][i], avg_latency_ms: [812, 903, 640, 1120][i],
      conversations: [12, 9, 3, 7][i],
    })),
  "/usage/current": () =>
    CREDS.map((c, i) => {
      const five = [72, 41, 100, 18, 0][i];
      const seven = [58, 63, 92, 22, 0][i];
      const sonnet = [44, 51, 78, 15, 0][i];
      return {
        credential_id: c.id, label: c.label, subscription_type: c.type, status: c.status, weight: c.weight,
        five_hour: { pct: five, resets_at: now + (3600 * (1 + i)) },
        seven_day: { pct: seven, resets_at: now + (86400 * (2 + i)) },
        seven_day_sonnet: { pct: sonnet, resets_at: now + (86400 * (2 + i)) },
        captured_at: now - 300 - i * 90,
      };
    }),
  "/usage/history": (q) => {
    const n = 48;
    const b = buckets(q.get("period") || "24h", n);
    const series = CREDS.filter((c) => c.status !== "disabled").map((c, i) => ({
      credential_id: c.id, label: c.label,
      points: b.map((ts, j) => ({
        ts,
        five_hour_pct: Math.min(100, wave(n, 30 + i * 12, 55, i + 2)[j]),
        seven_day_pct: Math.min(100, wave(n, 25 + i * 10, 45, i + 5)[j]),
        seven_day_sonnet_pct: Math.min(100, wave(n, 18 + i * 8, 40, i + 8)[j]),
      })),
    }));
    return { series };
  },
  "/credentials": () =>
    CREDS.map((c, i) => ({
      id: c.id, label: c.label, subscription_type: c.type, status: c.status, weight: c.weight,
      request_count: [8200, 5400, 1200, 3600, 40][i], last_used_at: c.status === "disabled" ? null : now - 60 * (i + 1),
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

  const handler = DB[path];
  if (handler) return json(handler(u.searchParams));
  return json({ error: "mock: no route for " + path }, 404);
};

console.info("[claude-proxy] mock mode active — /ui/api/* served from sample data");
