// Package watcher implements the Kubernetes pod watcher for Kube-Sentinel.
//
// Rather than polling the API server on a timer, it uses client-go's
// SharedIndexInformer — a local cache that stays in sync with the cluster
// via a long-running WATCH stream. This is the idiomatic, efficient way to
// observe Kubernetes resource changes.
//
// How informers work (short version):
//  1. The informer issues a LIST to get all current pods.
//  2. It then opens a streaming WATCH to receive incremental changes.
//  3. Changes are stored in an in-memory cache and forwarded to registered
//     event handlers (Add / Update / Delete callbacks).
//  4. Periodically (resyncPeriod) the informer re-lists to guard against
//     dropped watch events.
package watcher

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// resyncPeriod is how often the informer performs a full re-list of pods to
// catch any events that may have been missed on the watch stream.
const resyncPeriod = 10 * time.Minute

// PodHandler is the signature of the callback invoked when a pod enters
// CrashLoopBackOff. The pod argument is a snapshot of the pod's full state
// at the moment of detection.
type PodHandler func(pod *corev1.Pod)

// Watcher watches Kubernetes pods across one or more namespaces and calls
// a PodHandler whenever a pod enters (or is already in) CrashLoopBackOff.
type Watcher struct {
	// client is the Kubernetes API client. Using the Interface type (rather
	// than the concrete *kubernetes.Clientset) makes the watcher easy to
	// test with a fake client.
	client kubernetes.Interface

	// logger is the structured logger used throughout the watcher.
	logger *zap.Logger

	// handler is invoked for every pod detected in CrashLoopBackOff.
	handler PodHandler

	// namespaces lists which namespaces to watch. An empty slice is treated
	// as "watch all namespaces".
	namespaces []string
}

// New constructs a Watcher.
//
//   - client:     a real or fake Kubernetes clientset
//   - logger:     a zap structured logger
//   - handler:    function to call when CrashLoopBackOff is detected
//   - namespaces: namespaces to watch; pass nil or [] for all namespaces
func New(client kubernetes.Interface, logger *zap.Logger, handler PodHandler, namespaces []string) *Watcher {
	return &Watcher{
		client:     client,
		logger:     logger,
		handler:    handler,
		namespaces: namespaces,
	}
}

// Start begins watching pods. It launches one goroutine per namespace and
// blocks until either the context is cancelled (clean shutdown) or one of
// the namespace watchers encounters a fatal error.
func (w *Watcher) Start(ctx context.Context) error {
	// When no namespaces are specified, we watch everything.
	// In client-go, passing an empty string "" as the namespace to
	// NewListWatchFromClient tells it to watch across all namespaces.
	namespacesToWatch := w.namespaces
	if len(namespacesToWatch) == 0 {
		namespacesToWatch = []string{""}
	}

	w.logger.Info("Starting pod watcher",
		zap.Int("namespace_count", len(namespacesToWatch)),
		zap.Strings("namespaces", namespacesToWatch),
	)

	// errCh receives fatal errors from namespace watcher goroutines.
	// Buffered so each goroutine can send without blocking even if we
	// haven't started the select yet.
	errCh := make(chan error, len(namespacesToWatch))

	for _, ns := range namespacesToWatch {
		// IMPORTANT: capture the loop variable.
		// In Go, `ns` is a single variable reused each iteration. Without
		// this `ns := ns` re-declaration, every goroutine closure would
		// reference the same variable and all see the last value the loop
		// assigned to it.
		ns := ns
		go func() {
			if err := w.watchNamespace(ctx, ns); err != nil {
				errCh <- fmt.Errorf("namespace %q watcher: %w", ns, err)
			}
		}()
	}

	// Block until graceful shutdown or error.
	select {
	case <-ctx.Done():
		w.logger.Info("Pod watcher shutting down", zap.String("reason", ctx.Err().Error()))
		return nil
	case err := <-errCh:
		return err
	}
}

