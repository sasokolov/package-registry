import { NavLink, Outlet, useLocation } from "react-router-dom";
import type { SiteStatus, WhoAmI } from "../api/types";
import { tokenStore } from "../api/client";

interface Props {
  status: SiteStatus | undefined;
  who: WhoAmI | undefined;
  onSignOut: () => void;
}

export function Layout({ status, who, onSignOut }: Props) {
  const signedIn = Boolean(who && who.kind !== "anonymous");
  const location = useLocation();
  // A sign-in from an identity provider ends; saying so beats letting the
  // sidebar read "browsing anonymously" as though nothing had happened.
  const expired = !signedIn && tokenStore.expired();
  return (
    <div className="layout">
      <aside className="sidebar">
        <h1>
          Package registry
          <small>{status ? status.site : "…"}</small>
        </h1>
        <nav>
          <NavLink to="/ui/" end>Overview</NavLink>
          <NavLink to="/ui/feeds">Feeds</NavLink>
          {/* Operational views need an identity, so a signed-out visitor is
              not offered a link that can only answer 401. */}
          {signedIn ? (
            <>
              <NavLink to="/ui/replication">Replication</NavLink>
              <NavLink to="/ui/conflicts">Conflicts</NavLink>
              <NavLink to="/ui/quarantine">Quarantine</NavLink>
            </>
          ) : null}
          {who?.admin ? <NavLink to="/ui/tokens">Tokens</NavLink> : null}
          {who?.admin ? <NavLink to="/ui/access">Access</NavLink> : null}
          {who?.admin ? <NavLink to="/ui/config">Configuration</NavLink> : null}
        </nav>
        <div className="spacer" />
        <div className="muted" style={{ padding: "8px 10px", fontSize: 12 }}>
          {signedIn ? (
            <>
              <div>
                {who?.kind}:{who?.subject}
              </div>
              {who?.admin ? <div>administrator</div> : null}
              {who?.stale ? (
                <div title="The token backend is unreachable; this identity was reused from cache.">
                  cached identity
                </div>
              ) : null}
              <button
                style={{ marginTop: 8 }}
                onClick={() => {
                  tokenStore.clear();
                  onSignOut();
                }}
              >
                Sign out
              </button>
            </>
          ) : (
            <>
              {expired ? (
                <div className="refused">sign-in expired</div>
              ) : who?.auth_error ? (
                <div className="refused" title={who.auth_error}>
                  credential refused
                </div>
              ) : (
                <div>browsing anonymously</div>
              )}
              <div style={{ marginTop: 2 }}>
                {expired
                  ? "Your identity provider's token has run out. Signing in again usually takes one click."
                  : who?.auth_error
                    ? "The registry did not accept the stored credential."
                    : "Open feeds only. Sign in to see the rest."}
              </div>
              <NavLink
                className="signin"
                to="/ui/signin"
                state={{ from: location.pathname + location.search }}
              >
                Sign in
              </NavLink>
            </>
          )}
        </div>
      </aside>
      <main className="content">
        <Outlet />
      </main>
    </div>
  );
}
