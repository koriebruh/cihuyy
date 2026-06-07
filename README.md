# Matching Engine

A production-grade **crypto trading matching engine** written in Go.
Implements price-time priority (FIFO) matching with real-time WebSocket streaming,
Prometheus metrics, and a clean event bus for external integration.

---

## Architecture

### System Overview

```plantuml
@startuml System Overview
!theme plain
skinparam backgroundColor #FAFAFA
skinparam defaultFontName Monospaced
skinparam componentStyle rectangle
skinparam linetype ortho

skinparam component {
  BackgroundColor #E8F4FD
  BorderColor     #2980B9
  FontColor       #1A252F
}
skinparam package {
  BackgroundColor #FDFEFE
  BorderColor     #AAB7B8
}
skinparam database {
  BackgroundColor #FEF9E7
  BorderColor     #F39C12
}
skinparam note {
  BackgroundColor #F9F9F9
  BorderColor     #BDC3C7
}

actor "Trading Client" as Client

package "API Layer" {
  [REST API\n(Gin HTTP)] as REST
  [WebSocket Hub\n(Gorilla WS)] as WS
}

package "Core Matching Engine" {
  [Engine\nMarket Registry] as Engine
  package "Order Books (per market)" {
    [BTC-USDT\nOrderBook] as BTC
    [ETH-USDT\nOrderBook] as ETH
    [SOL-USDT\nOrderBook] as SOL
  }
  [Event Bus\n(buffered channel)] as Bus
}

package "Observability Stack" {
  [Prometheus\n:9091] as Prom
  [Grafana\n:3000] as Graf
}

database "Redis\n(optional)" as Redis

Client -right-> REST   : HTTP/HTTPS\nPlace / Cancel / Query
Client -right-> WS     : WebSocket\nws://host/ws/:market_id

REST   -down->  Engine : PlaceOrder()\nCancelOrder()\nGetOrderBook()
Engine -down->  BTC    : Match()
Engine -down->  ETH    : Match()
Engine -down->  SOL    : Match()
Engine -right-> Bus    : emit(Event)

Bus    -up->    WS     : BroadcastToMarket()
Bus    -right-> Prom   : metrics counters\n& histograms
Prom   -right-> Graf   : scrape /metrics

Engine ..> Redis        : optional\npersistence / pub-sub

note right of Bus
  Every state change emits
  a typed Event so external
  systems (Kafka, Redis Streams)
  can be hooked in here
end note

@enduml
```

---

### Order Matching — Sequence Flow

```plantuml
@startuml Order Matching Sequence
!theme plain
skinparam backgroundColor #FAFAFA
skinparam defaultFontName Monospaced
skinparam sequenceMessageAlign center
skinparam sequence {
  ParticipantBackgroundColor #E8F4FD
  ParticipantBorderColor     #2980B9
  LifeLineBorderColor        #AAB7B8
  ArrowColor                 #2C3E50
  NoteBackgroundColor        #FEF9E7
  NoteBorderColor            #F39C12
}

participant "Client"         as C
participant "REST API"       as API
participant "Engine"         as E
participant "OrderBook"      as OB
participant "Event Bus"      as EB
participant "WebSocket Hub"  as WS

C    ->  API : POST /api/v1/orders\n{market_id, side, type, price, qty, tif}
activate API

API  ->  E   : PlaceOrder(order)
activate E

E    ->  E   : validateOrder()\nassign ID, timestamps
E    ->  EB  : emit(order.created)

E    ->  OB  : Match(taker, makerFeeRate, takerFeeRate)
activate OB

group FOK Pre-check [TimeInForce = FOK]
  OB -> OB : canFill() — scan opposing levels\nwithout modifying any order
  OB -> OB : not fillable → Status = CANCELLED
end

loop RemainingQty > 0 AND opposing book non-empty
  OB -> OB : bestLevel() — O(1) peek sorted prices
  note right : maker price wins\n(price-time priority)
  OB -> OB : FIFO: dequeue front maker order
  OB -> OB : fillQty = min(taker.rem, maker.rem)
  OB -> OB : taker.Fill() + maker.Fill()
  OB -> OB : create Trade{id, price, qty, fees}
  OB -> OB : maker fully filled?\n→ evict from book
end

group Post-match TIF handling
  OB -> OB : GTC + Limit → rest remainder in book
  OB -> OB : IOC / Market → cancel remainder
end

OB  -->  E   : trades[], changedOrders[]
deactivate OB

loop for each trade
  E -> EB : emit(trade.created)
end

loop for each changed order
  E -> EB : emit(order.updated | order.cancelled)
end

E    ->  EB  : emit(book.updated, Snapshot(depth=20))

EB   ->  WS  : BroadcastToMarket(marketID, json)
WS  -->  C   : JSON event pushed to all\nsubscribers of this market

E   -->  API : []*Trade
deactivate E

API -->  C   : 201 { order, trades }
deactivate API

@enduml
```

