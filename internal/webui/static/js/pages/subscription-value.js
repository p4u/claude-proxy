import { api } from "../api.js";
import { el, clear, spinner, errorState, emptyState, button, modal, toast } from "../ui.js";
import { sectionHead, statTile, chartFrame } from "../components.js";
import { timeChart, seriesColors } from "../charts.js";
import { compactNum, fullNum, pct, localTime } from "../format.js";

// Page-local, deliberately independent of the Control room's short time window.
const selection = { period: "30d", window: "seven_day", provider: "all", metric: "total" };
const METRICS = [
  ["total", "All tokens"], ["input", "Input"], ["output", "Output"],
  ["cache_read", "Cache read"], ["cache_creation", "Cache write"],
];
const PARTS = METRICS.slice(1);
const DAY = 86400;
const metricName = (metric) => METRICS.find(([key]) => key === metric)?.[1] || "Tokens";
const tokenValue = (tokens, metric = "total") => tokens == null ? null : Number(tokens[metric] || 0);
const dayOf = (ts) => Math.floor(ts / DAY) * DAY;
const dateUTC = (ts) => new Date(ts * 1000).toLocaleDateString(undefined, { month: "short", day: "numeric", timeZone: "UTC" });
const groupLabel = (group) => group.label || [group.plan || "Unknown plan", group.tier || "tier unknown"].join(" · ");
const providerName = (group) => group.provider_name || group.provider || "Unknown provider";
const sumTokens = (rows) => Object.fromEntries(METRICS.map(([key]) => [key, rows.reduce((sum, row) => sum + (tokenValue(row.tokens, key) || 0), 0)]));
const median = (values) => {
  const sorted = values.slice().sort((a, b) => a - b);
  const mid = Math.floor(sorted.length / 2);
  return sorted.length ? (sorted.length % 2 ? sorted[mid] : (sorted[mid - 1] + sorted[mid]) / 2) : null;
};

