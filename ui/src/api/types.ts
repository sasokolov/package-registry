// The shapes the registry API returns. Kept in one file so a change to the
// Go side has exactly one place to land on the console side.

// Fields the registry withholds from an anonymous caller are optional here,
// because "you may not know this" is a real answer and the console has to
// render it without pretending it got a zero.
export interface SiteStatus {
  site: string;
  config_version?: string;
  config_source?: string;
  feeds: number;
  database?: "up" | "unavailable" | "disabled";
  replication: { enabled: boolean; peers: number; topology?: string };
}

export interface WhoAmI {
  kind: string;
  subject: string;
  project_path?: string;
  admin: boolean;
  stale?: boolean;
  can_publish?: string[];
  /** Set when a credential was offered and the registry refused it. */
  auth_error?: string;
}

export interface FeedSummary {
  name: string;
  format: string;
  upstream?: string;
  hosted: boolean;
  anonymous: boolean;
  publishers?: string[];
  policies?: string[];
  publish_policy?: string;
  replication_mode?: string;
  peer_fallback?: boolean;
  packages?: number;
  /** A read-only view over other feeds. */
  group?: boolean;
  /** Its members, in order. Configuration, so identified callers only. */
  members?: string[];
}

export interface PackageEntry {
  feed: string;
  path: string;
  coordinate: string;
  sha256: string;
  size: number;
  checksums?: Record<string, string>;
  metadata?: Record<string, string>;
  site?: string;
  published_by?: string;
  published_at: string;
  quarantined?: boolean;
}

export interface PackageList {
  feed: string;
  total: number;
  offset: number;
  limit: number;
  packages: PackageEntry[] | null;
}

export interface CursorInfo {
  peer: string;
  origin: string;
  applied_seq: number;
  durable_seq: number;
  last_ok_at: string;
  last_error?: string;
}

export interface PeerIdentity {
  peer: string;
  uuid: string;
  last_seen: string;
}

export interface ReplicationStatus {
  enabled: boolean;
  site: string;
  cursors: CursorInfo[];
  peers: PeerIdentity[];
  parked: number;
  heads: Record<string, number>;
}

export interface ConflictEntry {
  feed: string;
  path: string;
  coordinate: string;
  canonical_sha256: string;
  other_sha256: string;
  canonical_site: string;
  other_site: string;
  detected_at: string;
  resolved: boolean;
  resolved_sha256?: string;
}

export interface QuarantineEntry {
  feed: string;
  coordinate: string;
  reason: string;
  detail?: string;
  created_at: string;
}

export interface TokenInfo {
  name: string;
  hash_prefix: string;
  created_at: string;
  revoked_at?: string | null;
}

export interface ConfigDocument {
  version: string;
  source: string;
  writable: boolean;
  document: string;
}

/** One way of signing in, as the site says the form should present it. */
export interface AuthMethod {
  type: "token" | "oidc";
  label?: string;
  issuer?: string;
  help?: string;
}

export interface AccessRule {
  path: string;
  capabilities: string[];
}

export interface AccessPolicy {
  name: string;
  /** Compiled from a feed's anonymous/publishers or the site's admins. */
  generated?: boolean;
  rules: AccessRule[];
}

export interface AccessBinding {
  name?: string;
  generated?: boolean;
  policies: string[];
  match?: Record<string, string>;
}

export interface AccessRules {
  policies: AccessPolicy[];
  bindings: AccessBinding[];
  capabilities: string[];
}

export interface Explanation {
  path: string;
  capability: string;
  identity: string;
  allowed: boolean;
  reason: string;
  policy?: string;
  rule?: string;
  policies?: string[];
  effective_capabilities?: string[];
  bindings?: string[];
}
