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

`kubectl-why-pending` re-checks the scheduler's main filters locally against live
cluster state and explains the result like a teammate would. It models the
common filters (see [What it doesn't model yet](#what-it-doesnt-model-yet)); the
scheduler's own last message is always shown alongside for comparison.

## What it detects

First, the Pending pods that aren't a scheduling problem at all, so it never
invents a capacity blocker for them:

- **Already scheduled, stuck on the node** (image pull, volume mount, sandbox):
  says so and shows the container's waiting reason.
- **Held by a scheduling gate** (Kueue, Volcano, custom controllers): names the
  gate and the controller that set it; the scheduler hasn't looked yet.
- **Preemption in progress** (`nominatedNodeName` set): findings are shown as
  warnings, because the scheduler is already acting.

Then the real scheduling causes:

- **Resource fragmentation** — enough total CPU/memory, but not on any single node.
- **Pod larger than any node** — defrag can't help; the request or the hardware is the problem.
- **Insufficient free capacity** — not enough room now, even in aggregate.
  Requests are counted like the scheduler does: sidecar containers added to the
  total, RuntimeClass `overhead` included.
- **Ephemeral-storage and max-pods** — nodes whose root disk or pod-count limit
  (kubelet `maxPods`, 110 by default) is used up, even with CPU and memory free.
- **Untolerated taints** — including the classic control-plane taint on small
  clusters. A pod pinned to a tainted or cordoned node is told about that node
  by name.
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
- **Device taints (k8s 1.37+)** — when devices *are* published but every one
  carries a `NoSchedule`/`NoExecute` device taint the claim doesn't tolerate,
  names the taint and where it came from (the driver's ResourceSlice or a
  `DeviceTaintRule`), kept separate from ordinary node taints. Partial cases
  say how many devices were really candidates.
- **Pod topology spread** (`DoNotSchedule`) — computes the skew per domain with
  the scheduler's default node inclusion (nodes the pod can't select aren't
  domains, terminating pods don't count) and tells you which under-filled
  zone/host needs a node, or that tainted control-plane nodes are the empty
  domains and `nodeTaintsPolicy: Honor` is the fix.
- **Inter-pod affinity / anti-affinity** (required) — including the classic
  "one replica per host" that runs out of hosts, the first-replica self-match
  exception, and existing pods' anti-affinity against this pod.
- **Cordoned / NotReady nodes** removed from scheduling (unless the pod
  tolerates the `unschedulable` / `not-ready` taints, like DaemonSet pods).
- **Missing or unbound PersistentVolumeClaims.** A claim on a
  `WaitForFirstConsumer` StorageClass is reported as expected, not as a blocker.
- **Failed reads are reported**, never silent: a forbidden PVC or event read
  shows up as a warning instead of a wrong conclusion.
- Falls back to the raw scheduler event + dynamic-cause hints (priority/preemption)
  when no static blocker is found.

## What it doesn't model yet

It's a static re-check, not the scheduler itself, so some filters are missing.
When one of these is the cause, the tool says it found no static blocker and
shows the scheduler's own event:

- hostPort conflicts, CSI volume attach limits
- PersistentVolume node affinity / volume zones (local-path, OpenEBS, TopoLVM)
- pod-level `spec.resources`
- topology spread `minDomains` / `matchLabelKeys`, affinity `namespaceSelector`
  with labels
- DRA device selectors, counts and per-node device placement

## Permissions

It reads cluster-wide: `list` on nodes and pods in all namespaces (for
free-capacity math), plus `get`/`list` on events, PVCs and StorageClasses, and
on `resource.k8s.io` objects for DRA pods. Without cluster-wide pod/node access
it exits 3 with the API error.

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
# or: go install github.com/SaiRohithGuntupally/kubectl-why-pending@latest
#     (installs as kubectl-why-pending; rename to kubectl-why_pending for kubectl)
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
kubectl why-pending --version
```

All the standard kubectl connection flags work, in either `--flag value` or
`--flag=value` form: `--context`, `--kubeconfig`, `--as` / `--as-group`
(debug what a user or service account can see), `--token`, `--server`,
`--request-timeout`, `--insecure-skip-tls-verify`. `--timeout` (default 60s)
bounds the whole run; raise it for `-A` on very large clusters.

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

Full plan in [docs/ROADMAP.md](docs/ROADMAP.md). In short:

- **v0.7.x:** finish scheduler fidelity: pod overhead and sidecars, PersistentVolume
  node affinity (local-path / OpenEBS / TopoLVM), scheduler-identical taint and
  affinity matching.
- **v0.8.0:** standard kubectl flags (`--as`, `--request-timeout`), `version`,
  a per-node table in text output, group-by-cause for `-A`, fix commands with
  real node names.
- **v0.9.0:** explain Deployments with zero pods, hostPort and volume-limit
  conflicts, deeper GPU and DRA diagnosis, `--watch`.
- **v1.0.0:** a versioned JSON schema and an MCP / agent-skill interface.

## License

MIT — see [LICENSE](LICENSE).
