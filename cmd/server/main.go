package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"stockmarket/internal/api"
	"stockmarket/internal/config"
	"stockmarket/internal/db"
	"stockmarket/internal/web"
)

func main() {
	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Initialize database
	database, err := db.New(cfg.DatabasePath)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer database.Close()

	// Create templ handlers (new type-safe components)
	templHandlers := web.NewTemplHandlers(database)

	// Create API server
	apiServer := api.NewServer(database, cfg)

	// Start background polling service for alerts
	pollingCtx, pollingCancel := context.WithCancel(context.Background())
	apiServer.StartPollingService(pollingCtx)

	// Setup routes
	mux := http.NewServeMux()

	// API routes
	apiServer.SetupRoutes(mux)

	// Static files
	staticFS := http.FileServer(http.Dir("internal/web/static"))
	mux.Handle("/static/", http.StripPrefix("/static/", staticFS))

	// Page routes (templ components + HTMX)
	mux.HandleFunc("/", templHandlers.Dashboard)
	mux.HandleFunc("/analysis", templHandlers.Analysis)
	mux.HandleFunc("/analysis/", templHandlers.Analysis)
	mux.HandleFunc("/recommendations", templHandlers.Recommendations)
	mux.HandleFunc("/alerts", templHandlers.Alerts)
	mux.HandleFunc("/settings", templHandlers.Settings)

	// Partial routes for HTMX
	mux.HandleFunc("/partials/watchlist", templHandlers.PartialWatchlist)
	mux.HandleFunc("/partials/recommendations", templHandlers.PartialRecommendations)
	mux.HandleFunc("/partials/recommendations-list", templHandlers.PartialRecommendationsList)
	mux.HandleFunc("/partials/analysis-history", templHandlers.PartialAnalysisHistory)
	mux.HandleFunc("/partials/analysis-detail/", templHandlers.PartialAnalysisDetail)
	mux.HandleFunc("/partials/alerts-list", templHandlers.PartialAlertsList)
	mux.HandleFunc("/partials/quick-analyze", templHandlers.PartialQuickAnalyze)
	mux.HandleFunc("/partials/watchlist-alert-buttons", templHandlers.PartialWatchlistAlertButtons)

	// Add CORS middleware
	handler := corsMiddleware(mux)

	// Create HTTP server
	httpServer := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: handler,
	}

	// Graceful shutdown
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		<-sigChan

		log.Println("Shutting down server...")
		pollingCancel() // Stop polling service
		httpServer.Close()
	}()

	log.Printf("Starting server on port %s", cfg.Port)
	log.Printf("Environment: %s", cfg.Environment)
	if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Server failed: %v", err)
	}
}

// corsMiddleware adds CORS headers to responses
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, HX-Request, HX-Target, HX-Trigger")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}
