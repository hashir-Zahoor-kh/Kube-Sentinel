// Kube-Sentinel is a Kubernetes controller that automatically detects and
// remediates CrashLoopBackOff events in a cluster.
//
// Build stages:
//   Stage 1: pod watcher — detects CrashLoopBackOff, logs it.
//   Stage 2 (this file): failure classifier — reads exit codes and logs.
//   Stage 3: remediation engine — patches resources, restarts pods, rollback.
//   Stage 4: Prometheus metrics endpoint on :8080/metrics.
//   Stage 5: structured JSON alerting.
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/hashir/kube-sentinel/internal/classifier"
	"github.com/hashir/kube-sentinel/internal/config"
	"github.com/hashir/kube-sentinel/internal/watcher"
)

func main() {
	// -------------------------------------------------------------------------
	// 1. Command-line flags
	// -------------------------------------------------------------------------
	// flag.String registers a string flag. The three arguments are:
	//   name        — the flag name used on the command line (-config=...)
	//   defaultVal  — value used when the flag is not provided
	//   usage       — shown by `./kube-sentinel -help`
	configPath := flag.String("config", "config.yaml", "Path to the configuration YAML file")
	kubeconfig := flag.String(
		"kubeconfig", "",
		"Path to a kubeconfig file. Leave empty to use in-cluster service-account credentials.",
	)
	flag.Parse() // parse os.Args[1:] and populate the flag variables

	// -------------------------------------------------------------------------
	// 2. Structured logger (zap)
	// -------------------------------------------------------------------------
	// zap.NewProduction creates a logger that writes JSON to stdout.
	// JSON output is easy to ingest into log aggregators (Loki, Datadog, etc.).
	// Example output:
	//   {"level":"info","ts":1700000000.0,"msg":"Config loaded","max_restart_attempts":3}
	logger, err := zap.NewProduction()
	if err != nil {
		// The structured logger isn't ready yet — fall back to plain stderr.
		os.Stderr.WriteString("FATAL: failed to create logger: " + err.Error() + "\n")
		os.Exit(1)
	}
	// Sync flushes any buffered log entries. `defer` ensures it runs even if
	// we return early. The error is intentionally ignored here — a sync
	// failure on stdout/stderr is harmless.
	defer logger.Sync() //nolint:errcheck

	// -------------------------------------------------------------------------
	// 3. Configuration
	// -------------------------------------------------------------------------
	cfg, err := config.Load(*configPath)
	if err != nil {
		// logger.Fatal logs at ERROR level and then calls os.Exit(1).
		// zap.Error wraps a Go error as a structured log field.
		logger.Fatal("Failed to load configuration",
			zap.String("path", *configPath),
			zap.Error(err),
		)
	}
	logger.Info("Configuration loaded",
		zap.Int("max_restart_attempts", cfg.MaxRestartAttempts),
		zap.Int("memory_limit_increase_percent", cfg.MemoryLimitIncreasePercent),
		zap.Int("alert_threshold", cfg.AlertThreshold),
		zap.Strings("namespaces_to_watch", cfg.NamespacesToWatch),
	)

	// -------------------------------------------------------------------------
	// 4. Kubernetes client
	// -------------------------------------------------------------------------
	// The REST client config holds the API server address, TLS certificates,
	// and authentication tokens. There are two sources:
	//
	//   In-cluster (default): when Kube-Sentinel runs as a Pod, Kubernetes
	//   mounts a service-account token and CA cert at a well-known path.
	//   rest.InClusterConfig() reads these automatically.
	//
	//   Out-of-cluster (local dev): a kubeconfig file (usually ~/.kube/config)
	//   contains the same information for a developer's workstation.
	var k8sConfig *rest.Config
	if *kubeconfig != "" {
		// BuildConfigFromFlags reads the kubeconfig file at the given path.
		// The first argument is the API server URL override — empty means
		// "use whatever is in the kubeconfig".
		k8sConfig, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
	} else {
		k8sConfig, err = rest.InClusterConfig()
	}
	if err != nil {
		logger.Fatal("Failed to build Kubernetes client config",
			zap.Error(err),
			zap.Bool("in_cluster_mode", *kubeconfig == ""),
		)
	}

	// kubernetes.NewForConfig creates a "clientset" — a struct that contains
	// a typed client for every Kubernetes API group. We use:
	//   clientset.CoreV1()   for Pods, Services, ConfigMaps …
	//   clientset.AppsV1()   for Deployments, ReplicaSets …
	clientset, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		logger.Fatal("Failed to create Kubernetes clientset", zap.Error(err))
	}

	// -------------------------------------------------------------------------
	// 5. Graceful shutdown via OS signals
	// -------------------------------------------------------------------------
	// signal.NotifyContext returns a context that is automatically cancelled
	// when the process receives SIGINT (Ctrl+C) or SIGTERM (sent by Kubernetes
	// when it wants to terminate a Pod). When the context is cancelled, every
	// component that accepts ctx will begin shutting down cleanly.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel() // always release the signal registration

	logger.Info("Kube-Sentinel starting", zap.String("stage", "2 — watcher + classifier"))

	// -------------------------------------------------------------------------
	// 6. Classifier setup
	// -------------------------------------------------------------------------
	// K8sLogReader fetches real container logs from the Kubernetes API.
	// In tests, this is swapped for a fake reader so no cluster is needed.
	logReader := classifier.NewK8sLogReader(clientset)
	clsf := classifier.New(logger, logReader)

	// -------------------------------------------------------------------------
	// 7. Pod watcher
	// -------------------------------------------------------------------------
	// podHandler is called for every pod that enters CrashLoopBackOff.
	// Stage 2: call the classifier and log the result.
	// Stage 3 will pass the result to the remediator.
	podHandler := func(pod *corev1.Pod) {
		// Run the classification in a goroutine so the watcher event handler
		// returns quickly and doesn't block the informer's event loop.
		go func() {
			result, err := clsf.Classify(ctx, pod)
			if err != nil {
				logger.Error("Classification failed",
					zap.String("pod", pod.Name),
					zap.String("namespace", pod.Namespace),
					zap.Error(err),
				)
				return
			}
			logger.Warn("CrashLoopBackOff classified — remediation not yet wired (Stage 2)",
				zap.String("pod", pod.Name),
				zap.String("namespace", pod.Namespace),
				zap.String("container", result.ContainerName),
				zap.String("failure_type", string(result.FailureType)),
				zap.Int32("exit_code", result.ExitCode),
			)
		}()
	}

	w := watcher.New(clientset, logger, podHandler, cfg.NamespacesToWatch)

	// Start blocks until ctx is cancelled or a fatal watcher error occurs.
	if err := w.Start(ctx); err != nil {
		logger.Fatal("Watcher exited with an error", zap.Error(err))
	}

	logger.Info("Kube-Sentinel stopped cleanly")
}
