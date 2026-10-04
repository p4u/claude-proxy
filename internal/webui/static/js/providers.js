import { el } from "./ui.js";

// Local wordmarks/symbols: no third-party image requests from the dashboard.
const PROVIDERS = {
  anthropic: { name: "Anthropic", mark: "AⅠ", color: "#cc785c", order: 0, description: "Claude subscriptions · live quota windows" },
  glm: { name: "Z.AI", mark: "Z", color: "#7280ed", order: 1, description: "GLM coding plans · proxy-metered usage" },
  mimo: { name: "Xiaomi MiMo", mark: "mi", color: "#ff6900", order: 2, description: "MiMo token plans · proxy-metered usage" },
  codex: { name: "OpenAI Codex", mark: "O", color: "#10a37f", order: 3, description: "Codex subscriptions · account quota windows" },
  gemini: { name: "Google Gemini", mark: "✦", color: "#5286ee", order: 4, description: "Gemini subscriptions · shared model quota" },
  custom: { name: "Custom Anthropic", mark: "</>", color: "#9a79c7", order: 5, description: "Anthropic-compatible hosts · proxy-metered usage" },
  custom_openai: { name: "Custom OpenAI", mark: "</>", color: "#699cae", order: 6, description: "OpenAI-compatible hosts · proxy-metered usage" },
};

export function providerInfo(id = "anthropic") {
  return Object.hasOwn(PROVIDERS, id) ? PROVIDERS[id] : {
    name: id || "Other", mark: "◇", color: "#699cae", order: 99, description: "Managed credentials",
  };
}

export function providerMark(id, size = "sm") {
  const p = providerInfo(id);
  return el("span", {
    class: `provider-mark provider-mark--${size}`,
    style: `--provider-color:${p.color}`,
    text: p.mark,
    "aria-hidden": "true",
  });
}

export function groupProviders(rows) {
  const groups = new Map();
  for (const row of rows) {
    const id = row.provider || "anthropic";
    if (!groups.has(id)) groups.set(id, []);
    groups.get(id).push(row);
  }
  return [...groups].sort(([a], [b]) => providerInfo(a).order - providerInfo(b).order || a.localeCompare(b));
}