// watchNamespace creates and runs a SharedIndexInformer for pods in the
// given namespace. It blocks until ctx is cancelled.
// Passing "" as namespace watches all namespaces.
func (w *Watcher) watchNamespace(ctx context.Context, namespace string) error {
	// ListWatch defines two operations the informer uses internally:
	//   - List:  fetches all pods (used on startup and resync)
	//   - Watch: opens a streaming connection to receive incremental changes
	//
	// NewListWatchFromClient builds both using the Core API REST client.
	listWatch := cache.NewListWatchFromClient(
		w.client.CoreV1().RESTClient(), // REST client for the "core" API group (pods, services, etc.)
		"pods",                         // resource type (plural, lowercase)
		namespace,                      // namespace scope; "" = cluster-wide
		fields.Everything(),            // no field selector = get all pods
	)

	// NewSharedIndexInformer wraps the ListWatch with a local cache.
	// Multiple callers can share the same informer without extra API calls.
	// The cache.Indexers argument enables fast O(1) namespace-based lookups.
	informer := cache.NewSharedIndexInformer(
		listWatch,
		&corev1.Pod{}, // the Go type of the objects we're caching
		resyncPeriod,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)

	// AddEventHandler registers our callbacks.
	// In client-go v0.27+ this returns a registration handle and an error.
	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		// AddFunc fires for each pod that exists when the informer first
		// starts (from the initial LIST), and for newly created pods.
		// This ensures we catch pods that were already crashing on launch.
		AddFunc: func(obj interface{}) {
			// Type assertion: convert the opaque interface{} to *corev1.Pod.
			// The second return value `ok` is false if the assertion fails —
			// we guard against that to avoid a panic.
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				return
			}
			if IsCrashLoopBackOff(pod) {
				w.logger.Info("Detected existing pod in CrashLoopBackOff",
					zap.String("pod", pod.Name),
					zap.String("namespace", pod.Namespace),
				)
				w.handler(pod)
			}
		},

		// UpdateFunc fires whenever a pod's spec or status changes.
		// This is the primary trigger — a pod transitions from Running
		// (or Pending) into CrashLoopBackOff via an Update event.
		UpdateFunc: func(_, newObj interface{}) {
			pod, ok := newObj.(*corev1.Pod)
			if !ok {
				return
			}
			if IsCrashLoopBackOff(pod) {
				w.logger.Info("Pod entered CrashLoopBackOff",
					zap.String("pod", pod.Name),
					zap.String("namespace", pod.Namespace),
				)
				w.handler(pod)
			}
		},
	})
	if err != nil {
		return fmt.Errorf("registering event handler: %w", err)
	}

	// Run starts the informer's internal list-watch loop in the foreground.
	// It returns when ctx.Done() is closed (i.e., when we're shutting down).
	// We start it in a goroutine so we can call WaitForCacheSync below.
	go informer.Run(ctx.Done())

	// WaitForCacheSync blocks until the informer has finished its initial
	// LIST and populated the local cache. Without this, event handlers could
	// be called before the cache is ready, leading to missed or duplicate events.
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		// ctx was cancelled before the cache finished syncing — this is fine
		// during a fast shutdown, but we return an error to signal it.
		return fmt.Errorf("cache sync timed out or cancelled (namespace: %q)", namespace)
	}

	nsLabel := namespace
	if nsLabel == "" {
		nsLabel = "<all>"
	}
	w.logger.Info("Pod cache synced — watcher is ready", zap.String("namespace", nsLabel))

	// Block until the context is cancelled (SIGTERM / Ctrl+C).
	<-ctx.Done()
	return nil
}

// IsCrashLoopBackOff returns true if any container in the pod — including
// init containers — is currently in the CrashLoopBackOff waiting state.
//
// In Kubernetes, CrashLoopBackOff appears in ContainerStatus.State.Waiting
// after the container has crashed repeatedly and kubelet is applying an
// exponential back-off before the next restart attempt.
func IsCrashLoopBackOff(pod *corev1.Pod) bool {
	// Init containers run before the main containers start.
	// They can also enter CrashLoopBackOff (e.g., a broken DB migration job).
	for _, cs := range pod.Status.InitContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff" {
			return true
		}
	}
	// Regular (main application) containers.
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff" {
			return true
		}
	}
	return false
}
