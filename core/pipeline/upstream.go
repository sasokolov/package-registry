package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/sasokolov/package-registry/core/api"
)

const (
	defaultRetries = 3
	backoffBase    = 100 * time.Millisecond
	// metadataSizeCap bounds metadata bodies buffered in memory
	// (npm package roots can reach tens of MB, but not this).
	metadataSizeCap = 128 << 20
)

// UpstreamOptions configures a per-feed upstream client.
type UpstreamOptions struct {
	// Feed is the label used in logs and metrics.
	Feed string
	// BaseURL is the upstream root; the intent's RemotePath is joined to it.
	BaseURL string
	// RPS rate-limits outgoing requests; 0 means unlimited.
	RPS float64
	// Client must have no global Timeout (it would cut long artifact
	// downloads); use Transport.ResponseHeaderTimeout instead.
	Client *http.Client
	// Retries is the max attempt count (default 3).
	Retries int
	Logger  *slog.Logger
	Metrics *Metrics
	Now     func() time.Time
}

// Upstream is a per-feed upstream HTTP client with retry+jitter, a circuit
// breaker and an optional rate limiter.
type Upstream struct {
	feed    string
	base    *url.URL
	client  *http.Client
	limiter *rate.Limiter
	breaker *Breaker
	retries int
	logger  *slog.Logger
	metrics *Metrics
}

// NewUpstream builds the client.
func NewUpstream(o UpstreamOptions) (*Upstream, error) {
	base, err := url.Parse(o.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("upstream base URL %q: %w", o.BaseURL, err)
	}
	u := &Upstream{
		feed:    o.Feed,
		base:    base,
		client:  o.Client,
		breaker: NewBreaker(0, 0, o.Now),
		retries: o.Retries,
		logger:  o.Logger,
		metrics: o.Metrics,
	}
	if u.client == nil {
		u.client = &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 15 * time.Second}}
	}
	if u.retries <= 0 {
		u.retries = defaultRetries
	}
	if u.logger == nil {
		u.logger = slog.Default()
	}
	if o.RPS > 0 {
		u.limiter = rate.NewLimiter(rate.Limit(o.RPS), 1)
	}
	return u, nil
}

// BreakerState exposes the circuit state for metrics scraping.
func (u *Upstream) BreakerState() int { return u.breaker.State() }

func (u *Upstream) buildURL(remotePath string) string {
	joined := *u.base
	joined.Path = strings.TrimSuffix(u.base.Path, "/") + "/" + strings.TrimPrefix(remotePath, "/")
	return joined.String()
}

// Fetch GETs remotePath from the upstream with retries (5xx and transport
// errors only), jittered backoff, rate limiting and the circuit breaker.
// The caller owns the returned body.
func (u *Upstream) Fetch(ctx context.Context, remotePath string) (*http.Response, error) {
	return u.fetchTarget(ctx, u.buildURL(remotePath))
}

