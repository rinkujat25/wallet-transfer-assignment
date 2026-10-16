package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
	"wallet-transfer-assignment/internal/bootstrap"
	"wallet-transfer-assignment/internal/config"
	"wallet-transfer-assignment/internal/database"
	"wallet-transfer-assignment/internal/feature/wallet/route"
	"wallet-transfer-assignment/internal/middleware"

	"github.com/gofiber/fiber/v3"
	"gorm.io/gorm"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	var cfg *config.Config
	var err error
	if cfg, err = config.Load(); err != nil {
		logger.Error("configuration validation failed", "error", err)
		os.Exit(1)
	}

	logger.Info(
		"configuration loaded successfully",
		"environment", cfg.AppEnv,
		"port", cfg.AppPort,
	)

	// 3. Connect PostgreSQL
	var db *gorm.DB
	var sqlDB *sql.DB
	if db, err = database.Connect(cfg); err != nil {
		slog.Error("failed to connect database", "error", err)
		os.Exit(1)
	}
	if sqlDB, err = db.DB(); err != nil {
		slog.Error("failed to get database connection", "error", err)
		os.Exit(1)
	}
	defer sqlDB.Close()
	slog.Info("database connected successfully")

	slog.SetDefault(logger)

	app := fiber.New(fiber.Config{
		AppName: "Wallet Transfer Service",
	})

	app.Get("/health", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"status":  "ok",
			"service": "wallet-transfer-service",
		})
	})

	// Register global middleware
	middleware.Register(app)

	// Initialize dependencies
	application := bootstrap.NewApp(db)

	// Register routes
	route.RegisterRoutes(app, route.Handlers{
		Transfer: application.TransferHandler,
	})

	serverErr := make(chan error, 1)

	go func() {
		address := fmt.Sprintf(":%s", cfg.AppPort)
		logger.Info("starting HTTP server", "address", address)
		if err := app.Listen(address); err != nil {
			serverErr <- err
		}
	}()

	signalCtx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer stop()

	select {
	case <-signalCtx.Done():
		logger.Info("shutdown signal received")

	case err := <-serverErr:
		logger.Error("HTTP server stopped unexpectedly", "error", err)
		os.Exit(1)
	}

	shutdownCtx, cancel := context.WithTimeout(
		context.Background(),
		10*time.Second,
	)
	defer cancel()

	if err := app.ShutdownWithContext(shutdownCtx); err != nil {
		if !errors.Is(err, context.Canceled) {
			logger.Error("graceful shutdown failed", "error", err)
			os.Exit(1)
		}
	}

	logger.Info("application stopped")
}
