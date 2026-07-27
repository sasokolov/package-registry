import { NavLink, Outlet } from "react-router-dom";
import type { SiteStatus, WhoAmI } from "../api/types";
import { tokenStore } from "../api/client";

interface Props {
  status: SiteStatus | undefined;
  who: WhoAmI | undefined;
  onSignOut: () => void;
}

export function Layout({ status, who, onSignOut }: Props) {
  const signedIn = Boolean(who && who.kind !== "anonymous");
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
          <NavLink to="/ui/replication">Replication</NavLink>
          <NavLink to="/ui/conflicts">Conflicts</NavLink>
          <NavLink to="/ui/quarantine">Quarantine</NavLink>
          {who?.admin ? <NavLink to="/ui/tokens">Tokens</NavLink> : null}
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
              <div>not signed in</div>
              <NavLink className="signin" to="/ui/signin">
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
