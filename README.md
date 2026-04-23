# Kube-Sentinel

A Kubernetes controller that watches every pod in your cluster for
`CrashLoopBackOff` events, classifies the root cause, and automatically applies
the right remediation — memory limit patch, pod restart, or deployment rollback
— without human intervention.

---

## Why

`CrashLoopBackOff` is one of the most common Kubernetes failure modes. The
default response is to page an engineer at 3 AM to run `kubectl describe pod`,
read logs, and manually delete or redeploy. Kube-Sentinel automates the whole
loop: it reads the same logs and exit codes an engineer would, applies the
appropriate fix, and fires a structured alert if the problem persists.

---

## Architecture

```
Kubernetes API
      │
      │  list + watch /api/v1/pods  (client-go SharedIndexInformer)
      ▼
┌─────────────────────────────────────────────────────────────────────┐
│                          Kube-Sentinel                              │
│                                                                     │
│  ┌──────────┐   pod enters    ┌─────────────────┐                  │
│  │  Watcher │─CrashLoopBack──▶│   Classifier    │                  │
│  │          │   Off           │                 │                  │
│  │ informer │                 │ exit code 137   │──▶ OOMKilled      │
│  │ AddFunc  │                 │ exit 1 + logs   │──▶ ConfigError    │
│  │ UpdateFn │                 │ "panic/fatal"   │──▶ AppCrash       │
│  └──────────┘                 │ anything else   │──▶ Unknown        │
│                               └────────┬────────┘                  │
│                                        │ Result                    │
│                               ┌────────▼────────┐                  │
│                               │   Remediator    │                  │
│                               │                 │                  │
│                               │ OOMKilled ──────│──▶ patch memory  │
│                               │                 │    +25%, restart │
│                               │ ConfigError ────│──▶ alert only    │
│                               │                 │    (no auto-fix) │
│                               │ AppCrash ───────│──▶ restart ×N    │
│                               │                 │    → rollback    │
│                               │ Unknown ────────│──▶ restart once  │
│                               │                 │    → alert       │
│                               └────────┬────────┘                  │
│                                        │                           │
│                    ┌───────────────────┼────────────────────┐      │
│                    │                   │                    │      │
│             ┌──────▼──────┐   ┌────────▼───────┐           │      │
│             │   Alerter   │   │    Metrics     │           │      │
│             │             │   │                │           │      │
│             │ sliding     │   │ :8080/metrics  │           │      │
│             │ window      │   │                │           │      │
│             │ threshold → │   │ crashes_total  │           │      │
│             │ JSON alert  │   │ remediations   │           │      │
│             │ to stdout   │   │ rollbacks      │           │      │
│             └─────────────┘   │ duration_hist  │           │      │
│                               └────────────────┘           │      │
└─────────────────────────────────────────────────────────────────────┘
```

---

## Features

- **Zero-poll watching** via client-go `SharedIndexInformer` — no API server
  hammering
- **Root-cause classification** using exit codes and log keyword matching
- **Four remediation strategies** mapped to failure types
- **Deployment rollback** using ReplicaSet revision history
- **Sliding-window alerting** — JSON alerts to stdout when a pod exceeds the
  crash threshold within a configurable time window
- **Prometheus metrics** on `:8080/metrics` with Go runtime stats included
- **Kubernetes liveness probe** on `:8080/healthz`
- **Distroless, non-root container image** with zero shell attack surface

---

## Prerequisites

| Tool | Minimum version |
|------|----------------|
| Go | 1.21 |
| Docker | 20.10 |
| kind or minikube | any recent |
| kubectl | 1.26 |

---

## Quick start (kind)

### 1. Create a local cluster

```bash
kind create cluster --name sentinel-dev
```

### 2. Build the image and load it into kind

```bash
docker build -t kube-sentinel:dev .
kind load docker-image kube-sentinel:dev --name sentinel-dev
```

### 3. Apply RBAC

```bash
kubectl apply -f deploy/rbac.yaml
```

`deploy/rbac.yaml`:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: kube-sentinel
  namespace: kube-sentinel
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kube-sentinel
rules:
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get", "list", "watch", "delete"]
- apiGroups: [""]
  resources: ["pods/log"]
  verbs: ["get"]
- apiGroups: [""]
  resources: ["events"]
  verbs: ["create", "patch"]
- apiGroups: ["apps"]
  resources: ["deployments"]
  verbs: ["get", "list", "update", "patch"]
