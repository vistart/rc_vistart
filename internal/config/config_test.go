package config

import (
	"strings"
	"testing"
	"time"
)

func TestDefault(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg != Default() {
		t.Errorf("Load() = %+v, want the defaults %+v", cfg, Default())
	}
}

func TestLoadFromEnvironment(t *testing.T) {
	t.Setenv("RC_VISTART_ADDR", "127.0.0.1:9999")
	t.Setenv("RC_VISTART_PUBLIC_URL", "https://notify.example.com")
	t.Setenv("RC_VISTART_READ_TIMEOUT", "3s")
	t.Setenv("RC_VISTART_WRITE_TIMEOUT", "7s")
	t.Setenv("RC_VISTART_IDLE_TIMEOUT", "90s")
	t.Setenv("RC_VISTART_SHUTDOWN_TIMEOUT", "2s")
	t.Setenv("RC_VISTART_POLL_INTERVAL", "250ms")
	t.Setenv("RC_VISTART_MAX_BODY_BYTES", "4096")
	t.Setenv("RC_VISTART_MAX_CALLBACK_RESPONSE_BYTES", "2048")
	t.Setenv("RC_VISTART_WORKERS", "3")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"Addr", cfg.Addr, "127.0.0.1:9999"},
		{"PublicURL", cfg.PublicURL, "https://notify.example.com"},
		{"ReadTimeout", cfg.ReadTimeout, 3 * time.Second},
		{"WriteTimeout", cfg.WriteTimeout, 7 * time.Second},
		{"IdleTimeout", cfg.IdleTimeout, 90 * time.Second},
		{"ShutdownTimeout", cfg.ShutdownTimeout, 2 * time.Second},
		{"PollInterval", cfg.PollInterval, 250 * time.Millisecond},
		{"MaxBodyBytes", cfg.MaxBodyBytes, int64(4096)},
		{"MaxCallbackResponseBytes", cfg.MaxCallbackResponseBytes, int64(2048)},
		{"Workers", cfg.Workers, 3},
	}
	for _, tc := range checks {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

func TestLoadIgnoresBlankValues(t *testing.T) {
	t.Setenv("RC_VISTART_READ_TIMEOUT", "")
	t.Setenv("RC_VISTART_MAX_BODY_BYTES", "   ")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.ReadTimeout != Default().ReadTimeout {
		t.Errorf("ReadTimeout = %v, want the default %v", cfg.ReadTimeout, Default().ReadTimeout)
	}
	if cfg.MaxBodyBytes != Default().MaxBodyBytes {
		t.Errorf("MaxBodyBytes = %d, want the default %d", cfg.MaxBodyBytes, Default().MaxBodyBytes)
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{"bad duration", "RC_VISTART_READ_TIMEOUT", "ten seconds"},
		{"zero duration", "RC_VISTART_READ_TIMEOUT", "0s"},
		{"negative duration", "RC_VISTART_POLL_INTERVAL", "-1s"},
		{"bad int", "RC_VISTART_MAX_BODY_BYTES", "lots"},
		{"bad int64", "RC_VISTART_MAX_CALLBACK_RESPONSE_BYTES", "1.5"},
		{"empty addr", "RC_VISTART_ADDR", " "},
		{"empty public url", "RC_VISTART_PUBLIC_URL", " "},
		{"workers below one", "RC_VISTART_WORKERS", "0"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			// Every case except the address ones needs a valid addr, and every
			// case except the public-url one needs a valid public url.
			if tc.key != "RC_VISTART_ADDR" {
				t.Setenv("RC_VISTART_ADDR", ":8080")
			}
			if tc.key != "RC_VISTART_PUBLIC_URL" {
				t.Setenv("RC_VISTART_PUBLIC_URL", "http://localhost:8080")
			}

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() error = nil, want an error for %s=%q", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("Load() error = %q, want it to mention %s", err, tc.key)
			}
		})
	}
}
