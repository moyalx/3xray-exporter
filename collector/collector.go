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
	"strings"
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

	// --- balancer live selection (from panel RoutingService) ---
	balSelected     *prometheus.Desc
	balOnFallback   *prometheus.Desc
	balFallbackTag  *prometheus.Desc
	balOutboundRole *prometheus.Desc
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

		balSelected: prometheus.NewDesc(
			namespaceXray+"_balancer_outbound_selected",
			"1 if this outbound is currently selected by the balancer (RoutingService principle target / override).",
			[]string{"balancer_tag", "outbound_tag", "via_fallback"}, nil,
		),
		balOnFallback: prometheus.NewDesc(
			namespaceXray+"_balancer_on_fallback",
			"1 if the balancer's live selection is its configured fallbackTag.",
			[]string{"balancer_tag"}, nil,
		),
		balFallbackTag: prometheus.NewDesc(
			namespaceXray+"_balancer_fallback_outbound",
			"1 for the outbound configured as this balancer's fallbackTag.",
			[]string{"balancer_tag", "outbound_tag"}, nil,
		),
		balOutboundRole: prometheus.NewDesc(
			namespaceXray+"_balancer_outbound_role",
			"Balancer member role: 3=selected via fallback, 2=selected from pool, 1=standby pool member, 0=configured fallback idle.",
			[]string{"balancer_tag", "outbound_tag"}, nil,
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
	ch <- c.balSelected
	ch <- c.balOnFallback
	ch <- c.balFallbackTag
	ch <- c.balOutboundRole
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

	// Balancer live selection is best-effort: a failure here must not hide the
	// inbound/client metrics we already emitted.
	if err := c.collectBalancers(ctx, ch); err != nil {
		c.log.Warn("scraping balancer status failed", "err", err)
	}

	return nil
}

// collectBalancers reads routing.balancers from the Xray template and the live
// picks from POST /panel/api/xray/balancerStatus.
func (c *Collector) collectBalancers(ctx context.Context, ch chan<- prometheus.Metric) error {
	settings, err := c.xui.XraySettings(ctx)
	if err != nil {
		return err
	}
	if settings.XraySetting.Routing == nil || len(settings.XraySetting.Routing.Balancers) == 0 {
		return nil
	}

	balancers := settings.XraySetting.Routing.Balancers
	tags := make([]string, 0, len(balancers))
	for _, b := range balancers {
		if b.Tag != "" {
			tags = append(tags, b.Tag)
		}
	}
	live, err := c.xui.BalancerStatuses(ctx, tags)
	if err != nil {
		return err
	}

	outboundTags := collectOutboundTags(settings)

	for _, b := range balancers {
		if b.Tag == "" {
			continue
		}
		status := live[b.Tag]
		selected := firstSelected(status)
		viaFallback := b.FallbackTag != "" && selected != "" && selected == b.FallbackTag

		ch <- prometheus.MustNewConstMetric(c.balOnFallback, prometheus.GaugeValue, boolToFloat(viaFallback), b.Tag)
		if b.FallbackTag != "" {
			ch <- prometheus.MustNewConstMetric(c.balFallbackTag, prometheus.GaugeValue, 1, b.Tag, b.FallbackTag)
		}

		if selected != "" {
			ch <- prometheus.MustNewConstMetric(
				c.balSelected, prometheus.GaugeValue, 1,
				b.Tag, selected, strconv.FormatBool(viaFallback),
			)
		}

		// Role for every pool member + configured fallback.
		emitted := map[string]struct{}{}
		emitRole := func(outbound string, role float64) {
			if outbound == "" {
				return
			}
			if _, ok := emitted[outbound]; ok {
				return
			}
			emitted[outbound] = struct{}{}
			ch <- prometheus.MustNewConstMetric(c.balOutboundRole, prometheus.GaugeValue, role, b.Tag, outbound)
		}

		for _, tag := range outboundTags {
			if !matchesSelector(tag, b.Selector) {
				continue
			}
			switch {
			case tag == selected && viaFallback:
				emitRole(tag, 3)
			case tag == selected:
				emitRole(tag, 2)
			default:
				emitRole(tag, 1)
			}
		}
		if b.FallbackTag != "" {
			switch {
			case viaFallback:
				emitRole(b.FallbackTag, 3)
			case selected == b.FallbackTag:
				emitRole(b.FallbackTag, 2)
			default:
				emitRole(b.FallbackTag, 0)
			}
		}
		// Selected may be outside the known outbound list (e.g. subscription
		// tag race); still emit its role.
		if selected != "" {
			if viaFallback {
				emitRole(selected, 3)
			} else {
				emitRole(selected, 2)
			}
		}
	}
	return nil
}

