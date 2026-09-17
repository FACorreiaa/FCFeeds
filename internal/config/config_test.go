package config

import (
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	c, err := Load(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != ":8080" || c.DBPath != "/data/feeds.db" || c.MinPoll != 5*time.Minute || c.Workers != 4 ||
		c.MaxBodyBytes != 5<<20 || c.FetchTimeout != 15*time.Second || c.EvictAfter != 14*24*time.Hour ||
		c.ColdFetchBudget != 8*time.Second || c.UserAgent == "" {
		t.Errorf("defaults: %+v", c)
	}
}

func TestOverridesAndValidation(t *testing.T) {
	env := map[string]string{
		"FEEDS_ADDR": ":9999", "FEEDS_DB_PATH": "/tmp/x.db", "FEEDS_MIN_POLL": "10m", "FEEDS_WORKERS": "2",
		"FEEDS_MAX_BODY_BYTES": "1048576", "FEEDS_FETCH_TIMEOUT": "5s", "FEEDS_EVICT_AFTER": "48h",
		"FEEDS_USER_AGENT": "custom/1", "FEEDS_COLD_FETCH_BUDGET": "3s",
	}
	c, err := Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != ":9999" || c.DBPath != "/tmp/x.db" || c.MinPoll != 10*time.Minute || c.Workers != 2 ||
		c.MaxBodyBytes != 1<<20 || c.FetchTimeout != 5*time.Second || c.EvictAfter != 48*time.Hour ||
		c.UserAgent != "custom/1" || c.ColdFetchBudget != 3*time.Second {
		t.Errorf("overrides: %+v", c)
	}
	if _, err := Load(func(k string) string { return map[string]string{"FEEDS_MIN_POLL": "nope"}[k] }); err == nil {
		t.Error("bad duration should error")
	}
	if _, err := Load(func(k string) string { return map[string]string{"FEEDS_WORKERS": "0"}[k] }); err == nil {
		t.Error("zero workers should error")
	}
	if _, err := Load(func(k string) string { return map[string]string{"FEEDS_MIN_POLL": "30s"}[k] }); err == nil {
		t.Error("min poll under 1m should error (politeness floor)")
	}
}
