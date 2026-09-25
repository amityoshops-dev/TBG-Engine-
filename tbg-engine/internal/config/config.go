package config

import "os"

// Config holds runtime configuration loaded from environment variables, with
// sensible local-development defaults matching docker-compose.yml.
type Config struct {
	PostgresDSN string
	RedisAddr   string
	ListenAddr  string
	HMACSalt    string
}

func Load() *Config {
	return &Config{
		PostgresDSN: getEnv("POSTGRES_DSN", "postgres://postgres:postgres@localhost:5432/cms_ledger?sslmode=disable"),
		RedisAddr:   getEnv("REDIS_ADDR", "localhost:6379"),
		ListenAddr:  getEnv("LISTEN_ADDR", ":8080"),
		HMACSalt:    getEnv("HMAC_SALT", "change-me-in-production"),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
