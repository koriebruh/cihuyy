package engine

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/shopspring/decimal"
)

// ErrOrderNotFound is returned when an order ID does not exist in the book.
var ErrOrderNotFound = errors.New("order not found")

// ─── Price Level ─────────────────────────────────────────────────────────────

// PriceLevel holds the FIFO queue of resting orders at one price point.
type PriceLevel struct {
	Price  decimal.Decimal
	Orders []*Order
}

// TotalQuantity returns the sum of remaining quantity across all orders.
func (pl *PriceLevel) TotalQuantity() decimal.Decimal {
	total := decimal.Zero
	for _, o := range pl.Orders {
		total = total.Add(o.RemainingQty)
	}
	return total
}

// add appends an order to the back of the FIFO queue.
func (pl *PriceLevel) add(o *Order) {
	pl.Orders = append(pl.Orders, o)
}

// remove deletes an order by ID; returns true if found.
func (pl *PriceLevel) remove(orderID string) bool {
	for i, o := range pl.Orders {
		if o.ID == orderID {
			pl.Orders = append(pl.Orders[:i], pl.Orders[i+1:]...)
			return true
		}
	}
	return false
}

// ─── Order Book Side ─────────────────────────────────────────────────────────

// orderBookSide manages one side (bids or asks) of the order book.
//
// Bids are kept sorted descending (highest price first).
// Asks are kept sorted ascending  (lowest  price first).
type orderBookSide struct {
	isBuy  bool
	levels map[string]*PriceLevel // keyed by price.String()
	prices []decimal.Decimal      // sorted price index
}

func newOrderBookSide(isBuy bool) *orderBookSide {
	return &orderBookSide{
		isBuy:  isBuy,
		levels: make(map[string]*PriceLevel),
	}
}

func (s *orderBookSide) addOrder(o *Order) {
	key := o.Price.String()
	level, ok := s.levels[key]
	if !ok {
		level = &PriceLevel{Price: o.Price}
		s.levels[key] = level
		s.insertPrice(o.Price)
	}
	level.add(o)
}

func (s *orderBookSide) removeOrder(o *Order) {
	key := o.Price.String()
	level, ok := s.levels[key]
	if !ok {
		return
	}
	level.remove(o.ID)
	if len(level.Orders) == 0 {
		delete(s.levels, key)
		s.removePrice(o.Price)
	}
}

// insertPrice inserts a price into the sorted index.
//
// For bids  (descending): find first position where prices[i] < newPrice.
// For asks  (ascending):  find first position where prices[i] > newPrice.
func (s *orderBookSide) insertPrice(price decimal.Decimal) {
	idx := sort.Search(len(s.prices), func(i int) bool {
		if s.isBuy {
			return s.prices[i].LessThan(price)
		}
		return s.prices[i].GreaterThan(price)
	})
	s.prices = append(s.prices, decimal.Zero)
	copy(s.prices[idx+1:], s.prices[idx:])
	s.prices[idx] = price
}

func (s *orderBookSide) removePrice(price decimal.Decimal) {
	for i, p := range s.prices {
		if p.Equal(price) {
			s.prices = append(s.prices[:i], s.prices[i+1:]...)
			return
		}
	}
}

func (s *orderBookSide) bestLevel() *PriceLevel {
	if len(s.prices) == 0 {
		return nil
	}
	return s.levels[s.prices[0].String()]
}

// ─── Order Book ──────────────────────────────────────────────────────────────

// OrderBook is the central data structure for a single market.
// All public methods are protected by a mutex.
type OrderBook struct {
	mu       sync.RWMutex
	MarketID string
	bids     *orderBookSide
	asks     *orderBookSide
	orders   map[string]*Order // active orders by ID for O(1) lookup
}

// NewOrderBook creates an empty order book for the given market.
func NewOrderBook(marketID string) *OrderBook {
	return &OrderBook{
		MarketID: marketID,
		bids:     newOrderBookSide(true),
		asks:     newOrderBookSide(false),
		orders:   make(map[string]*Order),
	}
}

// GetOrder returns a resting order by ID.
func (ob *OrderBook) GetOrder(orderID string) (*Order, bool) {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	o, ok := ob.orders[orderID]
	return o, ok
}

