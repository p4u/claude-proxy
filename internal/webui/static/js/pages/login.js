import { api } from "../api.js";
import { el, clear } from "../ui.js";

// Renders the login screen into document body's #app-login slot. onSuccess() called after auth.
// method is what /api/session reported: "sso" sends the operator to the
// single sign-on provider instead of asking for the password.
export function renderLogin(mount, onSuccess, method) {
  clear(mount);
  if (method === "sso") {
    renderSSO(mount);
    return;
  }
  const pw = el("input", {
    class: "input login__input", type: "password", placeholder: "Password",
    autocomplete: "current-password", "aria-label": "UI password", autofocus: true,
  });
  const err = el("p", { class: "login__err", role: "alert" });
  const submit = el("button", { class: "btn btn--primary login__btn", type: "submit", text: "Enter" });

  const form = el("form", { class: "login__form", onSubmit: async (e) => {
    e.preventDefault();
    err.textContent = "";
    submit.disabled = true;
    submit.textContent = "Checking…";
    try {
      await api.login(pw.value);
      onSuccess();
    } catch (ex) {
      err.textContent = ex.status === 429
        ? "Too many attempts — wait a minute and try again."
        : ex.status === 401
        ? "That password isn't right."
        : ex.message;
      submit.disabled = false;
      submit.textContent = "Enter";
      pw.select();
    }
  } }, [
    el("label", { class: "field-label", text: "Password", for: "pw" }),
    pw,
    err,
    submit,
  ]);
  pw.id = "pw";

  mount.append(
    el("div", { class: "login" }, [
      el("div", { class: "login__card" }, [
        brandBlock(),
        form,
      ]),
      el("p", { class: "login__foot", text: "Sessions renew while in use · same-origin only" }),
    ])
  );
  setTimeout(() => pw.focus(), 30);
}

// Single sign-on: a plain link to the provider round trip, which lands back
// on the page the operator was looking at. There is no password field.
function renderSSO(mount) {
  const back = "/" + location.hash;
  const go = el("a", {
    class: "btn btn--primary login__btn login__sso",
    href: "/auth/login?return_to=" + encodeURIComponent(back),
    text: "Sign in with Vocdoni",
  });
  mount.append(
    el("div", { class: "login" }, [
      el("div", { class: "login__card" }, [
        brandBlock(),
        el("p", { class: "login__hint", text: "Use your @vocdoni.org Google account." }),
        go,
      ]),
      el("p", { class: "login__foot", text: "Single sign-on · oauth.vocdoni.org" }),
    ])
  );
  setTimeout(() => go.focus(), 30);
}

function brandBlock() {
  return el("div", { class: "login__brand" }, [
    el("div", { class: "brand-mark", "aria-hidden": "true" }, buildMark()),
    el("div", {}, [
      el("div", { class: "login__title", text: "claude-proxy" }),
      el("div", { class: "login__tag", text: "subscription control tower" }),
    ]),
  ]);
}

// The signature mark: three stacked signal bars (multiplexed channels).
function buildMark() {
  const wrap = el("div", { class: "sig-bars" });
  [0.55, 0.85, 0.4, 1, 0.7].forEach((h, i) => {
    wrap.append(el("span", { class: "sig-bar", style: `--h:${h}; --i:${i}` }));
  });
  return wrap;
}
