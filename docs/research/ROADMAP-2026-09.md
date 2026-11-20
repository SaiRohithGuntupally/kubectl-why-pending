# kubectl-why-pending roadmap (research pass, 2026-09-09)

> Snapshot from 2026-09-09. The living plan is [docs/ROADMAP.md](../ROADMAP.md).

Inputs: a full code audit of v0.6.0 (every Go file against the real kube-scheduler
filter plugins) and an outside-world survey (competing tools, Kubernetes 1.33 to
1.37 scheduler and DRA changes, user complaints 2024 to 2026, krew plugin UX and
distribution norms). Full appendices: `audit-2026-09.md` and `landscape-2026-09.md`.

## Where the plugin stands

Strengths that hold up: pure analysis engine with a fake-clientset e2e test, no
crash paths found, JSON/YAML output with per-node verdicts, exit codes, NO_COLOR
handling, streamed pod list, and a DRA story (device taints, DeviceTaintRules)
that no other CLI has. Test coverage is 84.5% in pkg/diagnose, 55% in main.

Honest position in the market: only three tools re-execute predicates at all
(kube-scheduler-simulator, cluster-capacity and kluster-capacity, and the new
kubectl-can-schedule which is pre-deploy "will this fit"). Nobody does post-hoc,
per-stuck-pod, per-node, DRA-aware explanation in one command. Every AI tool
(K8sGPT, HolmesGPT, kubectl-ai, kagent, the MCP servers) bottoms out at
`kubectl describe` text. Realistic star ceiling for a single-purpose plugin is
the kube-capacity / view-allocations band (hundreds to low thousands), not kubectx.