// Match executes price-time priority (FIFO) matching for an incoming taker order.
//
// Returns the list of trades generated and all orders whose state changed.
// The caller must NOT hold ob.mu when calling this.
func (ob *OrderBook) Match(
	taker *Order,
	makerFeeRate, takerFeeRate decimal.Decimal,
) ([]*Trade, []*Order, error) {
	ob.mu.Lock()
	defer ob.mu.Unlock()

	var trades []*Trade
	var changed []*Order

	// Determine which side the taker hits.
	var oppSide *orderBookSide
	if taker.IsBuy() {
		oppSide = ob.asks
	} else {
		oppSide = ob.bids
	}

	// ── FOK pre-check ──────────────────────────────────────────────────────
	// A FOK order must be fully fillable right now or it is cancelled outright.
	if taker.TimeInForce == TIFFOK && !ob.canFill(taker, oppSide) {
		taker.Status = StatusCancelled
		taker.UpdatedAt = time.Now().UTC()
		return nil, []*Order{taker}, nil
	}

	// ── Matching loop ──────────────────────────────────────────────────────
	for taker.RemainingQty.IsPositive() {
		best := oppSide.bestLevel()
		if best == nil {
			break
		}

		// Price check — limit orders must match at a favourable price.
		if taker.IsLimit() {
			if taker.IsBuy() && taker.Price.LessThan(best.Price) {
				break // taker's bid is below the best ask
			}
			if taker.IsSell() && taker.Price.GreaterThan(best.Price) {
				break // taker's ask is above the best bid
			}
		}

		// Work through the FIFO queue at this price level.
		for len(best.Orders) > 0 && taker.RemainingQty.IsPositive() {
			maker := best.Orders[0]

			fillQty := decimal.Min(taker.RemainingQty, maker.RemainingQty)
			fillPrice := maker.Price // maker's price prevails

			makerFee := fillQty.Mul(fillPrice).Mul(makerFeeRate)
			takerFee := fillQty.Mul(fillPrice).Mul(takerFeeRate)

			trade := &Trade{
				ID:           generateID(),
				MarketID:     ob.MarketID,
				MakerOrderID: maker.ID,
				TakerOrderID: taker.ID,
				MakerUserID:  maker.UserID,
				TakerUserID:  taker.UserID,
				Side:         taker.Side,
				Price:        fillPrice,
				Quantity:     fillQty,
				MakerFee:     makerFee,
				TakerFee:     takerFee,
				CreatedAt:    time.Now().UTC(),
			}
			trades = append(trades, trade)

			taker.Fill(fillQty, fillPrice)
			maker.Fill(fillQty, fillPrice)

			if maker.IsFilled() {
				best.Orders = best.Orders[1:]
				delete(ob.orders, maker.ID)
			}
			changed = append(changed, maker)
		}

		// Remove the price level if it is now empty.
		if len(best.Orders) == 0 {
			delete(oppSide.levels, best.Price.String())
			oppSide.prices = oppSide.prices[1:]
		}
	}

	// ── Post-match handling ────────────────────────────────────────────────
	if taker.IsActive() {
		switch {
		case taker.IsMarket(), taker.TimeInForce == TIFIOC:
			// Market orders and IOC orders cancel any unfilled remainder.
			taker.Status = StatusCancelled
			taker.UpdatedAt = time.Now().UTC()

		case taker.IsLimit() && taker.TimeInForce == TIFGTC:
			// GTC limit orders rest in the book.
			if taker.IsBuy() {
				ob.bids.addOrder(taker)
			} else {
				ob.asks.addOrder(taker)
			}
			ob.orders[taker.ID] = taker
		}
	}

	changed = append(changed, taker)
	return trades, changed, nil
}

// CancelOrder removes a resting order from the book and marks it cancelled.
func (ob *OrderBook) CancelOrder(orderID string) (*Order, error) {
	ob.mu.Lock()
	defer ob.mu.Unlock()

	order, ok := ob.orders[orderID]
	if !ok {
		return nil, ErrOrderNotFound
	}

	if order.IsBuy() {
		ob.bids.removeOrder(order)
	} else {
		ob.asks.removeOrder(order)
	}
	delete(ob.orders, orderID)

	order.Status = StatusCancelled
	order.UpdatedAt = time.Now().UTC()
	return order, nil
}

// Snapshot returns a read-only depth snapshot of the order book.
// A depth of 0 returns all levels.
func (ob *OrderBook) Snapshot(depth int) *OrderBookSnapshot {
	ob.mu.RLock()
	defer ob.mu.RUnlock()

	snap := &OrderBookSnapshot{
		MarketID:  ob.MarketID,
		Timestamp: time.Now().UTC(),
	}

	addLevels := func(side *orderBookSide) []PriceLevelSnapshot {
		var out []PriceLevelSnapshot
		for i, price := range side.prices {
			if depth > 0 && i >= depth {
				break
			}
			level := side.levels[price.String()]
			out = append(out, PriceLevelSnapshot{
				Price:    price,
				Quantity: level.TotalQuantity(),
				Count:    len(level.Orders),
			})
		}
		return out
	}

	snap.Bids = addLevels(ob.bids)
	snap.Asks = addLevels(ob.asks)
	return snap
}

// canFill checks whether the order can be fully filled against the given side.
// Used for FOK pre-flight.
func (ob *OrderBook) canFill(order *Order, side *orderBookSide) bool {
	remaining := order.RemainingQty
	for _, price := range side.prices {
		if order.IsLimit() {
			if order.IsBuy() && order.Price.LessThan(price) {
				break
			}
			if order.IsSell() && order.Price.GreaterThan(price) {
				break
			}
		}
		level := side.levels[price.String()]
		remaining = remaining.Sub(level.TotalQuantity())
		if !remaining.IsPositive() {
			return true
		}
	}
	return false
}

// ─── Snapshot Types ───────────────────────────────────────────────────────────

// OrderBookSnapshot is a point-in-time read of the order book depth.
type OrderBookSnapshot struct {
	MarketID  string               `json:"market_id"`
	Bids      []PriceLevelSnapshot `json:"bids"`
	Asks      []PriceLevelSnapshot `json:"asks"`
	Timestamp time.Time            `json:"timestamp"`
}

// PriceLevelSnapshot is one row of a depth snapshot.
type PriceLevelSnapshot struct {
	Price    decimal.Decimal `json:"price"`
	Quantity decimal.Decimal `json:"quantity"`
	Count    int             `json:"count"`
}
