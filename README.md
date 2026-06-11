# GracefulSet

A Kubernetes CRD and controller for managing stateful applications that require zero-disruption upgrades and scaling.

## What it does

GracefulSet creates new pods when the spec changes (version upgrade) but **never kills existing pods**. Old pods continue running until they finish their work — they can wait for the process to exit, for an application-reported drain signal, or for a TTL to expire.

This solves the problem of upgrading or scaling applications that hold long-lived sessions or in-flight work, where killing a pod means losing active user connections or unfinished processing.

## How it works

```
Upgrade v1 → v2:

Before:  [v1-pod-a] [v1-pod-b] [v1-pod-c]    (3 replicas, all v1)
During:  [v1-pod-a] [v1-pod-b] [v1-pod-c]    (draining, still serving)
         [v2-pod-d] [v2-pod-e] [v2-pod-f]    (new, accepting traffic)
After:   [v2-pod-d] [v2-pod-e] [v2-pod-f]    (v1 pods finished and removed)
```

```
Scale down 5 → 2:

Before:  [pod-a] [pod-b] [pod-c] [pod-d] [pod-e]   (5 active)
During:  [pod-a] [pod-b]                            (2 active)
         [pod-c] [pod-d] [pod-e]                    (draining, still serving)
After:   [pod-a] [pod-b]                            (excess pods finished and removed)
```

On scale-down the **oldest pods are drained first**, leaving the newest (warmest) pods serving.

## Key Features

- **Zero-disruption upgrades** — old pods are never forcefully terminated
- **Zero-disruption scale-down** — excess pods drain instead of being killed
- **Version coexistence** — multiple versions run simultaneously during transition
- **Drain policies** — WaitForCompletion, WaitForDrain, TTL, or Manual
- **Scale subresource** — works with `kubectl scale` and HorizontalPodAutoscaler
- **Leader election** — run multiple controller replicas for high availability
- **Status tracking** — see active vs draining pods per version
- **Helm-compatible** — use as a drop-in replacement for Jobs/Deployments in Helm charts

## When to use it

GracefulSet fits when **all three** are true:
1. The pod holds **ephemeral state in memory** (a session, in-flight job, warm cache)
2. **Killing the pod loses that state** or disrupts a user
3. The state **eventually completes** on its own (session ends, job finishes, batch completes)

Good fits:
- Session-holding apps (interactive UI nodes, WebSocket servers, VDI/AppStream)
- Long-running compute (ML training, batch ETL, transcoding, report generation)
- Message consumers / workers that must finish in-flight messages
- Workflow runners and CI/CD runners

Not a fit (use StatefulSet or a dedicated operator instead):
- Databases — need single-writer semantics + stable storage
- Message brokers (Kafka, RabbitMQ, ZooKeeper) — need stable identity + storage

## Comparison with Deployment, OpenKruise, and Argo Rollouts

| | Deployment | OpenKruise CloneSet | Argo Rollouts | GracefulSet |
|--|-----------|--------------------|--------------| ------------|
| Upgrade model | Rolling replace (kills old pods) | Controlled replace, in-place updates | Canary / blue-green progressive | **Old pods never replaced** |
| Version coexistence | Transient during rollout | Transient during rollout | Transient during rollout | **Indefinite until work completes** |
| Who decides a pod dies | Controller (on its schedule) | Controller (+ PreDelete hook) | Controller (+ analysis) | **The application** (drain signal / process exit) |
| Built-in drain check | No (just terminationGracePeriod) | PreDelete hook you clear externally | No | **Yes — HTTP poll / process exit / TTL** |
| Scale-down behavior | Kills excess pods | Configurable ordering, still deletes | Kills excess pods | **Drains excess pods, app exits cleanly** |
| Needs stable storage/identity | No | Optional (Advanced StatefulSet) | No | No |

### How it differs in one sentence

- **Deployment / Argo Rollouts / CloneSet** answer: *"How do I safely replace old pods with new ones at a controlled pace?"*
- **GracefulSet** answers: *"How do I let the old generation of pods live until their in-flight work organically finishes, without the controller ever forcing them out?"*

### When to choose which

| Need | Use |
|------|-----|
| Progressive/canary delivery with traffic shifting and metric analysis | **Argo Rollouts** |
| Advanced rolling updates, in-place image swaps, broad workload features | **OpenKruise** |
| Standard stateless rolling update | **Deployment** |
| Old pods must finish long-lived sessions/work before going away, with zero hook plumbing | **GracefulSet** |

### Relationship to OpenKruise

Much of GracefulSet's behavior can be approximated with OpenKruise CloneSet + a `PreDelete` lifecycle hook plus an external process that clears the hook once a pod has drained. GracefulSet's distinction is making this a first-class, zero-plumbing model: generational coexistence is the default behavior, and the drain check (HTTP poll / process exit / TTL) is built in rather than wired up with sidecars and hook-clearing controllers.

## Usage

