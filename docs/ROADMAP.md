# kubectl-why-pending roadmap

Living plan. Last updated 2026-11-20. Inputs: the September research pass in
`docs/research/` (ROADMAP-2026-09.md is the original snapshot; "risk N" refers
to `audit-2026-09.md`), an external code review received 2026-11-20
("review #N" below), and a live judge run of v0.6.0 on a 6-node RKE2 1.35
cluster (3 etcd-tainted control-plane nodes, 3 workers; 21 scenarios: 12
right, 3 partial, 6 wrong; "live #N" below). A retest of v0.7.1 on the same
cluster confirmed every reproducible live bug fixed and found 2 minor new ones
("retest #N").

Guiding rule: **a confident wrong answer is worse than no answer.** Correctness
work comes before features, and anything not modelled is said out loud.

## Status at a glance

| Release | Theme | State |
|---|---|---|
| v0.6.0 | DRA device taints (k8s 1.37) | Released 2026-09-09 |
| v0.7.0 | Stop the confident wrong answers | Released 2026-11-20 (krew-index #6712) |
| v0.7.1 | Live-judge fixes | Released 2026-11-20 (krew-index #6713), retest passed |
| v0.7.2 | Retest fixes + kubectl-standard flags | Released 2026-11-20 (krew-index #6714) |
| v0.7.3 | Review-3 fixes, then the scheduler cross-check | Released 2026-11-22, kind e2e 20/20 |
| v0.8.0 | Usability: readable output, real fix commands | Planned |
| v0.9.0 | Explain more than one stuck pod; deeper GPU/DRA | Planned |
| v1.0.0 | Stable JSON contract + agent surface | Planned |

## v0.7.0: stop the confident wrong answers

Done (2026-11-20, 21 new regression tests):

- [x] Triage before predicates: scheduling gates (names gate + managing
      controller), Pending pods already bound to a node (kubelet-side, shows the
      waiting reason, no longer "No Pending pods found"), `nominatedNodeName`
      (preemption in progress, blockers downgraded). Risks 1, 2, 20.
- [x] WaitForFirstConsumer PVCs are Info, not Blocker; generic ephemeral volume
      PVCs are checked. Risk 12 (part).
- [x] DRA claims listed per namespace; an unreadable API is "unknown", never
      "not found". Risk 14.
- [x] Scheduler event: timestamp fallback (series, eventTime, lastTimestamp,
      firstTimestamp) and involvedObject.uid filter. Risk 16.
- [x] Per-node verdicts updated after every stage (extended resources, spread,
      affinity, DRA). Risk 11.
- [x] Inter-pod affinity: self-match exception, anchors on any node count, empty
      `namespaceSelector` = all namespaces, symmetric anti-affinity. Risks 3-6.
- [x] Topology spread census: `nodeAffinityPolicy: Honor` default, opt-in
      `nodeTaintsPolicy: Honor`, terminating pods skipped. Risk 7 (part).
- [x] Pods tolerating `not-ready` / `unschedulable` taints can use those nodes.
      Risk 10.
- [x] Fragmentation when the largest free CPU and largest free memory are on
      different nodes was reported as "insufficient capacity". Review #2.
- [x] Requests counted like the scheduler: sidecars summed, ordinary init
      containers maxed with sidecars started before them, RuntimeClass
      `overhead` added; ephemeral-storage and max-pods checked per node.
      Risk 9, review #3.
- [x] Failed PVC / event reads surface as a Warning instead of silence.
      Review #5.
- [x] Module path matches the repo (`github.com/SaiRohithGuntupally/...`), so
      `go install ...@latest` works. `--version` stamped via ldflags in
      `make build`, `make release` and the release workflow.
- [x] README: no "re-runs the scheduler" overclaim; "What it doesn't model yet"
      and "Permissions" sections.
- [x] Real-cluster e2e kit (`e2e/`, kind 1 control plane + 3 workers, 11
      scenarios, `expected.tsv`).

v0.7.0 already fixes live #14 (gates), #15 (sidecar), #16 (ephemeral-storage),
#19 (overhead), #21 (cross-dimension fragmentation), the always-empty
scheduler event and the RBAC "not found" false blocker.

Release gate (was planned; v0.7.0 shipped at the maintainer's call before the
mini e2e run, which is now a post-release check):

1. `./e2e/setup-mac.sh` on the Mac mini: 11/11 PASS, scheduler event renders,
   `-A` shows the image-pull pod. Anything FAIL becomes a fix before tagging.
2. Maintainer walkthrough of `pkg/diagnose/triage.go` and `podAffinity` in
   `topology.go` (the new logic users will ask about).
3. Commit, tag `v0.7.0`. Do not touch `.krew.yaml` description; the krew bot
   only auto-merges version/uri/sha256 changes.

## v0.7.1: live-judge fixes

Done (2026-11-20), everything v0.7.0 still got wrong in the live run:

- [x] Placement first: nodeSelector / nodeAffinity are checked before cordon,
      NotReady and taints, so a pod pinned to a cordoned or tainted node gets
      that node's problem as the Blocker (selector becomes Info) and the
      tainted node is named with its taint. Live #4, #20.
- [x] Topology spread blocked only because the least-filled domains are
      tainted nodes (control plane, `nodeTaintsPolicy: Ignore` default): says
      so and recommends `nodeTaintsPolicy: Honor` instead of "add a node".
      Live #11.
- [x] DRA hints fire on clusters with zero DeviceClasses / ResourceSlices (an
      empty list decoded as nil and was treated as "couldn't read"). Live #17.
- [x] The "should fit" / "couldn't determine a cause" verdict line appears
      whenever there is no Blocker, not only when there are no causes at all
      (a taint warning used to hide it). Review 2.2.
- [x] A Running pod asked for by name prints only "is Running, not Pending".
- [x] e2e kit: 6 new scenarios mirroring the live failures (sidecar, ephemeral,
      overhead, cross-dimension fragmentation with fillers, DRA with no driver,
      pinned to a tainted node); s07 now asserts the nodeTaintsPolicy fix.

Still open from the live report: exit code for "Pending, cause unknown"
(v0.8.0 item 6), max-pods / NotReady / preemption not exercised live.

## v0.7.2: retest fixes

Done (2026-11-20):

- [x] Unreadable StorageClass (no `storage.k8s.io` RBAC) made a WFFC PVC an
      "Unbound" Blocker. Now a Warning saying the binding mode is unknown; a
      StorageClass that doesn't exist is still a Blocker. Retest #1.
- [x] Replicas sharing one required anti-affinity rule produced two blockers
      (own rule + "existing pods' anti-affinity"). The symmetric check skips
      rules identical to the pod's own. Retest #2.
- [x] Control-plane / etcd / master / CriticalAddonsOnly taints get "keep the
      taint; use a worker or tolerate", never `kubectl taint nodes ... -`.
- [x] Missing DeviceClass named in the cause title.
- [x] Fallback text no longer lists maxPods as unmodelled (it is since v0.7.0).
- [x] **kubectl-standard CLI** (pulled forward from v0.8.0; flagged by every
      review): pflag + `genericclioptions.ConfigFlags`, so `--flag=value`,
      `-n=ns`, `-ojson`, `--as` / `--as-group`, `--token`, `--server`,
      `--request-timeout`, `--insecure-skip-tls-verify`, `--cluster` / `--user`
      all work. The hard-coded 30s deadline is now `--timeout` (default 60s).
      Release binary +5% (27.9 to 29.3 MB, 9.8 MB gzipped).

Retest timing: `-A` across the 6-node cluster in 3.6s.

## v0.7.3: review 3 fixes + finish scheduler fidelity

Review 3 (full live report on v0.7.1, RKE2 1.35, 2026-11-20) listed 13 open
bugs. Done (unreleased):

- [x] **B1** "Larger than any node" compared per-dimension maxima from
      different nodes (16 CPU / 8Gi + 4 CPU / 64Gi vs a 12 CPU / 32Gi pod said
      "defragment"). Now counts nodes where both fit when empty.
- [x] **B2** Any `nominatedNodeName` downgraded every Blocker. Now only when
      the nominated node exists and fails at most on capacity (preemption can
      free that); otherwise the nomination is called stale.
- [x] **B3** `spec.nodeName` of a node that doesn't exist: Blocker naming it
      (nodeName skips the scheduler; PodGC deletes the pod).
- [x] **B4** `-A some-pod` was an API 404; now a usage error (exit 2).
- [x] **B6** Scheduler silence in triage: custom `schedulerName` with no events
      after 1 min is a Blocker; the default scheduler gets a Warning pointing
      at the kube-scheduler pods and lease. The clock starts at the later of
      creation and the PodScheduled transition (so lifted gates don't count).
- [x] **B9** Several default StorageClasses: the newest wins, as in admission.
- [x] **B11** In-place resize (GA 1.35): a container's request is the larger
      of spec and `allocatedResources`.
- [x] **G1** Pod-level `spec.resources` replaces the container sum for
      cpu/memory, for the pending pod and for every running pod's usage.
- [x] B5, B8, B10 were already fixed in v0.7.2 (review 3 covered v0.7.1).
- [x] e2e: s14 pod-level resources, s16 missing scheduler, s17 hostPort
      (known gap); wait raised to 75s. The ghost-node repro isn't in e2e
      because PodGC deletes it within ~40s; it's a unit test.

- [x] **Scheduler cross-check** (item 1 below, the review's top feature):
      the FailedScheduling message is parsed into counted reasons
      (`schedulerReasons` in JSON). With no blocker of its own, the tool now
      reports each scheduler reason as a Blocker, labelled "not modelled here"
      (hostPorts, PV node affinity / zones, attach limits, RWOP, unknown
      plugins) or "this tool's model disagrees" (modelled filters). With
      blockers, unmodelled scheduler reasons add one Warning. "We found X, the
      scheduler didn't say X" is deliberately not flagged: the scheduler stops
      at the first failing filter per node in a different order. Wording
      table tested for 1.26-1.37 messages. Review 3 R2 (hostPort) and R3 (PV
      on a missing node) go from "should fit", exit 0, to a named Blocker.

Not done: **B7** quota / admission (needs workload targets, v0.9.0).

Remaining correctness items, ordered by how often they cause a wrong answer.
Review 3 asks to pull G5 (`matchLabelKeys`, false Blockers during rollouts)
and hostPorts (G2) up: both now lead the list after the cross-check.

1. ~~Cross-check against the scheduler's own verdict~~ done (see above).
   Original plan: parse the FailedScheduling message (`0/N nodes are available: 3
   Insufficient cpu, 2 node(s) had untolerated taint ...`) into per-reason
   counts and compare with this tool's per-node verdicts. Agreement = explain
   each count in plain English. Disagreement = say so ("the scheduler reports 2
   nodes with Insufficient cpu, this model found 0; trust the scheduler"), and
   name scheduler reasons this tool doesn't model. This keeps the engine honest
   as Kubernetes adds filters, without importing `k8s.io/kubernetes`. Message
   wording changed in 1.33, 1.35 and 1.37: keep a per-version pattern table
   with tests.
2. **Volume topology for the on-prem niche** (risk 12 rest, M): a Bound PV's
   `nodeAffinity` checked against candidate nodes (local-path, OpenEBS,
   TopoLVM pinning, the #2 user complaint), StorageClass `allowedTopologies`,
   PV pinned to a node that no longer exists.
3. **Swap hand-rolled matching for `k8s.io/component-helpers`** (risk 8, M):
   taints, node affinity, label selectors become scheduler-identical by
   construction; delete ~200 lines; add `predicates_test.go` for what remains.
   Also dedupe `tolerationTolerates` / `deviceTolerationTolerates`.
4. **Record every failing predicate per node** (risk 21, S): counts stop being
   check-order artifacts; fix hints stop pointing at a node that fails
   something else too.
5. **Topology spread rest** (risk 7 rest, S, review 3 G4/G5: do first after
   item 1): `matchLabelKeys` (old ReplicaSet pods counted = false Blocker
   mid-rollout), `minDomains`.
   **hostPorts** (review 3 G2, S): rke2-traefik holds 443 on every worker;
   today that's "should fit".
   Affinity `namespaceSelector` with labels (needs a namespaces list; today such
   terms are skipped rather than guessed).
6. **Units** (risk 23, S): hugepages rendered as bytes (`2Mi`), not
   "2097152 of hugepages-2Mi"; no "0B".
7. **Edge nodes** (risk 22, S): OS/arch mismatch note, virtual-kubelet nodes
   excluded from fragmentation math, Ready node with empty allocatable.
8. **Watch-cache caveat** (risk 17, S): document or drop `ResourceVersion: "0"`
   paging on large clusters; verify against the kind cluster with 20k pods.
9. **Batch per-pod API calls** (review, S): list FailedScheduling events once
   per namespace (`reason=FailedScheduling`) and PVCs once per namespace, then
   look up by pod UID / claim name. Today `-A` does one events LIST plus one
   GET per PVC per pending pod, sequentially, inside a 30s deadline: hundreds
   of Pending pods during an incident hit the timeout.
10. ~~Pod-level `spec.resources`~~ done (review 3 G1).
11. **CI hardening** (review 3 F10): run the kind e2e kit on PRs,
    `golangci-lint`, `govulncheck`, Actions pinned by SHA.
12. **"Largest request that fits now"** hint on capacity blockers (review 3
    F7), e.g. "max 7.3 CPU / 14.6Gi on node X".

Exit criterion: each item lands with an e2e scenario in `e2e/scenarios/`
(volume pinning and sidecars need real kind clusters to be believable).

## v0.8.0: usability

1. ~~kubectl-standard CLI~~ done in v0.7.2. Left: print partial results when
   `--timeout` hits instead of exit 3.
2. ~~`--version`~~ done in v0.7.0. Add commit and build date to it, and print
   it in the misdiagnosis issue template.
3. **Per-node table in text output** (`--nodes`, automatic at 20 nodes or fewer),
   laid out like kube-capacity `--available`. The scheduler's own message hides
   node names; this is the most-quoted complaint.
4. **Group by cause for `-A`**: "37 pods share: Resource fragmentation", one
   line each, `--verbose` for full blocks.
5. **Fix commands with real names**: `kubectl taint nodes worker-3 <key>-`,
   `kubectl uncordon worker-2`, `kubectl label node ...`. Single-node clusters
   get both fixes (toleration vs untaint) and recognise k3s
   `CriticalAddonsOnly`, Talos, minikube.
6. **Exit code for "Pending, cause unknown"** so `|| alert` never stays silent
   on an unmodelled cause.
7. `--color auto|always|never`, `--ascii`, windows/amd64 build.
8. Golden-file tests for text and JSON output (inject an `io.Writer` into the
   reporter so README samples are asserted).

## v0.9.0: beyond one stuck pod

1. **Not-yet-created pods**: `-f pod.yaml` dry-run against live state, and
   `deployment/x`, `statefulset/x`, `job/x` targets that explain zero replicas
   (ResourceQuota, LimitRange, admission webhook, PodSecurity, PVC templates).
2. **Missing filters**: hostPort / NodePorts conflicts (risk 13), maxPods per
   node, CSI attach limits (`CSINode` allocatable).
3. **GPU heuristics with named fixes**: MIG name mismatch (`nvidia.com/mig-*`
   advertised, `nvidia.com/gpu` requested), allocatable 0 = "device plugin
   hasn't advertised it on this node", hugepages not preallocated.
4. **DRA depth** (risk 15): map ResourceSlices to the nodes the pod can reach,
   sum other claims' allocations (measured "in use", not guessed), `count`,
   `allocationMode: All`, DeviceClass CEL selectors, partitionable counters,
   binding conditions, and the 1.37 DeviceClass `extendedResourceName` mapping.
5. **Priority context**: show the pod's priority next to the lowest priority
   running on each candidate node, so "preemption could free this" is visible
   before the scheduler acts (review #4).
6. **Scheduler silence**: no FailedScheduling events + default `schedulerName`
   = check the kube-scheduler lease ("scheduler hasn't attempted this pod");
   custom `schedulerName` = say so.
7. **1.35-1.37 surface**: gang scheduling PodGroups, NodeDeclaredFeatures,
   `CSIDriver.preventPodSchedulingIfMissing`. Detect the cluster minor and
   print which features this build models.
8. **Autoscalers, translated not reimplemented**: surface Karpenter / Cluster
   Autoscaler pod events verbatim; silent when absent so bare-metal output
   stays clean.
9. `--watch`: re-diagnose on change and print what changed when it scheduled.

## v1.0.0: stable contract and agent surface

1. Versioned JSON schema: `schemaVersion` on results, stable `code` ids on
   causes (CI gates and dashboards stop string-matching titles).
2. `SKILL.md` in the repo for Claude Code / Codex / Gemini CLI / Cursor.
3. `why-pending mcp` stdio subcommand (precedents: kubectl-ai `--mcp-server`,
   k8sgpt `serve --mcp`); HolmesGPT / kagent integration as an enrichment.
4. **Pending-pod benchmark**: a reproducible set of kind scenarios (grown from
   `e2e/scenarios/`) scoring why-pending vs `kubectl describe` vs AI
   assistants on correct root cause. Publish the harness and results.

## Stability engineering (every release)

- CI: golangci-lint/staticcheck, gofmt check, `go mod tidy` diff, govulncheck,
  dependabot, coverage report. Replace deprecated `fake.NewSimpleClientset`.
- Pin every GitHub Action by commit SHA (the release job has `contents: write`
  and runs third-party actions: softprops/action-gh-release,
  krew-release-bot). Review.
- Move the e2e kit into CI: kind matrix N, N-1, N-2 with pinned
  `kindest/node` digests, run on every PR, and assert that each diagnosis
  agrees with the scheduler's own FailedScheduling event for that scenario
  (catches drift from the real scheduler that unit tests can't). Review.
- Release with goreleaser instead of the Makefile shell loop: SBOM and cosign
  signatures for little extra work. Keep the asset names identical so the
  krew manifest template keeps working.
- Compatibility matrix in the README; warn when cluster minor differs from the
  compiled minor.
- Refactor `Analyze` into a `Filter` interface so a new predicate touches one
  file and updates verdicts automatically; update CONTRIBUTING to match.
- Never import `k8s.io/kubernetes` scheduler plugins (unsupported as a
  library, breaks every minor).

## Distribution and feedback

- Repo hygiene (review): move the launch drafts (`docs/show-hn.md`,
  `devto-post.md`, `launch-post.md`, `launch-playbook.md`) out of the public
  tree; the README "Pro tier" line is already gone.
- The launch drafts were never posted; the only public feedback so far is the krew maintainer's.
  Post after v0.7.0 is validated on a real cluster, with the coverage list as
  the honest pitch, so first users don't hit the bugs v0.7.0 just fixed.
- GitHub topics `kubectl-plugin`, `krew-plugin`; PR to awesome-kubectl-plugins;
  Homebrew tap with the completion shim; k9s plugin snippet in the README.
- KubeWeekly submission; a "how the scheduler decides, illustrated" blog post
  rather than a launch post.
- Feedback loop: every misdiagnosis issue becomes an e2e scenario before the
  fix merges.
