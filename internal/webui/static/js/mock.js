// Dev mock: intercepts fetch for /api/* and returns realistic sample data.
// Activated only when the page URL contains ?mock=1. No effect in production.

import { API_BASE } from "./api.js";

// `provider` is omitted for Anthropic rows (the API always sends it, but
// defaulting here keeps the fixtures readable). The GLM entry exercises the
// API-key rendering path: no usage meters, no expiry, no refresh action.
const CREDS = [
  { id: "cred_ax91", label: "max-personal", type: "max", tier: "20x", weight: 5, status: "active" },
  { id: "cred_bt42", label: "team-eu", type: "team", tier: "5x", weight: 5, status: "active" },
  { id: "cred_ck88", label: "pro-backup", type: "pro", weight: 1, status: "limited" },
  { id: "cred_dz17", label: "enterprise-01", type: "enterprise", weight: 5, status: "active" },
  { id: "cred_er05", label: "pro-old", type: "pro", weight: 1, status: "disabled" },
  { id: "cred_gl01", label: "zai-main", type: "pro", weight: 1, status: "active", provider: "glm" },
  { id: "cred_oa01", label: "local-openai", type: "", weight: 2, status: "active",
    provider: "custom_openai", endpoint: "http://localhost:8000/v1",
    models: [{ id: "local-model", display_name: "local-model" }] },
];

const claudeSessions = new Set();

const CODEX_ACCOUNTS = [
  { name: "codex-owner-pro-91c7.json", auth_index: "mock-codex-owner", email: "owner@example.com",
    account_type: "pro", status: "active", disabled: false, unavailable: false, blocked: false,
    success: 418, failed: 3, weight: 733, base_weight: 5, effective_weight: 733,
    quota: { five_hour_pct: 12, seven_day_pct: 3, plan_type: "pro", has_signals: true },
    last_refresh: new Date(Date.now() - 42 * 60 * 1000).toISOString() },
  // A "prolite" plan publishes only a weekly limit (its 5-hour window has zero
  // length), so the card must draw a single meter for it.
  { name: "codex-lite-2f0a.json", auth_index: "mock-codex-lite", email: "lite@example.com",
    account_type: "oauth", status: "active", disabled: false, unavailable: false, blocked: false,
    success: 61, failed: 0, weight: 50, base_weight: 1, effective_weight: 50,
    quota: { five_hour_pct: 0, seven_day_pct: 7, plan_type: "prolite", has_signals: true, weekly_only: true },
    last_refresh: new Date(Date.now() - 9 * 60 * 1000).toISOString() },
];
// Google Gemini (Antigravity) accounts: one quota bucket per account, carried
// in the five_hour slot; plan_type is Google's tier.
const GEMINI_ACCOUNTS = [
  { name: "antigravity-owner@gmail.com.json", auth_index: "mock-gemini-owner", email: "owner@gmail.com",
    account_type: "oauth", status: "active", disabled: false, unavailable: false, blocked: false,
    success: 37, failed: 0, weight: 1000, base_weight: 1, effective_weight: 1000,
    quota: { five_hour_pct: 18, plan_type: "free", has_signals: true, five_hour_window: true },
    last_refresh: new Date(Date.now() - 20 * 60 * 1000).toISOString() },
];
const SIDECAR_ACCOUNTS = { codex: CODEX_ACCOUNTS, gemini: GEMINI_ACCOUNTS };
const sidecarOAuthPolls = { codex: 0, gemini: 0 };

// Mirrors internal/provider: only Anthropic publishes a utilization API.
const providerOf = (c) => c.provider || "anthropic";
const MOCK_ENDPOINTS = {
  glm: { name: "Z.AI GLM", default: "https://api.z.ai/api/anthropic", endpoints: [
    { name: "global", desc: "Global (api.z.ai)", url: "https://api.z.ai/api/anthropic" },
    { name: "cn", desc: "China (open.bigmodel.cn)", url: "https://open.bigmodel.cn/api/anthropic" }]},
  mimo: { name: "Xiaomi MiMo", default: "https://token-plan-sgp.xiaomimimo.com/anthropic", endpoints: [
    { name: "sgp", desc: "Token Plan — Singapore", url: "https://token-plan-sgp.xiaomimimo.com/anthropic" },
    { name: "ams", desc: "Token Plan — Amsterdam", url: "https://token-plan-ams.xiaomimimo.com/anthropic" },
    { name: "cn", desc: "Token Plan — China", url: "https://token-plan-cn.xiaomimimo.com/anthropic" },
    { name: "payg", desc: "Pay-as-you-go", url: "https://api.xiaomimimo.com/anthropic" }]},
};
const endpointOf = (c) => {
  const p = MOCK_ENDPOINTS[providerOf(c)];
  return p ? (c.endpoint || p.default) : (c.endpoint || "");
};
const endpointNameOf = (c) => {
  const p = MOCK_ENDPOINTS[providerOf(c)];
  return p ? (p.endpoints.find((e) => e.url === endpointOf(c))?.name || "") : "";
};
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
  return { "1h": 3600, "6h": 21600, "24h": 86400, "7d": 604800, "30d": 2592000, "90d": 7776000 }[p] || 86400;
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

