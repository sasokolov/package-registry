import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { tokenStore } from "../api/client";

/**
 * Signing in is pasting a credential. There is no session cookie and no login
 * endpoint on purpose: the console authenticates exactly the way every other
 * client does — one Authorization header — so a credential that works here
 * works with npm and maven too, and revoking it revokes all of them at once.
 * That is also why both credential kinds land in one field: the registry
 * tells a static token from an OIDC id_token by its shape, and so the
 * console does not have to ask.
 */
export function SignIn({ onSignedIn }: { onSignedIn: () => void }) {
  const [token, setToken] = useState("");
  const [remember, setRemember] = useState(false);
  const navigate = useNavigate();

  return (
    <div className="login stack">
      <div>
        <h2>Sign in</h2>
        <p className="muted">
          Paste a registry token or an OIDC id_token from a configured issuer. It is the same
          credential your CI uses; the console has no separate login.
        </p>
      </div>
      <form
        className="stack"
        onSubmit={(event) => {
          event.preventDefault();
          if (!token.trim()) return;
          tokenStore.set(token.trim(), remember);
          setToken("");
          onSignedIn();
          navigate("/ui/");
        }}
      >
        <label>
          <span>Token or id_token</span>
          <input
            type="password"
            value={token}
            autoComplete="off"
            placeholder="reg_… or eyJ…"
            onChange={(event) => setToken(event.target.value)}
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
          <button className="primary" type="submit" disabled={!token.trim()}>
            Sign in
          </button>
          <span className="muted">Anonymous browsing works for feeds that allow it.</span>
        </div>
      </form>
    </div>
  );
}