export function render(root) {
  clear(root);
  const page = el("div", { class: "subscription-value" });
  const content = el("div", { class: "sv-content" });
  const status = el("div", { class: "sv-status", role: "status", "aria-live": "polite" });
  const failure = el("div");
  let report = null;
  let requestID = 0;
  let tierDialog = null;
  const refresh = button("Refresh report", { onClick: load });
  const exact = el("button", { class: "btn btn--ghost", type: "button", text: "Exact table counts", "aria-pressed": "false", onClick: () => {
    const enabled = page.classList.toggle("sv-exact");
    exact.setAttribute("aria-pressed", String(enabled));
  } });
  const period = selectControl("Observed period", [["7d", "Last 7 days"], ["30d", "Last 30 days"], ["90d", "Last 90 days"]], selection.period, (value) => { selection.period = value; load(); });
  const quota = selectControl("Quota window", [["seven_day", "7-day quota"], ["five_hour", "5-hour quota"]], selection.window, (value) => { selection.window = value; load(); });
  const provider = selectControl("Provider", [["all", "All providers"]], selection.provider, (value) => { selection.provider = value; draw(); });
  const metric = selectControl("Token metric", METRICS, selection.metric, (value) => { selection.metric = value; draw(); });
  page.append(
    sectionHead("Subscription value", "What your subscriptions actually delivered — and what observed quota changes suggest for the same workload.", [exact, refresh]),
    el("div", { class: "sv-filters", "aria-label": "Subscription value filters" }, [period.root, quota.root, provider.root, metric.root]),
    status, failure, content,
  );
  root.append(page);
  content.append(spinner("Matching traffic to quota observations…"));

  // A modal is mounted outside the outlet; close it if navigation removes this
  // page. Plots are released by charts.js's shared disconnected-node observer.
  const cleanup = new MutationObserver(() => {
    if (page.isConnected) return;
    requestID++;
    tierDialog?.close();
    cleanup.disconnect();
  });
  cleanup.observe(document.body, { childList: true, subtree: true });

  async function load() {
    const request = ++requestID;
    refresh.disabled = true;
    content.setAttribute("aria-busy", "true");
    content.classList.toggle("is-loading", !!report);
    status.textContent = report ? "Updating filters… previous report remains visible until the new report arrives." : "Reading recorded traffic and quota evidence…";
    clear(failure);
    try {
      const data = await api.statsSubscriptions(selection.period, selection.window);
      if (!page.isConnected || request !== requestID) return;
      report = data;
      const providers = new Map((report.groups || []).map((group) => [group.provider, providerName(group)]));
      clear(provider.input);
      provider.input.append(el("option", { value: "all", text: "All providers" }));
      for (const [id, name] of providers) provider.input.append(el("option", { value: id, text: name }));
      if (!providers.has(selection.provider)) selection.provider = "all";
      provider.input.value = selection.provider;
      draw();
    } catch (error) {
      if (!page.isConnected || request !== requestID) return;
      status.textContent = report ? "Refresh failed. The previous report is still shown; its dates below have not changed." : "Report unavailable.";
      if (!report) clear(content);
      failure.append(errorState(error.message, load));
    } finally {
      if (page.isConnected && request === requestID) {
        refresh.disabled = false;
        content.setAttribute("aria-busy", "false");
        content.classList.remove("is-loading");
      }
    }
  }

  function draw() {
    if (!report || !page.isConnected) return;
    clear(content);
    const groups = (report.groups || []).filter((group) => selection.provider === "all" || group.provider === selection.provider);
    const keys = new Set(groups.map((group) => group.key));
    const accounts = (report.accounts || []).filter((account) => keys.has(account.group_key));
    const history = (report.capacity_history || []).filter((sample) => keys.has(sample.group_key));
    const daily = (report.daily || []).filter((row) => keys.has(row.group_key));
    const models = (report.models || []).filter((row) => keys.has(row.group_key));
    const totals = sumTokens(groups);
    const requests = groups.reduce((sum, group) => sum + group.requests, 0);
    const sampleCount = groups.reduce((sum, group) => sum + (group.estimate_samples || 0), 0);
    const inputTotal = totals.input + totals.cache_creation + totals.cache_read;
    const quotaLabel = report.window === "five_hour" ? "5-hour" : "7-day";
    status.textContent = `${localTime(report.from)} – ${localTime(report.to)} · As of ${localTime(report.as_of)} · ${quotaLabel} quota evidence`;
    content.append(el("div", { class: "tiles sv-tiles" }, [
      statTile({ label: `Observed ${metricName(selection.metric).toLowerCase()}`, value: compactNum(totals[selection.metric]), unit: "tok", sub: `${fullNum(requests)} recorded requests · through this proxy` }),
      statTile({ label: "Output delivered", value: compactNum(totals.output), unit: "tok", sub: "Recorded output, not an allowance" }),
      statTile({ label: "Cache-read share", value: inputTotal ? pct(totals.cache_read / inputTotal * 100) : "—", sub: "Cache read ÷ (input + cache write + cache read)" }),
      statTile({ label: "Valid estimate samples", value: fullNum(sampleCount), sub: `${groups.filter((group) => group.estimate != null).length} of ${groups.length} cohorts have quota evidence` }),
    ]));
    if (report.truncated) content.append(el("p", { class: "sv-notice sv-notice--warning", role: "note", text: "Partial report — the server's bounded scan was truncated. Totals and estimates may be incomplete; use a shorter period." }));
    if (!groups.length) {
      content.append(emptyState("No recorded subscription traffic", "Choose a different provider or a longer period. Quota estimates appear only after enough matching observations have been recorded."));
      content.append(methodology(report.notes));
      return;
    }
    content.append(cohortTable(groups, accounts, history, selection.metric));
    const charts = el("div", { class: "sv-charts" });
    content.append(charts);
    trafficChart(charts, report, daily, selection.metric);
    capacityChart(charts, report, groups, history, selection.metric);
    content.append(accountTable(accounts, groups, selection.metric, editTier));
    content.append(modelTable(models, groups, selection.metric), methodology(report.notes));
  }

  function editTier(account) {
    if (!page.isConnected || account.attribution !== "credential" || !account.id) return;
    const input = el("input", { class: "input", type: "text", value: account.tier === "unknown" ? "" : account.tier || "", placeholder: "e.g. 20x or 5x", maxlength: "128", "aria-label": "Subscription tier", "aria-describedby": "sv-tier-help", autocomplete: "off" });
    const error = el("p", { class: "form-err", role: "alert" });
    const help = el("p", { class: "sv-note", id: "sv-tier-help", text: "Use the tier shown by your provider, or leave empty for unknown. This is metadata only: it does not change routing weight or quota. Historical traffic will be regrouped using this current tier; it does not record when a plan changed." });
    let saving = false;
    let closed = false;
    let dialog;
    const save = button("Save tier", { kind: "primary", onClick: submit });
    const cancel = button("Cancel", { onClick: () => dialog.close() });
    const form = el("form", { class: "sv-tier-form", onSubmit: (event) => { event.preventDefault(); submit(); } }, [el("label", { class: "form-row" }, [el("span", { class: "field-label", text: "Subscription tier" }), input]), help, error]);
    dialog = modal({ title: "Edit subscription tier", subtitle: account.name || account.id, body: form, actions: [cancel, save], onClose: () => { closed = true; tierDialog = null; } });
    tierDialog = dialog;
    async function submit() {
      if (saving || closed) return;
      const tier = input.value.trim();
      if (new TextEncoder().encode(tier).length > 128 || /[\p{Cc}]/u.test(tier)) {
        error.textContent = "Use at most 128 UTF-8 bytes, without control characters.";
        input.focus();
        return;
      }
      saving = true;
      save.disabled = true;
      input.disabled = true;
      error.textContent = "";
      try {
        await api.setCredentialTier(account.id, tier);
        if (!page.isConnected) return;
        if (!closed) dialog.close();
        toast("Tier saved. Historical rows use the updated classification.", "good");
        load();
      } catch (e) {
        if (!closed && page.isConnected) error.textContent = e.message;
      } finally {
        saving = false;
        if (!closed) { save.disabled = false; input.disabled = false; }
      }
    }
  }
  load();
}

