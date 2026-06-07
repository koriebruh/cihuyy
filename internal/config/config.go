package config

import (
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config holds every tunable parameter for the matching engine server.
type Config struct {
	Server ServerConfig
	Engine EngineConfig
	Redis  RedisConfig
	Log    LogConfig
}

// ServerConfig controls the HTTP/WebSocket listener.
type ServerConfig struct {
	Port            string        `mapstructure:"port"`
	ReadTimeout     time.Duration `mapstructure:"read_timeout"`
	WriteTimeout    time.Duration `mapstructure:"write_timeout"`
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`
}

// EngineConfig controls matching-engine internals.
type EngineConfig struct {
	EventBufferSize int            `mapstructure:"event_buffer_size"`
	DefaultMarkets  []MarketConfig `mapstructure:"default_markets"`
}

// MarketConfig is the serialisable form of a market definition.
// Fee rates and sizes are strings to avoid floating-point precision issues;
// main.go converts them to shopspring/decimal values.
type MarketConfig struct {
	ID            string `mapstructure:"id"`
	BaseCurrency  string `mapstructure:"base_currency"`
	QuoteCurrency string `mapstructure:"quote_currency"`
	MakerFeeRate  string `mapstructure:"maker_fee_rate"`
	TakerFeeRate  string `mapstructure:"taker_fee_rate"`
	MinOrderSize  string `mapstructure:"min_order_size"`
	MaxOrderSize  string `mapstructure:"max_order_size"`
}

// RedisConfig controls the optional Redis integration.
type RedisConfig struct {
	Addr     string `mapstructure:"addr"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
	Enabled  bool   `mapstructure:"enabled"`
}

// LogConfig controls the logger.
type LogConfig struct {
	Level string `mapstructure:"level"`
	JSON  bool   `mapstructure:"json"`
}

// Load reads configuration from environment variables (prefixed with "ME_"),
// falling back to sensible defaults for every field.
//
// Environment variable mapping rules:
//   - prefix:      ME_
//   - separator:   _   (dots in key names become underscores)
//
// Examples:
//
//	ME_SERVER_PORT=8080
//	ME_ENGINE_EVENTBUFFERSIZE=10000
//	ME_REDIS_ADDR=localhost:6379
func Load() (*Config, error) {
	v := viper.New()

	// ── Defaults ────────────────────────────────────────────────────────────
	v.SetDefault("server.port", "8080")
	v.SetDefault("server.read_timeout", "30s")
	v.SetDefault("server.write_timeout", "30s")
	v.SetDefault("server.shutdown_timeout", "10s")

	v.SetDefault("engine.event_buffer_size", 10000)
	v.SetDefault("engine.default_markets", []map[string]interface{}{
		{
			"id":             "BTC-USDT",
			"base_currency":  "BTC",
			"quote_currency": "USDT",
			"maker_fee_rate": "0.001",
			"taker_fee_rate": "0.002",
			"min_order_size": "0.0001",
			"max_order_size": "100",
		},
		{
			"id":             "ETH-USDT",
			"base_currency":  "ETH",
			"quote_currency": "USDT",
			"maker_fee_rate": "0.001",
			"taker_fee_rate": "0.002",
			"min_order_size": "0.001",
			"max_order_size": "1000",
		},
		{
			"id":             "SOL-USDT",
			"base_currency":  "SOL",
			"quote_currency": "USDT",
			"maker_fee_rate": "0.001",
			"taker_fee_rate": "0.002",
			"min_order_size": "0.01",
			"max_order_size": "10000",
		},
	})

	v.SetDefault("redis.addr", "localhost:6379")
	v.SetDefault("redis.password", "")
	v.SetDefault("redis.db", 0)
	v.SetDefault("redis.enabled", false)

	v.SetDefault("log.level", "info")
	v.SetDefault("log.json", false)

	// ── Environment variables ────────────────────────────────────────────────
	v.SetEnvPrefix("ME")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// ── Unmarshal ─────────────────────────────────────────────────────────────
	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
