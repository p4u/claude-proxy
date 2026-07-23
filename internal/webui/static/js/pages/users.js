import { api } from "../api.js";
import {
  el, clear, spinner, errorState, emptyState, statusBadge, toast, modal, confirmDialog, button, copyText,
} from "../ui.js";
import { segmented, sectionHead } from "../components.js";
import { getPeriod, setPeriod, PERIODS } from "../store.js";
import { compactNum, fullNum, ms, relTime } from "../format.js";

export async function render(root) {
  clear(root);
  const period = getPeriod();
  const head = sectionHead("Users", "Per-user bearer tokens and their traffic attribution.", [
    segmented(PERIODS, period, (p) => { setPeriod(p); render(root); }, "Stats period"),
    button("Create user", { kind: "primary", onClick: () => createModal(root) }),
  ]);
  const body = el("div", { class: "card table-card" }, spinner("Loading users…"));
  root.append(head, body);

  try {
    const [users, stats] = await Promise.all([api.users(), api.statsUsers(period).catch(() => [])]);
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

function buildTable(users, statById, root) {
  const table = el("table", { class: "table" });
  table.append(el("thead", {}, el("tr", {}, [
    th("Name"), th("Status"), th("Requests", "num"), th("Errors", "num"),
    th("Tokens in", "num"), th("Tokens out", "num"), th("Avg latency", "num"),
    th("Last used"), th("", "actions"),
  ])));
  const tb = el("tbody");
  for (const u of users) tb.append(userRow(u, statById.get(u.id) || {}, root));
  table.append(tb);
  return el("div", { class: "table-scroll" }, table);
}

function th(label, cls) {
  return el("th", { class: cls || null, text: label });
}

function userRow(u, s, root) {
  const disabled = (u.status || "").toLowerCase() === "disabled";
  const errTone = (s.errors || 0) > 0 ? "cell-warn" : "";
  const actions = el("div", { class: "row-actions" }, [
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