function selectControl(label, options, value, onChange) {
  const input = el("select", { class: "input", "aria-label": label, onChange: (event) => onChange(event.target.value) }, options.map(([key, name]) => el("option", { value: key, text: name })));
  input.value = value;
  return { input, root: el("label", { class: "sv-filter" }, [el("span", { class: "field-label", text: label }), input]) };
}

function section(title, subtitle, className = "") {
  return el("section", { class: `card sv-section ${className}` }, [el("div", { class: "sv-section__head" }, [el("h2", { text: title }), el("p", { class: "sv-note", text: subtitle })])]);
}

function table(headers, rows, caption) {
  return el("div", { class: "table-scroll", tabindex: "0", role: "region", "aria-label": caption }, [el("table", { class: "table sv-table" }, [
    el("caption", { class: "sr-only", text: caption }),
    el("thead", {}, el("tr", {}, headers.map((header) => el("th", { scope: "col", text: header })))),
    el("tbody", {}, rows),
  ])]);
}
const td = (children, className = "") => el("td", { class: className }, children);
const note = (text) => el("div", { class: "sv-note", text });
function number(value) {
  return el("span", { class: "sv-number", title: fullNum(value) }, [
    el("span", { class: "sv-number__compact", "aria-hidden": "true", text: compactNum(value) }),
    el("span", { class: "sv-number__full", text: fullNum(value) }),
  ]);
}
function tokenBreakdown(tokens) {
  if (!tokens) return note("Not recorded");
  return el("dl", { class: "sv-token-breakdown" }, PARTS.map(([key, label]) => el("div", {}, [el("dt", { text: label }), el("dd", {}, number(tokenValue(tokens, key)))])));
}
function attribution(group) {
  return group.attribution === "gateway" ? "Gateway totals only" : group.attribution === "unknown" ? "Unattributed traffic" : "Credential-attributed";
}
function noEstimate(group, accounts) {
  if (group.attribution === "gateway") return "Gateway traffic cannot be attributed to individual accounts or plans.";
  if (group.attribution === "unknown") return "Credential or plan attribution is unavailable.";
  if (group.provider !== "anthropic") return "Metered only — no matched quota percentage is available.";
  const reasons = [...new Set(accounts.filter((account) => account.group_key === group.key).map((account) => account.current?.reason).filter(Boolean))];
  return reasons.length ? reasons.map(reasonText).join(" · ") : "Not enough aligned quota observations in this period.";
}
function reasonText(reason) {
  const reasons = {
    missing_quota: "No quota reading is available", unobserved_quota: "Latest quota reading was not observed",
    invalid_quota: "Latest quota reading is invalid", invalid_reset: "Latest quota reset is invalid",
    ambiguous_timestamp: "Quota observation time is ambiguous", stale_snapshot: "Latest valid quota reading is stale",
    expired_window: "Last observed quota window has ended", partial_coverage: "Partial window coverage; estimate uses the narrower matched interval",
    "insufficient-samples": "Not enough quota readings", "insufficient-delta": "Less than 10 percentage points observed",
    "insufficient-duration": "Less than 30 minutes of matched evidence", "no-quota": "No quota readings",
    "no-usage-api": "No quota API", "gateway-attribution": "Gateway totals have no account attribution",
    "no-traffic": "No matched traffic", "reset-unknown": "Quota reset is unknown", "stale": "Latest observation is stale",
    "window-expired": "Last observed quota window has ended", "metered-only": "Metered only; no quota percentage",
  };
  return reasons[reason] || reason;
}
function estimateCell(estimate, metric, low, high, samples, reason) {
  if (!estimate) return el("div", { class: "sv-estimate" }, [el("span", { class: "sv-unavailable", text: "Not available" }), note(reason)]);
  return el("div", { class: "sv-estimate" }, [
    el("strong", { class: "sv-number" }, ["≈ ", number(tokenValue(estimate, metric))]),
    low && high ? el("div", { class: "sv-note" }, ["IQR ", number(tokenValue(low, metric)), "–", number(tokenValue(high, metric))]) : null,
    samples != null ? note(`${fullNum(samples)} valid account-window ${samples === 1 ? "sample" : "samples"}`) : null,
  ]);
}

