package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/events"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/customer-fabric/customer-fabric/backend/internal/workflows"
	"github.com/customer-fabric/customer-fabric/backend/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/client"
	tlog "go.temporal.io/sdk/log"
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
	// Only local development may send webhooks to private networks or over plain http.
	cfg.WebhookAllowPrivate = os.Getenv("CF_WEBHOOK_ALLOW_PRIVATE_NETWORKS") == "true"
	cfg.WebhookAllowPlaintext = os.Getenv("CF_WEBHOOK_ALLOW_PLAINTEXT") == "true"
	if err := cfg.Validate(); err != nil {
		return err
	}
	keys, err := sealingKeys()
	if err != nil {
		return err
	}
	if len(cfg.WebhookHosts) > 0 && !keys.Configured() {
		return errors.New("webhooks need CF_WEBHOOK_SECRET_KEYS_FILE or CF_WEBHOOK_SECRET_KEYS")
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
	store := platform.Store{Pool: pool, Keys: keys}
	// A wrong or removed key does not stop the API: sends to the destinations it affects wait, and
	// nothing stored is changed. The status says what an operator must do.
	if status, err := store.SecretStatus(boot); err != nil {
		return err
	} else if !status.SigningAvailable {
		slog.Error("webhook signing secrets cannot be opened with the configured keys; see GET /v1/operator/destination-secrets",
			"wrong_keys", len(status.WrongKeys), "unavailable", len(status.Unavailable))
	}
	// Sources are deployment configuration; callers cannot create or impersonate them.
	for source := range cfg.Tokens {
		if err = db.New(pool).RegisterSource(boot, source); err != nil {
			return err
		}
	}
	handler, err := platform.NewAPI(store, cfg)
	if err != nil {
		return err
	}
	// Activation deliveries go only to configured webhook hosts, so without them nothing is dispatched.
	if len(cfg.WebhookHosts) > 0 {
		go platform.NewDispatcher(store, cfg.Egress()).Run(ctx, 2*time.Second)
	}
	go platform.Housekeeper{Store: store}.Run(ctx, time.Minute)
	go platform.IdentityReconciler{Store: store}.Run(ctx, time.Second)
	// Audience jobs run in this process, outside any request. Two workers bound the load they add.
	// Each attempt is one transaction, so after a restart an unfinished job is evaluated again from the
	// start under a new snapshot.
	for range 2 {
		go platform.NewAudienceWorker(store).Run(ctx, 2*time.Second)
	}
	// The outbox relay is opt-in: without brokers the outbox simply accumulates, as before (ADR 0013).
	if brokers := os.Getenv("CF_KAFKA_BROKERS"); brokers != "" {
		relay, closeRelay, err := newRelay(store, strings.Split(brokers, ","))
		if err != nil {
			return err
		}
		defer closeRelay()
		go relay.Run(ctx, time.Second)
		// Profile-change workflows are a further opt-in on top of the relay (ADR 0014).
		if address := os.Getenv("CF_TEMPORAL_ADDRESS"); address != "" {
			stopWorkflows, err := startWorkflows(ctx, store, cfg.Egress(), strings.Split(brokers, ","), address, relay.Tenant)
			if err != nil {
				return err
			}
			defer stopWorkflows()
		}
	} else if os.Getenv("CF_TEMPORAL_ADDRESS") != "" {
		return errors.New("CF_TEMPORAL_ADDRESS needs CF_KAFKA_BROKERS: workflows start from relayed events")
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

// sealingKeys reads the keys that seal webhook signing secrets, from CF_WEBHOOK_SECRET_KEYS_FILE (a file
// a secret manager mounts, the production path) or CF_WEBHOOK_SECRET_KEYS, never both.
func sealingKeys() (platform.SealingKeys, error) {
	raw, path := os.Getenv("CF_WEBHOOK_SECRET_KEYS"), os.Getenv("CF_WEBHOOK_SECRET_KEYS_FILE")
	if raw != "" && path != "" {
		return platform.SealingKeys{}, errors.New("set CF_WEBHOOK_SECRET_KEYS or CF_WEBHOOK_SECRET_KEYS_FILE, not both")
	}
	if path != "" {
		content, err := os.ReadFile(path)
		if err != nil {
			return platform.SealingKeys{}, errors.New("CF_WEBHOOK_SECRET_KEYS_FILE cannot be read")
		}
		raw = string(content)
	}
	return platform.ParseSealingKeys(raw)
}

// newRelay configures the outbox relay for Kafka. Invalid configuration fails startup; a broker that is
// down does not: the relay keeps trying to create its topic in the background, and events wait in the
// outbox meanwhile, as they do during any later outage.
func newRelay(store platform.Store, brokers []string) (platform.Relay, func(), error) {
	topic := os.Getenv("CF_KAFKA_TOPIC")
	if topic == "" {
		topic = "customer-fabric.profile-events.v1"
	}
	tenant := os.Getenv("CF_TENANT_ID")
	if !events.ValidTenant(tenant) {
		return platform.Relay{}, nil, errors.New("CF_TENANT_ID must name the tenant when CF_KAFKA_BROKERS is set")
	}
	kafka, err := events.NewKafka(brokers, topic)
	if err != nil {
		return platform.Relay{}, nil, err
	}
	prepare := func(ctx context.Context) error { return kafka.EnsureTopic(ctx, 3, 7*24*time.Hour) }
	return platform.Relay{Store: store, Publisher: kafka, Tenant: tenant, Batch: 500, Prepare: prepare}, kafka.Close, nil
}

// startWorkflows runs the Temporal worker for profile-change and recovery workflows, the Kafka consumer
// that starts profile-change workflows and the starter for operator-requested recoveries. None needs
// Temporal to be reachable at startup: the client connects lazily, the worker retries, and the consumer
// and the starter hold their place until workflows can be started.
func startWorkflows(ctx context.Context, store platform.Store, egress platform.Egress, brokers []string, address, tenant string) (func(), error) {
	namespace := os.Getenv("CF_TEMPORAL_NAMESPACE")
	if namespace == "" {
		namespace = "customer-fabric"
	}
	temporal, err := client.NewLazyClient(client.Options{HostPort: address, Namespace: namespace,
		Logger: tlog.NewStructuredLogger(slog.Default())})
	if err != nil {
		return nil, err
	}
	activities := &workflows.Activities{Notifier: platform.NewNotifier(store, egress)}
	topic := os.Getenv("CF_KAFKA_TOPIC")
	if topic == "" {
		topic = "customer-fabric.profile-events.v1"
	}
	kafka, err := workflows.NewConsumerClient(brokers, topic, "customer-fabric.profile-change-workflows")
	if err != nil {
		temporal.Close()
		return nil, err
	}
	run, stop := context.WithCancel(ctx)
	var done sync.WaitGroup
	done.Go(func() { workflows.RunWorker(run, temporal, activities) })
	done.Go(func() { workflows.Consumer{Kafka: kafka, Temporal: temporal, Tenant: tenant}.Run(run) })
	done.Go(func() { workflows.RecoveryStarter{Store: store, Temporal: temporal, Tenant: tenant}.Run(run) })
	return func() {
		stop()
		done.Wait()
		kafka.Close()
		temporal.Close()
	}, nil
}