- apiGroups: ["apps"]
  resources: ["replicasets"]
  verbs: ["get", "list"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: kube-sentinel
subjects:
- kind: ServiceAccount
  name: kube-sentinel
  namespace: kube-sentinel
roleRef:
  kind: ClusterRole
  name: kube-sentinel
  apiGroup: rbac.authorization.k8s.io
```

### 4. Deploy Kube-Sentinel

```bash
kubectl create namespace kube-sentinel
kubectl apply -f deploy/deployment.yaml
```

`deploy/deployment.yaml`:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: kube-sentinel
  namespace: kube-sentinel
spec:
  replicas: 1
  selector:
    matchLabels:
      app: kube-sentinel
  template:
    metadata:
      labels:
        app: kube-sentinel
    spec:
      serviceAccountName: kube-sentinel
      containers:
      - name: kube-sentinel
        image: kube-sentinel:dev
        args: ["-config=/etc/kube-sentinel/config.yaml"]
        ports:
        - containerPort: 8080
          name: metrics
        livenessProbe:
          httpGet:
            path: /healthz
            port: 8080
          initialDelaySeconds: 5
          periodSeconds: 10
        resources:
          requests:
            cpu: 50m
            memory: 64Mi
          limits:
            cpu: 200m
            memory: 128Mi
        volumeMounts:
        - name: config
          mountPath: /etc/kube-sentinel
      volumes:
      - name: config
        configMap:
          name: kube-sentinel-config
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: kube-sentinel-config
  namespace: kube-sentinel
data:
  config.yaml: |
    max_restart_attempts: 3
    memory_limit_increase_percent: 25
    crash_window_minutes: 10
    alert_threshold: 3
    namespaces_to_watch: []
```

### 5. Verify it's running

```bash
kubectl -n kube-sentinel logs -f deploy/kube-sentinel
```

Expected startup log:

```json
{"level":"info","ts":1700000000.0,"msg":"Configuration loaded","max_restart_attempts":3,...}
{"level":"info","ts":1700000000.1,"msg":"Metrics server listening","addr":":8080"}
{"level":"info","ts":1700000000.2,"msg":"Kube-Sentinel starting","version":"stage-5"}
{"level":"info","ts":1700000000.3,"msg":"Pod cache synced — watcher is ready","namespace":"<all>"}
```

### 6. Trigger a test crash

```bash
# Deploy a pod that immediately exits
kubectl run crasher \
  --image=busybox \
  --restart=Always \
  -- /bin/sh -c "exit 1"

# Watch Kube-Sentinel detect and remediate it
kubectl -n kube-sentinel logs -f deploy/kube-sentinel
```

---

## Running locally (out-of-cluster)

```bash
# Point at your current kubeconfig context
go run . -kubeconfig ~/.kube/config -config ./config.yaml
```

To run against minikube:

```bash
minikube start
go run . -kubeconfig $(minikube kubectl -- config view --raw --output=json | \
  jq -r '.clusters[0].cluster.server') -config ./config.yaml
# or simply:
go run . -kubeconfig ~/.kube/config
```

---

## Configuration reference

All settings live in `config.yaml`. Every field is optional — the defaults
shown below are used for any field that is absent from the file.

| Field | Type | Default | Description |
|---|---|---|---|
| `max_restart_attempts` | int | `3` | Maximum number of pod restarts for AppCrash before escalating to rollback |
| `memory_limit_increase_percent` | int | `25` | Percentage to increase memory limit for OOMKilled pods (e.g. `25` turns 512 Mi into 640 Mi) |
| `crash_window_minutes` | int | `10` | Rolling time window for counting crashes for alerting purposes |
| `alert_threshold` | int | `3` | Number of crashes in `crash_window_minutes` that triggers a critical alert |
| `namespaces_to_watch` | []string | `[]` | Namespaces to monitor. Empty list means **all namespaces** |

Example with all fields set:

```yaml
max_restart_attempts: 2
memory_limit_increase_percent: 50
crash_window_minutes: 5
alert_threshold: 2
namespaces_to_watch:
  - default
  - production
  - staging
```

---

## Prometheus metrics

Kube-Sentinel exposes metrics at `http://<pod-ip>:8080/metrics`.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `kubesentinel_crashes_detected_total` | Counter | `namespace`, `failure_type` | Total CrashLoopBackOff events detected |
| `kubesentinel_remediations_total` | Counter | `action` | Remediation actions taken (`memory_patch`, `pod_restart`, `rollback`, `alert_only`) |
| `kubesentinel_rollbacks_total` | Counter | — | Deployment rollbacks performed |
| `kubesentinel_remediation_duration_seconds` | Histogram | `failure_type` | Time taken per remediation, in seconds |

Standard Go metrics (`go_goroutines`, `go_gc_duration_seconds`, etc.) and
process metrics (`process_resident_memory_bytes`, etc.) are also included.

### Example scrape output

```
# HELP kubesentinel_crashes_detected_total Total number of CrashLoopBackOff pods detected
# TYPE kubesentinel_crashes_detected_total counter
kubesentinel_crashes_detected_total{failure_type="AppCrash",namespace="default"} 5
kubesentinel_crashes_detected_total{failure_type="OOMKilled",namespace="production"} 2

# HELP kubesentinel_remediations_total Total number of remediation actions taken
# TYPE kubesentinel_remediations_total counter
kubesentinel_remediations_total{action="memory_patch"} 2
kubesentinel_remediations_total{action="pod_restart"} 4
kubesentinel_remediations_total{action="rollback"} 1

# HELP kubesentinel_rollbacks_total Total number of Deployment rollbacks performed
# TYPE kubesentinel_rollbacks_total counter
kubesentinel_rollbacks_total 1

# HELP kubesentinel_remediation_duration_seconds Duration of remediation actions in seconds
# TYPE kubesentinel_remediation_duration_seconds histogram
kubesentinel_remediation_duration_seconds_bucket{failure_type="OOMKilled",le="0.25"} 2
kubesentinel_remediation_duration_seconds_bucket{failure_type="OOMKilled",le="0.5"} 2
kubesentinel_remediation_duration_seconds_sum{failure_type="OOMKilled"} 0.183
kubesentinel_remediation_duration_seconds_count{failure_type="OOMKilled"} 2
```

Add this Prometheus scrape config to scrape Kube-Sentinel:

```yaml
scrape_configs:
  - job_name: kube-sentinel
    kubernetes_sd_configs:
      - role: pod
        namespaces:
          names: [kube-sentinel]
    relabel_configs:
      - source_labels: [__meta_kubernetes_pod_label_app]
        regex: kube-sentinel
        action: keep
```

---

## Alert output

When a pod exceeds `alert_threshold` crashes within `crash_window_minutes`,
Kube-Sentinel writes one JSON line to stdout per alert:

```json
{
  "level": "CRITICAL",
  "timestamp": "2024-01-15T03:42:17Z",
  "alert": "crash_threshold_exceeded",
  "pod": "web-api-7d9f8b-xk2pq",
  "namespace": "production",
  "failure_type": "AppCrash",
  "exit_code": 1,
  "crash_count": 4,
  "window_minutes": 10,
  "threshold": 3,
  "remediation_history": [
    "pod_restart",
    "pod_restart",
    "pod_restart",
    "rollback"
  ]
}
```

To route these alerts to PagerDuty or Slack, pipe stdout through a log
aggregator or use a tool such as `jq` + `alertmanager` webhook:

```bash
# Example: forward CRITICAL lines to a webhook
kubectl -n kube-sentinel logs -f deploy/kube-sentinel \
  | grep '"level":"CRITICAL"' \
  | xargs -I{} curl -s -X POST https://hooks.example.com/alerts -d '{}'
```

---

## How it works

### Stage 1 — Watcher
Uses a `client-go` `SharedIndexInformer` to maintain a local cache of all pods,
kept in sync via a streaming Kubernetes watch. `AddFunc` and `UpdateFunc`
callbacks fire whenever a pod enters `CrashLoopBackOff`. No polling.

### Stage 2 — Classifier
Reads `pod.Status.ContainerStatuses[*].LastTerminationState.Terminated` for the
exit code, then fetches the last 50 lines of logs via the `pods/log` sub-resource.
Classifies into one of four types using exit-code rules and case-insensitive
keyword matching on the log text.

### Stage 3 — Remediator
Dispatches to the correct handler based on failure type:
- **OOMKilled**: walks the Pod→ReplicaSet→Deployment owner chain, increases the
  container's memory limit by `memory_limit_increase_percent`, and deletes the
  pod so the ReplicaSet creates a replacement with the new limit.
- **ConfigError**: logs a critical error and fires an alert. No auto-fix —
  config problems require human intervention.
- **AppCrash**: restarts the pod up to `max_restart_attempts` times. On
  exhaustion, queries ReplicaSet revision history, identifies the previous
  stable image by the `deployment.kubernetes.io/revision` annotation, and
  patches the Deployment to roll back.
- **Unknown**: restarts once. If the pod crashes again, alerts without further
  auto-remediation.

### Stage 4 — Metrics
Exposes four Prometheus metrics on `:8080/metrics` using a custom registry.
The remediator records action counters and duration histograms internally.
The main pipeline records crash-detection counters after each classification.

### Stage 5 — Alerter
Maintains a per-pod sliding window of crash timestamps. After each crash event,
old timestamps outside the window are pruned. If the remaining count exceeds
`alert_threshold`, a JSON object is written to stdout and the pod's remediation
history is included.

---

## Project layout

```
kube-sentinel/
├── main.go                    Entry point — wires all components together
├── config.yaml                Default configuration
├── Dockerfile                 Multi-stage, distroless, non-root
├── go.mod / go.sum
└── internal/
    ├── config/                YAML config loader with defaults
    ├── watcher/               client-go informer — detects CrashLoopBackOff
    ├── classifier/            Exit code + log keyword classifier
    ├── remediator/            OOMKilled / ConfigError / AppCrash / Unknown handlers
    ├── metrics/               Prometheus Recorder
    └── alerter/               Sliding-window crash counter + JSON alert emitter
```

---

## Running the tests

```bash
# All packages, verbose
go test ./... -v

# A specific package
go test ./internal/alerter/... -v

# With race detector (recommended before committing)
go test ./... -race
```

All tests run without a real Kubernetes cluster. The watcher and classifier
tests use fabricated pod objects; the remediator tests use
`k8s.io/client-go/kubernetes/fake`; the alerter tests inject a controllable
clock via the `WithNow` option.
