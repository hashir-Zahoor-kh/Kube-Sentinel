// Kube-Sentinel is a Kubernetes controller that automatically detects and
// remediates CrashLoopBackOff events in a cluster.
//
// Build stages:
//   Stage 1: pod watcher — detects CrashLoopBackOff, logs it.
//   Stage 2: failure classifier — reads exit codes and logs.
//   Stage 3: remediation engine — patches resources, restarts pods, rollback.
//   Stage 4: Prometheus metrics endpoint on :8080/metrics.
//   Stage 5 (this file): structured JSON alerting when thresholds are exceeded.
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

	"github.com/hashir/kube-sentinel/internal/alerter"
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
	configPath := flag.String("config", "config.yaml", "Path to the configuration YAML file")
	kubeconfig := flag.String(
		"kubeconfig", "",
		"Path to a kubeconfig file. Leave empty to use in-cluster service-account credentials.",
	)
	metricsAddr := flag.String("metrics-addr", ":8080", "Address for the Prometheus metrics and healthz HTTP server")
	flag.Parse()

	// -------------------------------------------------------------------------
	// 2. Structured logger
	// -------------------------------------------------------------------------
	// zap.NewProduction writes JSON to stdout, one object per log line.
	// That format is ingested directly by Loki, Datadog, Splunk, etc.
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
		zap.Int("crash_window_minutes", cfg.CrashWindowMinutes),
		zap.Strings("namespaces_to_watch", cfg.NamespacesToWatch),
	)

	// -------------------------------------------------------------------------
	// 4. Kubernetes client
	// -------------------------------------------------------------------------
	var k8sConfig *rest.Config
	if *kubeconfig != "" {
		// Out-of-cluster mode: read kubeconfig from disk (local development).
		k8sConfig, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
	} else {
		// In-cluster mode: Kubernetes auto-mounts credentials into the Pod.
		k8sConfig, err = rest.InClusterConfig()
	}
	if err != nil {
		logger.Fatal("Failed to build Kubernetes client config",
			zap.Error(err),
			zap.Bool("in_cluster_mode", *kubeconfig == ""),
		)
	}
	clientset, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		logger.Fatal("Failed to create Kubernetes clientset", zap.Error(err))
	}

	// -------------------------------------------------------------------------
	// 5. Prometheus metrics
	// -------------------------------------------------------------------------
	// Custom registry instead of the global default — avoids any collision
	// with metrics registered by imported packages.
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	rec := metrics.New(registry)

	// -------------------------------------------------------------------------
	// 6. Alerter
	// -------------------------------------------------------------------------
	// Alerts are written as newline-delimited JSON to os.Stdout.
	// Any log aggregator can parse them with a simple JSON filter.
	alrt := alerter.New(cfg, logger, os.Stdout)

	// -------------------------------------------------------------------------
	// 7. Metrics + healthz HTTP server
	// -------------------------------------------------------------------------
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	srv := &http.Server{
		Addr:              *metricsAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		logger.Info("Metrics server listening", zap.String("addr", *metricsAddr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("Metrics server exited unexpectedly", zap.Error(err))
		}
	}()

	// -------------------------------------------------------------------------
	// 8. Graceful shutdown
	// -------------------------------------------------------------------------
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go func() {
		<-ctx.Done()
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutCancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			logger.Warn("Metrics server did not shut down cleanly", zap.Error(err))
		}
	}()

	logger.Info("Kube-Sentinel starting", zap.String("version", "stage-5"))

	// -------------------------------------------------------------------------
	// 9. Classifier
	// -------------------------------------------------------------------------
	logReader := classifier.NewK8sLogReader(clientset)
	clsf := classifier.New(logger, logReader)

	// -------------------------------------------------------------------------
	// 10. Remediator
	// -------------------------------------------------------------------------
	rem := remediator.New(clientset, logger, cfg, rec, alrt)

	// -------------------------------------------------------------------------
	// 11. Pod watcher
	// -------------------------------------------------------------------------
	// Full pipeline per event: detect → classify → record metrics + alert → remediate.
	// Each pod runs in its own goroutine so the informer loop is never blocked.
	podHandler := func(pod *corev1.Pod) {
		go func() {
			// Step 1: determine failure type.
			result, err := clsf.Classify(ctx, pod)
			if err != nil {
				logger.Error("Classification failed",
					zap.String("pod", pod.Name),
					zap.String("namespace", pod.Namespace),
					zap.Error(err),
				)
				return
			}

			// Step 2: record crash metric (now that we know failure_type).
			rec.RecordCrashDetected(pod.Namespace, string(result.FailureType))

			// Step 3: record crash in the alerter's sliding window.
			// The alerter fires a JSON alert if the threshold is exceeded.
			alrt.RecordCrash(pod, result)

			logger.Info("Failure classified",
				zap.String("pod", pod.Name),
				zap.String("namespace", pod.Namespace),
				zap.String("failure_type", string(result.FailureType)),
				zap.Int32("exit_code", result.ExitCode),
			)

			// Step 4: apply remediation.
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
	if err := w.Start(ctx); err != nil {
		logger.Fatal("Watcher exited with an error", zap.Error(err))
	}

	logger.Info("Kube-Sentinel stopped cleanly")
}
