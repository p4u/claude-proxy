import { api, OPENAI_BASE } from "./api.js";
import { el, clear, modal, button, copyText, toast, spinner } from "./ui.js";
import { segmented } from "./components.js";

// Connect panel: everything a user needs to point an agent at this proxy —
// the OpenAI-compatible and Anthropic base URLs, their token, the models they
// can reach, and ready-to-paste settings for common clients. URLs come from
// the page's own origin, which is the proxy's public address (the UI and the
// API share one listener).

function copyButton(text, label = "Copy") {
  const b = button(label, {
    onClick: async () => {
      if (await copyText(text())) {
        b.textContent = "Copied";
        setTimeout(() => (b.textContent = label), 1600);
      } else {
        toast("Copy failed — select the text manually", "warning");
      }
    },
  });
  return b;
}

function valueRow(label, value, { secret = false } = {}) {
  let shown = !secret;
  const code = el("code", { class: "token-reveal connect__value" });
  const paint = () => { code.textContent = shown ? value : "•".repeat(Math.min(value.length, 40)); };
  paint();
  const controls = [copyButton(() => value)];
  if (secret) {
    const toggle = button("Show", {
      onClick: () => { shown = !shown; toggle.textContent = shown ? "Hide" : "Show"; paint(); },
    });
    controls.unshift(toggle);
  }
  return el("div", { class: "connect__row" }, [
    el("span", { class: "connect__label", text: label }),
    el("div", { class: "token-box" }, [code, ...controls]),
  ]);
}

// Context windows in decimal units, the way providers quote them: 1,048,576
// and 1,000,000 both read "1M", 131,072 "131K", 400,000 "400K".
function ctxLabel(n) {
  return n >= 1e6 ? `${Math.round(n / 1e5) / 10}M` : `${Math.round(n / 1e3)}K`;
}

function snippetBlock(text) {
  const pre = el("pre", { class: "connect__snippet" });
  pre.textContent = text;
  return el("div", { class: "connect__snippet-wrap" }, [pre, copyButton(() => text)]);
}

function snippets({ origin, openAIBase, token, models }) {
  const first = models[0]?.id || "privatemode/glm-5.3";
  const piModels = models.map((m) => {
    const entry = { id: m.id, name: m.name || m.id };
    if (m.context_length) entry.contextWindow = m.context_length;
    return entry;
  });
  return {
    env: {
      label: "Environment",
      help: "Any OpenAI SDK or agent that reads the standard variables.",
      text: `export OPENAI_BASE_URL=${openAIBase}\nexport OPENAI_API_KEY=${token}`,
    },
    curl: {
      label: "curl",
      help: "A one-off request, to check the connection.",
      text:
        `curl ${openAIBase}/chat/completions \\\n` +
        `  -H "Authorization: Bearer ${token}" \\\n` +
        `  -H "Content-Type: application/json" \\\n` +
        `  -d '${JSON.stringify({ model: first, messages: [{ role: "user", content: "Hello!" }] })}'`,
    },
    pi: {
      label: "Pi",
      help: "Merge into ~/.pi/agent/models.json, then pick a model with /model.",
      text: JSON.stringify({
        providers: {
          "claude-proxy": { baseUrl: openAIBase, api: "openai-completions", apiKey: token, models: piModels },
        },
      }, null, 2),
    },
    claude: {
      label: "Claude Code",
      help: "~/.claude/settings.json. Gateway mode makes Claude Code list this proxy's models in /model.",
      text: JSON.stringify({
        env: {
          ANTHROPIC_BASE_URL: origin,
          ANTHROPIC_AUTH_TOKEN: token,
          CLAUDE_CODE_USE_GATEWAY: "1",
          CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY: "1",
        },
      }, null, 2),
    },
  };
}

export function connectModal(user) {
  const origin = location.origin;
  const openAIBase = origin + OPENAI_BASE;
  const body = el("div", { class: "connect" }, [spinner("Loading connection details…")]);
  const m = modal({
    title: `Connect · ${user.name}`,
    subtitle: "Point any OpenAI-compatible agent, or Claude Code, at this proxy with this user's token.",
    wide: true,
    body,
    actions: [button("Done", { kind: "ghost", onClick: () => m.close() })],
  });
  load();

  async function load() {
    let token;
    try {
      token = (await api.userToken(user.id)).token;
    } catch (e) {
      clear(body).append(el("p", { class: "form-err", text: e.message }));
      return;
    }
    let models = [];
    let modelsErr = "";
    try {
      models = await api.openAIModels(token);
    } catch (e) {
      modelsErr = e.message;
    }
    render(token, models, modelsErr);
  }

  function render(token, models, modelsErr) {
    const snip = snippets({ origin, openAIBase, token, models });
    const panel = el("div", { class: "connect__panel" });
    const show = (k) => {
      clear(panel).append(el("p", { class: "field-help", text: snip[k].help }), snippetBlock(snip[k].text));
    };
    const tabs = segmented(Object.entries(snip).map(([value, s]) => ({ value, label: s.label })), "env", show, "Client");
    show("env");

    const modelList = modelsErr
      ? el("p", { class: "form-err", text: `Could not list models: ${modelsErr}` })
      : models.length
        ? el("ul", { class: "connect__models" }, models.map((mm) => el("li", {}, [
          el("code", { text: mm.id }),
          mm.context_length ? el("span", { class: "connect__ctx", text: `${ctxLabel(mm.context_length)} ctx` }) : null,
        ])))
        : el("p", { class: "field-help", text: "No OpenAI-compatible models yet. Add a Privatemode key, a Codex or Gemini subscription, or a custom OpenAI host." });

    clear(body).append(
      valueRow("OpenAI base URL", openAIBase),
      valueRow("API key", token, { secret: true }),
      valueRow("Anthropic base URL", origin),
      el("div", { class: "connect__row" }, [
        el("span", { class: "connect__label", text: "OpenAI models" }),
        modelList,
      ]),
      el("div", { class: "connect__row" }, [
        el("span", { class: "connect__label", text: "Client settings" }),
        el("div", { class: "cap__tabs" }, tabs),
        panel,
      ]),
      el("p", { class: "field-help", text:
        "The OpenAI API serves Chat Completions for Privatemode, Codex, Gemini and custom OpenAI hosts. " +
        "Anthropic, GLM and MiMo models are reachable through the Anthropic base URL. " +
        "The token is this user's: requests are attributed to them and count against their limit." }),
    );
  }
  return m;
}
