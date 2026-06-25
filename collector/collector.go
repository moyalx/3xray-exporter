// Package collector wires the 3X-UI panel and Xray-core data sources into a
// single prometheus.Collector. Polling happens at scrape time (inside Collect)
// rather than on a background timer, so the panel is only queried when
// Prometheus actually scrapes. This keeps load on the panel proportional to
// the scrape interval and guarantees fresh data on every scrape.
package collector

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/yourusername/3xui-exporter/client"
)

const (
	namespaceXUI  = "xui"
	namespaceXray = "xray"
)

// Collector implements prometheus.Collector. Add new metric descriptors as
// fields here, describe them in Describe, and populate them in collectXUI or
// collectXray.
type Collector struct {
	xui  *client.XUIClient
	xray *client.XrayClient
	log  *slog.Logger

	// timeout bounds the total time spent gathering one scrape's data.
	timeout time.Duration

	// --- internal exporter health metrics ---
	up           *prometheus.Desc
	scrapeErrors *prometheus.Desc

	// --- panel metrics (xui_) ---
	usersOnlineTotal *prometheus.Desc
	userTrafficUsed  *prometheus.Desc
	userTrafficLimit *prometheus.Desc
	userExpireTime   *prometheus.Desc
	userEnabled      *prometheus.Desc
	inboundUp        *prometheus.Desc
	inboundDown      *prometheus.Desc

	// --- xray engine metrics (xray_) ---
	obsAlive *prometheus.Desc
	obsDelay *prometheus.Desc
	memAlloc *prometheus.Desc
	memSys   *prometheus.Desc
	memHeap  *prometheus.Desc
	memNumGC *prometheus.Desc
}

// New constructs a Collector. xray may be nil to disable the Xray data source.
func New(xui *client.XUIClient, xray *client.XrayClient, timeout time.Duration, log *slog.Logger) *Collector {
	return &Collector{
		xui:     xui,
		xray:    xray,
		log:     log,
		timeout: timeout,

		up: prometheus.NewDesc(
			"xui_exporter_up",
			"1 if the last scrape of all enabled data sources succeeded, 0 otherwise.",
			nil, nil,
		),
		scrapeErrors: prometheus.NewDesc(
			"xui_exporter_scrape_errors_total",
			"Number of data sources that failed during the current scrape.",
			[]string{"source"}, nil,
		),

		usersOnlineTotal: prometheus.NewDesc(
			namespaceXUI+"_users_online_total",
			"Total number of concurrently online clients across all inbounds.",
			nil, nil,
		),
		userTrafficUsed: prometheus.NewDesc(
			namespaceXUI+"_user_traffic_used_bytes",
			"Total bytes used (up+down) by a client.",
			[]string{"email", "inbound_tag"}, nil,
		),
		userTrafficLimit: prometheus.NewDesc(
			namespaceXUI+"_user_traffic_limit_bytes",
			"Configured data limit in bytes for a client (0 means unlimited).",
			[]string{"email", "inbound_tag"}, nil,
		),
		userExpireTime: prometheus.NewDesc(
			namespaceXUI+"_user_expire_timestamp",
			"Unix timestamp (seconds) when a client expires (0 means never).",
			[]string{"email", "inbound_tag"}, nil,
		),
		userEnabled: prometheus.NewDesc(
			namespaceXUI+"_user_enabled",
			"1 if the client is enabled, 0 otherwise.",
			[]string{"email", "inbound_tag"}, nil,
		),
		inboundUp: prometheus.NewDesc(
			namespaceXUI+"_inbound_up_bytes",
			"Total uplink bytes for an inbound.",
			[]string{"inbound_tag", "protocol"}, nil,
		),
		inboundDown: prometheus.NewDesc(
			namespaceXUI+"_inbound_down_bytes",
			"Total downlink bytes for an inbound.",
			[]string{"inbound_tag", "protocol"}, nil,
		),

		obsAlive: prometheus.NewDesc(
			namespaceXray+"_observatory_outbound_alive",
			"1 if the observatory considers the outbound healthy, 0 otherwise.",
			[]string{"outbound_tag"}, nil,
		),
		obsDelay: prometheus.NewDesc(
			namespaceXray+"_observatory_outbound_delay_ms",
			"Round-trip latency of the outbound in milliseconds as measured by the observatory.",
			[]string{"outbound_tag"}, nil,
		),
		memAlloc: prometheus.NewDesc(
			namespaceXray+"_core_memory_alloc_bytes",
			"Bytes of allocated heap objects in the Xray-core process (memstats.Alloc).",
			nil, nil,
		),
		memSys: prometheus.NewDesc(
			namespaceXray+"_core_memory_sys_bytes",
			"Total bytes of memory obtained from the OS by Xray-core (memstats.Sys).",
			nil, nil,
		),
		memHeap: prometheus.NewDesc(
			namespaceXray+"_core_memory_heap_inuse_bytes",
			"Bytes in in-use heap spans in Xray-core (memstats.HeapInuse).",
			nil, nil,
		),
		memNumGC: prometheus.NewDesc(
			namespaceXray+"_core_gc_total",
			"Number of completed GC cycles in Xray-core (memstats.NumGC).",
			nil, nil,
		),
	}
}

