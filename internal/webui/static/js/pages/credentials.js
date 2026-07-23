import { api } from "../api.js";
import {
  el, clear, spinner, errorState, emptyState, statusBadge, toast, modal, confirmDialog, button,
} from "../ui.js";
import { sectionHead } from "../components.js";
import { compactNum, relTime, localTime } from "../format.js";

export async function render(root) {
  clear(root);
  const head = sectionHead("Credentials", "Managed OAuth subscriptions in the rotation pool.", [
    button("Add credential", { kind: "primary", onClick: () => addModal(root) }),
  ]);
  const body = el("div", { class: "card table-card" }, spinner("Loading credentials…"));
  root.append(head, body);

  try {
    const rows = await api.credentials();
    clear(body);
    if (!rows || !rows.length) {
      body.append(emptyState("No credentials yet", 'Click "Add credential" and paste a .credentials.json to bring a subscription into the pool.'));
      return;
    }
    body.append(buildTable(rows, root));
  } catch (e) {
    clear(body).append(errorState(e.message, () => render(root)));
  }
}

function buildTable(rows, root) {
  const table = el("table", { class: "table" });
  table.append(
    el("thead", {}, el("tr", {}, [
      th("Label"), th("Type"), th("Status"), th("Weight"), th("Requests", "num"),
      th("Last used"), th("Expires"), th("", "actions"),
    ]))
  );
  const tb = el("tbody");
  for (const c of rows) tb.append(credRow(c, root));
  table.append(tb);
  return el("div", { class: "table-scroll" }, table);
}

function th(label, cls) {
  return el("th", { class: cls || null, text: label });
}

function credRow(c, root) {
  const id = c.id || c.credential_id;
  const disabled = (c.status || "").toLowerCase() === "disabled";
  const actions = el("div", { class: "row-actions" }, [
    button("Weight", { onClick: () => weightModal(id, c.weight, root) }),
    button("Refresh", { onClick: () => act(() => api.post(`/credentials/${id}/refresh`), "Token refreshed", root) }),
    button("Update tokens", { onClick: () => updateModal(id, root) }),
    button(disabled ? "Enable" : "Disable", {
      onClick: () => act(() => api.post(`/credentials/${id}/${disabled ? "enable" : "disable"}`), disabled ? "Enabled" : "Disabled", root),
    }),
    button("Delete", {
      kind: "danger-ghost",
      onClick: async () => {
        const ok = await confirmDialog({
          title: "Delete credential?",
          message: `"${c.label || id}" will be removed from the pool and its conversation bindings cleared. This can't be undone.`,
          confirmLabel: "Delete",
        });
        if (ok) act(() => api.del(`/credentials/${id}`), "Credential deleted", root);
      },
    }),
  ]);
  return el("tr", {}, [
    el("td", {}, [el("span", { class: "cell-strong", text: c.label || "—" }), el("span", { class: "cell-id", text: id })]),
    el("td", { text: c.subscription_type || "—" }),
    el("td", {}, statusBadge(c.status)),
    el("td", { class: "num", text: String(c.weight ?? "—") }),
    el("td", { class: "num", text: compactNum(c.request_count ?? c.requests ?? 0) }),
    el("td", { text: c.last_used_at ? relTime(tsOf(c.last_used_at)) : "never" }),
    el("td", { text: c.expires_at ? localTime(tsOf(c.expires_at)) : "—" }),
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

function weightModal(id, current, root) {
  const input = el("input", { class: "input", type: "number", min: "0", step: "1", value: String(current ?? 1) });
  const m = modal({
    title: "Set selection weight",
    subtitle: "Higher weight → more traffic. Pro tiers default to 1, max/team/enterprise to 5.",
    body: el("div", { class: "form-row" }, [el("label", { class: "field-label", text: "Weight" }), input]),
    actions: [
      button("Cancel", { onClick: () => m.close() }),
      button("Save", {
        kind: "primary",
        onClick: async () => {
          const w = parseInt(input.value, 10);
          if (isNaN(w) || w < 0) return toast("Enter a non-negative integer", "warning");
          m.close();
          act(() => api.post(`/credentials/${id}/weight`, { weight: w }), "Weight updated", root);
        },
      }),
    ],
  });
}

function jsonModal({ title, subtitle, submitLabel, onSubmit, extraFields }) {
  const ta = el("textarea", {
    class: "input input--code", rows: "10", spellcheck: "false",
    placeholder: '{\n  "claudeAiOauth": {\n    "accessToken": "...",\n    "refreshToken": "...",\n    "expiresAt": ...\n  }\n}',
  });
  const err = el("p", { class: "form-err", role: "alert" });
  const body = el("div", { class: "form" }, [
    ...(extraFields || []),
    el("div", { class: "form-row" }, [el("label", { class: "field-label", text: "credentials.json" }), ta]),
    err,
  ]);
  const m = modal({
    title, subtitle, wide: true, body,
    actions: [
      button("Cancel", { onClick: () => m.close() }),
      button(submitLabel, {
        kind: "primary",
        onClick: async (ev) => {
          const btn = ev.currentTarget;
          const raw = ta.value.trim();
          err.textContent = "";
          if (!raw) { err.textContent = "Paste a credentials JSON first."; return; }
          try {
            JSON.parse(raw);
          } catch {
            err.textContent = "That isn't valid JSON.";
            return;
          }
          btn.disabled = true;
          const orig = btn.textContent;
          btn.textContent = "Verifying…";
          try {
            await onSubmit(raw, m);
          } catch (e) {
            err.textContent = e.message || "Import failed.";
          } finally {
            btn.disabled = false;
            btn.textContent = orig;
          }
        },
      }),
    ],
  });
  return m;
}

function addModal(root) {
  const label = el("input", { class: "input", type: "text", placeholder: "e.g. team-account-2" });
  const weight = el("input", { class: "input input--sm", type: "number", min: "0", placeholder: "auto" });
  jsonModal({
    title: "Add credential",
    subtitle: "Paste a fresh .credentials.json. Liveness is verified; duplicates are rejected.",
    submitLabel: "Import",
    extraFields: [
      el("div", { class: "form-grid" }, [
        el("div", { class: "form-row" }, [el("label", { class: "field-label", text: "Label (optional)" }), label]),
        el("div", { class: "form-row" }, [el("label", { class: "field-label", text: "Weight (optional)" }), weight]),
      ]),
    ],
    onSubmit: async (raw, m) => {
      const payload = { credentials_json: raw };
      if (label.value.trim()) payload.label = label.value.trim();
      const w = parseInt(weight.value, 10);
      if (!isNaN(w)) payload.weight = w;
      await api.post("/credentials", payload);
      m.close();
      toast("Credential imported", "good");
      render(root);
    },
  });
}

function updateModal(id, root) {
  jsonModal({
    title: "Update tokens",
    subtitle: "Replace this credential's tokens from a fresh login file. Identity and weight are kept.",
    submitLabel: "Update",
    onSubmit: async (raw, m) => {
      await api.put(`/credentials/${id}/tokens`, { credentials_json: raw });
      m.close();
      toast("Tokens updated", "good");
      render(root);
    },
  });
}
