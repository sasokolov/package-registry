import { useEffect, useState } from "react";
import { ApiError, api } from "../api/client";
import { useResource } from "../api/hooks";
import type { ConfigDocument } from "../api/types";
import { ErrorNotice, Loading } from "../components/common";

/**
 * The configuration is one YAML document, and this edits it as one. A form
 * per setting would be a second model of the schema that drifts from the
 * real one; the document IS the interface, and the registry validates it
 * before it is stored, so a mistake here is a rejection, never a broken
 * site.
 */
export function Configuration() {
  const config = useResource<ConfigDocument>("/config");
  const [draft, setDraft] = useState("");
  const [version, setVersion] = useState("");
  const [error, setError] = useState<ApiError>();
  const [saved, setSaved] = useState<string>();
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (config.data) {
      setDraft(config.data.document);
      setVersion(config.data.version);
    }
  }, [config.data]);

  const dirty = Boolean(config.data && draft !== config.data.document);

  async function save() {
    setBusy(true);
    setError(undefined);
    setSaved(undefined);
    try {
      const result = await api.putRaw<{ version: string }>("/config", draft, version);
      setSaved(`Saved as ${result.data.version.slice(0, 12)}…`);
      config.reload();
    } catch (err) {
      setError(err instanceof ApiError ? err : new ApiError(0, String(err)));
    } finally {
      setBusy(false);
    }
  }

  if (config.loading && !config.data) return <Loading what="configuration" />;

  return (
    <div className="stack">
      <header>
        <h2>Configuration</h2>
        <p>
          The whole document, replaced whole. It is validated before it is stored, so an
          invalid edit is refused rather than half-applied.
        </p>
      </header>

      <ErrorNotice error={config.error ?? error} />
      {saved ? <div className="notice ok">{saved}</div> : null}

      {config.data && !config.data.writable ? (
        <div className="notice">
          This source is read-only. Set <code>config_source.type: store</code> to manage the
          document through the API.
        </div>
      ) : null}

      {error?.conflict ? (
        <div className="notice bad">
          Someone else changed the document since you opened it. Reload to see their version —
          your text is still here, so you can re-apply your edit on top.
        </div>
      ) : null}

      <div className="row muted" style={{ fontSize: 12 }}>
        <span>
          Source: <code>{config.data?.source}</code>
        </span>
        <span>
          Version: <code>{version.slice(0, 12)}…</code>
        </span>
        {dirty ? <span className="right">unsaved changes</span> : null}
      </div>

      <textarea
        rows={28}
        spellCheck={false}
        value={draft}
        onChange={(event) => setDraft(event.target.value)}
      />

      <div className="row">
        <button className="primary" disabled={busy || !dirty} onClick={() => void save()}>
          Save
        </button>
        <button
          disabled={busy || !dirty}
          onClick={() => setDraft(config.data?.document ?? "")}
        >
          Discard changes
        </button>
        <button disabled={busy} onClick={config.reload}>
          Reload
        </button>
      </div>
    </div>
  );
}
