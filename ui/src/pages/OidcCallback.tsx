import { useEffect, useRef, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { completeBrowserSignIn, describe } from "../api/oidc";

/**
 * Where the identity provider sends the browser back.
 *
 * This page exists rather than an API endpoint because the secret half of the
 * flow — the PKCE verifier — is in this tab's sessionStorage and nowhere
 * else. The registry never held it, which is what lets any replica answer the
 * exchange.
 *
 * It runs exactly once. A code is good for a single exchange, and React's
 * development double-render would otherwise spend it on the first pass and
 * report the second as a failure.
 */
export function OidcCallback({ onSignedIn }: { onSignedIn: () => void }) {
  const location = useLocation();
  const navigate = useNavigate();
  const [problem, setProblem] = useState<string>();
  const started = useRef(false);

  useEffect(() => {
    if (started.current) return;
    started.current = true;

    completeBrowserSignIn(location.search)
      .then((returnTo) => {
        onSignedIn();
        // replace, so the back button does not return to a spent code.
        navigate(returnTo, { replace: true });
      })
      .catch((err: unknown) => setProblem(describe(err)));
  }, [location.search, navigate, onSignedIn]);

  if (problem) {
    return (
      <div className="login stack">
        <h2>Sign-in did not finish</h2>
        <div className="notice bad">{problem}</div>
        <div className="row">
          <button className="primary" onClick={() => navigate("/ui/signin", { replace: true })}>
            Try again
          </button>
        </div>
      </div>
    );
  }

  return (
    <div className="login stack">
      <h2>Signing in…</h2>
      <p className="muted">Checking what your identity provider sent back.</p>
    </div>
  );
}
