package engine

import (
	"time"

	"github.com/shopspring/decimal"
)

// Trade represents a completed match between a maker and a taker order.
type Trade struct {
	ID           string          `json:"id"`
	MarketID     string          `json:"market_id"`
	MakerOrderID string          `json:"maker_order_id"`
	TakerOrderID string          `json:"taker_order_id"`
	MakerUserID  string          `json:"maker_user_id"`
	TakerUserID  string          `json:"taker_user_id"`
	Side         OrderSide       `json:"side"` // taker's side
	Price        decimal.Decimal `json:"price"`
	Quantity     decimal.Decimal `json:"quantity"`
	MakerFee     decimal.Decimal `json:"maker_fee"`
	TakerFee     decimal.Decimal `json:"taker_fee"`
	CreatedAt    time.Time       `json:"created_at"`
}
