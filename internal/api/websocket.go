package api

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"matching-engine/internal/metrics"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 512
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		return true // allow all origins; restrict in production via a whitelist
	},
}

// ─── Client ──────────────────────────────────────────────────────────────────

// client represents a single WebSocket connection subscribed to a market.
type client struct {
	hub      *Hub
	conn     *websocket.Conn
	send     chan []byte
	marketID string
}

// writePump drains the send channel and forwards messages to the WebSocket.
// It also sends periodic ping frames to keep the connection alive.
func (c *client) writePump(met *metrics.Metrics) {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
		met.ActiveConnections.Dec()
	}()

	for {
		select {
		case msg, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// Hub closed the channel.
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			w, err := c.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(msg)
			// Batch any queued messages into the same write.
			n := len(c.send)
			for i := 0; i < n; i++ {
				w.Write([]byte("\n"))
				w.Write(<-c.send)
			}
			if err := w.Close(); err != nil {
				return
			}

		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// readPump reads frames from the WebSocket and handles the connection lifecycle.
// It must run in its own goroutine.
func (c *client) readPump() {
	defer func() {
		c.hub.unsubscribe <- c
		c.conn.Close()
	}()

	c.conn.SetReadLimit(maxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, _, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseGoingAway,
				websocket.CloseAbnormalClosure,
			) {
				// Unexpected disconnect — could log here if needed.
			}
			break
		}
		// Incoming messages from clients are intentionally ignored in this
		// design; all real-time data flows server → client only.
	}
}

// ─── Hub ─────────────────────────────────────────────────────────────────────

// Hub manages all WebSocket client connections and routes outbound broadcasts.
//
// Architecture note: the hub owns a single goroutine (Run) that serialises
// all subscribe/unsubscribe/broadcast operations, so no locking is needed
// on the internal maps.
type Hub struct {
	mu          sync.RWMutex
	markets     map[string]map[*client]struct{} // marketID → set of clients
	subscribe   chan *client
	unsubscribe chan *client
	broadcast   chan marketMsg
	logger      *zap.Logger
	met         *metrics.Metrics
}

type marketMsg struct {
	marketID string
	data     []byte
}

// NewHub creates a ready-to-use Hub.
// Call hub.Run() in a goroutine before serving any WebSocket connections.
func NewHub(logger *zap.Logger, met *metrics.Metrics) *Hub {
	return &Hub{
		markets:     make(map[string]map[*client]struct{}),
		subscribe:   make(chan *client, 256),
		unsubscribe: make(chan *client, 256),
		broadcast:   make(chan marketMsg, 1024),
		logger:      logger,
		met:         met,
	}
}

// Run processes subscribe, unsubscribe, and broadcast events.
// It must run in a dedicated goroutine for the lifetime of the server.
func (h *Hub) Run() {
	for {
		select {
		case c := <-h.subscribe:
			h.mu.Lock()
			if _, ok := h.markets[c.marketID]; !ok {
				h.markets[c.marketID] = make(map[*client]struct{})
			}
			h.markets[c.marketID][c] = struct{}{}
			h.mu.Unlock()
			h.logger.Debug("ws client subscribed",
				zap.String("market", c.marketID))

		case c := <-h.unsubscribe:
			h.mu.Lock()
			if clients, ok := h.markets[c.marketID]; ok {
				if _, ok := clients[c]; ok {
					delete(clients, c)
					close(c.send)
				}
				if len(clients) == 0 {
					delete(h.markets, c.marketID)
				}
			}
			h.mu.Unlock()
			h.logger.Debug("ws client unsubscribed",
				zap.String("market", c.marketID))

		case msg := <-h.broadcast:
			h.mu.RLock()
			clients := h.markets[msg.marketID]
			h.mu.RUnlock()

			for c := range clients {
				select {
				case c.send <- msg.data:
				default:
					// Slow client: remove it to prevent backpressure.
					h.mu.Lock()
					delete(h.markets[c.marketID], c)
					h.mu.Unlock()
					close(c.send)
				}
			}
		}
	}
}

// BroadcastToMarket sends data to every client subscribed to marketID.
// It is safe to call from any goroutine.
func (h *Hub) BroadcastToMarket(marketID string, data []byte) {
	h.broadcast <- marketMsg{marketID: marketID, data: data}
}

// ServeWS upgrades an HTTP connection to WebSocket and registers the client
// with the hub for the market specified in the :market_id path parameter.
func (h *Hub) ServeWS(c *gin.Context) {
	marketID := c.Param("market_id")
	if marketID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "market_id is required"})
		return
	}

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		h.logger.Error("websocket upgrade failed", zap.Error(err))
		return
	}

	cl := &client{
		hub:      h,
		conn:     conn,
		send:     make(chan []byte, 256),
		marketID: marketID,
	}

	h.subscribe <- cl
	h.met.ActiveConnections.Inc()

	// Start the read pump in the background; write pump runs in the caller's goroutine.
	go cl.readPump()
	cl.writePump(h.met)
}
