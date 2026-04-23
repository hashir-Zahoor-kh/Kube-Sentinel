// Package alerter fires structured JSON critical alerts to an io.Writer
// (stdout in production) when a pod crashes more than AlertThreshold times
// within a rolling CrashWindowMinutes window.
//
// Why a sliding window instead of a simple counter?
// A simple counter never resets — a pod that crashed 4 times six months ago
// and is now perfectly healthy would still be "above threshold". A sliding
// window gives a fair picture of the pod's *current* stability.
//
// Why write JSON directly instead of using zap?
// Alerts are a machine-readable signal, not a developer log. Writing one JSON
// object per line makes it trivial for any log aggregator (Loki, Splunk,
// Datadog) to parse and route them as alerts without custom grok patterns.
package alerter

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"

	"github.com/hashir/kube-sentinel/internal/classifier"
	"github.com/hashir/kube-sentinel/internal/config"
)

// Alert is the JSON structure written to the output when a threshold is
// exceeded. Every field is exported so encoding/json can serialize it.
type Alert struct {
	// Level is always "CRITICAL" — a fixed string that lets log routers
	// match this line with a simple grep.
	Level string `json:"level"`

	// Timestamp is RFC3339 UTC — parseable by every log system.
	Timestamp string `json:"timestamp"`

	// Alert is a machine-readable event name.
	Alert string `json:"alert"`

	// Pod and Namespace identify which workload is crashing.
	Pod       string `json:"pod"`
	Namespace string `json:"namespace"`

	// FailureType is one of OOMKilled / ConfigError / AppCrash / Unknown.
	FailureType string `json:"failure_type"`

	// ExitCode is the last container exit code.
	ExitCode int32 `json:"exit_code"`

	// CrashCount is how many crashes occurred in the window that triggered
	// this alert.
	CrashCount int `json:"crash_count"`

	// WindowMinutes is the length of the sliding window.
	WindowMinutes int `json:"window_minutes"`

	// Threshold is the configured alert_threshold.
	Threshold int `json:"threshold"`

	// RemediationHistory is the list of actions Kube-Sentinel has already
	// taken for this pod (e.g., ["pod_restart", "pod_restart", "rollback"]).
	RemediationHistory []string `json:"remediation_history"`
}

// Option is a functional option for tuning Alerter behaviour.
// The pattern lets callers override specific fields without needing a large
// constructor argument list.
type Option func(*Alerter)

// WithNow overrides the clock function used to stamp crash events.
// Pass a fixed or manually-advanced clock in tests to exercise the
// window-expiry logic without real sleeping.
func WithNow(fn func() time.Time) Option {
	return func(a *Alerter) { a.nowFn = fn }
}

// Alerter tracks crash events per pod and fires structured JSON alerts when
// the crash count within the sliding window exceeds the configured threshold.
type Alerter struct {
	cfg    *config.Config
	logger *zap.Logger

	// out is where alert JSON lines are written.
	// In production this is os.Stdout; in tests it is a *bytes.Buffer.
	out io.Writer

	// nowFn returns the current time. Overrideable for testing.
	nowFn func() time.Time

	// mu protects all maps below from concurrent access.
	// The podHandler goroutines call RecordCrash concurrently.
	mu sync.Mutex

	// crashHistory maps "namespace/name" to the timestamps of recent crashes
	// that fall within the current sliding window.
	crashHistory map[string][]time.Time

	// remHistory maps "namespace/name" to the list of remediation actions
	// Kube-Sentinel has taken, in chronological order.
	remHistory map[string][]string
}