Two positioning notes for the README: kubectl-can-schedule is pre-deploy, this is
post-hoc; and an unrelated repo cilasbeltrame/kubectl-why-pending shares the name
(krew's `why-pending` resolves to this repo).

## The problem to fix first: confident wrong answers

The audit found 24 correctness risks. The ones that produce a red Blocker and
exit 1 for a pod that is NOT stuck on anything the tool says are the priority,
because the misdiagnosis issue template will collect exactly these reports.

| # | Confident-wrong case | Today | Effort |
|---|---|---|---|
| 1 | Pod has `spec.schedulingGates` (Kueue, Volcano, any queue) | Full capacity analysis, fabricated Blocker | S |
| 2 | Pod has `status.nominatedNodeName` (preemption in flight, PreBind wait, autoscaler hint) | "Insufficient free capacity", tells user to add nodes | S |
| 3 | Pending pod already bound to a node (ContainerCreating, FailedMount, image pull) | Dropped in -A, party emoji "No Pending pods found" | S |
| 4 | Required pod affinity whose only match is the pod itself (first replica of "co-locate with app=web") | Blocker on every fresh rollout | M |
| 5 | WaitForFirstConsumer PVC | Blocker, buries the real cause | M |
| 6 | DRA API unreadable (RBAC, or 1.32/1.33 cluster on v1beta1/v1beta2) | "ResourceClaim not found" Blocker | S |
| 7 | Pod tolerates `not-ready` / `unschedulable` taints (DaemonSet-style infra pods) | NotReady/cordoned nodes excluded unconditionally | S |
| 8 | Per-node verdict frozen before topology/affinity/extended/DRA analysis | JSON says `schedulable: true` for a node that cannot host the pod | S |
| 9 | Scheduler event with zero `lastTimestamp` (events.k8s.io projection) | Event never rendered, README promises it | S |

## Phased plan

### Phase 1: v0.7.0, correctness (target: 2 weeks of evenings)

1. Triage stage before predicates (audit risks 1, 2, 20; landscape rec 1).
   Order: pod not found -> owner `FailedCreate` / quota hint; `spec.nodeName` set ->
   "already scheduled to X, this is a kubelet/runtime problem" plus the container
   waiting reason; `spec.schedulingGates` -> list gate names and the owning manager
   from managedFields, never run capacity math; `status.nominatedNodeName` ->
   "scheduler has a plan: preemption in progress or PreBind wait", downgrade to Info.
2. DRA gather honesty (risk 14). List claims per pending-pod namespace, keep
   "could not read" separate from "absent", add the v1beta1/v1beta2 fallback that
   DeviceTaintRules already have, Info not Blocker when the API is unreadable.
3. Scheduler event fallback chain (risk 16): `series.lastObservedTime`, then
   `eventTime`, then `lastTimestamp`; select by `involvedObject.uid` so a recreated
   StatefulSet pod does not inherit stale events; print the event age and note the
   5-minute unschedulable flush lag. Add a test.
4. Update NodeVerdict after every analysis stage (risk 11) so JSON is true.
5. NodeResourcesFit completeness (risk 9): `pod.Spec.Overhead`, native sidecars
   summed not maxed, ephemeral-storage vs allocatable, pod-level resources (beta,
   on by default since 1.34) when `spec.resources` is set.
6. Node health via taints, not conditions (risk 10): honour tolerations for
   `node.kubernetes.io/not-ready` and `node.kubernetes.io/unschedulable`.
7. Replace hand-rolled taint, node affinity and label selector code with
   `k8s.io/component-helpers` (risk 8, health 5). Deletes ~200 lines, removes the
   empty-term divergence, scheduler-identical by construction. Add predicates_test.go
   for what remains.
8. Volume awareness for the on-prem niche (risk 12): read StorageClass
   `volumeBindingMode` so WFFC is Info; check a Bound PV's `nodeAffinity` against
   candidate nodes (local-path, OpenEBS, TopoLVM pinning is the #2 user complaint);
   include generic ephemeral volumes' generated PVCs.
9. Inter-pod affinity semantics (risks 3 to 6): self-match exception, anchor pods
   on non-eligible nodes count, `namespaceSelector`, symmetric anti-affinity from
   existing pods (needs affinity terms on PlacedPod).
10. Topology spread domain census (risk 7): honour `nodeAffinityPolicy` default
    (exclude nodes failing selector/affinity), skip terminating pods, then add
    `minDomains`, `matchLabelKeys`, `nodeTaintsPolicy`.

Also in this phase: README stops saying "re-runs the scheduler's filtering logic"
and instead ships a coverage matrix (modelled / partial / not modelled) plus the
RBAC requirement (cluster-wide list on nodes and pods) and supported versions.

### Phase 2: v0.8.0, usability

1. Adopt `genericclioptions.ConfigFlags` (audit usability 1 to 3): gets
   `--flag=value`, `--as`, `--request-timeout`, `--server`, `--token`, `-v`,
   and the auth plugins. Replace the fixed 30s deadline with `--timeout`
   defaulting to 60s, and emit partial results on deadline instead of exit 3.
2. `version` subcommand with commit and date via ldflags. Required for the
   misdiagnosis template to be useful.
3. Per-node verdict table in text mode (`--nodes` or always when <= 20 nodes),
   laid out like kube-capacity's `--available` table since users already read it.
   This is the single most-quoted complaint about the scheduler's own message
   (it hides node names).
4. Group-by-cause summary for `-A`: "37 pods share: Resource fragmentation",
   one line each, `--verbose` for the full blocks.
5. Concrete fix commands with real names: `kubectl taint nodes worker-3
   node-role.kubernetes.io/control-plane-`, `kubectl label node worker-3 gpu=true`,
   `kubectl uncordon worker-2`. Single-node control-plane case prints both fixes
   (toleration vs untaint) and recognises k3s `CriticalAddonsOnly`, Talos, minikube.
6. `--exit-code` semantics: keep 0/1/2/3 but add a distinct code for "Pending,
   cause unknown" so `|| alert` never stays silent on an unmodelled cause.
7. `--color auto|always|never`, `--ascii` for log shippers, windows/amd64 build
   (krew supports it and the Makefile is one line away).
8. First-failure attribution (risk 21): record every failing predicate per node,
   not just the first, so counts stop being check-order artifacts.

### Phase 3: v0.9 and beyond, features and differentiation

1. Explain a not-yet-created pod: `-f pod.yaml` dry-run against live cluster
   state, and `deployment/x`, `job/x`, `statefulset/x` targets that explain zero
   replicas (ResourceQuota, LimitRange, admission webhook, PodSecurity, PVC
   template). This is the sibling question every user asks next. Borrow
   volumeClaimTemplates handling from kubectl-can-schedule.
2. hostPort / NodePorts conflicts (ingress controllers, node-exporter, DaemonSet
   maxSurge): needs ports on PlacedPod. User complaint #10.
3. maxPods per node and CSI volume attach limits (`CSINode.spec.drivers[].allocatable`).
   Complaints #8 and #12.
4. GPU heuristics that name the fix: MIG name mismatch (`nvidia.com/mig-*`
   advertised, `nvidia.com/gpu` requested), allocatable 0 => "device plugin has not
   advertised it on this node" with plugin pod state, hugepages not preallocated or
   request not a page multiple, SR-IOV VF snapshot ordering.
5. Finish DRA where nobody else is: map ResourceSlice `nodeName`/`nodeSelector`/
   `allNodes` to candidate nodes, sum other claims' `status.allocation` so "in use"
   is measured not guessed, honour `count`, `allocationMode: All`, `adminAccess`,
   DeviceClass CEL selectors, partitionable device counters (beta 1.36), consumable
   capacity (beta 1.36), binding conditions with timeout (beta 1.36), and the
   1.37 GA extended-resource mapping (`DeviceClass.spec.extendedResourceName`):
   never print "no node provides vendor/gpu" when a DeviceClass maps it.
6. 1.35 to 1.37 scheduler surface: gang scheduling PodGroups
   (`pod.spec.schedulingGroup.podGroupName`, PreEnqueue holds, `minCount` math),
   NodeDeclaredFeatures (GA 1.37), `CSIDriver.preventPodSchedulingIfMissing`
   (beta 1.37), taint comparison operators (alpha 1.35, message loses key/value).
   Detect the cluster minor and print which features the binary models.
7. Scheduler-silence detection: zero FailedScheduling events plus default
   `schedulerName` => check the kube-scheduler lease and say "the scheduler has not
   attempted this pod"; custom `schedulerName` => say so.
8. Translate, do not reimplement, autoscalers: surface Karpenter and Cluster
   Autoscaler pod events verbatim (`NotTriggerScaleUp`, `incompatible with
   nodepool`, `daemonset overhead`) and explain them. Silent when absent so
   bare-metal output stays clean.
9. `--watch`: re-diagnose on pod/node events and print what changed when the pod
   finally scheduled.
10. Agent surface: versioned JSON schema (add `code` ids on causes and
    `schemaVersion` on results), a `SKILL.md` in the repo for Claude Code / Codex /
    Gemini CLI / Cursor, and only then an `mcp` stdio subcommand (precedents:
    kubectl-ai `--mcp-server`, k8sgpt `serve --mcp`). HolmesGPT and Robusta can
    consume the JSON as an enrichment.

### Stability engineering (runs alongside every phase)

- golangci-lint or staticcheck, gofmt check, `go mod tidy` diff, govulncheck,
  dependabot, coverage report in CI. staticcheck already flags the deprecated
  `fake.NewSimpleClientset`.
- Golden-file tests on `-o json` with color off; inject an `io.Writer` into the
  text reporter so README samples are asserted, not hoped.
- kind matrix in CI (N, N-1, N-2 minors, pinned `kindest/node` digests) with a
  handful of real Pending scenarios. envtest cannot produce Pending (no scheduler).
- Compatibility matrix in the README; warn when the cluster minor differs from
  the compiled minor (predicate messages changed in 1.33, 1.35, 1.37).
- Do NOT import `k8s.io/kubernetes` to run the real filter plugins: unsupported as
  a library, ~35 replace directives, breaks every minor (scheduler-plugins ships
  one release line per Kubernetes minor to cope). Stay with component-helpers plus
  the local engine, and state the caveat like kube-scheduler-simulator does.
- Watch-cache pagination caveat (risk 17): `ResourceVersion: "0"` lists ignore
  `limit` on clusters without ListFromCacheSnapshot (beta 1.34); the memory bound
  in docs/issues/large-cluster-scaling.md is only true on new clusters. Either
  drop RV=0 for large clusters or document it.
- Refactor `Analyze` (260 lines) into a `Filter` interface so a new predicate
  touches one file and can update verdicts. CONTRIBUTING currently understates
  the work.

### Distribution

- Homebrew tap formula that also ships the `kubectl_complete-why_pending` shim
  (krew cannot, issue #812).
- GitHub topics `kubectl-plugin`, `krew-plugin`; PR to ishantanu/awesome-kubectl-plugins;
  Artifact Hub is automatic.
- KubeWeekly submission form; a kubernetes.io or CNCF blog post framed as "how the
  scheduler decides, illustrated" rather than a launch.
- k9s plugin snippet in the README (one YAML block, free distribution).
- LinkedIn: each Phase 1 item is a "debugging story" post; the coverage matrix is
  a carousel.

## Suggested order for the next two weeks

Day 1 to 3: triage stage, DRA gather honesty, event fallback, verdict update
(all S, all remove confident-wrong output). Day 4 to 7: component-helpers swap
plus predicates tests, NodeResourcesFit completeness, node health via taints.
Day 8 to 12: volume awareness, affinity semantics, topology census. Day 13 to 14:
README coverage matrix, CI lint, tag v0.7.0 (pure bump, bot auto-merges).
