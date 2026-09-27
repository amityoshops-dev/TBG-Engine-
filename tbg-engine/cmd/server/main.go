package main

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"fmt"
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

	payoutSvc := service.NewPayoutService(repo, lienEngine, rdb, logger, cfg.HMACSalt)
	eodSvc := service.NewEODReconciler(repo, logger)
	_ = eodSvc

	mux := http.NewServeMux()

	// 1. Root Landing & API Directory Dashboard
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head>
<title>TBG-CORE Transaction Banking Engine</title>
<style>
  body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, monospace; background: #0b0f19; color: #e2e8f0; padding: 40px; margin: 0; }
  .card { background: #131c2e; border: 1px solid #1e293b; border-radius: 8px; padding: 24px; max-width: 750px; margin: 0 auto; box-shadow: 0 4px 20px rgba(0,0,0,0.5); }
  h1 { color: #38bdf8; font-size: 22px; margin-top: 0; }
  .badge { background: #0284c7; color: #fff; padding: 2px 8px; border-radius: 4px; font-size: 11px; font-weight: bold; }
  .status { color: #4ade80; font-weight: bold; }
  ul { line-height: 1.8; font-size: 14px; }
  code { background: #0f172a; color: #f472b6; padding: 2px 6px; border-radius: 4px; }
  pre { background: #0f172a; padding: 12px; border-radius: 6px; overflow-x: auto; color: #a5f3fc; font-size: 12px; }
</style>
</head>
<body>
<div class="card">
  <h1>⚡ TBG-CORE Transaction Banking Engine <span class="badge">LIVE</span></h1>
  <p>Status: <span class="status">● System Operational (Postgres + Redis + Lien Engine Active)</span></p>
  <hr style="border: 0; border-top: 1px solid #1e293b; margin: 20px 0;">
  <h3>Active Endpoints</h3>
  <ul>
    <li><b>GET</b> <a href="/healthz" style="color:#38bdf8;"><code>/healthz</code></a> — Health Check (DB & Cache)</li>
    <li><b>GET</b> <a href="/api/v1/cms/stats" style="color:#38bdf8;"><code>/api/v1/cms/stats</code></a> — Real-time CMS & Escrow Balance Stats</li>
    <li><b>POST</b> <code>/api/v1/cms/payout</code> — Cash Management Escrow Payout Rail</li>
  </ul>
  <h3>Sample Payout Payload (POST)</h3>
<pre>{
  "client_id": "CORP-CLIENT-001",
  "virtual_account": "VA8800112233",
  "beneficiary_account": "912345678901",
  "ifsc": "HDFC0000001",
  "amount": 25000.00,
  "payment_rail": "NEFT_RTGS",
  "idempotency_key": "TXN-` + time.Now().Format("20060102150405") + `"
}</pre>
</div>
</body>
</html>`)
	})

	// 2. Health check aliases
	mux.HandleFunc("/healthz", payoutSvc.HandleHealthz)
	mux.HandleFunc("/health", payoutSvc.HandleHealthz)

	// 3. Operational APIs
	mux.HandleFunc("/api/v1/cms/payout", payoutSvc.HandlePayout)
	mux.HandleFunc("/api/v1/cms/stats", payoutSvc.HandleStats)

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

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown error: %v", err)
	}
	log.Println("Server stopped")
}
