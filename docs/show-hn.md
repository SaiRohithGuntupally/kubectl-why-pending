# Show HN draft

## Title (pick one — keep it factual, HN dislikes marketing)

- **Show HN: kubectl why-pending – explain why a Kubernetes pod won't schedule**
- Show HN: A kubectl plugin that tells you why a pod is stuck Pending
- Show HN: kubectl why-pending – the "why is my pod Pending" answer, with -o json

## URL

https://github.com/SaiRohithGuntupally/kubectl-why-pending

## First comment (post this right after submitting)

Hi HN. I run Kubernetes on bare metal, and the most annoying recurring moment is
a pod stuck `Pending` with no useful explanation. `kubectl describe` gives you a
`FailedScheduling` event that tells you *that* it failed, rarely *why* in a way
you can act on. In the cloud the autoscaler hides this by adding a node; on bare
metal it's your problem to solve by hand.

So I wrote `kubectl why-pending`. It re-runs the scheduler's filtering logic
locally and tells you, per node, why the pod can't land — and the fix. It covers
the things the events gloss over: resource fragmentation (you have the CPU, just
not on one node), control-plane taints eating a small cluster, GPU/extended-
resource shortfalls, topology-spread skew, anti-affinity running out of hosts,
unbound PVCs, and so on.

Two newer bits I'm happy with, both aimed at GPU clusters: when no node advertises
a GPU resource, it walks the enablement chain (NFD → driver → container-toolkit →
device-plugin → GFD → DCGM → MIG-manager) and names the broken link from pod
status, rather than handing you a generic checklist. And it diagnoses Dynamic
Resource Allocation (DRA, k8s 1.34+) `resourceClaims` — unallocated/missing claims,
missing DeviceClasses, whether a DRA driver is publishing ResourceSlices — which
almost nothing else explains yet.

It also does `-o json`, which turned out to be a 2017 Kubernetes feature request
(#53908, a "WhyPending" annotation) that was closed without ever shipping —
machine-readable scheduling reasons for CI gates and dashboards. The JSON
includes a per-node breakdown of which node was filtered by which predicate.

The design choice I'm happiest with: the analysis engine takes plain data
structs, never a Kubernetes client. So the scheduler logic is unit-tested against
hand-built clusters with no kind/minikube — fast, deterministic tests for
fragmentation, skew math, GPU exhaustion, etc. The CLI just gathers cluster state
and hands it to the engine.

Honest about limits: it's best-effort static analysis. It doesn't model
preemption timing or every topology-spread knob, and when it can't find a hard
blocker it says so and shows the raw scheduler event.

Install: `kubectl krew install why-pending`. MIT-licensed. The most useful
feedback I can get is "it misdiagnosed my cluster" — open an issue with the pod
spec and node shape and I'll add the case.

## Posting tips

- Best time: Tue–Thu, ~8–10am US Eastern (weekday morning traffic).
- Submit the GitHub URL with a "Show HN:" title, then immediately post the comment above.
- Do NOT ask for upvotes anywhere — it's against HN rules and gets flagged.
- Reply to every comment quickly and substantively for the first few hours; that
  engagement is what keeps it on the front page.
- Have a GIF/asciinema in the README (the demo block helps) — HN clicks through.
