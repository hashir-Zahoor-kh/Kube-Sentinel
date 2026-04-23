// Tests for the config package.
// We use the _test suffix on the package name ("config_test") so the tests
// can only access exported symbols — the same view an external caller has.
package config_test

import (
	"os"
	"testing"

	"github.com/hashir/kube-sentinel/internal/config"
)

// writeTempConfig is a helper that writes content to a temporary YAML file
// and returns its path. The caller is responsible for removing it.
func writeTempConfig(t *testing.T, content string) string {
	t.Helper() // marks this as a helper so test failures show the caller's line

	f, err := os.CreateTemp("", "ks-config-*.yaml")
	if err != nil {
		t.Fatalf("creating temp file: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("writing temp file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing temp file: %v", err)
	}
	return f.Name()
}

// TestLoadDefaults verifies that fields absent from the YAML file fall back
// to the built-in defaults rather than Go zero values.
func TestLoadDefaults(t *testing.T) {
	// Specify only one field; everything else should use defaults.
	path := writeTempConfig(t, "max_restart_attempts: 5\n")
	defer os.Remove(path)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	// The field we set should match what we wrote.
	if cfg.MaxRestartAttempts != 5 {
		t.Errorf("MaxRestartAttempts = %d, want 5", cfg.MaxRestartAttempts)
	}
	// Fields not in the file should fall back to defaults.
	if cfg.MemoryLimitIncreasePercent != 25 {
		t.Errorf("MemoryLimitIncreasePercent = %d, want default 25", cfg.MemoryLimitIncreasePercent)
	}
	if cfg.CrashWindowMinutes != 10 {
		t.Errorf("CrashWindowMinutes = %d, want default 10", cfg.CrashWindowMinutes)
	}
	if cfg.AlertThreshold != 3 {
		t.Errorf("AlertThreshold = %d, want default 3", cfg.AlertThreshold)
	}
}

// TestLoadFullConfig verifies that a fully-specified YAML file is parsed
// correctly and all fields are populated.
func TestLoadFullConfig(t *testing.T) {
	content := `
max_restart_attempts: 2
memory_limit_increase_percent: 50
crash_window_minutes: 5
alert_threshold: 2
namespaces_to_watch:
  - default
  - monitoring
`
	path := writeTempConfig(t, content)
	defer os.Remove(path)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	if cfg.MaxRestartAttempts != 2 {
		t.Errorf("MaxRestartAttempts = %d, want 2", cfg.MaxRestartAttempts)
	}
	if cfg.MemoryLimitIncreasePercent != 50 {
		t.Errorf("MemoryLimitIncreasePercent = %d, want 50", cfg.MemoryLimitIncreasePercent)
	}
	if cfg.CrashWindowMinutes != 5 {
		t.Errorf("CrashWindowMinutes = %d, want 5", cfg.CrashWindowMinutes)
	}
	if cfg.AlertThreshold != 2 {
		t.Errorf("AlertThreshold = %d, want 2", cfg.AlertThreshold)
	}
	if len(cfg.NamespacesToWatch) != 2 {
		t.Fatalf("NamespacesToWatch len = %d, want 2", len(cfg.NamespacesToWatch))
	}
	if cfg.NamespacesToWatch[0] != "default" {
		t.Errorf("NamespacesToWatch[0] = %q, want %q", cfg.NamespacesToWatch[0], "default")
	}
}

// TestLoadMissingFile verifies that Load returns a descriptive error when
// the config file does not exist.
func TestLoadMissingFile(t *testing.T) {
	_, err := config.Load("/nonexistent/path/kube-sentinel-config.yaml")
	if err == nil {
		t.Error("Load() expected an error for a missing file, got nil")
	}
}

// TestLoadInvalidYAML verifies that malformed YAML returns a parse error.
func TestLoadInvalidYAML(t *testing.T) {
	path := writeTempConfig(t, "max_restart_attempts: [not an int\n")
	defer os.Remove(path)

	_, err := config.Load(path)
	if err == nil {
		t.Error("Load() expected a parse error for invalid YAML, got nil")
	}
}
