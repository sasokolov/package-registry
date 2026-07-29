package client

// The wire shapes of the admin API. They mirror the registry's own JSON
// names exactly; anything the registry adds later is simply ignored here
// until a resource needs it.

// Feed is one configured feed.
type Feed struct {
	Name            string   `json:"name"`
	Format          string   `json:"format"`
	Upstream        string   `json:"upstream,omitempty"`
	Anonymous       bool     `json:"anonymous,omitempty"`
	Hosted          bool     `json:"hosted,omitempty"`
	Publishers      []string `json:"publishers,omitempty"`
	UpstreamRPS     float64  `json:"upstream_rps,omitempty"`
	Redirect        bool     `json:"redirect,omitempty"`
	RedirectTTL     string   `json:"redirect_ttl,omitempty"`
	PublishPolicy   string   `json:"publish_policy,omitempty"`
	ReplicationMode string   `json:"replication_mode,omitempty"`
	PeerFallback    bool     `json:"peer_fallback,omitempty"`
	Members         []string `json:"members,omitempty"`
	Policies        []Policy `json:"policies,omitempty"`
}

// Policy is one entry in a feed's policy chain. Options travel as raw JSON
// so a policy the provider has never heard of still round-trips.
type Policy struct {
	Name    string         `json:"name"`
	Options map[string]any `json:"config,omitempty"`
}

// FeedList is the response of GET /config/feeds.
type FeedList struct {
	Version string `json:"version"`
	Feeds   []Feed `json:"feeds"`
}

// Peer is one replication partner.
type Peer struct {
	Name         string `json:"name"`
	URL          string `json:"url"`
	PublicURL    string `json:"public_url,omitempty"`
	PullInterval string `json:"pull_interval,omitempty"`
	TokenFile    string `json:"token_file,omitempty"`
}

// PeerList is the response of GET /config/peers.
type PeerList struct {
	Version string `json:"version"`
	Peers   []Peer `json:"peers"`
}

// OIDCIssuer is one trusted issuer.
type OIDCIssuer struct {
	Issuer   string `json:"issuer"`
	Audience string `json:"audience"`
	JWKSURL  string `json:"jwks_url,omitempty"`
	// Browser sign-in. A client_id is what turns the console's paste-a-token
	// field into a button; the rest is only needed when discovery is absent
	// or the issuer refuses to treat the registry as a public client.
	ClientID              string   `json:"client_id,omitempty"`
	ClientSecretEnv       string   `json:"client_secret_env,omitempty"`
	Scopes                []string `json:"scopes,omitempty"`
	AuthorizationEndpoint string   `json:"authorization_endpoint,omitempty"`
	TokenEndpoint         string   `json:"token_endpoint,omitempty"`
}

// OIDCList is the response of GET /config/oidc.
type OIDCList struct {
	Version string       `json:"version"`
	Issuers []OIDCIssuer `json:"oidc_issuers"`
}

// AdminList is the response of GET /config/admins.
type AdminList struct {
	Version string   `json:"version"`
	Admins  []string `json:"admins"`
}

// Site is the response of GET /status.
type Site struct {
	Site          string `json:"site"`
	ConfigVersion string `json:"config_version"`
	ConfigSource  string `json:"config_source"`
	Feeds         int    `json:"feeds"`
	Database      string `json:"database"`
	Replication   struct {
		Enabled  bool   `json:"enabled"`
		Peers    int    `json:"peers"`
		Topology string `json:"topology"`
	} `json:"replication"`
}

// WhoAmI is the response of GET /whoami.
type WhoAmI struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
	Admin   bool   `json:"admin"`
}

// Token is one static token, as the registry describes it: by name and hash
// prefix. The secret exists only in the response that issues it.
type Token struct {
	Name       string  `json:"name"`
	HashPrefix string  `json:"hash_prefix"`
	CreatedAt  string  `json:"created_at"`
	RevokedAt  *string `json:"revoked_at,omitempty"`
}

