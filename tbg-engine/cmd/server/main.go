package main

import (
	"context"
	"crypto/tls"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

type App struct {
	DB    *sql.DB
	Redis *redis.Client
}

func main() {
	log.Println("Starting TBG-Engine service...")

	// 1. Initialize PostgreSQL
	db := initPostgres()
	defer db.Close()

	// 2. Initialize Redis
	rdb := initRedis()
	defer rdb.Close()

	app := &App{
		DB:    db,
		Redis: rdb,
	}

	// 3. HTTP Server Setup
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", app.healthHandler)
	mux.HandleFunc("/", app.rootHandler)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	server := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// 4. Graceful Shutdown Listener
	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("Server listening on port %s", port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	<-stopChan
	log.Println("Shutting down server gracefully...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("Forced shutdown error: %v", err)
	}

	log.Println("TBG-Engine stopped cleanly.")
}

func initPostgres() *sql.DB {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL environment variable is missing")
	}

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Fatalf("PostgreSQL connection configuration error: %v", err)
	}

	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("PostgreSQL ping failed: %v", err)
	}

	log.Println("PostgreSQL connected successfully")
	return db
}

func initRedis() *redis.Client {
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		log.Fatal("REDIS_URL environment variable is missing")
	}

	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		log.Fatalf("Invalid REDIS_URL format: %v", err)
	}

	// Fix for the EOF issue on Render:
	// If the URL starts with rediss:// (TLS) or the host is an external render URL
	if strings.HasPrefix(redisURL, "rediss://") || opt.TLSConfig != nil {
		if opt.TLSConfig == nil {
			opt.TLSConfig = &tls.Config{}
		}
		// Allows connection to self-signed or proxy TLS terminations
		opt.TLSConfig.InsecureSkipVerify = true
	}

	client := redis.NewClient(opt)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		log.Fatalf("redis connection error: %v", err)
	}

	log.Println("Redis connected successfully")
	return client
}

func (a *App) healthHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := a.DB.PingContext(ctx); err != nil {
		http.Error(w, "Database unavailable", http.StatusServiceUnavailable)
		return
	}

	if err := a.Redis.Ping(ctx).Err(); err != nil {
		http.Error(w, "Redis unavailable", http.StatusServiceUnavailable)
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintln(w, "OK")
}

func (a *App) rootHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintln(w, "TBG-Engine is active")
}
