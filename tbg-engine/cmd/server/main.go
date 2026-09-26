
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
	// 1. Load configuration
	cfg := config.Load()

	logger := observability.New()

	// 2. Connect to PostgreSQL
	db, err := sql.Open("postgres", cfg.PostgresDSN)
	if err != nil {
		log.Fatalf("postgres connection error: %v", err)
	}
	defer db.Close()

	db.SetMaxOpenConns(50)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(30 * time.Minute)

	ctx, cancel := context.WithTimeout(
		context.Background(),
		10*time.Second,
	)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("postgres ping failed: %v", err)
	}

	log.Println("PostgreSQL connected successfully")

	// 3. Configure Redis using Render environment variables
	rawRedisURL := strings.TrimSpace(os.Getenv("REDIS_URL"))

	if rawRedisURL == "" {
		rawRedisURL = strings.TrimSpace(os.Getenv("REDIS_ADDR"))
	}

	if rawRedisURL == "" {
		rawRedisURL = strings.TrimSpace(cfg.RedisAddr)
	}

	if rawRedisURL == "" {
		log.Fatal("Redis configuration missing: set REDIS_URL or REDIS_ADDR")
	}

	var redisOpt *redis.Options

	if strings.HasPrefix(rawRedisURL, "redis://") ||
		strings.HasPrefix(rawRedisURL, "rediss://") {

		redisOpt, err = redis.ParseURL(rawRedisURL)
		if err != nil {
			log.Fatalf("failed to parse Redis URL: %v", err)
		}

	} else {
		// Supports host:port Redis addresses
		redisOpt = &redis.Options{
			Addr: rawRedisURL,
		}
	}

	// Enable TLS for rediss:// managed Redis connections
	if strings.HasPrefix(rawRedisURL, "rediss://") {
		redisOpt.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
		}
	}

	// Create exactly ONE Redis client
	rdb := redis.NewClient(redisOpt)
	defer rdb.Close()

	redisCtx, redisCancel := context.WithTimeout(
		context.Background(),
		10*time.Second,
	)
	defer redisCancel()

	if err := rdb.Ping(redisCtx).Err(); err != nil {
		log.Fatalf("redis connection error: %v", err)
	}

	log.Println("Redis connected successfully")

	// 4. Initialize application services
	repo := ledger.NewRepository(db)

	lienEngine := ledger.NewLienEngine(rdb)

	payoutSvc := service.NewPayoutService(
		repo,
		lienEngine,
		rdb,
		logger,
		cfg.HMACSalt,
	)

	eodSvc := service.NewEODReconciler(repo, logger)

	// Reserved for scheduled EOD reconciliation.
	// Connect to a cron scheduler when implementing automation.
	_ = eodSvc

	// 5. Register HTTP routes
	mux := http.NewServeMux()

	mux.HandleFunc(
		"/api/v1/cms/payout",
		payoutSvc.HandlePayout,
	)

	mux.HandleFunc(
		"/api/v1/cms/stats",
		payoutSvc.HandleStats,
	)

	mux.HandleFunc(
		"/healthz",
		payoutSvc.HandleHealthz,
	)

	// 6. Configure the HTTP server
	addr := strings.TrimSpace(os.Getenv("PORT"))

	if addr == "" {
		addr = strings.TrimSpace(cfg.ListenAddr)
	}

	if addr == "" {
		addr = "8080"
	}

	// Render provides PORT as a numeric value.
	// Convert it into a valid TCP listen address.
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

	// 7. Start HTTP server
	go func() {
		log.Printf("TBG-CORE API starting on %s", addr)

		if err := srv.ListenAndServe(); err != nil &&
			err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	// 8. Graceful shutdown
	stop := make(chan os.Signal, 1)

	signal.Notify(
		stop,
		syscall.SIGINT,
		syscall.SIGTERM,
	)

	<-stop

	log.Println("Shutting down gracefully...")

	shutdownCtx, shutdownCancel := context.WithTimeout(
		context.Background(),
		10*time.Second,
	)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown error: %v", err)
	}

	log.Println("Server stopped")
}
