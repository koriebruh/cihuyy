package engine

import (
	"time"

	"github.com/shopspring/decimal"
)

// OrderSide represents which side of the book an order is on.
type OrderSide string

// OrderType represents the execution type of an order.
type OrderType string

// TimeInForce controls order lifetime / fill behavior.
type TimeInForce string

// OrderStatus tracks the lifecycle of an order.
type OrderStatus string

const (
	SideBuy  OrderSide = "buy"
	SideSell OrderSide = "sell"
)

const (
	TypeLimit     OrderType = "limit"
	TypeMarket    OrderType = "market"
	TypeStopLimit OrderType = "stop_limit"
)

const (
	// TIFGTC Good Till Cancel — rests in book until filled or explicitly cancelled.
	TIFGTC TimeInForce = "gtc"
	// TIFIOC Immediate or Cancel — fill what you can right now, cancel the rest.
	TIFIOC TimeInForce = "ioc"
	// TIFFOK Fill or Kill — fill the entire quantity immediately or reject outright.
	TIFFOK TimeInForce = "fok"
)

const (
	StatusOpen            OrderStatus = "open"
	StatusPartiallyFilled OrderStatus = "partially_filled"
	StatusFilled          OrderStatus = "filled"
	StatusCancelled       OrderStatus = "cancelled"
	StatusRejected        OrderStatus = "rejected"
)

// Order represents a trading order submitted to the engine.
type Order struct {
	ID           string          `json:"id"`
	MarketID     string          `json:"market_id"`
	UserID       string          `json:"user_id"`
	Side         OrderSide       `json:"side"`
	Type         OrderType       `json:"type"`
	TimeInForce  TimeInForce     `json:"time_in_force"`
	Price        decimal.Decimal `json:"price"`
	StopPrice    decimal.Decimal `json:"stop_price,omitempty"`
	Quantity     decimal.Decimal `json:"quantity"`
	FilledQty    decimal.Decimal `json:"filled_qty"`
	RemainingQty decimal.Decimal `json:"remaining_qty"`
	AvgFillPrice decimal.Decimal `json:"avg_fill_price,omitempty"`
	Status       OrderStatus     `json:"status"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

func (o *Order) IsBuy() bool  { return o.Side == SideBuy }
func (o *Order) IsSell() bool { return o.Side == SideSell }

func (o *Order) IsMarket() bool    { return o.Type == TypeMarket }
func (o *Order) IsLimit() bool     { return o.Type == TypeLimit }
func (o *Order) IsStopLimit() bool { return o.Type == TypeStopLimit }

func (o *Order) IsFilled() bool    { return o.Status == StatusFilled }
func (o *Order) IsCancelled() bool { return o.Status == StatusCancelled }
func (o *Order) IsRejected() bool  { return o.Status == StatusRejected }

// IsActive returns true when the order can still be matched or cancelled.
func (o *Order) IsActive() bool {
	return o.Status == StatusOpen || o.Status == StatusPartiallyFilled
}

// Fill updates quantities and status after a partial or full execution.
// It also maintains a running weighted-average fill price.
func (o *Order) Fill(qty, fillPrice decimal.Decimal) {
	// Weighted-average fill price
	if o.FilledQty.IsZero() {
		o.AvgFillPrice = fillPrice
	} else {
		totalCost := o.AvgFillPrice.Mul(o.FilledQty).Add(fillPrice.Mul(qty))
		o.AvgFillPrice = totalCost.Div(o.FilledQty.Add(qty))
	}

	o.FilledQty = o.FilledQty.Add(qty)
	o.RemainingQty = o.Quantity.Sub(o.FilledQty)
	o.UpdatedAt = time.Now().UTC()

	if o.RemainingQty.IsZero() || o.RemainingQty.IsNegative() {
		o.RemainingQty = decimal.Zero
		o.Status = StatusFilled
	} else {
		o.Status = StatusPartiallyFilled
	}
}
