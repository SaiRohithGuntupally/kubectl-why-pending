# Launch playbook — how & what to post, per channel

Ground rules (apply everywhere):
- **Smoke-test on a real cluster first.** Don't drive traffic to a tool that
  might crash on someone's setup.
- **Always disclose you're the author.** "I built this" — never pretend to be a
  neutral discoverer.
- **Stagger over days, don't blast all at once.** It reads as spam, and you want
  to be present to reply on each one.
- **Reply to every comment, fast**, for the first few hours. Engagement is what
  carries a post.
- **Never ask for upvotes** anywhere. It backfires.
- **Lead with the problem, not the product.** "Pending pods are confusing" first,
  "I made a tool" second.

Suggested order: real-cluster test → r/kubernetes + dev.to (day 1, Tue–Thu AM) →
CNCF Slack + X + LinkedIn (day 1–2) → r/devops (day 2–3) → warm up HN for ~2 weeks
→ Show HN.

---

## 1. r/kubernetes (your best first channel)

**How:** Read the subreddit rules in the sidebar first (some flair/self-promo
rules apply). New post → **Text post**. Add flair if the sub requires it
("Tool"/"Project" if available). Post Tue–Thu morning US time.

**Title:**
> I built a kubectl plugin that tells you *why* a pod is stuck Pending (on-prem causes + JSON output)

**Body:**
```
If you run Kubernetes — especially on-prem / bare metal — you know the moment: a
pod sits in Pending, `kubectl describe` shows a FailedScheduling event, and it
tells you *that* it failed but not *why* in a way you can act on. In the cloud the
autoscaler papers over it; on bare metal it's on you.

So I built kubectl-why-pending. It re-runs the scheduler's filtering logic
locally and explains, per node, why the pod can't land — and the fix. It calls
out the stuff the events gloss over:

- Resource fragmentation — you have the CPU, just not on one node
- Control-plane taints eating a small cluster
- GPU / extended resources — no node provides it (device plugin missing?), or none free
- GPU enablement chain — names the broken link (NFD → driver → device-plugin → GFD → DCGM → MIG-manager) from pod status
- DRA (Dynamic Resource Allocation, k8s 1.34+) — unallocated/missing claims, missing DeviceClasses, no driver publishing ResourceSlices
- Topology-spread skew, anti-affinity running out of hosts
- Unbound PVCs, cordoned/NotReady nodes, nodeSelector/affinity mismatches

It also does `-o json` with a per-node breakdown — which turned out to be a 2017
Kubernetes feature request (#53908, "WhyPending") that was closed without ever
shipping. Handy for CI gates / dashboards.

Install (it's in the official krew-index):
    kubectl krew install why-pending

MIT-licensed, and I'm the author. The most useful feedback I can get is "it
misdiagnosed my cluster" — drop the pod spec + node shape in an issue and I'll
add the case.

https://github.com/SaiRohithGuntupally/kubectl-why-pending
```

---

## 2. dev.to

**How:** Sign in (GitHub login) → **Create Post** → paste the contents of
`docs/devto-post.md`. In the front matter, set `published: true`, keep the 4
tags (`kubernetes, go, devops, opensource`), and optionally add a `cover_image`
(a screenshot of the output or a simple banner — posts with covers get more
clicks). Optionally set `canonical_url` to the repo. Publish.

(The article is already written in `docs/devto-post.md`.)

---

## 3. CNCF Slack — #kubernetes-users

