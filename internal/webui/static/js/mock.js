// Dev mock: intercepts fetch for /api/* and returns realistic sample data.
// Activated only when the page URL contains ?mock=1. No effect in production.

import { API_BASE } from "./api.js";

// `provider` is omitted for Anthropic rows (the API always sends it, but
// defaulting here keeps the fixtures readable). The GLM entry exercises the
// API-key rendering path: no usage meters, no expiry, no refresh action.
const CREDS = [
  { id: "cred_ax91", label: "max-personal", type: "max", weight: 5, status: "active" },
  { id: "cred_bt42", label: "team-eu", type: "team", weight: 5, status: "active" },
  { id: "cred_ck88", label: "pro-backup", type: "pro", weight: 1, status: "limited" },
  { id: "cred_dz17", label: "enterprise-01", type: "enterprise", weight: 5, status: "active" },
  { id: "cred_er05", label: "pro-old", type: "pro", weight: 1, status: "disabled" },
  { id: "cred_gl01", label: "zai-main", type: "pro", weight: 1, status: "active", provider: "glm" },
];

// Mirrors internal/provider: only Anthropic publishes a utilization API.
const providerOf = (c) => c.provider || "anthropic";
const hasUsageAPI = (c) => providerOf(c) === "anthropic";
// full_capture and the usage limit are mutated in place by
// POST /users/{id}/capture and POST /users/{id}/limit, so both toggles stay
// stateful for the whole mock session.
// `usage_output_tokens` is fixed traffic; the limit is what the operator edits,
// so usage_pct/blocked are always derived — editing a limit visibly moves the
// meter. The four users cover every limit state: healthy, near-limit,
// unlimited, blocked.
const USERS = [
  { id: "utok_alice", name: "alice", status: "active", full_capture: true, block_suggestions: false,
    limit_output_tokens: 1_000_000, limit_window_seconds: 86400, usage_output_tokens: 302_400 },  // ~30%
  { id: "utok_bob", name: "bob", status: "active", full_capture: false, block_suggestions: true,
    limit_output_tokens: 400_000, limit_window_seconds: 21600, usage_output_tokens: 344_900 },    // ~86%
  { id: "utok_carol", name: "carol", status: "disabled", full_capture: false, block_suggestions: false,
    limit_output_tokens: 0, limit_window_seconds: 0, usage_output_tokens: 0 },                     // unlimited
  { id: "utok_ci", name: "ci-runner", status: "active", full_capture: false, block_suggestions: true,
    limit_output_tokens: 100_000, limit_window_seconds: 3600, usage_output_tokens: 122_400 },     // ~122%, blocked
];

