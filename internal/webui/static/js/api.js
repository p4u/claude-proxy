// Single API surface. All requests are same-origin with the session cookie.
// In production the app is served at /ui/ so API_BASE is "/ui/api".

export const API_BASE = "/ui/api";

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
  overview: () => request("GET", "/overview"),
  statsRequests: (period, buckets, groupBy) =>
    request("GET", `/stats/requests?period=${period}&buckets=${buckets}&group_by=${groupBy}`),
  statsTokens: (period, buckets, groupBy) =>
    request("GET", `/stats/tokens?period=${period}&buckets=${buckets}&group_by=${groupBy}`),
  statsLatency: (period, buckets) =>
    request("GET", `/stats/latency?period=${period}&buckets=${buckets}`),
  statsUsers: (period) => request("GET", `/stats/users?period=${period}`),
  usageCurrent: () => request("GET", "/usage/current"),
  usageHistory: (period, credId) =>
    request("GET", `/usage/history?period=${period}` + (credId ? `&credential_id=${credId}` : "")),
  credentials: () => request("GET", "/credentials"),
  users: () => request("GET", "/users"),
  conversations: (limit = 100) => request("GET", `/conversations?limit=${limit}`),
};

export { ApiError };
