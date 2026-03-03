// CubeOS HAL (Hardware Abstraction Layer) Service
//
// @title CubeOS HAL API
// @version 1.1.0
// @description Hardware Abstraction Layer for Raspberry Pi and ARM64 SBCs
//
// @host localhost:6005
// @BasePath /hal
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	"cubeos-hal/internal/config"
	"cubeos-hal/internal/handlers"
	"cubeos-hal/internal/middleware"
)

func main() {
	// Get configuration from environment
	host := os.Getenv("HAL_HOST")
	if host == "" {
		host = "0.0.0.0"
	}
	port := os.Getenv("HAL_PORT")
	if port == "" {
		port = "6005"
	}

	// Load ACL configuration
	acl := config.LoadACLConfig()

	// Create router
	r := chi.NewRouter()

	// Apply ACL authentication middleware to all routes
	r.Use(middleware.ACLAuth(acl))

	// Create handler
	h := handlers.NewHALHandler()

	// Power monitor: restore persisted UPS configuration if available.
	// On previous runs, ConfigureUPS persists the model to /cubeos/config/ups.json.
	// On restart, we load that state and auto-start the monitor with the saved model.
	if persistedModel := handlers.LoadPersistedUPSState(); persistedModel != "" && persistedModel != "none" {
		log.Printf("Power monitor: restoring persisted UPS model=%q from disk", persistedModel)
		os.Setenv("HAL_UPS_MODEL", persistedModel)
		if msg, err := h.PowerMonitorRef().Start(); err != nil {
			log.Printf("Power monitor: failed to auto-start with persisted model: %v", err)
		} else {
			log.Printf("Power monitor: auto-started with persisted model: %s", msg)
		}
	} else {
		log.Printf("Power monitor: no persisted UPS config — waiting for API configuration (POST /power/ups/configure)")
	}

	// Health check at root (outside timeout wrapper — must always respond fast)
	r.Get("/health", h.HealthCheck)

	// Mount all HAL routes under /hal with request timeout middleware
	r.Route("/hal", func(r chi.Router) {
		handlers.SetupRoutes(r, h)
	})

	// Wrap router with request timeout, but bypass for SSE endpoints
	// (http.TimeoutHandler wraps ResponseWriter, stripping http.Flusher)
	// Default 120s: accommodates AT+CSQ (up to 60s per Iridium spec) and
	// SBDIX (up to 90s) with room for handler overhead. Overridable via env.
	requestTimeout := 120 * time.Second
	if v := os.Getenv("HAL_REQUEST_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			requestTimeout = d
		}
	}
	timeoutHandler := http.TimeoutHandler(r, requestTimeout, `{"error":"request timeout","code":504}`)
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// SSE endpoints need http.Flusher — serve directly without timeout wrapper
		if strings.HasSuffix(req.URL.Path, "/events") {
			r.ServeHTTP(w, req)
			return
		}
		timeoutHandler.ServeHTTP(w, req)
	})

	// Configure HTTP server with timeouts
	addr := fmt.Sprintf("%s:%s", host, port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 150 * time.Second, // Must be > request timeout (120s)
		IdleTimeout:  120 * time.Second,
	}

	// Start server in goroutine
	go func() {
		log.Printf("CubeOS HAL starting on %s", addr)
		log.Printf("Health: http://%s/health", addr)
		log.Printf("API: http://%s/hal/...", addr)
		log.Printf("Docs: http://%s/hal/docs", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Failed to start server: %v", err)
		}
	}()

	// Wait for interrupt signal for graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	log.Printf("Received signal %s, shutting down gracefully...", sig)

	// Stop power monitor first
	h.PowerMonitorRef().Shutdown()

	// Stop stream and other HAL resources
	h.Close()

	// Give 15 seconds for graceful shutdown (aligned with Pi watchdog timeout)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("Server forced to shutdown: %v", err)
	}

	log.Println("CubeOS HAL stopped")
}