// ---------- Routing decision log (/routing/events) ----------
// Deterministic fixtures with cursor pagination. One "pass" is a realistic
// slice of elective-router behaviour: a live pending→switched pair and
// outcomes that ended without a move (rebalance is the only policy that
// executes live moves), plus one expiry shadow decision. Expiry is
// shadow-only and per-destination: conversation/source are empty and the
// nested evidence object describes the destination-opportunity measurement
// — never executable, with the unknowns that rule out a real proposal.
// "quota-unknown" passes omit the forecast block, "workload-unknown" passes
// omit workload_last_hour, so partial evidence renders get exercised too.
// Passes are stamped backwards in time, ids descend with them, so the
// `before` cursor (id < before) pages strictly newest-first.

const ROUTING_CONVS = ["9c1b7f3ae2d48501", "b42e0d6c91f7a3e8", "7d95c2f01e8ab463", "e0b8412c6d97f5aa"];

const ROUTING_EVENTS = (() => {
  const evs = [];
  for (let p = 0; p < 12; p++) {
    const t0 = now - 300 - p * 31300; // ~8.7h between passes → ~4.3 days of log
    const cred = (i) => ["cred_ax91", "cred_bt42", "cred_ck88", "cred_dz17"][(i + p) % 4];
    const [a, b, c, d] = [cred(0), cred(1), cred(2), cred(3)];
    const convA = ROUTING_CONVS[p % ROUTING_CONVS.length];
    const convB = ROUTING_CONVS[(p + 2) % ROUTING_CONVS.length];
    const ev = (ts, o) => evs.push({ ts, ...o });

    // Live rebalance that ran to completion: announced, then switched once
    // the source's in-flight responses drained. Reasons alternate between
    // operator sentences and backend tokens so both render paths are visible.
    ev(t0 - 2060, {
      policy: "rebalance", mode: "live", kind: "pending", conversation: convA,
      source_id: a, target_id: b, evidence: {},
      reason: p % 2 ? "destination qualifies at ≥4× effective score" : "destination-qualifies-4x",
    });
    ev(t0 - 1900, {
      policy: "rebalance", mode: "live", kind: "switched", conversation: convA,
      source_id: a, target_id: b, evidence: {},
      reason: p % 2 ? "destination revalidated after drain" : "drained-and-revalidated",
    });
    // Announced, but in-flight responses never drained in time: pin kept.
    ev(t0 - 3700, {
      policy: "rebalance", mode: "live", kind: "pending", conversation: convB,
      source_id: c, target_id: d, reason: "destination-qualifies-4x", evidence: {},
    });
    ev(t0 - 3540, {
      policy: "rebalance", mode: "live", kind: "deferred", conversation: convB,
      source_id: c, target_id: d, reason: "drain-timeout", evidence: {},
    });
    // Announced, then the destination stopped qualifying before the switch.
    ev(t0 - 5200, {
      policy: "rebalance", mode: "live", kind: "pending", conversation: convA,
      source_id: b, target_id: c, evidence: {},
      reason: "target effective score ≥ 4× source with fresh snapshots",
    });
    ev(t0 - 5000, {
      policy: "rebalance", mode: "live", kind: "cancelled", conversation: convA,
      source_id: b, target_id: c, reason: "no-longer-eligible", evidence: {},
    });
    // The announcement window lapsed with no eligible follow-up request.
    ev(t0 - 6800, {
      policy: "rebalance", mode: "live", kind: "cancelled", conversation: convB,
      source_id: a, target_id: b, reason: "notice-expired", evidence: {},
    });
    // Shadow: expiry policy — the only shadow producer. Never executes
    // moves: per-destination evaluations only (conversation/source empty),
    // each marked not executable with the unknowns that rule out a real
    // proposal. Evidence follows the nested destination-opportunity
    // contract; forecast is omitted when quota is unknown, workload when
    // workload is unknown.
    ev(t0 - 7600, {
      policy: "expiry", mode: "shadow", kind: "shadow_decision",
      conversation: "", source_id: "", target_id: d,
      reason: ["expiry-opportunity", "no-expiry-opportunity", "quota-unknown", "workload-unknown"][p % 4],
      evidence: {
        executable: false,
        confidence: p % 2 ? "heuristic" : "unknown",
        scope_knowledge: "max_only",
        measurement: "destination_opportunity",
        baseline: ["unknown", "outside_horizon", "no_need", "spare_weekly_capacity", "uncertain_spare_capacity"][p % 5],
        blockers: ["scope_eligibility_unknown", "incremental_demand_unknown", "cache_cost_unknown", "stream_success_unknown"],
        ...(p % 4 === 2
          ? {}
          : {
              forecast: {
                sample_times: [t0 - 600, t0 - 360, t0 - 120],
                sample_age_seconds: 120,
                reset_at: t0 + 5400,
                horizon_seconds: 5520,
                five_hour_pct_per_hour: { min: 2 + (p % 3), max: 5 + (p % 3) },
                weekly_pct_per_hour: { min: 0.4 + (p % 3) * 0.1, max: 0.9 + (p % 3) * 0.1 },
                unused_weekly_pct: { min: 18 + (p % 3) * 6, max: 52 + (p % 3) * 7 },
              },
            }),
        ...(p % 4 === 3
          ? {}
          : {
              workload_last_hour: {
                requests: 180 + p * 7,
                output_tokens: 42000 + p * 1100,
                cache_creation_tokens: 5200 + p * 90,
                cache_read_tokens: 640000 + p * 9000,
                previous_requests_per_hour: 140 + p * 5,
                recent_requests_per_hour: 205 + p * 6,
                request_rate_trend: p % 2 ? "rising" : "steady",
                requests_after_sample: 12 + p,
                arrivals_after_sample: 9 + p,
                pending_targets: 1 + (p % 3),
                inflight: p % 4,
                complete: true,
              },
            }),
      },
    });
  }
  evs.sort((x, y) => y.ts - x.ts);
  // Ids descend with the sort so `before` cursor paging is stable.
  return evs.map((e, i) => ({ id: 1600 - i, ...e }));
})();

