// Routing activity panel (dashboard page).
//
// Renders GET /api/routing/events — the elective routing decision log. Live
// rows describe real conversation pins: "pending" means a move was prepared
// and its notice header emitted — emission is not client acknowledgment —
// "switched" is the only kind that actually moved a conversation, and
// "cancelled"/"deferred" ended without one. Shadow rows are per-destination
// evaluations that never touch a binding; their nested evidence carries
// confidence/baseline, an optional forecast (projected unused weekly %, the
// reset instant, burn rates) and last-hour workload counts in native units.
// Headline facts render as chips; the full bounded field set and the complete
// blocker list live behind a details toggle — nothing is collapsed into a
// dead-end count.
//
// Rows are cursor-paginated newest-first (`before` = the previous page's
// `next_before`), and paging is bounded so a long session cannot grow the
// DOM without limit. Every async continuation re-checks isConnected, so a
// navigation away mid-fetch simply drops the result.

import { api } from "./api.js";
import { el, clear, spinner, errorState, emptyState, button } from "./ui.js";
import { localTime, relTime, pct, fullNum, compactNum } from "./format.js";

const PAGE = 50;
// Hard ceiling on accumulated rows ("Load older" disables here). Ten pages is
// far beyond anything an operator scans in one sitting, and the repaint is
// O(rows), so the bound also bounds the work.
const MAX_ROWS = 500;

const KINDS = {
  switched: { label: "moved", cls: "good" },
  pending: { label: "pending", cls: "warning" },
  deferred: { label: "deferred", cls: "warning" },
  cancelled: { label: "cancelled", cls: "muted" },
  shadow_decision: { label: "shadow", cls: "shadow" },
};

