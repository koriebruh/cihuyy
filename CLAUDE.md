# CLAUDE.md — Matching Engine

This file provides project-specific instructions for Claude when working on this codebase.
Read this before making any changes.

---

## Project Overview

This is a **production-grade crypto trading matching engine** written in Go.
It implements price-time priority (FIFO) order matching for multiple markets,
exposes a REST API and real-time WebSocket feed, and emits typed domain events
for external integration (Kafka, Redis Streams, etc.).

**Module path**: `matching-engine`  
**Go version**: 1.21+  
**Entry point**: `cmd/server/main.go`

---

## Architecture Summary

```
cmd/server/main.go           ← wires everything, starts HTTP server, runs fan-out goroutine
internal/engine/             ← core matching logic (no HTTP, no I/O concerns)
  engine.go                  ← Engine: market registry, PlaceOrder, CancelOrder, event bus
  orderbook.go               ← OrderBook: price-time priority matching, price levels, FIFO queues
  order.go                   ← Order model and Fill() logic
  trade.go                   ← Trade model
internal/api/                ← HTTP and WebSocket layer
  handler.go                 ← Gin HTTP handlers
  router.go                  ← Gin router + middleware
  websocket.go               ← WebSocket Hub (per-market broadcast)
internal/config/config.go    ← Viper env-var config
internal/metrics/metrics.go  ← Prometheus instruments
pkg/logger/logger.go         ← zap logger factory
```

**Dependency direction** (strict, never reverse):
```
cmd/server → internal/api → internal/engine
cmd/server → internal/engine
cmd/server → internal/config
cmd/server → internal/metrics
cmd/server → pkg/logger
internal/api → internal/metrics
```

The `internal/engine` package must NEVER import from `internal/api` or any HTTP layer.

---

## Key Design Decisions

### 1. Decimal arithmetic everywhere
All prices and quantities use `github.com/shopspring/decimal`.
**Never use `float64` for financial values.** Always use `decimal.Decimal`.

```go
// CORRECT
price, _ := decimal.NewFromString("50000.00")
qty := decimal.NewFromInt(1)

// WRONG — never do this in financial code
price := 50000.0
```

### 2. Price-Time Priority (FIFO) matching
- Bids sorted **descending** (highest price first) — `prices[0]` is best bid
- Asks sorted **ascending** (lowest price first) — `prices[0]` is best ask
- Within a price level, orders are served in FIFO order (the `Orders []` slice)
- Maker's price always wins on the trade record

### 3. Event bus is non-blocking
The engine emits events with a non-blocking `select`:
```go
select {
case e.events <- event:
default:
    e.logger.Warn("event channel full, dropping event", ...)
}
```
A slow consumer **never blocks the matching hot path**. If you add a slow sink (Kafka, DB),
run it in its own goroutine and buffer appropriately.

### 4. Per-book mutex, not per-engine
`Engine` uses `sync.RWMutex` only for the `books` and `markets` maps.
Each `OrderBook` has its own `sync.RWMutex`.
This allows different markets to match **concurrently** without contention.

### 5. Order states are engine-managed
Never set `order.Status`, `order.FilledQty`, `order.RemainingQty`, or `order.AvgFillPrice`
directly from outside the engine. Use `order.Fill(qty, price)` or let the engine handle it.

### 6. WebSocket Hub uses a single event loop
`Hub.Run()` is the sole goroutine that mutates `hub.markets`.
Do not access `hub.markets` from outside `Run()` — use the channels (`subscribe`, `unsubscribe`, `broadcast`).
The `hub.mu` RWMutex guards the map for reads from `BroadcastToMarket`.

---

## Development Commands

```bash
# Build
go build ./...
go build -v ./cmd/server/

# Run
go run ./cmd/server/

# Test
go test ./...
go test -race ./...
go test -v ./internal/engine/...

# Vet / lint
go vet ./...

# Module maintenance
go mod tidy

# Docker
docker-compose up -d        # full stack (engine + redis + prometheus + grafana)
docker-compose down
docker-compose logs -f matching-engine
```

---

## Environment Variables

All config is via `ME_*` environment variables (see `.env.example`).
Key ones for development:

```bash
ME_LOG_LEVEL=debug    # verbose logging
ME_LOG_JSON=false     # human-readable console output
ME_SERVER_PORT=8080
ME_ENGINE_EVENTBUFFERSIZE=10000
```

---

## Adding a New Market

Markets are registered at startup from config. To add one programmatically:

```go
eng.AddMarket(&engine.Market{
    ID:             "XRP-USDT",
    BaseCurrency:   "XRP",
    QuoteCurrency:  "USDT",
    MakerFeeRate:   decimal.NewFromFloat(0.001),
    TakerFeeRate:   decimal.NewFromFloat(0.002),
    MinOrderSize:   decimal.NewFromFloat(1.0),
    MaxOrderSize:   decimal.NewFromFloat(1000000),
    PricePrecision: 8,
    SizePrecision:  8,
    IsActive:       true,
})
```

Or add a new entry to the `engine.default_markets` config section.

---

## Adding a New Event Sink (Integration)

The canonical integration point is `fanOutEvents()` in `cmd/server/main.go`.
Add a new `case` inside the switch:

```go
// Example: publish all trades to Kafka
case engine.EventTradeCreated:
    if trade, ok := evt.Payload.(*engine.Trade); ok {
        // serialize and publish
    }

// Example: publish all events to Redis Streams
default:
    rdb.XAdd(ctx, &redis.XAddArgs{
        Stream: "engine:" + evt.MarketID,
        Values: map[string]interface{}{"event": string(data)},
    })
```

