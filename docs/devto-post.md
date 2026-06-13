---
title: "kubectl why-pending: stop guessing why your pod is stuck Pending"
published: false
description: "A kubectl plugin that re-runs the scheduler's filtering locally and tells you, in plain English, why a pod won't schedule — built for the bare-metal causes cloud clusters hide."
tags: kubernetes, go, devops, opensource
cover_image: ""
canonical_url: ""
---

> **`kubectl krew install why-pending`** — it's in the official krew-index.

If you run Kubernetes on bare metal, you know this exact moment. You `kubectl
apply`, you check the pod, and there it sits: `Pending`. No crash. No log. Just a
pod the scheduler quietly refuses to place — and a `kubectl describe` wall of
`FailedScheduling` events that tells you *that* it failed, rarely *why* in a way
you can act on.

In the cloud you barely notice this, because the cluster-autoscaler sees the
unschedulable pod and spins up a node. **On bare metal there is no fairy
godmother.** A `Pending` pod is a puzzle you solve by hand — and the most common
real cause usually isn't "out of capacity." It's something the events don't spell
out.

So I built [`kubectl-why-pending`](https://github.com/SaiRohithGuntupally/kubectl-why-pending):
a plugin that re-runs the scheduler's filtering logic locally and explains the
result like a teammate would.

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

## The cases that actually bite on-prem

**Resource fragmentation.** You have 3 free CPUs across the cluster, your pod
wants 2, and it *still* won't schedule — because those CPUs are 1-per-node and a
pod must fit on a *single* node. The autoscaler would hide this in the cloud; on
bare metal it's a daily reality, and "insufficient cpu" badly undersells it.
`why-pending` reports it as fragmentation, which points you at *rebalancing*, not
*buying hardware*.

**The control-plane taint.** Small clusters often taint one or two nodes
`node-role.kubernetes.io/control-plane:NoSchedule`. On a 3-node cluster where two
are control-plane, your workload has exactly one home — and when it's full, the
events say "insufficient cpu" while the real story is "you tainted away most of
your cluster."

**GPUs and extended resources.** A pod asks for `nvidia.com/gpu: 1` and hangs.
Is it because no node has a GPU, the NVIDIA device plugin isn't running, or the
GPU nodes are tainted and your pod lacks the toleration? The plugin tells you
which — including the brutal one: *no schedulable node advertises the resource at
all.*

**Anti-affinity out of hosts.** The classic "one replica per node"
`podAntiAffinity` — lovely until you want more replicas than you have nodes.
`why-pending` detects when every eligible host already holds a matching pod.

**Topology spread skew.** `DoNotSchedule` topology spread, great for balance and
infuriating when it blocks. The plugin computes the real skew across *all*
domains — including the under-filled zone with no schedulable node, the one the
spread is trying to balance into — and names the domain that needs a node.

## How it's built (the part I'm happiest with)

The design goal was **correct enough to trust, and testable without a cluster.**
The CLI does the Kubernetes-y part — gather nodes, pods-per-node requests, the
pod's scheduling events, its PVCs — and hands a plain data struct to a pure
analysis engine. The engine re-implements the scheduler's filter predicates
(taints/tolerations, nodeSelector, node affinity, resource fit including extended
resources, topology spread, inter-pod affinity) and ranks the findings.

Because the engine takes **no Kubernetes client**, the whole thing is unit-tested
against hand-built clusters — fragmentation, control-plane taints, GPU
exhaustion, skew math, anti-affinity — plus an end-to-end test that drives the
full pipeline against a fake API. No kind cluster in CI.

It's deliberately honest about its limits: when it finds no hard blocker, it says
so and points at the dynamic causes it doesn't model (priority/preemption) plus
the raw scheduler event.

## Try it

```sh
kubectl krew install why-pending
kubectl why-pending                  # every Pending pod in the namespace
kubectl why-pending my-pod -n data   # a specific pod
kubectl why-pending -A               # all namespaces
```

MIT-licensed and open source. If it misdiagnoses something on your cluster,
**that's the most useful bug report I can get** — open an issue with the pod spec
and node shape and I'll add the case. The on-prem failures I haven't seen yet are
exactly what I want to teach it next.

→ **https://github.com/SaiRohithGuntupally/kubectl-why-pending**
