import { api, conversationExportUrl } from "../api.js";
import {
  el, clear, spinner, errorState, emptyState, statusBadge, toast, modal, confirmDialog, button, copyText,
} from "../ui.js";
import { periodControl, sectionHead, segmented, meter } from "../components.js";
import { getWindow, setWindowPeriod, setWindowCustom } from "../store.js";
import { compactNum, fullNum, ms, relTime, localTime, pct } from "../format.js";

export async function render(root) {
  clear(root);
  const win = getWindow();
  const head = sectionHead("Users", "Per-user bearer tokens and their traffic attribution.", [
    periodControl(win, (sel) => {
      if (sel.mode === "custom") setWindowCustom(sel.from, sel.to);
      else setWindowPeriod(sel.period);
      render(root);
    }),
    button("Create user", { kind: "primary", onClick: () => createModal(root) }),
  ]);
  const body = el("div", { class: "card table-card" }, spinner("Loading users…"));
  root.append(head, body);

  try {
    const [users, stats] = await Promise.all([api.users(), api.statsUsers(win).catch(() => [])]);
    clear(body);
    if (!users || !users.length) {
      body.append(emptyState("No users yet", 'Create a user to mint a bearer token. Each request it makes is attributed here.'));
      return;
    }
    const statById = new Map((stats || []).map((s) => [s.id, s]));
    body.append(buildTable(users, statById, root));
  } catch (e) {
    clear(body).append(errorState(e.message, () => render(root)));
  }
}

const CACHE_READ_HELP =
  "Cache-read tokens over the selected period. Usually the largest number here by far, " +
  "and deliberately NOT counted towards the usage limit.";

const CAPTURE_HELP =
  "Full capture stores both sides of every conversation, including pasted file contents. " +
  "Off (the default) keeps only the last user prompt of each request.";

const LIMIT_METRIC = "Counts output tokens only — input and cache tokens are ignored.";
const LIMIT_HELP =
  "Blocks a user once their output tokens over a rolling window exceed the cap. " +
  LIMIT_METRIC + " No limit by default.";

// Window presets, matching the period vocabulary used elsewhere in the app.
const LIMIT_WINDOWS = [
  { value: 3600, label: "1H", short: "1h" },
  { value: 21600, label: "6H", short: "6h" },
  { value: 86400, label: "24H", short: "24h" },
  { value: 604800, label: "7D", short: "7d" },
];
const DEFAULT_WINDOW = 86400;

function windowShort(sec) {
  const w = LIMIT_WINDOWS.find((x) => x.value === Number(sec));
  if (w) return w.short;
  const s = Number(sec) || 0;
  if (s % 86400 === 0) return s / 86400 + "d";
  if (s % 3600 === 0) return s / 3600 + "h";
  return Math.max(0, Math.round(s / 60)) + "m";
}

// A limit is active only when BOTH the token cap and the window are > 0.
function hasLimit(u) {
  return Number(u.limit_output_tokens) > 0 && Number(u.limit_window_seconds) > 0;
}

// "1M" / "500k" / "1.5M" / "1,000,000" / "1000000" → integer output tokens.
// Returns {value} or {error}. Empty input is an error, not a silent zero.
export function parseTokens(raw) {
  const s = String(raw == null ? "" : raw).trim().replace(/[,_\s]/g, "");
  if (!s) return { error: "Enter a number of output tokens, e.g. 1M." };
  const m = /^(-?\d+(?:\.\d+)?)([kmgKMG]?)$/.exec(s);
  if (!m) return { error: "Use a plain number or a K/M/G shorthand, e.g. 1M or 500K." };
  const mult = { k: 1e3, m: 1e6, g: 1e9 }[m[2].toLowerCase()] || 1;
  const n = Number(m[1]) * mult;
  if (!isFinite(n)) return { error: "That number is too large." };
  if (n < 0) return { error: "The cap can't be negative." };
  return { value: Math.round(n) };
}

