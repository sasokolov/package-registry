// One place that talks to the registry. Every call goes through here, so
// authentication, error shape and the "your token expired" path exist once
// rather than in every screen.

export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
    readonly etag?: string,
  ) {
    super(message);
    this.name = "ApiError";
  }

  /** The caller is not authenticated at all. */
  get unauthenticated(): boolean {
    return this.status === 401;
  }

  /** Authenticated, but not allowed. */
  get forbidden(): boolean {
    return this.status === 403;
  }

  /** Someone changed the document since it was read. */
  get conflict(): boolean {
    return this.status === 409;
  }
}

const TOKEN_KEY = "registry.token";

/**
 * The token lives in sessionStorage, not localStorage: a credential that
 * outlives the browser tab is a credential nobody remembers leaving behind.
 * "Remember" is an explicit choice, and it says so in the UI.
 */
export const tokenStore = {
  get(): string {
    return sessionStorage.getItem(TOKEN_KEY) ?? localStorage.getItem(TOKEN_KEY) ?? "";
  },
  set(token: string, remember: boolean): void {
    // A browser can refuse to store — private modes, partitioned storage, a
    // full quota. Failing loudly here is the difference between "the console
    // told me why" and "the button does nothing".
    sessionStorage.setItem(TOKEN_KEY, token);
    if (remember) {
      localStorage.setItem(TOKEN_KEY, token);
    } else {
      localStorage.removeItem(TOKEN_KEY);
    }
  },
  clear(): void {
    sessionStorage.removeItem(TOKEN_KEY);
    localStorage.removeItem(TOKEN_KEY);
  },
};

export interface RequestOptions {
  method?: string;
  body?: unknown;
  /** Sent as If-Match, for endpoints with optimistic concurrency. */
  ifMatch?: string;
  signal?: AbortSignal;
  /**
   * Use this credential instead of the stored one. Signing in needs it: a
   * token has to be checked before it is kept, or a bad paste is stored and
   * the console then says "not signed in" with no idea why.
   */
  token?: string;
}

export interface Envelope<T> {
  data: T;
  etag?: string;
}

const API_BASE = "/api/v1";

async function call<T>(path: string, options: RequestOptions = {}): Promise<Envelope<T>> {
  const headers: Record<string, string> = { Accept: "application/json" };
  const token = options.token ?? tokenStore.get();
  if (token) headers.Authorization = `Bearer ${token}`;
  if (options.body !== undefined) headers["Content-Type"] = "application/json";
  if (options.ifMatch) headers["If-Match"] = `"${options.ifMatch}"`;

  const response = await fetch(`${API_BASE}${path}`, {
    method: options.method ?? "GET",
    headers,
    body: options.body === undefined ? undefined : JSON.stringify(options.body),
    signal: options.signal,
  });

  const etag = response.headers.get("ETag")?.replace(/"/g, "") ?? undefined;
  const text = await response.text();
  let payload: unknown = undefined;
  if (text) {
    try {
      payload = JSON.parse(text);
    } catch {
      payload = { error: text };
    }
  }

  if (!response.ok) {
    const message =
      (payload as { error?: string } | undefined)?.error ??
      `${response.status} ${response.statusText}`;
    throw new ApiError(response.status, message, etag);
  }
  return { data: payload as T, etag };
}

export const api = {
  get: <T,>(path: string, signal?: AbortSignal) => call<T>(path, { signal }),
  /** getAs is get with an explicit credential, used to verify one. */
  getAs: <T,>(path: string, token: string) => call<T>(path, { token }),
  post: <T,>(path: string, body: unknown) => call<T>(path, { method: "POST", body }),
  put: <T,>(path: string, body: unknown, ifMatch?: string) =>
    call<T>(path, { method: "PUT", body, ifMatch }),
  putRaw: <T,>(path: string, body: string, ifMatch?: string) =>
    callRaw<T>(path, body, ifMatch),
  del: <T,>(path: string, ifMatch?: string) => call<T>(path, { method: "DELETE", ifMatch }),
};

/** putRaw sends a document verbatim — the config endpoint takes YAML. */
async function callRaw<T>(path: string, body: string, ifMatch?: string): Promise<Envelope<T>> {
  const headers: Record<string, string> = {
    Accept: "application/json",
    "Content-Type": "application/yaml",
  };
  const token = tokenStore.get();
  if (token) headers.Authorization = `Bearer ${token}`;
  if (ifMatch) headers["If-Match"] = `"${ifMatch}"`;

  const response = await fetch(`${API_BASE}${path}`, { method: "PUT", headers, body });
  const etag = response.headers.get("ETag")?.replace(/"/g, "") ?? undefined;
  const text = await response.text();
  let payload: unknown = undefined;
  if (text) {
    try {
      payload = JSON.parse(text);
    } catch {
      payload = { error: text };
    }
  }
  if (!response.ok) {
    const message =
      (payload as { error?: string } | undefined)?.error ??
      `${response.status} ${response.statusText}`;
    throw new ApiError(response.status, message, etag);
  }
  return { data: payload as T, etag };
}
