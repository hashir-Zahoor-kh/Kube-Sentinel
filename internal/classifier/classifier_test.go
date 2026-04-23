// Tests for the classifier package.
// All tests use a fakeLogReader instead of a real Kubernetes cluster,
// so they run instantly and work fully offline.
package classifier_test

import (
	"context"
	"fmt"
	"testing"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/hashir/kube-sentinel/internal/classifier"
)

// fakeLogReader implements classifier.LogReader and returns preset log text.
// This lets us test the classifier's keyword logic without a real cluster.
type fakeLogReader struct {
	// logs maps "namespace/pod/container" → log text to return.
	logs map[string]string
	// err, if non-nil, is returned for every GetLogs call.
	err error
}

// GetLogs implements classifier.LogReader.
func (f *fakeLogReader) GetLogs(_ context.Context, namespace, podName, containerName string, _ int64) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	key := namespace + "/" + podName + "/" + containerName
	return f.logs[key], nil
}

// makeCrashPod constructs a minimal Pod that appears to be in CrashLoopBackOff
// with the given exit code and reason in its LastTerminationState.
func makeCrashPod(name, ns, container string, exitCode int32, reason string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: container,
					// Current state: waiting (CrashLoopBackOff)
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: "CrashLoopBackOff",
						},
					},
					// Last termination: this is where exit code lives
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode: exitCode,
							Reason:   reason,
						},
					},
				},
			},
		},
	}
}

// newTestClassifier creates a Classifier wired to a fakeLogReader.
// We use zap.NewNop() to suppress log output during tests.
func newTestClassifier(logs map[string]string) *classifier.Classifier {
	reader := &fakeLogReader{logs: logs}
	return classifier.New(zap.NewNop(), reader)
}

// --- Tests ---

// TestClassify_OOMKilled verifies that exit code 137 maps to OOMKilled
// regardless of log content.
func TestClassify_OOMKilled(t *testing.T) {
	pod := makeCrashPod("oom-pod", "default", "app", 137, "OOMKilled")

	c := newTestClassifier(nil) // no logs needed for OOMKilled
	result, err := c.Classify(context.Background(), pod)
	if err != nil {
		t.Fatalf("Classify() unexpected error: %v", err)
	}

	if result.FailureType != classifier.FailureOOMKilled {
		t.Errorf("FailureType = %q, want %q", result.FailureType, classifier.FailureOOMKilled)
	}
	if result.ExitCode != 137 {
		t.Errorf("ExitCode = %d, want 137", result.ExitCode)
	}
}

// TestClassify_OOMKilled_ByReason verifies that the "OOMKilled" reason string
// alone (even with a non-137 exit code) is classified as OOMKilled.
func TestClassify_OOMKilled_ByReason(t *testing.T) {
	pod := makeCrashPod("oom-pod", "default", "app", 1, "OOMKilled")

	c := newTestClassifier(nil)
	result, err := c.Classify(context.Background(), pod)
	if err != nil {
		t.Fatalf("Classify() unexpected error: %v", err)
	}
	if result.FailureType != classifier.FailureOOMKilled {
		t.Errorf("FailureType = %q, want OOMKilled", result.FailureType)
	}
}

// TestClassify_ConfigError verifies that exit 1 + config-related log keywords
// are classified as ConfigError.
func TestClassify_ConfigError(t *testing.T) {
	pod := makeCrashPod("cfg-pod", "default", "app", 1, "Error")

	logs := map[string]string{
		"default/cfg-pod/app": "Starting server...\nFailed to load config: no such file or directory: /etc/app/config.yaml\n",
	}
	c := newTestClassifier(logs)

	result, err := c.Classify(context.Background(), pod)
	if err != nil {
		t.Fatalf("Classify() unexpected error: %v", err)
	}
	if result.FailureType != classifier.FailureConfigError {
		t.Errorf("FailureType = %q, want ConfigError", result.FailureType)
	}
}

