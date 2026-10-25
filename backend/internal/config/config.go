// Package config loads all runtime configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	HTTPAddr        string
	DatabaseURL     string
	JWTSecret       string
	AccessTTL       time.Duration
	RefreshTTL      time.Duration
	RequestTimeout  time.Duration
	ShutdownTimeout time.Duration
	LogLevel        string
	CORSOrigin      string
}

func Load() (Config, error) {
	c := Config{
		HTTPAddr:        env("HTTP_ADDR", ":8080"),
		DatabaseURL:     env("DATABASE_URL", ""),
		JWTSecret:       env("JWT_SECRET", ""),
		AccessTTL:       envDur("JWT_ACCESS_TTL", 15*time.Minute),
		RefreshTTL:      envDur("JWT_REFRESH_TTL", 7*24*time.Hour),
		RequestTimeout:  envDur("REQUEST_TIMEOUT", 30*time.Second),
		ShutdownTimeout: envDur("SHUTDOWN_TIMEOUT", 15*time.Second),
		LogLevel:        env("LOG_LEVEL", "info"),
		CORSOrigin:      env("CORS_ORIGIN", "http://localhost:3000"),
	}
	if c.DatabaseURL == "" {
		return c, fmt.Errorf("DATABASE_URL is required")
	}
	if len(c.JWTSecret) < 32 {
		return c, fmt.Errorf("JWT_SECRET must be at least 32 characters")
	}
	return c, nil
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second
	}
	return def
}