function cohortTable(groups, accounts, history, metric) {
  const card = section("Compare subscription cohorts", "Actual tokens are recorded throughput. The 100%-equivalent is a workload-conditioned estimate per account-window, not a provider allowance or a pool total.");
  const rows = groups.map((group) => {
    const samples = history.filter((sample) => sample.group_key === group.key);
    const days = [...new Set(samples.map((sample) => dayOf(sample.end)))].sort((a, b) => a - b);
    const first = days.length > 1 ? median(samples.filter((sample) => dayOf(sample.end) === days[0]).map((sample) => tokenValue(sample.estimate, metric))) : null;
    const last = days.length > 1 ? median(samples.filter((sample) => dayOf(sample.end) === days.at(-1)).map((sample) => tokenValue(sample.estimate, metric))) : null;
    const change = first > 0 && last != null ? (last / first - 1) * 100 : null;
    return el("tr", {}, [
      el("th", { scope: "row", class: "sv-identity" }, [el("strong", { text: groupLabel(group) }), note(providerName(group)), note(`${group.credentials} ${group.attribution === "gateway" ? "gateway" : "credential"}${group.credentials === 1 ? "" : "s"} · ${attribution(group)}`)]),
      td([number(tokenValue(group.tokens, metric)), note(`${fullNum(group.requests)} requests`)]),
      td(tokenBreakdown(group.tokens)),
      td(estimateCell(group.estimate, metric, group.estimate_low, group.estimate_high, group.estimate_samples, noEstimate(group, accounts))),
      td(change == null ? [note("Not enough history")] : [el("span", { class: "sv-number", text: `${change > 0 ? "+" : ""}${pct(change)}` }), note(`${dateUTC(days[0])} → ${dateUTC(days.at(-1))}`), note("First vs last daily median")]),
    ]);
  });
  card.append(table(["Provider / plan / tier", `Observed · ${metricName(metric)}`, "Recorded token mix", `Median 100%-equivalent · ${metricName(metric)}`, "Historical change"], rows, "Subscription cohort comparison"));
  card.append(el("p", { class: "sv-footnote", text: "Medians and IQR describe the valid samples (IQR = middle 50%). Changes may reflect model mix, caching, or coverage — not a change to the provider's allowance. Current plan/tier metadata classifies the entire history; unknown tiers are never inferred from routing weight." }));
  return card;
}

