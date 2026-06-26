// Package config loads runtime configuration for the exporter.
//
// All configuration is sourced from environment variables (12-factor style)
// using github.com/kelseyhightower/envconfig. There are intentionally no flags
// or config files so the exporter is trivial to run inside containers and
// orchestrators where environment variables are the native configuration unit.
package config

import (
	"fmt"
	"time"

	"github.com/kelseyhightower/envconfig"
)

// Config holds every tunable for the exporter. Tags map each field to an
// environment variable. Add new fields here (with sensible defaults) when you
// introduce new data sources or behaviours.
type Config struct {
	// XUIURI is the base URL of the 3X-UI panel, e.g. http://127.0.0.1:2053.
	// If your panel is served under a custom base path (XUI_INIT_WEB_BASE_PATH),
	// include it here, e.g. http://127.0.0.1:2053/mypanel.
	XUIURI string `envconfig:"XUI_URI" required:"true"`

	// XUIAPIToken is a panel API token (Bearer) created under
	// Settings -> Security -> API Token. This is the RECOMMENDED auth method
	// for 3X-UI v3+: it is sent as "Authorization: Bearer <token>" on every
	// /panel/api/* request and bypasses the panel's CSRF protection entirely,
	// so no /login round-trip is needed. When set, XUI_USERNAME / XUI_PASSWORD
	// are ignored.
	XUIAPIToken string `envconfig:"XUI_API_TOKEN" default:""`

	// XUIUsername / XUIPassword are the panel login credentials used to obtain
	// a session cookie via the /login endpoint. Only used when XUI_API_TOKEN is
	// empty. On v3+ the exporter performs the required CSRF handshake for this
	// flow automatically.
	XUIUsername string `envconfig:"XUI_USERNAME" default:""`
	XUIPassword string `envconfig:"XUI_PASSWORD" default:""`

	// XrayMetricsURI is the full URL of the Xray-core expvar endpoint, e.g.
	// http://127.0.0.1:11111/debug/vars. Leave empty to disable Xray metrics.
	XrayMetricsURI string `envconfig:"XRAY_METRICS_URI" default:""`

	// ListenAddr is the address the exporter's HTTP server binds to.
	ListenAddr string `envconfig:"LISTEN_ADDR" default:":9808"`

	// MetricsPath is the HTTP path that exposes Prometheus metrics.
	MetricsPath string `envconfig:"METRICS_PATH" default:"/metrics"`

	// Timeout bounds every outbound HTTP request to the panel and Xray.
	Timeout time.Duration `envconfig:"HTTP_TIMEOUT" default:"10s"`

	// InsecureSkipVerify disables TLS certificate verification when talking to
	// the panel over HTTPS (useful for self-signed certs). Default false.
	InsecureSkipVerify bool `envconfig:"INSECURE_SKIP_VERIFY" default:"false"`
}

// Load reads the configuration from the environment and validates it.
func Load() (*Config, error) {
	var c Config
	if err := envconfig.Process("", &c); err != nil {
		return nil, fmt.Errorf("parsing environment configuration: %w", err)
	}

	// Require exactly one authentication method to be configured.
	if c.XUIAPIToken == "" && (c.XUIUsername == "" || c.XUIPassword == "") {
		return nil, fmt.Errorf("authentication not configured: set XUI_API_TOKEN (recommended), or both XUI_USERNAME and XUI_PASSWORD")
	}
	return &c, nil
}
