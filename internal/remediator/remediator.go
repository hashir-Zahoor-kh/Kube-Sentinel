// Package remediator applies automated fixes to CrashLoopBackOff pods based
// on the failure type determined by the Classifier.
//
// Each failure type maps to a specific remediation strategy:
//
//	OOMKilled   → patch Deployment memory limit +N%, delete pod to restart
//	ConfigError → no auto-fix; fire critical alert only (wired in Stage 5)
//	AppCrash    → restart pod up to MaxRestartAttempts, then rollback Deployment
//	Unknown     → restart once, alert if it crashes again
//
// The remediator interacts with Kubernetes via the client-go clientset, so
// it requires RBAC permissions to get/list/update Deployments and ReplicaSets
// and to delete Pods. See the README for the required ClusterRole.
package remediator

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync"

	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"

	"github.com/hashir/kube-sentinel/internal/classifier"
	"github.com/hashir/kube-sentinel/internal/config"
)

// revisionAnnotation is the annotation Kubernetes sets on each ReplicaSet to
// track which rollout revision it belongs to. We use it to find the
// "previous stable" ReplicaSet during a rollback.
const revisionAnnotation = "deployment.kubernetes.io/revision"

// defaultMemoryLimit is used as the base when a container has no explicit
// memory limit set. We need a base to compute "+25%".
const defaultMemoryLimit = "256Mi"

// Remediator applies automated fixes to CrashLoopBackOff pods.
type Remediator struct {
	// client is the Kubernetes API client.
	client kubernetes.Interface

	// logger is the structured logger.
	logger *zap.Logger

	// cfg holds configuration values such as MaxRestartAttempts and
	// MemoryLimitIncreasePercent.
	cfg *config.Config

	// mu protects restartCounts from concurrent writes.
	// Multiple pod-handler goroutines may call Remediate simultaneously.
	mu sync.Mutex

	// restartCounts tracks how many times Kube-Sentinel has restarted each pod.
	// Key: "namespace/name". Reset when the remediator is restarted.
	restartCounts map[string]int
}

// New constructs a Remediator.
func New(client kubernetes.Interface, logger *zap.Logger, cfg *config.Config) *Remediator {
	return &Remediator{
		client:        client,
		logger:        logger,
		cfg:           cfg,
		restartCounts: make(map[string]int),
	}
}

// Remediate dispatches to the appropriate handler based on the classifier result.
// It is safe to call concurrently from multiple goroutines.
func (r *Remediator) Remediate(ctx context.Context, pod *corev1.Pod, result *classifier.Result) error {
	r.logger.Info("Starting remediation",
		zap.String("pod", pod.Name),
		zap.String("namespace", pod.Namespace),
		zap.String("failure_type", string(result.FailureType)),
	)

	switch result.FailureType {
	case classifier.FailureOOMKilled:
		return r.handleOOMKilled(ctx, pod, result)
	case classifier.FailureConfigError:
		return r.handleConfigError(ctx, pod, result)
	case classifier.FailureAppCrash:
		return r.handleAppCrash(ctx, pod, result)
	default: // FailureUnknown
		return r.handleUnknown(ctx, pod, result)
	}
}

// ---------------------------------------------------------------------------
// Failure-type handlers
// ---------------------------------------------------------------------------

// handleOOMKilled increases the owning Deployment's memory limit by
// cfg.MemoryLimitIncreasePercent percent, then deletes the pod so it
// restarts with the new limit. If the pod has no owning Deployment
// (e.g., it's a standalone pod), we fall back to a plain pod restart.
func (r *Remediator) handleOOMKilled(ctx context.Context, pod *corev1.Pod, result *classifier.Result) error {
	r.logger.Info("OOMKilled: patching memory limit and restarting pod",
		zap.String("pod", pod.Name),
		zap.String("namespace", pod.Namespace),
		zap.String("container", result.ContainerName),
		zap.Int("increase_percent", r.cfg.MemoryLimitIncreasePercent),
	)

	dep, err := r.findOwningDeployment(ctx, pod)
	if err != nil {
		// No Deployment found — still worth restarting the pod.
		r.logger.Warn("OOMKilled: no owning Deployment found, restarting pod without limit patch",
			zap.String("pod", pod.Name),
			zap.Error(err),
		)
		return r.deletePod(ctx, pod)
	}

	if err := r.patchMemoryLimit(ctx, dep, result.ContainerName, r.cfg.MemoryLimitIncreasePercent); err != nil {
		return fmt.Errorf("OOMKilled: patching memory limit: %w", err)
	}

	if err := r.deletePod(ctx, pod); err != nil {
		return fmt.Errorf("OOMKilled: deleting pod for restart: %w", err)
	}

	r.logger.Info("OOMKilled remediation complete",
		zap.String("pod", pod.Name),
		zap.String("deployment", dep.Name),
	)
	return nil
}

