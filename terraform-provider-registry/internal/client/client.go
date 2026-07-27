// Package client talks to the registry's admin API.
//
// It is deliberately thin. The registry already owns every rule about what a
// valid configuration is, applies each change to the whole document under a
// cross-replica lock, and answers with the resulting version — so the
// provider's job is to say what it wants and report what came back, not to
// model the document itself. A client that assembled configuration locally
// would be a second implementation of the schema, and the two would drift.
package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// APIPath is where the registry's own API lives.
const APIPath = "/api/v1"

// ErrNotFound is returned when the registry has no such resource. Every Read
// turns it into "this disappeared", which is how Terraform learns that
// something was removed behind its back.
var ErrNotFound = errors.New("not found")

// Error is a failure the registry reported, with the status it used. The
// status matters: 409 means someone else wrote first, 422 means the change
// would not load, and those deserve different words in a plan.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("registry returned %d", e.Status)
	}
	return fmt.Sprintf("registry returned %d: %s", e.Status, e.Message)
}

// Conflict reports whether the change lost a race with another writer.
func (e *Error) Conflict() bool { return e.Status == http.StatusConflict }

// Invalid reports whether the registry refused the change because the
// resulting configuration would not load.
func (e *Error) Invalid() bool { return e.Status == 422 }

// Client is an authenticated connection to one registry site.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// Options configure a Client.
type Options struct {
	// Endpoint is the registry's base URL, e.g. https://registry.example.com.
	Endpoint string
	// Token is a static registry token or an OIDC id_token. It must map to
	// an identity listed in the site's admins patterns.
	Token string
	// Insecure disables TLS verification. Development only.
	Insecure bool
	// Timeout bounds a single request. Default 30s.
	Timeout time.Duration
}

// New builds a client.
func New(opts Options) (*Client, error) {
	if opts.Endpoint == "" {
		return nil, errors.New("endpoint is required")
	}
	u, err := url.Parse(opts.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("endpoint %q is not a URL: %w", opts.Endpoint, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("endpoint %q must be http or https", opts.Endpoint)
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	transport := http.DefaultTransport
	if opts.Insecure {
		clone := http.DefaultTransport.(*http.Transport).Clone()
		clone.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in, documented as development only
		transport = clone
	}
	return &Client{
		base:  strings.TrimSuffix(opts.Endpoint, "/") + APIPath,
		token: opts.Token,
		http:  &http.Client{Timeout: timeout, Transport: transport},
	}, nil
}

// Result is what a write returned: the configuration version it produced and
// whether the resource had to be created.
type Result struct {
	Version string `json:"version"`
	Created bool   `json:"created"`
}

// Get decodes a GET into out.
func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

// Put sends a JSON body and decodes the result.
func (c *Client) Put(ctx context.Context, path string, body any) (Result, error) {
	var res Result
	err := c.do(ctx, http.MethodPut, path, body, &res)
	return res, err
}

// Post sends a JSON body and decodes into out.
func (c *Client) Post(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPost, path, body, out)
}

// Delete removes a resource.
func (c *Client) Delete(ctx context.Context, path string) error {
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

// do performs one request.
//
// There is no If-Match here on purpose. Every per-resource endpoint is
// already a read-modify-write of the whole document inside a cross-replica
// lock, so two Terraform resources changing different feeds in parallel
// cannot lose each other's write. Sending a version would instead make them
// collide on a document they both only partly own, and turn ordinary
// parallelism into spurious conflicts.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode >= 300 {
		return &Error{Status: resp.StatusCode, Message: messageOf(raw)}
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// messageOf pulls the registry's error text out of a response body.
func messageOf(raw []byte) string {
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &payload); err == nil && payload.Error != "" {
		return payload.Error
	}
	return strings.TrimSpace(string(raw))
}

// Query builds a path with one query parameter, used for the resources whose
// identity is a URL or an identity pattern and therefore cannot be a path
// segment.
func Query(path, key, value string) string {
	return path + "?" + url.Values{key: []string{value}}.Encode()
}
