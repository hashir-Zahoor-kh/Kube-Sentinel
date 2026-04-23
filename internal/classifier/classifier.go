// Package classifier analyses a CrashLoopBackOff pod and determines the root
// cause of the failure. It does this by inspecting two things:
//  1. The container's exit code (from the pod's last-termination state).
//  2. The last 50 lines of the container's logs (fetched via the Kubernetes API).
//
// Classification result is one of four types:
//
//	OOMKilled   — exit 137, container was killed by the kernel OOM reaper
//	ConfigError — exit 1 + config-related keywords in logs
//	AppCrash    — exit 1 + application error keywords in logs
//	Unknown     — anything that doesn't match the above
package classifier

import (
	"context"
	"fmt"
	"io"
	"strings"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

// FailureType is a string enum describing why a pod is crashing.
// Using a named type (rather than a plain string) lets the compiler catch
// mismatched values at compile time.
type FailureType string

const (
	// FailureOOMKilled means the container was killed because it exceeded its
	// memory limit. The kernel OOM reaper sends SIGKILL, which maps to exit 137.
	FailureOOMKilled FailureType = "OOMKilled"

	// FailureConfigError means the container exited 1 and the logs contain
	// keywords associated with misconfiguration (e.g., "invalid config", "no such file").
	FailureConfigError FailureType = "ConfigError"

	// FailureAppCrash means the container exited 1 and the logs suggest an
	// application-level error (e.g., "panic", "exception", "fatal").
	FailureAppCrash FailureType = "AppCrash"

	// FailureUnknown means the exit code or logs don't match any known pattern.
	FailureUnknown FailureType = "Unknown"
)

// exitCodeOOMKilled is the Linux exit code produced when the kernel OOM
// reaper kills a process. It equals 128 + signal 9 (SIGKILL).
const exitCodeOOMKilled = 137

// logTailLines is how many lines from the end of the container log we fetch.
// 50 lines is enough to capture most stack traces and error messages without
// pulling megabytes of logs.
const logTailLines = 50

// configKeywords are substrings (lowercased) we look for in logs when the
// exit code is 1 to decide the failure is a configuration problem.
var configKeywords = []string{
	"no such file",
	"not found",
	"invalid config",
	"configuration error",
	"failed to load config",
	"missing env",
	"environment variable",
	"permission denied",
	"unable to read",
	"cannot open",
}

// appCrashKeywords are substrings (lowercased) we look for in logs when the
// exit code is 1 to decide the failure is an application crash.
var appCrashKeywords = []string{
	"panic",
	"exception",
	"fatal",
	"segfault",
	"segmentation fault",
	"null pointer",
	"nullpointerexception",
	"unhandled",
	"runtime error",
	"stack overflow",
	"out of memory",    // app-level OOM (not kernel) — e.g., Java heap space
	"heap space",
	"killed",
}

// Result holds the full classification output for a pod.
type Result struct {
	// FailureType is the determined root cause.
	FailureType FailureType

	// ContainerName is the first crashing container that was analysed.
	ContainerName string

	// ExitCode is the exit code from the container's last termination.
	ExitCode int32

	// Logs holds the tail of the container logs used for classification.
	// Stored so the remediator and alerter can include it in their output.
	Logs string

	// Reason is the human-readable reason string from the pod status
	// (e.g., "OOMKilled", "Error", "Completed").
	Reason string
}

// LogReader abstracts fetching container logs so we can swap in a fake
// implementation during unit tests without needing a real Kubernetes cluster.
type LogReader interface {
	// GetLogs returns the last n lines of logs for the named container in pod.
	GetLogs(ctx context.Context, namespace, podName, containerName string, tailLines int64) (string, error)
}

// K8sLogReader implements LogReader using the real Kubernetes API.
type K8sLogReader struct {
	client kubernetes.Interface
}

// NewK8sLogReader constructs a log reader backed by the given clientset.
func NewK8sLogReader(client kubernetes.Interface) *K8sLogReader {
	return &K8sLogReader{client: client}
}

// GetLogs fetches the last tailLines lines from the container's log stream.
// It uses the Kubernetes pod/log sub-resource via the REST API.
func (r *K8sLogReader) GetLogs(ctx context.Context, namespace, podName, containerName string, tailLines int64) (string, error) {
	// PodLogOptions configures the log request.
	// TailLines tells the API server to return only the last N lines —
	// much cheaper than fetching the full log and slicing client-side.
	opts := &corev1.PodLogOptions{
		Container: containerName,
		TailLines: &tailLines,
		// Previous: true would fetch the logs of the *previous* (terminated)
		// container instance, which is what we want for a crashed container.
		Previous: true,
	}

	// GetLogs returns a *rest.Request. We call Stream() to open an HTTP
	// chunked-transfer stream and read all bytes from it.
	req := r.client.CoreV1().Pods(namespace).GetLogs(podName, opts)
	stream, err := req.Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("opening log stream for %s/%s[%s]: %w", namespace, podName, containerName, err)
	}
	defer stream.Close() // always close the stream when we're done

	// io.ReadAll reads the entire stream into memory.
	data, err := io.ReadAll(stream)
	if err != nil {
		return "", fmt.Errorf("reading log stream for %s/%s[%s]: %w", namespace, podName, containerName, err)
	}

	return string(data), nil
}

