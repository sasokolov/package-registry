package npm

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sasokolov/package-registry/core/api"
)

// MetadataIntent points at the package root, which carries per-version
// license, publication time and the tarball digest.
func (Module) MetadataIntent(_ api.Feed, coord api.PackageCoordinate) (api.Intent, bool) {
	if coord.Name == "" || coord.Version == "" {
		return api.Intent{}, false
	}
	return api.Intent{
		Kind:        api.IntentMetadata,
		Coord:       api.PackageCoordinate{Format: "npm", Name: coord.Name},
		CacheTTL:    metadataTTL,
		RemotePath:  packageRootPath(coord.Name),
		ContentType: "application/json",
	}, true
}

// packageRootPath is the upstream path of a package document. The decoded
// form is used deliberately: it survives URL building unchanged and npm
// registries accept it as readily as the %2f-escaped variant.
func packageRootPath(name string) string { return name }

// packageRoot is the subset of the npm package document we interpret.
type packageRoot struct {
	Versions map[string]struct {
		License any `json:"license"`
		Dist    struct {
			Integrity string `json:"integrity"`
			Shasum    string `json:"shasum"`
		} `json:"dist"`
	} `json:"versions"`
	Time map[string]string `json:"time"`
}

// ExtractMetadata pulls canonical keys for one version out of the package
// root: license, publication time and the digest the registry must verify
// the tarball against (invariant 5).
func (Module) ExtractMetadata(coord api.PackageCoordinate, body []byte) (map[string]string, error) {
	var doc packageRoot
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("parse npm package document: %w", err)
	}
	meta := map[string]string{api.MetaEcosystem: "npm"}
	if coord.Version == "" {
		return meta, nil
	}
	v, ok := doc.Versions[coord.Version]
	if !ok {
		return meta, nil
	}
	if lic := licenseString(v.License); lic != "" {
		meta[api.MetaLicense] = lic
	}
	if t := doc.Time[coord.Version]; t != "" {
		meta[api.MetaPublishedAt] = t
	}
	if sum := checksumFromDist(v.Dist.Integrity, v.Dist.Shasum); sum != "" {
		meta[api.MetaChecksum] = sum
	}
	return meta, nil
}

// licenseString copes with the many shapes npm's license field takes:
// a string, {"type": "..."} , or a list of either (very old packages).
func licenseString(raw any) string {
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case map[string]any:
		if s, ok := v["type"].(string); ok {
			return strings.TrimSpace(s)
		}
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			if s := licenseString(item); s != "" {
				parts = append(parts, s)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, " OR ")
		}
	}
	return ""
}

// checksumFromDist prefers Subresource-Integrity ("sha512-<base64>") and
// falls back to the legacy hex shasum (sha1).
func checksumFromDist(integrity, shasum string) string {
	if integrity != "" {
		algo, b64, ok := strings.Cut(integrity, "-")
		if ok {
			// npm may list several integrity values separated by spaces;
			// the first one is enough to verify the bytes.
			b64, _, _ = strings.Cut(b64, " ")
			if raw, err := base64.StdEncoding.DecodeString(b64); err == nil {
				return strings.ToLower(algo) + ":" + hex.EncodeToString(raw)
			}
		}
	}
	if shasum != "" {
		return "sha1:" + strings.ToLower(shasum)
	}
	return ""
}
