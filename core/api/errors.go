package api

import "errors"

// Sentinel errors shared by the core and all modules. Wrap them with
// fmt.Errorf("...: %w", err); the HTTP layer maps them to status codes.
var (
	// ErrNotFound: the requested coordinate/blob does not exist here or upstream.
	ErrNotFound = errors.New("not found")
	// ErrUnauthorized: no valid credentials were presented.
	ErrUnauthorized = errors.New("unauthorized")
	// ErrForbidden: a policy or permission denied the operation.
	ErrForbidden = errors.New("forbidden")
	// ErrChecksumMismatch: fetched or uploaded content does not match the
	// expected checksum; the artifact must not be stored or served.
	ErrChecksumMismatch = errors.New("checksum mismatch")
	// ErrImmutable: attempt to overwrite an already published release.
	ErrImmutable = errors.New("published release is immutable")
	// ErrUpstreamUnavailable: the upstream could not be reached (or its
	// circuit breaker is open) and no cached copy exists.
	ErrUpstreamUnavailable = errors.New("upstream unavailable")
	// ErrUnavailable: a required backend (e.g. the database) is down and no
	// cached state allows the operation to proceed.
	ErrUnavailable = errors.New("temporarily unavailable")
)