**How:** Get an invite at https://slack.cncf.io, join, open **#kubernetes-users**
(one relevant channel — don't cross-post to many). Keep it short; Slack hates
walls of text.

**Message:**
```
👋 Sharing a small OSS tool I built: `kubectl why-pending` — it explains *why* a
pod is stuck Pending (per node, with the fix), including on-prem causes like
resource fragmentation and control-plane taints, plus `-o json` for automation.
It's in krew: `kubectl krew install why-pending`. Feedback / "it got my cluster
wrong" reports very welcome 🙏
https://github.com/SaiRohithGuntupally/kubectl-why-pending
```

---

## 4. X / Twitter (thread)

**How:** Post as a thread (reply-chain). First tweet has the hook + link; the
rest add detail. One or two hashtags max.

```
1/ Ever had a Kubernetes pod stuck `Pending` with a useless FailedScheduling
event? I built a kubectl plugin that just tells you *why* — and how to fix it.
Now in the official krew index.

    kubectl krew install why-pending
https://github.com/SaiRohithGuntupally/kubectl-why-pending

2/ On bare metal there's no autoscaler to hide it. why-pending re-runs the
scheduler's filtering locally and explains, per node: resource fragmentation,
control-plane taints, GPU/extended-resource shortfalls, topology-spread skew,
anti-affinity, unbound PVCs.

3/ For GPU clusters: when no node advertises the GPU, it walks the enablement
chain (NFD → driver → device-plugin → GFD → DCGM → MIG-manager) and names the
broken link. It also diagnoses DRA (k8s 1.34+) resourceClaims — claims,
DeviceClasses, ResourceSlices — which almost nothing else explains yet.

4/ It also does `-o json` with a per-node breakdown — which was a 2017 k8s
feature request (#53908 "WhyPending") that got closed without shipping. Great for
CI gates + dashboards.

5/ The design I like: the analysis engine takes plain structs, no k8s client — so
the scheduler logic is unit-tested against fake clusters, no kind/minikube.
Releases are fully automated on `git tag`.

6/ MIT, feedback very welcome — especially "it misdiagnosed my cluster" reports.
#kubernetes #golang
```

---

## 5. LinkedIn (professional framing — good for the portfolio angle)

**How:** Personal post (not an article). Narrative + a few hashtags.

```
I shipped something I'm proud of: kubectl-why-pending — an open-source kubectl
plugin that's now in the official Kubernetes krew index.

If you run Kubernetes on bare metal, you know the pain: a pod stuck "Pending"
with a vague FailedScheduling event that never tells you what to actually fix.
This tool re-runs the scheduler's logic locally and explains, in plain English,
why the pod won't schedule — resource fragmentation, control-plane taints, GPU
shortfalls, topology spread, and more — plus the exact fix. For GPU clusters it
even pinpoints the broken link in the NVIDIA enablement chain and diagnoses the
new Dynamic Resource Allocation (DRA) claims in Kubernetes 1.34.

It even adds machine-readable output (-o json), something Kubernetes itself had
as a feature request back in 2017 and never shipped.

Install:  kubectl krew install why-pending
Code (MIT):  https://github.com/SaiRohithGuntupally/kubectl-why-pending

Built in Go, with a fully unit-tested scheduler engine and automated releases.
Feedback welcome — especially "it got my cluster wrong" reports.

#kubernetes #devops #opensource #golang #sre
```

---

## 6. r/devops

**How:** Check the rules — many devops subs only allow self-promotion in a
**weekly/pinned "self-promo" or "what are you working on" thread**. If so, post
there, not as a standalone. Same body as r/kubernetes, lightly reworded to be
less K8s-insider.

---

## 7. lobste.rs (HN-like, often kinder to new tools)

**How:** **Invite-only** — you need an existing member to invite you. If you get
in: submit the repo URL, tag `devops` + `go`, and post a short authored comment
(reuse the Show HN first comment from `docs/show-hn.md`).

---

## 8. Hacker News — Show HN (later, ~2 weeks out)

Currently gated for new accounts (the message you got). Warm up: read the
guidelines, comment thoughtfully on stories you care about, gain some karma over
~2 weeks, then post. Draft is ready in `docs/show-hn.md`. By then you'll also
have "already on krew + written up on dev.to + used by N people" as credibility.

---

## Measuring what works

krew doesn't expose install counts directly, but you can track:
- **GitHub stars** and the repo's **Insights → Traffic** (views/clones/referrers)
- **Release asset download counts** (GitHub API per release)
- Which channel sent the most traffic (Traffic → Referring sites)

Double down on whatever channel actually moves the needle.
```