```yaml
apiVersion: apps.gracefulset.io/v1alpha1
kind: GracefulSet
metadata:
  name: my-app
  namespace: default
spec:
  replicas: 3
  version: "2024.1.0"
  drainPolicy:
    mode: WaitForDrain      # WaitForCompletion | WaitForDrain | TTL | Manual
    ttl: 24h                # safety cap (TTL mode, or max drain time for WaitForDrain)
    maxDrainingPods: 10     # safety limit on simultaneously draining pods
    drainCheck:             # only used with WaitForDrain
      path: /drain-status
      port: 8080
      scheme: HTTP
      periodSeconds: 30
      jsonField: inflight
  selector:
    matchLabels:
      app: my-app
  template:
    metadata:
      labels:
        app: my-app
        gracefulset.io/name: my-app
    spec:
      containers:
        - name: app
          image: registry/my-app:2024.1.0
          ports:
            - containerPort: 8080
```

## Drain Policies

| Mode | Behavior | Best for |
|------|----------|----------|
| `WaitForCompletion` | Old pods run until the container process exits (Succeeded/Failed), then are removed | Batch jobs, workers that self-terminate |
| `WaitForDrain` | Controller polls an HTTP endpoint on each pod and removes it only when the app reports zero in-flight work | Long-running session/consumer apps that don't self-exit |
| `TTL` | Old pods are deleted after the `ttl` duration | Sessions that should expire after a fixed window |
| `Manual` | Old pods run until an operator deletes them | Full manual control |

### WaitForDrain contract

Your application exposes an HTTP endpoint (default `/drain-status` on port `8080`) that returns JSON. The controller polls it every `periodSeconds` and removes the pod when the configured `jsonField` reports zero (or `true` for a boolean field).

```json
// still serving — keep the pod
{"inflight": 5}

// drained — safe to remove
{"inflight": 0}
```

A `ttl` acts as a safety cap: even if a pod never reports drained, it is force-removed once the TTL elapses.

## Scaling

GracefulSet implements the Kubernetes scale subresource, so both of these work:

```bash
# Manual scale
kubectl scale gracefulset my-app --replicas=5 -n default
```

```yaml
# HorizontalPodAutoscaler
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: my-app
spec:
  scaleTargetRef:
    apiVersion: apps.gracefulset.io/v1alpha1
    kind: GracefulSet
    name: my-app
  minReplicas: 2
  maxReplicas: 10
  metrics:
    - type: Resource
      resource:
        name: cpu
        target:
          type: Utilization
          averageUtilization: 70
```

Scale-up creates new pods of the current version. Scale-down marks the oldest excess pods as draining (they keep running and are handled by the drain policy) rather than killing them.

## Status

```yaml
status:
  activeVersion: "2024.1.0"
  readyReplicas: 3
  totalPods: 5
  drainingPods: 2
  drainingVersions:
    - version: "2024.0.9"
      pods: 2
      readyPods: 2
      oldestPodCreation: "2026-06-11T08:00:00Z"
  conditions:
    - type: Available
      status: "True"
      message: "3/3 replicas ready"
    - type: Draining
      status: "True"
      message: "2 pods from old versions still running"
```

## Installation

```bash
# Install CRD
kubectl apply -f config/crd/gracefulset.yaml

# Install RBAC + controller
kubectl apply -f config/rbac/role.yaml
kubectl apply -f config/manager/deployment.yaml
```

The controller deployment runs 2 replicas with leader election enabled, so only one instance reconciles at a time while the other stands by.

## Architecture

```
┌──────────────────────────────────────────────┐
│  GracefulSet Controller (Deployment, 2x HA)   │
│                                              │
│  Watches: GracefulSet resources              │
│  Creates: Pods (owned by the GracefulSet)    │
│  Manages: version tracking, scale-down drain, │
│           drain policy lifecycle              │
└──────────────────────────────────────────────┘
```

## Pod Labels

The controller manages these labels on pods it creates:

| Label | Meaning |
|-------|---------|
| `gracefulset.io/name` | Owning GracefulSet name |
| `gracefulset.io/version` | The version the pod was created for |
| `gracefulset.io/draining` | Set to `true` when a pod is marked for draining (upgrade or scale-down) |

## Project Structure

```
gracefulset/
├── api/v1alpha1/            CRD type definitions + deepcopy
├── internal/controller/     Reconciliation logic
├── config/
│   ├── crd/                 CRD manifest
│   ├── rbac/                RBAC permissions
│   ├── manager/             Controller deployment
│   └── samples/             Example GracefulSet resources
├── Dockerfile
├── Makefile
└── main.go
```

## Development

```bash
# Resolve dependencies
go mod tidy

# Compile
go build ./...

# Install the CRD, then run the controller locally against your kubeconfig
kubectl apply -f config/crd/gracefulset.yaml
go run ./main.go

# Build and push the container image
make docker-build docker-push IMG=<registry>/gracefulset-controller:<tag>

# Deploy to cluster
make deploy IMG=<registry>/gracefulset-controller:<tag>
```

## Roadmap

- Pod disruption budget awareness during drain
- Configurable scale-down ordering (oldest-first, by readiness, by zone)
- Metrics for drain duration and pods-per-version
- Webhook validation for spec fields
