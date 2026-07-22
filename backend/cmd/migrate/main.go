// Command migrate applies the embedded MySQL migrations using the configured .env file.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/migration"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/database"
	"github.com/J-S-Te/Basic-Platform/backend/migrations"
)

func main() {
	if err := run(); err != nil {
		slog.Error("migration process stopped with an error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	db, err := database.OpenMySQL(cfg.MySQL)
	if err != nil {
		return err
	}
	defer func() { _ = database.Close(db) }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	applied, err := migration.Run(ctx, db, migrations.Files)
	if err != nil {
		return err
	}
	if len(applied) == 0 {
		slog.Info("database schema is already up to date")
		return nil
	}

	for _, item := range applied {
		slog.Info("database migration applied", "version", fmt.Sprintf("%06d", item.Version), "name", item.Name)
	}
	return nil
}
