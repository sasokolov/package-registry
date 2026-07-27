import { useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { ApiError, api, tokenStore } from "../api/client";
import type { WhoAmI } from "../api/types";

/**
 * Signing in is pasting a credential. There is no session cookie and no login
 * endpoint on purpose: the console authenticates exactly the way every other
 * client does — one Authorization header — so a credential that works here
 * works with npm and maven too, and revoking it revokes all of them at once.
 * That is also why both credential kinds land in one field: the registry
 * tells a static token from an OIDC id_token by its shape, and so the
 * console does not have to ask.
 *
 * The credential is checked before it is kept. Storing first would mean a
 * truncated paste — easy with a token this long — is saved, the console
 * navigates away, and the sidebar says "not signed in" with nothing anywhere
 * to explain why.
 */
export function SignIn({ onSignedIn }: { onSignedIn: () => void }) {
  const [token, setToken] = useState("");
  const [remember, setRemember] = useState(false);
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string>();
  const field = useRef<HTMLInputElement>(null);
  const navigate = useNavigate();

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
      navigate("/ui/");
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

  return (
    <div className="login stack">
      <div>
        <h2>Sign in</h2>
        <p className="muted">
          Paste a registry token or an OIDC id_token from a configured issuer. It is the same
          credential your CI uses; the console has no separate login.
        </p>
      </div>

      {problem ? <div className="notice bad">{problem}</div> : null}

      <form
        className="stack"
        onSubmit={(event) => {
          event.preventDefault();
          void submit();
        }}
      >
        <label>
          <span>Token or id_token</span>
          <input
            ref={field}
            type="password"
            value={token}
            autoComplete="off"
            placeholder="reg_… or eyJ…"
            onChange={(event) => {
              setToken(event.target.value);
              setProblem(undefined);
            }}
          />
        </label>
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
            Otherwise the token is forgotten when the tab closes.
          </p>
        </div>
        <div className="row">
          {/* Never disabled on an empty field: a button that silently does
              nothing is indistinguishable from a broken one. */}
          <button className="primary" type="submit" disabled={busy}>
            {busy ? "Checking…" : "Sign in"}
          </button>
          <span className="muted">Anonymous browsing works for feeds that allow it.</span>
        </div>
      </form>
    </div>
  );
}
