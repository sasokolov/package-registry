import { useState } from "react";
import { ApiError, api } from "../api/client";
import { useResource } from "../api/hooks";
import type { AccessRules, Explanation } from "../api/types";
import { Badge, ErrorNotice, Empty, Loading } from "../components/common";

/**
 * What the rules are, and what they would decide.
 *
 * The explain form is the part that earns its place. An access system whose
 * refusals cannot be accounted for is one people route around — a second
 * token, a wider policy, an exception that outlives its reason — so being
 * able to ask "would this be allowed, and which rule says so" before granting
 * anything is what keeps the written rules the real ones.
 */
export function Access() {
  const rules = useResource<AccessRules>("/access");

  if (rules.loading && !rules.data) return <Loading what="access rules" />;

  return (
    <div className="stack">
      <header>
        <h2>Access</h2>
        <p>
          Named policies of path capabilities, and the bindings that attach them to
          identities. Nothing is permitted until a policy says so; a <code>deny</code> beats
          every other capability at the same specificity, and the most specific matching rule
          decides.
        </p>
      </header>

      <ErrorNotice error={rules.error} />
      <Explain capabilities={rules.data?.capabilities ?? []} />
      <Policies rules={rules.data} />
      <Bindings rules={rules.data} />
    </div>
  );
}