function accountTable(accounts, groups, metric, editTier) {
  const names = new Map(groups.map((group) => [group.key, group]));
  const card = section("Current-window evidence", "The latest observed quota window per credential. Recorded tokens and quota may have different coverage; an estimate is shown only when readings align. This section is not the selected-period total.");
  if (!accounts.length) { card.append(emptyState("No account evidence", "Recorded cohort traffic remains available above.")); return card; }
  card.append(table(["Credential / tier", "Quota used · as of", "Recorded window tokens", `Window 100%-equivalent · ${metricName(metric)}`, "Evidence / limitations"], accounts.map((account) => {
    const current = account.current;
    // A missing/invalid quota object carries zero-valued placeholders. Only a
    // timestamp proves an observation; an actually observed 0% remains valid.
    const observed = Number(current?.observed_at) > 0;
    const historical = observed && ["unobserved_quota", "invalid_quota", "invalid_reset", "ambiguous_timestamp", "stale_snapshot", "expired_window"].includes(current.reason);
    const group = names.get(account.group_key) || account;
    const editable = account.attribution === "credential" && !!account.id;
    const gateway = account.attribution === "gateway";
    const evidence = current ? [
      note(`${fullNum(current.samples || 0)} readings · ${Number(current.delta_pct || 0).toFixed(1)} pp observed change`),
      note(current.reason ? reasonText(current.reason) : "Matched observations; workload-dependent"),
      current.reset_at ? note(`Reset ${localTime(current.reset_at)}`) : note("Reset unknown"),
    ] : [note(gateway ? "No per-account token attribution. Do not compare these totals with a sidecar account's quota." : noEstimate(group, [account]))];
    return el("tr", {}, [
      el("th", { scope: "row", class: "sv-identity" }, [el("strong", { text: account.name || account.id || "Unknown credential" }), note(groupLabel(group)), editable ? button("Edit tier", { onClick: () => editTier(account) }) : note(gateway ? "Gateway · not a subscription account" : "Historical / unavailable metadata")]),
      td(observed && current.used_pct != null ? [el("strong", { class: "sv-number", text: pct(current.used_pct) }), note(`${historical ? "Last observed " : ""}${localTime(current.observed_at)}`), historical ? note("Current quota unknown") : null] : [note("Not available")]),
      td(observed ? [tokenBreakdown(current.tokens), note(`${localTime(current.start)} → ${localTime(current.observed_at)}`)] : note("No matched quota window")),
      td(estimateCell(observed ? current.estimate : null, metric, null, null, null, current?.reason ? reasonText(current.reason) : "No aligned estimate")),
      td(evidence),
    ]);
  }), "Current quota window evidence by credential"));
  return card;
}

