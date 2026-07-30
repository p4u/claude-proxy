import { api } from "../api.js";
import {
  el, clear, spinner, errorState, emptyState, statusBadge, toast, modal, confirmDialog, button,
} from "../ui.js";
import { sectionHead } from "../components.js";
import { compactNum, relTime, localTime } from "../format.js";

export async function render(root) {
  clear(root);
  const head = sectionHead("Credentials", "Managed subscriptions and API keys in the rotation pool.", [
    button("Add API key", { onClick: () => addKeyModal(root) }),
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
      th("Label"), th("Provider"), th("Type"), th("Status"), th("Weight"), th("Requests", "num"),
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

// The badge style capitalizes its text, which turns "glm" into "Glm". Spell
// out the display form instead of fighting the CSS; unknown providers fall
// back to the raw id so a new one is still legible before this map is updated.
const PROVIDER_LABELS = { anthropic: "Anthropic", glm: "GLM" };
function providerLabel(id) {
  return PROVIDER_LABELS[id] || id || "Anthropic";
}

function credRow(c, root) {
  const id = c.id || c.credential_id;
  const disabled = (c.status || "").toLowerCase() === "disabled";
  // A static API key has no OAuth lineage: there is nothing to refresh and no
  // .credentials.json to re-paste, so those two actions are omitted rather
  // than offered and then failing.
  const isKey = c.has_usage_api === false;
  const actions = el("div", { class: "row-actions" }, [
    button("Weight", { onClick: () => weightModal(id, c.weight, root) }),
    ...(isKey ? [] : [
      button("Refresh", { onClick: () => act(() => api.post(`/credentials/${id}/refresh`), "Token refreshed", root) }),
      button("Update tokens", { onClick: () => updateModal(id, root) }),
    ]),
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
    el("td", {}, el("span", { class: "badge", text: providerLabel(c.provider) })),
    el("td", { text: c.subscription_type || "—" }),
    el("td", {}, statusBadge(c.status)),
    el("td", { class: "num", text: String(c.weight ?? "—") }),
    el("td", { class: "num", text: compactNum(c.request_count ?? c.requests ?? 0) }),
    el("td", { text: c.last_request_at ? relTime(tsOf(c.last_request_at)) : "never" }),
    el("td", { text: isKey ? "never" : (c.expires_at ? localTime(tsOf(c.expires_at)) : "—") }),
    el("td", { class: "actions" }, actions),
  ]);
}

// addKeyModal collects a static API key. Kept separate from addModal, which
// takes a pasted .credentials.json — an API-key provider has no analogue of it.
function addKeyModal(root) {
  const provider = el("select", { class: "input" }, [
    el("option", { value: "glm", text: "Z.AI GLM" }),
  ]);
  const key = el("input", { class: "input", type: "password", placeholder: "API key", spellcheck: "false" });
  const label = el("input", { class: "input", type: "text", placeholder: "e.g. zai-main" });
  const plan = el("input", { class: "input", type: "text", placeholder: "lite | pro | max" });
  const weight = el("input", { class: "input input--sm", type: "number", min: "0", placeholder: "auto" });
  const err = el("p", { class: "form-err", role: "alert" });

  const body = el("div", { class: "form" }, [
    el("div", { class: "form-grid" }, [
      el("div", { class: "form-row" }, [el("label", { class: "field-label", text: "Provider" }), provider]),
      el("div", { class: "form-row" }, [el("label", { class: "field-label", text: "Label (optional)" }), label]),
      el("div", { class: "form-row" }, [el("label", { class: "field-label", text: "Plan (optional)" }), plan]),
      el("div", { class: "form-row" }, [el("label", { class: "field-label", text: "Weight (optional)" }), weight]),
    ]),
    el("div", { class: "form-row" }, [el("label", { class: "field-label", text: "API key" }), key]),
    err,
  ]);

  const m = modal({
    title: "Add API key",
    subtitle: "The key is verified against the provider before it enters the pool.",
    body,
    actions: [
      button("Cancel", { onClick: () => m.close() }),
      button("Verify & add", {
        kind: "primary",
        onClick: async (ev) => {
          const btn = ev.currentTarget;
          err.textContent = "";
          if (!key.value.trim()) { err.textContent = "Paste an API key first."; return; }
          btn.disabled = true;
          const orig = btn.textContent;
          btn.textContent = "Verifying…";
          try {
            await api.addKey({
              provider: provider.value,
              api_key: key.value.trim(),
              label: label.value.trim(),
              plan: plan.value.trim(),
              weight: Number(weight.value) || 0,
            });
            m.close();
            toast("API key added");
            render(root);
          } catch (e) {
            err.textContent = e.message || "Could not add the key.";
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