The `data` variable is the JSON-serialized event envelope already computed
at the top of the loop. Reuse it — don't re-marshal.

---

## HTTP API Conventions

All HTTP responses use this envelope:
```json
{ "data": <payload>, "error": null }
{ "data": null,      "error": "message string" }
```

Helper functions in `handler.go`:
```go
successResponse(data interface{}) gin.H
errorResponse(msg string) gin.H
```

HTTP status code mapping for engine errors:
| Engine error | HTTP status |
|---|---|
| `ErrMarketNotFound` | 404 |
| `ErrOrderNotFound` | 404 |
| `ErrMarketClosed` | 503 |
| validation errors | 400 |

---

## Code Style Rules

1. **No naked `panic`** outside of `main()` startup. Return errors instead.
2. **No `float64`** for prices, quantities, or fees. Use `decimal.Decimal`.
3. **Errors are wrapped** with context before returning: `fmt.Errorf("doing X: %w", err)`.
4. **Log at the right level**:
   - `Debug` — per-order/trade detail (disabled in production)
   - `Info` — market lifecycle, server start/stop
   - `Warn` — recoverable issues (dropped events, slow clients)
   - `Error` — unexpected failures that need attention
5. **No global mutable state** — everything is wired through dependency injection.
6. **Comments explain WHY**, not WHAT. Don't restate the code.
7. Keep `internal/engine` free of HTTP, WebSocket, and I/O concerns.

---

## Testing Guidelines

### Engine unit tests
Test `OrderBook.Match()` directly — it is the most critical path.
Key scenarios to cover:
- Limit buy matches limit sell (exact qty, partial, over-fill)
- Market order consumes multiple price levels
- IOC: partial fill then cancel remainder
- FOK: cancel entire order if not fully fillable
- GTC: remainder rests in book after partial fill
- Price improvement: taker buy at 50100 matches ask at 50000 (maker price wins)
- Same-price FIFO: earlier order fills first

```go
func TestOrderBook_LimitBuyMatchesLimitSell(t *testing.T) {
    book := engine.NewOrderBook("BTC-USDT")
    makerFee := decimal.NewFromFloat(0.001)
    takerFee := decimal.NewFromFloat(0.002)

    // Place a resting sell
    sell := &engine.Order{
        ID: "sell-1", UserID: "u1",
        Side: engine.SideSell, Type: engine.TypeLimit,
        TimeInForce: engine.TIFGTC,
        Price: decimal.NewFromFloat(50000), Quantity: decimal.NewFromFloat(1),
        RemainingQty: decimal.NewFromFloat(1), Status: engine.StatusOpen,
    }
    book.Match(sell, makerFee, takerFee)

    // Place a matching buy
    buy := &engine.Order{
        ID: "buy-1", UserID: "u2",
        Side: engine.SideBuy, Type: engine.TypeLimit,
        TimeInForce: engine.TIFGTC,
        Price: decimal.NewFromFloat(50000), Quantity: decimal.NewFromFloat(1),
        RemainingQty: decimal.NewFromFloat(1), Status: engine.StatusOpen,
    }
    trades, _, _ := book.Match(buy, makerFee, takerFee)

    require.Len(t, trades, 1)
    assert.Equal(t, "50000", trades[0].Price.String())
    assert.Equal(t, "1", trades[0].Quantity.String())
}
```

### API integration tests
Use `httptest.NewRecorder()` and Gin's test mode:

```go
gin.SetMode(gin.TestMode)
w := httptest.NewRecorder()
c, router := gin.CreateTestContext(w)
// ...
```

---

## Files NOT to Modify Without Care

| File | Why |
|---|---|
| `internal/engine/orderbook.go` | Core matching algorithm — any bug here is a financial bug |
| `internal/engine/order.go` `Fill()` | Volume-weighted avg price calculation — must be exact |
| `go.mod` / `go.sum` | Only change via `go get` or `go mod tidy` |
| `docker-compose.yml` | Shared dev environment — coordinate changes with the team |

---

## Known Limitations / TODOs

1. **No trade persistence** — `GetRecentTrades` returns an empty slice.
   Wire a PostgreSQL or Redis store to fix this.
2. **Stop-limit orders are accepted but not triggered** — the stop-price trigger
   mechanism (monitoring last trade price) is not implemented yet.
3. **No authentication** — all endpoints are open. Add JWT or API-key middleware
   in `router.go` before production use.
4. **Single-node only** — the in-memory order book does not replicate.
   For HA, replace the book with a distributed state machine (etcd, NATS JetStream).
5. **No order-book persistence on restart** — resting orders are lost on process exit.
   Implement a snapshot/restore mechanism using Redis or a DB.

---

## Dependency Summary

| Package | Purpose |
|---|---|
| `github.com/gin-gonic/gin` | HTTP router and middleware |
| `github.com/gorilla/websocket` | WebSocket server |
| `github.com/shopspring/decimal` | Exact decimal arithmetic for prices/quantities |
| `github.com/google/uuid` | Order and trade ID generation |
| `github.com/prometheus/client_golang` | Prometheus metrics |
| `go.uber.org/zap` | Structured logging |
| `github.com/spf13/viper` | Environment-variable configuration |

Do not add new dependencies without a clear justification. The engine core
(`internal/engine`) should remain free of any network or I/O dependencies.