// Classifier determines the failure type for a crashing pod.
type Classifier struct {
	logger    *zap.Logger
	logReader LogReader
}

// New constructs a Classifier.
//
//   - logger:    structured logger
//   - logReader: implementation that fetches container logs
func New(logger *zap.Logger, logReader LogReader) *Classifier {
	return &Classifier{
		logger:    logger,
		logReader: logReader,
	}
}

// Classify examines a CrashLoopBackOff pod and returns a Result describing
// why it is crashing. It inspects the first container (including init
// containers) that shows a CrashLoopBackOff waiting state.
func (c *Classifier) Classify(ctx context.Context, pod *corev1.Pod) (*Result, error) {
	// Find the first crashing container. We look at init containers first
	// because if one is failing, the main containers never even start.
	containerName, exitCode, reason, err := findCrashingContainer(pod)
	if err != nil {
		return nil, err
	}

	c.logger.Info("Classifying failure",
		zap.String("pod", pod.Name),
		zap.String("namespace", pod.Namespace),
		zap.String("container", containerName),
		zap.Int32("exit_code", exitCode),
		zap.String("reason", reason),
	)

	// OOMKilled is indicated directly by the reason field in the pod status,
	// OR by exit code 137. We check both to be safe.
	if reason == "OOMKilled" || exitCode == exitCodeOOMKilled {
		return &Result{
			FailureType:   FailureOOMKilled,
			ContainerName: containerName,
			ExitCode:      exitCode,
			Reason:        reason,
		}, nil
	}

	// For exit code 1 (generic failure), we need to look at the logs to
	// determine whether it's a config problem or an application crash.
	logs, err := c.logReader.GetLogs(ctx, pod.Namespace, pod.Name, containerName, logTailLines)
	if err != nil {
		// Log the error but don't fail — we can still classify as Unknown.
		c.logger.Warn("Failed to fetch container logs; classifying as Unknown",
			zap.String("pod", pod.Name),
			zap.String("container", containerName),
			zap.Error(err),
		)
		return &Result{
			FailureType:   FailureUnknown,
			ContainerName: containerName,
			ExitCode:      exitCode,
			Reason:        reason,
			Logs:          "",
		}, nil
	}

	// Normalize to lowercase once so keyword matching is case-insensitive.
	logsLower := strings.ToLower(logs)

	failureType := classifyByLogs(logsLower)

	c.logger.Info("Classification complete",
		zap.String("pod", pod.Name),
		zap.String("failure_type", string(failureType)),
	)

	return &Result{
		FailureType:   failureType,
		ContainerName: containerName,
		ExitCode:      exitCode,
		Reason:        reason,
		Logs:          logs,
	}, nil
}

// findCrashingContainer scans pod status for the first container in
// CrashLoopBackOff and returns its name, last exit code, and termination
// reason. Init containers are checked before regular containers.
func findCrashingContainer(pod *corev1.Pod) (name string, exitCode int32, reason string, err error) {
	// Helper closure: check one ContainerStatus slice.
	check := func(statuses []corev1.ContainerStatus) (string, int32, string, bool) {
		for _, cs := range statuses {
			// The container is in CrashLoopBackOff when its current state
			// is Waiting with reason "CrashLoopBackOff".
			if cs.State.Waiting == nil || cs.State.Waiting.Reason != "CrashLoopBackOff" {
				continue
			}
			// LastTerminationState holds the previous container run's exit info.
			// This is what we want — not the current (Waiting) state.
			last := cs.LastTerminationState.Terminated
			if last == nil {
				// The pod hasn't terminated yet (e.g., very first crash before
				// k8s records a LastTerminationState). Return exit 1 as a safe default.
				return cs.Name, 1, "Error", true
			}
			return cs.Name, last.ExitCode, last.Reason, true
		}
		return "", 0, "", false
	}

	// Check init containers first.
	if n, ec, r, found := check(pod.Status.InitContainerStatuses); found {
		return n, ec, r, nil
	}
	// Then regular containers.
	if n, ec, r, found := check(pod.Status.ContainerStatuses); found {
		return n, ec, r, nil
	}

	return "", 0, "", fmt.Errorf(
		"pod %s/%s has no container in CrashLoopBackOff state — was Classify called without checking IsCrashLoopBackOff first?",
		pod.Namespace, pod.Name,
	)
}

// classifyByLogs scans the lowercased log text for known keyword patterns
// and returns the most specific FailureType it can determine.
// ConfigError is checked first because config problems often also mention
// application-level errors (e.g., "fatal: no such file or directory").
func classifyByLogs(logsLower string) FailureType {
	for _, kw := range configKeywords {
		if strings.Contains(logsLower, kw) {
			return FailureConfigError
		}
	}
	for _, kw := range appCrashKeywords {
		if strings.Contains(logsLower, kw) {
			return FailureAppCrash
		}
	}
	return FailureUnknown
}

// GetCrashingContainerName is a convenience helper used by other packages
// to get the name of the first crashing container without running a full
// classification.
func GetCrashingContainerName(pod *corev1.Pod) (string, error) {
	name, _, _, err := findCrashingContainer(pod)
	return name, err
}

