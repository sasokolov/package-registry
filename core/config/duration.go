package config

import (
	"encoding/json"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that unmarshals from human-readable YAML values
// such as "10s" or "1m30s".
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string like \"10s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// MarshalYAML writes the human-readable form back. Without it a duration
// would go into the document as a nanosecond count, which the loader then
// refuses to read — so an API write of any section containing a duration
// would corrupt the document it just validated.
func (d Duration) MarshalYAML() (any, error) {
	if d == 0 {
		return "0s", nil
	}
	return time.Duration(d).String(), nil
}

// MarshalJSON writes the same human-readable form, so what an API client
// reads is what it can write back.
func (d Duration) MarshalJSON() ([]byte, error) {
	if d == 0 {
		return json.Marshal("0s")
	}
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON accepts the string form ("15m"). A bare number is refused
// rather than guessed at: nanoseconds and seconds look identical in JSON and
// differ by nine orders of magnitude.
func (d *Duration) UnmarshalJSON(raw []byte) error {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"10s\": %w", err)
	}
	if s == "" {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) String() string { return time.Duration(d).String() }

// Std converts to time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }
