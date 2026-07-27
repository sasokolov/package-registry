import { useState } from "react";
import { ApiError, api } from "../api/client";
import { useResource } from "../api/hooks";
import type { ConflictEntry, WhoAmI } from "../api/types";
import { Badge, ErrorNotice, Empty, Loading, age, short } from "../components/common";

/**
 * A cross-site conflict means two sites published different bytes at one
 * coordinate. The registry picks a canonical digest deterministically and
 * blocks the coordinate everywhere until a human decides — this screen is
 * that decision, and it deliberately shows both sides rather than nudging
 * towards the automatic pick.
 */
export function Conflicts({ who }: { who: WhoAmI | undefined }) {
  const conflicts = useResource<{ conflicts: ConflictEntry[] | null }>("/conflicts");
  const [busy, setBusy] = useState<string>();
  const [error, setError] = useState<ApiError>();
  const [done, setDone] = useState<string>();

  const rows = conflicts.data?.conflicts ?? [];

  async function resolve(conflict: ConflictEntry, keep: string) {
    const key = `${conflict.feed}/${conflict.path}`;
    setBusy(key);
    setError(undefined);
    setDone(undefined);
    try {
      await api.post("/conflicts/resolve", {
        feed: conflict.feed,
        path: conflict.path,
        coordinate: conflict.coordinate,
        keep_sha256: keep,
      });
      setDone(`${conflict.coordinate} resolved to ${short(keep)}…`);
      conflicts.reload();
    } catch (err) {
      setError(err instanceof ApiError ? err : new ApiError(0, String(err)));
    } finally {
      setBusy(undefined);
    }
  }

  if (conflicts.loading && !conflicts.data) return <Loading what="conflicts" />;

  return (
    <div className="stack">
      <header>
        <h2>Conflicts</h2>
        <p>
          Coordinates where two sites published different bytes. They are not served until
          resolved, and both blobs are kept.
        </p>
      </header>

      <ErrorNotice error={conflicts.error ?? error} />
      {done ? <div className="notice ok">{done}</div> : null}

      {rows.length === 0 ? (
        <Empty>No open conflicts.</Empty>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Coordinate</th>
                <th>Canonical (automatic pick)</th>
                <th>Other side</th>
                <th>Detected</th>
                {who?.admin ? <th>Resolve as</th> : null}
              </tr>
            </thead>
            <tbody>
              {rows.map((conflict) => {
                const key = `${conflict.feed}/${conflict.path}`;
                return (
                  <tr key={key}>
                    <td>
                      <div className="mono">{conflict.coordinate}</div>
                      <div className="muted mono" style={{ fontSize: 12 }}>
                        {conflict.feed}/{conflict.path}
                      </div>
                    </td>
                    <td>
                      <div className="mono">{short(conflict.canonical_sha256)}…</div>
                      <div className="muted" style={{ fontSize: 12 }}>
                        from {conflict.canonical_site}
                      </div>
                    </td>
                    <td>
                      <div className="mono">{short(conflict.other_sha256)}…</div>
                      <div className="muted" style={{ fontSize: 12 }}>
                        from {conflict.other_site}
                      </div>
                    </td>
                    <td title={conflict.detected_at}>{age(conflict.detected_at)}</td>
                    {who?.admin ? (
                      <td>
                        <div className="row">
                          <button
                            disabled={busy === key}
                            onClick={() => resolve(conflict, conflict.canonical_sha256)}
                          >
                            {conflict.canonical_site}
                          </button>
                          <button
                            disabled={busy === key}
                            onClick={() => resolve(conflict, conflict.other_sha256)}
                          >
                            {conflict.other_site}
                          </button>
                        </div>
                      </td>
                    ) : null}
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}

      {rows.length > 0 && !who?.admin ? (
        <div className="notice">
          Resolving a conflict is an administrator action. <Badge>read only</Badge>
        </div>
      ) : null}
    </div>
  );
}
