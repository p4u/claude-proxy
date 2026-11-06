// Plugins page: the Claude Code plugins every active user's claudio loads
// (GET /v1/claudio/plugins). A plugin is a pinned bundle (release zip URL and
// SHA-256) plus the environment its MCP servers and hooks need. Env values are
// write-only: typed in password fields and never shown again, only their keys.

import { api } from "../api.js";
import { el, clear, spinner, errorState, emptyState, statusBadge, toast, modal, confirmDialog, button } from "../ui.js";
import { sectionHead } from "../components.js";
import { relTime } from "../format.js";

let renderToken = 0;

export async function render(root) {
  const myToken = ++renderToken;
  clear(root);
  const head = sectionHead(
    "Plugins",
    "Bundles and environment served to every active user's claudio. Values are never shown after saving.",
    [button("Add plugin", { kind: "primary", onClick: () => metaModal(root, null) })],
  );
  const card = el("div", { class: "card table-card" }, spinner("Loading plugins…"));
  root.append(head, card);
  try {
    const res = await api.plugins();
    if (myToken !== renderToken || !card.isConnected) return;
    const list = (res && res.plugins) || [];
    if (!list.length) {
      clear(card).append(emptyState(
        "No plugins yet",
        "Add a plugin: the release zip's URL and SHA-256, then the variables its MCP servers need.",
      ));
      return;
    }
    clear(card).append(buildTable(list, root));
  } catch (e) {
    if (myToken !== renderToken || !card.isConnected) return;
    clear(card).append(errorState(e.message, () => render(root)));
  }
}

function th(label, cls) {
  return el("th", { scope: "col", class: cls || null, text: label });
}

function buildTable(list, root) {
  const table = el("table", { class: "table" }, [
    el("caption", { class: "sr-only", text: "Plugins served to claudio" }),
    el("thead", {}, el("tr", {}, [
      th("Name"), th("Version"), th("Status"), th("Bundle"), th("Environment"), th("Updated"), th("Actions", "actions"),
    ])),
  ]);
  const tb = el("tbody");
  for (const p of list) tb.append(pluginRow(p, root));
  table.append(tb);
  return el("div", { class: "table-scroll" }, table);
}

