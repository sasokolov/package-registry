import { Link } from "react-router-dom";
import { useResource } from "../api/hooks";
import type { FeedSummary } from "../api/types";
import { Badge, ErrorNotice, Empty, Loading, bytes } from "../components/common";

export function Feeds({ anonymous }: { anonymous: boolean }) {
  const feeds = useResource<{ feeds: FeedSummary[] | null }>("/feeds", [anonymous]);

  if (feeds.loading && !feeds.data) return <Loading what="feeds" />;

  const rows = feeds.data?.feeds ?? [];
  return (
    <div className="stack">
      <header>
        <h2>Feeds</h2>
        <p>Every configured feed, what it proxies and what it hosts.</p>
        {anonymous ? (
          <p className="muted" style={{ fontSize: 12 }}>
            Signed out: this lists only the feeds open to everyone, without their
            configuration.
          </p>
        ) : null}
      </header>
      <ErrorNotice error={feeds.error} />
      {rows.length === 0 ? (
        <Empty>No feeds configured.</Empty>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Feed</th>
                <th>Format</th>
                <th>Upstream</th>
                <th>Mode</th>
                <th>Access</th>
                <th>Packages</th>
                <th>Stored</th>
                <th>Downloads</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((feed) => (
                <tr key={feed.name}>
                  <td>
                    <Link to={`/ui/feeds/${encodeURIComponent(feed.name)}`}>{feed.name}</Link>
                    {feed.members?.length ? (
                      <div className="muted" style={{ fontSize: 12 }}>
                        {/* Order is meaningful: the first member that has a
                            coordinate wins it. */}
                        members: {feed.members.join(" → ")}
                      </div>
                    ) : null}
                    {feed.policies?.length ? (
                      <div className="muted" style={{ fontSize: 12 }}>
                        policies: {feed.policies.join(", ")}
                      </div>
                    ) : null}
                  </td>
                  <td>{feed.format}</td>
                  <td className="mono" style={{ maxWidth: 320, wordBreak: "break-all" }}>
                    {feed.upstream ?? <span className="muted">—</span>}
                  </td>
                  <td>
                    <div className="row" style={{ gap: 4 }}>
                      {feed.group ? <Badge kind="warn">group</Badge> : null}
                      {feed.hosted ? <Badge kind="ok">hosted</Badge> : null}
                      {feed.upstream ? <Badge>proxy</Badge> : null}
                      {feed.publish_policy ? <Badge>{feed.publish_policy}</Badge> : null}
                      {feed.peer_fallback ? <Badge>peer fallback</Badge> : null}
                    </div>
                  </td>
                  <td>
                    {feed.anonymous ? (
                      <Badge>anonymous</Badge>
                    ) : (
                      <Badge kind="warn">authenticated</Badge>
                    )}
                  </td>
                  <td>
                    {/* The count covers what a proxy has cached as well as
                        what a feed hosts, so a proxy is no longer a dash. */}
                    {feed.usage ? (
                      feed.usage.packages.toLocaleString()
                    ) : feed.hosted && feed.packages !== undefined ? (
                      feed.packages
                    ) : (
                      <span className="muted">—</span>
                    )}
                  </td>
                  <td>
                    {feed.usage ? bytes(feed.usage.bytes) : <span className="muted">—</span>}
                  </td>
                  <td>
                    {feed.usage ? (
                      feed.usage.downloads.toLocaleString()
                    ) : (
                      <span className="muted">—</span>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
