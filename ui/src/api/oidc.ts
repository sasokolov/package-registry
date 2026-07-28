// Signing in through an identity provider.
//
// The browser owns the halves of the flow that must never leave it: the PKCE
// verifier, the state that proves the redirect answers this request, and the
// nonce that ties the token to it. The registry owns the rest — which issuers
// exist, where they are, what client this registry is — so this file never
// assembles an authorization URL by hand. It asks for one.
//
// Everything in flight lives in sessionStorage, which is exactly the lifetime
// it should have: one tab, until the flow finishes.

import { ApiError, api, tokenStore } from "./client";
import type { AuthMethod } from "./types";

const PENDING_KEY = "registry.oidc.pending";

interface Pending {
  issuer: string;
  state: string;
  nonce: string;
  verifier: string;
  remember: boolean;
  /** Where the person was going before they were asked to sign in. */
  returnTo: string;
}

interface ExchangeResult {
  id_token: string;
  expires_at?: string;
  identity: string;
  subject: string;
  issuer: string;
}

/** The callback route, which must match what the registry registers. */
export const CALLBACK_PATH = "/ui/oidc/callback";

/**
 * Starts a sign-in: generates the secrets, asks the registry where to go,
 * and goes there. It does not return — the page navigates away.
 */
export async function startBrowserSignIn(
  method: AuthMethod,
  remember: boolean,
  returnTo: string,
): Promise<void> {
  if (!method.issuer) {
    throw new Error("This sign-in method names no issuer, so there is nowhere to send you.");
  }

  const verifier = randomString(64);
  const pending: Pending = {
    issuer: method.issuer,
    state: randomString(32),
    nonce: randomString(32),
    verifier,
    remember,
    returnTo,
  };
  const challenge = await codeChallenge(verifier);

  // Written before navigating, or the callback comes back to nothing.
  sessionStorage.setItem(PENDING_KEY, JSON.stringify(pending));

  const query = new URLSearchParams({
    issuer: pending.issuer,
    state: pending.state,
    nonce: pending.nonce,
    code_challenge: challenge,
  });
  const { data } = await api.get<{ authorization_url: string }>(
    `/auth/oidc/authorize?${query.toString()}`,
  );

  window.location.assign(data.authorization_url);
}

/**
 * Finishes a sign-in from the callback URL. Returns where to go next.
 *
 * Throws with something a person can act on: an issuer that refused, a
 * redirect that does not answer any flow this tab started, a registry that
 * would not accept the token it was given.
 */
export async function completeBrowserSignIn(search: string): Promise<string> {
  const params = new URLSearchParams(search);
  const raw = sessionStorage.getItem(PENDING_KEY);
  // Read once and drop: a code is good for one exchange, and leaving the
  // verifier behind after a failure invites replaying it.
  sessionStorage.removeItem(PENDING_KEY);

  const problem = params.get("error");
  if (problem) {
    throw new Error(
      `${params.get("error_description") ?? "The identity provider refused the sign-in"} (${problem})`,
    );
  }

  if (!raw) {
    throw new Error(
      "This tab did not start a sign-in, so there is nothing to finish. " +
        "Opening the callback URL directly, or in a second tab, does that.",
    );
  }
  const pending = JSON.parse(raw) as Pending;

  const state = params.get("state");
  if (!state || state !== pending.state) {
    throw new Error(
      "The identity provider came back with a different sign-in than the one this tab started.",
    );
  }

  const code = params.get("code");
  if (!code) {
    throw new Error("The identity provider came back without an authorization code.");
  }

  const { data } = await api.post<ExchangeResult>("/auth/oidc/exchange", {
    issuer: pending.issuer,
    code,
    code_verifier: pending.verifier,
    nonce: pending.nonce,
  });

  tokenStore.set(data.id_token, pending.remember, data.expires_at);
  return pending.returnTo || "/ui/";
}

/** Whether a sign-in is waiting to be finished in this tab. */
export function signInPending(): boolean {
  return sessionStorage.getItem(PENDING_KEY) !== null;
}

/** Turns an ApiError into the sentence to show. */
export function describe(error: unknown): string {
  if (error instanceof ApiError) return error.message;
  if (error instanceof Error) return error.message;
  return String(error);
}

/** randomString returns base64url of n random bytes — no padding, per RFC 7636. */
function randomString(bytes: number): string {
  const buffer = new Uint8Array(bytes);
  crypto.getRandomValues(buffer);
  return base64url(buffer);
}

async function codeChallenge(verifier: string): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(verifier));
  return base64url(new Uint8Array(digest));
}

function base64url(bytes: Uint8Array): string {
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}