// Derived limit fields, mirroring the backend contract: usage/pct/blocked are 0
// or null when unlimited, and blocked_until is non-null ONLY while blocked
// (rolling windows have no reset instant to report otherwise).
function limitFields(u) {
  const active = u.limit_output_tokens > 0 && u.limit_window_seconds > 0;
  if (!active) {
    return { limit_output_tokens: 0, limit_window_seconds: 0, usage_output_tokens: 0, usage_pct: 0, blocked: false, blocked_until: null };
  }
  const pct = (u.usage_output_tokens / u.limit_output_tokens) * 100;
  const blocked = pct >= 100;
  return {
    limit_output_tokens: u.limit_output_tokens,
    limit_window_seconds: u.limit_window_seconds,
    usage_output_tokens: u.usage_output_tokens,
    usage_pct: Math.round(pct * 10) / 10,
    blocked,
    // ~22 minutes out: near-future, so the HH:MM render is easy to eyeball.
    blocked_until: blocked ? new Date((now + 1320) * 1000).toISOString() : null,
  };
}

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
    // Trailing 0s are the GLM key: providers with no usage API report no
    // percentages at all, and the frontend hides the meters for them.
    const fives = [72, 41, 100, 18, 0, 0];
    const sevens = [58, 63, 92, 22, 0, 0];
    const sonnets = [44, 51, 78, 15, 0, 0];
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
    // Share is totalled per provider, matching the backend: the pool filters by
    // provider before scoring, so a GLM key only competes with other GLM keys.
    const sums = {};
    for (const r of rows) sums[providerOf(r.c)] = (sums[providerOf(r.c)] || 0) + r.score;
    return rows.map(({ c, i, five, seven, sonnet, room5, room7, saturated, score }) => ({
      credential_id: c.id, label: c.label, subscription_type: c.type, status: c.status, weight: c.weight,
      provider: providerOf(c), has_usage_api: hasUsageAPI(c),
      five_hour: { pct: five, resets_at: now + (3600 * (1 + i)) },
      seven_day: { pct: seven, resets_at: now + (86400 * (2 + i)) },
      seven_day_sonnet: { pct: sonnet, resets_at: now + (86400 * (2 + i)) },
      // No usage API ⇒ no snapshot was ever recorded.
      captured_at: hasUsageAPI(c) ? now - 300 - i * 90 : null,
      selection: {
        room_5h: room5, room_7d: room7, score,
        share_pct: score > 0 ? (score / (sums[providerOf(c)] || 1)) * 100 : 0,
        saturated,
      },
    }));
  },
  "/usage/history": (q) => {
    // Aligned grid: shared buckets, one value per bucket per series, null gaps.
    const n = 48;
    const b = buckets(q, n);
    // Only credentials whose provider has a usage API ever produce history
    // rows, so a GLM key has no series at all — not a flat or zeroed one.
    const active = CREDS.filter((c) => c.status !== "disabled" && hasUsageAPI(c));
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
      provider: providerOf(c), has_usage_api: hasUsageAPI(c),
      request_count: [8200, 5400, 1200, 3600, 40, 970][i], last_request_at: c.status === "disabled" ? null : now - 60 * (i + 1),
      expires_at: now + 3600 * (5 - i), created_at: now - 86400 * (30 - i * 4),
    })),
  "/users": () =>
    USERS.map((u, i) => ({
      id: u.id, name: u.name, status: u.status, full_capture: u.full_capture,
      block_suggestions: u.block_suggestions,
      ...limitFields(u),
      created_at: now - 86400 * (20 - i * 3),
      last_used_at: u.status === "disabled" ? now - 86400 * 4 : now - 120 * (i + 1),
    })),
  "/conversations": () =>
    Array.from({ length: 12 }, (_, i) => ({
      key: "conv_" + (1000 + i), credential_id: CREDS[i % 3].id, credential_label: CREDS[i % 3].label,
      last_seen: now - 60 * i, requests: 3 + (i % 7),
    })),
};

// ---------- v3: prompts, conversations, messages ----------

const MODELS = ["claude-opus-4-8", "claude-sonnet-4-5", "claude-haiku-4-5"];

const PROMPT_SAMPLES = [
  "Refactor the pool selection to prefer the least-saturated credential.",
  "Why does my SSE stream cut off after ~30s behind the proxy?",
  "Summarize the diff in internal/webui/static and flag any contract drift.",
  "Write a table-driven test for winParams covering custom windows.",
  "Explain the 4-priority conversation key derivation with an example.\nInclude the fallback hashing case.",
  "<script>alert('xss')</script> — make sure this renders as literal text.",
  "Here is the config I pasted:\n\n    HOST_BIND=127.0.0.1\n    HOST_PORT=8787\n    PROMPT_RETENTION_DAYS=7\n\nAnything unsafe?",
  "Trace what happens on a 429 from api.anthropic.com, step by step.",
];

const USER_TURNS = [
  "Refactor the pool selection so a saturated credential is never picked for a new conversation.",
  "Here's the failing test output:\n\n```\n--- FAIL: TestBind/rebinds_off_saturated (0.00s)\n    pool_test.go:142: got cred_ck88, want cred_ax91\n```\n\nWhat's wrong?",
  "<script>alert('xss')</script> and <img src=x onerror=alert(1)> — both of these must render as literal text, never execute.",
  "Explain the 4-priority conversation key derivation with a worked example.",
  "Paste from the terminal — mind the entities: & < > \" ' and a stray ``` fence.",
  "Can you show the migration SQL for the new conversation_message table?",
];