// Prefill text for the token input: shorthand when it round-trips exactly.
function tokensToInput(n) {
  const v = Number(n) || 0;
  if (v <= 0) return "";
  for (const [div, sfx] of [[1e9, "G"], [1e6, "M"], [1e3, "K"]]) {
    if (v >= div && v % div === 0) return v / div + sfx;
  }
  return String(v);
}

// HH:MM (local) for a blocked_until instant.
function clockTime(v) {
  const ts = tsOf(v);
  if (!ts) return "";
  return new Date(ts * 1000).toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
}

// Usage-limit cell. Reuses the Subscriptions utilization meter verbatim so the
// two read as one idiom. Rolling windows have no reset instant, so nothing is
// rendered below the bar unless the user is actually blocked.
function limitCell(u) {
  if (!hasLimit(u)) {
    return el("span", {
      class: "limit-none",
      text: "no limit",
      title: "This user is unlimited — the default. Use Limit to cap their usage.",
    });
  }
  const cap = Number(u.limit_output_tokens) || 0;
  const used = Number(u.usage_output_tokens) || 0;
  const rawPct = u.usage_pct != null ? Number(u.usage_pct) : cap ? (used / cap) * 100 : 0;
  const blocked = !!u.blocked;
  const tone = blocked || rawPct >= 100 ? "critical" : rawPct >= 80 ? "warning" : "good";
  const until = blocked ? clockTime(u.blocked_until) : "";
  const label = `${compactNum(used)} / ${compactNum(cap)} output`;
  const bar = meter({
    label,
    value: rawPct,
    tone,
    segments: 14,
    resets: blocked ? (until ? `blocked until ${until}` : "blocked") : null,
  });
  // The shared meter clamps its bar (and therefore its own readout) at 100%.
  // Over the cap, the true overshoot is the interesting number, so restate it —
  // the bar stays pinned full, which is the correct visual.
  if (rawPct > 100) {
    const pctEl = bar.querySelector(".meter__pct");
    if (pctEl) pctEl.textContent = pct(rawPct);
    const barEl = bar.querySelector(".meter__bar");
    if (barEl) barEl.setAttribute("aria-label", `${label}, ${pct(rawPct)} of the limit`);
  }
  const wrap = el("div", { class: "limit-cell" + (blocked ? " is-blocked" : "") }, [
    bar,
    el("span", {
      class: "limit-cell__win",
      text: `per ${windowShort(u.limit_window_seconds)}`,
    }),
  ]);
  wrap.title =
    `${fullNum(used)} of ${fullNum(cap)} output tokens used in the last ${windowShort(u.limit_window_seconds)}` +
    (blocked && until ? ` — blocked until ${until}` : "");
  return wrap;
}

function buildTable(users, statById, root) {
  const table = el("table", { class: "table" });
  table.append(el("thead", {}, el("tr", {}, [
    th("Name"), th("Status"),
    el("th", { title: CAPTURE_HELP }, [
      el("span", { text: "Full capture" }),
      el("span", { class: "th-info", "aria-hidden": "true", text: "?" }),
    ]),
    el("th", { class: "limit-col", title: LIMIT_HELP }, [
      el("span", { text: "Usage limit" }),
      el("span", { class: "th-info", "aria-hidden": "true", text: "?" }),
    ]),
    th("Requests", "num"), th("Errors", "num"),
    th("Tokens in", "num"), th("Tokens out", "num"),
    el("th", { class: "num", title: CACHE_READ_HELP }, [
      el("span", { text: "Cache read" }),
      el("span", { class: "th-info", "aria-hidden": "true", text: "?" }),
    ]),
    th("Avg latency", "num"),
    th("Last used"), th("", "actions"),
  ])));
  const tb = el("tbody");
  for (const u of users) tb.append(userRow(u, statById.get(u.id) || {}, root));
  table.append(tb);
  return el("div", {}, [
    el("div", { class: "table-scroll" }, table),
    el("p", { class: "table-note" }, [
      el("strong", { text: "Full capture is off by default. " }),
      el("span", { text: "Leaving it off keeps only the last user prompt per request. Turning it on stores both sides of every conversation for that user, including pasted file contents. Everything is purged on the same retention schedule." }),
    ]),
  ]);
}

