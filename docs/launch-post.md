# Why is my pod stuck Pending on bare metal? I built a kubectl plugin that just tells you.

> **`kubectl krew install why-pending`** — now in the official krew-index.

If you run Kubernetes on-prem, you know this moment: you `kubectl apply`, you
`kubectl get pods`, and there it sits — `Pending`. No crash, no error, no log.
Just a pod the scheduler quietly refuses to place, and a `kubectl describe` wall
of events that says *FailedScheduling* without telling you what to actually do.

In the cloud you barely notice this, because the cluster-autoscaler sees the
unschedulable pod and conjures a new node. **On bare metal there is no fairy
godmother.** A `Pending` pod is a puzzle you have to solve by hand — and the most
common real cause isn't "out of capacity," it's something subtler that the events
don't spell out.

So I built [`kubectl-why-pending`](https://github.com/SaiRohithGuntupally/kubectl-why-pending):
a plugin that re-runs the scheduler's filtering logic locally and explains, in
plain English, why your pod won't schedule — and what to change.

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

A few of the diagnoses, and why they're the ones cloud users rarely see:

**Resource fragmentation.** You have 3 free CPUs across the cluster, your pod
wants 2, and yet it won't schedule — because those 3 CPUs are 1-per-node and a
pod must fit on a *single* node. In the cloud the autoscaler adds a right-sized
node and you never learn this happened. On bare metal it's a daily reality, and
"insufficient cpu" in the events badly undersells it. The tool computes the total
free vs. the largest single node's free and names it as fragmentation — which
points you at *rebalancing*, not *buying hardware*.

**Pod larger than any node.** Subtly different: your pod requests 8 CPU but no
node has more than 4, even empty. Defragmentation can't help here — and the tool
says so, instead of sending you on a rebalancing goose chase.

**The control-plane taint.** Small on-prem clusters often have one or two
control-plane nodes carrying `node-role.kubernetes.io/control-plane:NoSchedule`.
On a 3-node cluster where two are control-plane, your workload has exactly one
place to go — and when that's full, the events just say "insufficient cpu" while
the real story is "you tainted away most of your cluster."

**Anti-affinity that runs out of hosts.** The classic "one replica per node"
`podAntiAffinity`: lovely until you want more replicas than you have hosts. The
tool detects when every eligible host already holds a matching pod and tells you
plainly that you've run out of domains.

**Topology spread skew.** `DoNotSchedule` topology spread is great for balance and
infuriating when it blocks. The plugin computes the real skew across *all* domains
(including the under-filled zone with no schedulable node — the one the spread is
trying to balance into) and tells you which domain needs a node.

## How it works

The design goal was: **be correct enough to trust, and testable without a live
cluster.** So the CLI does the Kubernetes-y part — gather nodes, pods-per-node
requests, the pod's scheduling events, its PVCs — and hands a plain data struct to
a pure analysis engine. The engine re-implements the scheduler's filter
predicates (taints/tolerations, nodeSelector, node affinity, resource fit,
topology spread, inter-pod affinity) and ranks the findings by severity.

Because the engine takes no Kubernetes client, the whole thing is unit-tested
against hand-built clusters — fragmentation, control-plane taints, skew math,
anti-affinity exhaustion — plus an end-to-end test that drives the full pipeline
against a fake API. No kind cluster required in CI.

It's deliberately honest about its limits: when static analysis finds no hard
blocker, it says so and points you at the dynamic causes it doesn't model
(priority/preemption, extended resources like GPUs) plus the raw scheduler event.

## Try it

```sh
kubectl krew install why-pending     # it's in the official krew-index
kubectl why-pending                  # every Pending pod in the namespace
kubectl why-pending my-pod -n data   # one pod
kubectl why-pending -A               # all namespaces
```

It's MIT-licensed and open source. If it misdiagnoses something on your cluster,
that's the most useful thing you can tell me — open an issue with the pod spec and
node shape and I'll add the case. The on-prem scheduling failures I haven't seen
yet are exactly what I want to teach it next.

→ **https://github.com/SaiRohithGuntupally/kubectl-why-pending**

---

### Shorter version for r/kubernetes

> **I built a kubectl plugin that explains why pods are stuck Pending — with the
> on-prem causes the events gloss over**
>
> Tired of `kubectl describe`-ing Pending pods and getting "insufficient cpu"
> when the real problem is resource fragmentation (capacity exists, just not on
> one node), a control-plane taint eating half your small cluster, anti-affinity
> running out of hosts, or topology-spread skew?
>
> `kubectl why-pending` re-runs the scheduler's filtering locally and tells you
> the cause in plain English, with the fix. Especially aimed at bare-metal/on-prem
> where there's no autoscaler to paper over it. Pure, unit-tested engine; MIT.
>
> Install: `kubectl krew install why-pending`. Feedback and "it got my cluster
> wrong" reports very welcome.
> https://github.com/SaiRohithGuntupally/kubectl-why-pending