// New creates an Alerter. Alerts are written to out as newline-delimited JSON.
// Apply Option functions to customise behaviour (e.g., WithNow for tests).
func New(cfg *config.Config, logger *zap.Logger, out io.Writer, opts ...Option) *Alerter {
	a := &Alerter{
		cfg:          cfg,
		logger:       logger,
		out:          out,
		nowFn:        time.Now,
		crashHistory: make(map[string][]time.Time),
		remHistory:   make(map[string][]string),
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// RecordCrash registers one crash event for the pod, prunes events older
// than the configured window, and fires a critical alert if the resulting
// count exceeds AlertThreshold.
//
// This is safe to call concurrently from multiple goroutines.
func (a *Alerter) RecordCrash(pod *corev1.Pod, result *classifier.Result) {
	key := podKey(pod)
	now := a.nowFn()
	cutoff := now.Add(-time.Duration(a.cfg.CrashWindowMinutes) * time.Minute)

	a.mu.Lock()
	defer a.mu.Unlock()

	// Append the current crash timestamp.
	a.crashHistory[key] = append(a.crashHistory[key], now)

	// Prune timestamps that have slid outside the window.
	// We build a new slice from the existing one, keeping only recent entries.
	var recent []time.Time
	for _, t := range a.crashHistory[key] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	a.crashHistory[key] = recent

	// Fire a critical alert if the in-window crash count exceeds the threshold.
	// We use strictly-greater-than so exactly AlertThreshold crashes is OK;
	// the (threshold+1)th crash is the one that fires.
	if len(recent) > a.cfg.AlertThreshold {
		a.fireAlert(pod, result, len(recent))
	}
}

// RecordRemediation appends an action string to the pod's remediation history.
// This is called by the remediator each time it performs an action, so that
// subsequent alerts can include a full timeline of what was tried.
func (a *Alerter) RecordRemediation(pod *corev1.Pod, action string) {
	key := podKey(pod)

	a.mu.Lock()
	defer a.mu.Unlock()

	a.remHistory[key] = append(a.remHistory[key], action)

	// Cap the history at 50 entries to bound memory growth for long-running pods.
	const maxHistory = 50
	if len(a.remHistory[key]) > maxHistory {
		a.remHistory[key] = a.remHistory[key][len(a.remHistory[key])-maxHistory:]
	}
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// fireAlert serialises an Alert as a single JSON line and writes it to a.out.
// It also logs at ERROR level through zap so the alert appears in the
// structured log stream as well.
//
// Must be called with a.mu held.
func (a *Alerter) fireAlert(pod *corev1.Pod, result *classifier.Result, crashCount int) {
	key := podKey(pod)

	// Defensive copy of remediation history so we can release the lock promptly.
	history := make([]string, len(a.remHistory[key]))
	copy(history, a.remHistory[key])

	payload := Alert{
		Level:              "CRITICAL",
		Timestamp:          a.nowFn().UTC().Format(time.RFC3339),
		Alert:              "crash_threshold_exceeded",
		Pod:                pod.Name,
		Namespace:          pod.Namespace,
		FailureType:        string(result.FailureType),
		ExitCode:           result.ExitCode,
		CrashCount:         crashCount,
		WindowMinutes:      a.cfg.CrashWindowMinutes,
		Threshold:          a.cfg.AlertThreshold,
		RemediationHistory: history,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		a.logger.Error("Failed to marshal alert payload", zap.Error(err))
		return
	}

	// Write one JSON object per line. Line-delimited JSON is the de-facto
	// standard for machine-parseable log streams.
	if _, err := fmt.Fprintf(a.out, "%s\n", data); err != nil {
		a.logger.Error("Failed to write alert to output", zap.Error(err))
	}

	// Also log through zap so the alert shows up in the operator's log view
	// alongside the standard structured log stream.
	a.logger.Error("CRITICAL ALERT: pod crash threshold exceeded",
		zap.String("pod", pod.Name),
		zap.String("namespace", pod.Namespace),
		zap.String("failure_type", string(result.FailureType)),
		zap.Int32("exit_code", result.ExitCode),
		zap.Int("crash_count", crashCount),
		zap.Int("threshold", a.cfg.AlertThreshold),
		zap.Int("window_minutes", a.cfg.CrashWindowMinutes),
	)
}

// podKey returns the map key used to bucket crash and remediation history.
func podKey(pod *corev1.Pod) string {
	return pod.Namespace + "/" + pod.Name
}
