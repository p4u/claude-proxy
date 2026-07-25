import { api, conversationExportUrl } from "../api.js";
import {
  el, clear, spinner, errorState, emptyState, statusBadge, toast, modal, confirmDialog, button, copyText,
} from "../ui.js";
import { periodControl, sectionHead, segmented } from "../components.js";
import { getWindow, setWindowPeriod, setWindowCustom } from "../store.js";
import { compactNum, fullNum, ms, relTime, localTime } from "../format.js";

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

const CAPTURE_HELP =
  "Full capture stores both sides of every conversation, including pasted file contents. " +
  "Off (the default) keeps only the last user prompt of each request.";

function buildTable(users, statById, root) {
  const table = el("table", { class: "table" });
  table.append(el("thead", {}, el("tr", {}, [
    th("Name"), th("Status"),
    el("th", { title: CAPTURE_HELP }, [
      el("span", { text: "Full capture" }),
      el("span", { class: "th-info", "aria-hidden": "true", text: "?" }),
    ]),
    th("Requests", "num"), th("Errors", "num"),
    th("Tokens in", "num"), th("Tokens out", "num"), th("Avg latency", "num"),
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
    el("td", { class: "num", text: compactNum(s.requests || 0) }),
    el("td", { class: "num " + errTone, text: fullNum(s.errors || 0) }),
    el("td", { class: "num", text: compactNum(s.tokens_in || 0) }),
    el("td", { class: "num", text: compactNum(s.tokens_out || 0) }),
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