// FetchURL is Fetch for an absolute URL — indirect artifact locations may
// live on another host (e.g. Terraform's X-Terraform-Get). The feed's
// retry/breaker/rate-limit discipline still applies.
func (u *Upstream) FetchURL(ctx context.Context, absURL string) (*http.Response, error) {
	parsed, err := url.Parse(absURL)
	if err != nil || !parsed.IsAbs() {
		return nil, fmt.Errorf("upstream %s: invalid absolute URL %q: %w", u.feed, absURL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("upstream %s: unsupported scheme in %q", u.feed, absURL)
	}
	return u.fetchTarget(ctx, absURL)
}

// ResolveReference resolves loc (absolute, or relative to the upstream
// document at remotePath) into an absolute URL.
func (u *Upstream) ResolveReference(remotePath, loc string) (string, error) {
	ref, err := url.Parse(loc)
	if err != nil {
		return "", fmt.Errorf("upstream %s: invalid location %q: %w", u.feed, loc, err)
	}
	base, err := url.Parse(u.buildURL(remotePath))
	if err != nil {
		return "", fmt.Errorf("upstream %s: resolve base: %w", u.feed, err)
	}
	return base.ResolveReference(ref).String(), nil
}

func (u *Upstream) fetchTarget(ctx context.Context, target string) (*http.Response, error) {
	if !u.breaker.Allow() {
		u.count("breaker_open")
		return nil, fmt.Errorf("upstream %s: circuit breaker open: %w", u.feed, api.ErrUpstreamUnavailable)
	}

	var lastErr error
	for attempt := 0; attempt < u.retries; attempt++ {
		if attempt > 0 {
			// Exponential backoff with full jitter.
			backoff := backoffBase << (attempt - 1)
			sleep := time.Duration(rand.Int64N(int64(backoff))) + backoff/2 //nolint:gosec // jitter, not crypto
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(sleep):
			}
		}
		if u.limiter != nil {
			if err := u.limiter.Wait(ctx); err != nil {
				return nil, fmt.Errorf("upstream %s: rate limiter: %w", u.feed, err)
			}
		}

		resp, err := u.attempt(ctx, target)
		switch {
		case err == nil:
			u.breaker.Success()
			u.count("ok")
			return resp, nil
		case errors.Is(err, api.ErrNotFound):
			// A clean 404 is a valid upstream answer, not a failure.
			u.breaker.Success()
			u.count("not_found")
			return nil, err
		case !retryable(err):
			u.breaker.Failure()
			u.count("error")
			return nil, err
		default:
			u.breaker.Failure()
			u.count("error")
			lastErr = err
			u.logger.Warn("upstream attempt failed",
				"feed", u.feed, "url", target, "attempt", attempt+1, "error", err)
			if !u.breaker.Allow() {
				return nil, fmt.Errorf("upstream %s: circuit breaker opened after failures: %w", u.feed, api.ErrUpstreamUnavailable)
			}
		}
	}
	return nil, fmt.Errorf("upstream %s: %v: %w", u.feed, lastErr, api.ErrUpstreamUnavailable)
}

// FetchAll fetches a complete (metadata) body into memory.
func (u *Upstream) FetchAll(ctx context.Context, remotePath string) ([]byte, error) {
	resp, err := u.Fetch(ctx, remotePath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := readAllCapped(resp.Body, metadataSizeCap)
	if err != nil {
		return nil, fmt.Errorf("upstream %s: read body: %w", u.feed, err)
	}
	return body, nil
}

type transientError struct{ error }

func (t transientError) Unwrap() error { return t.error }

func retryable(err error) bool {
	var te transientError
	return errors.As(err, &te)
}

// readAllCapped reads at most cap bytes and errors if the body is larger.
func readAllCapped(r io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("body exceeds %d bytes cap", limit)
	}
	return body, nil
}

func (u *Upstream) attempt(ctx context.Context, target string) (*http.Response, error) {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := u.client.Do(req)
	if u.metrics != nil {
		u.metrics.UpstreamDuration.WithLabelValues(u.feed).Observe(time.Since(start).Seconds())
	}
	if err != nil {
		return nil, transientError{fmt.Errorf("request %s: %w", target, err)}
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return resp, nil
	case resp.StatusCode == http.StatusNotFound:
		_ = resp.Body.Close()
		return nil, api.NotFoundf("upstream %s returned 404 for %s", u.feed, target)
	case resp.StatusCode >= 500:
		_ = resp.Body.Close()
		return nil, transientError{fmt.Errorf("upstream status %d for %s", resp.StatusCode, target)}
	default:
		_ = resp.Body.Close()
		return nil, fmt.Errorf("upstream status %d for %s", resp.StatusCode, target)
	}
}

func (u *Upstream) count(outcome string) {
	if u.metrics != nil {
		u.metrics.UpstreamRequests.WithLabelValues(u.feed, outcome).Inc()
	}
}
