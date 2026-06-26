// Package client contains HTTP clients for the two data sources this exporter
// scrapes: the 3X-UI panel REST API (xui.go) and the Xray-core expvar metrics
// endpoint (xray.go).
package client

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"
)

// userAgent is sent on every request. Some panels sit behind a WAF/CDN that
// rejects requests with an empty or default Go user agent.
const userAgent = "3xui-exporter"

// httpStatusError carries the HTTP status of a non-2xx panel response so
// callers can branch on it (e.g. fall back to a legacy endpoint on 404).
type httpStatusError struct {
	status int
	method string
	path   string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("panel %s %s returned HTTP %d", e.method, e.path, e.status)
}

// ---------------------------------------------------------------------------
// API response models
//
// These structs mirror the JSON returned by the 3X-UI panel API. The panel is
// built on top of Xray-core and stores per-client traffic accounting in its
// database; the API surfaces that as the structures below.
//
// To add a new endpoint: define its response struct here, then add a method on
// *XUIClient that calls c.getJSON(...) and unmarshals into it.
// ---------------------------------------------------------------------------

// apiResponse is the standard envelope wrapping every 3X-UI API payload.
// The concrete data lives in Obj, which we decode lazily per-endpoint.
type apiResponse struct {
	Success bool            `json:"success"`
	Msg     string          `json:"msg"`
	Obj     json.RawMessage `json:"obj"`
}

// Inbound represents a single inbound configuration plus its aggregated and
// per-client traffic statistics, as returned by /panel/api/inbounds/list.
type Inbound struct {
	ID          int             `json:"id"`
	Up          int64           `json:"up"`
	Down        int64           `json:"down"`
	Total       int64           `json:"total"`
	Remark      string          `json:"remark"`
	Enable      bool            `json:"enable"`
	ExpiryTime  int64           `json:"expiryTime"`
	Listen      string          `json:"listen"`
	Port        int             `json:"port"`
	Protocol    string          `json:"protocol"`
	Tag         string          `json:"tag"`
	ClientStats []ClientTraffic `json:"clientStats"`
}

// ClientTraffic is the per-user traffic accounting record nested inside an
// Inbound. Up/Down are cumulative byte counters; Total is the configured data
// limit (0 means unlimited); ExpiryTime is a Unix timestamp in milliseconds
// (0 means it never expires). 3X-UI stores timestamps in milliseconds.
type ClientTraffic struct {
	ID         int    `json:"id"`
	InboundID  int    `json:"inboundId"`
	Enable     bool   `json:"enable"`
	Email      string `json:"email"`
	Up         int64  `json:"up"`
	Down       int64  `json:"down"`
	ExpiryTime int64  `json:"expiryTime"`
	Total      int64  `json:"total"`
	Reset      int    `json:"reset"`
}

// XUIClient is a thread-safe client for the 3X-UI panel API.
//
// It supports the two authentication modes of 3X-UI v3+:
//
//   - Bearer token (recommended): when apiToken is set, every /panel/api/*
//     request carries "Authorization: Bearer <token>", which bypasses the
//     panel's CSRF protection. No /login is performed.
//   - Session cookie: when only username/password are set, it logs in via
//     /login. Because v3 guards /login (and unsafe API methods) with CSRF, the
//     client first mints a CSRF token from /csrf-token and replays it in the
//     X-CSRF-Token header.
type XUIClient struct {
	baseURL  string
	username string
	password string
	apiToken string

	http *http.Client

	// mu guards the session auth flow so concurrent scrapes do not trigger
	// multiple simultaneous logins. The cookie lives in the http client's jar.
	mu            sync.Mutex
	authenticated bool
	csrfToken     string
}

// NewXUIClient builds a panel client. If apiToken is non-empty the client uses
// Bearer-token auth; otherwise it falls back to username/password session auth.
// The cookie jar persists the session cookie across requests automatically.
func NewXUIClient(baseURL, username, password, apiToken string, timeout time.Duration, insecure bool) (*XUIClient, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("creating cookie jar: %w", err)
	}

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure}, // #nosec G402 - opt-in for self-signed panels
	}

	return &XUIClient{
		baseURL:  strings.TrimRight(baseURL, "/"),
		username: username,
		password: password,
		apiToken: apiToken,
		http: &http.Client{
			Timeout:   timeout,
			Jar:       jar,
			Transport: transport,
		},
	}, nil
}

// usingToken reports whether the client authenticates with a Bearer token.
func (c *XUIClient) usingToken() bool { return c.apiToken != "" }

// login authenticates against /login and stores the returned session cookie in
// the client's cookie jar. On v3+ it first mints a CSRF token and sends it in
// the X-CSRF-Token header, otherwise the panel rejects the POST with 403.
// It must be called with c.mu held.
func (c *XUIClient) login(ctx context.Context) error {
	// Mint a CSRF token (this also seeds the session cookie in the jar). Older
	// panels (pre-v3) have no /csrf-token endpoint and no CSRF requirement, so
	// a failure here is non-fatal: we simply proceed without a token.
	csrf, _ := c.fetchCSRF(ctx)

	form := url.Values{}
	form.Set("username", c.username)
	form.Set("password", c.password)

	endpoint := c.baseURL + "/login"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("building login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("sending login request: %w", err)
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("login returned HTTP %d (a 403 usually means a CSRF/credentials problem)", resp.StatusCode)
	}

	var out apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("decoding login response: %w", err)
	}
	if !out.Success {
		return fmt.Errorf("login rejected by panel: %s", out.Msg)
	}

	// The session cookie is now stored in the jar; keep the CSRF token so we
	// can attach it to unsafe (POST) API requests during this session.
	c.csrfToken = csrf
	c.authenticated = true
	return nil
}

