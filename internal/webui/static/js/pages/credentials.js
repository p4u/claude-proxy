import { api } from "../api.js";
import {
  el, clear, spinner, errorState, emptyState, statusBadge, toast, modal, confirmDialog, button,
} from "../ui.js";
import { sectionHead } from "../components.js";
import { compactNum, relTime, localTime } from "../format.js";

export async function render(root) {
  clear(root);
  const head = sectionHead("Credentials", "Managed subscriptions and API keys in the rotation pool.", [
    button("Add credential", { kind: "primary", onClick: () => addCredentialModal(root) }),
  ]);
  const body = el("div", { class: "card table-card" }, spinner("Loading credentials…"));
  root.append(head, body);

  try {
    const [rows] = await Promise.all([api.credentials(), loadEndpoints()]);
    clear(body);
    if (!rows || !rows.length) {
      body.append(emptyState(
        "No credentials yet",
        'Click "Add credential" to bring an Anthropic subscription, a provider API key, or any Anthropic-compatible host into the pool.',
      ));
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

// ---------------------------------------------------------------------------
// Provider metadata
// ---------------------------------------------------------------------------

// Endpoint presets, fetched once from /credentials/endpoints. The Go registry
// stays the single source of truth; this is only a cache.
let ENDPOINTS = {};
async function loadEndpoints() {
  try {
    ENDPOINTS = (await api.endpoints()) || {};
  } catch {
    ENDPOINTS = {}; // presets unavailable → the endpoint field still accepts a URL
  }
}

// The badge style capitalizes its text, which turns "glm" into "Glm". Spell out
// the display form instead of fighting the CSS; unknown providers fall back to
// the raw id so a new one stays legible before this map is updated.
const PROVIDER_LABELS = { anthropic: "Anthropic", glm: "GLM", mimo: "MiMo", custom: "Custom" };
function providerLabel(id) {
  return PROVIDER_LABELS[id] || id || "Anthropic";
}

// What each credential kind needs from the operator, and what to tell them
// about it. Keeping the copy here rather than inline keeps the form builder
// readable and the guidance consistent between the add and edit modals.
const KINDS = {
  anthropic: {
    label: "Anthropic subscription",
    blurb: "A Pro/Max/Team/Enterprise login. The proxy keeps its token refreshed for you.",
    needsJSON: true,
  },
  glm: {
    label: "Z.AI GLM",
    blurb: "A GLM coding-plan API key.",
    keyHelp: "From z.ai → API Keys. Verified against the endpoint below before it is stored.",
    endpointHelp: "Pick the cluster your key belongs to, or type any other URL.",
    plan: true,
  },
  mimo: {
    label: "Xiaomi MiMo",
    blurb: "A MiMo token-plan API key.",
    keyHelp: "Token Plan keys start with tp-. Verified against the endpoint below before it is stored.",
    endpointHelp:
      "A Token Plan key works on exactly one cluster — the others answer “Invalid API Key” without saying why. Type a URL directly for a cluster not listed.",
    plan: true,
  },
  custom: {
    label: "Custom Anthropic-compatible host",
    blurb: "Any endpoint speaking the Anthropic Messages API — a self-hosted model, a gateway, another vendor.",
    keyHelp: "Leave blank if the host needs no key.",
    endpointHelp: "Base URL of the host, e.g. http://10.0.0.5:3456 or https://host/anthropic.",
    models: true,
    freeEndpoint: true,
  },
};

// ---------------------------------------------------------------------------
// Form building blocks
// ---------------------------------------------------------------------------

let uid = 0;

// field wraps a control with its label and an optional help line. The help text
// is the point of this refactor: every input says what it is for and what goes
// wrong if it is off, instead of assuming the operator already knows.
function field(labelText, control, help) {
  return el("div", { class: "form-row" }, [
    el("label", { class: "field-label", text: labelText }),
    control,
    el("p", { class: "field-help", text: help || "" }),
  ]);
}

function setHelp(row, text) {
  const p = row.querySelector(".field-help");
  if (p) p.textContent = text || "";
}

// endpointInput is a free-text URL box backed by native suggestions.
//
// Deliberately an <input list> rather than a <select> plus a separate "custom"
// field: presets are a shortcut, not a constraint, so one control both offers
// them and accepts anything else. No mode switch, and the effective value is
// always visible and editable.
function endpointInput(kind, value) {
  const listID = `ep-${++uid}`;
  const input = el("input", {
    class: "input", type: "text", spellcheck: "false", list: listID,
    placeholder: "https://host/anthropic",
    value: value || "",
  });
  const datalist = el("datalist", { id: listID });
  const fill = (k) => {
    clear(datalist);
    for (const e of ENDPOINTS[k]?.endpoints || []) {
      datalist.append(el("option", { value: e.url, label: e.desc || e.name }));
    }
    // Default to the provider's own default so the common case needs no typing.
    if (!input.value) input.value = ENDPOINTS[k]?.default || "";
  };
  fill(kind);
  return { input, datalist, setKind: fill, value: () => input.value.trim() };
}

// probePanel renders what a connection test discovered.
function probePanel() {
  const root = el("div", { class: "probe" });
  const line = (k, v, tone) => el("div", { class: "probe__row" }, [
    el("span", { class: "probe__k", text: k }),
    el("span", { class: "probe__v" + (tone ? ` probe__v--${tone}` : ""), text: v }),
  ]);
  return {
    root,
    clear: () => clear(root),
    render: (p) => {
      clear(root);
      const yn = (b) => (b ? "yes" : "no");
      root.append(
        line("reachable", yn(p.ok), p.ok ? "good" : "bad"),
        line("auth enforced", yn(p.auth_required), p.auth_required ? "good" : "warn"),
        line("/v1/models", yn(p.has_models_api)),
        line("count_tokens", yn(p.has_count_tokens)),
      );
      if (p.reported_model) root.append(line("reports model", p.reported_model));
      if (p.error) root.append(line("error", p.error, "bad"));
      for (const m of p.models || []) {
        // Context window is only knowable from a /v1/models the host may not
        // serve; say so rather than leaving a silent blank.
        root.append(line("model", m.id + (m.context_window ? ` · ${m.context_window} ctx` : " · context unknown")));
      }
    },
  };
}

// ---------------------------------------------------------------------------
// Add credential — one modal for every kind
// ---------------------------------------------------------------------------

// This was three near-identical modals (OAuth paste, provider key, custom host)
// that had drifted apart. They ask for overlapping things, so they are now one
// form whose fields follow the selected kind.
function addCredentialModal(root) {
  const kindSel = el("select", { class: "input" },
    Object.entries(KINDS).map(([k, v]) => el("option", { value: k, text: v.label })));

  const ep = endpointInput("glm", "");
  const key = el("input", { class: "input", type: "password", spellcheck: "false", placeholder: "API key" });
  const json = el("textarea", {
    class: "input input--code", rows: "9", spellcheck: "false",
    placeholder: '{\n  "claudeAiOauth": {\n    "accessToken": "...",\n    "refreshToken": "...",\n    "expiresAt": ...\n  }\n}',
  });
  const models = el("input", {
    class: "input", type: "text", spellcheck: "false",
    placeholder: "discovered on test — comma-separated to override",
  });
  const label = el("input", { class: "input", type: "text", placeholder: "defaults to the provider or host name" });
  const plan = el("input", { class: "input", type: "text", placeholder: "lite | pro | max" });
  const weight = el("input", { class: "input input--sm", type: "number", min: "0", placeholder: "auto" });

  const probe = probePanel();
  const err = el("p", { class: "form-err", role: "alert" });

  const kindRow = field("Type", kindSel, "");
  const endpointRow = field("Endpoint", el("div", { class: "field-control" }, [ep.input, ep.datalist]), "");
  const keyRow = field("API key", key, "");
  const jsonRow = field("credentials.json", json,
    "Run `claude /login` with a dedicated CLAUDE_CONFIG_DIR, then paste that file. Liveness is verified and duplicates are rejected.");
  const modelsRow = field("Models", models,
    "Left blank, the host is asked what it serves. Translation shims often answer as a different name than the one requested — that reported name is what gets stored.");
  const planRow = field("Plan", plan, "Display only — it does not affect routing.");

  const advanced = el("details", { class: "form-adv" }, [
    el("summary", { text: "Advanced" }),
    el("div", { class: "form-adv__body" }, [
      field("Label", label, "Shown in listings and charts."),
      planRow,
      field("Weight", weight, "Higher weight takes more new conversations. Weights only compete within one provider."),
    ]),
  ]);

  const body = el("div", { class: "form" }, [
    kindRow, endpointRow, keyRow, jsonRow, modelsRow, advanced, probe.root, err,
  ]);

  const busy = async (btn, verb, fn) => {
    err.textContent = "";
    btn.disabled = true;
    const orig = btn.textContent;
    btn.textContent = verb;
    try {
      await fn();
    } catch (e) {
      err.textContent = e.message || "Something went wrong.";
    } finally {
      btn.disabled = false;
      btn.textContent = orig;
    }
  };

  // Testing before saving is offered for every key-based kind, not only custom
  // hosts: it is the only way to tell a bad key from a right key pointed at the
  // wrong cluster, which fail with the same upstream message.
  const testBtn = button("Test connection", {
    onClick: (ev) => busy(ev.currentTarget, "Testing…", async () => {
      if (!ep.value()) throw new Error("Enter an endpoint first.");
      const p = await api.probeHost({ base_url: ep.value(), api_key: key.value.trim() });
      probe.render(p);
      if ((p.models || []).length && !models.value.trim()) {
        models.value = p.models.map((m) => m.id).join(", ");
      }
    }),
  });

  const submit = button("Add", {
    kind: "primary",
    onClick: (ev) => busy(ev.currentTarget, "Verifying…", async () => {
      const k = kindSel.value;
      if (k === "anthropic") {
        const raw = json.value.trim();
        if (!raw) throw new Error("Paste a credentials.json first.");
        try { JSON.parse(raw); } catch { throw new Error("That isn't valid JSON."); }
        const payload = { credentials_json: raw };
        if (label.value.trim()) payload.label = label.value.trim();
        const w = parseInt(weight.value, 10);
        if (!isNaN(w)) payload.weight = w;
        await api.post("/credentials", payload);
      } else if (k === "custom") {
        if (!ep.value()) throw new Error("Enter the host's base URL.");
        await api.addCustom({
          base_url: ep.value(),
          api_key: key.value.trim(),
          label: label.value.trim(),
          models: models.value.split(",").map((v) => v.trim()).filter(Boolean)
            .map((id) => ({ id, display_name: id })),
          weight: Number(weight.value) || 0,
        });
      } else {
        if (!key.value.trim()) throw new Error("Enter the API key.");
        await api.addKey({
          provider: k,
          endpoint: ep.value(),
          api_key: key.value.trim(),
          label: label.value.trim(),
          plan: plan.value.trim(),
          weight: Number(weight.value) || 0,
        });
      }
      m.close();
      toast("Credential added", "good");
      render(root);
    }),
  });

  const sync = () => {
    const k = kindSel.value;
    const cfg = KINDS[k];
    setHelp(kindRow, cfg.blurb);
    endpointRow.hidden = !!cfg.needsJSON;
    keyRow.hidden = !!cfg.needsJSON;
    jsonRow.hidden = !cfg.needsJSON;
    modelsRow.hidden = !cfg.models;
    planRow.hidden = !cfg.plan;
    testBtn.hidden = !!cfg.needsJSON;
    setHelp(endpointRow, cfg.endpointHelp);
    setHelp(keyRow, cfg.keyHelp);
    // Presets belong to the selected provider; a custom host has none.
    ep.input.value = "";
    ep.setKind(k);
    probe.clear();
    err.textContent = "";
  };
  kindSel.addEventListener("change", sync);

  const m = modal({
    title: "Add credential",
    subtitle: "Anthropic subscriptions, provider API keys, and any Anthropic-compatible host.",
    wide: true,
    body,
    actions: [button("Cancel", { onClick: () => m.close() }), testBtn, submit],
  });
  sync();
  return m;
}

// ---------------------------------------------------------------------------
// Rows
// ---------------------------------------------------------------------------

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

// endpointModal moves an existing key to another endpoint. The backend
// re-verifies the key there before committing, so a wrong pick is rejected
// rather than silently breaking a working credential.
function endpointModal(c, root) {
  const id = c.id || c.credential_id;
  const ep = endpointInput(c.provider, c.endpoint);
  const err = el("p", { class: "form-err", role: "alert" });
  const body = el("div", { class: "form" }, [
    field("Endpoint", el("div", { class: "field-control" }, [ep.input, ep.datalist]),
      "Pick a preset or type any URL. The key is re-verified there before the move — if it fails, nothing changes."),
    err,
  ]);
  const m = modal({
    title: "Change endpoint",
    subtitle: `Move ${c.label || id} to another cluster.`,
    body,
    actions: [
      button("Cancel", { onClick: () => m.close() }),
      button("Verify & move", {
        kind: "primary",
        onClick: async (ev) => {
          const btn = ev.currentTarget;
          err.textContent = "";
          if (!ep.value()) { err.textContent = "Pick a preset or enter a URL."; return; }
          btn.disabled = true;
          const orig = btn.textContent;
          btn.textContent = "Verifying…";
          try {
            await api.post(`/credentials/${id}/endpoint`, { endpoint: ep.value() });
            m.close();
            toast("Endpoint updated", "good");
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

function credRow(c, root) {
  const id = c.id || c.credential_id;
  const disabled = (c.status || "").toLowerCase() === "disabled";
  // A static API key has no OAuth lineage: there is nothing to refresh and no
  // .credentials.json to re-paste, so those actions are omitted rather than
  // offered and then failing.
  const isKey = c.has_usage_api === false;
  const actions = el("div", { class: "row-actions" }, [
    button("Weight", { onClick: () => weightModal(id, c.weight, root) }),
    ...(c.endpoint_editable ? [button("Endpoint", { onClick: () => endpointModal(c, root) })] : []),
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

  const modelIDs = (c.models || []).map((m) => m.id).join(", ");
  const typeCell = c.subscription_type || (modelIDs ? `${(c.models || []).length} model(s)` : "—");
  return el("tr", {}, [
    el("td", {}, [el("span", { class: "cell-strong", text: c.label || "—" }), el("span", { class: "cell-id", text: id })]),
    el("td", {}, el("span", { class: "badge", text: providerLabel(c.provider) })),
    el("td", { text: endpointLabel(c), title: c.endpoint || "" }),
    el("td", { text: typeCell, title: modelIDs }),
    el("td", {}, statusBadge(c.status)),
    el("td", { class: "num", text: String(c.weight ?? "—") }),
    el("td", { class: "num", text: compactNum(c.request_count ?? c.requests ?? 0) }),
    el("td", { text: c.last_request_at ? relTime(tsOf(c.last_request_at)) : "never" }),
    el("td", { text: isKey ? "never" : (c.expires_at ? localTime(tsOf(c.expires_at)) : "—") }),
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
    subtitle: "Higher weight → more new conversations.",
    body: el("div", { class: "form" }, [
      field("Weight", input,
        "Weights only compete within one provider. Anthropic defaults: pro 1, max/team/enterprise 5. API keys default to 1."),
    ]),
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
  return m;
}

function updateModal(id, root) {
  const ta = el("textarea", {
    class: "input input--code", rows: "9", spellcheck: "false",
    placeholder: '{\n  "claudeAiOauth": {\n    "accessToken": "...",\n    "refreshToken": "...",\n    "expiresAt": ...\n  }\n}',
  });
  const err = el("p", { class: "form-err", role: "alert" });
  const m = modal({
    title: "Update tokens",
    subtitle: "Replace this credential's tokens from a fresh login. Identity, weight and history are kept.",
    wide: true,
    body: el("div", { class: "form" }, [
      field("credentials.json", ta,
        "Use this when the subscription was re-logged-in elsewhere and the stored refresh token no longer works."),
      err,
    ]),
    actions: [
      button("Cancel", { onClick: () => m.close() }),
      button("Update", {
        kind: "primary",
        onClick: async (ev) => {
          const btn = ev.currentTarget;
          const raw = ta.value.trim();
          err.textContent = "";
          if (!raw) { err.textContent = "Paste a credentials JSON first."; return; }
          try { JSON.parse(raw); } catch { err.textContent = "That isn't valid JSON."; return; }
          btn.disabled = true;
          const orig = btn.textContent;
          btn.textContent = "Verifying…";
          try {
            await api.put(`/credentials/${id}/tokens`, { credentials_json: raw });
            m.close();
            toast("Tokens updated", "good");
            render(root);
          } catch (e) {
            err.textContent = e.message || "Update failed.";
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
