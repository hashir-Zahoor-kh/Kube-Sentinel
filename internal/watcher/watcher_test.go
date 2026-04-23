// Tests for the watcher package.
// These are pure unit tests — they construct Pod objects directly without
// hitting a real (or fake) Kubernetes API, so they run instantly offline.
package watcher_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/hashir/kube-sentinel/internal/watcher"
)

// makePod is a test helper that builds a minimal Pod object.
// ObjectMeta carries the name and namespace; ContainerStatuses carries
// the per-container state that IsCrashLoopBackOff inspects.
func makePod(name, namespace string, containerStatuses, initStatuses []corev1.ContainerStatus) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Status: corev1.PodStatus{
			ContainerStatuses:     containerStatuses,
			InitContainerStatuses: initStatuses,
		},
	}
}

// waitingStatus returns a ContainerStatus whose container is in the given
// waiting state (e.g., "CrashLoopBackOff", "ImagePullBackOff").
func waitingStatus(reason string) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name: "app",
		State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: reason},
		},
	}
}

// runningStatus returns a ContainerStatus whose container is running normally.
func runningStatus() corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name:  "app",
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}
}

// --- IsCrashLoopBackOff tests ---

// TestIsCrashLoopBackOff_CrashingContainer is the happy-path test:
// a pod with exactly one container in CrashLoopBackOff must be detected.
func TestIsCrashLoopBackOff_CrashingContainer(t *testing.T) {
	pod := makePod("crashing", "default",
		[]corev1.ContainerStatus{waitingStatus("CrashLoopBackOff")},
		nil,
	)
	if !watcher.IsCrashLoopBackOff(pod) {
		t.Error("IsCrashLoopBackOff() = false, want true for CrashLoopBackOff container")
	}
}

// TestIsCrashLoopBackOff_HealthyPod confirms a running pod is not flagged.
func TestIsCrashLoopBackOff_HealthyPod(t *testing.T) {
	pod := makePod("healthy", "default",
		[]corev1.ContainerStatus{runningStatus()},
		nil,
	)
	if watcher.IsCrashLoopBackOff(pod) {
		t.Error("IsCrashLoopBackOff() = true, want false for a running pod")
	}
}

// TestIsCrashLoopBackOff_EmptyPod confirms a pod with no container statuses
// (e.g., still being scheduled) is not flagged.
func TestIsCrashLoopBackOff_EmptyPod(t *testing.T) {
	pod := makePod("pending", "default", nil, nil)
	if watcher.IsCrashLoopBackOff(pod) {
		t.Error("IsCrashLoopBackOff() = true, want false for a pod with no statuses")
	}
}

// TestIsCrashLoopBackOff_CrashingInitContainer verifies that a crashing init
// container is detected even when the main containers have no issues.
// Init containers run before the main app and can also enter CrashLoopBackOff
// (e.g., a broken schema-migration init container).
func TestIsCrashLoopBackOff_CrashingInitContainer(t *testing.T) {
	pod := makePod("init-crash", "default",
		[]corev1.ContainerStatus{runningStatus()},                         // main container is fine
		[]corev1.ContainerStatus{waitingStatus("CrashLoopBackOff")},       // init container is crashing
	)
	if !watcher.IsCrashLoopBackOff(pod) {
		t.Error("IsCrashLoopBackOff() = false, want true for crashing init container")
	}
}

// TestIsCrashLoopBackOff_OtherWaitingReason ensures that other waiting reasons
// such as ImagePullBackOff are NOT mistakenly treated as CrashLoopBackOff.
func TestIsCrashLoopBackOff_OtherWaitingReason(t *testing.T) {
	pod := makePod("image-pull-fail", "default",
		[]corev1.ContainerStatus{waitingStatus("ImagePullBackOff")},
		nil,
	)
	if watcher.IsCrashLoopBackOff(pod) {
		t.Errorf("IsCrashLoopBackOff() = true, want false for ImagePullBackOff pod")
	}
}

// TestIsCrashLoopBackOff_MultipleContainersOneCrashing tests a pod with
// multiple containers where only one is crashing — the pod should be flagged.
func TestIsCrashLoopBackOff_MultipleContainersOneCrashing(t *testing.T) {
	pod := makePod("sidecar-crash", "default",
		[]corev1.ContainerStatus{
			runningStatus(),                          // main app is fine
			waitingStatus("CrashLoopBackOff"),        // sidecar is crashing
		},
		nil,
	)
	if !watcher.IsCrashLoopBackOff(pod) {
		t.Error("IsCrashLoopBackOff() = false, want true when any container is crashing")
	}
}