// handleConfigError fires a critical alert but does NOT auto-remediate.
// Configuration errors require human intervention — auto-restarting would
// just produce more crashes. The full structured-JSON alert is wired in Stage 5.
func (r *Remediator) handleConfigError(ctx context.Context, pod *corev1.Pod, result *classifier.Result) error {
	// Log at ERROR level so this is immediately visible in log aggregators.
	r.logger.Error("ConfigError: skipping auto-remediation — human intervention required",
		zap.String("pod", pod.Name),
		zap.String("namespace", pod.Namespace),
		zap.String("container", result.ContainerName),
		zap.Int32("exit_code", result.ExitCode),
		zap.String("hint", "check your ConfigMaps, Secrets, and environment variables"),
	)
	// Stage 5 will call alerter.FireCritical(pod, result) here.
	return nil
}

// handleAppCrash restarts the pod up to cfg.MaxRestartAttempts times.
// Once the limit is reached it rolls back the owning Deployment to the
// last stable ReplicaSet image instead of restarting again.
func (r *Remediator) handleAppCrash(ctx context.Context, pod *corev1.Pod, result *classifier.Result) error {
	key := podKey(pod)

	// Read and increment the restart counter atomically.
	r.mu.Lock()
	count := r.restartCounts[key]
	r.mu.Unlock()

	r.logger.Info("AppCrash: evaluating restart vs rollback",
		zap.String("pod", pod.Name),
		zap.String("namespace", pod.Namespace),
		zap.Int("restart_count", count),
		zap.Int("max_attempts", r.cfg.MaxRestartAttempts),
	)

	if count < r.cfg.MaxRestartAttempts {
		// Increment first, then restart. This order means even if deletePod
		// fails we don't accidentally retry without counting the attempt.
		r.mu.Lock()
		r.restartCounts[key]++
		r.mu.Unlock()

		r.logger.Info("AppCrash: restarting pod",
			zap.String("pod", pod.Name),
			zap.Int("attempt", count+1),
			zap.Int("of", r.cfg.MaxRestartAttempts),
		)
		return r.deletePod(ctx, pod)
	}

	// Exceeded max restarts — roll back to the last known-good image.
	r.logger.Warn("AppCrash: max restart attempts reached, rolling back deployment",
		zap.String("pod", pod.Name),
		zap.Int("attempts_exhausted", count),
	)
	if err := r.rollbackDeployment(ctx, pod); err != nil {
		// If there is no owning Deployment (standalone pod), fall back to
		// a final restart rather than returning an error.
		r.logger.Warn("AppCrash: rollback failed, falling back to pod restart",
			zap.String("pod", pod.Name),
			zap.Error(err),
		)
		return r.deletePod(ctx, pod)
	}
	return nil
}

// handleUnknown restarts the pod once. If it crashes again (count > 0),
// a critical alert is fired instead of restarting indefinitely.
func (r *Remediator) handleUnknown(ctx context.Context, pod *corev1.Pod, result *classifier.Result) error {
	key := podKey(pod)

	r.mu.Lock()
	count := r.restartCounts[key]
	r.mu.Unlock()

	if count == 0 {
		r.logger.Info("Unknown failure: restarting pod once",
			zap.String("pod", pod.Name),
			zap.String("namespace", pod.Namespace),
		)
		r.mu.Lock()
		r.restartCounts[key]++
		r.mu.Unlock()

		return r.deletePod(ctx, pod)
	}

	// Already restarted once and it crashed again — alert without auto-fix.
	// Stage 5 will call alerter.FireCritical(pod, result) here.
	r.logger.Error("Unknown failure persists after restart — manual investigation required",
		zap.String("pod", pod.Name),
		zap.String("namespace", pod.Namespace),
		zap.Int("crash_count", count+1),
	)
	return nil
}

// ---------------------------------------------------------------------------
// Core operations
// ---------------------------------------------------------------------------

