// Kube-Sentinel is a Kubernetes controller that automatically detects and
// remediates CrashLoopBackOff events in a cluster.
//
// Build stages:
//   Stage 1: pod watcher — detects CrashLoopBackOff, logs it.
//   Stage 2: failure classifier — reads exit codes and logs.
//   Stage 3: remediation engine — patches resources, restarts pods, rollback.
//   Stage 4 (this file): Prometheus metrics endpoint on :8080/metrics.
//   Stage 5: structured JSON alerting.
package main

import (
	"context"
	"errors"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/hashir/kube-sentinel/internal/classifier"
	"github.com/hashir/kube-sentinel/internal/config"
	"github.com/hashir/kube-sentinel/internal/metrics"
	"github.com/hashir/kube-sentinel/internal/remediator"
	"github.com/hashir/kube-sentinel/internal/watcher"
)

func main() {
	// -------------------------------------------------------------------------
	// 1. Command-line flags
	// -------------------------------------------------------------------------
	// flag.String registers a string flag. Three arguments:
	//   name       — used on the command line  (-config=...)
	//   defaultVal — used when the flag is absent
	//   usage      — shown by `./kube-sentinel -help`
	configPath := flag.String("config", "config.yaml", "Path to the configuration YAML file")
	kubeconfig := flag.String(
		"kubeconfig", "",
		"Path to a kubeconfig file. Leave empty to use in-cluster service-account credentials.",
	)
	metricsAddr := flag.String("metrics-addr", ":8080", "Address for the Prometheus metrics HTTP server")
	flag.Parse()

	// -------------------------------------------------------------------------
	// 2. Structured logger (zap)
	// -------------------------------------------------------------------------
	// zap.NewProduction writes JSON to stdout — ideal for log aggregators.
	// Example line:
	//   {"level":"info","ts":1700000000.0,"msg":"Config loaded","max_restart_attempts":3}
	logger, err := zap.NewProduction()
	if err != nil {
		os.Stderr.WriteString("FATAL: failed to create logger: " + err.Error() + "\n")
		os.Exit(1)
	}
	defer logger.Sync() //nolint:errcheck

	// -------------------------------------------------------------------------
	// 3. Configuration
	// -------------------------------------------------------------------------
	cfg, err := config.Load(*configPath)
	if err != nil {
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
	// The REST config holds the API server address, TLS certs, and auth tokens.
	// Two modes:
	//   In-cluster: credentials are auto-mounted by Kubernetes into every Pod.
	//   Out-of-cluster: kubeconfig file on the developer's workstation.
	var k8sConfig *rest.Config
	if *kubeconfig != "" {
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

	// kubernetes.NewForConfig creates a clientset — a collection of typed API
	// group clients (CoreV1 for Pods, AppsV1 for Deployments, etc.).
	clientset, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		logger.Fatal("Failed to create Kubernetes clientset", zap.Error(err))
	}

	// -------------------------------------------------------------------------
	// 5. Prometheus metrics
	// -------------------------------------------------------------------------
	// We use a custom registry rather than the global default.
	// prometheus.NewRegistry() starts empty — no risk of name collisions with
	// metrics registered by third-party libraries we import.
	registry := prometheus.NewRegistry()

	// Register standard Go runtime and process collectors (goroutines, GC, FDs).
	// These give Prometheus the same baseline metrics as any Go exporter.
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	// Our application metrics (crashes, remediations, rollbacks, durations).
	rec := metrics.New(registry)

	// -------------------------------------------------------------------------
	// 6. Metrics HTTP server
	// -------------------------------------------------------------------------
	// promhttp.HandlerFor serialises all registered metrics on each GET /metrics
	// request, in Prometheus text format (or OpenMetrics if the scraper asks).
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	}))

	// /healthz is a Kubernetes liveness probe endpoint.
	// As long as the process is alive this returns HTTP 200.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:    *metricsAddr,
		Handler: mux,
		// ReadHeaderTimeout prevents Slowloris-style denial-of-service attacks
		// where a client sends request headers extremely slowly to hold open
		// many connections and exhaust server resources.
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Info("Metrics server listening", zap.String("addr", *metricsAddr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("Metrics server exited unexpectedly", zap.Error(err))
		}
	}()

	// -------------------------------------------------------------------------
	// 7. Graceful shutdown via OS signals
	// -------------------------------------------------------------------------
	// signal.NotifyContext cancels ctx on SIGINT (Ctrl+C) or SIGTERM.
	// SIGTERM is the signal Kubernetes sends when it terminates a Pod.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// When ctx is cancelled, give the HTTP server 5 s to finish in-flight
	// /metrics scrapes before closing the listener.
	go func() {
		<-ctx.Done()
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutCancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			logger.Warn("Metrics server did not shut down cleanly", zap.Error(err))
		}
	}()

	logger.Info("Kube-Sentinel starting", zap.String("stage", "4 — metrics endpoint live"))

	// -------------------------------------------------------------------------
	// 8. Classifier setup
	// -------------------------------------------------------------------------
	// K8sLogReader fetches real container logs via the Kubernetes API.
	// In tests it is replaced by a fake reader so no cluster is needed.
	logReader := classifier.NewK8sLogReader(clientset)
	clsf := classifier.New(logger, logReader)

	// -------------------------------------------------------------------------
	// 9. Remediator setup
	// -------------------------------------------------------------------------
	rem := remediator.New(clientset, logger, cfg, rec)

	// -------------------------------------------------------------------------
	// 10. Pod watcher
	// -------------------------------------------------------------------------
	// podHandler is invoked for every pod that enters CrashLoopBackOff.
	// The full pipeline: detect → record metric → classify → remediate.
	// Each event runs in its own goroutine so the informer's event loop is
	// never blocked by slow Kubernetes API calls (log fetches, patches, etc.).
	podHandler := func(pod *corev1.Pod) {
		go func() {
			// Step 1: classify the failure.
			result, err := clsf.Classify(ctx, pod)
			if err != nil {
				logger.Error("Classification failed",
					zap.String("pod", pod.Name),
					zap.String("namespace", pod.Namespace),
					zap.Error(err),
				)
				return
			}

			// Step 2: record the crash-detection counter now that we have
			// the failure_type label from the classifier.
			rec.RecordCrashDetected(pod.Namespace, string(result.FailureType))

			logger.Info("Failure classified",
				zap.String("pod", pod.Name),
				zap.String("namespace", pod.Namespace),
				zap.String("failure_type", string(result.FailureType)),
				zap.Int32("exit_code", result.ExitCode),
			)

			// Step 3: remediate. The remediator records its own action counters
			// and the remediation_duration histogram internally.
			if err := rem.Remediate(ctx, pod, result); err != nil {
				logger.Error("Remediation failed",
					zap.String("pod", pod.Name),
					zap.String("namespace", pod.Namespace),
					zap.String("failure_type", string(result.FailureType)),
					zap.Error(err),
				)
			}
		}()
	}

	w := watcher.New(clientset, logger, podHandler, cfg.NamespacesToWatch)

	// Start blocks until ctx is cancelled or a fatal watcher error occurs.
	if err := w.Start(ctx); err != nil {
		logger.Fatal("Watcher exited with an error", zap.Error(err))
	}

	logger.Info("Kube-Sentinel stopped cleanly")
}