const ASSISTANT_TURNS = [
  "The selection score mirrors the pool exactly:\n\n```go\nfunc Score(weight, room5h, room7d float64) float64 {\n\treturn weight * room5h * math.Pow(room7d, SevenDayExp)\n}\n```\n\nThe 5h and 7d windows are **independent ceilings** — a request 429s on whichever it hits first — so their remaining room is multiplied, not averaged.",
  "Short answer: the binding is sticky, but no longer unconditionally.\n\n1. `Bind()` looks up the conversation row\n2. If the pinned credential's latest snapshot is ≥100% on either window, it re-picks\n3. Otherwise it returns the existing pin unchanged\n\nThat's why the test sees `cred_ck88`: its snapshot is at 100%, so it should have been excluded before scoring.",
  "That string is stored verbatim and rendered with `textContent`, so the browser shows it as text instead of parsing it as HTML. Nothing executes.",
  "Priority order:\n\n| # | Source |\n|---|---|\n| 1 | `X-Router-Conversation-ID` header |\n| 2 | `$.metadata.user_id` in the body |\n| 3 | `SHA256(system_prompt + first_user_message)` |\n| 4 | `SHA256(remote_addr + body[:4096])` |\n\nThe first one that yields a non-empty value wins.",
  "Here it is — additive, so it rides the existing swallow-duplicate ALTER mechanism:\n\n```sql\nCREATE TABLE IF NOT EXISTS conversation_message (\n  id INTEGER PRIMARY KEY AUTOINCREMENT,\n  conv_id TEXT NOT NULL,\n  seq INTEGER NOT NULL,\n  role TEXT NOT NULL,\n  content TEXT NOT NULL DEFAULT '',\n  UNIQUE(conv_id, seq)\n);\n```",
];

const userById = (id) => USERS.find((u) => u.id === id) || null;

// Per-user volume: alice has enough prompts to page through several times.
const PROMPT_TOTALS = { utok_alice: 63, utok_bob: 12, utok_carol: 0, utok_ci: 4 };
// Conversation counts; alice has full capture on, so hers are "full".
const CONV_TOTALS = { utok_alice: 7, utok_bob: 3, utok_carol: 0, utok_ci: 1 };
// Message counts per conversation index (alice's first is the long multi-turn one).
const CONV_SIZES = [34, 8, 4, 6, 2, 10, 4, 6];

function convIdFor(uid, i) {
  return `conv_${uid.replace(/^utok_/, "")}${String(i).padStart(2, "0")}9c1b`;
}
// Parse a conversation id back to its owner + index so the message route can
// regenerate exactly the same content the list advertised.
function parseConvId(cid) {
  const m = String(cid).match(/^conv_([a-z-]+)(\d\d)9c1b$/);
  if (!m) return null;
  const u = USERS.find((x) => x.id.replace(/^utok_/, "") === m[1]);
  if (!u) return null;
  const i = parseInt(m[2], 10);
  if (i >= (CONV_TOTALS[u.id] || 0)) return null;
  return { user: u, index: i };
}

// A conversation is "full" only when both sides were captured at the time it
// ran. Frozen at load: flipping the toggle now can't rewrite stored history.
const CAPTURED_FULL = Object.fromEntries(USERS.map((u) => [u.id, u.full_capture]));
function convSource(u, i) {
  if (!CAPTURED_FULL[u.id]) return "prompts";
  return i === CONV_TOTALS[u.id] - 1 ? "prompts" : "full"; // oldest predates the flag
}

function convMeta(u, i) {
  const size = CONV_SIZES[i % CONV_SIZES.length];
  const source = convSource(u, i);
  const last = now - i * 5400 - 300;
  return {
    conv_id: convIdFor(u.id, i),
    first_ts: last - size * 95,
    last_ts: last,
    messages: source === "full" ? size : 0,
    prompts: source === "full" ? Math.ceil(size / 2) : Math.max(1, Math.round(size / 2)),
    model: MODELS[i % MODELS.length],
    source,
  };
}

