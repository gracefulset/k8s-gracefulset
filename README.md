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
- **Traffic cut-off** — draining pods are taken out of Service endpoints via a readiness gate, and an optional drain hook tells the app to stop taking new work
- **Disruption protection** — draining pods are annotated so Karpenter and Cluster Autoscaler don't evict them during consolidation
- **Graceful deletion** — deleting a GracefulSet drains its pods instead of killing them
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
| Built-in drain check | No (preStop + terminationGracePeriod) | PreDelete hook you clear externally | No | **Yes — HTTP poll / process exit / TTL** |
| Scale-down behavior | Kills excess pods | Configurable ordering, still deletes | Kills excess pods | **Drains excess pods, app exits cleanly** |
| Needs stable storage/identity | No | Optional (Advanced StatefulSet) | No | No |

### Deployment with a long `terminationGracePeriodSeconds`

A Deployment with a `preStop` hook that polls a drain endpoint and a grace period of hours gets part of the way: the pod leaves Service endpoints and waits. The differences:

- The controller picks which pods go and when; SIGTERM / preStop starts the countdown immediately.
- There is no per-version view of what is still draining, and no TTL or count cap across a rollout.
- A Terminating pod blocks `kubectl drain` and node consolidation for the whole grace period without being protected from it.
- Scale-down and rollback cannot reuse the drain logic; every removal is a kill with a long grace period.

### Agones Fleets

