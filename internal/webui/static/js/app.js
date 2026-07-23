// App shell: session gate, hash router, sidebar nav, theme toggle.

import { api, onUnauthorized } from "./api.js";
import { el, clear, toast } from "./ui.js";
import { applyTheme, getTheme, setTheme } from "./store.js";
import { renderLogin } from "./pages/login.js";
import * as dashboard from "./pages/dashboard.js";
import * as usage from "./pages/usage.js";
import * as credentials from "./pages/credentials.js";
import * as users from "./pages/users.js";
import { seriesColors } from "./charts.js";

window.__seriesColors = seriesColors;

const ROUTES = {
  dashboard: { label: "Control room", icon: "grid", mod: dashboard },
  usage: { label: "Subscriptions", icon: "gauge", mod: usage },
  credentials: { label: "Credentials", icon: "key", mod: credentials },
  users: { label: "Users", icon: "users", mod: users },
};

const appRoot = document.getElementById("app");
let shellBuilt = false;
let outlet;

applyTheme();

onUnauthorized(() => {
  shellBuilt = false;
  showLogin();
});

async function boot() {
  applyTheme();
  try {
    const s = await api.session();
    if (s && s.authenticated) showApp();
    else showLogin();
  } catch {
    showLogin();
  }
}

function showLogin() {
  clear(appRoot);
  const mount = el("div", { class: "login-shell" });
  appRoot.append(mount);
  renderLogin(mount, () => showApp());
}

function showApp() {
  if (!shellBuilt) buildShell();
  if (!location.hash || !ROUTES[currentRoute()]) location.hash = "#/dashboard";
  route();
}

function currentRoute() {
  return (location.hash.replace(/^#\//, "") || "dashboard").split("?")[0];
}

function buildShell() {
  clear(appRoot);
  outlet = el("main", { class: "outlet", id: "outlet", tabindex: "-1" });

  const nav = el("nav", { class: "nav", "aria-label": "Sections" });
  const navLinks = new Map();
  for (const [key, r] of Object.entries(ROUTES)) {
    const a = el("a", { class: "nav__link", href: `#/${key}` }, [
      el("span", { class: "nav__icon", html: ICONS[r.icon] }),
      el("span", { class: "nav__label", text: r.label }),
    ]);
    navLinks.set(key, a);
    nav.append(a);
  }

  const themeBtn = el("button", {
    class: "icon-btn theme-btn", "aria-label": "Toggle theme", title: "Toggle theme",
    onClick: cycleTheme, html: ICONS.theme,
  });
  const logoutBtn = el("button", {
    class: "icon-btn", "aria-label": "Log out", title: "Log out",
    onClick: async () => { try { await api.logout(); } catch {} shellBuilt = false; showLogin(); },
    html: ICONS.logout,
  });

  const brand = el("div", { class: "sidebar__brand" }, [
    el("div", { class: "brand-mark brand-mark--sm", "aria-hidden": "true" }, sigBars()),
    el("div", {}, [
      el("div", { class: "brand-name", text: "claude-proxy" }),
      el("div", { class: "brand-tag", text: "control tower" }),
    ]),
  ]);

  const sidebar = el("aside", { class: "sidebar" }, [
    brand,
    nav,
    el("div", { class: "sidebar__foot" }, [themeBtn, logoutBtn]),
  ]);

  // Mobile top bar
  const menuBtn = el("button", { class: "icon-btn menu-btn", "aria-label": "Menu", html: ICONS.menu, onClick: () => document.body.classList.toggle("nav-open") });
  const topbar = el("header", { class: "topbar" }, [
    menuBtn,
    el("div", { class: "topbar__brand", text: "claude-proxy" }),
    themeBtn.cloneNode(true),
  ]);
  topbar.lastChild.addEventListener("click", cycleTheme);

  const scrim = el("div", { class: "nav-scrim", onClick: () => document.body.classList.remove("nav-open") });

  appRoot.append(el("div", { class: "layout" }, [sidebar, scrim, el("div", { class: "main" }, [topbar, outlet])]));
  shellBuilt = true;

  window.addEventListener("hashchange", () => {
    document.body.classList.remove("nav-open");
    route();
  });
  nav.addEventListener("click", () => document.body.classList.remove("nav-open"));

  window.__navLinks = navLinks;
}

function highlightNav() {
  const cur = currentRoute();
  if (!window.__navLinks) return;
  for (const [key, a] of window.__navLinks) {
    const on = key === cur;
    a.classList.toggle("is-active", on);
    if (on) a.setAttribute("aria-current", "page");
    else a.removeAttribute("aria-current");
  }
}

async function route() {
  const key = currentRoute();
  const r = ROUTES[key];
  if (!r) {
    location.hash = "#/dashboard";
    return;
  }
  highlightNav();
  clear(outlet);
  try {
    await r.mod.render(outlet);
    outlet.focus({ preventScroll: true });
  } catch (e) {
    toast(e.message || "Failed to render", "critical", 6000);
  }
}

function cycleTheme() {
  const order = ["auto", "light", "dark"];
  const next = order[(order.indexOf(getTheme()) + 1) % order.length];
  setTheme(next);
  toast(`Theme: ${next}`, "info", 1500);
  if (shellBuilt) route(); // re-render charts with new palette
}

function sigBars() {
  const wrap = el("div", { class: "sig-bars" });
  [0.55, 0.85, 0.4, 1, 0.7].forEach((h, i) => wrap.append(el("span", { class: "sig-bar", style: `--h:${h}; --i:${i}` })));
  return wrap;
}

const ICONS = {
  grid: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7"><rect x="3" y="3" width="7" height="7" rx="1.5"/><rect x="14" y="3" width="7" height="7" rx="1.5"/><rect x="3" y="14" width="7" height="7" rx="1.5"/><rect x="14" y="14" width="7" height="7" rx="1.5"/></svg>',
  gauge: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7"><path d="M4 15a8 8 0 0 1 16 0"/><path d="M12 15l4-4"/><circle cx="12" cy="15" r="1.3" fill="currentColor" stroke="none"/></svg>',
  key: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7"><circle cx="8" cy="12" r="4"/><path d="M12 12h9M18 12v4M21 12v3"/></svg>',
  users: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7"><circle cx="9" cy="8" r="3.2"/><path d="M3.5 20a5.5 5.5 0 0 1 11 0"/><path d="M16 5.2a3.2 3.2 0 0 1 0 6M17.5 20a5.5 5.5 0 0 0-3-4.9"/></svg>',
  theme: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7"><circle cx="12" cy="12" r="4.2"/><path d="M12 2v2M12 20v2M2 12h2M20 12h2M5 5l1.4 1.4M17.6 17.6L19 19M19 5l-1.4 1.4M6.4 17.6L5 19"/></svg>',
  logout: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7"><path d="M14 4H6a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h8"/><path d="M16 8l4 4-4 4M20 12H9"/></svg>',
  menu: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"><path d="M4 7h16M4 12h16M4 17h16"/></svg>',
};

boot();
