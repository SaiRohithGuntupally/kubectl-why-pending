# krew-index submission — how to open the PR

> **Note:** the initial submission is already merged, and updates are now
> **automated** by [krew-release-bot](https://github.com/rajatjindal/krew-release-bot)
> via `.github/workflows/release.yml` — every `v*` tag auto-renders the root
> `.krew.yaml` template and opens an auto-approved krew-index PR. The manual steps
> below are kept only as a record of the one-time bootstrap; you shouldn't need
> them again.

This directory holds everything needed to add `why-pending` to the central
[krew-index](https://github.com/kubernetes-sigs/krew-index), which is what makes
`kubectl krew install why-pending` work for everyone.

## Steps

1. **Fork** https://github.com/kubernetes-sigs/krew-index
2. Copy `why-pending.yaml` (next to this file) into the fork at
   **`plugins/why-pending.yaml`**
3. **Validate locally first** (catches problems before CI does):
   ```sh
   kubectl krew install --manifest=plugins/why-pending.yaml
   kubectl why-pending --help     # confirm it runs
   kubectl krew uninstall why-pending
   ```
4. Commit on a branch, push to your fork, and open a PR against
   `kubernetes-sigs/krew-index:master`.
5. Paste the PR body below. The krew-index bot runs CI that re-downloads each
   archive and re-checks the sha256 — already verified to match.

---

## PR title

```
Add why-pending plugin
```

## PR body

```markdown
This adds `why-pending`, a kubectl plugin that explains why a pod is stuck
Pending and suggests the fix — re-running the scheduler's filtering logic
locally and reporting causes in plain English.

It focuses on the scheduling failures that are hard to read from
`kubectl describe` events, especially on bare-metal / on-prem clusters with no
autoscaler: resource fragmentation (capacity exists but not on one node),
control-plane taints, topology-spread skew, anti-affinity exhaustion, a pod
larger than any node, nodeSelector/affinity mismatches, cordoned/NotReady nodes,
and unbound PVCs.

- Repo: https://github.com/SaiRohithGuntupally/kubectl-why-pending
- Release: https://github.com/SaiRohithGuntupally/kubectl-why-pending/releases/tag/v0.2.0
- License: MIT
- Platforms: darwin/amd64, darwin/arm64, linux/amd64, linux/arm64
- Read-only: the plugin only lists nodes/pods/events/PVCs and makes no changes.

I tested installation locally with:
`kubectl krew install --manifest=plugins/why-pending.yaml`

### Plugin submission checklist
- [x] The plugin name follows the [naming guide](https://krew.sigs.k8s.io/docs/developer-guide/develop/naming-guide/).
- [x] The `shortDescription` is concise and the `description` explains what the plugin does.
- [x] The manifest validates (`kubectl krew install --manifest=...` succeeds).
- [x] `sha256` sums match the released archives.
- [x] The plugin works on all listed platforms.
- [x] The source is publicly available and the license is included.
```
