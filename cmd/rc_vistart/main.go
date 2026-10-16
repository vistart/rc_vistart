// Command rc_vistart runs the internal delivery service: business systems
// submit orders, and rc_vistart notifies the downstream endpoints they
// registered, retrying until acknowledged and escalating what never lands.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/vistart/rc_vistart/internal/config"
	"github.com/vistart/rc_vistart/internal/httpapi"
	"github.com/vistart/rc_vistart/internal/notify"
	"github.com/vistart/rc_vistart/internal/order"
	"github.com/vistart/rc_vistart/internal/version"
)

func main() {
	if err := run(); err != nil {
		slog.Error("rc_vistart exited", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := newLogger()
	slog.SetDefault(logger)

	logger.Info("starting rc_vistart",
		slog.String("version", version.String()),
		slog.String("addr", cfg.Addr),
		slog.String("public_url", cfg.PublicURL),
	)

	registry := notify.NewMemoryRegistry()
	stats := notify.NewStats()

	invoker := notify.NewHTTPInvoker(&http.Client{
		// No client-side timeout: each attempt bounds itself through the
		// endpoint's timeout, and a global one would cut long requests short.
		Transport: http.DefaultTransport,
	}, cfg.MaxCallbackResponseBytes)

	engine := order.NewEngine(registry, invoker, stats, order.Config{
		Workers:      cfg.Workers,
		PollInterval: cfg.PollInterval,
		PublicURL:    cfg.PublicURL,
	}, logger)

	// An escalation means a task will not be delivered and a human is now
	// responsible for it. At prototype scale a loud log line and the
	// /api/v1/escalations view are the escalation channel; a real deployment
	// would page from this hook.
	engine.OnEscalate(func(esc order.Escalation) {
		logger.Error("ESCALATION: task needs manual recovery",
			slog.String("order_id", esc.OrderID),
			slog.String("task_key", esc.TaskKey),
			slog.String("topic", esc.Topic),
			slog.Int("attempts", esc.Attempts),
			slog.String("error", esc.Err.Error()),
		)
	})
	engine.Start()

	srv := &http.Server{
		Addr: cfg.Addr,
		Handler: httpapi.NewRouter(httpapi.Dependencies{
			Registry:     registry,
			Stats:        stats,
			Engine:       engine,
			Logger:       logger,
			MaxBodyBytes: cfg.MaxBodyBytes,
		}),
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		ReadHeaderTimeout: cfg.ReadTimeout,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("listening", slog.String("addr", cfg.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-serveErr:
		engine.Stop()
		return err
	case <-ctx.Done():
		stop() // a second signal kills the process outright
		logger.Info("shutdown signal received",
			slog.Duration("timeout", cfg.ShutdownTimeout),
		)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		// In-flight deliveries outlived the grace period; drop them and exit
		// non-zero so a supervisor restarts the process.
		return errors.Join(errors.New("graceful shutdown failed: "+err.Error()), srv.Close())
	}

	// Only stop the workers once the listener is closed, so an in-flight
	// delivery can still be retried by the caller rather than being cut off.
	engine.Stop()
	logger.Info("shutdown complete")
	return nil
}

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	if v := strings.TrimSpace(os.Getenv("RC_VISTART_LOG_LEVEL")); v != "" {
		if err := level.UnmarshalText([]byte(v)); err != nil {
			slog.Warn("invalid RC_VISTART_LOG_LEVEL, falling back to info", slog.String("value", v))
			level = slog.LevelInfo
		}
	}
	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	if strings.EqualFold(strings.TrimSpace(os.Getenv("RC_VISTART_LOG_FORMAT")), "text") {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(handler).With(slog.String("service", "rc_vistart"))
}