function th(label, cls) {
  return el("th", { class: cls || null, text: label });
}

function userRow(u, s, root) {
  const disabled = (u.status || "").toLowerCase() === "disabled";
  const errTone = (s.errors || 0) > 0 ? "cell-warn" : "";
  const actions = el("div", { class: "row-actions" }, [
    button("Prompts", { onClick: () => promptsModal(u) }),
    button("Limit", { onClick: () => limitModal(u, root), title: LIMIT_HELP }),
    button("Rotate", {
      onClick: async () => {
        const ok = await confirmDialog({
          title: "Rotate token?",
          message: `The current token for "${u.name}" stops working immediately. A new one is shown once.`,
          confirmLabel: "Rotate", danger: false,
        });
        if (!ok) return;
        try {
          const r = await api.post(`/users/${u.id}/rotate`);
          tokenReveal(u.name, r.token, "rotated");
          render(root);
        } catch (e) {
          toast(e.message, "critical", 6000);
        }
      },
    }),
    button(disabled ? "Enable" : "Disable", {
      onClick: () => act(() => api.post(`/users/${u.id}/${disabled ? "enable" : "disable"}`), disabled ? "Enabled" : "Disabled", root),
    }),
    button("Delete", {
      kind: "danger-ghost",
      onClick: async () => {
        const ok = await confirmDialog({
          title: "Delete user?",
          message: `"${u.name}" and its token will be removed. Requests already logged stay in history.`,
          confirmLabel: "Delete",
        });
        if (ok) act(() => api.del(`/users/${u.id}`), "User deleted", root);
      },
    }),
  ]);
  return el("tr", {}, [
    el("td", {}, [el("span", { class: "cell-strong", text: u.name }), el("span", { class: "cell-id", text: u.id })]),
    el("td", {}, statusBadge(u.status)),
    el("td", {}, captureToggle(u)),
    el("td", { class: "limit-col" }, limitCell(u)),
    el("td", { class: "num", text: compactNum(s.requests || 0) }),
    el("td", { class: "num " + errTone, text: fullNum(s.errors || 0) }),
    el("td", { class: "num", text: compactNum(s.tokens_in || 0) }),
    el("td", { class: "num", text: compactNum(s.tokens_out || 0) }),
    el("td", {
      class: "num cell-muted",
      text: compactNum(s.cache_read || 0),
      title: `${fullNum(s.cache_read || 0)} cache-read tokens — not counted towards the usage limit`,
    }),
    el("td", { class: "num", text: s.avg_latency_ms ? ms(s.avg_latency_ms) : "—" }),
    el("td", { text: u.last_used_at ? relTime(tsOf(u.last_used_at)) : "never" }),
    el("td", { class: "actions" }, actions),
  ]);
}

function tsOf(v) {
  if (v == null) return 0;
  if (typeof v === "number") return v;
  const t = Date.parse(v);
  return isNaN(t) ? 0 : t / 1000;
}

async function act(fn, okMsg, root) {
  try {
    await fn();
    toast(okMsg, "good");
    render(root);
  } catch (e) {
    toast(e.message, "critical", 6000);
  }
}

function createModal(root) {
  const name = el("input", { class: "input", type: "text", placeholder: "e.g. alice", autocomplete: "off" });
  const m = modal({
    title: "Create user",
    subtitle: "Mints a named bearer token, shown once.",
    body: el("div", { class: "form-row" }, [el("label", { class: "field-label", text: "Name" }), name]),
    actions: [
      button("Cancel", { onClick: () => m.close() }),
      button("Create", {
        kind: "primary",
        onClick: async () => {
          const n = name.value.trim();
          if (!n) return toast("Give the user a name", "warning");
          try {
            const r = await api.post("/users", { name: n });
            m.close();
            tokenReveal(r.name || n, r.token, "created");
            render(root);
          } catch (e) {
            toast(e.message, "critical", 6000);
          }
        },
      }),
    ],
  });
}

// Edit the rolling usage limit for one user.
// The cap accepts K/M/G shorthand and echoes the parsed value back so the
// operator never has to guess what was understood. Crucially it also shows what
// this user ACTUALLY produces over the selected window, refetched whenever the
// window changes: without that number a cap is a blind guess. Only the two
// client-side rules are enforced here (unparseable input, negatives) —
// everything else, including the backend's both-or-neither rule, is surfaced
// inline from the server's 400.
function limitModal(u, root) {
  const active = hasLimit(u);
  const err = el("p", { class: "form-err", role: "alert" });
  const echo = el("p", { class: "limit-echo" });
  const usageLine = el("p", { class: "limit-usage", "aria-live": "polite" });

  const capInput = el("input", {
    class: "input",
    type: "text",
    inputmode: "decimal",
    autocomplete: "off",
    spellcheck: "false",
    id: "limit-output-tokens",
    placeholder: "e.g. 1M",
    value: active ? tokensToInput(u.limit_output_tokens) : "",
  });

  let windowSec = active ? Number(u.limit_window_seconds) || DEFAULT_WINDOW : DEFAULT_WINDOW;
  const windowSeg = segmented(
    LIMIT_WINDOWS.map((w) => ({ value: String(w.value), label: w.label })),
    String(windowSec),
    (v) => {
      windowSec = Number(v);
      clearErr();
      paintEcho();
      loadUsage();
    },
    "Rolling window"
  );

  const clearErr = () => {
    err.textContent = "";
  };
  const showErr = (msg) => {
    err.textContent = msg;
  };
  const paintEcho = () => {
    const raw = capInput.value.trim();
    if (!raw) {
      echo.textContent = "";
      return;
    }
    const p = parseTokens(raw);
    echo.textContent = p.error
      ? ""
      : `= ${fullNum(p.value)} output tokens per ${windowShort(windowSec)}`;
  };
  capInput.addEventListener("input", () => {
    clearErr();
    paintEcho();
  });
  paintEcho();

  // Current usage for the selected window. `seq` drops out-of-order responses
  // when the operator flips presets faster than the requests come back.
  let seq = 0;
  async function loadUsage() {
    const mine = ++seq;
    const win = windowSec;
    usageLine.classList.remove("is-error");
    usageLine.textContent = `Checking usage over the last ${windowShort(win)}…`;
    try {
      const r = await api.userWindowUsage(u.id, win);
      if (mine !== seq) return;
      const used = Number(r && r.output_tokens) || 0;
      usageLine.textContent =
        `${u.name} used ${compactNum(used)} output tokens in the last ${windowShort(win)}` +
        ` (${fullNum(used)}). Pick a cap above that if you only want to stop runaways.`;
    } catch (e) {
      if (mine !== seq) return;
      usageLine.classList.add("is-error");
      usageLine.textContent = `Couldn't read current usage: ${e.message}`;
    }
  }
  loadUsage();

  const save = async () => {
    const p = parseTokens(capInput.value);
    if (p.error) return showErr(p.error);
    await submit(p.value, windowSec, `Limit set for ${u.name}`);
  };

  async function submit(outputTokens, windowSeconds, okMsg) {
    clearErr();
    saveBtn.disabled = true;
    clearBtn.disabled = true;
    try {
      const r = await api.setUserLimit(u.id, { outputTokens, windowSeconds });
      u.limit_output_tokens =
        r && r.limit_output_tokens != null ? r.limit_output_tokens : outputTokens;
      u.limit_window_seconds = r && r.limit_window_seconds != null ? r.limit_window_seconds : windowSeconds;
      m.close();
      toast(okMsg, "good");
      render(root);
    } catch (e) {
      // Inline, not just a toast: the message explains what to change.
      showErr(e.message);
      capInput.focus();
    } finally {
      saveBtn.disabled = false;
      clearBtn.disabled = false;
    }
  }

  const saveBtn = button("Save limit", { kind: "primary", onClick: save });
  const clearBtn = button("No limit", {
    kind: "danger-ghost",
    disabled: !active,
    title: active ? "Remove the cap — this user becomes unlimited" : "This user is already unlimited",
    onClick: () => submit(0, 0, `${u.name} is now unlimited`),
  });

  capInput.addEventListener("keydown", (e) => {
    if (e.key === "Enter") {
      e.preventDefault();
      save();
    }
  });

  const body = el("div", { class: "form limit-form" }, [
    el("div", { class: "form-row" }, [
      el("label", { class: "field-label", for: "limit-output-tokens", text: "Output tokens" }),
      capInput,
      echo,
    ]),
    el("div", { class: "form-row" }, [
      el("span", { class: "field-label", text: "Rolling window" }),
      windowSeg,
      usageLine,
      el("p", {
        class: "limit-hint",
        text: "Usage is summed over the trailing window. There is no reset moment — the oldest usage simply ages out.",
      }),
    ]),
    el("p", { class: "limit-formula", text: LIMIT_METRIC }),
    err,
  ]);

  const m = modal({
    title: `Usage limit · ${u.name}`,
    subtitle: active
      ? `Currently ${compactNum(u.limit_output_tokens)} output tokens per ${windowShort(u.limit_window_seconds)} — ` +
        `${compactNum(u.usage_output_tokens || 0)} used so far` +
        (u.blocked ? `, blocked until ${clockTime(u.blocked_until) || "the window clears"}.` : ".")
      : "This user has no limit. Set one to cap the output tokens they can produce.",
    body,
    actions: [button("Cancel", { onClick: () => m.close() }), clearBtn, saveBtn],
  });
  requestAnimationFrame(() => capInput.focus());
}