[Agones](https://agones.dev) solves the same problem for game servers: an `Allocated` GameServer survives fleet upgrades and scale-down until the game ends. GracefulSet applies that model to generic pods without the game-server SDK, using HTTP drain checks or process exit as the "session finished" signal.

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
  version: "2024.1.0"        # optional; defaults to a hash of the pod template
  maxSurge: 100%             # new pods allowed above replicas during an upgrade (number or %)
  minReadySeconds: 0         # how long a new pod must be Ready before an old one starts draining
  progressDeadlineSeconds: 600
  paused: false
  revisionHistoryLimit: 10   # ControllerRevisions kept for rollback
  deletionPolicy: Drain      # Drain | Delete
  drainPolicy:
    mode: WaitForDrain      # WaitForCompletion (default) | WaitForDrain | TTL | Manual
    ttl: 24h                # counted from when the pod started draining (TTL mode, or cap for any mode)
    maxDrainingPods: 10     # cap; the longest-draining pods are force-deleted above it (unset = no cap)
    removeFromEndpoints: true    # default true: readiness gate takes draining pods out of Services
    protectFromDisruption: true  # default true: Karpenter / Cluster Autoscaler won't evict draining pods
    drainHook:              # optional: POSTed once when a pod starts draining
      path: /drain
      port: 8080
    drainCheck:             # only used with WaitForDrain
      type: HTTP            # HTTP | TCP | Exec
      path: /drain-status
      port: 8080
      scheme: HTTP
      periodSeconds: 30
      jsonField: inflight   # dot path, e.g. stats.sessions
      threshold: 0          # drained when the value is <= threshold
  selector:
    matchLabels:
      app: my-app
  template:
    metadata:
      labels:
        app: my-app
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

`ttl` applies in every mode except `Manual` and is measured from the moment the pod started draining (the `gracefulset.io/draining-since` annotation), not from pod creation.

Pods that exit are always cleaned up. A draining pod that exits has finished its work. An active pod that exits or is evicted is replaced.

### What happens when a pod starts draining

1. It gets the label `gracefulset.io/draining=true` and the annotation `gracefulset.io/draining-since`.
2. With `removeFromEndpoints` (default), its `gracefulset.io/serving` readiness gate is set to `False`, so Services stop sending it new traffic. Existing connections are not closed.
3. With `protectFromDisruption` (default), it gets `karpenter.sh/do-not-disrupt: "true"` and `cluster-autoscaler.kubernetes.io/safe-to-evict: "false"`.
4. If `drainHook` is set, the controller POSTs to it once (retried every 30s until it returns 2xx). Make the hook idempotent.

**Readiness gate trade-off:** with `removeFromEndpoints`, new pods become Ready only after the controller sets their gate, so pod readiness depends on the controller running. Set `removeFromEndpoints: false` if your Service selector already excludes draining pods or you don't front the pods with a Service.

### Deleting a GracefulSet

With `deletionPolicy: Delete`, pods are deleted right away like a Deployment's. With the default `Drain`, the controller adds a finalizer. On `kubectl delete gracefulset`, every pod is drained under the drain policy, and the GracefulSet disappears once its pods are gone. With `Manual` mode this waits until you delete the pods. To skip draining, delete with `--cascade=foreground` (garbage collection then deletes the pods directly), or remove the `gracefulset.io/finalizer` finalizer.

### Upgrades

Changing `version` (or the template, when `version` is empty) starts an upgrade. Old pods **keep serving** until a new pod has been Ready for `minReadySeconds`; only then is an old pod moved to draining. `maxSurge` limits how many new pods may exist above `replicas` (default `100%`: all new pods at once; `1`: one at a time). It is rounded up and is at least 1.

The `Progressing` condition reports `RollingOut`, `RolloutComplete`, `Paused`, or `False/ProgressDeadlineExceeded` when new pods are not all available after `progressDeadlineSeconds`.

`paused: true` stops the controller from creating pods or starting new drains (including scaling). Pods that are already draining are still handled.

### Rollback

Each version's template is stored as a `ControllerRevision` (`kubectl get controllerrevisions -l gracefulset.io/name=my-app`). To roll back:

```bash
kubectl annotate gracefulset my-app gracefulset.io/rollback-to=previous   # or a version, e.g. 2024.0.9
```

The controller restores `spec.version` and `spec.template` from that revision and removes the annotation. If you manage the GracefulSet with Helm or GitOps, roll back there instead, or the next sync will undo it.

### Which pods drain first

On scale-down and during upgrades, pods are picked in this order:
1. lowest `controller.kubernetes.io/pod-deletion-cost` annotation (apps can set this to their session count),
2. pods that are not Ready,
3. fewest in-flight requests, read from the drain endpoint (`WaitForDrain` with HTTP checks only),
4. oldest.

On scale-up, pods of the current version that are still draining are put back into service before new pods are created, unless their drain hook was already called.

### Drain check types

| `type` | Drained when | Notes |
|--------|--------------|-------|
| `HTTP` (default) | `jsonField` in the JSON body is `<= threshold`, or is boolean `true` | `jsonField` may be a dot path (`stats.sessions`) |
| `TCP` | `port` refuses connections | for apps that close their listener when done; timeouts are retried, not treated as drained |
| `Exec` | `exec.command` exits 0 | needs `rbac.podExec=true` in the chart, which lets the controller exec into pods |

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

The scale subresource reports `status.replicas` (active pods) and a selector that excludes draining pods, so the HPA neither counts draining pods as capacity nor averages their metrics in.

## Status

```yaml
status:
  observedGeneration: 4
  activeVersion: "2024.1.0"
  replicas: 3
  updatedReplicas: 3
  readyReplicas: 3
  availableReplicas: 3
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
      message: "2 pods draining"
```

If `spec.selector` does not match the template labels, no pods are created and the `Available` condition reports `InvalidSelector`.

## Metrics

| Metric | Type | Labels |
|--------|------|--------|
| `gracefulset_pods` | gauge | `namespace`, `gracefulset`, `version`, `state` (`active`/`draining`) |
| `gracefulset_drain_duration_seconds` | histogram | `namespace`, `gracefulset`, `reason` (`drained`, `completed`, `ttl`, `max-draining`) |
| `gracefulset_drain_check_failures_total` | counter | `namespace`, `gracefulset` |

The controller also records Kubernetes Events on the GracefulSet (`Draining`, `Drained`, `TTLExpired`, `MaxDrainingExceeded`, `DrainHookFailed`, `PodExited`).

## Installation

```bash
# Add the Helm repo
helm repo add gracefulset https://gracefulset.github.io/k8s-gracefulset
helm repo update

# Install the controller (CRD is included automatically)
helm install gracefulset gracefulset/gracefulset-controller -n gracefulset-system --create-namespace
```

### Configuration

Override defaults via `--set` or a values file:

```bash
helm install gracefulset gracefulset/gracefulset-controller \
  -n gracefulset-system --create-namespace \
  --set replicaCount=2 \
  --set image.tag=0.1.0 \
  --set nodeSelector.dedicated=system \
  --set tolerations[0].key=dedicated \
  --set tolerations[0].value=system \
  --set tolerations[0].effect=NoSchedule
```

Available values:

| Key | Default | Description |
|-----|---------|-------------|
| `replicaCount` | 2 | Controller replicas (with leader election) |
| `image.repository` | `pradyumnakashyap/gracefulset-controller` | Container image |
| `image.tag` | `0.1.0` | Image tag |
| `leaderElection.enabled` | `true` | Enable leader election for HA |
| `maxConcurrentReconciles` | `4` | GracefulSets reconciled in parallel |
| `podDisruptionBudget.enabled` | `true` | PDB for the controller (when `replicaCount` > 1) |
| `metrics.service.enabled` | `false` | Expose metrics through a Service |
| `serviceAccount.create` | `true` | Create the controller ServiceAccount |
| `watchNamespace` | `""` | Only manage GracefulSets in this namespace |
| `rbac.podExec` | `false` | Grant `pods/exec` for Exec drain checks |
| `resources.requests.cpu` | `100m` | CPU request |
| `resources.requests.memory` | `128Mi` | Memory request |
| `nodeSelector` | `{}` | Node selector for controller pods |
| `tolerations` | `[]` | Tolerations for controller pods |

### Upgrading

Helm installs files in `crds/` only on first install and never upgrades them. After upgrading the chart, apply the CRD yourself:

```bash
kubectl apply --server-side -f https://raw.githubusercontent.com/gracefulset/k8s-gracefulset/main/charts/gracefulset-controller/crds/apps.gracefulset.io_gracefulsets.yaml
```

### Uninstall

```bash
helm uninstall gracefulset -n gracefulset-system
# CRDs are not removed by helm uninstall. To remove:
kubectl delete crd gracefulsets.apps.gracefulset.io
```

## Architecture

```
┌──────────────────────────────────────────────┐
│  GracefulSet Controller (Deployment, 2x HA)  │
│                                              │
│  Watches: GracefulSet resources              │
│  Creates: Pods (owned by the GracefulSet)    │
│  Manages: version tracking, scale-down drain,│
│           drain policy lifecycle             │
└──────────────────────────────────────────────┘
```

## Pod Labels

The controller manages these labels on pods it creates:

| Label | Meaning |
|-------|---------|
| `gracefulset.io/name` | Owning GracefulSet name |
| `gracefulset.io/version` | The version the pod was created for |
| `gracefulset.io/draining` | Set to `true` when a pod is marked for draining (upgrade, scale-down or deletion) |

Annotations set on draining pods: `gracefulset.io/draining-since`, `gracefulset.io/drain-hook-called`, and (with `protectFromDisruption`) `karpenter.sh/do-not-disrupt` and `cluster-autoscaler.kubernetes.io/safe-to-evict`.

## Security note

Anyone who can create a GracefulSet in a namespace can make the controller create pods there. Pod Security Admission still applies to those pods, the same as for Deployments.

## Project Structure

```
k8s-gracefulset/
├── api/v1alpha1/                        CRD type definitions + deepcopy
├── internal/controller/                 Reconciliation logic
├── charts/gracefulset-controller/       Helm chart
│   ├── crds/                            CRD manifest (auto-installed by Helm)
│   ├── templates/                       Deployment + RBAC templates
│   ├── Chart.yaml
│   └── values.yaml
├── examples/                            Sample GracefulSet resources
├── .github/workflows/release.yaml       CI: build image + publish chart on tag
├── Dockerfile
├── Makefile
├── main.go
└── go.mod
```

## Development

```bash
# Resolve dependencies
go mod tidy

# Compile
go build ./...

# Regenerate deepcopy code and the CRD after changing api/ (needs controller-gen)
make generate manifests
# Unit tests, vet + helm lint
make test lint
# Install the CRD into your cluster (from the chart)
kubectl apply --server-side -f charts/gracefulset-controller/crds/

# Run the controller locally against your kubeconfig
go run ./main.go

# Build the container image locally
make docker-build IMG=pradyumnakashyap/gracefulset-controller:dev
```

## Releasing

Tag a version to trigger the CI pipeline:

```bash
git tag v0.2.0
git push origin v0.2.0
```

This automatically:
1. Builds multi-arch Docker image → pushes to Docker Hub
2. Packages the Helm chart → publishes to GitHub Pages
3. Creates a GitHub Release with auto-generated notes

## Roadmap

- gRPC drain checks (use an `Exec` check with `grpc_health_probe` meanwhile)
- Zone-aware scale-down ordering
