// Package main runs the migration service: HTTP API + asynq worker.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/NIPA-Mimir/services/migrator/internal/api"
	"github.com/NIPA-Mimir/services/migrator/internal/cortexcheck"
	"github.com/NIPA-Mimir/services/migrator/internal/mimirconfig"
	"github.com/NIPA-Mimir/services/migrator/internal/queue"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := loadConfig()
	if err != nil {
		logger.Error("failed to load config", "err", err)
		os.Exit(1)
	}
	logger.Info("config loaded",
		"redis_addr", cfg.redisAddr,
		"http_addr", cfg.httpAddr,
		"tsdb_path", cfg.tsdbPath,
		"max_batch_bytes", cfg.maxBatchBytes,
		"writer_concurrency", cfg.writerConcurrency,
		"queue_concurrency", cfg.queueConcurrency,
		"dry_run", cfg.dryRun,
	)

	if err := cortexcheck.Probe(context.Background(), cfg.cortexURL, logger); err != nil {
		logger.Error("cortex_url validation failed", "err", err)
		os.Exit(1)
	}

	rdb := redis.NewClient(&redis.Options{Addr: cfg.redisAddr})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		logger.Warn("redis ping failed — will retry on demand", "err", err)
	}

	progressStore := queue.NewProgressStore(rdb)

	// Recover from crash: mark any "active" progress entries left over from a
	// previous worker as failed. Runs before the asynq worker starts so no
	// currently-running task is misclassified.
	if n, err := progressStore.ReconcileStaleActive(context.Background()); err != nil {
		logger.Warn("reconcile stale active tasks failed", "err", err)
	} else if n > 0 {
		logger.Info("reconciled stale active tasks", "count", n)
	}

	// nil historyLogger is safe — tasks still run without history enabled.
	var historyLogger *queue.HistoryLogger
	if cfg.historyLogPath != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.historyLogPath), 0755); err != nil {
			logger.Warn("history log mkdir failed; history disabled", "path", cfg.historyLogPath, "err", err)
		} else {
			f, err := os.OpenFile(cfg.historyLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
			if err != nil {
				logger.Warn("history log open failed; history disabled", "path", cfg.historyLogPath, "err", err)
			} else {
				historyLogger = queue.NewHistoryLogger(f)
				defer f.Close()
				logger.Info("history log opened", "path", cfg.historyLogPath)
			}
		}
	}

	// Applier resolution order: in-cluster → KUBECONFIG → NoopApplier.
	var applier api.OverrideApplier = mimirconfig.NoopApplier{}
	k8sCfg, err := rest.InClusterConfig()
	if err != nil {
		kubeconfigPath := os.Getenv("KUBECONFIG")
		k8sCfg, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
		if err != nil {
			logger.Warn("no kubeconfig found — mimir overrides disabled (NoopApplier)", "err", err)
		}
	}
	if k8sCfg != nil {
		clientset, err := kubernetes.NewForConfig(k8sCfg)
		if err != nil {
			logger.Warn("failed to build k8s clientset — mimir overrides disabled", "err", err)
		} else {
			applier = mimirconfig.New(mimirconfig.Config{
				K8s:           clientset,
				Namespace:     "monitoring",
				ConfigMapName: "mimir-runtime",
				DataKey:       "runtime.yaml",
				OOOWindow:     "2880h",
				Logger:        logger,
			})
			logger.Info("mimir override applier initialised", "namespace", "monitoring", "configmap", "mimir-runtime")
		}
	}

	asynqClient := asynq.NewClient(asynq.RedisClientOpt{Addr: cfg.redisAddr})
	defer asynqClient.Close()

	inspector := asynq.NewInspector(asynq.RedisClientOpt{Addr: cfg.redisAddr})
	defer inspector.Close()

	asynqServer := asynq.NewServer(
		asynq.RedisClientOpt{Addr: cfg.redisAddr},
		asynq.Config{
			Concurrency: cfg.queueConcurrency,
			Queues:      map[string]int{"migration": 10, "default": 1},
			Logger:      newAsynqLogger(logger),
		},
	)

	handler := &queue.MigrationHandler{
		Store:   progressStore,
		Logger:  logger,
		History: historyLogger,
	}

	asynqMux := asynq.NewServeMux()
	asynqMux.Handle(queue.TypeMigrateTenant, handler)

	// HTTP server.
	httpMux := api.NewServer(api.ServerConfig{
		Client:            asynqClient,
		ProgressStore:     progressStore,
		Applier:           applier,
		Inspector:         inspector,
		TSDBPath:          cfg.tsdbPath,
		CortexURL:         cfg.cortexURL,
		MaxBatchBytes:     cfg.maxBatchBytes,
		WriterConcurrency: cfg.writerConcurrency,
		DryRun:            cfg.dryRun,
		Logger:            logger,
	})

	httpServer := &http.Server{
		Addr:         cfg.httpAddr,
		Handler:      httpMux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	go func() {
		if err := asynqServer.Start(asynqMux); err != nil {
			logger.Error("asynq server failed", "err", err)
			os.Exit(1)
		}
	}()
	logger.Info("asynq worker started", "concurrency", cfg.queueConcurrency)

	go func() {
		logger.Info("http server starting", "addr", cfg.httpAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http server failed", "err", err)
			os.Exit(1)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	logger.Info("received signal, shutting down", "signal", sig)

	asynqServer.Shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		logger.Error("http shutdown error", "err", err)
	}

	_ = rdb.Close()
	logger.Info("shutdown complete")
}

type config struct {
	redisAddr         string
	tsdbPath          string
	cortexURL         string
	historyLogPath    string
	maxBatchBytes     int
	writerConcurrency int
	queueConcurrency  int
	httpAddr          string
	dryRun            bool
}

func loadConfig() (config, error) {
	c := config{
		redisAddr:         envOrDefault("REDIS_ADDR", "localhost:6379"),
		tsdbPath:          envOrDefault("TSDB_PATH", "/data/tsdb"),
		cortexURL:         envOrDefault("CORTEX_URL", "http://localhost:8080"),
		historyLogPath:    envOrDefault("HISTORY_LOG_PATH", "/var/log/migrator/history.jsonl"),
		httpAddr:          envOrDefault("HTTP_ADDR", ":8090"),
		maxBatchBytes:     3 * 1024 * 1024,
		writerConcurrency: 4,
		queueConcurrency:  1,
		dryRun:            false,
	}

	if v := os.Getenv("MAX_BATCH_BYTES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return c, fmt.Errorf("invalid MAX_BATCH_BYTES: %w", err)
		}
		c.maxBatchBytes = n
	}
	if v := os.Getenv("WRITER_CONCURRENCY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return c, fmt.Errorf("invalid WRITER_CONCURRENCY: %w", err)
		}
		c.writerConcurrency = n
	}
	if v := os.Getenv("QUEUE_CONCURRENCY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return c, fmt.Errorf("invalid QUEUE_CONCURRENCY: %w", err)
		}
		c.queueConcurrency = n
	}
	if v := os.Getenv("DRY_RUN"); v == "true" || v == "1" {
		c.dryRun = true
	}

	if c.cortexURL == "" {
		return c, fmt.Errorf("CORTEX_URL is required")
	}
	return c, nil
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

type asynqLogAdapter struct {
	logger *slog.Logger
}

func newAsynqLogger(l *slog.Logger) *asynqLogAdapter {
	return &asynqLogAdapter{logger: l.With("component", "asynq")}
}

func (a *asynqLogAdapter) Debug(args ...interface{}) { a.logger.Debug(fmt.Sprint(args...)) }
func (a *asynqLogAdapter) Info(args ...interface{})  { a.logger.Info(fmt.Sprint(args...)) }
func (a *asynqLogAdapter) Warn(args ...interface{})  { a.logger.Warn(fmt.Sprint(args...)) }
func (a *asynqLogAdapter) Error(args ...interface{}) { a.logger.Error(fmt.Sprint(args...)) }
func (a *asynqLogAdapter) Fatal(args ...interface{}) {
	a.logger.Error(fmt.Sprint(args...))
	os.Exit(1)
}
