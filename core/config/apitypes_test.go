package config

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// The admin API hands these types to clients as JSON and splices them back
// into the YAML document. Both directions have to name the same fields, or a
// setting a client sends is silently dropped and a setting the document
// holds is silently rewritten.

func TestFeedSurvivesJSONAndYAMLRoundTrip(t *testing.T) {
	want := FeedConfig{
		Name:            "central",
		Format:          "maven",
		Upstream:        "https://repo1.maven.org/maven2",
		Anonymous:       true,
		Hosted:          true,
		Publishers:      []string{"token:ci-*", "project:group/*"},
		UpstreamRPS:     12.5,
		Redirect:        true,
		RedirectTTL:     Duration(90 * time.Second),
		PublishPolicy:   "forward:eu",
		ReplicationMode: "eager",
		PeerFallback:    true,
		Policies: []PolicyConfig{
			{Name: "allowlist", Options: map[string]any{"allow": []any{"com.example:liba"}}},
		},
	}

	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	// Every field must travel under its documented name, not its Go name.
	var asMap map[string]any
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	for _, key := range []string{
		"name", "format", "upstream", "anonymous", "hosted", "publishers",
		"upstream_rps", "redirect", "redirect_ttl", "publish_policy",
		"replication_mode", "peer_fallback", "policies",
	} {
		if _, ok := asMap[key]; !ok {
			t.Errorf("JSON is missing %q; keys are %v", key, keysOf(asMap))
		}
	}
	if got := asMap["redirect_ttl"]; got != "1m30s" {
		t.Errorf("redirect_ttl = %v, want the human form", got)
	}

	var back FeedConfig
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal JSON: %v", err)
	}
	if !reflect.DeepEqual(back, want) {
		t.Errorf("JSON round trip changed the feed:\n got %+v\nwant %+v", back, want)
	}

	// And the same value has to survive being written into the document and
	// loaded back out of it — this is what an API write actually does.
	doc, err := yaml.Marshal(want)
	if err != nil {
		t.Fatalf("marshal YAML: %v", err)
	}
	var fromYAML FeedConfig
	if err := yaml.Unmarshal(doc, &fromYAML); err != nil {
		t.Fatalf("unmarshal YAML %q: %v", doc, err)
	}
	if !reflect.DeepEqual(fromYAML, want) {
		t.Errorf("YAML round trip changed the feed:\n got %+v\nwant %+v", fromYAML, want)
	}
}

// A duration written back as a nanosecond count would be unreadable by the
// loader, so the document would fail to parse right after a successful write.
func TestDurationWritesTheHumanForm(t *testing.T) {
	out, err := yaml.Marshal(struct {
		TTL Duration `yaml:"redirect_ttl"`
	}{Duration(15 * time.Minute)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(out), "redirect_ttl: 15m0s\n"; got != want {
		t.Fatalf("marshalled %q, want %q", got, want)
	}
}

func TestDurationRefusesABareNumberInJSON(t *testing.T) {
	var d Duration
	if err := json.Unmarshal([]byte("900"), &d); err == nil {
		t.Fatal("a bare number was accepted; seconds and nanoseconds look alike")
	}
}

func TestPeerAndIssuerUseDocumentedJSONNames(t *testing.T) {
	peer, err := json.Marshal(PeerConfig{
		Name: "eu", URL: "https://eu.internal:9443",
		PublicURL: "https://eu.example.com", PullInterval: Duration(2 * time.Second),
		TokenFile: "/etc/registry/peer-eu.token",
	})
	if err != nil {
		t.Fatalf("marshal peer: %v", err)
	}
	assertKeys(t, peer, "name", "url", "public_url", "pull_interval", "token_file")

	issuer, err := json.Marshal(OIDCIssuer{
		Issuer: "https://gitlab.com", Audience: "registry", JWKSURL: "https://gitlab.com/keys",
	})
	if err != nil {
		t.Fatalf("marshal issuer: %v", err)
	}
	assertKeys(t, issuer, "issuer", "audience", "jwks_url")
}

// An optional field left unset must not appear in the document at all;
// otherwise every API write buries the operator's file in empty keys.
func TestUnsetFeedFieldsAreNotWritten(t *testing.T) {
	out, err := yaml.Marshal(FeedConfig{Name: "hosted", Format: "maven", Hosted: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := "name: hosted\nformat: maven\nhosted: true\n"
	if string(out) != want {
		t.Fatalf("wrote:\n%s\nwant:\n%s", out, want)
	}
}

func assertKeys(t *testing.T, raw []byte, keys ...string) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			t.Errorf("missing %q; keys are %v", k, keysOf(m))
		}
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
