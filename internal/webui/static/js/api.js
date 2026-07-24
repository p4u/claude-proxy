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
  userPrompts: (id, limit = 50) => request("GET", `/users/${id}/prompts?limit=${limit}`),
  conversations: (limit = 100) => request("GET", `/conversations?limit=${limit}`),
};

export { ApiError };
