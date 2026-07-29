import { usePolledResource } from "../api/hooks";
import type { ReplicationStatus } from "../api/types";
import { Badge, Card, ErrorNotice, Empty, Loading, age, short } from "../components/common";

export function Replication() {
  const status = usePolledResource<ReplicationStatus>("/replication", 5_000);

  if (status.loading && !status.data) return <Loading what="replication status" />;
  const data = status.data;

  return (
    <div className="stack">
      <header>
        <h2>Replication</h2>
        <p>Every stream this site pulls, how far behind it is, and what it cannot apply.</p>
      </header>

      <ErrorNotice error={status.error} />

      {!data?.enabled ? (
        <Empty>Replication is not enabled on this site.</Empty>
      ) : (
        <>
          <div className="cards">
            <Card label="Site" value={data.site} />
            <Card label="Streams" value={data.cursors.length} />
            <Card
              label="Parked events"
              value={
                data.parked > 0 ? <Badge kind="warn">{data.parked}</Badge> : <Badge kind="ok">0</Badge>
              }
              hint={data.parked > 0 ? "retried every poll cycle" : undefined}
            />
          </div>

          <div>
            <h3>Streams</h3>
            {data.cursors.length === 0 ? (
              <Empty>No peer has been reached yet.</Empty>
            ) : (
              <div className="table-wrap">
                <table>
                  <thead>
                    <tr>
                      <th>Peer</th>
                      <th>Origin</th>
                      <th>Applied</th>
                      <th>Durable (RPO)</th>
                      <th>Head</th>
                      <th>Last contact</th>
                      <th>State</th>
                    </tr>
                  </thead>
                  <tbody>
                    {data.cursors.map((cursor) => {
                      const head = data.heads[cursor.origin];
                      const lag = head === undefined ? undefined : head - cursor.applied_seq;
                      return (
                        <tr key={`${cursor.peer}/${cursor.origin}`}>
                          <td>{cursor.peer}</td>
                          <td>{cursor.origin}</td>
                          <td>
                            {cursor.applied_seq}
                            {lag !== undefined && lag > 0 ? (
                              <span className="muted"> ({lag} behind)</span>
                            ) : null}
                          </td>
                          <td>
                            {cursor.durable_seq}
                            {cursor.applied_seq > cursor.durable_seq ? (
                              <span className="muted">
                                {" "}
                                ({cursor.applied_seq - cursor.durable_seq} without local blobs)
                              </span>
                            ) : null}
                          </td>
                          <td>{head ?? "—"}</td>
                          <td title={cursor.last_ok_at}>{age(cursor.last_ok_at)}</td>
                          <td>
                            {cursor.last_error ? (
                              <span title={cursor.last_error}>
                                <Badge kind="bad">failing</Badge>
                              </span>
                            ) : (
                              <Badge kind="ok">ok</Badge>
                            )}
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
            )}
          </div>

          {data.cursors.some((c) => c.last_error) ? (
            <div className="notice bad">
              {data.cursors.find((c) => c.last_error)?.last_error}
            </div>
          ) : null}

          <div>
            <h3>Pinned peer identities</h3>
            <p className="muted">
              A peer whose UUID changes is a different site under a familiar name; replication
              stops until an operator runs <code>fondaco repl trust-reset</code>.
            </p>
            {data.peers.length === 0 ? (
              <Empty>No peer has completed a handshake yet.</Empty>
            ) : (
              <div className="table-wrap">
                <table>
                  <thead>
                    <tr>
                      <th>Peer</th>
                      <th>Pinned identity</th>
                      <th>Last seen</th>
                    </tr>
                  </thead>
                  <tbody>
                    {data.peers.map((peer) => (
                      <tr key={peer.peer}>
                        <td>{peer.peer}</td>
                        <td className="mono">{short(peer.uuid)}…</td>
                        <td title={peer.last_seen}>{age(peer.last_seen)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </div>
        </>
      )}
    </div>
  );
}