---

### Order Lifecycle — State Diagram

```plantuml
@startuml Order Lifecycle
!theme plain
skinparam backgroundColor #FAFAFA
skinparam defaultFontName Monospaced
skinparam state {
  BackgroundColor     #E8F4FD
  BorderColor         #2980B9
  FontColor           #1A252F
  ArrowColor          #2C3E50
  StartColor          #2ECC71
  EndColor            #E74C3C
}

[*]              --> Open           : PlaceOrder()\nvalidation passed

Open             --> PartiallyFilled : partial match\n(RemainingQty > 0)
PartiallyFilled  --> PartiallyFilled : additional\npartial matches
Open             --> Filled          : full match on first attempt
PartiallyFilled  --> Filled          : remainder fully matched

Open             --> Cancelled : CancelOrder()\nor IOC/Market remainder\nor FOK not fillable
PartiallyFilled  --> Cancelled : CancelOrder()

[*]              --> Rejected  : PlaceOrder()\nvalidation failed

Filled           --> [*]
Cancelled        --> [*]
Rejected         --> [*]

note right of Cancelled
  IOC: fill what you can,\n cancel the rest
  FOK: fill all or cancel all
  Market: never rests in book
end note

note right of PartiallyFilled
  AvgFillPrice is updated\non every partial execution\n(volume-weighted average)
end note

@enduml
```

---

### Matching Algorithm — Flow Diagram

```plantuml
@startuml Matching Algorithm
!theme plain
skinparam backgroundColor #FAFAFA
skinparam defaultFontName Monospaced
skinparam activity {
  BackgroundColor     #E8F4FD
  BorderColor         #2980B9
  FontColor           #1A252F
  ArrowColor          #2C3E50
  StartColor          #2ECC71
  EndColor            #E74C3C
  DiamondBackgroundColor #FEF9E7
  DiamondBorderColor     #F39C12
}

start
:Receive taker Order;

if (TimeInForce = FOK?) then (yes)
  :canFill() pre-flight\n(read-only scan of opposing side);
  if (Fully fillable at acceptable price?) then (no)
    :Status ← CANCELLED;
    stop
  endif
endif

while (taker.RemainingQty > 0 AND opposing side non-empty) is (yes)
  :best ← oppSide.bestLevel() — O(1);

  if (Limit order?) then (yes)
    if (Buy: taker.Price < best.Price\nOR Sell: taker.Price > best.Price?) then (yes)
      break
    endif
  endif

  :Front maker ← best.Orders[0] (FIFO);
  :fillQty ← min(taker.RemainingQty, maker.RemainingQty);
  :fillPrice ← maker.Price  ← maker price wins;
  :makerFee = fillQty × fillPrice × makerFeeRate;
  :takerFee = fillQty × fillPrice × takerFeeRate;
  :Create Trade record;
  :taker.Fill(fillQty, fillPrice);
  :maker.Fill(fillQty, fillPrice);

  if (maker.IsFilled?) then (yes)
    :Evict maker from price level;
  endif

  if (price level empty?) then (yes)
    :Remove price level from sorted index;
  endif
endwhile

if (taker.IsActive?) then (yes)
  if (Type = Market OR TIF = IOC?) then (yes)
    :Status ← CANCELLED\n(cancel unfilled remainder);
  elseif (Type = Limit AND TIF = GTC?) then (yes)
    :Add remainder to order book\n(rest until filled or cancelled);
  endif
endif

:emit Events (trades, orders, book snapshot);
stop

@enduml
```

---

### WebSocket — Connection & Event Flow

