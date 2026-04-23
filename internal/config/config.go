// Package config provides configuration loading for Kube-Sentinel.
// Configuration is read from a YAML file; any field not present in the file
// falls back to a built-in default value.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config holds all runtime configuration for Kube-Sentinel.
// Each field has a `yaml:"..."` struct tag that maps it to the
// corresponding key in the YAML file.
type Config struct {
	// MaxRestartAttempts is the number of times to restart a crashing pod
	// before escalating to a rollback (AppCrash) or giving up (Unknown).
	MaxRestartAttempts int `yaml:"max_restart_attempts"`

	// MemoryLimitIncreasePercent is how much to increase the memory limit
	// for OOMKilled pods. 25 means "add 25% on top of the current limit".
	MemoryLimitIncreasePercent int `yaml:"memory_limit_increase_percent"`

	// CrashWindowMinutes is the rolling time window used to count crashes
	// for alerting. If a pod crashes more than AlertThreshold times within
	// this window, a critical alert fires.
	CrashWindowMinutes int `yaml:"crash_window_minutes"`

	// AlertThreshold is how many crashes within CrashWindowMinutes trigger
	// a critical structured-JSON alert to stdout.
	AlertThreshold int `yaml:"alert_threshold"`

	// NamespacesToWatch is the list of namespaces Kube-Sentinel monitors.
	// An empty slice means "watch every namespace in the cluster".
	NamespacesToWatch []string `yaml:"namespaces_to_watch"`
}

// defaults returns a Config pre-populated with safe, sensible values.
// It is called before YAML unmarshalling so that any field omitted from
// the config file keeps its default rather than the Go zero value.
func defaults() *Config {
	return &Config{
		MaxRestartAttempts:         3,
		MemoryLimitIncreasePercent: 25,
		CrashWindowMinutes:         10,
		AlertThreshold:             3,
		NamespacesToWatch:          []string{},
	}
}

// Load reads the YAML file at path and returns a fully-populated Config.
// Fields absent from the file are filled with the values from defaults().
// Returns an error if the file cannot be read or contains invalid YAML.
func Load(path string) (*Config, error) {
	// Start from defaults so missing fields get reasonable values instead of
	// Go zero values (0, nil, "").
	cfg := defaults()

	// os.ReadFile reads the entire file into memory as a []byte.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file %q: %w", path, err)
	}

	// yaml.Unmarshal decodes the YAML bytes into the struct.
	// Because cfg already holds defaults, only the fields present in the
	// YAML will be overwritten — absent fields keep their default values.
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config file %q: %w", path, err)
	}

	return cfg, nil
}