function Explain({ capabilities }: { capabilities: string[] }) {
  const [path, setPath] = useState("feed/releases/maven:com.example:lib@1.0.0");
  const [capability, setCapability] = useState("read");
  const [kind, setKind] = useState("");
  const [subject, setSubject] = useState("");
  const [projectPath, setProjectPath] = useState("");
  const [ref, setRef] = useState("");
  const [result, setResult] = useState<Explanation>();
  const [error, setError] = useState<ApiError>();
  const [busy, setBusy] = useState(false);

  async function ask() {
    setBusy(true);
    setError(undefined);
    try {
      const params = new URLSearchParams({ path: path.trim(), capability });
      // Leaving the identity blank asks about yourself, which is the
      // question a developer has; filling it in asks about somebody else,
      // which is the question an operator has before granting anything.
      for (const [key, value] of Object.entries({
        kind,
        subject,
        project_path: projectPath,
        ref,
      })) {
        if (value.trim()) params.set(key, value.trim());
      }
      const { data } = await api.get<Explanation>(`/access/explain?${params.toString()}`);
      setResult(data);
    } catch (err) {
      setError(err instanceof ApiError ? err : new ApiError(0, String(err)));
      setResult(undefined);
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="card stack">
      <strong>Would this be allowed?</strong>
      <form
        className="stack"
        onSubmit={(event) => {
          event.preventDefault();
          void ask();
        }}
      >
        <div className="row" style={{ alignItems: "flex-end" }}>
          <label style={{ flex: "1 1 340px", marginBottom: 0 }}>
            <span>Path</span>
            <input
              className="mono"
              value={path}
              onChange={(event) => setPath(event.target.value)}
              placeholder="feed/&lt;feed&gt;/&lt;coordinate&gt; or sys/&lt;area&gt;"
            />
          </label>
          <label style={{ flex: "0 0 150px", marginBottom: 0 }}>
            <span>Capability</span>
            <select value={capability} onChange={(event) => setCapability(event.target.value)}>
              {(capabilities.length ? capabilities : ["read"]).map((c) => (
                <option key={c} value={c}>
                  {c}
                </option>
              ))}
            </select>
          </label>
        </div>

        <details>
          <summary className="muted" style={{ fontSize: 12, cursor: "pointer" }}>
            Ask about another identity (blank asks about you)
          </summary>
          <div className="row" style={{ marginTop: 8, alignItems: "flex-end" }}>
            <label style={{ flex: "0 0 130px", marginBottom: 0 }}>
              <span>Kind</span>
              <select value={kind} onChange={(event) => setKind(event.target.value)}>
                <option value="">— me —</option>
                <option value="token">token</option>
                <option value="oidc">oidc</option>
                <option value="anonymous">anonymous</option>
              </select>
            </label>
            <label style={{ flex: "1 1 160px", marginBottom: 0 }}>
              <span>Subject</span>
              <input value={subject} onChange={(event) => setSubject(event.target.value)} />
            </label>
            <label style={{ flex: "1 1 160px", marginBottom: 0 }}>
              <span>project_path</span>
              <input
                value={projectPath}
                onChange={(event) => setProjectPath(event.target.value)}
              />
            </label>
            <label style={{ flex: "1 1 160px", marginBottom: 0 }}>
              <span>ref</span>
              <input value={ref} onChange={(event) => setRef(event.target.value)} />
            </label>
          </div>
        </details>

        <div className="row">
          <button className="primary" type="submit" disabled={busy || !path.trim()}>
            {busy ? "Asking…" : "Explain"}
          </button>
        </div>
      </form>

      <ErrorNotice error={error} />

      {result ? (
        <div className={result.allowed ? "notice ok" : "notice bad"}>
          <div>
            <Badge kind={result.allowed ? "ok" : "bad"}>
              {result.allowed ? "allowed" : "refused"}
            </Badge>{" "}
            <code>{result.identity}</code> · <code>{result.capability}</code> on{" "}
            <code>{result.path}</code>
          </div>
          <div style={{ marginTop: 6 }}>{result.reason}</div>
          {result.rule ? (
            <div className="muted" style={{ fontSize: 12, marginTop: 6 }}>
              decided by <code>{result.policy}</code> at <code>{result.rule}</code>
              {result.effective_capabilities?.length
                ? ` — effective: ${result.effective_capabilities.join(", ")}`
                : null}
            </div>
          ) : null}
          {result.policies?.length ? (
            <div className="muted" style={{ fontSize: 12, marginTop: 4 }}>
              bound policies: {result.policies.join(", ")}
              {result.bindings?.length ? <> — via {result.bindings.join(", ")}</> : null}
            </div>
          ) : (
            // Which is the difference between "the policy is wrong" and "the
            // binding never matched", and they are fixed in different places.
            <div className="muted" style={{ fontSize: 12, marginTop: 4 }}>
              No binding matched this identity, so no policy applied.
            </div>
          )}
        </div>
      ) : null}
    </div>
  );
}

function Policies({ rules }: { rules: AccessRules | undefined }) {
  const [showGenerated, setShowGenerated] = useState(false);
  const all = rules?.policies ?? [];
  const written = all.filter((p) => !p.generated);
  const generated = all.filter((p) => p.generated);
  const shown = showGenerated ? all : written;

  return (
    <div className="stack">
      <div className="row">
        <strong>Policies</strong>
        <span className="muted" style={{ fontSize: 12 }}>
          {written.length} written, {generated.length} generated from{" "}
          <code>anonymous</code>/<code>publishers</code>/<code>admins</code>
        </span>
        <label className="row right" style={{ gap: 6, flexWrap: "nowrap", marginBottom: 0 }}>
          <input
            type="checkbox"
            checked={showGenerated}
            style={{ width: "auto" }}
            onChange={(event) => setShowGenerated(event.target.checked)}
          />
          <span style={{ margin: 0, fontSize: 12 }}>show generated</span>
        </label>
      </div>

      {shown.length === 0 ? (
        <Empty>No policies are written by hand. Everything in force is generated.</Empty>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Policy</th>
                <th>Path</th>
                <th>Capabilities</th>
              </tr>
            </thead>
            <tbody>
              {shown.flatMap((policy) =>
                policy.rules.map((rule, index) => (
                  <tr key={`${policy.name}:${rule.path}:${index}`}>
                    <td>
                      {index === 0 ? (
                        <>
                          {policy.name}
                          {policy.generated ? (
                            <div className="muted" style={{ fontSize: 12 }}>
                              generated
                            </div>
                          ) : null}
                        </>
                      ) : null}
                    </td>
                    <td className="mono" style={{ wordBreak: "break-all" }}>
                      {rule.path}
                    </td>
                    <td>
                      <div className="row" style={{ gap: 4 }}>
                        {rule.capabilities.map((c) => (
                          <Badge key={c} kind={c === "deny" ? "bad" : undefined}>
                            {c}
                          </Badge>
                        ))}
                      </div>
                    </td>
                  </tr>
                )),
              )}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

function Bindings({ rules }: { rules: AccessRules | undefined }) {
  const bindings = rules?.bindings ?? [];
  return (
    <div className="stack">
      <strong>Bindings</strong>
      <p className="muted" style={{ margin: 0, fontSize: 12 }}>
        What authentication established, and which policies that brings into play. A binding
        with no conditions applies to everyone, anonymous callers included.
      </p>
      {bindings.length === 0 ? (
        <Empty>No bindings.</Empty>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Binding</th>
                <th>Match</th>
                <th>Policies</th>
              </tr>
            </thead>
            <tbody>
              {bindings.map((binding, index) => (
                <tr key={index}>
                  <td>
                    {binding.name ? (
                      binding.name
                    ) : (
                      <span className="muted" style={{ fontSize: 12 }}>
                        generated
                      </span>
                    )}
                  </td>
                  <td className="mono" style={{ fontSize: 12 }}>
                    {binding.match
                      ? Object.entries(binding.match)
                          .map(([key, value]) => `${key}=${value}`)
                          .join("  ")
                      : "everyone"}
                  </td>
                  <td>{binding.policies.join(", ")}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
