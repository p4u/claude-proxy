import { api } from "../api.js";
import {
  el, clear, spinner, errorState, emptyState, statusBadge, toast, modal, confirmDialog, button,
} from "../ui.js";
import { sectionHead } from "../components.js";
import { compactNum, relTime, localTime } from "../format.js";

export async function render(root) {
  clear(root);
  const head = sectionHead("Credentials", "Managed subscriptions and API keys in the rotation pool.", [
    button("Add custom host", { onClick: () => addCustomModal(root) }),
    button("Add API key", { onClick: () => addKeyModal(root) }),
    button("Add credential", { kind: "primary", onClick: () => addModal(root) }),
  ]);
  const body = el("div", { class: "card table-card" }, spinner("Loading credentials…"));
  root.append(head, body);

  try {
    const [rows] = await Promise.all([
      api.credentials(),
      loadEndpoints(),
    ]);
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
      th("Label"), th("Provider"), th("Endpoint"), th("Type"), th("Status"), th("Weight"), th("Requests", "num"),
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

// Endpoint presets, fetched once from /credentials/endpoints. The registry in
// Go stays the single source of truth; this is only a cache.
let ENDPOINTS = {};
async function loadEndpoints() {
  try {
    ENDPOINTS = (await api.endpoints()) || {};
  } catch {
    ENDPOINTS = {}; // presets unavailable → the custom URL field still works
  }
}

// endpointPicker builds a preset <select> plus a free-text field revealed by
// the "Custom…" option, and returns { wrap, value() }. Presets cover the common
// case; the text field means a cluster the proxy has never heard of is still
// reachable without a rebuild.
const CUSTOM = "__custom";
function endpointPicker(providerId, current) {
  const sel = el("select", { class: "input" });
  const custom = el("input", {
    class: "input", type: "text", spellcheck: "false",
    placeholder: "https://your-cluster.example.com/anthropic",
  });
  const customRow = el("div", { class: "form-row" }, [
    el("label", { class: "field-label", text: "Custom URL" }), custom,
  ]);

  const fill = (pid) => {
    clear(sel);
    for (const e of (ENDPOINTS[pid]?.endpoints) || []) {
      sel.append(el("option", { value: e.name, text: e.desc || e.name }));
    }
    sel.append(el("option", { value: CUSTOM, text: "Custom…" }));
    // Preselect the credential's current endpoint: a known preset by name,
    // otherwise the custom field pre-filled with its URL.
    if (current?.name) sel.value = current.name;
    else if (current?.url) { sel.value = CUSTOM; custom.value = current.url; }
    sync();
  };
  const sync = () => { customRow.hidden = sel.value !== CUSTOM; };
  sel.addEventListener("change", sync);
  fill(providerId);

  return {
    select: sel,
    customRow,
    setProvider: fill,
    value: () => (sel.value === CUSTOM ? custom.value.trim() : sel.value),
  };
}

// endpointLabel keeps the column narrow: a preset name when the URL matches
// one, otherwise the custom URL's host.
function endpointLabel(c) {
  if (!c.endpoint) return "—";
  if (c.endpoint_name) return c.endpoint_name;
  try {
    return new URL(c.endpoint).host;
  } catch {
    return c.endpoint;
  }
}

// endpointModal moves an existing key to another cluster. The backend
// re-verifies the key there before committing, so a wrong pick is rejected
// rather than silently breaking a working credential.
function endpointModal(c, root) {
  const id = c.id || c.credential_id;
  const ep = endpointPicker(c.provider, { name: c.endpoint_name, url: c.endpoint });
  const err = el("p", { class: "form-err", role: "alert" });
  const body = el("div", { class: "form" }, [
    el("div", { class: "form-row" }, [el("label", { class: "field-label", text: "Endpoint" }), ep.select]),
    ep.customRow,
    err,
  ]);
  const m = modal({
    title: "Change endpoint",
    subtitle: `The key is re-verified against the new endpoint before ${c.label || id} is moved.`,
    body,
    actions: [
      button("Cancel", { onClick: () => m.close() }),
      button("Verify & move", {
        kind: "primary",
        onClick: async (ev) => {
          const btn = ev.currentTarget;
          err.textContent = "";
          const value = ep.value();
          if (!value) { err.textContent = "Pick a preset or enter a URL."; return; }
          btn.disabled = true;
          const orig = btn.textContent;
          btn.textContent = "Verifying…";
          try {
            await api.post(`/credentials/${id}/endpoint`, { endpoint: value });
            m.close();
            toast("Endpoint updated");
            render(root);
          } catch (e) {
            err.textContent = e.message || "Could not change the endpoint.";
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

// The badge style capitalizes its text, which turns "glm" into "Glm". Spell
// out the display form instead of fighting the CSS; unknown providers fall
// back to the raw id so a new one is still legible before this map is updated.
const PROVIDER_LABELS = { anthropic: "Anthropic", glm: "GLM", mimo: "MiMo", custom: "Custom" };
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
    ...(c.endpoint_editable ? [
      button("Endpoint", { onClick: () => endpointModal(c, root) }),
    ] : []),
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
    // Preset name when it matches one, else the bare host of a custom URL —
    // the full URL is long enough to wreck the table layout.
    el("td", { text: endpointLabel(c), title: c.endpoint || "" }),
    el("td", { text: c.subscription_type || "—" }),
    el("td", {}, statusBadge(c.status)),
    el("td", { class: "num", text: String(c.weight ?? "—") }),
    el("td", { class: "num", text: compactNum(c.request_count ?? c.requests ?? 0) }),
    el("td", { text: c.last_request_at ? relTime(tsOf(c.last_request_at)) : "never" }),
    el("td", { text: isKey ? "never" : (c.expires_at ? localTime(tsOf(c.expires_at)) : "—") }),
    el("td", { class: "actions" }, actions),
  ]);
}

// addCustomModal adds any Anthropic-compatible endpoint.
//
// The host is probed before it can be saved, and the probe fills in everything
// it can discover — the model list above all, since a translation shim often
// answers as a name the operator would not have guessed. Discovered values are
// editable: the probe is a starting point, not a verdict.
function addCustomModal(root) {
  const url = el("input", { class: "input", type: "text", spellcheck: "false",
    placeholder: "http://10.0.0.5:3456 or https://host/anthropic" });
  const key = el("input", { class: "input", type: "password", spellcheck: "false",
    placeholder: "API key (blank if the host needs none)" });
  const label = el("input", { class: "input", type: "text", placeholder: "defaults to the host name" });
  const models = el("input", { class: "input", type: "text", spellcheck: "false",
    placeholder: "discovered automatically — comma-separated to override" });
  const weight = el("input", { class: "input input--sm", type: "number", min: "0", placeholder: "auto" });
  const report = el("div", { class: "probe" });
  const err = el("p", { class: "form-err", role: "alert" });

  const body = el("div", { class: "form" }, [
    el("div", { class: "form-row" }, [el("label", { class: "field-label", text: "Base URL" }), url]),
    el("div", { class: "form-row" }, [el("label", { class: "field-label", text: "API key" }), key]),
    el("div", { class: "form-grid" }, [
      el("div", { class: "form-row" }, [el("label", { class: "field-label", text: "Label" }), label]),
      el("div", { class: "form-row" }, [el("label", { class: "field-label", text: "Weight" }), weight]),
    ]),
    el("div", { class: "form-row" }, [el("label", { class: "field-label", text: "Models" }), models]),
    report,
    err,
  ]);

  const yn = (b) => (b ? "yes" : "no");
  const renderProbe = (p) => {
    clear(report);
    const line = (k, v, tone) => el("div", { class: "probe__row" }, [
      el("span", { class: "probe__k", text: k }),
      el("span", { class: "probe__v" + (tone ? ` probe__v--${tone}` : ""), text: v }),
    ]);
    report.append(
      line("reachable", yn(p.ok), p.ok ? "good" : "bad"),
      line("auth enforced", yn(p.auth_required), p.auth_required ? "good" : "warn"),
      line("/v1/models", yn(p.has_models_api)),
      line("count_tokens", yn(p.has_count_tokens)),
    );
    if (p.reported_model) report.append(line("reports model", p.reported_model));
    if (p.error) report.append(line("error", p.error, "bad"));
    // Context window is only knowable from a /v1/models the host may not
    // serve; say so rather than leaving a silent blank.
    for (const m of p.models || []) {
      report.append(line("model", m.id + (m.context_window ? ` · ${m.context_window} ctx` : " · context unknown")));
    }
    if ((p.models || []).length && !models.value.trim()) {
      models.value = (p.models || []).map((m) => m.id).join(", ");
    }
  };

  const parseModels = () =>
    models.value.split(",").map((v) => v.trim()).filter(Boolean)
      .map((id) => ({ id, display_name: id }));

  const m = modal({
    title: "Add custom Anthropic API host",
    subtitle: "Any endpoint speaking the Anthropic Messages API. Probe first — the model list and capabilities are discovered for you.",
    wide: true,
    body,
    actions: [
      button("Cancel", { onClick: () => m.close() }),
      button("Probe", {
        onClick: async (ev) => {
          const btn = ev.currentTarget;
          err.textContent = "";
          if (!url.value.trim()) { err.textContent = "Enter the base URL first."; return; }
          btn.disabled = true;
          const orig = btn.textContent;
          btn.textContent = "Probing…";
          try {
            renderProbe(await api.probeHost({ base_url: url.value.trim(), api_key: key.value.trim() }));
          } catch (e) {
            err.textContent = e.message || "Probe failed.";
          } finally {
            btn.disabled = false;
            btn.textContent = orig;
          }
        },
      }),
      button("Add host", {
        kind: "primary",
        onClick: async (ev) => {
          const btn = ev.currentTarget;
          err.textContent = "";
          if (!url.value.trim()) { err.textContent = "Enter the base URL first."; return; }
          btn.disabled = true;
          const orig = btn.textContent;
          btn.textContent = "Verifying…";
          try {
            await api.addCustom({
              base_url: url.value.trim(),
              api_key: key.value.trim(),
              label: label.value.trim(),
              models: parseModels(),
              weight: Number(weight.value) || 0,
            });
            m.close();
            toast("Custom host added");
            render(root);
          } catch (e) {
            err.textContent = e.message || "Could not add the host.";
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

// addKeyModal collects a static API key. Kept separate from addModal, which
// takes a pasted .credentials.json — an API-key provider has no analogue of it.
function addKeyModal(root) {
  const provider = el("select", { class: "input" }, [
    el("option", { value: "glm", text: "Z.AI GLM" }),
    el("option", { value: "mimo", text: "Xiaomi MiMo" }),
  ]);
  const ep = endpointPicker(provider.value, null);
  provider.addEventListener("change", () => ep.setProvider(provider.value));
  const key = el("input", { class: "input", type: "password", placeholder: "API key", spellcheck: "false" });
  const label = el("input", { class: "input", type: "text", placeholder: "e.g. zai-main" });
  const plan = el("input", { class: "input", type: "text", placeholder: "lite | pro | max" });
  const weight = el("input", { class: "input input--sm", type: "number", min: "0", placeholder: "auto" });
  const err = el("p", { class: "form-err", role: "alert" });

  const body = el("div", { class: "form" }, [
    el("div", { class: "form-grid" }, [
      el("div", { class: "form-row" }, [el("label", { class: "field-label", text: "Provider" }), provider]),
      el("div", { class: "form-row" }, [el("label", { class: "field-label", text: "Endpoint" }), ep.select]),
      ep.customRow,
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
              endpoint: ep.value(),
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
