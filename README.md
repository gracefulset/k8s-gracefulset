# GracefulSet

A Kubernetes CRD and controller for managing stateful applications that require zero-disruption upgrades.

## What it does

GracefulSet creates new pods when the spec changes (version upgrade) but **never kills existing pods**. Old pods continue running until they complete naturally, a TTL expires, or they're manually drained.

This solves the problem of upgrading applications that hold long-lived sessions (like M3 Foundation nodes) where killing a pod means losing user sessions.

## How it works

```
Upgrade v1 → v2:

Before:  [v1-pod-a] [v1-pod-b] [v1-pod-c]    (3 replicas, all v1)
During:  [v1-pod-a] [v1-pod-b] [v1-pod-c]    (draining, still serving)
         [v2-pod-d] [v2-pod-e] [v2-pod-f]    (new, accepting traffic)
After:   [v2-pod-d] [v2-pod-e] [v2-pod-f]    (v1 pods exited naturally)
```

## Key Features

- **Zero-disruption upgrades** — old pods never forcefully terminated
- **Version coexistence** — multiple versions run simultaneously during transition
- **Drain policies** — WaitForCompletion, TTL, or Manual
- **Status tracking** — see active vs draining pods per version
- **Helm-compatible** — use as a drop-in replacement for Jobs/Deployments in Helm charts

## Usage

```yaml
apiVersion: apps.infor.com/v1alpha1
kind: GracefulSet
metadata:
  name: foundation-interactive
  namespace: m3-apps
spec:
  replicas: 3
  version: "2024.1.0"
  drainPolicy:
    mode: WaitForCompletion  # or TTL or Manual
    ttl: 24h                 # only used with TTL mode
    maxDrainingPods: 10      # safety limit on old pods
  selector:
    matchLabels:
      app: foundation-interactive
  template:
    metadata:
      labels:
        app: foundation-interactive
    spec:
      containers:
        - name: foundation
          image: registry/foundation:2024.1.0
          ports:
            - containerPort: 10080
```

## Drain Policies

| Mode | Behavior |
|------|----------|
| `WaitForCompletion` | Old pods run until the container exits (code 0) |
| `TTL` | Old pods are deleted after `ttl` duration |
| `Manual` | Old pods run until explicitly deleted by an operator |

## Status

```yaml
status:
  activeVersion: "2024.1.0"
  readyReplicas: 3
  drainingVersions:
    - version: "2024.0.9"
      pods: 2
      oldestPodAge: "4h32m"
  conditions:
    - type: Available
      status: "True"
    - type: Draining
      status: "True"
      message: "2 pods from version 2024.0.9 still draining"
```

## Installation

```bash
# Install CRD
kubectl apply -f config/crd/gracefulset.yaml

# Deploy controller
helm install gracefulset-controller ./charts/controller -n dma
```

## Architecture

```
┌──────────────────────────────────────────────┐
│  GracefulSet Controller (runs as Deployment) │
│                                              │
│  Watches: GracefulSet resources              │
│  Creates: Pods (owned by the GracefulSet)    │
│  Manages: Version tracking, drain lifecycle  │
└──────────────────────────────────────────────┘
```

## Project Structure

```
gracefulset/
├── api/v1alpha1/            CRD type definitions
├── internal/controller/     Reconciliation logic
├── config/
│   ├── crd/                 Generated CRD manifests
│   ├── rbac/                RBAC permissions
│   └── manager/             Controller deployment
├── charts/controller/       Helm chart for the operator
├── Dockerfile
├── Makefile
└── main.go
```

## Development

```bash
# Generate CRD manifests from Go types
make manifests

# Run locally against a cluster
make run

# Build and push container image
make docker-build docker-push IMG=<registry>/gracefulset-controller:<tag>

# Deploy to cluster
make deploy IMG=<registry>/gracefulset-controller:<tag>
```
