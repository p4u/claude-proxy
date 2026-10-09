import { api } from "../api.js";
import { el, clear, spinner, errorState, emptyState, statusBadge, button } from "../ui.js";
import { providerInfo, providerMark, groupProviders } from "../providers.js";
import { meter, chartFrame, periodControl, segmented, sectionHead } from "../components.js";
import { getWindow, setWindowPeriod, setWindowCustom } from "../store.js";
import { timeChart } from "../charts.js";
import { pct, relTime, countdown, compactNum } from "../format.js";

export async function render(root) {
  clear(root);
  let rows = [];
  let selected = "all";
  let scopedLabel = "";
  const refresh = button("Refresh snapshots", { onClick: load });
  const head = sectionHead("Subscriptions", "Your upstream capacity, organized by provider. Quota windows are independent: either one reaching 100% can block requests.", [refresh]);
  const overview = el("div", { class: "subscription-overview" });
  const filters = el("div", { class: "provider-filters", role: "group", "aria-label": "Filter subscriptions by provider" });
  const cards = el("div", { class: "provider-sections" }, spinner("Reading utilization…"));
  const chartsWrap = el("div", { class: "usage-charts" });
  root.append(head, overview, filters, cards, chartsWrap);

  function draw() {
    const groups = groupProviders(rows);
    clear(overview);
    const ready = rows.filter((r) => r.status === "active" && !r.selection?.saturated).length;
    for (const [label, value] of [["Connected accounts", rows.length], ["Available", ready], ["Providers", groups.length]]) {
      overview.append(el("div", { class: "subscription-overview__item" }, [
        el("strong", { text: String(value) }), el("span", { text: label }),
      ]));
    }
    overview.append(el("p", { class: "subscription-overview__note", text: "Selection shares are within each provider, not across the whole pool." }));
    clear(filters);
    const filterButton = (id, name, count) => el("button", {
      type: "button", class: "provider-filter" + (selected === id ? " is-active" : ""),
      "aria-pressed": String(selected === id),
      onClick: () => { selected = id; draw(); [...filters.children].find((node) => node.dataset.provider === id)?.focus(); },
      dataset: { provider: id },
    }, [id !== "all" ? providerMark(id) : null, el("span", { text: name }), el("span", { class: "provider-filter__count", text: String(count) })]);
    filters.append(filterButton("all", "All providers", rows.length));
    for (const [id, list] of groups) filters.append(filterButton(id, providerInfo(id).name, list.length));
    clear(cards);
    if (!rows.length) {
      cards.append(emptyState("No subscriptions yet", "Add a credential to see its quota or observed usage here."));
      return;
    }
    for (const [id, list] of groups) {
      if (selected !== "all" && selected !== id) continue;
      const p = providerInfo(id);
      const headingID = `subscriptions-${id}`;
      const available = list.filter((r) => r.status === "active" && !r.selection?.saturated).length;
      cards.append(el("section", { class: "provider-section", style: `--provider-color:${p.color}`, "aria-labelledby": headingID }, [
        el("div", { class: "provider-section__head" }, [
          providerMark(id, "lg"),
          el("div", { class: "provider-section__identity" }, [
            el("h2", { id: headingID, text: p.name }),
            el("p", { text: p.description }),
          ]),
          el("span", { class: "provider-section__count", text: `${available} / ${list.length} available` }),
        ]),
        el("div", { class: "subscription-grid" }, list.map(subCard)),
      ]));
    }
  }

  async function load() {
    refresh.disabled = true;
    refresh.textContent = "Refreshing…";
    try {
      const result = await api.usageCurrent();
      if (!cards.isConnected) return;
      rows = result || [];
      if (selected !== "all" && !rows.some((r) => (r.provider || "anthropic") === selected)) selected = "all";
      scopedLabel = rows.find((r) => r.seven_day_scoped?.label)?.seven_day_scoped.label || "";
      draw();
      renderCharts(chartsWrap, scopedLabel);
    } catch (e) {
      if (cards.isConnected) clear(cards).append(errorState(e.message, load));
    } finally {
      refresh.disabled = false;
      refresh.textContent = "Refresh snapshots";
    }
  }
  await load();
}