func collectOutboundTags(settings *client.XraySettingsPayload) []string {
	seen := map[string]struct{}{}
	var tags []string
	add := func(tag string) {
		if tag == "" {
			return
		}
		if _, ok := seen[tag]; ok {
			return
		}
		seen[tag] = struct{}{}
		tags = append(tags, tag)
	}
	for _, o := range settings.XraySetting.Outbounds {
		add(o.Tag)
	}
	for _, tag := range settings.SubscriptionOutboundTags {
		add(tag)
	}
	return tags
}

func firstSelected(status client.BalancerLiveStatus) string {
	if status.Override != "" {
		return status.Override
	}
	if len(status.Selected) > 0 {
		return status.Selected[0]
	}
	return ""
}

// matchesSelector mirrors Xray's prefix match used by balancers/observatory.
func matchesSelector(tag string, selectors []string) bool {
	if len(selectors) == 0 {
		return false
	}
	for _, sel := range selectors {
		if sel == "" || strings.HasPrefix(tag, sel) {
			return true
		}
	}
	return false
}

// collectXray gathers observatory health and runtime memory metrics.
func (c *Collector) collectXray(ctx context.Context, ch chan<- prometheus.Metric) error {
	vars, err := c.xray.Vars(ctx)
	if err != nil {
		return err
	}

	emittedObs := false
	for tag, status := range vars.Observatory {
		outboundTag := observatoryTag(tag, status)
		ch <- prometheus.MustNewConstMetric(c.obsAlive, prometheus.GaugeValue, boolToFloat(status.Alive), outboundTag)
		ch <- prometheus.MustNewConstMetric(c.obsDelay, prometheus.GaugeValue, float64(status.Delay), outboundTag)
		emittedObs = true
	}

	// When expvar has no observatory map (null/empty), fall back to the panel's
	// cached snapshot from GET /panel/api/server/xrayObservatory.
	if !emittedObs {
		if snaps, snapErr := c.xui.ObservatorySnapshots(ctx); snapErr != nil {
			c.log.Warn("panel observatory snapshot unavailable", "err", snapErr)
		} else {
			for _, s := range snaps {
				tag := s.Tag
				if tag == "" {
					continue
				}
				ch <- prometheus.MustNewConstMetric(c.obsAlive, prometheus.GaugeValue, boolToFloat(s.Alive), tag)
				ch <- prometheus.MustNewConstMetric(c.obsDelay, prometheus.GaugeValue, float64(s.Delay), tag)
			}
		}
	}

	ms := vars.MemStats
	ch <- prometheus.MustNewConstMetric(c.memAlloc, prometheus.GaugeValue, float64(ms.Alloc))
	ch <- prometheus.MustNewConstMetric(c.memSys, prometheus.GaugeValue, float64(ms.Sys))
	ch <- prometheus.MustNewConstMetric(c.memHeap, prometheus.GaugeValue, float64(ms.HeapInuse))
	ch <- prometheus.MustNewConstMetric(c.memNumGC, prometheus.CounterValue, float64(ms.NumGC))

	return nil
}

// observatoryTag prefers the map key, falling back to the status's own tag.
func observatoryTag(mapKey string, status client.ObservatoryStatus) string {
	if mapKey != "" {
		return mapKey
	}
	return status.OutboundTag
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
