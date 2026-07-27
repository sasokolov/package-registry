package adminapi

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"

	"github.com/sasokolov/package-registry/core/config"
)

// Per-resource endpoints exist because Terraform and a UI form both think in
// resources, not documents. Each one is still a read-modify-write of the
// WHOLE document under the same lock and the same validation — the
// granularity is in the interface, never in the storage.

// handleListFeeds returns the configured feeds.
func (s *Server) handleListFeeds(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	cfg := s.manager.Current()
	writeJSON(w, http.StatusOK, map[string]any{
		"version": s.manager.Version(),
		"feeds":   cfg.Feeds,
	})
}

// handleGetFeed returns one feed's configuration.
func (s *Server) handleGetFeed(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	name := chi.URLParam(r, "feed")
	for _, f := range s.manager.Current().Feeds {
		if f.Name == name {
			w.Header().Set("ETag", `"`+s.manager.Version()+`"`)
			writeJSON(w, http.StatusOK, f)
			return
		}
	}
	s.writeError(w, http.StatusNotFound, "no feed named "+name)
}

// handlePutFeed creates or replaces one feed.
func (s *Server) handlePutFeed(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	name := chi.URLParam(r, "feed")

	var feed config.FeedConfig
	if err := decodeBody(r, &feed); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if feed.Name == "" {
		feed.Name = name
	}
	if feed.Name != name {
		s.writeError(w, http.StatusBadRequest,
			fmt.Sprintf("feed name in the body (%q) does not match the path (%q)", feed.Name, name))
		return
	}

	var created bool
	version, err := s.mutateDocument(r.Context(), trimETag(r.Header.Get("If-Match")),
		func(doc *yaml.Node) error {
			var err error
			created, err = upsertSequenceItem(doc, "feeds", "name", name, feed)
			return err
		})
	if err != nil {
		s.writeConfigError(w, err)
		return
	}

	s.audit.Info("feed configuration written",
		"identity", id.String(), "feed", name, "created", created,
		"version", version, "site", s.site)
	// A feed's generated indexes depend on its configuration, so rebuild
	// them rather than leaving a stale view behind.
	if s.deps.Reindex != nil {
		if err := s.deps.Reindex(r.Context(), name); err != nil {
			s.logger.Warn("reindex after a feed change failed", "feed", name, "error", err)
		}
	}

	w.Header().Set("ETag", `"`+version+`"`)
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"version": version, "created": created})
}

// handleDeleteFeed removes a feed from the configuration. The packages it
// served stay in storage: removing a feed is a configuration change, not a
// deletion of content (invariant 4 — published bytes are never destroyed by
// a config edit).
func (s *Server) handleDeleteFeed(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	name := chi.URLParam(r, "feed")

	var found bool
	version, err := s.mutateDocument(r.Context(), trimETag(r.Header.Get("If-Match")),
		func(doc *yaml.Node) error {
			var err error
			found, err = removeSequenceItem(doc, "feeds", "name", name)
			return err
		})
	if err != nil {
		s.writeConfigError(w, err)
		return
	}
	if !found {
		s.writeError(w, http.StatusNotFound, "no feed named "+name)
		return
	}
	s.audit.Warn("feed removed from the configuration",
		"identity", id.String(), "feed", name, "version", version, "site", s.site,
		"note", "stored packages are untouched")
	w.Header().Set("ETag", `"`+version+`"`)
	writeJSON(w, http.StatusOK, map[string]any{"version": version})
}

// handleGetAdmins returns the administrator patterns.
func (s *Server) handleGetAdmins(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	w.Header().Set("ETag", `"`+s.manager.Version()+`"`)
	writeJSON(w, http.StatusOK, map[string]any{
		"version": s.manager.Version(),
		"admins":  s.manager.Current().Admins,
	})
}

// handlePutAdmins replaces the administrator patterns. Locking yourself out
// is possible and deliberate — the file is still there — but an empty list
// is refused, because that turns the API off with no way back through it.
func (s *Server) handlePutAdmins(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	var body struct {
		Admins []string `json:"admins"`
	}
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(body.Admins) == 0 {
		s.writeError(w, http.StatusBadRequest,
			"refusing to remove every administrator through the API: edit the document directly if that is intended")
		return
	}

	version, err := s.mutateDocument(r.Context(), trimETag(r.Header.Get("If-Match")),
		func(doc *yaml.Node) error {
			return setMappingKey(doc, "admins", body.Admins)
		})
	if err != nil {
		s.writeConfigError(w, err)
		return
	}
	s.audit.Warn("administrators changed",
		"identity", id.String(), "admins", body.Admins, "version", version, "site", s.site)
	w.Header().Set("ETag", `"`+version+`"`)
	writeJSON(w, http.StatusOK, map[string]any{"version": version})
}

// handleGetPeers returns the replication peers.
func (s *Server) handleGetPeers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	w.Header().Set("ETag", `"`+s.manager.Version()+`"`)
	writeJSON(w, http.StatusOK, map[string]any{
		"version": s.manager.Version(),
		"peers":   s.manager.Current().Replication.Peers,
	})
}