// fetchCSRF retrieves a CSRF token from GET /csrf-token. The envelope's obj
// field holds the token string. Returns an empty string (and error) if the
// endpoint is absent, which is expected on pre-v3 panels.
func (c *XUIClient) fetchCSRF(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/csrf-token", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", &httpStatusError{status: resp.StatusCode, method: http.MethodGet, path: "/csrf-token"}
	}

	var env apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return "", err
	}
	var token string
	_ = json.Unmarshal(env.Obj, &token)
	return token, nil
}

// ensureAuth guarantees the client can authenticate before issuing a request.
// In token mode this is a no-op; in session mode it logs in once.
func (c *XUIClient) ensureAuth(ctx context.Context) error {
	if c.usingToken() {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.authenticated {
		return nil
	}
	return c.login(ctx)
}

// applyAuthHeaders sets auth-related headers on an outbound API request.
//   - Token mode: Authorization: Bearer <token> (also bypasses CSRF).
//   - Session mode: X-CSRF-Token on unsafe methods (POST/PUT/DELETE/PATCH).
func (c *XUIClient) applyAuthHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if c.usingToken() {
		req.Header.Set("Authorization", "Bearer "+c.apiToken)
		return
	}
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		// Safe methods are not CSRF-protected.
	default:
		if c.csrfToken != "" {
			req.Header.Set("X-CSRF-Token", c.csrfToken)
		}
	}
}

// getJSON performs an authenticated request to the given panel path and decodes
// the standard envelope's Obj field into out. On a 401 it re-authenticates once
// and retries, which keeps long-running exporters resilient to cookie expiry.
//
// method is the HTTP method; several 3X-UI endpoints expect POST even for
// read-only operations, so callers specify it explicitly.
func (c *XUIClient) getJSON(ctx context.Context, method, path string, out interface{}) error {
	if err := c.ensureAuth(ctx); err != nil {
		return fmt.Errorf("authenticating: %w", err)
	}

	body, status, err := c.do(ctx, method, path)
	if err != nil {
		return err
	}

	// Session expiry (401) or CSRF rejection (403): in session mode, force a
	// fresh login + CSRF token and retry exactly once. In token mode a 401/403
	// means the token is invalid/disabled, so retrying would not help.
	if (status == http.StatusUnauthorized || status == http.StatusForbidden) && !c.usingToken() {
		c.mu.Lock()
		c.authenticated = false
		err = c.login(ctx)
		c.mu.Unlock()
		if err != nil {
			return fmt.Errorf("re-authenticating after HTTP %d: %w", status, err)
		}
		if body, status, err = c.do(ctx, method, path); err != nil {
			return err
		}
	}

	if status != http.StatusOK {
		return &httpStatusError{status: status, method: method, path: path}
	}

	var env apiResponse
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("decoding envelope from %s: %w", path, err)
	}
	if !env.Success {
		return fmt.Errorf("panel %s reported failure: %s", path, env.Msg)
	}
	if out == nil || len(env.Obj) == 0 {
		return nil
	}
	if err := json.Unmarshal(env.Obj, out); err != nil {
		return fmt.Errorf("decoding obj from %s: %w", path, err)
	}
	return nil
}

// do issues a single authenticated HTTP request and returns the raw body and
// status code.
func (c *XUIClient) do(ctx context.Context, method, path string) ([]byte, int, error) {
	endpoint := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("building request for %s: %w", path, err)
	}
	c.applyAuthHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("requesting %s: %w", path, err)
	}
	defer drainAndClose(resp.Body)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("reading response from %s: %w", path, err)
	}
	return body, resp.StatusCode, nil
}

// Inbounds fetches every inbound and its nested per-client traffic stats.
// Endpoint: GET /panel/api/inbounds/list.
func (c *XUIClient) Inbounds(ctx context.Context) ([]Inbound, error) {
	var inbounds []Inbound
	if err := c.getJSON(ctx, http.MethodGet, "/panel/api/inbounds/list", &inbounds); err != nil {
		return nil, err
	}
	return inbounds, nil
}

// OnlineClients returns the list of client emails currently considered online
// by the panel. The endpoint moved in v3 to POST /panel/api/clients/onlines;
// if that 404s (older panel) we fall back to the legacy
// POST /panel/api/inbounds/onlines path.
func (c *XUIClient) OnlineClients(ctx context.Context) ([]string, error) {
	var emails []string
	err := c.getJSON(ctx, http.MethodPost, "/panel/api/clients/onlines", &emails)
	if err != nil {
		var statusErr *httpStatusError
		if errors.As(err, &statusErr) && statusErr.status == http.StatusNotFound {
			emails = nil
			if err = c.getJSON(ctx, http.MethodPost, "/panel/api/inbounds/onlines", &emails); err == nil {
				return emails, nil
			}
		}
		return nil, err
	}
	return emails, nil
}

// drainAndClose fully consumes and closes a response body so the underlying
// TCP connection can be reused by the keep-alive pool.
func drainAndClose(rc io.ReadCloser) {
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()
}
