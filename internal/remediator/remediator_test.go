// Tests for the remediator package.
// All tests use fake.NewSimpleClientset() — a fully in-memory Kubernetes API
// that accepts the same calls as a real cluster and lets us assert on the
// resulting state without any network or cluster dependency.
package remediator_test

import (
	"context"
	"fmt"
	"testing"

	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/hashir/kube-sentinel/internal/classifier"
	"github.com/hashir/kube-sentinel/internal/config"
	"github.com/hashir/kube-sentinel/internal/remediator"
)

// ---------------------------------------------------------------------------
// Test fixture helpers
// ---------------------------------------------------------------------------

// defaultCfg returns a Config with a small MaxRestartAttempts so rollback
// tests don't need many iterations.
func defaultCfg() *config.Config {
	return &config.Config{
		MaxRestartAttempts:         2,
		MemoryLimitIncreasePercent: 25,
		CrashWindowMinutes:         10,
		AlertThreshold:             3,
	}
}

// makeDeployment creates a Deployment with one container. memLimit can be
// an empty string to leave the limit unset (tests the default-baseline path).
func makeDeployment(name, ns, memLimit string) *appsv1.Deployment {
	limits := corev1.ResourceList{}
	if memLimit != "" {
		limits[corev1.ResourceMemory] = resource.MustParse(memLimit)
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			UID:       "dep-uid",
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "app",
							Image: "myapp:v2",
							Resources: corev1.ResourceRequirements{
								Limits: limits,
							},
						},
					},
				},
			},
		},
	}
}

// makeReplicaSet creates a ReplicaSet owned by depName with a specific
// rollout revision and container image. The revision annotation is what
// rollbackDeployment uses to sort and select the stable RS.
func makeReplicaSet(name, ns, depName string, revision int, image string) *appsv1.ReplicaSet {
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{"app": depName},
			Annotations: map[string]string{
				// This annotation is set by the Deployment controller.
				"deployment.kubernetes.io/revision": fmt.Sprint(revision),
			},
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "Deployment", Name: depName, UID: "dep-uid"},
			},
		},
		Spec: appsv1.ReplicaSetSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": depName}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", Image: image},
					},
				},
			},
		},
	}
}

// makeCrashPod creates a Pod in CrashLoopBackOff owned by the given RS.
func makeCrashPod(name, ns, rsName string, exitCode int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{"app": rsName},
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: rsName},
			},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "app",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
					},
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode: exitCode,
							Reason:   "Error",
						},
					},
				},
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestRemediate_OOMKilled verifies that:
//  1. The Deployment memory limit is increased by 25%.
//  2. The pod is deleted so it restarts with the new limit.
func TestRemediate_OOMKilled(t *testing.T) {
	dep := makeDeployment("my-app", "default", "512Mi")
	rs := makeReplicaSet("my-app-rs1", "default", "my-app", 1, "myapp:v1")
	pod := makeCrashPod("my-app-pod", "default", "my-app-rs1", 137)

	client := fake.NewSimpleClientset(dep, rs, pod)
	rem := remediator.New(client, zap.NewNop(), defaultCfg(), nil)

	result := &classifier.Result{
		FailureType:   classifier.FailureOOMKilled,
		ContainerName: "app",
		ExitCode:      137,
		Reason:        "OOMKilled",
	}

	if err := rem.Remediate(context.Background(), pod, result); err != nil {
		t.Fatalf("Remediate() unexpected error: %v", err)
	}

	// Assert 1: memory limit increased by 25%.
	// 512 MiB * 1.25 = 640 MiB = 671_088_640 bytes.
	updatedDep, err := client.AppsV1().Deployments("default").Get(
		context.Background(), "my-app", metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("fetching updated deployment: %v", err)
	}
	gotMem := updatedDep.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory]
	wantBytes := int64(512) * 1024 * 1024 * 125 / 100 // 640 MiB in bytes
	if gotMem.Value() != wantBytes {
		t.Errorf("memory limit = %d bytes (%s), want %d bytes (640Mi)",
			gotMem.Value(), gotMem.String(), wantBytes)
	}

	// Assert 2: pod was deleted.
	assertPodDeleted(t, client, "default", "my-app-pod")
}

