// DOM helpers, toasts, modals, confirm dialogs, status badges.

export function el(tag, attrs = {}, children = []) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v == null || v === false) continue;
    if (k === "class") node.className = v;
    else if (k === "html") node.innerHTML = v;
    else if (k === "text") node.textContent = v;
    else if (k.startsWith("on") && typeof v === "function") {
      node.addEventListener(k.slice(2).toLowerCase(), v);
    } else if (k === "dataset") {
      Object.assign(node.dataset, v);
    } else {
      node.setAttribute(k, v);
    }
  }
  const kids = Array.isArray(children) ? children : [children];
  for (const c of kids) {
    if (c == null || c === false) continue;
    node.append(c.nodeType ? c : document.createTextNode(String(c)));
  }
  return node;
}

export function clear(node) {
  while (node.firstChild) node.removeChild(node.firstChild);
  return node;
}

// Toast notifications.
let toastRoot;
export function toast(message, kind = "info", ttl = 4200) {
  if (!toastRoot) {
    toastRoot = el("div", { class: "toast-root", "aria-live": "polite" });
    document.body.append(toastRoot);
  }
  const t = el("div", { class: `toast toast--${kind}`, role: "status" }, [
    el("span", { class: "toast__dot" }),
    el("span", { class: "toast__msg", text: message }),
  ]);
  toastRoot.append(t);
  requestAnimationFrame(() => t.classList.add("is-in"));
  const close = () => {
    t.classList.remove("is-in");
    setTimeout(() => t.remove(), 220);
  };
  setTimeout(close, ttl);
  t.addEventListener("click", close);
}

// Status → badge kind mapping.
export function statusBadge(status) {
  const s = (status || "").toLowerCase();
  const map = {
    active: "good",
    ok: "good",
    enabled: "good",
    limited: "warning",
    disabled: "muted",
    errored: "critical",
    error: "critical",
    expired: "critical",
  };
  const kind = map[s] || "muted";
  return el("span", { class: `badge badge--${kind}` }, [
    el("span", { class: "badge__dot" }),
    el("span", { text: status || "unknown" }),
  ]);
}

// Modal dialog. Returns { root, close }. content is a DOM node.
export function modal({ title, subtitle, body, actions, wide }) {
  const overlay = el("div", { class: "modal-overlay" });
  const dialog = el("div", {
    class: "modal" + (wide ? " modal--wide" : ""),
    role: "dialog",
    "aria-modal": "true",
    "aria-label": title,
  });
  const close = () => {
    overlay.classList.remove("is-in");
    setTimeout(() => overlay.remove(), 200);
    document.removeEventListener("keydown", onKey);
  };
  const onKey = (e) => {
    if (e.key === "Escape") close();
  };
  document.addEventListener("keydown", onKey);
  overlay.addEventListener("mousedown", (e) => {
    if (e.target === overlay) close();
  });

  const header = el("div", { class: "modal__head" }, [
    el("div", {}, [
      el("h2", { class: "modal__title", text: title }),
      subtitle ? el("p", { class: "modal__sub", text: subtitle }) : null,
    ]),
    el("button", { class: "icon-btn", "aria-label": "Close", onClick: close, html: "&times;" }),
  ]);
  const bodyWrap = el("div", { class: "modal__body" }, [body]);
  const footer = actions ? el("div", { class: "modal__foot" }, actions) : null;
  dialog.append(header, bodyWrap, footer);
  overlay.append(dialog);
  document.body.append(overlay);
  requestAnimationFrame(() => {
    overlay.classList.add("is-in");
    const focusable = dialog.querySelector("input,textarea,button:not(.icon-btn)");
    if (focusable) focusable.focus();
  });
  return { overlay, dialog, close };
}

// Confirm dialog for destructive actions. Returns a Promise<boolean>.
export function confirmDialog({ title, message, confirmLabel = "Confirm", danger = true }) {
  return new Promise((resolve) => {
    let m;
    const cancel = el("button", {
      class: "btn btn--ghost",
      text: "Cancel",
      onClick: () => {
        m.close();
        resolve(false);
      },
    });
    const ok = el("button", {
      class: "btn " + (danger ? "btn--danger" : "btn--primary"),
      text: confirmLabel,
      onClick: () => {
        m.close();
        resolve(true);
      },
    });
    m = modal({
      title,
      body: el("p", { class: "confirm-msg", text: message }),
      actions: [cancel, ok],
    });
    requestAnimationFrame(() => ok.focus());
  });
}

export function button(label, { kind = "ghost", onClick, disabled, title } = {}) {
  return el("button", {
    class: `btn btn--${kind}`,
    text: label,
    disabled: disabled || false,
    title: title || null,
    onClick,
  });
}

export function spinner(label = "Loading…") {
  return el("div", { class: "loading" }, [el("span", { class: "loading__ring" }), el("span", { text: label })]);
}

export function emptyState(title, hint) {
  return el("div", { class: "empty" }, [
    el("div", { class: "empty__mark", html: "&middot;&middot;&middot;" }),
    el("p", { class: "empty__title", text: title }),
    hint ? el("p", { class: "empty__hint", text: hint }) : null,
  ]);
}

export function errorState(message, onRetry) {
  return el("div", { class: "empty empty--error" }, [
    el("p", { class: "empty__title", text: "Couldn't load this" }),
    el("p", { class: "empty__hint", text: message }),
    onRetry ? button("Retry", { kind: "ghost", onClick: onRetry }) : null,
  ]);
}

export async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    return false;
  }
}