function pluginRow(p, root) {
  const keys = el("div", { class: "plugin-keys" });
  for (const k of p.env_keys || []) {
    keys.append(el("span", { class: "plugin-key" }, [
      el("span", { text: k }),
      el("button", {
        class: "plugin-key__btn", type: "button", text: "replace", title: `Replace the value of ${k}`,
        onClick: () => envModal(root, p, k),
      }),
      el("button", {
        class: "plugin-key__btn", type: "button", text: "remove", title: `Remove ${k}`,
        onClick: async () => {
          const ok = await confirmDialog({
            title: `Remove ${k}?`,
            message: `${p.name} stops receiving ${k} at the next refresh of each claudio.`,
            confirmLabel: "Remove",
          });
          if (ok) act(() => api.del(`/plugins/${enc(p.name)}/env/${enc(k)}`), `${k} removed`, root);
        },
      }),
    ]));
  }
  if (!(p.env_keys || []).length) keys.append(el("span", { class: "cell-muted", text: "none" }));

  const updated = Date.parse(p.updated_at);
  return el("tr", {}, [
    el("td", {}, [
      el("span", { class: "cell-strong", text: p.name }),
      p.installed_id ? el("span", { class: "cell-id", text: `skipped when ${p.installed_id} is installed` }) : null,
    ]),
    el("td", { text: p.version }),
    el("td", {}, statusBadge(p.enabled ? "enabled" : "disabled")),
    el("td", {}, [
      el("span", { class: "cell-id", text: p.bundle_url, title: p.bundle_url }),
      el("span", { class: "cell-id", text: `sha256 ${p.bundle_sha256.slice(0, 12)}…`, title: p.bundle_sha256 }),
    ]),
    el("td", {}, keys),
    el("td", { text: isNaN(updated) ? "—" : relTime(updated / 1000) }),
    el("td", { class: "actions" }, [
      button("Edit", { onClick: () => metaModal(root, p) }),
      button("Set variables", { onClick: () => envModal(root, p, null) }),
      button(p.enabled ? "Disable" : "Enable", {
        onClick: () => act(
          () => api.post(`/plugins/${enc(p.name)}/${p.enabled ? "disable" : "enable"}`),
          `${p.name} ${p.enabled ? "disabled" : "enabled"}`, root),
      }),
      button("Delete", {
        kind: "danger-ghost",
        onClick: async () => {
          const ok = await confirmDialog({
            title: "Delete plugin?",
            message: `"${p.name}" and all its variables are removed. claudio drops it at its next refresh.`,
            confirmLabel: "Delete",
          });
          if (ok) act(() => api.del(`/plugins/${enc(p.name)}`), "Plugin deleted", root);
        },
      }),
    ]),
  ]);
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

function field(label, input, hint) {
  return el("div", { class: "form-row" }, [
    el("label", { class: "field-label", text: label }),
    input,
    hint ? el("p", { class: "form-hint", text: hint }) : null,
  ]);
}

function input(value, placeholder) {
  return el("input", { class: "input", type: "text", value: value || "", placeholder: placeholder || "", autocomplete: "off", spellcheck: "false" });
}

// Create a plugin (p null) or replace its bundle reference. Env is untouched.
function metaModal(root, p) {
  const name = input(p ? p.name : "", "vocdoni-internal");
  if (p) name.disabled = true;
  const version = input(p && p.version, "1.4.0");
  const url = input(p && p.bundle_url, "https://github.com/…/releases/download/v1.4.0/plugin.zip");
  const sha = input(p && p.bundle_sha256, "64 lowercase hex characters");
  const installed = input(p && p.installed_id, "vocdoni-internal@vocdoni (optional)");
  const m = modal({
    title: p ? `Edit ${p.name}` : "Add plugin",
    subtitle: "The proxy never downloads the bundle; claudio does, and checks the SHA-256.",
    body: el("div", { class: "env-rows" }, [
      field("Name", name, "Lower-case letters, digits and dashes."),
      field("Version", version),
      field("Bundle URL", url, "https only."),
      field("Bundle SHA-256", sha, "sha256sum of the zip."),
      field("Installed plugin ID", installed, "When this Claude Code plugin is already installed, claudio skips the bundle."),
    ]),
    actions: [
      button("Cancel", { onClick: () => m.close() }),
      button(p ? "Save" : "Add", {
        kind: "primary",
        onClick: async () => {
          const n = name.value.trim();
          try {
            await api.setPlugin(n, {
              version: version.value.trim(),
              bundle_url: url.value.trim(),
              bundle_sha256: sha.value.trim(),
              installed_id: installed.value.trim(),
            });
            m.close();
            toast(p ? `${n} updated` : `${n} added`, "good");
            render(root);
          } catch (e) {
            toast(e.message, "critical", 6000);
          }
        },
      }),
    ],
  });
}

// Set (or replace, when key is given) env variables. Values go in password
// fields and are sent once; the page never reads them back.
function envModal(root, p, key) {
  const rows = el("div", { class: "env-rows" });
  const addRow = (k) => {
    const keyIn = input(k || "", "VOCDONI_KB_TOKEN");
    if (k) keyIn.disabled = true;
    const valIn = el("input", { class: "input", type: "password", autocomplete: "new-password", placeholder: "value" });
    const row = el("div", { class: "env-row" }, [keyIn, valIn]);
    if (!k) row.append(button("×", { title: "Remove row", onClick: () => row.remove() }));
    rows.append(row);
    return { keyIn, valIn };
  };
  const first = addRow(key);
  const m = modal({
    title: key ? `Replace ${key}` : `Variables of ${p.name}`,
    subtitle: "Every active user's claudio receives these at its next refresh. Values are never shown after saving.",
    body: el("div", {}, [
      rows,
      key ? null : button("Add variable", { onClick: () => addRow(null).keyIn.focus() }),
    ]),
    actions: [
      button("Cancel", { onClick: () => m.close() }),
      button("Save", {
        kind: "primary",
        onClick: async () => {
          const env = {};
          for (const row of rows.children) {
            const [k, v] = row.querySelectorAll("input");
            const name = k.value.trim();
            if (!name) continue;
            env[name] = v.value;
          }
          if (!Object.keys(env).length) return toast("Add at least one variable", "warning");
          try {
            await api.setPluginEnv(p.name, env);
            m.close();
            toast(`${Object.keys(env).length} variable(s) saved for ${p.name}`, "good");
            render(root);
          } catch (e) {
            toast(e.message, "critical", 6000);
          }
        },
      }),
    ],
  });
  (key ? first.valIn : first.keyIn).focus();
}

function enc(v) {
  return encodeURIComponent(String(v == null ? "" : v));
}