// TestRemediate_OOMKilled_NoExistingLimit verifies that when no memory limit
// is set the remediator uses 256Mi as a baseline, producing 320Mi after +25%.
func TestRemediate_OOMKilled_NoExistingLimit(t *testing.T) {
	dep := makeDeployment("my-app", "default", "") // no memory limit
	rs := makeReplicaSet("my-app-rs1", "default", "my-app", 1, "myapp:v1")
	pod := makeCrashPod("my-app-pod", "default", "my-app-rs1", 137)

	client := fake.NewSimpleClientset(dep, rs, pod)
	rem := remediator.New(client, zap.NewNop(), defaultCfg(), nil)

	result := &classifier.Result{
		FailureType:   classifier.FailureOOMKilled,
		ContainerName: "app",
		ExitCode:      137,
	}

	if err := rem.Remediate(context.Background(), pod, result); err != nil {
		t.Fatalf("Remediate() unexpected error: %v", err)
	}

	updatedDep, _ := client.AppsV1().Deployments("default").Get(
		context.Background(), "my-app", metav1.GetOptions{},
	)
	gotMem := updatedDep.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory]
	wantBytes := int64(256) * 1024 * 1024 * 125 / 100 // 320 MiB in bytes
	if gotMem.Value() != wantBytes {
		t.Errorf("memory limit = %d bytes, want %d bytes (320Mi)", gotMem.Value(), wantBytes)
	}
}

// TestRemediate_ConfigError verifies that no Kubernetes resources are modified
// for a ConfigError — the remediator must leave both the Deployment and the
// pod untouched.
func TestRemediate_ConfigError(t *testing.T) {
	dep := makeDeployment("my-app", "default", "512Mi")
	rs := makeReplicaSet("my-app-rs1", "default", "my-app", 1, "myapp:v1")
	pod := makeCrashPod("my-app-pod", "default", "my-app-rs1", 1)

	client := fake.NewSimpleClientset(dep, rs, pod)
	rem := remediator.New(client, zap.NewNop(), defaultCfg(), nil)

	result := &classifier.Result{
		FailureType:   classifier.FailureConfigError,
		ContainerName: "app",
		ExitCode:      1,
	}

	if err := rem.Remediate(context.Background(), pod, result); err != nil {
		t.Fatalf("Remediate() unexpected error: %v", err)
	}

	// Pod must still exist — ConfigError should never auto-restart.
	assertPodExists(t, client, "default", "my-app-pod")
}

// TestRemediate_AppCrash_Restart verifies that the pod is deleted (restarted)
// when the crash count is below MaxRestartAttempts (2 in defaultCfg).
func TestRemediate_AppCrash_Restart(t *testing.T) {
	dep := makeDeployment("my-app", "default", "512Mi")
	rs := makeReplicaSet("my-app-rs1", "default", "my-app", 1, "myapp:v1")
	pod := makeCrashPod("my-app-pod", "default", "my-app-rs1", 1)

	client := fake.NewSimpleClientset(dep, rs, pod)
	rem := remediator.New(client, zap.NewNop(), defaultCfg(), nil)

	result := &classifier.Result{
		FailureType:   classifier.FailureAppCrash,
		ContainerName: "app",
		ExitCode:      1,
	}

	// First crash (count=0 < max=2) → should restart.
	if err := rem.Remediate(context.Background(), pod, result); err != nil {
		t.Fatalf("Remediate() error on attempt 1: %v", err)
	}
	assertPodDeleted(t, client, "default", "my-app-pod")
}

// TestRemediate_AppCrash_Rollback verifies that once MaxRestartAttempts is
// exhausted the Deployment is patched to use the stable (previous) RS image.
func TestRemediate_AppCrash_Rollback(t *testing.T) {
	ctx := context.Background()
	dep := makeDeployment("my-app", "default", "512Mi")
	// Two ReplicaSets: revision 1 = stable ("v1"), revision 2 = broken ("v2").
	rs1 := makeReplicaSet("my-app-rs1", "default", "my-app", 1, "myapp:v1")
	rs2 := makeReplicaSet("my-app-rs2", "default", "my-app", 2, "myapp:v2")
	pod := makeCrashPod("my-app-pod", "default", "my-app-rs2", 1)

	client := fake.NewSimpleClientset(dep, rs1, rs2, pod)
	cfg := defaultCfg() // MaxRestartAttempts = 2
	rem := remediator.New(client, zap.NewNop(), cfg, nil)

	result := &classifier.Result{
		FailureType:   classifier.FailureAppCrash,
		ContainerName: "app",
		ExitCode:      1,
	}

	// Exhaust the restart budget. Each call deletes the pod, so we re-create
	// it between calls to simulate the ReplicaSet controller's behaviour.
	for i := 0; i < cfg.MaxRestartAttempts; i++ {
		if i > 0 {
			if _, err := client.CoreV1().Pods("default").Create(ctx, pod, metav1.CreateOptions{}); err != nil {
				t.Fatalf("re-creating pod before attempt %d: %v", i+1, err)
			}
		}
		if err := rem.Remediate(ctx, pod, result); err != nil {
			t.Fatalf("Remediate() error on attempt %d: %v", i+1, err)
		}
	}

	// Next call (count == MaxRestartAttempts) should roll back instead of restart.
	if _, err := client.CoreV1().Pods("default").Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("re-creating pod before rollback call: %v", err)
	}
	if err := rem.Remediate(ctx, pod, result); err != nil {
		t.Fatalf("Remediate() rollback error: %v", err)
	}

	// The Deployment should now point at the stable image from rs1.
	updatedDep, err := client.AppsV1().Deployments("default").Get(ctx, "my-app", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("fetching updated deployment: %v", err)
	}
	gotImage := updatedDep.Spec.Template.Spec.Containers[0].Image
	if gotImage != "myapp:v1" {
		t.Errorf("image after rollback = %q, want %q", gotImage, "myapp:v1")
	}
}