// TestClassify_AppCrash verifies that exit 1 + "panic" in logs → AppCrash.
func TestClassify_AppCrash(t *testing.T) {
	pod := makeCrashPod("crash-pod", "default", "app", 1, "Error")

	logs := map[string]string{
		"default/crash-pod/app": "INFO starting\npanic: runtime error: index out of range [5] with length 3\ngoroutine 1 [running]:\nmain.main()\n",
	}
	c := newTestClassifier(logs)

	result, err := c.Classify(context.Background(), pod)
	if err != nil {
		t.Fatalf("Classify() unexpected error: %v", err)
	}
	if result.FailureType != classifier.FailureAppCrash {
		t.Errorf("FailureType = %q, want AppCrash", result.FailureType)
	}
}

// TestClassify_Unknown verifies that exit 1 + unrecognised logs → Unknown.
func TestClassify_Unknown(t *testing.T) {
	pod := makeCrashPod("unknown-pod", "default", "app", 1, "Error")

	logs := map[string]string{
		"default/unknown-pod/app": "some ambiguous output that doesn't match any keyword\n",
	}
	c := newTestClassifier(logs)

	result, err := c.Classify(context.Background(), pod)
	if err != nil {
		t.Fatalf("Classify() unexpected error: %v", err)
	}
	if result.FailureType != classifier.FailureUnknown {
		t.Errorf("FailureType = %q, want Unknown", result.FailureType)
	}
}

// TestClassify_LogFetchError verifies that when log fetching fails the
// classifier still returns a result (Unknown) instead of an error.
func TestClassify_LogFetchError(t *testing.T) {
	pod := makeCrashPod("log-fail-pod", "default", "app", 1, "Error")

	reader := &fakeLogReader{err: fmt.Errorf("connection refused")}
	c := classifier.New(zap.NewNop(), reader)

	result, err := c.Classify(context.Background(), pod)
	if err != nil {
		t.Fatalf("Classify() should not return an error when log fetch fails, got: %v", err)
	}
	if result.FailureType != classifier.FailureUnknown {
		t.Errorf("FailureType = %q, want Unknown when logs unavailable", result.FailureType)
	}
}

// TestClassify_ConfigError_CaseInsensitive checks that keyword matching is
// case-insensitive (logs may use "No Such File" or "NO SUCH FILE").
func TestClassify_ConfigError_CaseInsensitive(t *testing.T) {
	pod := makeCrashPod("cfg-pod", "default", "app", 1, "Error")

	logs := map[string]string{
		"default/cfg-pod/app": "ERROR: PERMISSION DENIED reading /etc/secrets/token\n",
	}
	c := newTestClassifier(logs)

	result, err := c.Classify(context.Background(), pod)
	if err != nil {
		t.Fatalf("Classify() unexpected error: %v", err)
	}
	if result.FailureType != classifier.FailureConfigError {
		t.Errorf("FailureType = %q, want ConfigError for uppercase PERMISSION DENIED", result.FailureType)
	}
}

// TestClassify_InitContainerCrash verifies that a crashing init container
// is detected and classified correctly.
func TestClassify_InitContainerCrash(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "init-crash", Namespace: "default"},
		Status: corev1.PodStatus{
			// Init container is crashing
			InitContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "migrate",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
					},
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 1,
							Reason:   "Error",
						},
					},
				},
			},
			// Main container is still waiting (never started)
			ContainerStatuses: []corev1.ContainerStatus{},
		},
	}

	logs := map[string]string{
		"default/init-crash/migrate": "Running migrations...\nFATAL: database connection refused\n",
	}
	c := newTestClassifier(logs)

	result, err := c.Classify(context.Background(), pod)
	if err != nil {
		t.Fatalf("Classify() unexpected error: %v", err)
	}
	if result.ContainerName != "migrate" {
		t.Errorf("ContainerName = %q, want %q", result.ContainerName, "migrate")
	}
	if result.FailureType != classifier.FailureAppCrash {
		t.Errorf("FailureType = %q, want AppCrash (fatal keyword)", result.FailureType)
	}
}
