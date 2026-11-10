package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/customer-fabric/customer-fabric/backend/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if err := run(); err != nil {
		slog.Error("startup or server failure; check configuration and database availability")
		os.Exit(1)
	}
}
func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg := platform.Config{
		Mode:                    os.Getenv("CF_MODE"),
		OperatorToken:           os.Getenv("CF_DEV_OPERATOR_TOKEN"),
		KratosPublicURL:         os.Getenv("CF_KRATOS_PUBLIC_URL"),
		OperatorKratosPublicURL: os.Getenv("CF_OPERATOR_KRATOS_PUBLIC_URL"),
		OperatorBrowserOrigin:   os.Getenv("CF_OPERATOR_BROWSER_ORIGIN"),
		CustomerBrowserOrigin:   os.Getenv("CF_CUSTOMER_BROWSER_ORIGIN"),
	}
	if err := json.Unmarshal([]byte(os.Getenv("CF_DEV_TOKENS")), &cfg.Tokens); err != nil {
		return err
	}
	if raw := os.Getenv("CF_DEV_LINKING_ISSUERS"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cfg.LinkingIssuers); err != nil {
			return err
		}
	}
	cfg.HydraAdminURL, cfg.HydraPublicURL, cfg.HydraIssuer = os.Getenv("CF_HYDRA_ADMIN_URL"), os.Getenv("CF_HYDRA_PUBLIC_URL"), os.Getenv("CF_HYDRA_ISSUER")
	if raw := os.Getenv("CF_WEBHOOK_ALLOWED_HOSTS"); raw != "" {
		cfg.WebhookHosts = strings.Split(raw, ",")
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	dbcfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	dbcfg.MaxConns = 10
	dbcfg.ConnConfig.ConnectTimeout = 5 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, dbcfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	boot, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err = pool.Ping(boot); err != nil {
		return err
	}
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		return migrations.Apply(boot, pool)
	}
	// Sources are deployment configuration; callers cannot create or impersonate them.
	for source := range cfg.Tokens {
		if err = db.New(pool).RegisterSource(boot, source); err != nil {
			return err
		}
	}
	handler, err := platform.NewAPI(platform.Store{Pool: pool}, cfg)
	if err != nil {
		return err
	}
	// Activation deliveries go only to configured webhook hosts, so without them nothing is dispatched.
	if len(cfg.WebhookHosts) > 0 {
		go platform.NewDispatcher(platform.Store{Pool: pool}, cfg.WebhookHosts).Run(ctx, 2*time.Second)
	}
	addr := os.Getenv("CF_LISTEN")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	srv := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	done := make(chan error, 1)
	go func() { slog.Info("development API listening", "address", addr); done <- srv.ListenAndServe() }()
	select {
	case err = <-done:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}
