package client

import (
	"errors"
	"fmt"
	"time"
)

// Config validation errors (fail fast at New, never mid-connect).
var (
	// ErrInvalidConfig reports configuration values that cannot work.
	ErrInvalidConfig = errors.New("client: invalid config")
)

// FillForTesting applies defaults (test seam; production paths call fill
// internally inside New).
func (c *Config) FillForTesting() { c.fill() }

// validate rejects configurations that would fail later in confusing ways.
// Defaults are applied by fill() before validation.
func (c *Config) validate() error {
	if c.Session == nil {
		return fmt.Errorf("%w: Session is required", ErrInvalidConfig)
	}
	if c.Host == "" {
		return fmt.Errorf("%w: Host resolved empty", ErrInvalidConfig)
	}
	// APIID/APIHash: zero/empty means "use the reference default" (fill
	// applies it) — reject only actively invalid values.
	if c.APIID < 0 {
		return fmt.Errorf("%w: APIID must be >= 0", ErrInvalidConfig)
	}
	if c.UpdateWorkers < 0 {
		return fmt.Errorf("%w: UpdateWorkers must be >= 0 (0 = default)", ErrInvalidConfig)
	}
	if c.UpdateQueue < 0 {
		return fmt.Errorf("%w: UpdateQueue must be >= 0 (0 = default)", ErrInvalidConfig)
	}
	if c.MaxPending < 0 {
		return fmt.Errorf("%w: MaxPending must be >= 0 (0 = default)", ErrInvalidConfig)
	}
	if c.Retries < 0 {
		return fmt.Errorf("%w: Retries must be >= 0", ErrInvalidConfig)
	}
	if c.RequestTimeout < 0 {
		return fmt.Errorf("%w: RequestTimeout must be >= 0 (negative = unbounded is not supported; use a context)", ErrInvalidConfig)
	}
	if c.FloodWaitThreshold < 0 {
		return fmt.Errorf("%w: FloodWaitThreshold must be >= 0", ErrInvalidConfig)
	}
	return nil
}

var _ = time.Second

// entityCacheSizeOrDefault resolves the configured cache capacity.
func (c *Config) entityCacheSizeOrDefault() int {
	if c.EntityCacheSize <= 0 {
		return 4096
	}
	return c.EntityCacheSize
}
