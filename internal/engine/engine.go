package engine

import (
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"
)

// Sentinel errors returned by the engine.
var (
	ErrMarketNotFound = errors.New("market not found")
	ErrMarketClosed   = errors.New("market is closed")
	ErrInvalidOrder   = errors.New("invalid order")
)

// ─── Event Bus ───────────────────────────────────────────────────────────────

// EventType is the discriminator for events emitted by the engine.
type EventType string

const (
	EventOrderCreated   EventType = "order.created"
	EventOrderUpdated   EventType = "order.updated"
	EventOrderCancelled EventType = "order.cancelled"
	EventTradeCreated   EventType = "trade.created"
	EventBookUpdated    EventType = "book.updated"
)

// Event is a domain event produced by the engine after every state change.
// Consumers receive events over the channel returned by Engine.Events().
type Event struct {
	Type      EventType   `json:"type"`
	MarketID  string      `json:"market_id"`
	Payload   interface{} `json:"payload"`
	Timestamp time.Time   `json:"timestamp"`
}

// ─── Market ───────────────────────────────────────────────────────────────────

// Market defines a tradeable pair and its operational parameters.
type Market struct {
	ID             string          `json:"id"`
	BaseCurrency   string          `json:"base_currency"`
	QuoteCurrency  string          `json:"quote_currency"`
	MakerFeeRate   decimal.Decimal `json:"maker_fee_rate"`
	TakerFeeRate   decimal.Decimal `json:"taker_fee_rate"`
	MinOrderSize   decimal.Decimal `json:"min_order_size"`
	MaxOrderSize   decimal.Decimal `json:"max_order_size"`
	PricePrecision int             `json:"price_precision"`
	SizePrecision  int             `json:"size_precision"`
	IsActive       bool            `json:"is_active"`
}

// ─── Engine ───────────────────────────────────────────────────────────────────

// Engine is the top-level matching engine. It manages multiple markets,
// dispatches orders to the correct order book, and publishes domain events.
// All public methods are safe for concurrent use.
type Engine struct {
	mu      sync.RWMutex
	books   map[string]*OrderBook
	markets map[string]*Market
	events  chan *Event
	logger  *zap.Logger
}

// NewEngine creates a ready-to-use matching engine.
// eventBufferSize controls the capacity of the internal event channel;
// 10 000 is a reasonable starting point for most deployments.
func NewEngine(logger *zap.Logger, eventBufferSize int) *Engine {
	return &Engine{
		books:   make(map[string]*OrderBook),
		markets: make(map[string]*Market),
		events:  make(chan *Event, eventBufferSize),
		logger:  logger,
	}
}

// Events returns the read-only channel through which the engine broadcasts
// domain events. Callers should consume this channel in a dedicated goroutine.
func (e *Engine) Events() <-chan *Event {
	return e.events
}

// ─── Market management ────────────────────────────────────────────────────────

// AddMarket registers a new market and its empty order book.
func (e *Engine) AddMarket(market *Market) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.markets[market.ID] = market
	e.books[market.ID] = NewOrderBook(market.ID)
	e.logger.Info("market added", zap.String("market", market.ID))
}

// GetMarket returns the market definition by ID.
func (e *Engine) GetMarket(id string) (*Market, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	m, ok := e.markets[id]
	return m, ok
}

// GetMarkets returns all registered markets.
func (e *Engine) GetMarkets() []*Market {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]*Market, 0, len(e.markets))
	for _, m := range e.markets {
		out = append(out, m)
	}
	return out
}

// ─── Order operations ─────────────────────────────────────────────────────────