export function routingHistoryPanel() {
  let rows = [];
  const seen = new Set();
  let cursor = null; // next_before of the last loaded page
  let serverDone = false;
  let retentionDays = 0;
  // Drops results of superseded loads (a refresh issued while an older-page
  // fetch is in flight, or vice versa).
  let seq = 0;

  const listEl = el("div", { class: "routing__list", role: "list", "aria-label": "Routing events, newest first" });
  const errEl = el("div", { class: "routing__err", role: "alert", hidden: true });
  const countEl = el("span", { class: "routing__count" });
  const liveEl = el("div", { class: "sr-only", role: "status", "aria-live": "polite" });
  const retentionEl = el("span", { class: "routing__retention" });
  const olderBtn = button("Load older", { onClick: () => load(false), title: "Append the next page of older events" });
  const refreshBtn = button("Refresh", { onClick: () => load(true), title: "Reload the newest routing events" });
  olderBtn.setAttribute("aria-label", "Load older routing events");
  refreshBtn.setAttribute("aria-label", "Refresh routing events");

  const root = el("section", { class: "card routing", "aria-label": "Routing activity" }, [
    el("div", { class: "routing__head" }, [
      el("div", { class: "routing__intro" }, [
        el("div", { class: "eyebrow", text: "routing" }),
        el("h2", { class: "routing__title", text: "Routing activity" }),
        el("p", {
          class: "routing__sub",
          text: "Elective credential moves the router executed or prepared for long-lived sessions — live rows describe real conversation pins; shadow rows are per-destination evaluations that never move anything.",
        }),
      ]),
      el("div", { class: "routing__tools" }, [retentionEl, refreshBtn]),
    ]),
    // Decorative key to the row badges; each row restates its own kind.
    el("div", { class: "routing__legend", "aria-hidden": "true" }, [
      legendItem("good", "moved", "conversation switched"),
      legendItem("warning", "pending", "move prepared, not executed"),
      legendItem("muted", "deferred · cancelled", "no move happened"),
      legendItem("shadow", "shadow", "evaluated, never executed"),
    ]),
    liveEl,
    listEl,
    errEl,
    el("div", { class: "routing__foot" }, [olderBtn, countEl]),
    el("p", { class: "routing__note" }, [
      el("strong", { text: "Shadow rows never move anything. " }),
      el("span", {
        text:
          "Near-reset observations — an account whose quota resets before its allowance can be spent — are recorded in shadow mode only, never executed. " +
          "A pending row means a move was prepared and its notice header emitted, not that the client saw it; the CLI does not necessarily display those headers, " +
          "and this log is the durable record.",
      }),
    ]),
  ]);

  function paintFoot() {
    const done = serverDone || cursor == null;
    const capped = rows.length >= MAX_ROWS;
    olderBtn.disabled = done || capped || rows.length === 0;
    olderBtn.textContent = "Load older";
    const bits = [`showing ${rows.length} event${rows.length === 1 ? "" : "s"}`];
    if (capped) bits.push(`view capped at ${MAX_ROWS}`);
    else if (done && rows.length) bits.push("oldest reached");
    countEl.textContent = bits.join(" · ");
    retentionEl.textContent = retentionDays
      ? `log kept ${retentionDays} day${retentionDays === 1 ? "" : "s"}`
      : "";
  }

  function paintList() {
    clear(listEl);
    const shown = rows.slice(0, MAX_ROWS);
    for (const ev of shown) listEl.append(eventRow(ev));
    if (!shown.length) {
      listEl.append(
        emptyState(
          "No routing events yet",
          "The log fills when the router announces, completes or reconsiders an elective credential move.",
        ),
      );
    }
  }

  async function load(refresh) {
    const mine = ++seq;
    const hadRows = rows.length > 0;
    errEl.hidden = true;
    refreshBtn.disabled = true;
    olderBtn.disabled = true;
    refreshBtn.textContent = "Refreshing…";
    olderBtn.textContent = "Loading…";
    // Keep the current rows visible during a refresh; only a first load (or a
    // refresh from empty) swaps the list for a spinner.
    if (!hadRows) clear(listEl).append(spinner("Loading routing events…"));
    try {
      const r = await api.routingEvents({
        limit: PAGE,
        before: !refresh && cursor != null ? cursor : undefined,
      });
      // Guard: navigated away, or a newer load superseded this one.
      if (!root.isConnected || mine !== seq) return;
      const items = Array.isArray(r && r.items) ? r.items : [];
      const rd = Number(r && r.retention_days);
      if (Number.isFinite(rd) && rd > 0) retentionDays = Math.round(rd);
      if (refresh) {
        rows = [];
        seen.clear();
      }
      cursor = r && r.next_before != null ? r.next_before : null;
      serverDone = cursor == null;
      const preCount = rows.length;
      // A refresh that lands on top of appended pages can overlap the already
      // loaded range; the id set keeps each event on screen once.
      for (const ev of items) {
        const key = dedupeKey(ev);
        if (key == null || seen.has(key)) continue;
        seen.add(key);
        rows.push(ev);
      }
      // An append can overshoot the cap by part of a page; keep the newest
      // MAX_ROWS so the count and the rendered rows always agree.
      if (rows.length > MAX_ROWS) rows = rows.slice(0, MAX_ROWS);
      // Retained rows only: whatever the trim dropped was never shown, so it
      // must not be announced either.
      const added = rows.length - preCount;
      paintList();
      liveEl.textContent = rows.length
        ? refresh
          ? `Routing activity refreshed — ${rows.length} events shown.`
          : `${added} older event${added === 1 ? "" : "s"} loaded.`
        : "No routing events recorded.";
    } catch (e) {
      if (!root.isConnected || mine !== seq) return;
      if (!rows.length) {
        clear(listEl).append(errorState(e.message, () => load(refresh)));
      } else {
        errEl.hidden = false;
        clear(errEl).append(
          el("span", {
            text: (refresh ? "Couldn't refresh: " : "Couldn't load older events: ") + e.message,
          }),
          button("Retry", { onClick: () => load(refresh) }),
        );
      }
      liveEl.textContent = "Loading routing events failed.";
    } finally {
      if (root.isConnected && mine === seq) {
        refreshBtn.disabled = false;
        refreshBtn.textContent = "Refresh";
        paintFoot();
      }
    }
  }

  paintFoot();
  load(true);
  return root;
}

// ---- row rendering (module-level, pure DOM) ----

function legendItem(cls, label, gloss) {
  return el("span", { class: "routing__lg" }, [
    el("span", { class: `badge badge--${cls}` }, [
      el("span", { class: "badge__dot" }),
      el("span", { text: label }),
    ]),
    el("span", { class: "routing__lg-txt", text: gloss }),
  ]);
}

function eventRow(ev) {
  const kind = String(ev.kind || "");
  const def = KINDS[kind];
  const shadow = String(ev.mode || "").toLowerCase() === "shadow";
  const ts = Number(ev.ts) > 0 ? Number(ev.ts) : 0;

  const timeEl = el("time", { class: "rev__time", text: localTime(ts) });
  if (ts) {
    const d = new Date(ts * 1000);
    timeEl.setAttribute("datetime", d.toISOString());
    timeEl.setAttribute("title", `${d.toLocaleString()} · ${relTime(ts)}`);
  }

  return el("div", { class: "rev" + (shadow ? " rev--shadow" : ""), role: "listitem" }, [
    el("div", { class: "rev__main" }, [
      timeEl,
      el("span", { class: `badge badge--${def ? def.cls : "muted"}` }, [
        el("span", { class: "badge__dot" }),
        el("span", { text: def ? def.label : kind || "event" }),
      ]),
      shadow ? el("span", { class: "rev__mode", text: "not executed" }) : null,
      ev.policy ? el("span", { class: "rev__policy", text: String(ev.policy) }) : null,
      credPath(ev),
      ev.conversation
        ? el("span", {
            class: "rev__conv",
            text: "conv " + shortRef(ev.conversation),
            title: "conversation " + String(ev.conversation),
          })
        : null,
    ]),
    reasonLine(ev.reason),
    evidenceRow(ev),
  ]);
}

function reasonLine(reason) {
  const raw = String(reason == null ? "" : reason).trim();
  if (!raw) return null;
  const s = humanToken(raw);
  return el("p", { class: "rev__reason", text: s, title: s !== raw ? raw : null });
}

function evidenceRow(ev) {
  const chips = evidenceSummaryChips(ev);
  const det = evidenceDetails(ev);
  if (!chips.length && !det) return null;
  return el("div", { class: "rev__ev" }, [
    chips.length ? el("div", { class: "rev__chips" }, chips) : null,
    det,
  ]);
}

// "ax91 → bt42" with the full IDs on hover. A one-sided row is neutral, never
// directional: per-destination shadow evaluations have a target only, and
// wording like "to X" would read as a proposed move.
function credPath(ev) {
  const src = shortCred(ev.source_id);
  const dst = shortCred(ev.target_id);
  let text;
  if (src && dst) text = `${src} → ${dst}`;
  else if (src) text = `source ${src}`;
  else if (dst) text = `destination ${dst}`;
  else return null;
  const title = [ev.source_id, ev.target_id].filter(Boolean).map(String).join("  →  ");
  return el("span", { class: "rev__path", text, title: title || null });
}

function shortCred(id) {
  const s = String(id == null ? "" : id).trim().replace(/^cred_/, "");
  if (!s) return "";
  return s.length > 12 ? s.slice(0, 11) + "…" : s;
}

function shortRef(id) {
  const s = String(id == null ? "" : id).trim();
  if (!s) return "—";
  return s.length > 10 ? s.slice(0, 8) + "…" : s;
}

// Backend reasons are short tokens ("no-longer-eligible") or plain sentences.
// Only the token form is rewritten for display; a sentence is already
// operator-readable, and the raw value is kept one hover away via the title.
function reasonText(r) {
  const s = String(r == null ? "" : r).trim();
  if (!s) return "";
  if (/^[a-z0-9]+(?:[-_][a-z0-9]+)+$/.test(s)) return s.replace(/[-_]+/g, " ");
  return s;
}

// ---- nested evidence contract (per-destination shadow evaluations) ----

