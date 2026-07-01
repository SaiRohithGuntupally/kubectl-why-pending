# Scale `gatherNodeViews` for large clusters — paginate + stream the cluster-wide pod list

> Reported via a code-review comment on the r/kubernetes launch thread (2026-07-01).
> Handoff spec for implementation.

## Problem

`gatherNodeViews` (`main.go:220`) fetches cluster state with two unbounded LIST calls:

- `main.go:221` — `client.CoreV1().Nodes().List(...)` (all nodes)
- `main.go:225` — `client.CoreV1().Pods("").List(...)` (**every pod cluster-wide, unpaginated**)

The whole result is held in memory (`allPods.Items`) and iterated to:
1. sum placed-pod requests per node for free-capacity math (`used` map),
2. build the `placed` slice for topology / affinity analysis,
3. feed `diagnose.AnalyzeOperatorChain(allPods.Items)`.

## Impact

At hundreds of nodes / tens of thousands of pods this is a single heavy apiserver
LIST plus a large memory spike (full pod specs materialized at once). API-call
count is O(1) in node count — that part is fine — but memory and per-call cost
scale with total pod count. Currently only validated on small-to-mid on-prem
clusters.

## Proposed fix (priority order)

1. **Read from the watch cache.** Set `ResourceVersion: "0"` on the node and pod
   LIST options so the apiserver serves from its cache instead of a quorum etcd
   read. Cheap, low-risk, big win on busy clusters.
2. **Paginate + stream instead of materializing.** Replace the single
   `Pods("").List` with client-go's `pager.New(...).EachListItem(...)`
   (`k8s.io/client-go/tools/pager`) using a bounded `Limit` (e.g. 500). Fold each
   pod incrementally into the `used` request map and the `placed` slice so the
   full pod set is never held in memory at once.
   - `AnalyzeOperatorChain` currently takes the full `[]Pod` slice — refactor it
     to accept the incremental stream (or to collect only the GPU/operator-relevant
     pods it actually needs), so streaming doesn't force re-materialization.
3. **(Optional, single-pod path) Scope the pod list.** When a specific pod is
   queried, first compute the candidate node set (nodes passing the pod's
   nodeSelector / affinity / taints), then list pods with a `spec.nodeName=<node>`
   field selector only for those nodes rather than the whole cluster. More complex;
   land after 1–2.

## Acceptance criteria

- No single in-memory slice of all cluster pods; memory bounded by page size, not
  pod count.
- Node/pod LISTs use `ResourceVersion: "0"`.
- Existing diagnoses (fragmentation, per-node free capacity, topology spread,
  anti-affinity, operator-chain, DRA) produce identical results — verify against
  current `pkg/diagnose` table tests and `main_test.go`.
- Add a test with a large synthetic cluster (e.g. 500 nodes / 20k placed pods)
  asserting correctness and that paging is exercised.

## Out of scope

- Informer / watch-based caching or a long-running daemon (that's the roadmap
  "Pro tier" fleet daemon, tracked separately).
