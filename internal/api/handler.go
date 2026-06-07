package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	"matching-engine/internal/engine"
	"matching-engine/internal/metrics"
)

// Handler holds dependencies for all HTTP route handlers.
type Handler struct {
	engine  *engine.Engine
	hub     *Hub
	metrics *metrics.Metrics
	logger  *zap.Logger
}

// NewHandler creates a handler wired to the provided engine, hub, and metrics.
func NewHandler(eng *engine.Engine, hub *Hub, met *metrics.Metrics, logger *zap.Logger) *Handler {
	return &Handler{
		engine:  eng,
		hub:     hub,
		metrics: met,
		logger:  logger,
	}
}

// ─── Request / Response types ─────────────────────────────────────────────────

// placeOrderRequest is the JSON body accepted by POST /api/v1/orders.
type placeOrderRequest struct {
	MarketID    string `json:"market_id"    binding:"required"`
	UserID      string `json:"user_id"      binding:"required"`
	Side        string `json:"side"         binding:"required"`
	Type        string `json:"type"         binding:"required"`
	TimeInForce string `json:"time_in_force"`
	Price       string `json:"price"`
	StopPrice   string `json:"stop_price"`
	Quantity    string `json:"quantity"     binding:"required"`
}

// successResponse wraps a payload in a standard envelope.
func successResponse(data interface{}) gin.H {
	return gin.H{"data": data, "error": nil}
}

// errorResponse wraps an error message in a standard envelope.
func errorResponse(msg string) gin.H {
	return gin.H{"data": nil, "error": msg}
}

// ─── Health ───────────────────────────────────────────────────────────────────

// HealthCheck returns 200 as long as the process is running.
// GET /health
func (h *Handler) HealthCheck(c *gin.Context) {
	c.JSON(http.StatusOK, successResponse(gin.H{
		"status":    "ok",
		"timestamp": time.Now().UTC(),
	}))
}

// ReadinessCheck verifies that the engine has at least one active market.
// GET /ready
func (h *Handler) ReadinessCheck(c *gin.Context) {
	markets := h.engine.GetMarkets()
	if len(markets) == 0 {
		c.JSON(http.StatusServiceUnavailable, errorResponse("no markets loaded"))
		return
	}
	c.JSON(http.StatusOK, successResponse(gin.H{
		"status":  "ready",
		"markets": len(markets),
	}))
}

// ─── Markets ──────────────────────────────────────────────────────────────────

// GetMarkets returns all active markets.
// GET /api/v1/markets
func (h *Handler) GetMarkets(c *gin.Context) {
	c.JSON(http.StatusOK, successResponse(h.engine.GetMarkets()))
}

// GetMarket returns a single market by ID.
// GET /api/v1/markets/:market_id
func (h *Handler) GetMarket(c *gin.Context) {
	marketID := c.Param("market_id")
	market, ok := h.engine.GetMarket(marketID)
	if !ok {
		c.JSON(http.StatusNotFound, errorResponse("market not found"))
		return
	}
	c.JSON(http.StatusOK, successResponse(market))
}

// GetOrderBook returns the depth snapshot for a market.
// GET /api/v1/markets/:market_id/orderbook?depth=20
func (h *Handler) GetOrderBook(c *gin.Context) {
	marketID := c.Param("market_id")

	depth := 20
	if d := c.Query("depth"); d != "" {
		if v, err := strconv.Atoi(d); err == nil && v > 0 {
			depth = v
		}
	}

	snap, err := h.engine.GetOrderBook(marketID, depth)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, engine.ErrMarketNotFound) {
			status = http.StatusNotFound
		}
		c.JSON(status, errorResponse(err.Error()))
		return
	}
	c.JSON(http.StatusOK, successResponse(snap))
}

// GetRecentTrades is a placeholder endpoint — wire this to a trade store.
// GET /api/v1/markets/:market_id/trades
func (h *Handler) GetRecentTrades(c *gin.Context) {
	marketID := c.Param("market_id")
	if _, ok := h.engine.GetMarket(marketID); !ok {
		c.JSON(http.StatusNotFound, errorResponse("market not found"))
		return
	}
	// TODO: persist trades to a store (Redis/PostgreSQL) and query here.
	c.JSON(http.StatusOK, successResponse([]interface{}{}))
}

