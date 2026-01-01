package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"stockmarket/internal/ai"
	"stockmarket/internal/config"
	"stockmarket/internal/db"
	"stockmarket/internal/market"
	"stockmarket/internal/models"
	"stockmarket/internal/notify"
)

// Server holds the API server dependencies
type Server struct {
	db            *db.DB
	config        *config.Config
	notifyService *notify.Service
	clients       map[*websocket.Conn]bool
	clientsMu     sync.RWMutex
	upgrader      websocket.Upgrader
}

// NewServer creates a new API server
func NewServer(database *db.DB, cfg *config.Config) *Server {
	// Initialize notification service with notifiers
	notifyService := notify.NewService()
	notifyService.RegisterNotifier(notify.NewEmailNotifier(map[string]string{}))
	notifyService.RegisterNotifier(notify.NewDiscordNotifier())
	notifyService.RegisterNotifier(notify.NewSMSNotifier(map[string]string{}))

	return &Server{
		db:            database,
		config:        cfg,
		notifyService: notifyService,
		clients:       make(map[*websocket.Conn]bool),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true // Allow all origins in development
			},
		},
	}
}

// SetupRoutes sets up all API routes
func (s *Server) SetupRoutes(mux *http.ServeMux) {
	// Health check
	mux.HandleFunc("/api/health", s.handleHealth)

	// Configuration
	mux.HandleFunc("/api/config", s.handleConfig)

	// Market data
	mux.HandleFunc("/api/quote/", s.handleQuote)
	mux.HandleFunc("/api/historical/", s.handleHistorical)

	// Analysis
	mux.HandleFunc("/api/analyze/", s.handleAnalyze)
	mux.HandleFunc("/api/analyses", s.handleAnalyses)
	mux.HandleFunc("/api/analyses/", s.handleAnalysesForSymbol)

	// Alerts
	mux.HandleFunc("/api/alerts", s.handleAlerts)
	mux.HandleFunc("/api/alerts/", s.handleAlertDelete)

	// Notification channels
	mux.HandleFunc("/api/notification-channels", s.handleNotificationChannels)
	mux.HandleFunc("/api/notification-channels/", s.handleNotificationChannelDelete)

	// WebSocket for real-time updates
	mux.HandleFunc("/api/ws", s.handleWebSocket)

	// Risk and frequency profiles
	mux.HandleFunc("/api/profiles", s.handleProfiles)
}

// CORS middleware
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// respondJSON sends a JSON response
func respondJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// respondError sends an error response
func respondError(w http.ResponseWriter, status int, message string) {
	respondJSON(w, status, map[string]string{"error": message})
}

// handleHealth returns server health status
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status": "healthy",
		"time":   time.Now().Format(time.RFC3339),
	})
}

// handleConfig handles configuration CRUD
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg, err := s.db.GetOrCreateConfig()
		if err != nil {
			respondError(w, http.StatusInternalServerError, err.Error())
			return
		}

		// Decrypt API keys for response (masked)
		if cfg.MarketDataAPIKey != "" {
			key, _ := config.Decrypt(cfg.MarketDataAPIKey, s.config.EncryptionKey)
			if len(key) > 4 {
				cfg.MarketDataAPIKey = key[:4] + "****" + key[len(key)-4:]
			}
		}
		if cfg.AIProviderAPIKey != "" {
			key, _ := config.Decrypt(cfg.AIProviderAPIKey, s.config.EncryptionKey)
			if len(key) > 4 {
				cfg.AIProviderAPIKey = key[:4] + "****" + key[len(key)-4:]
			}
		}

		respondJSON(w, http.StatusOK, cfg)

	case http.MethodPut:
		var input struct {
			MarketDataProvider string   `json:"market_data_provider"`
			MarketDataAPIKey   string   `json:"market_data_api_key"`
			AIProvider         string   `json:"ai_provider"`
			AIProviderAPIKey   string   `json:"ai_provider_api_key"`
			AIModel            string   `json:"ai_model"`
			RiskTolerance      string   `json:"risk_tolerance"`
			TradeFrequency     string   `json:"trade_frequency"`
			TrackedSymbols     []string `json:"tracked_symbols"`
		}

		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			respondError(w, http.StatusBadRequest, "Invalid JSON")
			return
		}

		cfg, err := s.db.GetOrCreateConfig()
		if err != nil {
			respondError(w, http.StatusInternalServerError, err.Error())
			return
		}

		// Update fields
		if input.MarketDataProvider != "" {
			cfg.MarketDataProvider = input.MarketDataProvider
		}
		if input.MarketDataAPIKey != "" && !strings.Contains(input.MarketDataAPIKey, "****") {
			encrypted, _ := config.Encrypt(input.MarketDataAPIKey, s.config.EncryptionKey)
			cfg.MarketDataAPIKey = encrypted
		}
		if input.AIProvider != "" {
			cfg.AIProvider = input.AIProvider
		}
		if input.AIProviderAPIKey != "" && !strings.Contains(input.AIProviderAPIKey, "****") {
			encrypted, _ := config.Encrypt(input.AIProviderAPIKey, s.config.EncryptionKey)
			cfg.AIProviderAPIKey = encrypted
		}
		if input.AIModel != "" {
			cfg.AIModel = input.AIModel
		}
		if input.RiskTolerance != "" {
			cfg.RiskTolerance = input.RiskTolerance
		}
		if input.TradeFrequency != "" {
			cfg.TradeFrequency = input.TradeFrequency
		}
		if input.TrackedSymbols != nil {
			// Normalize symbols to uppercase
			for i := range input.TrackedSymbols {
				input.TrackedSymbols[i] = strings.ToUpper(strings.TrimSpace(input.TrackedSymbols[i]))
			}
			cfg.TrackedSymbols = input.TrackedSymbols
		}

		if err := s.db.UpdateConfig(cfg); err != nil {
			respondError(w, http.StatusInternalServerError, err.Error())
			return
		}

		respondJSON(w, http.StatusOK, map[string]string{"status": "updated"})

	default:
		respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

