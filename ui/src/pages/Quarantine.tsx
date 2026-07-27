import { useState } from "react";
import { ApiError, api } from "../api/client";
import { useResource } from "../api/hooks";
import type { QuarantineEntry, WhoAmI } from "../api/types";
import { Badge, ErrorNotice, Empty, Loading, age } from "../components/common";

export function Quarantine({ who }: { who: WhoAmI | undefined }) {
  const entries = useResource<{ quarantine: QuarantineEntry[] | null }>("/quarantine");
  const [error, setError] = useState<ApiError>();
  const [busy, setBusy] = useState(false);
  const [feed, setFeed] = useState("");
  const [coordinate, setCoordinate] = useState("");
  const [detail, setDetail] = useState("");

  const rows = entries.data?.quarantine ?? [];

  async function act(body: Record<string, unknown>) {
    setBusy(true);
    setError(undefined);
    try {
      await api.post("/quarantine", body);
      entries.reload();
    } catch (err) {
      setError(err instanceof ApiError ? err : new ApiError(0, String(err)));
    } finally {
      setBusy(false);
    }
  }

  if (entries.loading && !entries.data) return <Loading what="quarantine" />;

  return (
    <div className="stack">
      <header>
        <h2>Quarantine</h2>
        <p>
          Blocked coordinates. A block removes access; it never deletes bytes, and it
          replicates to every site.
        </p>
      </header>

      <ErrorNotice error={entries.error ?? error} />

      {rows.length === 0 ? (
        <Empty>Nothing is quarantined.</Empty>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Coordinate</th>
                <th>Feed</th>
                <th>Reason</th>
                <th>Since</th>
                {who?.admin ? <th /> : null}
              </tr>
            </thead>
            <tbody>
              {rows.map((entry) => (
                <tr key={`${entry.feed}/${entry.coordinate}/${entry.reason}`}>
                  <td className="mono">{entry.coordinate}</td>
                  <td>{entry.feed}</td>
                  <td>
                    <Badge kind={entry.reason === "cross_site_conflict" ? "bad" : "warn"}>
                      {entry.reason}
                    </Badge>
                    {entry.detail ? (
                      <div className="muted" style={{ fontSize: 12 }}>
                        {entry.detail}
                      </div>
                    ) : null}
                  </td>
                  <td title={entry.created_at}>{age(entry.created_at)}</td>
                  {who?.admin ? (
                    <td>
                      {entry.reason === "cross_site_conflict" ? (
                        <span className="muted" style={{ fontSize: 12 }}>
                          lifts when the conflict is resolved
                        </span>
                      ) : (
                        <button
                          disabled={busy}
                          onClick={() =>
                            act({
                              feed: entry.feed,
                              coordinate: entry.coordinate,
                              reason: entry.reason,
                              active: false,
                            })
                          }
                        >
                          Release
                        </button>
                      )}
                    </td>
                  ) : null}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {who?.admin ? (
        <form
          className="card stack"
          onSubmit={(event) => {
            event.preventDefault();
            if (!feed.trim() || !coordinate.trim()) return;
            void act({
              feed: feed.trim(),
              coordinate: coordinate.trim(),
              reason: "manual",
              detail: detail.trim(),
              active: true,
            }).then(() => {
              setCoordinate("");
              setDetail("");
            });
          }}
        >
          <strong>Block a coordinate</strong>
          <div className="row" style={{ alignItems: "flex-end" }}>
            <label style={{ flex: "1 1 160px", marginBottom: 0 }}>
              <span>Feed</span>
              <input value={feed} onChange={(e) => setFeed(e.target.value)} placeholder="hosted" />
            </label>
            <label style={{ flex: "2 1 280px", marginBottom: 0 }}>
              <span>Coordinate</span>
              <input
                value={coordinate}
                onChange={(e) => setCoordinate(e.target.value)}
                placeholder="maven:com.example:lib@1.0.0"
              />
            </label>
            <label style={{ flex: "2 1 280px", marginBottom: 0 }}>
              <span>Why (kept in the audit log)</span>
              <input value={detail} onChange={(e) => setDetail(e.target.value)} />
            </label>
            <button className="primary" type="submit" disabled={busy}>
              Block
            </button>
          </div>
        </form>
      ) : null}
    </div>
  );
}
