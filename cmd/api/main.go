package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"arahin-mini/internal/ai"
	"arahin-mini/internal/platform"
	"arahin-mini/internal/quiz"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// config is loaded from environment variables (see .env.example).
type config struct {
	HTTPAddr       string
	DatabaseURL    string
	AIStubDelayMS  int
	AIStubFailRate float64
}

func loadConfig() config {
	return config{
		HTTPAddr:       envString("HTTP_ADDR", ":8081"),
		DatabaseURL:    envString("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/arahin_mini?sslmode=disable"),
		AIStubDelayMS:  envInt("AI_STUB_DELAY_MS", 400),
		AIStubFailRate: envFloat("AI_STUB_FAIL_RATE", 0.0),
	}
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	cfg := loadConfig()

	// -------------------------------------------------------------------
	// Dependency wiring (manual, on purpose — no DI framework).
	// Each line mirrors the architecture diagram:
	//
	//   db → repository → service → handler, and service → aiClient
	// -------------------------------------------------------------------
	ctx := context.Background()

	slog.Info("connecting to postgres")
	pool, err := platform.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("connect to postgres failed", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	repo := quiz.NewRepository(pool)
	aiClient := ai.NewStubClient(ai.StubOptions{
		Delay:    time.Duration(cfg.AIStubDelayMS) * time.Millisecond,
		FailRate: cfg.AIStubFailRate,
	})
	service := quiz.NewService(repo, aiClient)
	handler := quiz.NewHandler(service)

	// -------------------------------------------------------------------
	// Router
	// -------------------------------------------------------------------
	router := chi.NewRouter()
	router.Use(middleware.RequestID)
	router.Use(middleware.RealIP)
	router.Use(middleware.Logger)
	router.Use(middleware.Recoverer)

	router.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		platform.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	router.Mount("/v1", handler.Routes())

	// -------------------------------------------------------------------
	// HTTP server with graceful shutdown
	// -------------------------------------------------------------------
	server := &http.Server{
		Addr:         cfg.HTTPAddr,
		Handler:      router,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		slog.Info("server listening", "addr", cfg.HTTPAddr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	// Handle Ctrl+C: drain in-flight requests before exiting.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown failed", "error", err)
	}
}

// --- tiny environment helpers -------------------------------------------------

func envString(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		slog.Warn("invalid int for env var, using fallback", "key", key, "value", v, "fallback", fallback)
		return fallback
	}
	return n
}

func envFloat(key string, fallback float64) float64 {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		slog.Warn("invalid float for env var, using fallback", "key", key, "value", v, "fallback", fallback)
		return fallback
	}
	return f
}