// handleQuote fetches a stock quote
func (s *Server) handleQuote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	symbol := strings.TrimPrefix(r.URL.Path, "/api/quote/")
	if symbol == "" {
		respondError(w, http.StatusBadRequest, "Symbol required")
		return
	}
	symbol = strings.ToUpper(symbol)

	cfg, err := s.db.GetOrCreateConfig()
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Decrypt API key
	apiKey := ""
	if cfg.MarketDataAPIKey != "" {
		apiKey, _ = config.Decrypt(cfg.MarketDataAPIKey, s.config.EncryptionKey)
	}

	provider, err := market.NewProvider(cfg.MarketDataProvider, apiKey)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	quote, err := provider.GetQuote(ctx, symbol)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	respondJSON(w, http.StatusOK, quote)
}

// handleHistorical fetches historical data
func (s *Server) handleHistorical(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	symbol := strings.TrimPrefix(r.URL.Path, "/api/historical/")
	if symbol == "" {
		respondError(w, http.StatusBadRequest, "Symbol required")
		return
	}
	symbol = strings.ToUpper(symbol)

	period := r.URL.Query().Get("period")
	if period == "" {
		period = "1m" // Default to 1 month
	}

	cfg, err := s.db.GetOrCreateConfig()
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	apiKey := ""
	if cfg.MarketDataAPIKey != "" {
		apiKey, _ = config.Decrypt(cfg.MarketDataAPIKey, s.config.EncryptionKey)
	}

	provider, err := market.NewProvider(cfg.MarketDataProvider, apiKey)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	candles, err := provider.GetHistoricalData(ctx, symbol, period)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	respondJSON(w, http.StatusOK, candles)
}

// handleAnalyze triggers AI analysis for a symbol
func (s *Server) handleAnalyze(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	symbol := strings.TrimPrefix(r.URL.Path, "/api/analyze/")
	if symbol == "" {
		respondError(w, http.StatusBadRequest, "Symbol required")
		return
	}
	symbol = strings.ToUpper(symbol)

	var input struct {
		UserContext string `json:"user_context"`
	}
	json.NewDecoder(r.Body).Decode(&input)

	cfg, err := s.db.GetOrCreateConfig()
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Get market data
	marketAPIKey := ""
	if cfg.MarketDataAPIKey != "" {
		marketAPIKey, _ = config.Decrypt(cfg.MarketDataAPIKey, s.config.EncryptionKey)
	}

	provider, err := market.NewProvider(cfg.MarketDataProvider, marketAPIKey)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Market provider error: "+err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	quote, err := provider.GetQuote(ctx, symbol)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Failed to get quote: "+err.Error())
		return
	}

	historical, err := provider.GetHistoricalData(ctx, symbol, "1m")
	if err != nil {
		respondError(w, http.StatusBadRequest, "Failed to get historical data: "+err.Error())
		return
	}

	// Get AI analyzer
	aiAPIKey := ""
	if cfg.AIProviderAPIKey != "" {
		aiAPIKey, _ = config.Decrypt(cfg.AIProviderAPIKey, s.config.EncryptionKey)
	}

	analyzer, err := ai.NewAnalyzer(cfg.AIProvider, aiAPIKey, cfg.AIModel)
	if err != nil {
		respondError(w, http.StatusBadRequest, "AI provider error: "+err.Error())
		return
	}

	// Perform analysis
	analysisReq := models.AnalysisRequest{
		Symbol:         symbol,
		CurrentPrice:   quote.Price,
		HistoricalData: historical,
		RiskProfile:    cfg.RiskTolerance,
		TradeFrequency: cfg.TradeFrequency,
		UserContext:    input.UserContext,
	}

	analysis, err := analyzer.Analyze(ctx, analysisReq)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "Analysis failed: "+err.Error())
		return
	}

	// Save analysis
	if err := s.db.SaveAnalysis(analysis); err != nil {
		log.Printf("Failed to save analysis: %v", err)
	}

	// Send notifications if action is BUY or SELL with high confidence
	if (analysis.Action == "BUY" || analysis.Action == "SELL") && analysis.Confidence >= 0.7 {
		notification := models.Notification{
			Type:    strings.ToLower(analysis.Action) + "_signal",
			Title:   fmt.Sprintf("%s Signal: %s", analysis.Action, symbol),
			Message: analysis.Reasoning,
			Symbol:  symbol,
		}
		go s.notifyService.SendToChannels(notification, cfg.NotificationChannels)
	}

	respondJSON(w, http.StatusOK, analysis)
}
