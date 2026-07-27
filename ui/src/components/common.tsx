import type { ReactNode } from "react";
import { ApiError } from "../api/client";

export function Card({ label, value, hint }: { label: string; value: ReactNode; hint?: ReactNode }) {
  return (
    <div className="card">
      <div className="label">{label}</div>
      <div className="value">{value}</div>
      {hint ? <div className="muted" style={{ fontSize: 12 }}>{hint}</div> : null}
    </div>
  );
}

export function Badge({ kind, children }: { kind?: "ok" | "warn" | "bad"; children: ReactNode }) {
  return <span className={kind ? `badge ${kind}` : "badge"}>{children}</span>;
}

/**
 * One error presentation for the whole console. An operator needs to know
 * which of three things happened — not signed in, not allowed, or the
 * registry said no — because the next action differs for each.
 */
export function ErrorNotice({ error }: { error: ApiError | undefined }) {
  if (!error) return null;
  if (error.unauthenticated) {
    return <div className="notice bad">Not signed in. Add a token to continue.</div>;
  }
  if (error.forbidden) {
    return (
      <div className="notice bad">
        This identity is not allowed to see or do that. Administrator actions need an
        identity matching the <code>admins</code> patterns in the configuration.
      </div>
    );
  }
  return <div className="notice bad">{error.message}</div>;
}

export function Loading({ what }: { what: string }) {
  return <p className="muted">Loading {what}…</p>;
}

export function Empty({ children }: { children: ReactNode }) {
  return <p className="muted">{children}</p>;
}

/** Renders a byte count the way an operator reads one. */
export function bytes(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ["KiB", "MiB", "GiB", "TiB"];
  let value = n / 1024;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return `${value.toFixed(value < 10 ? 1 : 0)} ${units[unit]}`;
}

/** Renders a timestamp as an age, which is what status screens are read for. */
export function age(iso: string | undefined | null): string {
  if (!iso) return "never";
  const then = new Date(iso).getTime();
  if (!Number.isFinite(then) || then <= 0) return "never";
  const seconds = Math.max(0, Math.round((Date.now() - then) / 1000));
  if (seconds < 60) return `${seconds}s ago`;
  if (seconds < 3600) return `${Math.round(seconds / 60)}m ago`;
  if (seconds < 86400) return `${Math.round(seconds / 3600)}h ago`;
  return `${Math.round(seconds / 86400)}d ago`;
}

export function short(sha: string | undefined): string {
  return sha ? sha.slice(0, 12) : "";
}
