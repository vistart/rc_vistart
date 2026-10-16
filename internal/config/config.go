// Package config loads the runtime configuration of rc_vistart from the
// process environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds every tunable setting of the service.
type Config struct {
	// Addr is the TCP address the HTTP server listens on.
	Addr string
	// PublicURL is the base URL receivers use to reach this service, which is
	// handed to them as .result_url in every delivery.
	PublicURL string
	// ReadTimeout bounds reading a whole request, including the body.
	ReadTimeout time.Duration
	// WriteTimeout bounds writing a response.
	WriteTimeout time.Duration
	// IdleTimeout bounds how long a keep-alive connection may stay idle.
	IdleTimeout time.Duration
	// ShutdownTimeout bounds how long in-flight requests may finish during a
	// graceful shutdown.
	ShutdownTimeout time.Duration
	// MaxBodyBytes bounds the size of a request body that is accepted.
	MaxBodyBytes int64
	// MaxCallbackResponseBytes bounds how much of a callback response body is
	// read before the connection is dropped.
	MaxCallbackResponseBytes int64
	// Workers is the number of concurrent delivery attempts.
	Workers int
	// PollInterval is how often an idle worker re-checks the delivery queue.
	PollInterval time.Duration
}

// Default returns the configuration used when no environment variable is set.
func Default() Config {
	return Config{
		Addr:                     ":8080",
		PublicURL:                "http://localhost:8080",
		ReadTimeout:              5 * time.Second,
		WriteTimeout:             10 * time.Second,
		IdleTimeout:              60 * time.Second,
		ShutdownTimeout:          15 * time.Second,
		MaxBodyBytes:             1 << 20,
		MaxCallbackResponseBytes: 64 << 10,
		Workers:                  8,
		PollInterval:             50 * time.Millisecond,
	}
}

// Load builds a Config from the environment, falling back to Default for
// every value that is not set. It returns an error if a value is present but
// cannot be parsed.
func Load() (Config, error) {
	cfg := Default()
	var err error

	if cfg.Addr, err = envAddr("RC_VISTART_ADDR", cfg.Addr); err != nil {
		return Config{}, err
	}
	if cfg.PublicURL, err = envRequired("RC_VISTART_PUBLIC_URL", cfg.PublicURL); err != nil {
		return Config{}, err
	}
	if cfg.ReadTimeout, err = envDuration("RC_VISTART_READ_TIMEOUT", cfg.ReadTimeout); err != nil {
		return Config{}, err
	}
	if cfg.WriteTimeout, err = envDuration("RC_VISTART_WRITE_TIMEOUT", cfg.WriteTimeout); err != nil {
		return Config{}, err
	}
	if cfg.IdleTimeout, err = envDuration("RC_VISTART_IDLE_TIMEOUT", cfg.IdleTimeout); err != nil {
		return Config{}, err
	}
	if cfg.ShutdownTimeout, err = envDuration("RC_VISTART_SHUTDOWN_TIMEOUT", cfg.ShutdownTimeout); err != nil {
		return Config{}, err
	}
	if cfg.MaxBodyBytes, err = envInt64("RC_VISTART_MAX_BODY_BYTES", cfg.MaxBodyBytes); err != nil {
		return Config{}, err
	}
	if cfg.MaxCallbackResponseBytes, err = envInt64("RC_VISTART_MAX_CALLBACK_RESPONSE_BYTES", cfg.MaxCallbackResponseBytes); err != nil {
		return Config{}, err
	}
	if cfg.Workers, err = envInt("RC_VISTART_WORKERS", cfg.Workers); err != nil {
		return Config{}, err
	}
	if cfg.PollInterval, err = envDuration("RC_VISTART_POLL_INTERVAL", cfg.PollInterval); err != nil {
		return Config{}, err
	}

	if cfg.Addr == "" {
		return Config{}, fmt.Errorf("config: RC_VISTART_ADDR must not be empty")
	}
	if cfg.PublicURL == "" {
		return Config{}, fmt.Errorf("config: RC_VISTART_PUBLIC_URL must not be empty")
	}
	if cfg.Workers < 1 {
		return Config{}, fmt.Errorf("config: RC_VISTART_WORKERS must be at least 1, got %d", cfg.Workers)
	}
	return cfg, nil
}

// envString returns the trimmed value of key, or fallback when it is unset or
// blank.
func envString(key, fallback string) (string, error) {
	v, ok := os.LookupEnv(key)
	if !ok {
		return fallback, nil
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return fallback, nil
	}
	return v, nil
}

// envAddr behaves like envString, except that an explicitly blank value is an
// error rather than a silent fallback: a service with no listen address cannot
// start, so the mistake should surface immediately.
func envAddr(key, fallback string) (string, error) {
	v, ok := os.LookupEnv(key)
	if !ok {
		return fallback, nil
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return "", fmt.Errorf("config: %s must not be empty", key)
	}
	return v, nil
}

// envRequired behaves like envString, except that an explicitly blank value is
// an error rather than a silent fallback. A required setting that quietly
// reverts to its default is a misconfiguration nobody ever sees.
func envRequired(key, fallback string) (string, error) {
	v, ok := os.LookupEnv(key)
	if !ok {
		return fallback, nil
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return "", fmt.Errorf("config: %s must not be empty", key)
	}
	return v, nil
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("config: %s: %w", key, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("config: %s must be positive, got %s", key, d)
	}
	return d, nil
}

func envInt(key string, fallback int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("config: %s: %w", key, err)
	}
	return n, nil
}

func envInt64(key string, fallback int64) (int64, error) {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("config: %s: %w", key, err)
	}
	return n, nil
}