// GetTicker returns basic ticker information derived from the order book.
// GET /api/v1/markets/:market_id/ticker
func (h *Handler) GetTicker(c *gin.Context) {
	marketID := c.Param("market_id")

	snap, err := h.engine.GetOrderBook(marketID, 1)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, engine.ErrMarketNotFound) {
			status = http.StatusNotFound
		}
		c.JSON(status, errorResponse(err.Error()))
		return
	}

	ticker := gin.H{
		"market_id": marketID,
		"timestamp": snap.Timestamp,
	}
	if len(snap.Bids) > 0 {
		ticker["best_bid"] = snap.Bids[0].Price
	}
	if len(snap.Asks) > 0 {
		ticker["best_ask"] = snap.Asks[0].Price
	}
	if len(snap.Bids) > 0 && len(snap.Asks) > 0 {
		spread := snap.Asks[0].Price.Sub(snap.Bids[0].Price)
		ticker["spread"] = spread
	}

	c.JSON(http.StatusOK, successResponse(ticker))
}

// ─── Orders ───────────────────────────────────────────────────────────────────

// PlaceOrder accepts a new order and submits it to the matching engine.
// POST /api/v1/orders
func (h *Handler) PlaceOrder(c *gin.Context) {
	var req placeOrderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, errorResponse(err.Error()))
		return
	}

	// Parse decimal fields.
	qty, err := decimal.NewFromString(req.Quantity)
	if err != nil || qty.IsZero() || qty.IsNegative() {
		c.JSON(http.StatusBadRequest, errorResponse("invalid quantity"))
		return
	}

	var price decimal.Decimal
	if req.Price != "" {
		price, err = decimal.NewFromString(req.Price)
		if err != nil {
			c.JSON(http.StatusBadRequest, errorResponse("invalid price"))
			return
		}
	}

	var stopPrice decimal.Decimal
	if req.StopPrice != "" {
		stopPrice, err = decimal.NewFromString(req.StopPrice)
		if err != nil {
			c.JSON(http.StatusBadRequest, errorResponse("invalid stop_price"))
			return
		}
	}

	// Default time-in-force.
	tif := engine.TimeInForce(req.TimeInForce)
	if tif == "" {
		tif = engine.TIFGTC
	}

	order := &engine.Order{
		ID:          uuid.New().String(),
		MarketID:    req.MarketID,
		UserID:      req.UserID,
		Side:        engine.OrderSide(req.Side),
		Type:        engine.OrderType(req.Type),
		TimeInForce: tif,
		Price:       price,
		StopPrice:   stopPrice,
		Quantity:    qty,
	}

	start := time.Now()
	trades, err := h.engine.PlaceOrder(order)
	latency := time.Since(start).Seconds()

	// Record metrics regardless of outcome.
	h.metrics.RecordOrder(order.MarketID, string(order.Side), string(order.Type), string(order.Status))
	h.metrics.ObserveLatency(order.MarketID, latency)

	if err != nil {
		h.logger.Warn("order rejected",
			zap.String("market", order.MarketID),
			zap.String("user", order.UserID),
			zap.Error(err),
		)
		status := http.StatusBadRequest
		if errors.Is(err, engine.ErrMarketNotFound) {
			status = http.StatusNotFound
		}
		if errors.Is(err, engine.ErrMarketClosed) {
			status = http.StatusServiceUnavailable
		}
		c.JSON(status, errorResponse(err.Error()))
		return
	}

	for _, t := range trades {
		notional, _ := t.Price.Mul(t.Quantity).Float64()
		h.metrics.RecordTrade(t.MarketID, string(t.Side), notional)
	}

	c.JSON(http.StatusCreated, gin.H{
		"data": gin.H{
			"order":  order,
			"trades": trades,
		},
		"error": nil,
	})
}

// CancelOrder cancels a resting order from the book.
// DELETE /api/v1/orders/:market_id/:order_id
func (h *Handler) CancelOrder(c *gin.Context) {
	marketID := c.Param("market_id")
	orderID := c.Param("order_id")

	order, err := h.engine.CancelOrder(marketID, orderID)
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, engine.ErrMarketNotFound):
			status = http.StatusNotFound
		case errors.Is(err, engine.ErrOrderNotFound):
			status = http.StatusNotFound
		}
		c.JSON(status, errorResponse(err.Error()))
		return
	}

	h.metrics.RecordOrder(order.MarketID, string(order.Side), string(order.Type), string(order.Status))
	c.JSON(http.StatusOK, successResponse(order))
}

// GetOrder returns a resting order from the book.
// GET /api/v1/orders/:market_id/:order_id
func (h *Handler) GetOrder(c *gin.Context) {
	marketID := c.Param("market_id")
	orderID := c.Param("order_id")

	order, err := h.engine.GetOrder(marketID, orderID)
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, engine.ErrMarketNotFound):
			status = http.StatusNotFound
		case errors.Is(err, engine.ErrOrderNotFound):
			status = http.StatusNotFound
		}
		c.JSON(status, errorResponse(err.Error()))
		return
	}
	c.JSON(http.StatusOK, successResponse(order))
}