```plantuml
@startuml WebSocket Flow
!theme plain
skinparam backgroundColor #FAFAFA
skinparam defaultFontName Monospaced
skinparam sequence {
  ParticipantBackgroundColor #E8F4FD
  ParticipantBorderColor     #2980B9
  ArrowColor                 #2C3E50
  NoteBackgroundColor        #FEF9E7
  NoteBorderColor            #F39C12
}

participant "Client"         as C
participant "Hub.ServeWS"    as SRV
participant "Hub.Run()"      as HUB
participant "Engine EventBus" as E

C   ->  SRV : GET /ws/BTC-USDT\nUpgrade: websocket
SRV ->  HUB : subscribe ← client{marketID:"BTC-USDT"}
SRV -->  C  : 101 Switching Protocols

activate HUB

loop Keepalive every ~54s
  SRV ->  C   : PING frame
  C   ->  SRV : PONG frame (resets read deadline)
end

loop Engine produces events
  E   ->  HUB : BroadcastToMarket("BTC-USDT", json)
  HUB ->  C   : {"type":"book.updated","market_id":"BTC-USDT","payload":{...}}
  HUB ->  C   : {"type":"trade.created","market_id":"BTC-USDT","payload":{...}}
  HUB ->  C   : {"type":"order.updated","market_id":"BTC-USDT","payload":{...}}
end

note right of HUB
  Slow clients are dropped
  to prevent back-pressure
  on the hot matching path
end note

C   ->  HUB : Close frame / disconnect
HUB ->  HUB : unsubscribe client\nclose send channel
deactivate HUB

@enduml
```

---

### Package Dependency Graph

```plantuml
@startuml Package Dependencies
!theme plain
skinparam backgroundColor #FAFAFA
skinparam defaultFontName Monospaced
skinparam package {
  BackgroundColor #E8F4FD
  BorderColor     #2980B9
}
skinparam arrow {
  Color           #2C3E50
}

package "cmd/server" as CMD {
  [main.go]
}

package "internal/api" as API {
  [handler.go]
  [router.go]
  [websocket.go]
}

package "internal/engine" as ENG {
  [engine.go]
  [orderbook.go]
  [order.go]
  [trade.go]
}

package "internal/config" as CFG {
  [config.go]
}

package "internal/metrics" as MET {
  [metrics.go]
}

package "pkg/logger" as LOG {
  [logger.go]
}

CMD -down-> API
CMD -down-> ENG
CMD -down-> CFG
CMD -down-> MET
CMD -down-> LOG

API -down-> ENG
API -down-> MET

@enduml
```

---

## Features

| Feature | Detail |
|---|---|
| **Matching algorithm** | Price-Time Priority (FIFO) — industry standard |
| **Order types** | `limit`, `market`, `stop_limit` |
| **Time-in-Force** | `gtc` (Good Till Cancel), `ioc` (Immediate or Cancel), `fok` (Fill or Kill) |
| **Fee model** | Asymmetric maker/taker fees per market, calculated per trade |
| **Avg fill price** | Volume-weighted average maintained on every partial fill |
| **Real-time feed** | WebSocket per market — book updates, trades, order changes |
| **REST API** | Full order lifecycle + market data + health probes |
| **Observability** | Prometheus metrics + Grafana dashboard-ready |
| **Integration** | Typed event bus — hook into Kafka / Redis Streams with one function |
| **Graceful shutdown** | Drains in-flight requests before exit |
| **Containerised** | Multi-stage Docker build + full Docker Compose stack |
| **Precision math** | `shopspring/decimal` — no floating-point errors on prices/quantities |

---

## Quick Start

### Docker Compose (recommended)

```bash
# Clone and start the full stack
# (Engine + Redis + Prometheus + Grafana)
git clone <repo>
cd matching-engine
docker-compose up -d

# Check engine health
curl http://localhost:8080/health

# View logs
docker-compose logs -f matching-engine
```

| Service | URL |
|---|---|
| REST API | `http://localhost:8080` |
| Prometheus | `http://localhost:9091` |
| Grafana | `http://localhost:3000` (admin / admin) |
| Redis | `localhost:6379` |

### Local Development

```bash
# Copy env template and adjust values
cp .env.example .env

# Download dependencies
go mod download

# Run the server
make run

# Or directly:
go run ./cmd/server/
```

---

## Project Structure