function routingEventsPage(q) {
  const limit = Math.min(Math.max(parseInt(q.get("limit"), 10) || 50, 1), 200);
  const before = parseInt(q.get("before"), 10);
  const pool = Number.isFinite(before) ? ROUTING_EVENTS.filter((e) => e.id < before) : ROUTING_EVENTS;
  const items = pool.slice(0, limit);
  return {
    items,
    next_before: pool.length > limit ? items[items.length - 1].id : null,
    retention_days: 7,
  };
}

// Subscription value fixtures deliberately use synthetic metadata, not routing
// weights. Traffic reconciles across accounts, cohorts, daily rows and models.
// Gateway traffic never acquires a plan or an account-level quota estimate.
function subscriptionReport(q) {
  const query = new URLSearchParams(q);
  if (!query.has("period") && !query.has("from")) query.set("period", "30d");
  const range = resolveWindow(query);
  const five = query.get("quota_window") === "five_hour";
  const windowSeconds = five ? 18000 : 604800;
  const tokenKeys = ["input", "output", "cache_creation", "cache_read"];
  const tokens = (values = {}) => {
    const result = Object.fromEntries(tokenKeys.map((key) => [key, Math.max(0, Math.round(values[key] || 0))]));
    result.total = tokenKeys.reduce((sum, key) => sum + result[key], 0);
    return result;
  };
  const add = (rows) => tokens(Object.fromEntries(tokenKeys.map((key) => [key, rows.reduce((sum, row) => sum + row[key], 0)])));
  const scaled = (value, factor) => tokens(Object.fromEntries(tokenKeys.map((key) => [key, value[key] * factor])));
  const providerNames = { anthropic: "Anthropic", glm: "Z.AI GLM", custom_openai: "Custom OpenAI", codex: "OpenAI Codex", gemini: "Google Gemini" };
  const normalizedTier = (value) => {
    const tier = (value || "").trim().toLowerCase();
    if (!tier) return "unknown";
    if (tier === "5x" || tier.endsWith("_5x")) return "5x";
    if (tier === "20x" || tier.endsWith("_20x")) return "20x";
    return tier;
  };
  const sources = [
    ...CREDS.map((cred) => ({ id: cred.id, name: cred.label, provider: providerOf(cred), plan: cred.type.trim().toLowerCase() || "unknown", tier: normalizedTier(cred.tier), attribution: "credential" })),
    { id: "gateway_codex", name: "Codex gateway", provider: "codex", plan: "all", tier: "unknown", attribution: "gateway" },
    { id: "gateway_gemini", name: "Gemini gateway", provider: "gemini", plan: "all", tier: "unknown", attribution: "gateway" },
    { id: "cred_retired_demo", name: "Unknown or deleted credential", provider: "unknown", plan: "unknown", tier: "unknown", attribution: "unknown" },
  ];
  const day = 86400;
  const today = Math.floor(now / day) * day;
  function usage(index, from, to) {
    const rows = [];
    let requests = 0;
    for (let ts = Math.floor(from / day) * day; ts < to; ts += day) {
      const part = Math.max(0, Math.min(ts + day, to) - Math.max(ts, from)) / day;
      const age = (today - ts) / day;
      const pace = [650, 390, 68, 140, 7, 190, 45, 310, 130, 13][index] || 40;
      const r = Math.round(pace * part * (0.76 + 0.25 * Math.sin(age / 3 + index) + 0.25 * rand(ts / day + index)));
      const cache = 14000 + 9000 * (0.5 + 0.5 * Math.sin(age / 9 + index));
      requests += r;
      rows.push(tokens({ input: r * (1600 + index * 140), output: r * (520 + 180 * Math.sin(age / 6 + index)), cache_creation: r * 650, cache_read: r * cache }));
    }
    return { requests, tokens: add(rows) };
  }
  const groups = new Map();
  const accounts = [];
  const dailyMap = new Map();
  const modelMap = new Map();
  const capacity_history = [];
  sources.forEach((source, index) => {
    const key = [source.provider, source.plan, source.tier, source.attribution].map(encodeURIComponent).join("/");
    const providerLabel = providerNames[source.provider] || "Unknown provider";
    const planLabel = source.plan === "unknown" ? "Plan unknown" : source.plan[0].toUpperCase() + source.plan.slice(1);
    const label = source.attribution === "gateway" ? `${providerLabel} / All plans (gateway)` : source.attribution === "unknown" ? "Unknown or deleted credential" : `${providerLabel} / ${planLabel}` + (source.tier !== "unknown" ? ` (${source.tier})` : source.provider === "anthropic" ? " (tier unknown)" : "");
    if (!groups.has(key)) groups.set(key, { key, provider: source.provider, provider_name: providerNames[source.provider] || "Unknown provider", plan: source.plan, tier: source.tier, label, attribution: source.attribution, credentials: 0, requests: 0, tokens: tokens(), estimate: null, estimate_samples: 0, estimate_low: null, estimate_high: null });
    const group = groups.get(key);
    if (source.attribution !== "unknown") group.credentials++;
    const observed = usage(index, range.start, range.end);
    group.requests += observed.requests;
    group.tokens = add([group.tokens, observed.tokens]);
    const account = { ...source, group_key: key, ...observed, current: null };
    accounts.push(account);
    for (let ts = Math.floor(range.start / day) * day; ts < range.end; ts += day) {
      const value = usage(index, Math.max(ts, range.start), Math.min(ts + day, range.end));
      const dailyKey = JSON.stringify([key, ts]);
      const row = dailyMap.get(dailyKey) || { ts, group_key: key, requests: 0, tokens: tokens() };
      row.requests += value.requests;
      row.tokens = add([row.tokens, value.tokens]);
      dailyMap.set(dailyKey, row);
    }
    const modelIDs = source.provider === "anthropic" ? ["claude-opus-4-7", "claude-sonnet-4-6"] : source.provider === "codex" ? ["gpt-5.4", "gpt-5.4-mini"] : source.provider === "gemini" ? ["gemini-3.1-pro", "gemini-3-flash"] : source.provider === "glm" ? ["glm-4.7", "glm-5"] : ["demo-model", "demo-small"];
    const split = scaled(observed.tokens, 0.63 + index * 0.02);
    modelIDs.forEach((model, modelIndex) => {
      const modelKey = JSON.stringify([key, model]);
      const row = modelMap.get(modelKey) || { group_key: key, model, requests: 0, tokens: tokens() };
      const part = modelIndex === 0 ? split : tokens(Object.fromEntries(tokenKeys.map((name) => [name, observed.tokens[name] - split[name]])));
      row.tokens = add([row.tokens, part]);
      const firstRequests = Math.round(observed.requests * 0.67);
      row.requests += modelIndex === 0 ? firstRequests : observed.requests - firstRequests;
      modelMap.set(modelKey, row);
    });
    if (source.provider !== "anthropic") return;
    // Fixed reset cadence per account gives full-window current tokens and a
    // separate aligned interval for the estimate. The pro fixture lacks delta;
    // enterprise has intermittent observations and the disabled row none.
    const elapsed = five ? 10800 + index * 420 : day * (3.5 + index * 0.22);
    const start = now - elapsed;
    const observedAt = now - (index === 4 ? 10 * day : 240);
    const reset = start + windowSeconds;
    const used = [58, 63, 4, 29, 0][index] || 0;
    account.current = { start, reset_at: reset, observed_at: observedAt, used_pct: used, ...usage(index, start, Math.max(start, observedAt)), estimate: null, delta_pct: index === 2 ? 4 : 0, samples: index === 4 ? 0 : 2, reason: index === 2 ? "insufficient-delta" : index === 4 ? "insufficient-samples" : "" };
    if (index === 4) {
      // Backend placeholder object, not a genuine observed 0% reading.
      account.current = { start: 0, reset_at: 0, observed_at: 0, used_pct: 0, requests: 0, tokens: tokens(), estimate: null, delta_pct: 0, samples: 0, reason: "missing_quota" };
    }
    if (![0, 1, 3].includes(index)) return;
    const cadence = five ? day : windowSeconds;
    for (let end = observedAt, sequence = 0; end >= range.start; end -= cadence, sequence++) {
      if (end > range.end || (index === 3 && sequence % 3 === 1)) continue;
      const nominalStart = five ? end - 10800 : start - sequence * windowSeconds;
      const matchedStart = nominalStart + (five ? 1200 : 3600);
      if (matchedStart < range.start || end - matchedStart < 1800) continue;
      const interval = usage(index, matchedStart, end);
      const age = (now - end) / day;
      // Increasing Max throughput per quota point, decreasing Team; neither
      // direction is called a better entitlement. Cache mix changes as well.
      const trend = index === 0 ? 1.5 - age / 80 : index === 1 ? 0.55 + age / 40 : 1;
      const delta = Math.round(Math.min(used - 2, Math.max(11, (index === 0 ? 31 : index === 1 ? 37 : 21) / trend)) * 10) / 10;
      const estimate = tokens(Object.fromEntries(tokenKeys.map((key) => [key, interval.tokens[key] * 100 / delta])));
      const sample = { group_key: key, credential_id: source.id, start: matchedStart, end, reset_at: nominalStart + windowSeconds, observed_at: end, tokens: interval.tokens, estimate, delta_pct: Math.round(delta * 10) / 10, samples: Math.max(2, Math.floor((end - matchedStart) / 600)) };
      capacity_history.push(sample);
      if (sequence === 0) Object.assign(account.current, { estimate, delta_pct: sample.delta_pct, samples: sample.samples });
    }
  });
  // Component-wise summaries mirror the server's token bundle representation.
  const quantile = (values, fraction) => {
    const sorted = values.slice().sort((a, b) => a - b);
    const position = (sorted.length - 1) * fraction;
    const low = Math.floor(position);
    return sorted[low] + (sorted[Math.ceil(position)] - sorted[low]) * (position - low);
  };
  for (const group of groups.values()) {
    const samples = capacity_history.filter((sample) => sample.group_key === group.key);
    group.estimate_samples = samples.length;
    if (!samples.length) continue;
    const summary = (fraction) => tokens(Object.fromEntries(tokenKeys.map((key) => [key, quantile(samples.map((sample) => sample.estimate[key]), fraction)])));
    group.estimate = summary(0.5); group.estimate_low = summary(0.25); group.estimate_high = summary(0.75);
  }
  const list = [...groups.values()];
  return { from: range.start, to: range.end, as_of: now, window: five ? "five_hour" : "seven_day", requests: list.reduce((sum, group) => sum + group.requests, 0), tokens: add(list.map((group) => group.tokens)), groups: list, accounts, daily: [...dailyMap.values()].sort((a, b) => a.ts - b.ts), capacity_history: capacity_history.sort((a, b) => a.end - b.end), models: [...modelMap.values()], notes: ["Synthetic offline examples: Max20x and Team5x are explicitly stored metadata, never inferred from weight.", "Estimates cover only matched intervals. Some credentials have too little quota movement, missing readings, or no quota API."], truncated: false };
}

