package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// NewRouter builds and returns a configured Gin engine.
// It mounts all REST routes, the WebSocket endpoint, and the /metrics handler.
func NewRouter(h *Handler, hub *Hub, metricsHandler http.Handler, logger *zap.Logger) *gin.Engine {
	r := gin.New()

	// ── Middleware ─────────────────────────────────────────────────────────
	r.Use(recoveryMiddleware(logger))
	r.Use(requestIDMiddleware())
	r.Use(corsMiddleware())
	r.Use(loggerMiddleware(logger))

	// ── Health / readiness probes ──────────────────────────────────────────
	r.GET("/health", h.HealthCheck)
	r.GET("/ready", h.ReadinessCheck)

	// ── Prometheus metrics ─────────────────────────────────────────────────
	r.GET("/metrics", gin.WrapH(metricsHandler))

	// ── WebSocket ──────────────────────────────────────────────────────────
	// Connect: ws://host/ws/BTC-USDT
	r.GET("/ws/:market_id", hub.ServeWS)

	// ── REST API v1 ────────────────────────────────────────────────────────
	v1 := r.Group("/api/v1")
	{
		// Markets
		v1.GET("/markets", h.GetMarkets)
		v1.GET("/markets/:market_id", h.GetMarket)
		v1.GET("/markets/:market_id/orderbook", h.GetOrderBook)
		v1.GET("/markets/:market_id/trades", h.GetRecentTrades)
		v1.GET("/markets/:market_id/ticker", h.GetTicker)

		// Orders
		v1.POST("/orders", h.PlaceOrder)
		v1.DELETE("/orders/:market_id/:order_id", h.CancelOrder)
		v1.GET("/orders/:market_id/:order_id", h.GetOrder)
	}

	// ── 404 fallback ───────────────────────────────────────────────────────
	r.NoRoute(func(c *gin.Context) {
		c.JSON(http.StatusNotFound, gin.H{
			"data":  nil,
			"error": "route not found",
		})
	})

	return r
}

// ─── Middleware ───────────────────────────────────────────────────────────────

// requestIDMiddleware injects a unique X-Request-ID into each request context.
func requestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Request-ID")
		if id == "" {
			id = uuid.New().String()
		}
		c.Set("request_id", id)
		c.Header("X-Request-ID", id)
		c.Next()
	}
}

// corsMiddleware allows all origins for development.
// Replace with a strict allowlist before deploying to production.
func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Origin, Content-Type, Authorization, X-Request-ID")

		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// loggerMiddleware logs every request with method, path, status, and latency.
func loggerMiddleware(logger *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		query := c.Request.URL.RawQuery

		c.Next()

		latency := time.Since(start)
		status := c.Writer.Status()

		fields := []zap.Field{
			zap.String("request_id", c.GetString("request_id")),
			zap.String("method", c.Request.Method),
			zap.String("path", path),
			zap.String("query", query),
			zap.Int("status", status),
			zap.Duration("latency", latency),
			zap.String("ip", c.ClientIP()),
		}

		if status >= 500 {
			logger.Error("request", fields...)
		} else if status >= 400 {
			logger.Warn("request", fields...)
		} else {
			logger.Info("request", fields...)
		}
	}
}

// recoveryMiddleware catches panics, logs the stack, and returns 500.
func recoveryMiddleware(logger *zap.Logger) gin.HandlerFunc {
	return gin.CustomRecoveryWithWriter(nil, func(c *gin.Context, err interface{}) {
		logger.Error("panic recovered",
			zap.Any("error", err),
			zap.String("request_id", c.GetString("request_id")),
			zap.String("path", c.Request.URL.Path),
		)
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
			"data":  nil,
			"error": "internal server error",
		})
	})
}
