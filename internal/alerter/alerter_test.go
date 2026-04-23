// Tests for the alerter package.
// The controllable clock (WithNow option) is the key testing tool here —
// it lets us verify the sliding-window expiry without real sleeping.
package alerter_test

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/hashir/kube-sentinel/internal/alerter"
	"github.com/hashir/kube-sentinel/internal/classifier"
	"github.com/hashir/kube-sentinel/internal/config"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// testCfg returns a Config with AlertThreshold=3 and a 10-minute window.
func testCfg() *config.Config {
	return &config.Config{
		AlertThreshold:     3,
		CrashWindowMinutes: 10,
	}
}

// makePod creates a minimal Pod for alerter testing.
func makePod(name, ns string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
	}
}

// oomResult is a ready-made classifier.Result for OOMKilled.
func oomResult() *classifier.Result {
	return &classifier.Result{
		FailureType: classifier.FailureOOMKilled,
		ExitCode:    137,
	}
}

// appCrashResult is a ready-made classifier.Result for AppCrash.
func appCrashResult() *classifier.Result {
	return &classifier.Result{
		FailureType: classifier.FailureAppCrash,
		ExitCode:    1,
	}
}

// decodeAlerts parses zero or more newline-delimited JSON alert objects from b.
func decodeAlerts(t *testing.T, b *bytes.Buffer) []alerter.Alert {
	t.Helper()
	var alerts []alerter.Alert
	dec := json.NewDecoder(b)
	for dec.More() {
		var a alerter.Alert
		if err := dec.Decode(&a); err != nil {
			t.Fatalf("decoding alert JSON: %v", err)
		}
		alerts = append(alerts, a)
	}
	return alerts
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestRecordCrash_BelowThreshold verifies that exactly AlertThreshold crashes
// do NOT fire an alert — the (threshold+1)th crash is what triggers it.
func TestRecordCrash_BelowThreshold(t *testing.T) {
	var buf bytes.Buffer
	a := alerter.New(testCfg(), zap.NewNop(), &buf)
	pod := makePod("my-pod", "default")
	result := appCrashResult()

	// Record exactly AlertThreshold (3) crashes.
	for i := 0; i < 3; i++ {
		a.RecordCrash(pod, result)
	}

	if buf.Len() > 0 {
		t.Errorf("expected no alert for %d crashes (threshold is 3), got: %s", 3, buf.String())
	}
}

// TestRecordCrash_ExceedsThreshold verifies that the 4th crash (threshold+1)
// fires a critical alert.
func TestRecordCrash_ExceedsThreshold(t *testing.T) {
	var buf bytes.Buffer
	a := alerter.New(testCfg(), zap.NewNop(), &buf)
	pod := makePod("my-pod", "default")
	result := appCrashResult()

	// Record AlertThreshold+1 = 4 crashes.
	for i := 0; i <= 3; i++ {
		a.RecordCrash(pod, result)
	}

	alerts := decodeAlerts(t, &buf)
	if len(alerts) == 0 {
		t.Fatal("expected at least one alert after exceeding threshold, got none")
	}

	got := alerts[0]
	if got.Level != "CRITICAL" {
		t.Errorf("Level = %q, want CRITICAL", got.Level)
	}
	if got.Alert != "crash_threshold_exceeded" {
		t.Errorf("Alert = %q, want crash_threshold_exceeded", got.Alert)
	}
	if got.Pod != "my-pod" {
		t.Errorf("Pod = %q, want my-pod", got.Pod)
	}
	if got.Namespace != "default" {
		t.Errorf("Namespace = %q, want default", got.Namespace)
	}
	if got.FailureType != "AppCrash" {
		t.Errorf("FailureType = %q, want AppCrash", got.FailureType)
	}
	if got.CrashCount < 4 {
		t.Errorf("CrashCount = %d, want >= 4", got.CrashCount)
	}
}

// TestRecordCrash_WindowExpiry verifies that crashes outside the time window
// are pruned and do not contribute to the threshold check.
func TestRecordCrash_WindowExpiry(t *testing.T) {
	var buf bytes.Buffer

	// Controllable clock — starts at a fixed point in the past.
	now := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	a := alerter.New(testCfg(), zap.NewNop(), &buf, alerter.WithNow(func() time.Time { return now }))

	pod := makePod("my-pod", "default")
	result := appCrashResult()

	// Record 3 crashes at t=0 (within window but at threshold exactly — no alert).
	for i := 0; i < 3; i++ {
		a.RecordCrash(pod, result)
	}
	if buf.Len() > 0 {
		t.Fatal("alert fired unexpectedly before advancing the clock")
	}

	// Advance clock past the 10-minute window.
	// The 3 old crashes are now outside the window and will be pruned.
	now = now.Add(11 * time.Minute)

	// Record 3 more crashes. Old ones are pruned so the count resets to 3 —
	// still at (not above) the threshold, so no alert should fire.
	for i := 0; i < 3; i++ {
		a.RecordCrash(pod, result)
	}

	if buf.Len() > 0 {
		t.Errorf("alert fired after window expiry — old crashes should have been pruned: %s", buf.String())
	}
}

// TestRecordCrash_MultiplePodsIsolated verifies that crash counts for
// different pods do not bleed into each other.
func TestRecordCrash_MultiplePodsIsolated(t *testing.T) {
	var buf bytes.Buffer
	a := alerter.New(testCfg(), zap.NewNop(), &buf)
	result := appCrashResult()

	podA := makePod("pod-a", "default")
	podB := makePod("pod-b", "default")

	// Give pod-a 4 crashes (exceeds threshold).
	for i := 0; i <= 3; i++ {
		a.RecordCrash(podA, result)
	}

	// Give pod-b only 1 crash (below threshold).
	a.RecordCrash(podB, result)

	alerts := decodeAlerts(t, &buf)
	if len(alerts) == 0 {
		t.Fatal("expected alert for pod-a, got none")
	}
	// All alerts must be for pod-a, not pod-b.
	for _, al := range alerts {
		if al.Pod != "pod-a" {
			t.Errorf("alert pod = %q, want pod-a (pod-b should not have alerted)", al.Pod)
		}
	}
}

// TestRecordRemediation_AppearsInAlert verifies that remediation actions
// recorded before an alert fires appear in the alert's remediation_history.
func TestRecordRemediation_AppearsInAlert(t *testing.T) {
	var buf bytes.Buffer
	a := alerter.New(testCfg(), zap.NewNop(), &buf)
	pod := makePod("my-pod", "default")
	result := appCrashResult()

	// Record some remediation actions taken before the alert threshold.
	a.RecordRemediation(pod, "pod_restart")
	a.RecordRemediation(pod, "pod_restart")
	a.RecordRemediation(pod, "deployment_rollback")

	// Exceed the threshold to trigger an alert.
	for i := 0; i <= 3; i++ {
		a.RecordCrash(pod, result)
	}

	alerts := decodeAlerts(t, &buf)
	if len(alerts) == 0 {
		t.Fatal("expected an alert")
	}

	history := alerts[0].RemediationHistory
	if len(history) != 3 {
		t.Fatalf("RemediationHistory len = %d, want 3; got: %v", len(history), history)
	}
	if history[0] != "pod_restart" {
		t.Errorf("history[0] = %q, want pod_restart", history[0])
	}
	if history[2] != "deployment_rollback" {
		t.Errorf("history[2] = %q, want deployment_rollback", history[2])
	}
}

// TestAlert_JSONFields verifies the Alert struct has all required fields and
// that they round-trip cleanly through JSON serialisation.
func TestAlert_JSONFields(t *testing.T) {
	var buf bytes.Buffer
	a := alerter.New(testCfg(), zap.NewNop(), &buf)
	pod := makePod("web-app", "production")
	result := &classifier.Result{
		FailureType: classifier.FailureOOMKilled,
		ExitCode:    137,
	}

	for i := 0; i <= 3; i++ {
		a.RecordCrash(pod, result)
	}

	alerts := decodeAlerts(t, &buf)
	if len(alerts) == 0 {
		t.Fatal("expected an alert")
	}
	al := alerts[0]

	// Validate every required field is non-zero.
	checks := map[string]bool{
		"Level":         al.Level != "",
		"Timestamp":     al.Timestamp != "",
		"Alert":         al.Alert != "",
		"Pod":           al.Pod == "web-app",
		"Namespace":     al.Namespace == "production",
		"FailureType":   al.FailureType == "OOMKilled",
		"CrashCount":    al.CrashCount > 0,
		"WindowMinutes": al.WindowMinutes == 10,
		"Threshold":     al.Threshold == 3,
	}
	for field, ok := range checks {
		if !ok {
			t.Errorf("alert field %q has wrong or zero value in: %+v", field, al)
		}
	}

	// Verify the timestamp is valid RFC3339.
	if _, err := time.Parse(time.RFC3339, al.Timestamp); err != nil {
		t.Errorf("Timestamp %q is not valid RFC3339: %v", al.Timestamp, err)
	}
}

// TestRecordCrash_OOMKilledAlert verifies OOMKilled failures can also
// trigger alerts (it's not only AppCrash).
func TestRecordCrash_OOMKilledAlert(t *testing.T) {
	var buf bytes.Buffer
	a := alerter.New(testCfg(), zap.NewNop(), &buf)
	pod := makePod("mem-hungry", "default")
	result := oomResult()

	for i := 0; i <= 3; i++ {
		a.RecordCrash(pod, result)
	}

	alerts := decodeAlerts(t, &buf)
	if len(alerts) == 0 {
		t.Fatal("expected alert for OOMKilled threshold breach")
	}
	if alerts[0].FailureType != "OOMKilled" {
		t.Errorf("FailureType = %q, want OOMKilled", alerts[0].FailureType)
	}
}
