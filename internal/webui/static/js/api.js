// Single API surface. All requests are same-origin with the session cookie.
// The app is served at the root "/", so the API lives at "/api".

export const API_BASE = "/api";

// Build the time-window query fragment. Accepts either a bare period string
// (e.g. "24h") or a selection object {mode,period,from,to}. A valid custom
// window ({mode:"custom",from,to} with unix seconds) overrides `period`.
export function winParams(win) {
  if (win && typeof win === "object") {
    if (win.mode === "custom" && win.from != null && win.to != null) {
      return `from=${win.from}&to=${win.to}`;
    }
    return `period=${win.period || "24h"}`;
  }
  return `period=${win || "24h"}`;
}

// Listeners notified when a request is rejected with 401 (session expired).
const unauthedListeners = new Set();
export function onUnauthorized(fn) {
  unauthedListeners.add(fn);
  return () => unauthedListeners.delete(fn);
}

class ApiError extends Error {
  constructor(message, status) {
    super(message);
    this.status = status;
  }
}

async function request(method, path, body) {
  const opts = {
    method,
    credentials: "same-origin",
    headers: {},
  };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  let res;
  try {
    res = await fetch(API_BASE + path, opts);
  } catch (e) {
    throw new ApiError("Network error — is the proxy reachable?", 0);
  }
  // A 401 on /login is a wrong password, not an expired session — let it fall
  // through to the generic error path so the form can show the message.
  if (res.status === 401 && path !== "/login") {
    unauthedListeners.forEach((fn) => fn());
    throw new ApiError("Session expired", 401);
  }
  let data = null;
  const text = await res.text();
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = text;
    }
  }
  if (!res.ok) {
    const msg = (data && data.error) || res.statusText || "Request failed";
    throw new ApiError(msg, res.status);
  }
  return data;
}

export const api = {
  get: (p) => request("GET", p),
  post: (p, b) => request("POST", p, b),
  put: (p, b) => request("PUT", p, b),
  del: (p) => request("DELETE", p),

  // Auth
  login: (password) => request("POST", "/login", { password }),
  logout: () => request("POST", "/logout"),
  session: () => request("GET", "/session"),

  // Data
  overview: (win) => request("GET", `/overview?${winParams(win)}`),
  statsRequests: (win, buckets, groupBy) =>
    request("GET", `/stats/requests?${winParams(win)}&buckets=${buckets}&group_by=${groupBy}`),
  statsTokens: (win, buckets, groupBy) =>
    request("GET", `/stats/tokens?${winParams(win)}&buckets=${buckets}&group_by=${groupBy}`),
  statsTotals: (win, buckets) =>
    request("GET", `/stats/totals?${winParams(win)}&buckets=${buckets}`),
  statsLatency: (win, buckets) =>
    request("GET", `/stats/latency?${winParams(win)}&buckets=${buckets}`),
  statsUsers: (win) => request("GET", `/stats/users?${winParams(win)}`),
  statsSelection: (win, buckets) =>
    request("GET", `/stats/selection?${winParams(win)}&buckets=${buckets}`),
  usageCurrent: () => request("GET", "/usage/current"),
  usageHistory: (win, credId) =>
    request("GET", `/usage/history?${winParams(win)}` + (credId ? `&credential_id=${credId}` : "")),
  credentials: () => request("GET", "/credentials"),
  users: () => request("GET", "/users"),

  // Per-user capture mode. `full` true ⇒ store both sides of every conversation.
  setUserCapture: (id, full) => request("POST", `/users/${enc(id)}/capture`, { full: !!full }),

  // Per-user rolling usage limit, in output tokens. Both values 0 clears the
  // limit; the backend rejects a negative value, or one set and the other zero,
  // with a 400 whose message is meant to be shown to the operator verbatim.
  setUserLimit: (id, { outputTokens = 0, windowSeconds = 0 } = {}) =>
    request("POST", `/users/${enc(id)}/limit`, {
      output_tokens: Math.trunc(Number(outputTokens) || 0),
      window_seconds: Math.trunc(Number(windowSeconds) || 0),
    }),

  // Output tokens this user produced over an arbitrary rolling window,
  // whether or not a limit is configured. Lets the limit editor show real
  // usage for the window being capped instead of asking for a blind guess.
  userWindowUsage: (id, windowSeconds) =>
    request("GET", `/users/${enc(id)}/usage?window_seconds=${Math.trunc(Number(windowSeconds) || 0)}`),

  // Paginated envelopes: {items, total, limit, offset, has_more}.
  userPrompts: (id, { limit = 50, offset = 0 } = {}) =>
    request("GET", `/users/${enc(id)}/prompts?${page(limit, offset)}`),
  userConversations: (id, { limit = 25, offset = 0 } = {}) =>
    request("GET", `/users/${enc(id)}/conversations?${page(limit, offset)}`),
  conversationMessages: (convID, { limit = 20, offset = 0 } = {}) =>
    request("GET", `/conversations/${enc(convID)}/messages?${page(limit, offset)}`),

  conversations: (limit = 100) => request("GET", `/conversations?limit=${limit}`),
};

// Markdown export is a normal navigation, not an XHR: same-origin cookie auth
// applies and the browser handles the download + filename from
// Content-Disposition. Never fetch+blob this.
export function conversationExportUrl(convID) {
  return `${API_BASE}/conversations/${enc(convID)}/export.md`;
}

function enc(v) {
  return encodeURIComponent(String(v == null ? "" : v));
}

function page(limit, offset) {
  return `limit=${Math.max(1, limit | 0)}&offset=${Math.max(0, offset | 0)}`;
}

export { ApiError };