// Headline facts from the nested evidence object: non-executability,
// confidence, the demand baseline, the projected-unused range, the reset
// instant and last-hour demand in native units. Everything else lives in the
// details toggle — no field is summarised into a dead end.
function evidenceSummaryChips(ev) {
  const e = ev && ev.evidence;
  if (!e || typeof e !== "object" || Array.isArray(e)) return [];
  const chips = [];
  if (e.executable === false) {
    chips.push(el("span", { class: "rev__chip rev__chip--muted", text: "not executable" }));
  }
  if (e.confidence != null) chips.push(el("span", { class: "rev__chip", text: `confidence ${humanToken(e.confidence)}` }));
  if (e.baseline != null) chips.push(el("span", { class: "rev__chip", text: `baseline ${humanToken(e.baseline)}` }));
  const f = e.forecast;
  if (f && typeof f === "object" && !Array.isArray(f)) {
    const unused = rangePct(f.unused_weekly_pct);
    if (unused) chips.push(el("span", { class: "rev__chip", text: `projected unused ${unused}` }));
    const reset = numOrNull(f.reset_at);
    if (reset > 0) chips.push(el("span", { class: "rev__chip", text: `quota resets ${localTime(reset)}` }));
  }
  const w = e.workload_last_hour;
  if (w && typeof w === "object" && !Array.isArray(w) && numOrNull(w.requests) != null) {
    const out = numOrNull(w.output_tokens);
    chips.push(el("span", {
      class: "rev__chip",
      text: `last hour ${fullNum(w.requests)} req` + (out != null ? ` · ${compactNum(out)} out tok` : ""),
      title: `requests ${fullNum(w.requests)}` + (out != null ? ` · output tokens ${fullNum(out)} — native units, no cost conversion` : ""),
    }));
  }
  return chips;
}

// Collapsible disclosure holding the full bounded field set: descriptors,
// the complete blocker list, the forecast and the last-hour workload. Native
// units throughout — counts stay counts, percentages stay percentages.
function evidenceDetails(ev) {
  const e = ev && ev.evidence;
  if (!e || typeof e !== "object" || Array.isArray(e)) return null;
  const secs = [];

  const desc = [
    e.measurement != null ? ["measurement", humanToken(e.measurement)] : null,
    e.scope_knowledge != null ? ["scope knowledge", humanToken(e.scope_knowledge)] : null,
  ].filter(Boolean);
  if (desc.length) secs.push(kvSection("Evaluation", desc));

  const blockers = blockerList(e.blockers);
  if (blockers.length) {
    secs.push(el("div", { class: "rev__det-sec" }, [
      el("div", { class: "rev__det-h", text: "Blockers" }),
      el("ul", { class: "rev__det-list" }, blockers.map((b) => el("li", { text: b }))),
    ]));
  }

  const f = e.forecast;
  if (f && typeof f === "object" && !Array.isArray(f)) {
    const reset = numOrNull(f.reset_at);
    const rows = [
      reset > 0 ? ["reset at", localTime(reset)] : null,
      numOrNull(f.horizon_seconds) != null ? ["horizon", dur(f.horizon_seconds)] : null,
      numOrNull(f.sample_age_seconds) != null ? ["sample age", dur(f.sample_age_seconds)] : null,
      Array.isArray(f.sample_times) && f.sample_times.length
        ? ["samples", f.sample_times.map((t) => localTime(numOrNull(t) || 0)).join(" · ")]
        : null,
      rangePct(f.five_hour_pct_per_hour) ? ["5h burn / hour", rangePct(f.five_hour_pct_per_hour)] : null,
      rangePct(f.weekly_pct_per_hour) ? ["weekly burn / hour", rangePct(f.weekly_pct_per_hour)] : null,
      rangePct(f.unused_weekly_pct) ? ["unused weekly", rangePct(f.unused_weekly_pct)] : null,
    ].filter(Boolean);
    if (rows.length) secs.push(kvSection("Forecast", rows));
  }

  const w = e.workload_last_hour;
  if (w && typeof w === "object" && !Array.isArray(w)) {
    const rows = [
      numOrNull(w.requests) != null ? ["requests", fullNum(w.requests)] : null,
      numOrNull(w.output_tokens) != null ? ["output tokens", fullNum(w.output_tokens)] : null,
      numOrNull(w.cache_creation_tokens) != null ? ["cache creation tokens", fullNum(w.cache_creation_tokens)] : null,
      numOrNull(w.cache_read_tokens) != null ? ["cache read tokens", fullNum(w.cache_read_tokens)] : null,
      numOrNull(w.previous_requests_per_hour) != null ? ["previous requests / hour", fullNum(w.previous_requests_per_hour)] : null,
      numOrNull(w.recent_requests_per_hour) != null ? ["recent requests / hour", fullNum(w.recent_requests_per_hour)] : null,
      w.request_rate_trend != null ? ["request rate trend", String(w.request_rate_trend)] : null,
      numOrNull(w.requests_after_sample) != null ? ["requests after sample", fullNum(w.requests_after_sample)] : null,
      numOrNull(w.arrivals_after_sample) != null ? ["arrivals after sample", fullNum(w.arrivals_after_sample)] : null,
      numOrNull(w.pending_targets) != null ? ["pending targets", fullNum(w.pending_targets)] : null,
      numOrNull(w.inflight) != null ? ["in-flight", fullNum(w.inflight)] : null,
      typeof w.complete === "boolean" ? ["complete", w.complete ? "yes" : "no"] : null,
    ].filter(Boolean);
    if (rows.length) secs.push(kvSection("Workload · last hour", rows));
  }

  if (!secs.length) return null;
  return el("details", { class: "rev__det" }, [
    el("summary", { text: "Evidence" }),
    el("div", { class: "rev__det-body" }, secs),
  ]);
}

