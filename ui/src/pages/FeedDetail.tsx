import { useState } from "react";
import { Link, useParams } from "react-router-dom";
import { useResource } from "../api/hooks";
import type { PackageEntry, PackageList } from "../api/types";
import { Badge, ErrorNotice, Empty, Loading, age, bytes, short } from "../components/common";

const PAGE = 50;

export function FeedDetail() {
  const { feed = "" } = useParams();
  const [query, setQuery] = useState("");
  const [applied, setApplied] = useState("");
  const [offset, setOffset] = useState(0);
  const [selected, setSelected] = useState<PackageEntry | undefined>();

  const path = `/feeds/${encodeURIComponent(feed)}/packages?limit=${PAGE}&offset=${offset}${
    applied ? `&q=${encodeURIComponent(applied)}` : ""
  }`;
  const packages = useResource<PackageList>(path);

  const rows = packages.data?.packages ?? [];
  const total = packages.data?.total ?? 0;

  return (
    <div className="stack">
      <header>
        <h2>{feed}</h2>
        <p>
          <Link to="/ui/feeds">← all feeds</Link>
        </p>
      </header>

      <form
        className="row"
        onSubmit={(event) => {
          event.preventDefault();
          setOffset(0);
          setApplied(query.trim());
        }}
      >
        <input
          style={{ maxWidth: 360 }}
          placeholder="filter by path or coordinate"
          value={query}
          onChange={(event) => setQuery(event.target.value)}
        />
        <button type="submit">Search</button>
        {applied ? (
          <button
            type="button"
            onClick={() => {
              setQuery("");
              setApplied("");
              setOffset(0);
            }}
          >
            Clear
          </button>
        ) : null}
        <span className="muted right">{total} coordinate(s)</span>
      </form>

      <ErrorNotice error={packages.error} />
      {packages.loading && !packages.data ? <Loading what="packages" /> : null}

      {!packages.loading && rows.length === 0 && !packages.error ? (
        <Empty>
          {applied
            ? "Nothing matches that filter."
            : "This feed has no hosted packages. Proxied content is cached under the feed's own paths."}
        </Empty>
      ) : null}

      {rows.length > 0 ? (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Path</th>
                <th>Coordinate</th>
                <th>Size</th>
                <th>Published</th>
                <th>By</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {rows.map((pkg) => (
                <tr key={pkg.path}>
                  <td className="mono" style={{ wordBreak: "break-all", maxWidth: 380 }}>
                    {pkg.path}
                    {pkg.quarantined ? (
                      <>
                        {" "}
                        <Badge kind="bad">quarantined</Badge>
                      </>
                    ) : null}
                  </td>
                  <td className="mono">{pkg.coordinate}</td>
                  <td>{bytes(pkg.size)}</td>
                  <td title={pkg.published_at}>{age(pkg.published_at)}</td>
                  <td className="muted">{pkg.published_by || "—"}</td>
                  <td>
                    <button onClick={() => setSelected(pkg)}>Details</button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}

      {total > PAGE ? (
        <div className="row">
          <button disabled={offset === 0} onClick={() => setOffset(Math.max(0, offset - PAGE))}>
            Previous
          </button>
          <span className="muted">
            {offset + 1}–{Math.min(offset + PAGE, total)} of {total}
          </span>
          <button disabled={offset + PAGE >= total} onClick={() => setOffset(offset + PAGE)}>
            Next
          </button>
        </div>
      ) : null}

      {selected ? <PackageDetails pkg={selected} onClose={() => setSelected(undefined)} /> : null}
    </div>
  );
}

function PackageDetails({ pkg, onClose }: { pkg: PackageEntry; onClose: () => void }) {
  return (
    <div className="card stack">
      <div className="row">
        <strong className="mono">{pkg.path}</strong>
        <button className="right" onClick={onClose}>
          Close
        </button>
      </div>
      <table>
        <tbody>
          <tr>
            <th>Coordinate</th>
            <td className="mono">{pkg.coordinate}</td>
          </tr>
          <tr>
            <th>sha256</th>
            <td className="mono" style={{ wordBreak: "break-all" }}>
              {pkg.sha256}
            </td>
          </tr>
          <tr>
            <th>Size</th>
            <td>
              {bytes(pkg.size)} <span className="muted">({pkg.size} bytes)</span>
            </td>
          </tr>
          <tr>
            <th>Published</th>
            <td>
              {pkg.published_at} <span className="muted">({age(pkg.published_at)})</span>
            </td>
          </tr>
          <tr>
            <th>Published by</th>
            <td>{pkg.published_by || "—"}</td>
          </tr>
          <tr>
            <th>Site</th>
            <td>{pkg.site || "—"}</td>
          </tr>
          {pkg.checksums && Object.keys(pkg.checksums).length > 0 ? (
            <tr>
              <th>Checksums</th>
              <td className="mono" style={{ wordBreak: "break-all" }}>
                {Object.entries(pkg.checksums).map(([algo, value]) => (
                  <div key={algo}>
                    {algo}: {short(value)}…
                  </div>
                ))}
              </td>
            </tr>
          ) : null}
          {pkg.metadata && Object.keys(pkg.metadata).length > 0 ? (
            <tr>
              <th>Metadata</th>
              <td>
                {Object.entries(pkg.metadata).map(([key, value]) => (
                  <div key={key}>
                    <span className="muted">{key}:</span> {value}
                  </div>
                ))}
              </td>
            </tr>
          ) : null}
        </tbody>
      </table>
    </div>
  );
}
