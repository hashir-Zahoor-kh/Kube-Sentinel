// Package metrics defines and exposes all Prometheus metrics for Kube-Sentinel.
//
// Why a custom registry instead of the default global one?
// The default prometheus.DefaultRegisterer is shared across the whole process.
// By using prometheus.NewRegistry() we keep our metrics isolated — no
// accidental name collisions, and tests get a clean slate on every run.
//
// How Prometheus metrics work (short version):
//   - Counter:   monotonically increasing number (e.g., total crashes seen).
//                Never goes down. Use it for "how many times did X happen?"
//   - Histogram: records observations in configurable buckets AND tracks a
//                running sum and count. Use it for latency / duration.
//   - CounterVec / HistogramVec: same as above but labeled. Labels slice one
//                metric into multiple time-series (e.g., one per namespace).
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Action label values used by RemediationsTotal and RemediationDuration.
// Defining them as constants prevents typos at call sites.
const (
	ActionMemoryPatch   = "memory_patch"     // OOMKilled: patched limit + restarted
	ActionPodRestart    = "pod_restart"      // AppCrash or Unknown: deleted pod to restart
	ActionRollback      = "rollback"         // AppCrash: rolled back to stable ReplicaSet
	ActionAlertOnly     = "alert_only"       // ConfigError or repeated Unknown: no auto-fix
)

// Recorder holds every Prometheus metric Kube-Sentinel exposes.
//
// Fields are exported so tests can call testutil.ToFloat64 directly on
// individual counters without needing a public getter method per field.
type Recorder struct {
	// CrashesDetected counts every CrashLoopBackOff event we detect.
	// Labels: namespace (where the pod lives), failure_type (OOMKilled etc.).
	CrashesDetected *prometheus.CounterVec

	// RemediationsTotal counts every remediation action we take.
	// Label: action — one of the Action* constants above.
	RemediationsTotal *prometheus.CounterVec

	// RollbacksTotal counts deployment rollbacks specifically.
	// This is a subset of RemediationsTotal{action="rollback"}, kept separate
	// because rollbacks are high-severity events worth their own alert rule.
	RollbacksTotal prometheus.Counter

	// RemediationDuration tracks how long each remediation takes in seconds.
	// Label: failure_type — lets you compare OOMKilled vs AppCrash latency.
	RemediationDuration *prometheus.HistogramVec
}

// New creates a Recorder and registers all metrics with reg.
// For production pass prometheus.NewRegistry() (or DefaultRegisterer).
// For tests pass prometheus.NewRegistry() so each test gets a clean slate.
func New(reg prometheus.Registerer) *Recorder {
	r := &Recorder{
		CrashesDetected: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "kubesentinel_crashes_detected_total",
				Help: "Total number of CrashLoopBackOff pods detected, partitioned by namespace and failure type.",
			},
			// Label names — must match the label values passed to WithLabelValues().
			[]string{"namespace", "failure_type"},
		),

		RemediationsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "kubesentinel_remediations_total",
				Help: "Total number of remediation actions taken, partitioned by action type.",
			},
			[]string{"action"},
		),

		RollbacksTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "kubesentinel_rollbacks_total",
				Help: "Total number of Deployment rollbacks performed by Kube-Sentinel.",
			},
		),

		// Histogram buckets cover 10ms → 10s.
		// Most pod-delete operations complete in <100ms; log fetches and
		// Deployment patches may take 1–5s under cluster load.
		RemediationDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "kubesentinel_remediation_duration_seconds",
				Help:    "Duration of remediation actions in seconds, partitioned by failure type.",
				Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0},
			},
			[]string{"failure_type"},
		),
	}

	// MustRegister panics if a metric with the same name is already registered.
	// This catches double-registration bugs at startup rather than silently
	// producing wrong data at runtime.
	reg.MustRegister(
		r.CrashesDetected,
		r.RemediationsTotal,
		r.RollbacksTotal,
		r.RemediationDuration,
	)

	return r
}

// RecordCrashDetected increments kubesentinel_crashes_detected_total for the
// given namespace and failure type. Call this once per detected CrashLoopBackOff
// event, after the classifier has determined the failure type.
func (r *Recorder) RecordCrashDetected(namespace, failureType string) {
	// WithLabelValues returns (or creates) the counter for this label combination.
	r.CrashesDetected.WithLabelValues(namespace, failureType).Inc()
}

// RecordRemediation increments kubesentinel_remediations_total for the given
// action. Use one of the Action* constants as the action string.
func (r *Recorder) RecordRemediation(action string) {
	r.RemediationsTotal.WithLabelValues(action).Inc()
}

// RecordRollback increments both kubesentinel_rollbacks_total (the dedicated
// rollback counter) and kubesentinel_remediations_total{action="rollback"}.
func (r *Recorder) RecordRollback() {
	r.RollbacksTotal.Inc()
	r.RemediationsTotal.WithLabelValues(ActionRollback).Inc()
}

// ObserveRemediationDuration records a remediation duration observation in
// kubesentinel_remediation_duration_seconds. The failureType label should
// match the classifier.FailureType string (e.g., "OOMKilled").
func (r *Recorder) ObserveRemediationDuration(failureType string, seconds float64) {
	r.RemediationDuration.WithLabelValues(failureType).Observe(seconds)
}