```
matching-engine/
├── cmd/
│   └── server/
│       └── main.go            # Entry point — wires all components
│
├── internal/
│   ├── engine/
│   │   ├── engine.go          # Market registry, PlaceOrder, CancelOrder, event bus
│   │   ├── orderbook.go       # Price-time priority book (sorted levels + FIFO queues)
│   │   ├── order.go           # Order model, side/type/TIF/status types, Fill()
│   │   └── trade.go           # Trade model (maker + taker IDs, price, qty, fees)
│   │
│   ├── api/
│   │   ├── handler.go         # HTTP handlers (orders, markets, health)
│   │   ├── router.go          # Gin router + middleware (CORS, request-ID, logger)
│   │   └── websocket.go       # WebSocket hub — per-market broadcast, ping/pong
│   │
│   ├── config/
│   │   └── config.go          # Viper config loader (ME_* env vars)
│   │
│   └── metrics/
│       └── metrics.go         # Prometheus counters, histograms, gauges
│
├── pkg/
│   └── logger/
│       └── logger.go          # zap logger factory (JSON prod / colour dev)
│
├── Dockerfile                 # 2-stage build: golang:1.21-alpine → alpine:3.19
├── docker-compose.yml         # Engine + Redis + Prometheus + Grafana
├── prometheus.yml             # Scrape config
├── Makefile                   # Build / run / test / lint targets
├── .env.example               # All ME_* environment variables with defaults
├── go.mod
└── go.sum
```

---

## REST API Reference

All responses use the envelope:

```json
{ "data": <payload>,  "error": null }
{ "data": null,       "error": "message" }
```

### Health

#### `GET /health`
Liveness probe — always returns 200 while the process is alive.

```bash
curl http://localhost:8080/health
```
```json
{ "data": { "status": "ok", "timestamp": "2024-01-01T00:00:00Z" }, "error": null }
```

#### `GET /ready`
Readiness probe — returns 503 until at least one market is registered.

---

### Markets

#### `GET /api/v1/markets` — List all markets

```bash
curl http://localhost:8080/api/v1/markets
```
```json
{
  "data": [
    {
      "id": "BTC-USDT",
      "base_currency": "BTC",
      "quote_currency": "USDT",
      "maker_fee_rate": "0.001",
      "taker_fee_rate": "0.002",
      "min_order_size": "0.0001",
      "max_order_size": "100",
      "is_active": true
    }
  ],
  "error": null
}
```

#### `GET /api/v1/markets/:market_id` — Get market

```bash
curl http://localhost:8080/api/v1/markets/BTC-USDT
```

#### `GET /api/v1/markets/:market_id/orderbook?depth=20` — Order book snapshot

```bash
curl "http://localhost:8080/api/v1/markets/BTC-USDT/orderbook?depth=5"
```
```json
{
  "data": {
    "market_id": "BTC-USDT",
    "bids": [
      { "price": "50000.00", "quantity": "1.500", "count": 3 },
      { "price": "49990.00", "quantity": "0.800", "count": 1 }
    ],
    "asks": [
      { "price": "50001.00", "quantity": "0.500", "count": 1 },
      { "price": "50005.00", "quantity": "2.000", "count": 4 }
    ],
    "timestamp": "2024-01-01T00:00:00Z"
  },
  "error": null
}
```

#### `GET /api/v1/markets/:market_id/ticker` — Best bid/ask + spread

```bash
curl http://localhost:8080/api/v1/markets/BTC-USDT/ticker
```

#### `GET /api/v1/markets/:market_id/trades` — Recent trades (placeholder)

---

### Orders

#### `POST /api/v1/orders` — Place an order

**Limit GTC (rests in book)**
```bash
curl -X POST http://localhost:8080/api/v1/orders \
  -H "Content-Type: application/json" \
  -d '{
    "market_id":    "BTC-USDT",
    "user_id":      "user-abc",
    "side":         "buy",
    "type":         "limit",
    "time_in_force":"gtc",
    "price":        "50000.00",
    "quantity":     "0.5"
  }'
```

**Market order (fill immediately)**
```bash
curl -X POST http://localhost:8080/api/v1/orders \
  -H "Content-Type: application/json" \
  -d '{
    "market_id": "BTC-USDT",
    "user_id":   "user-abc",
    "side":      "sell",
    "type":      "market",
    "quantity":  "0.1"
  }'
```

