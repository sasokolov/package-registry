import { useState } from "react";
import { useResource } from "../api/hooks";
import type { TopPackage } from "../api/types";
import { Empty, ErrorNotice, Loading, age, bytes } from "./common";

/**
 * What is actually being downloaded.
 *
 * Feed counters say whether a feed is worth its disk; this says what in it is
 * worth keeping — and, read the other way, what a mirror of it would have to
 * contain. Metadata documents are deliberately absent: they are fetched on
 * every resolve, so including them would produce a leaderboard of what people
 * looked up rather than of what they installed.
 *
 * It is a query and never a metric. Coordinates are unbounded, and the only
 * thing anyone wants from them is the top of a sorted list.
 */
export function TopPackages({ feed, limit = 10 }: { feed?: string; limit?: number }) {
  const [expanded, setExpanded] = useState(false);
  const shown = expanded ? 50 : limit;
  const query = `/usage/packages?limit=${shown}${feed ? `&feed=${encodeURIComponent(feed)}` : ""}`;
  const top = useResource<{ packages: TopPackage[] | null }>(query, [feed, shown]);

  if (top.loading && !top.data) return <Loading what="downloads" />;

  const rows = top.data?.packages ?? [];

  return (
    <div className="stack">
      <div className="row">
        <strong>Most downloaded</strong>
        <span className="muted" style={{ fontSize: 12 }}>
          artifacts only — a metadata fetch is not an install
        </span>
        {rows.length >= shown ? (
          <button className="row right" onClick={() => setExpanded(!expanded)}>
            {expanded ? "show fewer" : "show more"}
          </button>
        ) : null}
      </div>

      <ErrorNotice error={top.error} />

      {rows.length === 0 ? (
        <Empty>
          Nothing has been downloaded yet. Counters are flushed on an interval, so a package
          pulled seconds ago may not be here.
        </Empty>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th style={{ width: 40 }}>#</th>
                <th>Coordinate</th>
                {feed ? null : <th>Feed</th>}
                <th>Downloads</th>
                <th>Served</th>
                <th>Last</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((row, index) => (
                <tr key={`${row.feed}/${row.coordinate}`}>
                  <td className="muted">{index + 1}</td>
                  <td className="mono" style={{ wordBreak: "break-all" }}>
                    {row.coordinate}
                  </td>
                  {feed ? null : <td>{row.feed}</td>}
                  <td>{row.downloads.toLocaleString()}</td>
                  <td>{bytes(row.bytes)}</td>
                  <td className="muted" style={{ fontSize: 12 }}>
                    {age(row.last_at)}
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