// TokenList is the response of GET /tokens.
type TokenList struct {
	Tokens []Token `json:"tokens"`
}

// IssuedToken is the one-time response of POST /tokens.
type IssuedToken struct {
	Name   string `json:"name"`
	Secret string `json:"secret"`
}

// QuarantineEntry is one active block.
type QuarantineEntry struct {
	Feed       string `json:"feed"`
	Coordinate string `json:"coordinate"`
	Reason     string `json:"reason"`
	Detail     string `json:"detail,omitempty"`
	CreatedAt  string `json:"created_at"`
}

// QuarantineList is the response of GET /quarantine.
type QuarantineList struct {
	Quarantine []QuarantineEntry `json:"quarantine"`
}

// QuarantineRequest blocks or releases a coordinate.
type QuarantineRequest struct {
	Feed       string `json:"feed"`
	Coordinate string `json:"coordinate"`
	Reason     string `json:"reason,omitempty"`
	Detail     string `json:"detail,omitempty"`
	Active     *bool  `json:"active,omitempty"`
}

// ReplicationStatus is the response of GET /replication.
type ReplicationStatus struct {
	Enabled bool             `json:"enabled"`
	Site    string           `json:"site"`
	Cursors []Cursor         `json:"cursors"`
	Peers   []PeerIdentity   `json:"peers"`
	Parked  int              `json:"parked"`
	Heads   map[string]int64 `json:"heads"`
}

// Cursor is how far this site has consumed one peer's journal.
type Cursor struct {
	Peer       string `json:"peer"`
	Origin     string `json:"origin"`
	AppliedSeq int64  `json:"applied_seq"`
	DurableSeq int64  `json:"durable_seq"`
	LastOKAt   string `json:"last_ok_at"`
	LastError  string `json:"last_error,omitempty"`
}

// PeerIdentity is a peer whose identity this site has pinned.
type PeerIdentity struct {
	Peer     string `json:"peer"`
	UUID     string `json:"uuid"`
	LastSeen string `json:"last_seen"`
}

// AccessPolicy is a named set of access rules.
type AccessPolicy struct {
	Name  string       `json:"name"`
	Rules []AccessRule `json:"rules"`
}

// AccessRule grants capabilities on a path.
type AccessRule struct {
	Path         string   `json:"path"`
	Capabilities []string `json:"capabilities"`
}

// AccessPolicyList is the response of GET /config/access/policies.
type AccessPolicyList struct {
	Version  string         `json:"version"`
	Policies []AccessPolicy `json:"policies"`
}

// Binding attaches policies to the identities a match selects.
type Binding struct {
	Name     string       `json:"name"`
	Policies []string     `json:"policies"`
	Match    BindingMatch `json:"match"`
}

// BindingMatch selects identities by what authentication established.
type BindingMatch struct {
	Kind          string `json:"kind,omitempty"`
	Issuer        string `json:"issuer,omitempty"`
	Subject       string `json:"subject,omitempty"`
	ProjectPath   string `json:"project_path,omitempty"`
	Ref           string `json:"ref,omitempty"`
	Authenticated bool   `json:"authenticated,omitempty"`
}

// BindingList is the response of GET /config/access/bindings.
type BindingList struct {
	Version  string    `json:"version"`
	Bindings []Binding `json:"bindings"`
}

// Explanation is the response of GET /access/explain.
type Explanation struct {
	Path         string   `json:"path"`
	Capability   string   `json:"capability"`
	Identity     string   `json:"identity"`
	Allowed      bool     `json:"allowed"`
	Reason       string   `json:"reason"`
	Policy       string   `json:"policy,omitempty"`
	Rule         string   `json:"rule,omitempty"`
	Policies     []string `json:"policies,omitempty"`
	Bindings     []string `json:"bindings,omitempty"`
	Capabilities []string `json:"effective_capabilities,omitempty"`
}
