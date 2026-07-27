import { Link } from "react-router-dom";
import { useResource } from "../api/hooks";
import type { FeedSummary } from "../api/types";
import { Badge, ErrorNotice, Empty, Loading } from "../components/common";

export function Feeds() {
  const feeds = useResource<{ feeds: FeedSummary[] | null }>("/feeds");

  if (feeds.loading && !feeds.data) return <Loading what="feeds" />;

  const rows = feeds.data?.feeds ?? [];
  return (
    <div className="stack">
      <header>
        <h2>Feeds</h2>
        <p>Every configured feed, what it proxies and what it hosts.</p>
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
              </tr>
            </thead>
            <tbody>
              {rows.map((feed) => (
                <tr key={feed.name}>
                  <td>
                    <Link to={`/ui/feeds/${encodeURIComponent(feed.name)}`}>{feed.name}</Link>
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
                  <td>{feed.hosted ? feed.packages : <span className="muted">—</span>}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
