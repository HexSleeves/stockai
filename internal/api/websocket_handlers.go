package api

import (
	"context"
	"fmt"
	"log"
	"net/http"

	"stockmarket/internal/config"
	"stockmarket/internal/market"
	"stockmarket/internal/models"
)

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}

	s.clientsMu.Lock()
	s.clients[conn] = true
	s.clientsMu.Unlock()

	defer func() {
		s.clientsMu.Lock()
		delete(s.clients, conn)
		s.clientsMu.Unlock()
		conn.Close()
	}()

	// Get user config for tracked symbols
	cfg, err := s.db.GetOrCreateConfig()
	if err != nil {
		log.Printf("Failed to get config: %v", err)
		return
	}

	if len(cfg.TrackedSymbols) == 0 {
		// Send initial message
		conn.WriteJSON(map[string]string{"type": "info", "message": "No symbols tracked"})
		// Keep connection alive, wait for updates
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				break
			}
		}
		return
	}

	// Decrypt API key
	apiKey := ""
	if cfg.MarketDataAPIKey != "" {
		apiKey, _ = config.Decrypt(cfg.MarketDataAPIKey, s.config.EncryptionKey)
	}

	// Create market data provider
	provider, err := market.NewProvider(cfg.MarketDataProvider, apiKey)
	if err != nil {
		conn.WriteJSON(map[string]string{"type": "error", "message": "Provider error: " + err.Error()})
		return
	}

	// Create quote channel
	quoteCh := make(chan models.Quote, 100)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Start streaming quotes
	go func() {
		err := provider.StreamQuotes(ctx, cfg.TrackedSymbols, quoteCh)
		if err != nil && err != context.Canceled {
			log.Printf("Stream error: %v", err)
		}
	}()

	// Read goroutine to detect client disconnect
	go func() {
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				cancel()
				return
			}
		}
	}()

	// Check alerts in the background
	go s.checkPriceAlerts(ctx, quoteCh, cfg)

	// Send quotes to client
	for {
		select {
		case <-ctx.Done():
			return
		case quote := <-quoteCh:
			msg := map[string]interface{}{
				"type":  "quote",
				"quote": quote,
			}
			if err := conn.WriteJSON(msg); err != nil {
				return
			}
		}
	}
}

// checkPriceAlerts checks if any price alerts should be triggered
func (s *Server) checkPriceAlerts(ctx context.Context, quoteCh chan models.Quote, cfg *models.UserConfig) {
	for {
		select {
		case <-ctx.Done():
			return
		case quote := <-quoteCh:
			alerts, err := s.db.GetActiveAlerts()
			if err != nil {
				continue
			}

			for _, alert := range alerts {
				if alert.Symbol != quote.Symbol {
					continue
				}

				var triggered bool
				switch alert.Condition {
				case "above":
					triggered = quote.Price >= alert.Price
				case "below":
					triggered = quote.Price <= alert.Price
				}

				if triggered {
					s.db.TriggerAlert(alert.ID)
					notification := models.Notification{
						Type:    "price_alert",
						Title:   fmt.Sprintf("Price Alert: %s", alert.Symbol),
						Message: fmt.Sprintf("%s is now $%.2f (%s $%.2f)", alert.Symbol, quote.Price, alert.Condition, alert.Price),
						Symbol:  alert.Symbol,
					}
					go s.notifyService.SendToChannels(notification, cfg.NotificationChannels)
				}
			}
		}
	}
}

// BroadcastToClients sends a message to all connected WebSocket clients
func (s *Server) BroadcastToClients(msg interface{}) {
	s.clientsMu.RLock()
	defer s.clientsMu.RUnlock()

	for conn := range s.clients {
		conn.WriteJSON(msg)
	}
}

// handleConfigMarket handles market data provider settings (form data for HTMX)
