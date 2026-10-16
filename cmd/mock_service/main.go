// Command mock_service simulates a downstream service (inventory, CRM, an ad
// network, ...) that registers its endpoint with rc_vistart and waits to be
// notified.
//
// It exists to exercise the contract rather than to be useful: it can be told
// to fail, to stay silent, and to deduplicate, which together cover the retry,
// result-timeout and duplicate-delivery paths.
//
// The same behaviour backs `make demo`, which drives it automatically.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vistart/rc_vistart/internal/mockservice"
)

func main() {
	var (
		addr        = flag.String("addr", "127.0.0.1:9001", "listen address")
		target      = flag.String("target", "http://127.0.0.1:8080", "rc_vistart base URL")
		service     = flag.String("service", "inventory", "service name to register")
		action      = flag.String("action", "stock.prehold", "action name to register")
		method      = flag.String("method", "POST", "HTTP method to advertise")
		path        = flag.String("path", "/notify", "path this service serves")
		publicURL   = flag.String("public-url", "", "URL to advertise (defaults to the listen address)")
		secret      = flag.String("secret", "", "shared secret, exposed to templates as .secret")
		bodyTmpl    = flag.String("body", "", "body template to advertise (defaults to a JSON one carrying the result URL)")
		failFirst   = flag.Int("fail-first", 0, "fail the first N attempts of each task, then succeed")
		failForever = flag.Bool("fail-forever", false, "always fail, to demonstrate escalation")
		failStatus  = flag.Int("fail-status", 503, "status returned when failing (503 retries, 400 escalates at once)")
		silent      = flag.Bool("silent", false, "acknowledge but never post a result, to demonstrate the result timeout")
		resultTO    = flag.Duration("result-timeout", 10*time.Second, "advertised result timeout")
		maxAttempts = flag.Int("max-attempts", 3, "advertised attempt budget")
		backoffBase = flag.Duration("backoff-base", 300*time.Millisecond, "advertised first retry delay")
		backoffMax  = flag.Duration("backoff-max", 3*time.Second, "advertised maximum retry delay")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	mock, err := mockservice.New(mockservice.Config{
		Target:        *target,
		ListenAddr:    *addr,
		Service:       *service,
		Action:        *action,
		Method:        *method,
		Path:          *path,
		PublicURL:     *publicURL,
		Secret:        *secret,
		BodyTmpl:      *bodyTmpl,
		FailFirst:     *failFirst,
		FailForever:   *failForever,
		FailStatus:    *failStatus,
		Silent:        *silent,
		ResultTimeout: *resultTO,
		MaxAttempts:   *maxAttempts,
		BackoffBase:   *backoffBase,
		BackoffMax:    *backoffMax,
		Logger:        logger,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "could not start mock: %v\n", err)
		os.Exit(1)
	}
	defer mock.Close()

	fmt.Printf("mock %s listening on %s (fail-first=%d fail-forever=%v fail-status=%d silent=%v)\n",
		mock.Topic(), mock.URL(), *failFirst, *failForever, *failStatus, *silent)
	fmt.Printf("inspect what it received: curl %s/state\n", mock.URL())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	fmt.Println("mock shutting down")
}
