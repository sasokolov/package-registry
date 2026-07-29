package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
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
	// Re-check every redirect hop: the first hop's destination check must
	// not be bypassable with a 302 to an internal address.
	if u.client.CheckRedirect == nil {
		u.client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return u.checkDestination(req.URL)
		}
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

func (u *Upstream) buildURL(remotePath, query string) string {
	joined := *u.base
	joined.Path = strings.TrimSuffix(u.base.Path, "/") + "/" + strings.TrimPrefix(remotePath, "/")
	// A search is a query, and the upstream cannot answer it without one.
	joined.RawQuery = query
	return joined.String()
}

// Fetch GETs remotePath from the upstream with retries (5xx and transport
// errors only), jittered backoff, rate limiting and the circuit breaker.
// The caller owns the returned body.
func (u *Upstream) Fetch(ctx context.Context, remotePath string) (*http.Response, error) {
	return u.FetchQuery(ctx, remotePath, "")
}

// FetchQuery is Fetch with a query string appended.
func (u *Upstream) FetchQuery(ctx context.Context, remotePath, query string) (*http.Response, error) {
	return u.fetchTarget(ctx, u.buildURL(remotePath, query))
}

// FetchURL is Fetch for an absolute URL — indirect artifact locations may
// live on another host (e.g. Terraform's X-Terraform-Get). Such locations
// come from the upstream, i.e. from untrusted input, so the destination is
// restricted: same host as the feed's upstream, or a public address.
// Redirects are re-checked hop by hop. The feed's retry/breaker/rate-limit
// discipline still applies.
func (u *Upstream) FetchURL(ctx context.Context, absURL string) (*http.Response, error) {
	parsed, err := url.Parse(absURL)
	if err != nil || !parsed.IsAbs() {
		return nil, fmt.Errorf("upstream %s: invalid absolute URL %q: %w", u.feed, redactURL(absURL), err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("upstream %s: unsupported scheme in %q", u.feed, redactURL(absURL))
	}
	if err := u.checkDestination(parsed); err != nil {
		return nil, err
	}
	return u.fetchTarget(ctx, absURL)
}

// checkDestination rejects upstream-supplied locations that point at
// non-public addresses (SSRF guard). The feed's own upstream host is always
// allowed: it is operator-configured, hence trusted.
func (u *Upstream) checkDestination(target *url.URL) error {
	if strings.EqualFold(target.Hostname(), u.base.Hostname()) {
		return nil
	}
	host := target.Hostname()
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("upstream %s: cannot resolve indirect host %q: %w", u.feed, host, err)
	}
	for _, ip := range ips {
		if !isPublicIP(ip) {
			return fmt.Errorf("upstream %s: indirect location host %q resolves to non-public address %s: %w",
				u.feed, host, ip, api.ErrUpstreamUnavailable)
		}
	}
	return nil
}

// isPublicIP reports whether ip is routable on the public internet:
// loopback, private, link-local (incl. cloud metadata 169.254.169.254),
// multicast and unspecified addresses are rejected.
func isPublicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	// Carrier-grade NAT (100.64.0.0/10) and IPv6 unique-local (fc00::/7)
	// are not covered by IsPrivate.
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
			return false
		}
		return true
	}
	return ip[0]&0xfe != 0xfc
}

// redactURL strips the query string: presigned upstream locations carry
// credentials there and must never reach the logs (invariant 12).
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparsable url>"
	}
	if u.RawQuery != "" {
		u.RawQuery = "REDACTED"
	}
	u.User = nil
	return u.String()
}

