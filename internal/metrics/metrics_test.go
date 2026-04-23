// Tests for the metrics package.
// Each test creates its own prometheus.NewRegistry() so metrics registered in
// one test don't bleed into another — an important property when counters are
// monotonically increasing and can't be reset.
package metrics_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/hashir/kube-sentinel/internal/metrics"
)

// newTestRecorder creates a Recorder backed by an isolated registry.
// Using prometheus.NewRegistry() (not prometheus.DefaultRegisterer) means
// every test starts with a fresh, empty metric state.
func newTestRecorder() *metrics.Recorder {
	return metrics.New(prometheus.NewRegistry())
}

// TestRecordCrashDetected verifies that the crashes_detected counter is
// incremented with the correct namespace and failure_type labels.
func TestRecordCrashDetected(t *testing.T) {
	rec := newTestRecorder()

	rec.RecordCrashDetected("default", "OOMKilled")
	rec.RecordCrashDetected("default", "OOMKilled")
	rec.RecordCrashDetected("kube-system", "AppCrash")

	// testutil.ToFloat64 gathers the current value from a single Collector.
	// Here we select the counter for a specific label combination.
	got := testutil.ToFloat64(
		rec.CrashesDetected.WithLabelValues("default", "OOMKilled"),
	)
	if got != 2 {
		t.Errorf("crashes_detected{namespace=default,failure_type=OOMKilled} = %v, want 2", got)
	}

	got = testutil.ToFloat64(
		rec.CrashesDetected.WithLabelValues("kube-system", "AppCrash"),
	)
	if got != 1 {
		t.Errorf("crashes_detected{namespace=kube-system,failure_type=AppCrash} = %v, want 1", got)
	}

	// A label combination we never recorded should be zero.
	got = testutil.ToFloat64(
		rec.CrashesDetected.WithLabelValues("default", "Unknown"),
	)
	if got != 0 {
		t.Errorf("crashes_detected{namespace=default,failure_type=Unknown} = %v, want 0", got)
	}
}

// TestRecordRemediation verifies the remediations_total counter with the
// action label.
func TestRecordRemediation(t *testing.T) {
	rec := newTestRecorder()

	rec.RecordRemediation(metrics.ActionPodRestart)
	rec.RecordRemediation(metrics.ActionPodRestart)
	rec.RecordRemediation(metrics.ActionMemoryPatch)

	got := testutil.ToFloat64(rec.RemediationsTotal.WithLabelValues(metrics.ActionPodRestart))
	if got != 2 {
		t.Errorf("remediations_total{action=pod_restart} = %v, want 2", got)
	}

	got = testutil.ToFloat64(rec.RemediationsTotal.WithLabelValues(metrics.ActionMemoryPatch))
	if got != 1 {
		t.Errorf("remediations_total{action=memory_patch} = %v, want 1", got)
	}
}

// TestRecordRollback verifies that RecordRollback increments both the
// dedicated rollbacks_total counter AND remediations_total{action="rollback"}.
func TestRecordRollback(t *testing.T) {
	rec := newTestRecorder()

	rec.RecordRollback()
	rec.RecordRollback()

	// Dedicated rollback counter.
	if got := testutil.ToFloat64(rec.RollbacksTotal); got != 2 {
		t.Errorf("rollbacks_total = %v, want 2", got)
	}

	// Rollbacks also show up in the remediations counter.
	got := testutil.ToFloat64(rec.RemediationsTotal.WithLabelValues(metrics.ActionRollback))
	if got != 2 {
		t.Errorf("remediations_total{action=rollback} = %v, want 2", got)
	}
}

// TestObserveRemediationDuration verifies that observations land in the
// histogram. We use testutil.CollectAndCount to confirm the histogram
// has metric series (one series per label set that has received at least
// one observation).
func TestObserveRemediationDuration(t *testing.T) {
	rec := newTestRecorder()

	rec.ObserveRemediationDuration("OOMKilled", 0.12)
	rec.ObserveRemediationDuration("OOMKilled", 0.35)
	rec.ObserveRemediationDuration("AppCrash", 1.5)

	// CollectAndCount counts the total number of metric samples collected.
	// A HistogramVec with 2 label values and 9 buckets produces:
	//   2 label sets × (9 bucket + 1 sum + 1 count) = 22 samples.
	count := testutil.CollectAndCount(rec.RemediationDuration)
	if count == 0 {
		t.Error("RemediationDuration has no metric samples after observations")
	}
}

// TestZeroBeforeRecording verifies that counters start at zero — no
// implicit initialisation occurs on Recorder creation.
func TestZeroBeforeRecording(t *testing.T) {
	rec := newTestRecorder()

	if got := testutil.ToFloat64(rec.RollbacksTotal); got != 0 {
		t.Errorf("rollbacks_total before any recording = %v, want 0", got)
	}
}

// TestIsolatedRegistries verifies that two Recorders created with different
// registries do not share metric state — a guard against test pollution when
// using the global default registry.
func TestIsolatedRegistries(t *testing.T) {
	rec1 := newTestRecorder()
	rec2 := newTestRecorder()

	rec1.RecordRollback()
	rec1.RecordRollback()
	rec1.RecordRollback()

	// rec2 was created from a fresh registry — its counter must be zero.
	if got := testutil.ToFloat64(rec2.RollbacksTotal); got != 0 {
		t.Errorf("isolated registry: rollbacks_total = %v, want 0 (leaked from rec1)", got)
	}
}
