// Package client contains HTTP clients for the two data sources this exporter
// scrapes: the 3X-UI panel REST API (xui.go) and the Xray-core expvar metrics
// endpoint (xray.go).
package client

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"
)

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

// XUIClient is a thread-safe client for the 3X-UI panel API. It manages a
// session cookie obtained via /login and transparently re-authenticates when
// the panel responds with 401 Unauthorized.
type XUIClient struct {
	baseURL  string
	username string
	password string

	http *http.Client

	// mu guards the authentication flow so concurrent scrapes do not trigger
	// multiple simultaneous logins. The cookie itself lives in the http
	// client's cookie jar.
	mu            sync.Mutex
	authenticated bool
}

// NewXUIClient builds a panel client. The cookie jar persists the session
// cookie returned by /login across subsequent requests automatically.
func NewXUIClient(baseURL, username, password string, timeout time.Duration, insecure bool) (*XUIClient, error) {
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
		http: &http.Client{
			Timeout:   timeout,
			Jar:       jar,
			Transport: transport,
		},
	}, nil
}

// login authenticates against /login and stores the returned session cookie in
// the client's cookie jar. It is safe to call concurrently.
func (c *XUIClient) login(ctx context.Context) error {
	form := url.Values{}
	form.Set("username", c.username)
	form.Set("password", c.password)

	endpoint := c.baseURL + "/login"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("building login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("sending login request: %w", err)
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("login returned HTTP %d", resp.StatusCode)
	}

	var out apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("decoding login response: %w", err)
	}
	if !out.Success {
		return fmt.Errorf("login rejected by panel: %s", out.Msg)
	}

	// The session cookie is now stored in the jar by the http client.
	c.authenticated = true
	return nil
}

// ensureAuth guarantees a valid session exists before issuing a request.
func (c *XUIClient) ensureAuth(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.authenticated {
		return nil
	}
	return c.login(ctx)
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

	// Cookie expired: force a fresh login and retry exactly once.
	if status == http.StatusUnauthorized {
		c.mu.Lock()
		c.authenticated = false
		err = c.login(ctx)
		c.mu.Unlock()
		if err != nil {
			return fmt.Errorf("re-authenticating after 401: %w", err)
		}
		if body, status, err = c.do(ctx, method, path); err != nil {
			return err
		}
	}

	if status != http.StatusOK {
		return fmt.Errorf("panel %s %s returned HTTP %d", method, path, status)
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

// do issues a single HTTP request and returns the raw body and status code.
func (c *XUIClient) do(ctx context.Context, method, path string) ([]byte, int, error) {
	endpoint := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("building request for %s: %w", path, err)
	}

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
// by the panel. Endpoint: POST /panel/api/inbounds/onlines.
func (c *XUIClient) OnlineClients(ctx context.Context) ([]string, error) {
	var emails []string
	if err := c.getJSON(ctx, http.MethodPost, "/panel/api/inbounds/onlines", &emails); err != nil {
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