// ResolveReference resolves loc (absolute, or relative to the upstream
// document at remotePath) into an absolute URL.
func (u *Upstream) ResolveReference(remotePath, loc string) (string, error) {
	ref, err := url.Parse(loc)
	if err != nil {
		return "", fmt.Errorf("upstream %s: invalid location %q: %w", u.feed, loc, err)
	}
	base, err := url.Parse(u.buildURL(remotePath, ""))
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
	// wait is what the last attempt asked us to wait, which for a throttled
	// upstream is the upstream's own answer rather than our guess.
	var wait time.Duration
	for attempt := 0; attempt < u.retries; attempt++ {
		if attempt > 0 {
			sleep := wait
			if sleep <= 0 {
				// Exponential backoff with full jitter.
				backoff := backoffBase << (attempt - 1)
				sleep = time.Duration(rand.Int64N(int64(backoff))) + backoff/2 //nolint:gosec // jitter, not crypto
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(sleep):
			}
			wait = 0
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
		case throttleDelay(err, &wait):
			// The upstream is up and telling us the rate. Counting that as
			// a failure would open the breaker and turn "slow down" into
			// "this feed is down" for everyone — which is what a public
			// mirror's rate limit would do to a whole build.
			u.count("throttled")
			lastErr = err
			u.logger.Warn("upstream asked us to slow down",
				"feed", u.feed, "url", redactURL(target), "attempt", attempt+1,
				"retry_in", wait)
		case !retryable(err):
			u.breaker.Failure()
			u.count("error")
			return nil, err
		default:
			u.breaker.Failure()
			u.count("error")
			lastErr = err
			u.logger.Warn("upstream attempt failed",
				"feed", u.feed, "url", redactURL(target), "attempt", attempt+1, "error", err)
			if !u.breaker.Allow() {
				return nil, fmt.Errorf("upstream %s: circuit breaker opened after failures: %w", u.feed, api.ErrUpstreamUnavailable)
			}
		}
	}
	return nil, fmt.Errorf("upstream %s: %v: %w", u.feed, lastErr, api.ErrUpstreamUnavailable)
}

// FetchAll fetches a complete (metadata) body into memory.
func (u *Upstream) FetchAll(ctx context.Context, remotePath string) ([]byte, error) {
	return u.FetchAllQuery(ctx, remotePath, "")
}

// FetchAllQuery is FetchAll with a query string appended.
func (u *Upstream) FetchAllQuery(ctx context.Context, remotePath, query string) ([]byte, error) {
	resp, err := u.FetchQuery(ctx, remotePath, query)
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

// throttledError is a 429: the upstream is working and is asking for a lower
// rate. It is deliberately not a transientError, because those count against
// the circuit breaker and this must not — an upstream that rate-limits is
// not an upstream that is down.
type throttledError struct {
	retryIn time.Duration
	error
}

func (t throttledError) Unwrap() error { return t.error }

// throttleDelay reports whether err is a throttle, and how long it asked for.
func throttleDelay(err error, out *time.Duration) bool {
	var te throttledError
	if !errors.As(err, &te) {
		return false
	}
	*out = te.retryIn
	return true
}

// maxRetryAfter caps what an upstream can make a request wait. Honouring an
// hour-long Retry-After would hold a client connection open for an hour; the
// cache and stale-while-revalidate are the better answer past this point.
const maxRetryAfter = 30 * time.Second

// retryAfter reads the header in either of the forms RFC 9110 allows. An
// absent or unparseable value falls back to the caller's own backoff.
func retryAfter(header string) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(header); err == nil {
		return capDelay(time.Duration(seconds) * time.Second)
	}
	if at, err := http.ParseTime(header); err == nil {
		return capDelay(time.Until(at))
	}
	return 0
}

func capDelay(d time.Duration) time.Duration {
	switch {
	case d <= 0:
		return 0
	case d > maxRetryAfter:
		return maxRetryAfter
	default:
		return d
	}
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
	case resp.StatusCode == http.StatusTooManyRequests:
		retry := retryAfter(resp.Header.Get("Retry-After"))
		_ = resp.Body.Close()
		return nil, throttledError{
			retryIn: retry,
			error:   fmt.Errorf("upstream status 429 for %s", target),
		}
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