// Charts section: a scoped period picker driving the History + Selection charts.
function renderCharts(wrap, scopedLabel) {
  clear(wrap);
  const win = getWindow();
  const picker = el("div", { class: "usage-charts__head" }, [
    el("div", {}, [
      el("div", { class: "eyebrow", text: "time window" }),
      el("p", { class: "usage-charts__note", text: "Scopes the history and selection charts below. Cards above always reflect the latest snapshot." }),
    ]),
    periodControl(win, (sel) => {
      if (sel.mode === "custom") setWindowCustom(sel.from, sel.to);
      else setWindowPeriod(sel.period);
      renderCharts(wrap, scopedLabel);
    }),
  ]);
  const grid = el("div", { class: "grid grid--charts" });
  wrap.append(picker, grid);
  historyChart(grid, win, scopedLabel);
  selectionChart(grid, win);
}

// meteredRows renders observed token usage in the same slot the utilization
// meters occupy, so the card reads consistently across providers.
function meteredRows(m) {
  const w = (label, d) => {
    d = d || {};
    const total = (d.input_tokens || 0) + (d.output_tokens || 0) +
      (d.cache_read_tokens || 0) + (d.cache_creation_tokens || 0);
    return el("div", { class: "metered" }, [
      el("div", { class: "meter__top" }, [
        el("span", { class: "meter__label", text: label }),
        el("span", { class: "meter__pct", text: compactNum(total) + " tok" }),
      ]),
      el("div", { class: "metered__break", text:
        `${compactNum(d.input_tokens || 0)} in · ${compactNum(d.output_tokens || 0)} out · ` +
        `${compactNum(d.cache_read_tokens || 0)} cache · ${compactNum(d.requests || 0)} req` }),
    ]);
  };
  return el("div", { class: "sub-card__meters" }, [
    w("5-hour window", m?.five_hour),
    w("7-day window", m?.seven_day),
  ]);
}

// unreportedRow takes a meter's slot for a window the upstream sent no reading
// for. Same label and value position, no bar: a bar would imply a measurement.
function unreportedRow(label, note) {
  return el("div", { class: "metered" }, [
    el("div", { class: "meter__top" }, [
      el("span", { class: "meter__label", text: label }),
      el("span", { class: "meter__pct", text: "not reported" }),
    ]),
    el("div", { class: "metered__break", text: note }),
  ]);
}

function subCard(r) {
  const five = r.five_hour || {};
  const seven = r.seven_day || {};
  const scoped = r.seven_day_scoped || null;
  const sel = r.selection || null;
  const noUsageAPI = r.has_usage_api === false;
  return el("article", { class: "card sub-card" + (r.status === "disabled" ? " sub-card--disabled" : ""), "aria-label": r.label || r.credential_id }, [
    el("div", { class: "sub-card__head" }, [
      el("div", { class: "sub-card__identity" }, [
        el("h3", { class: "sub-card__name", text: r.label || r.credential_id }),
        el("div", { class: "sub-card__meta", text: [r.subscription_type, `weight ${r.weight}`].filter(Boolean).join(" · ") }),
      ]),
      el("div", { class: "sub-card__badges" }, [
        sel && sel.saturated ? el("span", { class: "badge badge--critical", title: "Excluded from new sessions" }, [
          el("span", { class: "badge__dot" }), el("span", { text: "saturated" }),
        ]) : null,
        statusBadge(r.status),
      ]),
    ]),
    // Providers with no utilization API get metered rows instead of meters:
    // same layout and position, but the value is what this proxy actually
    // observed, and there is no bar — there is no published allowance to fill
    // against, and inventing one would read as authoritative when it is not.
    noUsageAPI
      ? meteredRows(r.metered)
      : el("div", { class: "sub-card__meters" }, quotaMeters(r, five, seven, scoped)),
    sel ? selectionRow(sel) : null,
    el("div", { class: "sub-card__foot", text: noUsageAPI
      ? "metered by this proxy — no quota API upstream"
      : (r.captured_at ? `snapshot ${relTime(tsOf(r.captured_at))}` : "no snapshot yet") }),
  ]);
}

// quotaMeters lists the utilization windows the upstream actually enforces.
// Anthropic publishes 5h, 7d and, on plans that have one, a weekly cap scoped
// to one model (`seven_day_scoped`, labelled by the API — "Fable" today; the
// per-model seven_day_sonnet bucket it replaced is always null now). OpenAI
// Codex publishes a 5-hour and a weekly limit — and not every plan has both:
// "prolite" has no 5-hour limit, which the server marks `absent`, so that
// meter is not drawn.
// A Codex window with a null reset is a different case: the account has been
// idle since its last reset, the server has rolled the reading over to 0%,
// and the next reset is only learned from the next request through it.
// Before the first request there are no signals at all; then both are drawn
// empty so the card has the same shape it will have once traffic arrives.
function quotaMeters(r, five, seven, scoped) {
  // Google Gemini (Antigravity) has one quota bucket shared by every Gemini
  // model, read from Google's fetchAvailableModels; the server carries it in
  // the five_hour slot. Google does not name the window, so neither do we.
  if (r.provider === "gemini") {
    const resets = five.resets_at ? countdown(five.resets_at) : (r.captured_at ? "full, no reset pending" : "—");
    return [meter({ label: "Gemini quota", value: five.pct, resets })];
  }
  if (r.provider !== "codex") {
    // A window Anthropic returned as null (`unreported`) has no reading at
    // all — Team seats publish no general weekly limit — so it gets a row with
    // no bar instead of a meter stuck at 0%. The model-scoped cap is not
    // substituted for it: it only counts that model's traffic.
    const sevenNote = scoped
      ? `no general weekly limit published · only the ${scoped.label || "model"} cap below applies`
      : "no general weekly limit published";
    return [
      five.unreported
        ? unreportedRow("5-hour window", "not published for this plan")
        : meter({ label: "5-hour window", value: five.pct, resets: countdown(five.resets_at) }),
      seven.unreported
        ? unreportedRow("7-day window", sevenNote)
        : meter({ label: "7-day window", value: seven.pct, resets: countdown(seven.resets_at) }),
      scoped ? meter({ label: `7-day ${scoped.label || "model"}`, value: scoped.pct, resets: countdown(scoped.resets_at) }) : null,
    ].filter(Boolean);
  }
  const resets = (w) => w.resets_at ? countdown(w.resets_at) : (r.captured_at ? "reset, unused since" : "—");
  const out = [];
  if (!five.absent) out.push(meter({ label: "5-hour limit", value: five.pct, resets: resets(five) }));
  if (!seven.absent) out.push(meter({ label: "Weekly limit", value: seven.pct, resets: resets(seven) }));
  return out;
}

// Selection metric row: pool share + effective score
// (weight × room_5h × room_7d^1.5 × (1 + urgency)). The score shown is the one
// the pool actually weighs, so the share matches routing. When the 7-day
// urgency term is doing real work — unspent weekly allowance about to reset —
// the multiplier is called out, since it is what pulls a low-usage credential
// ahead of an equally fresh one.
function selectionRow(sel) {
  const share = sel.share_pct == null ? "—" : pct(sel.share_pct, sel.share_pct >= 10 ? 0 : 1);
  const saturated = !!sel.saturated;
  const urgency = Number(sel.urgency) || 0;
  const scoreText = sel.score == null ? null
    : `score ${sel.score.toFixed(2)}` + (urgency >= 0.05 ? ` · ×${(1 + urgency).toFixed(1)} 7d reset soon` : "");
  return el("div", { class: "sub-card__sel" + (saturated ? " sub-card__sel--sat" : "") }, [
    el("span", { class: "sub-card__sel-lbl", text: saturated ? "excluded from new sessions" : "selection share" }),
    el("span", { class: "sub-card__sel-val", text: saturated ? "0%" : share }),
    scoreText ? el("span", { class: "sub-card__sel-score", text: scoreText,
      title: "weight × room_5h × room_7d^1.5 × (1 + urgency); urgency = room_7d ÷ remaining share of the 7-day window − 1 (0 when on pace)" }) : null,
  ]);
}

function tsOf(v) {
  if (typeof v === "number") return v;
  const t = Date.parse(v);
  return isNaN(t) ? 0 : t / 1000;
}

// History chart — consumes the aligned-grid shape:
// {buckets:[ts...], series:[{credential_id,label,five_hour_pct:[...], ...}]}.
// One value per bucket per series, null where a credential has no snapshot;
// uPlot renders the gaps natively.
async function historyChart(wrap, win, scopedLabel) {
  let metric = "five_hour_pct";
  let requestID = 0;
  const metricOpts = [
    { value: "five_hour_pct", label: "5-hour" },
    { value: "seven_day_pct", label: "7-day" },
  ];
  if (scopedLabel) metricOpts.push({ value: "seven_day_scoped_pct", label: `7-day ${scopedLabel}` });
  const frame = chartFrame({
    eyebrow: "history",
    title: "Utilization history",
    toolbar: [segmented(metricOpts, metric, (m) => { metric = m; draw(); }, "Metric")],
  });
  frame.root.classList.add("chart--wide");
  wrap.append(frame.root);

  async function draw() {
    const request = ++requestID;
    clear(frame.plot);
    clear(frame.legendSlot);
    frame.plot.append(spinner());
    try {
      const d = await api.usageHistory(win);
      if (!frame.root.isConnected || request !== requestID) return;
      clear(frame.plot);
      const buckets = d.buckets || [];
      const seriesRaw = d.series || [];
      const hasData = buckets.length && seriesRaw.some((s) => (s[metric] || []).some((v) => v != null));
      if (!hasData) {
        frame.plot.append(emptyState("No history yet", "The poller records utilization every 10 minutes — check back after the next cycle."));
        return;
      }
      const series = seriesRaw.map((s) => ({
        label: s.label || s.credential_id,
        values: s[metric] || [],
      }));
      const ch = timeChart(frame.plot, {
        buckets, series, mode: "line", fmt: (v) => pct(v), height: 260,
        yRange: [0, 100],
      });
      markCeiling(frame.plot);
      frame.legendSlot.append(ch.legendEl);
    } catch (e) {
      if (frame.root.isConnected && request === requestID) clear(frame.plot).append(errorState(e.message, draw));
    }
  }
  draw();
}

// Selection chart — how often each credential was picked for NEW conversations,
// stacked over time. Legend labels carry each credential's total share_pct.
async function selectionChart(wrap, win) {
  const frame = chartFrame({ eyebrow: "selection", title: "New-session picks by credential" });
  frame.root.classList.add("chart--wide");
  wrap.append(frame.root);

  async function draw() {
    clear(frame.plot);
    clear(frame.legendSlot);
    frame.plot.append(spinner());
    try {
      const d = await api.statsSelection(win, 60);
      if (!frame.root.isConnected) return;
      clear(frame.plot);
      const buckets = d.buckets || [];
      const seriesRaw = d.series || [];
      const hasData = buckets.length && seriesRaw.some((s) => (s.picks || []).some((v) => v));
      if (!hasData) {
        frame.plot.append(emptyState("No picks yet", "Selection is recorded when a new conversation binds to a credential."));
        return;
      }
      const shareById = new Map((d.totals || []).map((t) => [t.credential_id, t.share_pct]));
      const series = seriesRaw.map((s) => {
        const share = shareById.get(s.credential_id);
        const base = s.label || s.credential_id;
        return {
          label: share == null ? base : `${base} · ${pct(share, share >= 10 ? 0 : 1)}`,
          values: s.picks || [],
        };
      });
      const ch = timeChart(frame.plot, { buckets, series, mode: "stack", fmt: (v) => String(v), height: 260 });
      frame.legendSlot.append(ch.legendEl);
    } catch (e) {
      if (frame.root.isConnected) clear(frame.plot).append(errorState(e.message, draw));
    }
  }
  draw();
}

// Draw a dashed 100% ceiling line + label over the plot area.
function markCeiling(container) {
  // The history scale is fixed at [0, 100], so the ceiling is the plot's top.
  container.querySelector(".u-over")?.append(el("div", { class: "ceiling", text: "100% ceiling", style: "top:0" }));
}