// deletePod removes the pod with a zero grace period.
// Because the pod is in CrashLoopBackOff (container is stopped, not running),
// there is no running process to signal — an instant deletion is safe.
// The owning ReplicaSet will immediately create a replacement pod.
func (r *Remediator) deletePod(ctx context.Context, pod *corev1.Pod) error {
	// GracePeriodSeconds: 0 skips the normal SIGTERM wait.
	// We need a pointer to the int64 value — Go doesn't let you take the
	// address of a literal, so we assign to a variable first.
	grace := int64(0)

	err := r.client.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
		GracePeriodSeconds: &grace,
	})
	if err != nil {
		return fmt.Errorf("deleting pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}

	r.logger.Info("Pod deleted — ReplicaSet will create a replacement",
		zap.String("pod", pod.Name),
		zap.String("namespace", pod.Namespace),
	)
	return nil
}

// patchMemoryLimit finds the named container in the Deployment's pod template
// and increases its memory limit by increasePercent percent.
// If the container has no memory limit set, defaultMemoryLimit is used as
// the starting value before applying the percentage increase.
func (r *Remediator) patchMemoryLimit(ctx context.Context, dep *appsv1.Deployment, containerName string, increasePercent int) error {
	// DeepCopy is critical here. The Deployment object may come from the
	// informer cache, which is shared and must not be mutated. DeepCopy
	// creates an independent copy we can freely modify.
	updated := dep.DeepCopy()

	patched := false
	for i, c := range updated.Spec.Template.Spec.Containers {
		if c.Name != containerName {
			continue
		}

		// Ensure the Limits map exists before writing into it.
		if updated.Spec.Template.Spec.Containers[i].Resources.Limits == nil {
			updated.Spec.Template.Spec.Containers[i].Resources.Limits = corev1.ResourceList{}
		}

		// Get the current memory limit. If unset, start from the default.
		current := c.Resources.Limits[corev1.ResourceMemory]
		if current.IsZero() {
			current = resource.MustParse(defaultMemoryLimit)
			r.logger.Warn("No memory limit was set; using default as baseline",
				zap.String("container", containerName),
				zap.String("baseline", defaultMemoryLimit),
			)
		}

		// Arithmetic: newBytes = current + (current * percent / 100).
		// resource.Quantity.Value() returns bytes as int64.
		currentBytes := current.Value()
		newBytes := currentBytes + (currentBytes*int64(increasePercent))/100
		newLimit := resource.NewQuantity(newBytes, resource.BinarySI)

		updated.Spec.Template.Spec.Containers[i].Resources.Limits[corev1.ResourceMemory] = *newLimit
		patched = true

		r.logger.Info("Memory limit updated",
			zap.String("deployment", dep.Name),
			zap.String("container", containerName),
			zap.String("old_limit", current.String()),
			zap.String("new_limit", newLimit.String()),
		)
		break
	}

	if !patched {
		return fmt.Errorf("container %q not found in deployment %q/%q", containerName, dep.Namespace, dep.Name)
	}

	_, err := r.client.AppsV1().Deployments(dep.Namespace).Update(ctx, updated, metav1.UpdateOptions{})
	return err
}

// rollbackDeployment finds the owning Deployment, identifies the last stable
// ReplicaSet by revision number, and patches the Deployment's pod template
// to match that ReplicaSet's spec. This triggers Kubernetes to perform a
// rolling update back to the previous image.
func (r *Remediator) rollbackDeployment(ctx context.Context, pod *corev1.Pod) error {
	dep, err := r.findOwningDeployment(ctx, pod)
	if err != nil {
		return fmt.Errorf("rollback: %w", err)
	}

	// List all ReplicaSets whose labels match the Deployment's selector.
	// Every RS owned by a Deployment carries the same label set as the
	// Deployment's spec.selector.matchLabels.
	rsList, err := r.client.AppsV1().ReplicaSets(dep.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.Set(dep.Spec.Selector.MatchLabels).String(),
	})
	if err != nil {
		return fmt.Errorf("rollback: listing ReplicaSets: %w", err)
	}

	// rsWithRev pairs a ReplicaSet with its parsed revision number.
	type rsWithRev struct {
		rs  appsv1.ReplicaSet
		rev int64
	}

	var candidates []rsWithRev
	for _, rs := range rsList.Items {
		revStr, ok := rs.Annotations[revisionAnnotation]
		if !ok {
			continue // skip RSes that have no revision annotation
		}
		rev, err := strconv.ParseInt(revStr, 10, 64)
		if err != nil {
			continue
		}
		candidates = append(candidates, rsWithRev{rs: rs, rev: rev})
	}

	// Sort descending: candidates[0] = current revision, candidates[1] = previous.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].rev > candidates[j].rev
	})

	if len(candidates) < 2 {
		return fmt.Errorf("rollback: deployment %q has fewer than 2 revisions — nothing to roll back to", dep.Name)
	}

	currentRS := candidates[0]
	previousRS := candidates[1]

	r.logger.Info("Rolling back deployment",
		zap.String("deployment", dep.Name),
		zap.String("namespace", dep.Namespace),
		zap.Int64("from_revision", currentRS.rev),
		zap.Int64("to_revision", previousRS.rev),
	)

	// Patch the Deployment's pod template to match the previous ReplicaSet.
	// Kubernetes will detect the spec change and begin a rolling update that
	// replaces the current pods with ones running the previous image.
	updated := dep.DeepCopy()
	updated.Spec.Template = previousRS.rs.Spec.Template

	if _, err := r.client.AppsV1().Deployments(dep.Namespace).Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("rollback: updating deployment: %w", err)
	}

	// Record the rollback as a Kubernetes Event so it shows up in
	// `kubectl describe pod` and in cluster audit logs.
	var prevImage string
	if len(previousRS.rs.Spec.Template.Spec.Containers) > 0 {
		prevImage = previousRS.rs.Spec.Template.Spec.Containers[0].Image
	}
	r.recordEvent(ctx, pod,
		"KubeSentinelRollback",
		fmt.Sprintf("Kube-Sentinel rolled back deployment %q from revision %d to %d (image: %s) after %d failed restart attempts",
			dep.Name, currentRS.rev, previousRS.rev, prevImage, r.cfg.MaxRestartAttempts),
	)

	r.logger.Info("Rollback complete",
		zap.String("deployment", dep.Name),
		zap.Int64("stable_revision", previousRS.rev),
		zap.String("stable_image", prevImage),
	)
	return nil
}