const DB = {
  "/stats/subscriptions": (q) => subscriptionReport(q),
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
    // Team seats publish no general weekly limit: the usage API returns
    // seven_day: null, which the server reports as `unreported`.
    const sevens = [58, 0, 92, 22, 0, 0];
    const weeklyUnreported = (c) => c.type === "team";
    const scopeds = [44, 51, 78, 15, 0, 0];
    // Score mirrors the pool: weight × room_5h × room_7d^1.5 × (1 + urgency),
    // urgency = max(0, room_7d / remaining_fraction_7d − 1). Disabled creds and
    // saturated snapshots (≥100% on either window) are excluded from the share.
    const rows = CREDS.map((c, i) => {
      const five = fives[i] ?? 0, seven = sevens[i] ?? 0;
      const room5 = Math.max(0, 1 - five / 100);
      const room7 = Math.max(0, 1 - seven / 100);
      const saturated = five >= 100 || seven >= 100;
      const active = c.status !== "disabled" && !saturated;
      const remaining7d = (2 + i) / 7; // matches seven_day.resets_at below
      const urgency = hasUsageAPI(c) && !weeklyUnreported(c) ? Math.max(0, room7 / remaining7d - 1) : 0;
      const score = active ? c.weight * room5 * Math.pow(room7, 1.5) * (1 + urgency) : 0;
      return { c, i, five, seven, scoped: scopeds[i], room5, room7, urgency, saturated, score };
    });
    // Share is totalled per provider, matching the backend: the pool filters by
    // provider before scoring, so a GLM key only competes with other GLM keys.
    const sums = {};
    for (const r of rows) sums[providerOf(r.c)] = (sums[providerOf(r.c)] || 0) + r.score;
    const anthropicLike = rows.map(({ c, i, five, seven, scoped, room5, room7, urgency, saturated, score }) => ({
      credential_id: c.id, label: c.label, subscription_type: c.type, status: c.status, weight: c.weight,
      provider: providerOf(c), has_usage_api: hasUsageAPI(c),
      five_hour: { pct: five, resets_at: now + (3600 * (1 + i)) },
      seven_day: weeklyUnreported(c)
        ? { pct: 0, resets_at: null, unreported: true }
        : { pct: seven, resets_at: now + (86400 * (2 + i)) },
      seven_day_scoped: hasUsageAPI(c) ? { pct: scoped, resets_at: now + (86400 * (2 + i)), label: "Fable" } : undefined,
      captured_at: hasUsageAPI(c) ? now - 300 - i * 90 : null,
      metered: hasUsageAPI(c) ? undefined : {
        five_hour: { requests: 412, input_tokens: 380_000, output_tokens: 96_000,
                     cache_read_tokens: 2_400_000, cache_creation_tokens: 41_000 },
        seven_day: { requests: 8_930, input_tokens: 7_900_000, output_tokens: 2_050_000,
                     cache_read_tokens: 52_000_000, cache_creation_tokens: 880_000 },
      },
      selection: {
        room_5h: room5, room_7d: room7, urgency, score,
        share_pct: score > 0 ? (score / (sums[providerOf(c)] || 1)) * 100 : 0,
        saturated,
      },
    }));
    // Codex accounts appear as siblings of Anthropic subs, with per-account
    // quota lifted from the sidecar's X-Codex-* signals (5h/7d, resets_at,
    // saturation). They always render as meters; before the first request
    // there is just no snapshot yet.
    const codexRows = CODEX_ACCOUNTS.map((a) => {
      const bw = a.base_weight ?? 1;
      const five = a.quota?.five_hour_pct ?? 0;
      const seven = a.quota?.seven_day_pct ?? 0;
      const room5 = Math.max(0, 1 - five / 100);
      const room7 = Math.max(0, 1 - seven / 100);
      const saturated = five >= 100 || seven >= 100;
      const score = a.disabled || saturated ? 0 : bw * room5 * Math.pow(room7, 1.5);
      return {
        credential_id: "codex:" + a.name, label: a.email || a.label || a.name,
        subscription_type: a.quota?.plan_type || a.account_type || "subscription", provider: "codex",
        has_usage_api: true,
        status: a.disabled ? "disabled" : (a.blocked ? "errored" : "active"),
        weight: bw,
        five_hour: { pct: five, resets_at: a.quota?.has_signals && !a.quota?.weekly_only ? now + 4 * 3600 : null,
                     absent: !!a.quota?.weekly_only },
        seven_day: { pct: seven, resets_at: a.quota?.has_signals ? now + 6 * 86400 : null },
        captured_at: a.quota?.has_signals ? now - 120 : null,
        selection: { room_5h: room5, room_7d: room7, urgency: 0, score, share_pct: score > 0 ? 100 : 0, saturated },
      };
    });
    const geminiRows = GEMINI_ACCOUNTS.map((a) => {
      const bw = a.base_weight ?? 1;
      const five = a.quota?.five_hour_pct ?? 0;
      const room5 = Math.max(0, 1 - five / 100);
      const saturated = five >= 100;
      const score = a.disabled || saturated ? 0 : bw * room5;
      return {
        credential_id: "gemini:" + a.name, label: a.email || a.label || a.name,
        subscription_type: a.quota?.plan_type || a.account_type || "subscription", provider: "gemini",
        has_usage_api: true,
        status: a.disabled ? "disabled" : (a.blocked ? "errored" : "active"),
        weight: bw,
        five_hour: { pct: five, resets_at: a.quota?.has_signals ? now + 2.5 * 3600 : null },
        seven_day: { pct: 0, resets_at: null, absent: true },
        captured_at: a.quota?.has_signals ? now - 45 : null,
        selection: { room_5h: room5, room_7d: 1, urgency: 0, score, share_pct: score > 0 ? 100 : 0, saturated },
      };
    });
    for (const channelRows of [codexRows, geminiRows]) {
      const total = channelRows.reduce((sum, row) => sum + row.selection.score, 0);
      for (const row of channelRows) row.selection.share_pct = total ? row.selection.score / total * 100 : 0;
    }
    return [...anthropicLike, ...codexRows, ...geminiRows];
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
      const scoped = wave(n, 18 + i * 8, 40, i + 8).map((v) => Math.min(100, v));
      // Simulate missing snapshots (a fresh import mid-window) as nulls.
      const nulls = (arr) => arr.map((v, j) => (i === active.length - 1 && j < n / 3 ? null : v));
      return {
        credential_id: c.id, label: c.label,
        five_hour_pct: nulls(five),
        seven_day_pct: c.type === "team" ? seven.map(() => null) : nulls(seven),
        seven_day_scoped_pct: nulls(scoped),
        seven_day_scoped_label: "Fable",
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
      id: c.id, label: c.label, subscription_type: c.type, rate_limit_tier: c.tier || "", status: c.status, weight: c.weight,
      provider: providerOf(c), has_usage_api: hasUsageAPI(c),
      endpoint: endpointOf(c), endpoint_name: endpointNameOf(c),
      endpoint_editable: !hasUsageAPI(c),
      models: c.models || [],
      request_count: [8200, 5400, 1200, 3600, 40, 970][i], last_request_at: c.status === "disabled" ? null : now - 60 * (i + 1),
      expires_at: now + 3600 * (5 - i), created_at: now - 86400 * (30 - i * 4),
    })),
  "/credentials/endpoints": () => MOCK_ENDPOINTS,
  "/codex/accounts": () => ({ configured: true, accounts: CODEX_ACCOUNTS }),
  "/codex/oauth/status": () => ({ status: ++sidecarOAuthPolls.codex > 2 ? "ok" : "wait" }),
  "/gemini/accounts": () => ({ configured: true, accounts: GEMINI_ACCOUNTS }),
  "/gemini/oauth/status": () => ({ status: ++sidecarOAuthPolls.gemini > 2 ? "ok" : "wait" }),
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
  "/routing/events": (q) => routingEventsPage(q),
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
    // Sidecar channels share one handler set: /codex/* and /gemini/*.
    const sc = path.match(/^\/(codex|gemini)(\/.*)$/);
    const scAccounts = sc ? SIDECAR_ACCOUNTS[sc[1]] : null;
    const scPath = sc ? sc[2] : "";
    if (scPath === "/oauth/start") {
      sidecarOAuthPolls[sc[1]] = 0;
      return json({ url: `about:blank#mock-${sc[1]}-login`, state: `mock_${sc[1]}_state`, callback_mode: "localhost_or_manual" });
    }
    if (scPath === "/oauth/callback") return json({ ok: true });
    if (scPath === "/oauth/cancel") return json({ ok: true });
    if (scPath === "/accounts/status") {
      const b = JSON.parse(init.body || "{}");
      const a = scAccounts.find((x) => x.name === b.name);
      if (!a) return json({ error: "account not found" }, 404);
      a.disabled = !!b.disabled;
      return json({ ok: true });
    }
    if (scPath === "/accounts/weight") {
      const b = JSON.parse(init.body || "{}");
      const a = scAccounts.find((x) => x.name === b.name);
      if (!a) return json({ error: "account not found" }, 404);
      if (!Number.isInteger(b.weight) || b.weight < 1 || b.weight > 1_000_000) {
        return json({ error: "weight must be between 1 and 1000000" }, 400);
      }
      a.base_weight = b.weight;
      return json({ ok: true, weight: a.base_weight });
    }
    if (scPath === "/accounts/delete") {
      const b = JSON.parse(init.body || "{}");
      const i = scAccounts.findIndex((x) => x.name === b.name);
      if (i < 0) return json({ error: "account not found" }, 404);
      scAccounts.splice(i, 1);
      return json({ ok: true });
    }
    if (path === "/users" && method === "POST") return json({ id: "utok_new", name: JSON.parse(init.body || "{}").name || "new", token: "cpu_" + Math.random().toString(36).slice(2) + Math.random().toString(36).slice(2) });
    // Adding a key appends to CREDS so the credentials table reflects it for
    // the rest of the mock session, matching how the real endpoint behaves.
    // Mirrors the real endpoint: re-verification can reject the move, so the
    // mock refuses an obviously bogus URL rather than always succeeding.
    // Anthropic browser sign-in. Mirrors claudeoauth.Flow: sessions are
    // single-use, and the code "bad" stands in for one Anthropic rejects.
    if (path === "/credentials/oauth/start" && method === "POST") {
      const session = "mock_" + Math.random().toString(36).slice(2);
      claudeSessions.add(session);
      return json({ session, url: "https://claude.com/cai/oauth/authorize?code=true&state=" + session });
    }
    if (path === "/credentials/oauth/cancel" && method === "POST") {
      claudeSessions.delete(JSON.parse(init.body || "{}").session);
      return json({ ok: true });
    }
    if (path === "/credentials/oauth/exchange" && method === "POST") {
      const b = JSON.parse(init.body || "{}");
      const target = b.credential_id ? CREDS.find((x) => x.id === b.credential_id) : null;
      if (b.credential_id && !target) return json({ error: "credential not found" }, 404);
      if (!claudeSessions.delete(b.session)) return json({ error: "sign-in session expired or unknown; start the sign-in again" }, 400);
      if (!String(b.code || "").trim()) return json({ error: "paste the authentication code shown after approving access" }, 400);
      if (String(b.code).trim().startsWith("bad")) {
        return json({ error: "the code was rejected by Anthropic (400) — it may have expired or been used already; start the sign-in again: invalid_grant" }, 400);
      }
      if (target) {
        target.status = "active";
        return json({ ok: true, id: target.id, label: target.label, status: "active", subscription_type: target.type, weight: target.weight });
      }
      const c = { id: "cred_" + Math.random().toString(16).slice(2, 10), label: b.label || "new-owner@example.com",
        type: "max", weight: Number.isInteger(b.weight) ? b.weight : 5, status: "active" };
      CREDS.push(c);
      return json({ ok: true, id: c.id, label: c.label, status: "active", subscription_type: c.type, weight: c.weight });
    }
    const tierMatch = path.match(/^\/credentials\/([^/]+)\/tier$/);
    if (tierMatch && method === "POST") {
      const b = JSON.parse(init.body || "{}");
      const c = CREDS.find((row) => row.id === decodeURIComponent(tierMatch[1]));
      if (!c) return json({ error: "credential not found" }, 404);
      if (typeof b.tier !== "string" || new TextEncoder().encode(b.tier).length > 128 || /[\p{Cc}]/u.test(b.tier)) {
        return json({ error: "tier must be at most 128 UTF-8 bytes without control characters" }, 400);
      }
      c.tier = b.tier.trim();
      return json({ ok: true });
    }
    const settings = path.match(/^\/credentials\/([^/]+)\/settings$/);
    if (settings && method === "POST") {
      const b = JSON.parse(init.body || "{}");
      if (typeof b.label !== "string" || [...b.label.trim()].length > 200) return json({ error: "label must be a string of at most 200 characters" }, 400);
      if (!Number.isSafeInteger(b.weight) || b.weight < 1) return json({ error: "weight must be >= 1" }, 400);
      const c = CREDS.find((x) => x.id === decodeURIComponent(settings[1]));
      if (!c) return json({ error: "not found" }, 404);
      c.label = b.label.trim();
      c.weight = b.weight;
      return json({ ok: true, id: c.id, label: c.label, weight: c.weight });
    }
    const credentialAction = path.match(/^\/credentials\/([^/]+)(?:\/(enable|disable|weight|refresh))?$/);
    if (credentialAction && (credentialAction[2] || method === "DELETE")) {
      const c = CREDS.find((x) => x.id === decodeURIComponent(credentialAction[1]));
      if (!c) return json({ error: "not found" }, 404);
      if (method === "DELETE") CREDS.splice(CREDS.indexOf(c), 1);
      else if (credentialAction[2] === "enable") c.status = "active";
      else if (credentialAction[2] === "disable") c.status = "disabled";
      else if (credentialAction[2] === "weight") c.weight = JSON.parse(init.body || "{}").weight;
      return json({ ok: true });
    }
    const epm = path.match(/^\/credentials\/([^/]+)\/endpoint$/);
    if (epm && method === "POST") {
      const b = JSON.parse(init.body || "{}");
      if (!b.endpoint) return json({ error: "endpoint is required" }, 400);
      const c = CREDS.find((x) => x.id === epm[1]);
      if (!c) return json({ error: "not found" }, 404);
      const preset = (MOCK_ENDPOINTS[providerOf(c)]?.endpoints || []).find((e) => e.name === b.endpoint);
      const url = preset ? preset.url : b.endpoint;
      if (!/^https?:\/\//.test(url)) return json({ error: `unknown endpoint "${b.endpoint}"` }, 400);
      c.endpoint = url;
      if (c.status !== "disabled") c.status = "active";
      return json({ ok: true, id: c.id, status: c.status, endpoint: url });
    }
    // Mirrors ProbeCustomHost so the unified add-modal's "Test connection"
    // works offline for every provider kind.
    if (path === "/credentials/probe" && method === "POST") {
      const b = JSON.parse(init.body || "{}");
      if (!/^https?:\/\//.test(b.base_url || "")) {
        return json({ ok: false, error: "base URL must start with http:// or https://", models: [] });
      }
      return json({
        ok: true, auth_required: !!b.api_key, has_models_api: false,
        has_count_tokens: b.provider !== "custom_openai", reported_model: b.model || "demo-model-v1",
        models: [{ id: b.model || "demo-model-v1", display_name: b.model || "demo-model-v1" }],
      });
    }
    if (path === "/credentials/custom" && method === "POST") {
      const b = JSON.parse(init.body || "{}");
      if (!b.base_url) return json({ error: "base URL is required" }, 400);
      const c = { id: "cred_" + Math.random().toString(36).slice(2, 6),
        label: b.label || new URL(b.base_url).host, type: "", weight: b.weight || 1,
        status: "active", provider: b.provider || "custom", endpoint: b.base_url,
        models: b.models || [] };
      CREDS.push(c);
      return json({ ok: true, id: c.id, label: c.label, base_url: b.base_url });
    }
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