**Fill or Kill**
```bash
curl -X POST http://localhost:8080/api/v1/orders \
  -H "Content-Type: application/json" \
  -d '{
    "market_id":    "ETH-USDT",
    "user_id":      "user-xyz",
    "side":         "buy",
    "type":         "limit",
    "time_in_force":"fok",
    "price":        "3000.00",
    "quantity":     "10"
  }'
```

Response:
```json
{
  "data": {
    "order": {
      "id":            "550e8400-e29b-41d4-a716-446655440000",
      "market_id":     "BTC-USDT",
      "user_id":       "user-abc",
      "side":          "buy",
      "type":          "limit",
      "time_in_force": "gtc",
      "price":         "50000",
      "quantity":      "0.5",
      "filled_qty":    "0.5",
      "remaining_qty": "0",
      "avg_fill_price":"50000",
      "status":        "filled",
      "created_at":    "2024-01-01T00:00:00Z",
      "updated_at":    "2024-01-01T00:00:00Z"
    },
    "trades": [
      {
        "id":            "trade-uuid",
        "market_id":     "BTC-USDT",
        "maker_order_id":"maker-uuid",
        "taker_order_id":"550e8400-...",
        "side":          "buy",
        "price":         "50000",
        "quantity":      "0.5",
        "maker_fee":     "0.025",
        "taker_fee":     "0.05",
        "created_at":    "2024-01-01T00:00:00Z"
      }
    ]
  },
  "error": null
}
```

#### `GET /api/v1/orders/:market_id/:order_id` — Get a resting order

```bash
curl http://localhost:8080/api/v1/orders/BTC-USDT/550e8400-e29b-41d4-a716-446655440000
```

#### `DELETE /api/v1/orders/:market_id/:order_id` — Cancel an order

```bash
curl -X DELETE http://localhost:8080/api/v1/orders/BTC-USDT/550e8400-e29b-41d4-a716-446655440000
```

---

### Metrics

#### `GET /metrics` — Prometheus scrape endpoint

Key metrics exposed:

| Metric | Type | Labels |
|---|---|---|
| `matching_engine_orders_total` | Counter | market, side, type, status |
| `matching_engine_trades_total` | Counter | market, side |
| `matching_engine_trade_volume_quote_total` | Counter | market |
| `matching_engine_matching_latency_seconds` | Histogram | market |
| `matching_engine_orderbook_depth_levels` | Gauge | market, side |
| `matching_engine_orders_in_book` | Gauge | market |
| `matching_engine_websocket_connections_active` | Gauge | — |

---

## WebSocket Documentation

### Connect

```
ws://localhost:8080/ws/:market_id
```

Example: `ws://localhost:8080/ws/BTC-USDT`

Every event emitted by the engine for this market is pushed in real-time as a JSON object.

### Event Message Format

```json
{
  "type":      "<event_type>",
  "market_id": "BTC-USDT",
  "payload":   { ... },
  "timestamp": "2024-01-01T00:00:00Z"
}
```

| `type` | Trigger | Payload type |
|---|---|---|
| `order.created` | Every new order submitted | `Order` |
| `order.updated` | After any fill | `Order` |
| `order.cancelled` | Cancel or IOC/FOK/Market remainder | `Order` |
| `trade.created` | Every matched trade | `Trade` |
| `book.updated` | After every order event | `OrderBookSnapshot` |

### book.updated payload example

```json
{
  "type":      "book.updated",
  "market_id": "BTC-USDT",
  "payload": {
    "market_id": "BTC-USDT",
    "bids": [{ "price": "50000", "quantity": "1.5", "count": 2 }],
    "asks": [{ "price": "50001", "quantity": "0.5", "count": 1 }],
    "timestamp": "2024-01-01T00:00:00Z"
  }
}
```

### JavaScript Example

```javascript
const ws = new WebSocket("ws://localhost:8080/ws/BTC-USDT");

ws.onopen = () => console.log("Connected to BTC-USDT feed");

ws.onmessage = ({ data }) => {
  const event = JSON.parse(data);

  switch (event.type) {
    case "book.updated":
      renderOrderBook(event.payload.bids, event.payload.asks);
      break;
    case "trade.created":
      appendTradeRow(event.payload);
      break;
    case "order.updated":
    case "order.cancelled":
      updateOrderStatus(event.payload);
      break;
  }
};

ws.onclose = () => setTimeout(() => reconnect(), 1000); // auto-reconnect
```

