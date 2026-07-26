package api

import (
	"fmt"
	"sort"
	"sync"
)

// Compile-time module registries (Caddy style, per CLAUDE.md): module
// packages self-register from init(), the final binary chooses its module
// set through imports in cmd/registry. The maps are effectively read-only
// after init; the mutex only guards misuse and parallel tests.

// PolicyFactory builds a configured policy instance from its YAML options
// (already decoded into a generic map).
type PolicyFactory func(options map[string]any) (Policy, error)

// StorageFactory builds a blob store from its YAML options.
type StorageFactory func(options map[string]any) (BlobStore, error)

var (
	regMu    sync.RWMutex
	formats  = map[string]FormatModule{}
	policies = map[string]PolicyFactory{}
	storages = map[string]StorageFactory{}
)

// RegisterFormat registers a format module. It panics on a duplicate or
// empty name: both are programmer errors caught at process start.
func RegisterFormat(m FormatModule) {
	regMu.Lock()
	defer regMu.Unlock()
	name := m.Name()
	if name == "" {
		panic("api: RegisterFormat with empty name")
	}
	if _, dup := formats[name]; dup {
		panic(fmt.Sprintf("api: format module %q registered twice", name))
	}
	formats[name] = m
}

// Format looks up a registered format module by name.
func Format(name string) (FormatModule, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	m, ok := formats[name]
	return m, ok
}

// Formats returns the sorted names of all registered format modules.
func Formats() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	names := make([]string, 0, len(formats))
	for n := range formats {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// RegisterPolicy registers a policy factory under a name used in feed config.
func RegisterPolicy(name string, f PolicyFactory) {
	regMu.Lock()
	defer regMu.Unlock()
	if name == "" {
		panic("api: RegisterPolicy with empty name")
	}
	if _, dup := policies[name]; dup {
		panic(fmt.Sprintf("api: policy %q registered twice", name))
	}
	policies[name] = f
}

// NewPolicy instantiates a registered policy with its options.
func NewPolicy(name string, options map[string]any) (Policy, error) {
	regMu.RLock()
	f, ok := policies[name]
	regMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("policy %q is not registered (have %v)", name, policyNames())
	}
	p, err := f(options)
	if err != nil {
		return nil, fmt.Errorf("policy %q: %w", name, err)
	}
	return p, nil
}

// PolicyRegistered reports whether a policy name is known.
func PolicyRegistered(name string) bool {
	regMu.RLock()
	defer regMu.RUnlock()
	_, ok := policies[name]
	return ok
}

func policyNames() []string {
	names := make([]string, 0, len(policies))
	for n := range policies {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// RegisterStorage registers a blob-store factory under a storage type name.
func RegisterStorage(name string, f StorageFactory) {
	regMu.Lock()
	defer regMu.Unlock()
	if name == "" {
		panic("api: RegisterStorage with empty name")
	}
	if _, dup := storages[name]; dup {
		panic(fmt.Sprintf("api: storage %q registered twice", name))
	}
	storages[name] = f
}

// NewStorage instantiates a registered storage backend with its options.
func NewStorage(name string, options map[string]any) (BlobStore, error) {
	regMu.RLock()
	f, ok := storages[name]
	regMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("storage %q is not registered", name)
	}
	s, err := f(options)
	if err != nil {
		return nil, fmt.Errorf("storage %q: %w", name, err)
	}
	return s, nil
}
