import { useState } from "react";
import { Link } from "react-router-dom";
import { useResource } from "../api/hooks";
import type { FeedUsage, UsageReport } from "../api/types";
import { Badge, Card, ErrorNotice, Empty, Loading, age, bytes } from "../components/common";
import { TopPackages } from "../components/TopPackages";

/**
 * What every feed holds, and how much it is used.
 *
 * A proxy feed is a cache, and the only interesting question about a cache is
 * whether it is worth what it costs. That needs both halves on one screen:
 * forty gigabytes served two hundred times is a bargain, and the same forty
 * served twice is a disk bill nobody has looked at since it was set up.
 *
 * The numbers come from two places with different guarantees, and the screen
 * says so rather than blending them: downloads are counted as they happen,
 * and storage is whatever the last inventory scan found.
 */
export function Usage() {
  const report = useResource<UsageReport>("/usage");
  const [sort, setSort] = useState<SortKey>("bytes");

  if (report.loading && !report.data) return <Loading what="usage" />;

  const feeds = [...(report.data?.feeds ?? [])].sort(comparators[sort]);
  const totals = report.data?.totals;

  return (
    <div className="stack">
      <header>
        <h2>Usage</h2>
        <p>
          What each feed stores and how much of it goes out again. Proxy feeds are counted
          from what they have actually cached, not from what their upstream offers.
        </p>
      </header>

      <ErrorNotice error={report.error} />

      {report.data && !report.data.scan_enabled ? (
        <div className="notice">
          The inventory scan is switched off (<code>server.usage_scan</code>), so storage is
          unknown rather than zero. Downloads are still counted.
        </div>
      ) : null}

      {totals ? (
        <div className="cards">
          <Card
            label="Stored"
            value={bytes(totals.bytes)}
            hint={`${totals.blobs.toLocaleString()} blobs, each counted once`}
          />
          <Card
            label="Packages"
            value={totals.packages.toLocaleString()}
            hint={`${totals.artifacts.toLocaleString()} artifacts`}
          />
          <Card
            label="Downloads"
            value={totals.downloads.toLocaleString()}
            hint={`${bytes(totals.bytes_served)} served`}
          />
          <Card
            label="Saved by caching"
            value={bytes(totals.bytes_saved)}
            hint={`${bytes(totals.upstream_bytes)} pulled from upstreams`}
          />
        </div>
      ) : null}

      {report.data?.scanned_at ? (
        <p className="muted" style={{ margin: 0, fontSize: 12 }}>
          Inventory last scanned {age(report.data.scanned_at)}. Downloads are current.
        </p>
      ) : null}

      {feeds.length === 0 ? (
        <Empty>Nothing has been stored or served yet.</Empty>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Feed</th>
                <th>Kind</th>
                <SortableHeader label="Packages" name="packages" sort={sort} onSort={setSort} />
                <SortableHeader label="Stored" name="bytes" sort={sort} onSort={setSort} />
                <SortableHeader label="Downloads" name="downloads" sort={sort} onSort={setSort} />
                <SortableHeader label="Served" name="served" sort={sort} onSort={setSort} />
                <th>Cache</th>
                <th>Last used</th>
              </tr>
            </thead>
            <tbody>
              {feeds.map((feed) => (
                <Row key={feed.feed} feed={feed} />
              ))}
            </tbody>
          </table>
        </div>
      )}

      <p className="muted" style={{ fontSize: 12 }}>
        Blobs are content-addressed and shared, so the feed column can add up to more than
        the site holds: a tarball two feeds proxy is stored once. The site figure above is
        the one to bill against.
      </p>

      <TopPackages />
    </div>
  );
}

function Row({ feed }: { feed: FeedUsage }) {
  const proxy = feed.kind === "proxy" || feed.kind === "mixed";
  return (
    <tr>
      <td>
        <Link to={`/ui/feeds/${encodeURIComponent(feed.feed)}`}>{feed.feed}</Link>
        <div className="muted" style={{ fontSize: 12 }}>{feed.format}</div>
      </td>
      <td>
        <Badge kind={feed.kind === "group" ? "warn" : feed.kind === "hosted" ? "ok" : undefined}>
          {feed.kind}
        </Badge>
      </td>
      <td>
        {feed.packages.toLocaleString()}
        <div className="muted" style={{ fontSize: 12 }}>
          {feed.artifacts.toLocaleString()} files
          {feed.aggregated ? " (members)" : ""}
        </div>
      </td>
      <td>
        {bytes(feed.bytes)}
        {feed.shared_bytes > 0 ? (
          <div className="muted" style={{ fontSize: 12 }} title="Also held by another feed, so deleting this one would not free it">
            {bytes(feed.shared_bytes)} shared
          </div>
        ) : null}
      </td>
      <td>{feed.downloads.toLocaleString()}</td>
      <td>
        {bytes(feed.bytes_served)}
        {proxy && feed.bytes_saved > 0 ? (
          <div className="muted" style={{ fontSize: 12 }}>{bytes(feed.bytes_saved)} saved</div>
        ) : null}
      </td>
      <td>
        {feed.hit_ratio === undefined ? (
          <span className="muted">—</span>
        ) : (
          <Badge kind={feed.hit_ratio >= 0.8 ? "ok" : feed.hit_ratio >= 0.5 ? "warn" : "bad"}>
            {Math.round(feed.hit_ratio * 100)}% hit
          </Badge>
        )}
      </td>
      <td className="muted" style={{ fontSize: 12 }}>
        {feed.last_download_at ? age(feed.last_download_at) : "never"}
      </td>
    </tr>
  );
}

type SortKey = "packages" | "bytes" | "downloads" | "served";

const comparators: Record<SortKey, (a: FeedUsage, b: FeedUsage) => number> = {
  packages: (a, b) => b.packages - a.packages,
  bytes: (a, b) => b.bytes - a.bytes,
  downloads: (a, b) => b.downloads - a.downloads,
  served: (a, b) => b.bytes_served - a.bytes_served,
};

function SortableHeader({
  label,
  name,
  sort,
  onSort,
}: {
  label: string;
  name: SortKey;
  sort: SortKey;
  onSort: (key: SortKey) => void;
}) {
  return (
    <th>
      <button
        className="sortable"
        aria-pressed={sort === name}
        onClick={() => onSort(name)}
        title={`Sort by ${label.toLowerCase()}`}
      >
        {label}
        {sort === name ? " ↓" : ""}
      </button>
    </th>
  );
}