function promptRow(u, i) {
  const nConv = CONV_TOTALS[u.id] || 1;
  return {
    ts: now - i * 640 - Math.round(rand(i + 1) * 300),
    conv_id: convIdFor(u.id, i % nConv),
    model: MODELS[i % MODELS.length],
    prompt: PROMPT_SAMPLES[i % PROMPT_SAMPLES.length],
  };
}

function messageRow(u, ci, seq) {
  const meta = convMeta(u, ci);
  const isUser = seq % 2 === 0;
  const pool = isUser ? USER_TURNS : ASSISTANT_TURNS;
  const idx = Math.floor(seq / 2) % pool.length;
  return {
    seq,
    role: isUser ? "user" : "assistant",
    content: pool[idx],
    model: isUser ? "" : meta.model,
    ts: meta.first_ts + seq * 95,
  };
}

// Slice a generated collection into the {items,total,limit,offset,has_more}
// envelope the v3 endpoints return.
function envelope(total, q, defLimit, make, extra = {}) {
  const limit = Math.min(Math.max(parseInt(q.get("limit"), 10) || defLimit, 1), 500);
  const offset = Math.max(parseInt(q.get("offset"), 10) || 0, 0);
  const end = Math.min(offset + limit, total);
  const items = [];
  for (let i = offset; i < end; i++) items.push(make(i));
  return { items, total, limit, offset, has_more: end < total, ...extra };
}

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
    // Adding a key appends to CREDS so the credentials table reflects it for
    // the rest of the mock session, matching how the real endpoint behaves.
    if (path === "/credentials/keys" && method === "POST") {
      const b = JSON.parse(init.body || "{}");
      if (!b.api_key) return json({ error: "API key is empty" }, 400);
      if (!b.endpoint) return json({ error: "endpoint is required" }, 400);
      const c = {
        id: "cred_" + Math.random().toString(36).slice(2, 6),
        label: b.label || b.provider || "glm",
        type: b.plan || "", weight: b.weight || 1,
        status: "active", provider: b.provider || "glm",
      };
      CREDS.push(c);
      return json({ ok: true, id: c.id, label: c.label, provider: c.provider, weight: c.weight });
    }
    if (path.endsWith("/rotate")) return json({ token: "cpu_" + Math.random().toString(36).slice(2) + Math.random().toString(36).slice(2) });
    // Per-user capture mode — stateful for the session.
    const cm = path.match(/^\/users\/([^/]+)\/capture$/);
    if (cm && method === "POST") {
      const u = userById(decodeURIComponent(cm[1]));
      if (!u) return json({ error: "unknown user" }, 404);
      // carol is the failure fixture: exercises the revert-on-error path.
      if (u.id === "utok_carol") return json({ error: "user is disabled — enable it before changing capture mode" }, 409);
      u.full_capture = !!JSON.parse(init.body || "{}").full;
      return json({ ok: true, full_capture: u.full_capture });
    }

    // Per-user prompt-suggestion handling — stateful for the session.
    const sm = path.match(/^\/users\/([^/]+)\/suggestions$/);
    if (sm && method === "POST") {
      const u = userById(decodeURIComponent(sm[1]));
      if (!u) return json({ error: "unknown user" }, 404);
      u.block_suggestions = !!JSON.parse(init.body || "{}").block;
      return json({ ok: true, block_suggestions: u.block_suggestions });
    }

    // Per-user usage limit — stateful, and validated exactly like the backend:
    // negatives are rejected, and tokens/window must be both set or both zero.
    // (Entering 0 output tokens with a window selected is the reachable path
    // that exercises the inline 400 in the UI.)
    const lm = path.match(/^\/users\/([^/]+)\/limit$/);
    if (lm && method === "POST") {
      const u = userById(decodeURIComponent(lm[1]));
      if (!u) return json({ error: "unknown user" }, 404);
      const b = JSON.parse(init.body || "{}");
      const out = Math.trunc(Number(b.output_tokens) || 0);
      const win = Math.trunc(Number(b.window_seconds) || 0);
      if (out < 0 || win < 0) return json({ error: "output_tokens and window_seconds must not be negative" }, 400);
      if ((out === 0) !== (win === 0)) {
        return json(
          { error: "both output_tokens and window_seconds are required to set a limit; send both as 0 to clear it" },
          400
        );
      }
      u.limit_output_tokens = out;
      u.limit_window_seconds = win;
      return json({ ok: true, limit_output_tokens: out, limit_window_seconds: win });
    }
    return json({ ok: true });
  }

  // Mirror the backend's 400 on invalid custom windows (from>=to or span >90d).
  if (u.searchParams.has("from") && u.searchParams.has("to")) {
    const w = resolveWindow(u.searchParams);
    if (!w.valid) return json({ error: "invalid range: from must be before to and span ≤ 90 days" }, 400);
  }

  // Dynamic route: per-user prompts, paginated over ALL stored rows.
  const pm = path.match(/^\/users\/([^/]+)\/prompts$/);
  if (pm) {
    const usr = userById(decodeURIComponent(pm[1]));
    if (!usr) return json({ error: "unknown user" }, 404);
    // carol has no traffic → exercises the empty state.
    return json(envelope(PROMPT_TOTALS[usr.id] || 0, u.searchParams, 50, (i) => promptRow(usr, i)));
  }

  // Dynamic route: output tokens over an arbitrary rolling window, used by the
  // Edit limit modal. Scaled from the fixture's own window so switching the
  // preset visibly changes the number.
  const um = path.match(/^\/users\/([^/]+)\/usage$/);
  if (um) {
    const usr = userById(decodeURIComponent(um[1]));
    if (!usr) return json({ error: "unknown user" }, 404);
    const secs = Math.trunc(Number(u.searchParams.get("window_seconds")) || 0);
    if (secs <= 0) return json({ error: "window_seconds must be a positive integer" }, 400);
    const baseWin = usr.limit_window_seconds || 86400;
    const baseUse = usr.usage_output_tokens || Math.round(baseWin * 4.2);
    // Sub-linear with window length: traffic is bursty, not uniform.
    const scaled = Math.round(baseUse * Math.pow(secs / baseWin, 0.7));
    return json({ id: usr.id, window_seconds: secs, output_tokens: scaled });
  }

  // Dynamic route: per-user conversations, newest last_ts first.
  const cvm = path.match(/^\/users\/([^/]+)\/conversations$/);
  if (cvm) {
    const usr = userById(decodeURIComponent(cvm[1]));
    if (!usr) return json({ error: "unknown user" }, 404);
    return json(envelope(CONV_TOTALS[usr.id] || 0, u.searchParams, 25, (i) => convMeta(usr, i)));
  }

  // Dynamic route: conversation messages, ascending seq. Falls back to
  // prompt_log rows rendered as user turns when nothing full was captured.
  const mm = path.match(/^\/conversations\/([^/]+)\/messages$/);
  if (mm) {
    const ref = parseConvId(decodeURIComponent(mm[1]));
    if (!ref) return json({ error: "unknown conversation" }, 404);
    const meta = convMeta(ref.user, ref.index);
    const extra = {
      source: meta.source,
      conv_id: meta.conv_id,
      user: { id: ref.user.id, name: ref.user.name },
    };
    if (meta.source !== "full") {
      // Prompts-only fallback: user turns synthesized from prompt_log.
      return json(envelope(meta.prompts, u.searchParams, 20, (i) => ({
        seq: i,
        role: "user",
        content: PROMPT_SAMPLES[(ref.index + i) % PROMPT_SAMPLES.length],
        model: meta.model,
        ts: meta.first_ts + i * 190,
      }), extra));
    }
    return json(envelope(meta.messages, u.searchParams, 20, (i) => messageRow(ref.user, ref.index, i), extra));
  }

  const handler = DB[path];
  if (handler) return json(handler(u.searchParams));
  return json({ error: "mock: no route for " + path }, 404);
};

console.info("[claude-proxy] mock mode active — /api/* served from sample data");
