package callback

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// The wire format keeps Timeout human-readable ("3s") instead of leaking
// nanoseconds, and treats Secret as write-only so that the list endpoint can
// never echo a shared secret back to a caller.

// callbackJSON is the JSON representation of a Callback.
type callbackJSON struct {
	ID        string    `json:"id"`
	Topic     string    `json:"topic"`
	URL       string    `json:"url"`
	Timeout   string    `json:"timeout,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// MarshalJSON implements json.Marshaler.
func (c Callback) MarshalJSON() ([]byte, error) {
	out := callbackJSON{
		ID:        c.ID,
		Topic:     c.Topic,
		URL:       c.URL,
		CreatedAt: c.CreatedAt,
	}
	if c.Timeout > 0 {
		out.Timeout = c.Timeout.String()
	}
	return json.Marshal(out)
}

// UnmarshalJSON implements json.Unmarshaler.
func (c *Callback) UnmarshalJSON(data []byte) error {
	var in callbackJSON
	if err := json.Unmarshal(data, &in); err != nil {
		return err
	}

	timeout, err := parseWireDuration(in.Timeout)
	if err != nil {
		return fmt.Errorf("callback: field \"timeout\": %w", err)
	}

	*c = Callback{
		ID:        in.ID,
		Topic:     in.Topic,
		URL:       in.URL,
		Timeout:   timeout,
		CreatedAt: in.CreatedAt,
	}
	return nil
}

func parseWireDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("must be positive, got %s", d)
	}
	return d, nil
}
