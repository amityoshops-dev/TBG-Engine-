package main

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	"crypto/tls"
	"log"
	"os"
	"strings"

	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"

	"tbg-engine/internal/config"
	"tbg-engine/internal/ledger"
	"tbg-engine/internal/observability"
	"tbg-engine/internal/service"
)

func main() {
	cfg := config.Load()
	log.Printf("DEBUG REDIS_URL set=%v length=%d", cfg.RedisURL != "", len(cfg.RedisURL))
	logger := observability.New()

	db, err := sql.Open("postgres", cfg.PostgresDSN)
	if err != nil {
		log.Fatalf("postgres connection error: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(50)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(30 * time.Minute)

	if err := db.Ping(); err != nil {
		log.Fatalf("postgres ping failed: %v", err)
	}

	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr})
	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		log.Fatalf("redis connection error: %v", err)
	}

	repo := ledger.NewRepository(db)
	lienEngine := ledger.NewLienEngine(rdb)
	payoutSvc := service.NewPayoutService(repo, lienEngine, rdb, logger, cfg.HMACSalt)
	eodSvc := service.NewEODReconciler(repo, logger)
	_ = eodSvc // wire to a scheduled cron trigger (e.g. robfig/cron) for real EOD automation

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/cms/payout", payoutSvc.HandlePayout)
	mux.HandleFunc("/api/v1/cms/stats", payoutSvc.HandleStats)
	mux.HandleFunc("/healthz", payoutSvc.HandleHealthz)

	srv := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		// 1. Read directly from environment variable (falls back to cfg.RedisAddr if not set)
	rawRedisURL := os.Getenv("REDIS_URL")
	if rawRedisURL == "" {
		rawRedisURL = os.Getenv("REDIS_ADDR")
	}
	if rawRedisURL == "" {
		rawRedisURL = cfg.RedisAddr
	}

	log.Printf("DEBUG REDIS_URL present=%v length=%d", rawRedisURL != "", len(rawRedisURL))

	// 2. Parse URL and handle rediss:// (TLS) automatically for managed providers
	var redisOpt *redis.Options
	if strings.HasPrefix(rawRedisURL, "redis://") || strings.HasPrefix(rawRedisURL, "rediss://") {
		var err error
		redisOpt, err = redis.ParseURL(rawRedisURL)
		if err != nil {
			log.Fatalf("failed to parse REDIS_URL: %v", err)
		}
	} else {
		redisOpt = &redis.Options{
			Addr: rawRedisURL,
		}
	}

	// Enable TLS if rediss:// scheme is used (required for Upstash / Render managed Redis)
	if strings.HasPrefix(rawRedisURL, "rediss://") {
		redisOpt.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
		}
	}

	rdb := redis.NewClient(redisOpt)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	log.Println("shutting down gracefully...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("graceful shutdown failed: %v", err)
	}
}
