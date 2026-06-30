# kubectl-why-pending

[![ci](https://github.com/SaiRohithGuntupally/kubectl-why-pending/actions/workflows/ci.yml/badge.svg)](https://github.com/SaiRohithGuntupally/kubectl-why-pending/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/SaiRohithGuntupally/kubectl-why-pending)](https://github.com/SaiRohithGuntupally/kubectl-why-pending/releases)
[![license](https://img.shields.io/badge/license-MIT-blue)](LICENSE)
[![krew](https://img.shields.io/badge/krew-why--pending-326ce5)](https://krew.sigs.k8s.io/plugins/)

> Stop `kubectl describe`-ing. Ask your cluster *why* a pod is stuck `Pending` — and get the fix.

A `kubectl` plugin that diagnoses unschedulable pods in plain English, with the
**on-prem / bare-metal** causes that cloud clusters hide called out specifically:
resource **fragmentation**, **control-plane taints**, missing nodes, and unbound
local volumes. In the cloud the autoscaler papers over these — on bare metal,
they're on you.

```
$ kubectl why-pending analytics-7d9

Pod default/analytics-7d9  (requests 2 CPU / 512Mi memory)
────────────────────────────────────────────────────────────────
  ✗ Resource fragmentation — capacity exists, but not on any single node
      Pod requests 2 CPU / 512Mi memory.
      Total free across 3 eligible node(s): 3 CPU / 21.0Gi memory — enough in aggregate.
      But the largest single node only has 1 CPU / 7.0Gi memory free, and a pod must fit on ONE node.
      fix: Defragment the cluster: drain/rebalance pods to free a contiguous slot,
           lower this pod's requests if they're padded, or add a node large enough.

  ! 1/4 node(s) have taints the pod doesn't tolerate
      Untolerated taints: node-role.kubernetes.io/control-plane=:NoSchedule.
      fix: Add a matching toleration, or remove the taint if the node should accept workloads.
```

…or a GPU pod that can't find a GPU:

```
$ kubectl why-pending ml-train-0

Pod default/ml-train-0  (requests 2 CPU / 4.0Gi memory)
────────────────────────────────────────────────────────────────
  ✗ No eligible node provides nvidia.com/gpu
      The pod requests 1 of "nvidia.com/gpu", but no schedulable node advertises it.
      Common on-prem causes: the device plugin (e.g. the NVIDIA device plugin for
      nvidia.com/gpu) isn't installed or healthy, there's no node with that hardware,
      or the only nodes that have it were filtered out by a taint/selector/affinity.
      fix: Install/repair the device-plugin DaemonSet, add a node that advertises
           nvidia.com/gpu, or (if those nodes are tainted) add the matching toleration.
```

## Why

Every Kubernetes user eventually stares at a `Pending` pod and a wall of
`kubectl describe` output. The scheduler's own `FailedScheduling` event tells you
*that* it failed, rarely *why* in a way you can act on — especially on bare metal,
where the most common real cause is **fragmentation** (you have the capacity, just
not on one node) and there's no autoscaler to bail you out.

`kubectl-why-pending` re-runs the scheduler's filtering logic locally and explains
the result like a teammate would.

## What it detects

- **Resource fragmentation** — enough total CPU/memory, but not on any single node.
- **Pod larger than any node** — defrag can't help; the request or the hardware is the problem.
- **Insufficient free capacity** — not enough room now, even in aggregate.
- **Untolerated taints** — including the classic control-plane taint on small clusters.
- **nodeSelector / required nodeAffinity** mismatches.
- **Extended resources** (`nvidia.com/gpu`, hugepages, custom device-plugin
  resources) — no node advertises it (device plugin missing / no such hardware),
  or none has enough free.
- **GPU enablement chain** — when a GPU resource isn't advertised, names the
  exact broken link (NFD → driver → container-toolkit → device-plugin → GFD →
  DCGM → MIG-manager) from pod status, instead of a generic checklist.
- **Dynamic Resource Allocation (DRA, k8s 1.34+)** — for pods using
  `resourceClaims`: unallocated or missing claims, missing DeviceClasses, and
  whether any DRA driver is publishing ResourceSlices.
- **Pod topology spread** (`DoNotSchedule`) — computes the real skew across all
  domains and tells you which under-filled zone/host needs a node.
- **Inter-pod affinity / anti-affinity** (required) — including the classic
  "one replica per host" that runs out of hosts.
- **Cordoned / NotReady nodes** removed from scheduling.
- **Unbound PersistentVolumeClaims** (with the WaitForFirstConsumer caveat).
- Falls back to the raw scheduler event + dynamic-cause hints (priority/preemption)
  when no static blocker is found.

## Install

### Via [Krew](https://krew.sigs.k8s.io/) (recommended)

`why-pending` is in the official krew-index:

```sh
kubectl krew update
kubectl krew install why-pending
kubectl why-pending --help
```

### From source

```sh
make build
make install   # copies the binary onto your PATH as kubectl-why_pending
kubectl why-pending --help
```

## Usage

```sh
kubectl why-pending                     # every Pending pod in the namespace
kubectl why-pending my-pod              # one pod
kubectl why-pending -n data my-pod      # in a namespace
kubectl why-pending -A                  # all namespaces
kubectl why-pending --no-color          # plain output for logs/CI
kubectl why-pending -o json             # machine-readable (also: -o yaml)
```

## Machine-readable output

Kubernetes has wanted a machine-readable "why pending" since 2017
([kubernetes/kubernetes#53908](https://github.com/kubernetes/kubernetes/issues/53908))
and never shipped it. `-o json` (or `-o yaml`) is that: the full diagnosis as
structured data, including a **per-node breakdown** of which node was filtered by
which predicate — ready for CI gates, dashboards, or `jq`.

```jsonc
[
  {
    "namespace": "default",
    "pod": "api-server-0",
    "request": { "cpuMilli": 2000, "memBytes": 536870912 },
    "causes": [
      { "severity": "blocker", "title": "Insufficient free capacity right now", "detail": "…", "fix": "…" },
      { "severity": "warning", "title": "1/2 node(s) have taints the pod doesn't tolerate", "detail": "…", "fix": "…" }
    ],
    "nodes": [
      { "name": "cp-1",     "schedulable": false, "reason": "untolerated taint: node-role.kubernetes.io/control-plane" },
      { "name": "worker-1", "schedulable": false, "reason": "insufficient CPU" }
    ]
  }
]
```

**Exit codes** make it scriptable: `0` = no blocking cause, `1` = at least one
Pending pod is blocked, `2` = usage error, `3` = runtime error. So
`kubectl why-pending -A -o json || alert` just works.

## How it works

The CLI gathers cluster state once (nodes, pods-per-node requests, the pod's
scheduling events, its PVCs) and hands it to a pure analysis engine in
[`pkg/diagnose`](pkg/diagnose). Keeping the engine free of any Kubernetes client
means the scheduler logic is unit-tested without a live cluster — see
`pkg/diagnose/analyze_test.go` and the end-to-end `main_test.go`, which drives the
whole pipeline against a fake API.

## Roadmap

- `--watch` mode.
- A Prometheus exporter that emits the diagnosed reason as a metric label
  (kube-state-metrics only exposes *that* a pod is unschedulable, not *why*).
- Topology spread refinements (minDomains, nodeAffinityPolicy, matchLabelKeys).
- Pod priority / preemption awareness.
- Pro tier (planned): a fleet daemon that watches every cluster and alerts on
  Pending pods before anyone files a ticket.

## License

MIT — see [LICENSE](LICENSE).
