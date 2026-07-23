import { api } from "../api.js";
import { el, clear } from "../ui.js";

// Renders the login screen into document body's #app-login slot. onSuccess() called after auth.
export function renderLogin(mount, onSuccess) {
  clear(mount);
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
        el("div", { class: "login__brand" }, [
          el("div", { class: "brand-mark", "aria-hidden": "true" }, buildMark()),
          el("div", {}, [
            el("div", { class: "login__title", text: "claude-proxy" }),
            el("div", { class: "login__tag", text: "subscription control tower" }),
          ]),
        ]),
        form,
      ]),
      el("p", { class: "login__foot", text: "Sessions last 24 hours · same-origin only" }),
    ])
  );
  setTimeout(() => pw.focus(), 30);
}

// The signature mark: three stacked signal bars (multiplexed channels).
function buildMark() {
  const wrap = el("div", { class: "sig-bars" });
  [0.55, 0.85, 0.4, 1, 0.7].forEach((h, i) => {
    wrap.append(el("span", { class: "sig-bar", style: `--h:${h}; --i:${i}` }));
  });
  return wrap;
}