// handlePutPeer creates or replaces one replication peer.
func (s *Server) handlePutPeer(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	name := chi.URLParam(r, "peer")

	var peer config.PeerConfig
	if err := decodeBody(r, &peer); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if peer.Name == "" {
		peer.Name = name
	}
	if peer.Name != name {
		s.writeError(w, http.StatusBadRequest, "peer name in the body does not match the path")
		return
	}

	var created bool
	version, err := s.mutateDocument(r.Context(), trimETag(r.Header.Get("If-Match")),
		func(doc *yaml.Node) error {
			replication := ensureMapping(doc, "replication")
			var err error
			created, err = upsertSequenceItemIn(replication, "peers", "name", name, peer)
			return err
		})
	if err != nil {
		s.writeConfigError(w, err)
		return
	}
	s.audit.Info("replication peer written",
		"identity", id.String(), "peer", name, "created", created,
		"version", version, "site", s.site)
	w.Header().Set("ETag", `"`+version+`"`)
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"version": version, "created": created})
}

// handleDeletePeer removes a replication peer.
func (s *Server) handleDeletePeer(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	name := chi.URLParam(r, "peer")

	var found bool
	version, err := s.mutateDocument(r.Context(), trimETag(r.Header.Get("If-Match")),
		func(doc *yaml.Node) error {
			replication := findMapping(doc, "replication")
			if replication == nil {
				return nil
			}
			var err error
			found, err = removeSequenceItemIn(replication, "peers", "name", name)
			return err
		})
	if err != nil {
		s.writeConfigError(w, err)
		return
	}
	if !found {
		s.writeError(w, http.StatusNotFound, "no peer named "+name)
		return
	}
	s.audit.Warn("replication peer removed",
		"identity", id.String(), "peer", name, "version", version, "site", s.site)
	w.Header().Set("ETag", `"`+version+`"`)
	writeJSON(w, http.StatusOK, map[string]any{"version": version})
}

// ---------------------------------------------------------------------------
// YAML document surgery
//
// Edits are applied to the parsed node tree rather than to a re-marshalled
// struct, so comments and the operator's ordering survive an API write. A
// configuration people also edit by hand must stay readable after a robot
// touches it.

// findMapping returns a top-level mapping value node by key.
func findMapping(doc *yaml.Node, key string) *yaml.Node {
	root := documentRoot(doc)
	if root == nil {
		return nil
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			return root.Content[i+1]
		}
	}
	return nil
}

// ensureMapping returns a top-level mapping value node, creating it when
// absent.
func ensureMapping(doc *yaml.Node, key string) *yaml.Node {
	if node := findMapping(doc, key); node != nil {
		return node
	}
	root := documentRoot(doc)
	if root == nil {
		return nil
	}
	value := &yaml.Node{Kind: yaml.MappingNode}
	root.Content = append(root.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key}, value)
	return value
}

// documentRoot returns the mapping at the root of a document node.
func documentRoot(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 {
			return nil
		}
		doc = doc.Content[0]
	}
	if doc.Kind != yaml.MappingNode {
		return nil
	}
	return doc
}

// setMappingKey replaces a top-level key's value.
func setMappingKey(doc *yaml.Node, key string, value any) error {
	root := documentRoot(doc)
	if root == nil {
		return errors.New("configuration document is not a mapping")
	}
	node, err := yamlDocument(value)
	if err != nil {
		return err
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			root.Content[i+1] = node
			return nil
		}
	}
	root.Content = append(root.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key}, node)
	return nil
}

// upsertSequenceItem replaces (or appends) an item in a top-level sequence,
// matched by a field.
func upsertSequenceItem(doc *yaml.Node, key, matchField, matchValue string, value any) (bool, error) {
	root := documentRoot(doc)
	if root == nil {
		return false, errors.New("configuration document is not a mapping")
	}
	return upsertSequenceItemIn(root, key, matchField, matchValue, value)
}

// upsertSequenceItemIn is upsertSequenceItem within a given mapping.
func upsertSequenceItemIn(parent *yaml.Node, key, matchField, matchValue string, value any) (bool, error) {
	if parent == nil {
		return false, errors.New("parent section is missing")
	}
	node, err := yamlDocument(value)
	if err != nil {
		return false, err
	}

	seq := childValue(parent, key)
	if seq == nil {
		seq = &yaml.Node{Kind: yaml.SequenceNode}
		parent.Content = append(parent.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: key}, seq)
	}
	if seq.Kind != yaml.SequenceNode {
		return false, fmt.Errorf("%s is not a list", key)
	}
	for i, item := range seq.Content {
		if mappingField(item, matchField) == matchValue {
			seq.Content[i] = node
			return false, nil
		}
	}
	seq.Content = append(seq.Content, node)
	return true, nil
}

// removeSequenceItem drops an item from a top-level sequence.
func removeSequenceItem(doc *yaml.Node, key, matchField, matchValue string) (bool, error) {
	root := documentRoot(doc)
	if root == nil {
		return false, errors.New("configuration document is not a mapping")
	}
	return removeSequenceItemIn(root, key, matchField, matchValue)
}

// removeSequenceItemIn is removeSequenceItem within a given mapping.
func removeSequenceItemIn(parent *yaml.Node, key, matchField, matchValue string) (bool, error) {
	seq := childValue(parent, key)
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return false, nil
	}
	for i, item := range seq.Content {
		if mappingField(item, matchField) == matchValue {
			seq.Content = append(seq.Content[:i], seq.Content[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

// childValue returns a mapping's value node for a key.
func childValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

// mappingField reads a scalar field from a mapping node.
func mappingField(node *yaml.Node, field string) string {
	value := childValue(node, field)
	if value == nil {
		return ""
	}
	return value.Value
}