function kvSection(title, rows) {
  return el("div", { class: "rev__det-sec" }, [
    el("div", { class: "rev__det-h", text: title }),
    el("div", { class: "rev__kvs" }, rows.map(([k, v]) =>
      el("div", { class: "rev__kv" }, [
        el("span", { class: "rev__kv-k", text: k }),
        el("span", { class: "rev__kv-v", text: String(v) }),
      ]),
    )),
  ]);
}

// Blockers are strings in the contract; token forms are humanised, sentences
// pass through. The complete list renders in the details element.
function blockerList(v) {
  if (!Array.isArray(v)) return typeof v === "string" && v.trim() ? [humanToken(v)] : [];
  const out = [];
  for (const b of v) if (typeof b === "string" && b.trim()) out.push(humanToken(b));
  return out;
}

// Kebab/snake tokens → readable words ("no_need" → "no need", "max_only" →
// "max only"). Sentences and anything with spaces or symbols pass through.
function humanToken(v) {
  const s = String(v == null ? "" : v).trim();
  if (!s) return s;
  return /^[a-z0-9]+(?:[-_][a-z0-9]+)*$/.test(s) ? s.replace(/[-_]+/g, " ") : s;
}

// {min,max} percentage pair → "18–57%", "42%" or null when absent. The unit
// is written once after the range rather than per bound.
function rangePct(r) {
  if (!r || typeof r !== "object" || Array.isArray(r)) return null;
  const lo = numOrNull(r.min);
  const hi = numOrNull(r.max);
  if (lo == null && hi == null) return null;
  if (lo != null && hi != null && hi !== lo) return `${lo.toFixed(0)}–${hi.toFixed(0)}%`;
  return pct(lo != null ? lo : hi, 0);
}

// Compact duration for horizons and sample ages: "2h 30m".
function dur(sec) {
  const s = Math.max(0, Math.round(Number(sec) || 0));
  if (s < 90) return s + "s";
  const m = Math.round(s / 60);
  if (m < 90) return m + "m";
  const h = Math.floor(m / 60);
  if (h < 48) return `${h}h ${m % 60}m`;
  return `${Math.floor(h / 24)}d ${h % 24}h`;
}

function numOrNull(v) {
  const n = Number(v);
  return v != null && Number.isFinite(n) ? n : null;
}

function dedupeKey(ev) {
  if (!ev || typeof ev !== "object") return null;
  if (ev.id != null) return "id:" + ev.id;
  return `f:${ev.ts}|${ev.kind}|${ev.source_id}|${ev.target_id}`;
}
