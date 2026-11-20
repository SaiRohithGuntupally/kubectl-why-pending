# kubectl-why-pending roadmap

Living plan. Last updated 2026-11-20. Inputs: the September research pass in
`docs/research/` (ROADMAP-2026-09.md is the original snapshot; "risk N" refers
to `audit-2026-09.md`) and an external code review received 2026-11-20
("review #N" below).

Guiding rule: **a confident wrong answer is worse than no answer.** Correctness
work comes before features, and anything not modelled is said out loud.

## Status at a glance

| Release | Theme | State |
|---|---|---|
| v0.6.0 | DRA device taints (k8s 1.37) | Released 2026-09-09 |
| v0.7.0 | Stop the confident wrong answers | Code done, uncommitted. Release gate below |
| v0.7.1 | Cross-check with the scheduler, finish fidelity | Next |
| v0.8.0 | Usability: kubectl-standard CLI, readable output | Planned |
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

Release gate (all required):

1. `./e2e/setup-mac.sh` on the Mac mini: 11/11 PASS, scheduler event renders,
   `-A` shows the image-pull pod. Anything FAIL becomes a fix before tagging.
2. Maintainer walkthrough of `pkg/diagnose/triage.go` and `podAffinity` in
   `topology.go` (the new logic users will ask about).
3. Commit, tag `v0.7.0`. Do not touch `.krew.yaml` description; the krew bot
   only auto-merges version/uri/sha256 changes.

## v0.7.1: finish scheduler fidelity

Remaining correctness items, ordered by how often they cause a wrong answer.

1. **Cross-check against the scheduler's own verdict** (review, M, highest
   value): parse the FailedScheduling message (`0/N nodes are available: 3
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
5. **Topology spread rest** (risk 7 rest, S): `minDomains`, `matchLabelKeys`.
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
10. **Pod-level `spec.resources`** (beta, on by default since 1.34) in request
    math.

Exit criterion: each item lands with an e2e scenario in `e2e/scenarios/`
(volume pinning and sidecars need real kind clusters to be believable).

## v0.8.0: usability

1. **kubectl-standard CLI**: adopt `genericclioptions.ConfigFlags` (gets
   `--flag=value`, `--as`, `--server`, `--token`, `--request-timeout`, `-v`,
   auth plugins). Replace the fixed 30s deadline with `--timeout` (default 60s)
   and print partial results on timeout instead of exit 3.
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