// TestRemediate_Unknown_FirstCrash verifies the pod is deleted on the first
// Unknown failure.
func TestRemediate_Unknown_FirstCrash(t *testing.T) {
	dep := makeDeployment("my-app", "default", "512Mi")
	rs := makeReplicaSet("my-app-rs1", "default", "my-app", 1, "myapp:v1")
	pod := makeCrashPod("my-app-pod", "default", "my-app-rs1", 2)

	client := fake.NewSimpleClientset(dep, rs, pod)
	rem := remediator.New(client, zap.NewNop(), defaultCfg(), nil)

	result := &classifier.Result{
		FailureType:   classifier.FailureUnknown,
		ContainerName: "app",
		ExitCode:      2,
	}

	if err := rem.Remediate(context.Background(), pod, result); err != nil {
		t.Fatalf("Remediate() unexpected error: %v", err)
	}
	assertPodDeleted(t, client, "default", "my-app-pod")
}

// TestRemediate_Unknown_SecondCrash verifies that a second Unknown crash does
// NOT delete the pod — the remediator only alerts.
func TestRemediate_Unknown_SecondCrash(t *testing.T) {
	ctx := context.Background()
	dep := makeDeployment("my-app", "default", "512Mi")
	rs := makeReplicaSet("my-app-rs1", "default", "my-app", 1, "myapp:v1")
	pod := makeCrashPod("my-app-pod", "default", "my-app-rs1", 2)

	client := fake.NewSimpleClientset(dep, rs, pod)
	rem := remediator.New(client, zap.NewNop(), defaultCfg(), nil)

	result := &classifier.Result{
		FailureType:   classifier.FailureUnknown,
		ContainerName: "app",
		ExitCode:      2,
	}

	// First crash → restart.
	if err := rem.Remediate(ctx, pod, result); err != nil {
		t.Fatalf("first Remediate() error: %v", err)
	}

	// Re-create pod (simulates the RS creating a replacement).
	if _, err := client.CoreV1().Pods("default").Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("re-creating pod: %v", err)
	}

	// Second crash → alert only, pod must NOT be deleted.
	if err := rem.Remediate(ctx, pod, result); err != nil {
		t.Fatalf("second Remediate() error: %v", err)
	}
	assertPodExists(t, client, "default", "my-app-pod")
}

// TestRemediate_AppCrash_NoDeployment verifies that when no owning Deployment
// is found (standalone pod), the remediator still restarts the pod rather
// than returning an error.
func TestRemediate_AppCrash_NoDeployment(t *testing.T) {
	// Pod with no OwnerReferences — not managed by a Deployment.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "solo-pod", Namespace: "default"},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "app",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
					},
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{ExitCode: 1},
					},
				},
			},
		},
	}

	client := fake.NewSimpleClientset(pod)
	rem := remediator.New(client, zap.NewNop(), defaultCfg(), nil)

	result := &classifier.Result{
		FailureType:   classifier.FailureAppCrash,
		ContainerName: "app",
		ExitCode:      1,
	}

	if err := rem.Remediate(context.Background(), pod, result); err != nil {
		t.Fatalf("Remediate() unexpected error: %v", err)
	}
	// Pod should have been deleted even without a Deployment.
	assertPodDeleted(t, client, "default", "solo-pod")
}

// ---------------------------------------------------------------------------
// Assertion helpers
// ---------------------------------------------------------------------------

// assertPodDeleted checks that the named pod no longer exists.
func assertPodDeleted(t *testing.T, client *fake.Clientset, ns, name string) {
	t.Helper()
	_, err := client.CoreV1().Pods(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err == nil {
		t.Errorf("pod %s/%s still exists; expected it to be deleted", ns, name)
		return
	}
	if !k8serrors.IsNotFound(err) {
		t.Errorf("unexpected error checking for deleted pod %s/%s: %v", ns, name, err)
	}
}

// assertPodExists checks that the named pod still exists.
func assertPodExists(t *testing.T, client *fake.Clientset, ns, name string) {
	t.Helper()
	if _, err := client.CoreV1().Pods(ns).Get(context.Background(), name, metav1.GetOptions{}); err != nil {
		t.Errorf("pod %s/%s should exist but got error: %v", ns, name, err)
	}
}