// findOwningDeployment walks the owner-reference chain Pod → ReplicaSet →
// Deployment and returns the Deployment. Most application pods follow this
// chain; standalone pods or StatefulSet pods will return an error.
func (r *Remediator) findOwningDeployment(ctx context.Context, pod *corev1.Pod) (*appsv1.Deployment, error) {
	// OwnerReferences is a slice because a pod could theoretically have
	// multiple owners, though in practice it always has exactly one.
	for _, ref := range pod.OwnerReferences {
		if ref.Kind != "ReplicaSet" {
			continue
		}

		// Step 1: get the ReplicaSet that owns this pod.
		rs, err := r.client.AppsV1().ReplicaSets(pod.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("getting ReplicaSet %q: %w", ref.Name, err)
		}

		// Step 2: get the Deployment that owns that ReplicaSet.
		for _, rsRef := range rs.OwnerReferences {
			if rsRef.Kind != "Deployment" {
				continue
			}
			dep, err := r.client.AppsV1().Deployments(pod.Namespace).Get(ctx, rsRef.Name, metav1.GetOptions{})
			if err != nil {
				return nil, fmt.Errorf("getting Deployment %q: %w", rsRef.Name, err)
			}
			return dep, nil
		}
	}

	return nil, fmt.Errorf("pod %s/%s has no owning Deployment (standalone pod, DaemonSet, or StatefulSet?)",
		pod.Namespace, pod.Name)
}

// recordEvent creates a Kubernetes Event on the pod so that the remediation
// action is visible via `kubectl describe pod` and in audit logs.
// Errors are logged but do not fail the remediation — events are best-effort.
func (r *Remediator) recordEvent(ctx context.Context, pod *corev1.Pod, reason, message string) {
	now := metav1.Now()

	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			// Event names must be unique; we combine the pod name with a
			// Unix timestamp to ensure uniqueness.
			Name:      fmt.Sprintf("kube-sentinel.%s.%d", pod.Name, now.Unix()),
			Namespace: pod.Namespace,
		},
		// InvolvedObject links the event back to the pod it describes.
		InvolvedObject: corev1.ObjectReference{
			Kind:       "Pod",
			Name:       pod.Name,
			Namespace:  pod.Namespace,
			UID:        pod.UID,
			APIVersion: "v1",
		},
		Reason:  reason,
		Message: message,
		// EventTypeWarning makes the event show up as "Warning" in kubectl.
		Type:           corev1.EventTypeWarning,
		FirstTimestamp: now,
		LastTimestamp:  now,
		Count:          1,
		Source:         corev1.EventSource{Component: "kube-sentinel"},
	}

	if _, err := r.client.CoreV1().Events(pod.Namespace).Create(ctx, event, metav1.CreateOptions{}); err != nil {
		r.logger.Warn("Failed to record Kubernetes event",
			zap.String("pod", pod.Name),
			zap.String("reason", reason),
			zap.Error(err),
		)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// podKey returns the map key used to track restart counts for a pod.
// Using namespace+name rather than UID means the counter persists across
// pod deletion+recreation (which keeps the same name under a Deployment).
func podKey(pod *corev1.Pod) string {
	return pod.Namespace + "/" + pod.Name
}