// Per-user capture switch. Optimistic: flip immediately, revert on failure.
function captureToggle(u) {
  const input = el("input", {
    type: "checkbox",
    class: "switch__input",
    role: "switch",
    checked: !!u.full_capture,
    "aria-label": `Full conversation capture for ${u.name}`,
    "aria-checked": u.full_capture ? "true" : "false",
  });
  const state = el("span", { class: "switch__state", text: u.full_capture ? "On" : "Off" });
  const wrap = el("label", { class: "switch", title: CAPTURE_HELP }, [
    input,
    el("span", { class: "switch__track", "aria-hidden": "true" }, el("span", { class: "switch__thumb" })),
    state,
  ]);
  const paint = (on) => {
    input.checked = on;
    input.setAttribute("aria-checked", on ? "true" : "false");
    state.textContent = on ? "On" : "Off";
    wrap.classList.toggle("is-on", on);
  };
  paint(!!u.full_capture);
  input.addEventListener("change", async () => {
    const want = input.checked;
    paint(want);
    input.disabled = true;
    wrap.classList.add("is-busy");
    try {
      const r = await api.setUserCapture(u.id, want);
      const applied = r && typeof r.full_capture === "boolean" ? r.full_capture : want;
      u.full_capture = applied;
      paint(applied);
      toast(
        applied
          ? `Full capture on for ${u.name} — both sides of every conversation are now stored.`
          : `Full capture off for ${u.name} — only the last prompt per request is stored.`,
        applied ? "warning" : "good"
      );
    } catch (e) {
      paint(!want); // revert to the last known-good value
      toast(`Couldn't change capture mode: ${e.message}`, "critical", 6000);
    } finally {
      input.disabled = false;
      wrap.classList.remove("is-busy");
    }
  });
  return wrap;
}

const PROMPT_LIMIT = 25;
const CONV_LIMIT = 20;
const MSG_LIMIT = 20;

// Short, human-scannable conversation id. Full id stays in the title attribute.
function convShort(id) {
  const s = String(id || "").replace(/^conv[_-]/, "");
  return s.length > 10 ? s.slice(0, 8) : s || "—";
}

// Prev / range / next. onPage(newOffset).
function pager(total, limit, offset, onPage) {
  const lim = limit > 0 ? limit : 1;
  const from = total ? offset + 1 : 0;
  const to = Math.min(offset + lim, total);
  return el("div", { class: "pager" }, [
    button("← Prev", { onClick: () => onPage(Math.max(0, offset - lim)), disabled: offset <= 0 }),
    el("span", {
      class: "pager__range",
      text: total ? `showing ${from}–${to} of ${total}` : "nothing to show",
    }),
    button("Next →", { onClick: () => onPage(offset + lim), disabled: offset + lim >= total }),
  ]);
}

