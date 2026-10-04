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

let dialogID = 0;
const modalStack = [];

// Modal dialog. Nested dialogs preserve the previous dialog's focus/inert state.
export function modal({ title, subtitle, body, actions, wide, onClose }) {
  const returnFocus = document.activeElement;
  const titleID = `dialog-title-${++dialogID}`;
  const overlay = el("div", { class: "modal-overlay" });
  const dialog = el("div", {
    class: "modal" + (wide ? " modal--wide" : ""),
    role: "dialog",
    "aria-modal": "true",
    "aria-labelledby": titleID,
    tabindex: "-1",
  });
  const previousOverflow = document.body.style.overflow;
  const background = [...document.body.children].map((node) => [node, node.inert]);
  background.forEach(([node]) => { node.inert = true; });
  document.body.style.overflow = "hidden";
  modalStack.push(overlay);
  let closed = false;
  const close = () => {
    if (closed || modalStack.at(-1) !== overlay) return;
    closed = true;
    modalStack.pop();
    overlay.remove();
    document.removeEventListener("keydown", onKey);
    background.forEach(([node, inert]) => { node.inert = inert; });
    document.body.style.overflow = previousOverflow;
    if (returnFocus?.isConnected) returnFocus.focus();
    else document.querySelector(".outlet")?.focus();
    onClose?.();
  };
  const focusable = () => [...dialog.querySelectorAll('a[href],button,input,select,textarea,[tabindex="0"]')]
    .filter((node) => !node.disabled && !node.closest("[hidden],[inert]") && node.getClientRects().length);
  const onKey = (e) => {
    if (modalStack.at(-1) !== overlay) return;
    if (e.key === "Escape") { e.preventDefault(); close(); }
    if (e.key !== "Tab") return;
    const nodes = focusable();
    const first = nodes[0] || dialog;
    const last = nodes.at(-1) || dialog;
    if (e.shiftKey && (document.activeElement === first || !dialog.contains(document.activeElement))) {
      e.preventDefault(); last.focus();
    } else if (!e.shiftKey && (document.activeElement === last || !dialog.contains(document.activeElement))) {
      e.preventDefault(); first.focus();
    }
  };
  document.addEventListener("keydown", onKey);
  overlay.addEventListener("mousedown", (e) => {
    if (e.target === overlay) close();
  });

  const header = el("div", { class: "modal__head" }, [
    el("div", {}, [
      el("h2", { class: "modal__title", id: titleID, text: title }),
      subtitle ? el("p", { class: "modal__sub", text: subtitle }) : null,
    ]),
    el("button", { class: "icon-btn", "aria-label": "Close", onClick: close, html: "&times;" }),
  ]);
  const bodyWrap = el("div", { class: "modal__body" }, [body]);
  const footer = actions ? el("div", { class: "modal__foot" }, actions) : null;
  dialog.append(header, bodyWrap);
  if (footer) dialog.append(footer);
  overlay.append(dialog);
  document.body.append(overlay);
  requestAnimationFrame(() => {
    if (closed) return;
    overlay.classList.add("is-in");
    const nodes = focusable();
    (nodes.find((node) => node.matches("input,textarea,select")) || nodes.find((node) => !node.classList.contains("icon-btn")) || dialog).focus();
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
        resolve(true);
        m.close();
      },
    });
    m = modal({
      title,
      body: el("p", { class: "confirm-msg", text: message }),
      actions: [cancel, ok],
      onClose: () => resolve(false),
    });
    requestAnimationFrame(() => cancel.focus());
  });
}

// The native popover top layer keeps row menus clear of card/viewport clipping.
let menuID = 0;
export function actionMenu(label, items) {
  const id = `actions-${++menuID}`;
  const menu = el("div", { class: "action-menu__list", id, popover: "auto", role: "menu", "aria-label": label });
  const trigger = el("button", {
    class: "btn btn--ghost action-menu__trigger", type: "button", text: "Actions ▾",
    "aria-label": label, "aria-haspopup": "menu", "aria-expanded": "false", "aria-controls": id,
  });
  const root = el("div", { class: "action-menu" }, [trigger, menu]);
  const enabled = () => [...menu.querySelectorAll("button:not(:disabled)")];
  const close = (restore = true) => {
    if (menu.matches(":popover-open")) menu.hidePopover();
    if (restore && trigger.isConnected) trigger.focus();
  };
  const open = (last = false) => {
    menu.showPopover();
    const rect = trigger.getBoundingClientRect();
    menu.style.left = `${Math.max(8, Math.min(rect.right - menu.offsetWidth, innerWidth - menu.offsetWidth - 8))}px`;
    menu.style.top = `${Math.max(8, rect.bottom + menu.offsetHeight + 8 <= innerHeight ? rect.bottom + 6 : rect.top - menu.offsetHeight - 6)}px`;
    (last ? enabled().at(-1) : enabled()[0])?.focus();
  };
  for (const item of items) {
    menu.append(el("button", {
      class: "action-menu__item" + (item.danger ? " action-menu__item--danger" : ""),
      type: "button", role: "menuitem", tabindex: "-1", text: item.label, disabled: item.disabled,
      onClick: () => { close(); item.onClick?.(); },
    }));
  }
  trigger.addEventListener("click", () => menu.matches(":popover-open") ? close() : open());
  trigger.addEventListener("keydown", (e) => {
    if (e.key === "ArrowDown" || e.key === "ArrowUp") { e.preventDefault(); open(e.key === "ArrowUp"); }
  });
  menu.addEventListener("beforetoggle", (e) => {
    trigger.setAttribute("aria-expanded", String(e.newState === "open"));
    if (e.newState === "closed" && menu.contains(document.activeElement) && trigger.isConnected) trigger.focus();
  });
  menu.addEventListener("keydown", (e) => {
    if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); close(); return; }
    if (e.key === "Tab") { close(); return; }
    const buttons = enabled();
    let index = buttons.indexOf(document.activeElement);
    if (e.key === "ArrowDown") index = (index + 1) % buttons.length;
    else if (e.key === "ArrowUp") index = (index - 1 + buttons.length) % buttons.length;
    else if (e.key === "Home") index = 0;
    else if (e.key === "End") index = buttons.length - 1;
    else return;
    e.preventDefault(); buttons[index]?.focus();
  });
  return root;
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
