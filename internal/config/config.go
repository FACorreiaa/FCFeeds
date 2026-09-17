// Package config reads the service configuration from the environment.
package config

import (
	"fmt"
	"strconv"
	"time"
)

// Config is everything main needs to wire the service.
type Config struct {
	Addr            string
	DBPath          string
	UserAgent       string
	MinPoll         time.Duration
	Workers         int
	MaxBodyBytes    int64
	FetchTimeout    time.Duration
	EvictAfter      time.Duration
	ColdFetchBudget time.Duration
}

// Load reads FEEDS_* variables via getenv (so tests can inject) and applies
// defaults and validation.
func Load(getenv func(string) string) (Config, error) {
	c := Config{
		Addr:            ":8080",
		DBPath:          "/data/feeds.db",
		UserAgent:       "feeds/1.0 (+https://github.com/FACorreiaa/feeds; shared aggregator)",
		MinPoll:         5 * time.Minute,
		Workers:         4,
		MaxBodyBytes:    5 << 20,
		FetchTimeout:    15 * time.Second,
		EvictAfter:      14 * 24 * time.Hour,
		ColdFetchBudget: 8 * time.Second,
	}
	var err error
	if v := getenv("FEEDS_ADDR"); v != "" {
		c.Addr = v
	}
	if v := getenv("FEEDS_DB_PATH"); v != "" {
		c.DBPath = v
	}
	if v := getenv("FEEDS_USER_AGENT"); v != "" {
		c.UserAgent = v
	}
	if c.MinPoll, err = dur(getenv, "FEEDS_MIN_POLL", c.MinPoll); err != nil {
		return c, err
	}
	if c.Workers, err = integer(getenv, "FEEDS_WORKERS", c.Workers); err != nil {
		return c, err
	}
	if v := getenv("FEEDS_MAX_BODY_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return c, fmt.Errorf("FEEDS_MAX_BODY_BYTES: %q is not a positive integer", v)
		}
		c.MaxBodyBytes = n
	}
	if c.FetchTimeout, err = dur(getenv, "FEEDS_FETCH_TIMEOUT", c.FetchTimeout); err != nil {
		return c, err
	}
	if c.EvictAfter, err = dur(getenv, "FEEDS_EVICT_AFTER", c.EvictAfter); err != nil {
		return c, err
	}
	if c.ColdFetchBudget, err = dur(getenv, "FEEDS_COLD_FETCH_BUDGET", c.ColdFetchBudget); err != nil {
		return c, err
	}
	if c.Workers < 1 {
		return c, fmt.Errorf("FEEDS_WORKERS must be at least 1")
	}
	if c.MinPoll < time.Minute {
		return c, fmt.Errorf("FEEDS_MIN_POLL must be at least 1m; publishers are not ours to hammer")
	}
	return c, nil
}

func dur(getenv func(string) string, key string, def time.Duration) (time.Duration, error) {
	v := getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}

func integer(getenv func(string) string, key string, def int) (int, error) {
	v := getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}