// Per-user capture browser: Prompts tab (every stored prompt row, paginated)
// and Conversations tab (list → paginated message viewer).
// All prompt/message content is written with textContent, never innerHTML.
function promptsModal(u) {
  const panel = el("div", { class: "cap__panel" });
  const tabs = segmented(
    [{ value: "prompts", label: "Prompts" }, { value: "conversations", label: "Conversations" }],
    "prompts",
    (v) => (v === "prompts" ? showPrompts(0) : showConvs(0)),
    "Capture view"
  );
  const m = modal({
    title: `Captured activity · ${u.name}`,
    subtitle: u.full_capture
      ? "Full capture is on: both sides of every conversation are stored, until the retention window purges them."
      : "Only the last user prompt of each request is stored. Enable full capture to record assistant replies too.",
    wide: true,
    body: el("div", { class: "cap" }, [el("div", { class: "cap__tabs" }, tabs), panel]),
    actions: [button("Close", { kind: "ghost", onClick: () => m.close() })],
  });
  m.dialog.classList.add("modal--xwide");

  async function load(fn, retry) {
    clear(panel).append(spinner("Loading…"));
    try {
      return await fn();
    } catch (e) {
      clear(panel).append(errorState(e.message, retry));
      return null;
    }
  }

  async function showPrompts(offset) {
    const r = await load(() => api.userPrompts(u.id, { limit: PROMPT_LIMIT, offset }), () => showPrompts(offset));
    if (!r) return;
    const items = r.items || [];
    clear(panel);
    if (!r.total) {
      panel.append(emptyState("No prompts recorded", "Prompts are kept for a limited retention window, then purged. Nothing stored for this user."));
      return;
    }
    const list = el("div", { class: "prompts" });
    for (const p of items) list.append(promptItem(p));
    panel.append(list, pager(r.total, r.limit || PROMPT_LIMIT, r.offset || 0, showPrompts));
  }

  async function showConvs(offset) {
    const r = await load(() => api.userConversations(u.id, { limit: CONV_LIMIT, offset }), () => showConvs(offset));
    if (!r) return;
    const items = r.items || [];
    clear(panel);
    if (!r.total) {
      panel.append(emptyState("No conversations recorded", "Conversations appear once this user routes a request and capture is enabled."));
      return;
    }
    const list = el("div", { class: "convs" });
    for (const c of items) list.append(convRow(c, () => showMessages(c, 0, offset)));
    panel.append(list, pager(r.total, r.limit || CONV_LIMIT, r.offset || 0, showConvs));
  }

  async function showMessages(conv, offset, backOffset) {
    const r = await load(
      () => api.conversationMessages(conv.conv_id, { limit: MSG_LIMIT, offset }),
      () => showMessages(conv, offset, backOffset)
    );
    if (!r) return;
    clear(panel);
    const back = button("← All conversations", { onClick: () => showConvs(backOffset || 0) });
    const dl = el("a", {
      class: "btn btn--ghost",
      href: conversationExportUrl(conv.conv_id),
      download: `conversation-${convShort(conv.conv_id)}.md`,
      text: "Download .md",
    });
    const source = r.source || conv.source || "prompts";
    panel.append(
      el("div", { class: "cap__bar" }, [
        back,
        el("span", { class: "cap__bar-id", text: convShort(conv.conv_id), title: conv.conv_id }),
        sourceBadge(source),
        el("span", { class: "cap__bar-spacer" }),
        dl,
      ])
    );
    if (source === "prompts") {
      panel.append(
        el("p", {
          class: "cap__note",
          text: "Assistant replies were not captured for this conversation — only the user prompts below were stored.",
        })
      );
    }
    const items = r.items || [];
    if (!items.length) {
      panel.append(emptyState("Nothing stored for this conversation", "It may have been purged by the retention janitor."));
      return;
    }
    const list = el("div", { class: "msgs" });
    for (const msgRow of items) list.append(messageItem(msgRow));
    panel.append(list, pager(r.total || items.length, r.limit || MSG_LIMIT, r.offset || 0, (o) => showMessages(conv, o, backOffset)));
    requestAnimationFrame(() => back.focus());
  }

  showPrompts(0);
}

function sourceBadge(source) {
  const full = source === "full";
  return el("span", { class: "badge badge--" + (full ? "good" : "muted"), title: full ? "Both sides of the conversation were captured" : "Only user prompts were captured" }, [
    el("span", { class: "badge__dot" }),
    el("span", { text: full ? "full" : "prompts" }),
  ]);
}

function convRow(c, onOpen) {
  const msgs = c.messages || 0;
  const prompts = c.prompts || 0;
  const meta = [
    msgs ? `${fullNum(msgs)} message${msgs === 1 ? "" : "s"}` : null,
    prompts ? `${fullNum(prompts)} prompt${prompts === 1 ? "" : "s"}` : null,
  ].filter(Boolean).join(" · ") || "no stored turns";
  return el("button", { class: "conv", type: "button", onClick: onOpen, title: c.conv_id }, [
    el("span", { class: "conv__main" }, [
      el("span", { class: "conv__id", text: convShort(c.conv_id) }),
      sourceBadge(c.source),
      c.model ? el("span", { class: "prompt__model", text: c.model }) : null,
    ]),
    el("span", { class: "conv__meta" }, [
      el("span", { class: "conv__counts", text: meta }),
      el("span", { class: "conv__time", text: c.last_ts ? localTime(tsOf(c.last_ts)) : "—" }),
    ]),
  ]);
}

function promptItem(p) {
  const head = el("div", { class: "prompt__head" }, [
    el("span", { class: "prompt__time", text: localTime(tsOf(p.ts)) }),
    p.model ? el("span", { class: "prompt__model", text: p.model }) : null,
    p.conv_id ? el("span", { class: "prompt__conv", text: convShort(p.conv_id), title: p.conv_id }) : null,
  ]);
  // textContent (not innerHTML) — the prompt is untrusted user input.
  const bodyEl = el("pre", { class: "prompt__text" });
  bodyEl.textContent = p.prompt || "";
  return el("div", { class: "prompt" }, [head, bodyEl]);
}

function messageItem(msg) {
  const assistant = (msg.role || "").toLowerCase() === "assistant";
  const head = el("div", { class: "msg__head" }, [
    el("span", { class: "msg__role", text: assistant ? "Assistant" : "User" }),
    msg.seq != null ? el("span", { class: "msg__seq", text: "#" + (Number(msg.seq) + 1) }) : null,
    el("span", { class: "msg__time", text: msg.ts ? localTime(tsOf(msg.ts)) : "" }),
    msg.model ? el("span", { class: "prompt__model", text: msg.model }) : null,
  ]);
  // textContent (not innerHTML) — message bodies are untrusted and may contain
  // markup, code fences, or anything else a user pasted.
  const bodyEl = el("pre", { class: "msg__text" });
  bodyEl.textContent = msg.content || "";
  return el("div", { class: "msg msg--" + (assistant ? "assistant" : "user") }, [head, bodyEl]);
}

function tokenReveal(name, token, verb) {
  const field = el("code", { class: "token-reveal", text: token || "—" });
  const copyBtn = button("Copy", {
    kind: "primary",
    onClick: async () => {
      if (await copyText(token)) {
        copyBtn.textContent = "Copied";
        setTimeout(() => (copyBtn.textContent = "Copy"), 1600);
      } else {
        toast("Copy failed — select the token manually", "warning");
      }
    },
  });
  const m = modal({
    title: `Token ${verb}`,
    subtitle: `Copy this now — it won't be shown again. Use it as the Bearer token for ${name}.`,
    body: el("div", { class: "token-box" }, [field, copyBtn]),
    actions: [button("Done", { kind: "ghost", onClick: () => m.close() })],
  });
}
