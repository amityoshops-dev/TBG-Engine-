package main

import (
	"context"
	"crypto/tls"
	"database/sql"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"

	"tbg-engine/internal/config"
	"tbg-engine/internal/ledger"
	"tbg-engine/internal/observability"
	"tbg-engine/internal/service"
)

func main() {
	cfg := config.Load()
	logger := observability.New()

	db, err := sql.Open("postgres", cfg.PostgresDSN)
	if err != nil {
		log.Fatalf("postgres connection error: %v", err)
	}
	defer db.Close()

	db.SetMaxOpenConns(50)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(30 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("postgres ping failed: %v", err)
	}
	log.Println("PostgreSQL connected successfully")

	rawRedisURL := strings.TrimSpace(os.Getenv("REDIS_URL"))
	if rawRedisURL == "" {
		rawRedisURL = strings.TrimSpace(os.Getenv("REDIS_ADDR"))
	}
	if rawRedisURL == "" {
		rawRedisURL = "localhost:6379"
	}

	var redisOpt *redis.Options
	if strings.HasPrefix(rawRedisURL, "redis://") || strings.HasPrefix(rawRedisURL, "rediss://") {
		redisOpt, err = redis.ParseURL(rawRedisURL)
		if err != nil {
			log.Fatalf("failed to parse Redis URL: %v", err)
		}
	} else {
		redisOpt = &redis.Options{Addr: rawRedisURL}
	}

	if strings.HasPrefix(rawRedisURL, "rediss://") {
		redisOpt.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	rdb := redis.NewClient(redisOpt)
	defer rdb.Close()

	redisCtx, redisCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer redisCancel()

	if err := rdb.Ping(redisCtx).Err(); err != nil {
		log.Fatalf("redis connection error: %v", err)
	}
	log.Println("Redis connected successfully")

	repo := ledger.NewRepository(db)
	lienEngine := ledger.NewLienEngine(rdb)

	// Bootstrap corporate float in Redis (10,000,000 INR)
	if err := lienEngine.SeedFloat(context.Background(), "00040310001928", 10000000.00); err != nil {
		log.Printf("failed to seed float: %v", err)
	} else {
		log.Println("Corporate float seeded: 00040310001928 = INR 10,000,000.00")
	}

	payoutSvc := service.NewPayoutService(repo, lienEngine, rdb, logger, cfg.HMACSalt)
	eodSvc := service.NewEODReconciler(repo, logger)
	_ = eodSvc

	mux := http.NewServeMux()

	mux.HandleFunc("/api/v1/cms/payout", payoutSvc.HandlePayout)
	mux.HandleFunc("/api/v1/cms/stats", payoutSvc.HandleStats)
	mux.HandleFunc("/healthz", payoutSvc.HandleHealthz)
	mux.HandleFunc("/health", payoutSvc.HandleHealthz)

	// Root path handler to avoid 404
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		payoutSvc.HandleStats(w, r)
	})

	addr := strings.TrimSpace(os.Getenv("PORT"))
	if addr == "" {
		addr = strings.TrimSpace(cfg.ListenAddr)
	}
	if addr == "" {
		addr = "8080"
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = ":" + strings.TrimPrefix(addr, ":")
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		log.Printf("TBG-CORE API starting on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	log.Println("Shutting down gracefully...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	srv.Shutdown(shutdownCtx)
	log.Println("Server stopped")
}
