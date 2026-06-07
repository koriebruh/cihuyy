package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds all Prometheus instruments for the matching engine.
// Every instrument is pre-registered with the default registry via promauto.
type Metrics struct {
	// OrdersTotal counts orders received, labelled by market, side, type, and final status.
	OrdersTotal *prometheus.CounterVec

	// TradesTotal counts executed trades labelled by market and taker side.
	TradesTotal *prometheus.CounterVec

	// TradeVolume accumulates total notional (price × qty) in quote currency per market.
	TradeVolume *prometheus.CounterVec

	// MatchingLatency measures end-to-end order processing time in seconds.
	MatchingLatency *prometheus.HistogramVec

	// OrderBookDepth tracks the number of price levels per side per market.
	OrderBookDepth *prometheus.GaugeVec

	// OrdersInBook tracks how many orders are currently resting in each book.
	OrdersInBook *prometheus.GaugeVec

	// ActiveConnections tracks live WebSocket connections.
	ActiveConnections prometheus.Gauge
}

// New creates and registers all Prometheus metrics under the given namespace.
// Typical namespace: "matching_engine".
func New(namespace string) *Metrics {
	return &Metrics{
		OrdersTotal: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: namespace,
				Name:      "orders_total",
				Help:      "Total number of orders received, by market, side, type, and status.",
			},
			[]string{"market", "side", "type", "status"},
		),

		TradesTotal: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: namespace,
				Name:      "trades_total",
				Help:      "Total number of trades executed, by market and taker side.",
			},
			[]string{"market", "side"},
		),

		TradeVolume: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: namespace,
				Name:      "trade_volume_quote_total",
				Help:      "Cumulative trade volume in quote currency, by market.",
			},
			[]string{"market"},
		),

		MatchingLatency: promauto.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: namespace,
				Name:      "matching_latency_seconds",
				Help:      "End-to-end order matching latency in seconds.",
				Buckets:   prometheus.DefBuckets,
			},
			[]string{"market"},
		),

		OrderBookDepth: promauto.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Name:      "orderbook_depth_levels",
				Help:      "Current number of price levels in the order book.",
			},
			[]string{"market", "side"},
		),

		OrdersInBook: promauto.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Name:      "orders_in_book",
				Help:      "Number of resting orders currently in the order book.",
			},
			[]string{"market"},
		),

		ActiveConnections: promauto.NewGauge(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Name:      "websocket_connections_active",
				Help:      "Number of currently open WebSocket connections.",
			},
		),
	}
}

// Handler returns an http.Handler that serves the /metrics endpoint.
func (m *Metrics) Handler() http.Handler {
	return promhttp.Handler()
}

// RecordOrder records an order event. Call after the engine has processed
// the order so that `status` reflects the final state.
func (m *Metrics) RecordOrder(market, side, orderType, status string) {
	m.OrdersTotal.WithLabelValues(market, side, orderType, status).Inc()
}

// RecordTrade records a trade event and accumulates notional volume.
func (m *Metrics) RecordTrade(market, side string, notional float64) {
	m.TradesTotal.WithLabelValues(market, side).Inc()
	m.TradeVolume.WithLabelValues(market).Add(notional)
}

// ObserveLatency records a matching latency sample.
func (m *Metrics) ObserveLatency(market string, seconds float64) {
	m.MatchingLatency.WithLabelValues(market).Observe(seconds)
}

// SetBookDepth updates the depth gauge for a market side.
func (m *Metrics) SetBookDepth(market, side string, levels float64) {
	m.OrderBookDepth.WithLabelValues(market, side).Set(levels)
}
