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

	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-kosttiik-4a1f9845/internal/contract"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-kosttiik-4a1f9845/internal/exampleestablishment"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("example establishment stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	kitchenURL := envOrDefault("KITCHEN_API_URL", "http://localhost:8080")
	integrationKey := os.Getenv("ESTABLISHMENT_INTEGRATION_KEY")
	if integrationKey == "" {
		return errors.New("ESTABLISHMENT_INTEGRATION_KEY is required")
	}
	address := envOrDefault("ESTABLISHMENT_HTTP_ADDR", ":8081")
	pollInterval := time.Duration(envIntOrDefault("ESTABLISHMENT_POLL_INTERVAL_MS", 1000)) * time.Millisecond

	client, err := contract.NewClientWithResponses(
		kitchenURL,
		contract.WithHTTPClient(&http.Client{Timeout: 5 * time.Second}),
		contract.WithRequestEditorFn(func(_ context.Context, request *http.Request) error {
			request.Header.Set("Authorization", "Bearer "+integrationKey)
			return nil
		}),
	)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	service := exampleestablishment.New(client, logger, pollInterval)
	go service.Run(ctx)

	server := &http.Server{
		Addr:              address,
		Handler:           exampleestablishment.NewHandler(service),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("example establishment listening", "address", address)
		serverErrors <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownContext)
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func envOrDefault(name, fallback string) string {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	return value
}

func envIntOrDefault(name string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil || value < 1 {
		return fallback
	}
	return value
}
