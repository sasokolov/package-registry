package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Token handshakes with an upstream that answers 401.
//
// Some registries publish everything anonymously but still require a token
// for every request: the first attempt is refused with a WWW-Authenticate
// header naming the endpoint that hands them out, and the same request
// succeeds with the token attached. Docker Hub, ghcr.io and quay.io all work
// this way, so without this a proxy of any of them fetches nothing at all.
//
// This is HTTP, not protocol knowledge: the core never learns what an image
// is, only that an upstream answered 401 and said where to ask. The realm
// comes from the upstream, so it goes through the same destination check as
// any other location an upstream hands us, and the token itself is never
// logged (invariant 12).
//
// Tokens are cached in memory with the expiry the issuer gave, which is a
// derived cache with a TTL: losing it costs one extra handshake and nothing
// else (invariant 3).

// tokenLifetimeDefault is used when the issuer names no expiry. The spec's
// own default is 60 seconds.
const tokenLifetimeDefault = 60 * time.Second

// tokenLifetimeSlack is subtracted from an issuer's expiry so a token is not
// presented in the second it stops being valid.
const tokenLifetimeSlack = 10 * time.Second

type cachedToken struct {
	value   string
	expires time.Time
}

// tokenCache holds bearer tokens per challenge scope.
type tokenCache struct {
	mu     sync.Mutex
	tokens map[string]cachedToken
	now    func() time.Time
}

func newTokenCache(now func() time.Time) *tokenCache {
	if now == nil {
		now = time.Now
	}
	return &tokenCache{tokens: map[string]cachedToken{}, now: now}
}

func (c *tokenCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.tokens[key]
	if !ok || !c.now().Before(t.expires) {
		return "", false
	}
	return t.value, true
}

func (c *tokenCache) put(key, value string, lifetime time.Duration) {
	if lifetime <= 0 {
		lifetime = tokenLifetimeDefault
	}
	if lifetime > tokenLifetimeSlack {
		lifetime -= tokenLifetimeSlack
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokens[key] = cachedToken{value: value, expires: c.now().Add(lifetime)}
}

// challenge is a parsed WWW-Authenticate value.
type challenge struct {
	scheme string
	params map[string]string
}

// parseChallenge reads the first challenge out of a WWW-Authenticate header.
//
// Only the leading scheme is read. A header offering several is answered by
// its first, which is what the issuing side put in front for a reason; the
// alternative — trying each in turn — would turn one refused request into a
// series of them.
func parseChallenge(header string) challenge {
	header = strings.TrimSpace(header)
	if header == "" {
		return challenge{}
	}
	scheme, rest, _ := strings.Cut(header, " ")
	out := challenge{scheme: strings.ToLower(scheme), params: map[string]string{}}
	for _, part := range splitParams(rest) {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.Trim(strings.TrimSpace(value), `"`)
		if key != "" {
			out.params[key] = value
		}
	}
	return out
}

// splitParams splits on commas that are not inside a quoted value: a scope
// is one parameter and legitimately contains commas.
func splitParams(s string) []string {
	var out []string
	var current strings.Builder
	inQuotes := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuotes = !inQuotes
			current.WriteRune(r)
		case r == ',' && !inQuotes:
			out = append(out, current.String())
			current.Reset()
		default:
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		out = append(out, current.String())
	}
	return out
}

// authorize attaches a token already held for this request's scope.
//
// Scope is not known before the challenge, so the cache is keyed by the
// request URL's path prefix — the same repository asked for the same way.
func (u *Upstream) authorize(req *http.Request) {
	if u.tokens == nil {
		return
	}
	if token, ok := u.tokens.get(scopeKey(req.URL)); ok {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// retryAuthorized answers a 401 by fetching a token and repeating the
// request once. It reports whether it produced a new response; the caller
// keeps the original one otherwise, so a 401 that cannot be answered is
// still handled as the plain status it is.
func (u *Upstream) retryAuthorized(ctx context.Context, req *http.Request, resp *http.Response, o FetchOpts) (*http.Response, bool, error) {
	if req.Header.Get("Authorization") != "" {
		// A token was already presented and still refused: asking for
		// another one would loop.
		return nil, false, nil
	}
	ch := parseChallenge(resp.Header.Get("WWW-Authenticate"))
	if ch.scheme != "bearer" || ch.params["realm"] == "" {
		return nil, false, nil
	}
	_ = resp.Body.Close()

	token, lifetime, err := u.fetchToken(ctx, ch)
	if err != nil {
		u.logger.Warn("upstream token handshake failed",
			"feed", u.feed, "url", redactURL(req.URL.String()), "error", err)
		return nil, false, transientError{fmt.Errorf("upstream %s: token handshake: %w", u.feed, err)}
	}
	u.tokens.put(scopeKey(req.URL), token, lifetime)

	retry := req.Clone(ctx)
	retry.Header.Set("Authorization", "Bearer "+token)
	if o.Accept != "" {
		retry.Header.Set("Accept", o.Accept)
	}
	next, err := u.client.Do(retry)
	if err != nil {
		return nil, false, transientError{fmt.Errorf("request %s: %w", redactURL(req.URL.String()), err)}
	}
	return next, true, nil
}

// scopeKey is what a token is cached against: the upstream host and the
// first two path segments after the API root, which is as much of a
// repository name as any protocol puts there.
func scopeKey(u *url.URL) string {
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) > 3 {
		parts = parts[:3]
	}
	return u.Host + "/" + strings.Join(parts, "/")
}

// fetchToken carries out the handshake the challenge describes, returning
// the token and how long the issuer says it lasts.
func (u *Upstream) fetchToken(ctx context.Context, ch challenge) (string, time.Duration, error) {
	realm, err := url.Parse(ch.params["realm"])
	if err != nil || !realm.IsAbs() || (realm.Scheme != "http" && realm.Scheme != "https") {
		return "", 0, fmt.Errorf("challenge realm %q is not an absolute http(s) URL", ch.params["realm"])
	}
	// The realm came from the upstream, i.e. from outside: it gets the same
	// destination check as any other location an upstream hands us.
	if err := u.checkDestination(realm); err != nil {
		return "", 0, err
	}
	query := realm.Query()
	for _, name := range []string{"service", "scope"} {
		if v := ch.params[name]; v != "" {
			query.Set(name, v)
		}
	}
	realm.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("token endpoint answered %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", 0, err
	}
	var doc struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", 0, fmt.Errorf("token endpoint returned no usable document: %w", err)
	}
	token := doc.Token
	if token == "" {
		token = doc.AccessToken
	}
	if token == "" {
		return "", 0, fmt.Errorf("token endpoint returned no token")
	}
	return token, time.Duration(doc.ExpiresIn) * time.Second, nil
}