function modelTable(models, groups, metric) {
  const card = section("Model & cache mix", "A different model or cache pattern can change tokens per quota point. These are observed tokens in the selected period, not pricing or quota weights.");
  if (!models.length) { card.append(emptyState("No model accounting", "The report has no model breakdown for this selection.")); return card; }
  const names = new Map(groups.map((group) => [group.key, group]));
  const total = models.reduce((sum, row) => sum + (tokenValue(row.tokens, metric) || 0), 0);
  const rows = models.slice().sort((a, b) => tokenValue(b.tokens, metric) - tokenValue(a.tokens, metric));
  card.append(table(["Model", "Cohort", `Observed · ${metricName(metric)}`, "Metric share", "Cache-read share", "Requests"], rows.map((row) => {
    const input = row.tokens.input + row.tokens.cache_creation + row.tokens.cache_read;
    return el("tr", {}, [el("th", { scope: "row", class: "sv-identity", text: row.model || "Unknown model" }), td(groupLabel(names.get(row.group_key) || {})), td(number(tokenValue(row.tokens, metric))), td(note(total ? pct(tokenValue(row.tokens, metric) / total * 100) : "—")), td(note(input ? pct(row.tokens.cache_read / input * 100) : "—")), td(number(row.requests))]);
  }), "Observed model mix"));
  return card;
}

function methodology(notes) {
  return el("details", { class: "card sv-method", open: true }, [
    el("summary", { text: "How to read this report" }),
    el("div", { class: "sv-method__body" }, [
      el("p", {}, [el("strong", { text: "Estimate, not entitlement. " }), "100%-equivalent tokens = tokens recorded between aligned quota readings × 100 ÷ the observed percentage-point increase."]),
      el("p", { class: "sv-formula", text: "Matched tokens × 100 / Δ quota percentage points" }),
      el("ul", {}, [
        "Requires at least 10 percentage points across at least 30 minutes within the same quota-reset window. Never calculated as whole-period tokens ÷ the latest quota percentage.",
        "Input, output, cache write and cache read are non-overlapping token counters. Their total is throughput, not a fixed unit of quota, monetary value, or an estimate of money saved.",
        "Use outside this proxy consumes quota without appearing in its tokens. Missing response accounting, stale or missing readings, model-specific limits, and different model/cache mixes weaken comparability.",
        "Codex and Gemini are gateway totals only: there is no reliable per-account or per-plan token attribution. Metered-only providers have no quota percentages or 100%-equivalent estimates.",
        "Current plan/tier labels apply retrospectively. An unknown tier stays unknown until an operator supplies it; selection weight never determines subscription tier.",
      ].map((text) => el("li", { text }))),
      ...(notes || []).map((text) => el("p", { class: "sv-note", text })),
    ]),
  ]);
}

function dayBuckets(report) {
  const result = [];
  for (let day = dayOf(report.from); day <= dayOf(report.to); day += DAY) result.push(day);
  return result;
}

function trafficChart(wrap, report, daily, metric) {
  const buckets = dayBuckets(report);
  const byDay = new Map();
  daily.forEach((row) => {
    const rows = byDay.get(row.ts) || [];
    rows.push(row); byDay.set(row.ts, rows);
  });
  const colors = seriesColors();
  const fields = metric === "total" ? PARTS : METRICS.filter(([key]) => key === metric);
  const series = fields.map(([key, label]) => ({ label, color: colors[Math.max(0, PARTS.findIndex(([part]) => part === key))], values: buckets.map((day) => byDay.has(day) ? sumTokens(byDay.get(day))[key] : (report.truncated ? null : 0)) }));
  chart(wrap, {
    title: "Daily observed traffic", eyebrow: "actual throughput", buckets, series,
    description: "UTC-day totals through this proxy. First and last days may be partial. Zero means no recorded tokens; missing data in a truncated report remains a gap.",
  });
}

