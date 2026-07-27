import { Link } from "react-router-dom";
import { usePolledResource } from "../api/hooks";
import type { ConflictEntry, ReplicationStatus, SiteStatus } from "../api/types";
import { Badge, Card, ErrorNotice, Loading } from "../components/common";

export function Overview() {
  const status = usePolledResource<SiteStatus>("/status", 10_000);
  const replication = usePolledResource<ReplicationStatus>("/replication", 10_000);
  const conflicts = usePolledResource<{ conflicts: ConflictEntry[] | null }>("/conflicts", 15_000);

  if (status.loading && !status.data) return <Loading what="status" />;

  const site = status.data;
  const repl = replication.data;
  const openConflicts = conflicts.data?.conflicts?.length ?? 0;
  const behind = repl?.cursors.filter((c) => c.last_error).length ?? 0;

  return (
    <div className="stack">
      <header>
        <h2>Overview</h2>
        <p>What this site is serving, and whether anything needs attention.</p>
      </header>

      <ErrorNotice error={status.error} />

      {site ? (
        <div className="cards">
          <Card label="Site" value={site.site} hint={site.config_source} />
          <Card
            label="Feeds"
            value={<Link to="/ui/feeds">{site.feeds}</Link>}
          />
          <Card
            label="Database"
            value={
              <Badge kind={site.database === "up" ? "ok" : site.database === "disabled" ? undefined : "bad"}>
                {site.database}
              </Badge>
            }
            hint={site.database === "unavailable" ? "reads continue from cache" : undefined}
          />
          <Card
            label="Replication"
            value={
              site.replication.enabled ? (
                <Badge kind={behind > 0 ? "warn" : "ok"}>
                  {behind > 0 ? `${behind} stream(s) failing` : "healthy"}
                </Badge>
              ) : (
                <Badge>off</Badge>
              )
            }
            hint={site.replication.enabled ? `${site.replication.peers} peer(s)` : undefined}
          />
          <Card
            label="Open conflicts"
            value={
              openConflicts > 0 ? (
                <Link to="/ui/conflicts">
                  <Badge kind="bad">{openConflicts}</Badge>
                </Link>
              ) : (
                <Badge kind="ok">none</Badge>
              )
            }
            hint={openConflicts > 0 ? "these coordinates are NOT being served" : undefined}
          />
          <Card
            label="Parked events"
            value={
              (repl?.parked ?? 0) > 0 ? (
                <Badge kind="warn">{repl?.parked}</Badge>
              ) : (
                <Badge kind="ok">0</Badge>
              )
            }
            hint={(repl?.parked ?? 0) > 0 ? "retried every poll cycle" : undefined}
          />
        </div>
      ) : null}

      {site?.database === "unavailable" ? (
        <div className="notice">
          The database is unreachable. Downloads keep working from cache and previously
          verified identities keep working for a bounded window; publishing and token
          issuing do not.
        </div>
      ) : null}

      <div className="muted" style={{ fontSize: 12 }}>
        Configuration version <code>{site?.config_version.slice(0, 12)}</code>
      </div>
    </div>
  );
}