// Describe sends the static description of every metric to the channel.
// Implementing this (rather than relying on unchecked collectors) lets the
// registry detect duplicate or inconsistent descriptors at registration time.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.up
	ch <- c.scrapeErrors
	ch <- c.usersOnlineTotal
	ch <- c.userTrafficUsed
	ch <- c.userTrafficLimit
	ch <- c.userExpireTime
	ch <- c.userEnabled
	ch <- c.inboundUp
	ch <- c.inboundDown
	ch <- c.obsAlive
	ch <- c.obsDelay
	ch <- c.memAlloc
	ch <- c.memSys
	ch <- c.memHeap
	ch <- c.memNumGC
}

// Collect polls both data sources concurrently and emits the resulting
// samples. It never returns an error (the interface forbids it); instead a
// failed source increments xui_exporter_scrape_errors_total and pulls down
// xui_exporter_up so failures are observable in Prometheus.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		errsBySrc = map[string]int{}
	)
	recordErr := func(src string) {
		mu.Lock()
		errsBySrc[src]++
		mu.Unlock()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := c.collectXUI(ctx, ch); err != nil {
			c.log.Error("scraping 3X-UI panel failed", "err", err)
			recordErr("xui")
		}
	}()

	if c.xray != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.collectXray(ctx, ch); err != nil {
				c.log.Error("scraping Xray expvar failed", "err", err)
				recordErr("xray")
			}
		}()
	}

	wg.Wait()

	// Emit per-source error counts and an overall up gauge.
	totalErrs := 0
	for _, src := range []string{"xui", "xray"} {
		ch <- prometheus.MustNewConstMetric(c.scrapeErrors, prometheus.CounterValue, float64(errsBySrc[src]), src)
		totalErrs += errsBySrc[src]
	}
	upValue := 1.0
	if totalErrs > 0 {
		upValue = 0
	}
	ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, upValue)
}

// collectXUI gathers inbound and per-client metrics from the panel.
func (c *Collector) collectXUI(ctx context.Context, ch chan<- prometheus.Metric) error {
	inbounds, err := c.xui.Inbounds(ctx)
	if err != nil {
		return err
	}

	for _, in := range inbounds {
		tag := inboundLabel(in)

		ch <- prometheus.MustNewConstMetric(c.inboundUp, prometheus.GaugeValue, float64(in.Up), tag, in.Protocol)
		ch <- prometheus.MustNewConstMetric(c.inboundDown, prometheus.GaugeValue, float64(in.Down), tag, in.Protocol)

		for _, cl := range in.ClientStats {
			if cl.Email == "" {
				continue
			}
			ch <- prometheus.MustNewConstMetric(
				c.userTrafficUsed, prometheus.GaugeValue, float64(cl.Up+cl.Down), cl.Email, tag,
			)
			ch <- prometheus.MustNewConstMetric(
				c.userTrafficLimit, prometheus.GaugeValue, float64(cl.Total), cl.Email, tag,
			)
			// 3X-UI stores expiry in milliseconds; export seconds for Prometheus convention.
			ch <- prometheus.MustNewConstMetric(
				c.userExpireTime, prometheus.GaugeValue, msToSeconds(cl.ExpiryTime), cl.Email, tag,
			)
			ch <- prometheus.MustNewConstMetric(
				c.userEnabled, prometheus.GaugeValue, boolToFloat(cl.Enable), cl.Email, tag,
			)
		}
	}

	// Online users is a separate endpoint; treat its failure as non-fatal for
	// the rest of the panel data we already emitted, but still surface it.
	online, err := c.xui.OnlineClients(ctx)
	if err != nil {
		return err
	}
	ch <- prometheus.MustNewConstMetric(c.usersOnlineTotal, prometheus.GaugeValue, float64(len(online)))

	return nil
}

// collectXray gathers observatory health and runtime memory metrics.
func (c *Collector) collectXray(ctx context.Context, ch chan<- prometheus.Metric) error {
	vars, err := c.xray.Vars(ctx)
	if err != nil {
		return err
	}

	for tag, status := range vars.Observatory {
		// Prefer the tag embedded in the status if the map key is empty.
		outboundTag := tag
		if outboundTag == "" {
			outboundTag = status.OutboundTag
		}
		ch <- prometheus.MustNewConstMetric(c.obsAlive, prometheus.GaugeValue, boolToFloat(status.Alive), outboundTag)
		ch <- prometheus.MustNewConstMetric(c.obsDelay, prometheus.GaugeValue, float64(status.Delay), outboundTag)
	}

	ms := vars.MemStats
	ch <- prometheus.MustNewConstMetric(c.memAlloc, prometheus.GaugeValue, float64(ms.Alloc))
	ch <- prometheus.MustNewConstMetric(c.memSys, prometheus.GaugeValue, float64(ms.Sys))
	ch <- prometheus.MustNewConstMetric(c.memHeap, prometheus.GaugeValue, float64(ms.HeapInuse))
	ch <- prometheus.MustNewConstMetric(c.memNumGC, prometheus.CounterValue, float64(ms.NumGC))

	return nil
}

// inboundLabel returns a stable, human-friendly identifier for an inbound,
// preferring the Xray tag and falling back to the remark or port.
func inboundLabel(in client.Inbound) string {
	switch {
	case in.Tag != "":
		return in.Tag
	case in.Remark != "":
		return in.Remark
	default:
		return "inbound-" + strconv.Itoa(in.Port)
	}
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// msToSeconds converts a millisecond timestamp to fractional seconds, leaving
// the sentinel value 0 (never expires) untouched.
func msToSeconds(ms int64) float64 {
	if ms == 0 {
		return 0
	}
	return float64(ms) / 1000.0
}