function capacityChart(wrap, report, groups, history, metric) {
  const buckets = dayBuckets(report);
  // Allocate slots from the complete report, not the filtered/ranked subset.
  // Never recycle colors. Overflow stays in the comparison/evidence tables.
  const all = (report.groups || []).filter((group) => group.attribution === "credential" && group.provider === "anthropic").slice().sort((a, b) => a.key.localeCompare(b.key));
  const slots = new Map(all.slice(0, 6).map((group, index) => [group.key, index]));
  const colors = seriesColors();
  const available = new Set(history.map((sample) => sample.group_key));
  const series = groups.filter((group) => slots.has(group.key) && available.has(group.key)).map((group) => ({
    label: groupLabel(group), color: colors[slots.get(group.key)],
    values: buckets.map((day) => median(history.filter((sample) => sample.group_key === group.key && dayOf(sample.end) === day && sample.estimate).map((sample) => tokenValue(sample.estimate, metric)))),
  }));
  chart(wrap, {
    title: "Quota-equivalent capacity trend", eyebrow: `estimated · ${metricName(metric)}`, buckets, series, points: true,
    description: `Daily median 100%-equivalent per account-window, by evidence end date (UTC). No sample = a gap, never zero or a carried-forward estimate.${all.length > 6 ? " Chart limited to six cohorts; all cohorts remain in the tables." : ""}`,
  });
}

function chart(wrap, { title, eyebrow, description, buckets, series, points }) {
  const frame = chartFrame({ title, eyebrow });
  frame.root.classList.add("sv-chart");
  frame.root.insertBefore(el("p", { class: "sv-note sv-chart__description", text: description }), frame.plot);
  wrap.append(frame.root);
  if (!series.length || !series.some((row) => row.values.some((value) => value != null))) {
    frame.plot.append(emptyState("No aligned estimate samples", "A matching quota window needs at least 10 percentage points and 30 minutes of observed change. Gateway and metered-only traffic cannot produce this estimate."));
    return;
  }
  const ch = timeChart(frame.plot, { buckets, series, mode: "line", height: 250, fmt: (value) => `${fullNum(value)} tok` });
  if (series.length > 1) frame.legendSlot.append(ch.legendEl);
  // Sparse estimates need visible points; otherwise an isolated valid reading
  // surrounded by nulls would be invisible with the shared line-only defaults.
  if (points) {
    const surface = getComputedStyle(frame.root).getPropertyValue("--surface-1").trim();
    ch.u.series.slice(1).forEach((row) => Object.assign(row.points, { show: () => true, size: 8, width: 2, stroke: () => surface, fill: () => row._color }));
    // The constructor has queued its first draw. Do not call redraw() here:
    // it clears uPlot's pending initial axis-layout flag before axes exist.
    // That queued first draw already reads these live point options.
  }
  const readout = el("p", { class: "sv-chart__readout", "aria-live": "polite", text: "Focus chart and use ← / → to inspect days. The data table includes every value." });
  frame.root.append(readout);
  let index = buckets.length - 1;
  const inspect = () => {
    ch.u.setCursor({ left: ch.u.valToPos(buckets[index], "x"), top: 40 });
    readout.textContent = `${dateUTC(buckets[index])} UTC · ${series.map((row) => `${row.label}: ${row.values[index] == null ? "not available" : fullNum(row.values[index])}`).join(" · ")}`;
  };
  frame.plot.tabIndex = 0;
  frame.plot.setAttribute("role", "group");
  frame.plot.setAttribute("aria-label", `${title}. Left and right arrow keys inspect daily values. Home and End jump to first and last day.`);
  frame.plot.addEventListener("focus", inspect);
  frame.plot.addEventListener("keydown", (event) => {
    if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
    event.preventDefault();
    if (event.key === "Home") index = 0;
    else if (event.key === "End") index = buckets.length - 1;
    else index = Math.max(0, Math.min(buckets.length - 1, index + (event.key === "ArrowLeft" ? -1 : 1)));
    inspect();
  });
  const details = el("details", { class: "sv-chart-data" }, [el("summary", { text: "View daily data table" })]);
  // Lazy rendering keeps 90-day multi-cohort reports light until requested.
  details.addEventListener("toggle", () => {
    if (!details.open || details.children.length > 1) return;
    details.append(table(["Day (UTC)", ...series.map((row) => row.label)], buckets.map((day, i) => el("tr", {}, [el("th", { scope: "row", text: dateUTC(day) }), ...series.map((row) => td(el("span", { class: "sv-number", text: row.values[i] == null ? "Not available" : fullNum(row.values[i]) })))])), `${title}: daily values`));
  });
  frame.root.append(details);
}
