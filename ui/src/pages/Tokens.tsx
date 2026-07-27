import { useState } from "react";
import { ApiError, api } from "../api/client";
import { useResource } from "../api/hooks";
import type { TokenInfo } from "../api/types";
import { Badge, ErrorNotice, Empty, Loading, age } from "../components/common";

export function Tokens() {
  const tokens = useResource<{ tokens: TokenInfo[] | null }>("/tokens");
  const [name, setName] = useState("");
  const [issued, setIssued] = useState<{ name: string; secret: string }>();
  const [error, setError] = useState<ApiError>();
  const [busy, setBusy] = useState(false);

  const rows = tokens.data?.tokens ?? [];

  async function create() {
    setBusy(true);
    setError(undefined);
    try {
      const result = await api.post<{ name: string; secret: string }>("/tokens", {
        name: name.trim(),
      });
      setIssued(result.data);
      setName("");
      tokens.reload();
    } catch (err) {
      setError(err instanceof ApiError ? err : new ApiError(0, String(err)));
    } finally {
      setBusy(false);
    }
  }

  async function revoke(tokenName: string) {
    setBusy(true);
    setError(undefined);
    try {
      await api.del(`/tokens/${encodeURIComponent(tokenName)}`);
      tokens.reload();
    } catch (err) {
      setError(err instanceof ApiError ? err : new ApiError(0, String(err)));
    } finally {
      setBusy(false);
    }
  }

  if (tokens.loading && !tokens.data) return <Loading what="tokens" />;

  return (
    <div className="stack">
      <header>
        <h2>Tokens</h2>
        <p>
          Static tokens, by name. The secret exists only in the response that issues it — the
          registry stores a hash and nothing else.
        </p>
      </header>

      <ErrorNotice error={tokens.error ?? error} />

      {issued ? (
        <div className="card stack">
          <strong>Token “{issued.name}” issued</strong>
          <p className="muted">
            Copy it now. It is not recoverable: the registry kept only its hash.
          </p>
          <div className="secret">{issued.secret}</div>
          <div className="row">
            <button onClick={() => void navigator.clipboard?.writeText(issued.secret)}>
              Copy
            </button>
            <button onClick={() => setIssued(undefined)}>Done</button>
          </div>
        </div>
      ) : null}

      <form
        className="row"
        onSubmit={(event) => {
          event.preventDefault();
          if (name.trim()) void create();
        }}
      >
        <input
          style={{ maxWidth: 300 }}
          placeholder="new token name, e.g. ci-frontend"
          value={name}
          onChange={(event) => setName(event.target.value)}
        />
        <button className="primary" type="submit" disabled={busy || !name.trim()}>
          Issue token
        </button>
      </form>

      {rows.length === 0 ? (
        <Empty>No tokens yet.</Empty>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Name</th>
                <th>Hash</th>
                <th>Created</th>
                <th>State</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {rows.map((token) => (
                <tr key={token.name}>
                  <td>{token.name}</td>
                  <td className="mono">{token.hash_prefix}…</td>
                  <td title={token.created_at}>{age(token.created_at)}</td>
                  <td>
                    {token.revoked_at ? (
                      <Badge kind="bad">revoked {age(token.revoked_at)}</Badge>
                    ) : (
                      <Badge kind="ok">active</Badge>
                    )}
                  </td>
                  <td>
                    {token.revoked_at ? null : (
                      <button
                        className="danger"
                        disabled={busy}
                        onClick={() => void revoke(token.name)}
                      >
                        Revoke
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <p className="muted" style={{ fontSize: 12 }}>
        Revocation replicates to every site and takes effect within the revocation sweep
        window; a site whose database is unreachable keeps honouring already-verified
        identities until its stale-identity window expires.
      </p>
    </div>
  );
}
