import { useEffect, useRef, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { ApiError, api, tokenStore } from "../api/client";
import { useResource } from "../api/hooks";
import { describe, startBrowserSignIn } from "../api/oidc";
import type { AuthMethod, WhoAmI } from "../api/types";

/**
 * Signing in is presenting a credential. There is no session cookie and no
 * login endpoint on purpose: the console authenticates exactly the way every
 * other client does — one Authorization header — so a credential that works
 * here works with npm and maven too, and revoking it revokes all of them at
 * once.
 *
 * Where that credential comes from is the site's answer, not the console's
 * guess. A registry with no database issues no static tokens; one with no
 * trusted issuer accepts no id_tokens; and an issuer this registry is a
 * registered client of can hand one out through the browser, which is a
 * button rather than a field. So the form is built from what /auth/methods
 * reports, and a method the site does not advertise simply is not on it.
 */
export function SignIn({ onSignedIn }: { onSignedIn: () => void }) {
  const methods = useResource<{ methods: AuthMethod[] | null }>("/auth/methods");
  const available = methods.data?.methods ?? [];

  const [selected, setSelected] = useState(0);
  const [token, setToken] = useState("");
  const [remember, setRemember] = useState(false);
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string>();
  const field = useRef<HTMLInputElement>(null);
  const navigate = useNavigate();
  const location = useLocation();

  // Where to come back to. Being sent to the sign-in screen from a page and
  // landing on the overview afterwards is a small theft of context.
  const returnTo = (location.state as { from?: string } | null)?.from ?? "/ui/";

  // Selecting a method that has since disappeared would leave the form with
  // no labels at all.
  useEffect(() => {
    if (selected >= available.length) setSelected(0);
  }, [available.length, selected]);

  const method = available[selected];
  const browserFlow = method?.flow === "browser";

  async function submit() {
    // The field is controlled, but a password manager or a middle-click
    // paste can fill an input without React ever seeing it. Reading the
    // element as a fallback costs one line and removes a failure where the
    // button appears to do nothing at all.
    const credential = (token || field.current?.value || "").trim();
    if (!credential) {
      setProblem("Paste a credential first.");
      return;
    }
    setBusy(true);
    setProblem(undefined);
    try {
      const { data: who } = await api.getAs<WhoAmI>("/whoami", credential);
      if (who.kind === "anonymous") {
        setProblem(
          who.auth_error
            ? `The registry did not accept this credential: ${who.auth_error}`
            : "The registry did not accept this credential. Check that the whole token was " +
              "pasted — they are long and easy to truncate.",
        );
        return;
      }
      try {
        tokenStore.set(credential, remember);
      } catch (err) {
        setProblem(
          "This browser refused to store the credential " +
            `(${err instanceof Error ? err.message : String(err)}). ` +
            "Private browsing or blocked site data will do that.",
        );
        return;
      }
      setToken("");
      if (field.current) field.current.value = "";
      onSignedIn();
      navigate(returnTo);
    } catch (err) {
      setProblem(
        err instanceof ApiError
          ? `The registry answered ${err.status}: ${err.message}`
          : `Could not reach the registry: ${String(err)}`,
      );
    } finally {
      setBusy(false);
    }
  }

  async function redirect() {
    if (!method) return;
    setBusy(true);
    setProblem(undefined);
    try {
      // This navigates away, so there is no success path to handle here —
      // only the failure to get as far as the issuer.
      await startBrowserSignIn(method, remember, returnTo);
    } catch (err) {
      setProblem(`Could not start the sign-in: ${describe(err)}`);
      setBusy(false);
    }
  }

  return (
    <div className="login stack">
      <div>
        <h2>Sign in</h2>
        <p className="muted">
          The console has no separate login: it presents the same credential your clients do.
        </p>
      </div>

      {methods.loading && !methods.data ? <p className="muted">Loading sign-in methods…</p> : null}

      {!methods.loading && available.length === 0 ? (
        <div className="notice">
          This site advertises no sign-in method. Static tokens need a database, and id_tokens
          need a trusted issuer in <code>auth.oidc_issuers</code>; a site with neither can only
          be browsed anonymously.
        </div>
      ) : null}

      {available.length > 1 ? (
        <div className="methods" role="tablist" aria-label="Sign-in method">
          {available.map((m, index) => (
            <button
              key={`${m.type}:${m.issuer ?? ""}`}
              role="tab"
              aria-selected={index === selected}
              className={index === selected ? "method selected" : "method"}
              onClick={() => {
                setSelected(index);
                setProblem(undefined);
              }}
            >
              {m.label ?? m.type}
            </button>
          ))}
        </div>
      ) : null}

      {problem ? <div className="notice bad">{problem}</div> : null}

      {method ? (
        <form
          className="stack"
          onSubmit={(event) => {
            event.preventDefault();
            if (browserFlow) void redirect();
            else void submit();
          }}
        >
          {browserFlow ? (
            <p className="muted" style={{ margin: 0 }}>
              {method.help}
            </p>
          ) : (
            <>
              <label>
                <span>{method.label ?? method.type}</span>
                <input
                  ref={field}
                  type="password"
                  value={token}
                  autoComplete="off"
                  placeholder={method.type === "oidc" ? "eyJ…" : "reg_…"}
                  onChange={(event) => {
                    setToken(event.target.value);
                    setProblem(undefined);
                  }}
                />
              </label>
              {method.help ? (
                <p className="muted" style={{ margin: 0, fontSize: 12 }}>
                  {method.help}
                </p>
              ) : null}
            </>
          )}
          {method.issuer ? (
            <p className="muted" style={{ margin: 0, fontSize: 12 }}>
              Issuer: <code>{method.issuer}</code>
            </p>
          ) : null}

          <div className="stack" style={{ gap: 4 }}>
            <label className="row" style={{ gap: 6, flexWrap: "nowrap" }}>
              <input
                type="checkbox"
                checked={remember}
                style={{ width: "auto" }}
                onChange={(event) => setRemember(event.target.checked)}
              />
              <span style={{ margin: 0, fontSize: 14, color: "inherit" }}>
                Keep me signed in on this browser
              </span>
            </label>
            <p className="muted" style={{ margin: 0, fontSize: 12 }}>
              Otherwise the credential is forgotten when the tab closes.
            </p>
          </div>

          <div className="row">
            {/* Never disabled on an empty field: a button that silently does
                nothing is indistinguishable from a broken one. */}
            <button className="primary" type="submit" disabled={busy}>
              {busy
                ? browserFlow
                  ? "Redirecting…"
                  : "Checking…"
                : browserFlow
                  ? (method.label ?? "Sign in")
                  : "Sign in"}
            </button>
            <span className="muted">Anonymous browsing works for feeds that allow it.</span>
          </div>
        </form>
      ) : null}
    </div>
  );
}