---

## Integration Guide

The engine exposes a **typed event bus** (`engine.Events() <-chan *Event`).
The fan-out goroutine in `cmd/server/main.go` (`fanOutEvents`) is the single
integration point. To connect to any external message broker:

1. Open `cmd/server/main.go`
2. Find the `fanOutEvents` function
3. Add your publisher alongside the existing WebSocket broadcast

### Event Types

```go
const (
    EventOrderCreated   EventType = "order.created"
    EventOrderUpdated   EventType = "order.updated"
    EventOrderCancelled EventType = "order.cancelled"
    EventTradeCreated   EventType = "trade.created"
    EventBookUpdated    EventType = "book.updated"
)
```

Each `Event.Payload` is a fully typed Go value:
- `*engine.Order` for order events
- `*engine.Trade` for trade events
- `*engine.OrderBookSnapshot` for book events

### Kafka Integration (example)

```go
// In fanOutEvents, after the WebSocket broadcast:
case engine.EventTradeCreated:
    if trade, ok := evt.Payload.(*engine.Trade); ok {
        msg, _ := json.Marshal(trade)
        kafkaProducer.Produce(&kafka.Message{
            TopicPartition: kafka.TopicPartition{
                Topic:     &evt.MarketID,
                Partition: kafka.PartitionAny,
            },
            Key:   []byte(trade.ID),
            Value: msg,
        }, nil)
    }
```

### Redis Pub/Sub Integration (example)

```go
case engine.EventBookUpdated:
    rdb.Publish(ctx, "book:"+evt.MarketID, data)
```

---

## Configuration Reference

All configuration is via environment variables prefixed with `ME_`.

| Variable | Default | Description |
|---|---|---|
| `ME_SERVER_PORT` | `8080` | HTTP server port |
| `ME_SERVER_READTIMEOUT` | `30s` | HTTP read timeout |
| `ME_SERVER_WRITETIMEOUT` | `30s` | HTTP write timeout |
| `ME_SERVER_SHUTDOWNTIMEOUT` | `10s` | Graceful shutdown deadline |
| `ME_ENGINE_EVENTBUFFERSIZE` | `10000` | Event channel buffer capacity |
| `ME_REDIS_ADDR` | `localhost:6379` | Redis address |
| `ME_REDIS_PASSWORD` | `` | Redis password |
| `ME_REDIS_DB` | `0` | Redis database index |
| `ME_REDIS_ENABLED` | `false` | Enable Redis integration |
| `ME_LOG_LEVEL` | `info` | Log level: `debug`, `info`, `warn`, `error` |
| `ME_LOG_JSON` | `false` | JSON log output (enable in production) |

Default markets loaded at startup: **BTC-USDT**, **ETH-USDT**, **SOL-USDT** — all with 0.1% maker / 0.2% taker fees.

---

## Development Setup

### Prerequisites

- Go 1.21+
- Docker + Docker Compose (for the full stack)
- `golangci-lint` (for linting)

### Make Targets

```bash
make build        # Compile to ./server
make run          # go run ./cmd/server/
make test         # go test ./...
make test-cover   # run tests with coverage report
make lint         # golangci-lint run
make tidy         # go mod tidy
make docker-build # docker build -t matching-engine .
make docker-up    # docker-compose up -d
make docker-down  # docker-compose down
make clean        # remove build artifacts
make help         # list all targets
```

### Running Tests

```bash
go test ./...
go test -race ./...                          # with race detector
go test -v ./internal/engine/...            # engine tests only
go test -run TestOrderBook_Match ./...      # specific test
```

---

## Performance Notes

- **Mutex scope**: each `OrderBook` has its own `sync.RWMutex`, so different markets match concurrently without contention.
- **Price level lookup**: `O(1)` via `map[string]*PriceLevel` keyed by `price.String()`.
- **Price ordering**: sorted `[]decimal.Decimal` slice maintained via binary search insert — `O(log n)` inserts, `O(1)` best-price reads.
- **Event bus**: non-blocking `select` — a slow consumer never stalls the matching hot path; events are dropped with a warning log.
- **WebSocket batching**: queued messages are batched into a single WebSocket frame per write-pump cycle to reduce syscall overhead.

---

## License

MIT
