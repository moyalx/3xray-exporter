package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// ---------------------------------------------------------------------------
// Xray-core expvar models
//
// Xray-core, when started with the metrics/observatory features enabled,
// publishes a Go expvar endpoint (default :11111/debug/vars). The payload is a
// single JSON object that includes the standard Go runtime "memstats" plus
// Xray-specific "observatory" and "stats" sections.
// ---------------------------------------------------------------------------

// XrayVars is the top-level expvar document. Only the fields we export are
// modelled; unknown keys are ignored by encoding/json.
type XrayVars struct {
	// MemStats is the standard runtime.MemStats published by Go's expvar.
	MemStats MemStats `json:"memstats"`

	// Observatory maps an outbound tag to its latest health probe result.
	// The observatory feature periodically measures each outbound's liveness
	// and latency.
	Observatory map[string]ObservatoryStatus `json:"observatory"`
}

// MemStats is a trimmed subset of runtime.MemStats exposed via expvar.
// Extend this struct to export additional Go runtime gauges.
type MemStats struct {
	Alloc      uint64 `json:"Alloc"`
	TotalAlloc uint64 `json:"TotalAlloc"`
	Sys        uint64 `json:"Sys"`
	HeapAlloc  uint64 `json:"HeapAlloc"`
	HeapInuse  uint64 `json:"HeapInuse"`
	NumGC      uint32 `json:"NumGC"`
}

// ObservatoryStatus is a single outbound's health record. Delay is the
// measured round-trip latency in milliseconds; Alive indicates whether the
// last probe succeeded.
type ObservatoryStatus struct {
	Alive           bool   `json:"alive"`
	Delay           int64  `json:"delay"`
	LastErrorReason string `json:"last_error_reason"`
	OutboundTag     string `json:"outbound_tag"`
	LastSeenTime    int64  `json:"last_seen_time"`
	LastTryTime     int64  `json:"last_try_time"`
}

// XrayClient fetches and parses the Xray-core expvar metrics document.
type XrayClient struct {
	metricsURL string
	http       *http.Client
}

// NewXrayClient builds a client for the given expvar URL. A nil client is
// returned when metricsURL is empty so callers can cheaply disable the source.
func NewXrayClient(metricsURL string, timeout time.Duration) *XrayClient {
	if metricsURL == "" {
		return nil
	}
	return &XrayClient{
		metricsURL: metricsURL,
		http:       &http.Client{Timeout: timeout},
	}
}

// Vars fetches the expvar document and decodes it. The endpoint is unauthenticated
// and expected to be bound to localhost on the same host as Xray-core.
func (c *XrayClient) Vars(ctx context.Context) (*XrayVars, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.metricsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building xray metrics request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("requesting xray metrics: %w", err)
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("xray metrics returned HTTP %d", resp.StatusCode)
	}

	var vars XrayVars
	if err := json.NewDecoder(resp.Body).Decode(&vars); err != nil {
		return nil, fmt.Errorf("decoding xray expvar JSON: %w", err)
	}
	return &vars, nil
}
