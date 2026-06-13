# kubectl-why-pending

[![ci](https://github.com/SaiRohithGuntupally/kubectl-why-pending/actions/workflows/ci.yml/badge.svg)](https://github.com/SaiRohithGuntupally/kubectl-why-pending/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/SaiRohithGuntupally/kubectl-why-pending)](https://github.com/SaiRohithGuntupally/kubectl-why-pending/releases)
[![license](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

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
- **Pod topology spread** (`DoNotSchedule`) — computes the real skew across all
  domains and tells you which under-filled zone/host needs a node.
- **Inter-pod affinity / anti-affinity** (required) — including the classic
  "one replica per host" that runs out of hosts.
- **Cordoned / NotReady nodes** removed from scheduling.
- **Unbound PersistentVolumeClaims** (with the WaitForFirstConsumer caveat).
- Falls back to the raw scheduler event + dynamic-cause hints (priority/preemption,
  extended resources) when no static blocker is found.

## Install

### From source

```sh
make build
make install   # copies the binary onto your PATH as kubectl-why_pending
kubectl why-pending --help
```

### Via Krew (once published)

```sh
kubectl krew install why-pending
```

## Usage

```sh
kubectl why-pending                     # every Pending pod in the namespace
kubectl why-pending my-pod              # one pod
kubectl why-pending -n data my-pod      # in a namespace
kubectl why-pending -A                  # all namespaces
kubectl why-pending --no-color          # plain output for logs/CI
```

## How it works

The CLI gathers cluster state once (nodes, pods-per-node requests, the pod's
scheduling events, its PVCs) and hands it to a pure analysis engine in
[`pkg/diagnose`](pkg/diagnose). Keeping the engine free of any Kubernetes client
means the scheduler logic is unit-tested without a live cluster — see
`pkg/diagnose/analyze_test.go` and the end-to-end `main_test.go`, which drives the
whole pipeline against a fake API.

## Roadmap

- Extended/custom resources (GPUs, hugepages).
- `--watch` mode and a JSON output format.
- Topology spread refinements (minDomains, nodeAffinityPolicy, matchLabelKeys).
- Pro tier (planned): a fleet daemon that watches every cluster and alerts on
  Pending pods before anyone files a ticket.

## License

MIT — see [LICENSE](LICENSE).
