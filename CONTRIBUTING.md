# Contributing to kubectl-why-pending

Thanks for helping make `Pending`-pod debugging less painful. Bug reports,
diagnoses it got wrong, and new scheduling cases are all welcome.

## The most useful contribution

**"It misdiagnosed my cluster."** If `why-pending` gave the wrong cause (or missed
one), open an issue with:

- the pod spec (`kubectl get pod <name> -o yaml`), and
- the node shape (`kubectl get nodes -o wide`, plus relevant taints/labels).

That's exactly what's needed to add the case as a test. Real-world scheduling
failures are the roadmap.

## Project layout

```
main.go        # CLI: flags, kube client, gather cluster state
report.go      # rendering (colored, severity-ranked)
args.go        # argument parsing
pkg/diagnose/  # the pure analysis engine — NO kubernetes client
  resources.go   # CPU/mem/extended request + allocatable math
  predicates.go  # taints, nodeSelector, nodeAffinity matching
  extended.go    # GPU / hugepages / device-plugin resources
  topology.go    # pod topology spread + inter-pod (anti-)affinity
  analyze.go     # orchestration: classify nodes -> ranked causes
```

The key design rule: **`pkg/diagnose` takes plain data structs, never a
Kubernetes client.** That's what lets the scheduler logic be unit-tested against
hand-built clusters with no kind/minikube. Keep it that way — the CLI gathers
state, the engine reasons about it.

## Dev workflow

```sh
make test    # go test ./...  (unit + end-to-end via a fake API)
make vet     # go vet ./...
make build   # local binary
make demo    # run the full pipeline against a fake cluster and print a report
```

A change should keep `make test` and `make vet` green. CI runs both (plus
`go test -race`) on every push and PR.

## Adding a new diagnosis

1. Put the detection logic in `pkg/diagnose` as a pure function over nodes/pods.
2. Have it return one or more `Cause{Severity, Title, Detail, Fix}` — every
   finding must include an actionable **Fix**.
3. Wire it into `Analyze` (before the dynamic-cause fallback).
4. Add a table-style unit test with a hand-built cluster that triggers it, and one
   that confirms it stays quiet when it shouldn't fire.
5. Update the "What it detects" list in the README.

Style: match the surrounding code; explanations should read like a teammate, not
a stack trace; prefer plain English over Kubernetes jargon where possible.

## Releases (maintainers)

Releases are automated. Push a semver tag and the rest happens on its own:

```sh
git tag v0.3.0 && git push origin v0.3.0
```

That triggers `.github/workflows/release.yml`, which cross-compiles all four
platforms, publishes a GitHub Release with checksums, and runs
[krew-release-bot](https://github.com/rajatjindal/krew-release-bot) to open an
auto-approved PR to krew-index. No manual manifest edits.

## License

By contributing, you agree your contributions are licensed under the
[MIT License](LICENSE).