// PlaceOrder validates, initialises, and matches an order against the book.
// It returns all trades that were generated and emits events for each
// state change so that downstream consumers (WebSocket hub, metrics, etc.)
// stay in sync without polling.
func (e *Engine) PlaceOrder(order *Order) ([]*Trade, error) {
	e.mu.RLock()
	market, ok := e.markets[order.MarketID]
	if !ok {
		e.mu.RUnlock()
		return nil, ErrMarketNotFound
	}
	if !market.IsActive {
		e.mu.RUnlock()
		return nil, ErrMarketClosed
	}
	book := e.books[order.MarketID]
	e.mu.RUnlock()

	// Validate before touching the book.
	if err := e.validateOrder(order, market); err != nil {
		order.Status = StatusRejected
		order.UpdatedAt = time.Now().UTC()
		e.emit(&Event{
			Type:      EventOrderCreated,
			MarketID:  order.MarketID,
			Payload:   order,
			Timestamp: time.Now().UTC(),
		})
		return nil, err
	}

	// Initialise mutable fields.
	order.RemainingQty = order.Quantity
	order.FilledQty = decimal.Zero
	order.Status = StatusOpen
	if order.ID == "" {
		order.ID = generateID()
	}
	order.CreatedAt = time.Now().UTC()
	order.UpdatedAt = order.CreatedAt

	e.emit(&Event{
		Type:      EventOrderCreated,
		MarketID:  order.MarketID,
		Payload:   order,
		Timestamp: time.Now().UTC(),
	})

	// Run matching.
	trades, changedOrders, err := book.Match(order, market.MakerFeeRate, market.TakerFeeRate)
	if err != nil {
		return nil, err
	}

	// Publish trade events.
	for _, t := range trades {
		e.emit(&Event{
			Type:      EventTradeCreated,
			MarketID:  order.MarketID,
			Payload:   t,
			Timestamp: time.Now().UTC(),
		})
	}

	// Publish order-updated events for every touched order.
	for _, o := range changedOrders {
		evtType := EventOrderUpdated
		if o.IsCancelled() {
			evtType = EventOrderCancelled
		}
		e.emit(&Event{
			Type:      evtType,
			MarketID:  order.MarketID,
			Payload:   o,
			Timestamp: time.Now().UTC(),
		})
	}

	// Publish an updated book snapshot so subscribers can refresh depth.
	e.emit(&Event{
		Type:      EventBookUpdated,
		MarketID:  order.MarketID,
		Payload:   book.Snapshot(20),
		Timestamp: time.Now().UTC(),
	})

	e.logger.Info("order processed",
		zap.String("id", order.ID),
		zap.String("market", order.MarketID),
		zap.String("status", string(order.Status)),
		zap.Int("trades", len(trades)),
	)

	return trades, nil
}

// CancelOrder cancels a resting order and emits the appropriate event.
func (e *Engine) CancelOrder(marketID, orderID string) (*Order, error) {
	e.mu.RLock()
	book, ok := e.books[marketID]
	e.mu.RUnlock()

	if !ok {
		return nil, ErrMarketNotFound
	}

	order, err := book.CancelOrder(orderID)
	if err != nil {
		return nil, err
	}

	e.emit(&Event{
		Type:      EventOrderCancelled,
		MarketID:  marketID,
		Payload:   order,
		Timestamp: time.Now().UTC(),
	})

	e.emit(&Event{
		Type:      EventBookUpdated,
		MarketID:  marketID,
		Payload:   book.Snapshot(20),
		Timestamp: time.Now().UTC(),
	})

	e.logger.Info("order cancelled",
		zap.String("id", orderID),
		zap.String("market", marketID),
	)

	return order, nil
}

// GetOrderBook returns a depth snapshot for the named market.
func (e *Engine) GetOrderBook(marketID string, depth int) (*OrderBookSnapshot, error) {
	e.mu.RLock()
	book, ok := e.books[marketID]
	e.mu.RUnlock()

	if !ok {
		return nil, ErrMarketNotFound
	}
	return book.Snapshot(depth), nil
}

// GetOrder returns a resting order by ID from the given market's book.
func (e *Engine) GetOrder(marketID, orderID string) (*Order, error) {
	e.mu.RLock()
	book, ok := e.books[marketID]
	e.mu.RUnlock()

	if !ok {
		return nil, ErrMarketNotFound
	}
	order, found := book.GetOrder(orderID)
	if !found {
		return nil, ErrOrderNotFound
	}
	return order, nil
}

// ─── Internal helpers ─────────────────────────────────────────────────────────

// validateOrder enforces business rules before an order reaches the book.
func (e *Engine) validateOrder(order *Order, market *Market) error {
	if order.Side != SideBuy && order.Side != SideSell {
		return errors.New("invalid side: must be 'buy' or 'sell'")
	}
	if order.Type != TypeLimit && order.Type != TypeMarket && order.Type != TypeStopLimit {
		return errors.New("invalid type: must be 'limit', 'market', or 'stop_limit'")
	}
	if order.Quantity.IsZero() || order.Quantity.IsNegative() {
		return errors.New("quantity must be positive")
	}
	if !market.MinOrderSize.IsZero() && order.Quantity.LessThan(market.MinOrderSize) {
		return errors.New("quantity below market minimum order size")
	}
	if !market.MaxOrderSize.IsZero() && order.Quantity.GreaterThan(market.MaxOrderSize) {
		return errors.New("quantity above market maximum order size")
	}
	if order.IsLimit() && (order.Price.IsZero() || order.Price.IsNegative()) {
		return errors.New("limit order price must be positive")
	}
	if order.IsStopLimit() && (order.StopPrice.IsZero() || order.StopPrice.IsNegative()) {
		return errors.New("stop-limit order stop price must be positive")
	}
	return nil
}

// emit publishes an event non-blockingly.
// If the channel is full the event is dropped with a warning — this prevents
// a slow consumer from stalling the matching engine.
func (e *Engine) emit(event *Event) {
	select {
	case e.events <- event:
	default:
		e.logger.Warn("event channel full, dropping event",
			zap.String("type", string(event.Type)),
			zap.String("market", event.MarketID),
		)
	}
}

// generateID returns a new random UUID string used for order and trade IDs.
func generateID() string {
	return uuid.New().String()
}
