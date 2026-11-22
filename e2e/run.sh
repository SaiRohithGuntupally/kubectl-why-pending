#!/usr/bin/env bash
# Real-apiserver e2e for kubectl-why-pending on kind.
#   ./e2e/run.sh            create cluster (if needed), apply scenarios, check
#   ./e2e/run.sh --check    re-run checks only
#   ./e2e/run.sh --down     delete the cluster
# Env: KIND_IMAGE=kindest/node:vX.Y.Z to pin a Kubernetes version.
set -uo pipefail
cd "$(dirname "$0")/.."
CLUSTER=why-pending-e2e
CTX=kind-$CLUSTER
OUT=e2e/out
BIN=./bin/kubectl-why_pending
K="kubectl --context $CTX"

if [[ "${1:-}" == "--down" ]]; then kind delete cluster --name $CLUSTER; exit; fi

go build -ldflags "-X main.version=e2e" -o $BIN . || exit 3

if [[ "${1:-}" != "--check" ]]; then
  if ! kind get clusters 2>/dev/null | grep -qx $CLUSTER; then
    kind create cluster --name $CLUSTER --config e2e/kind-config.yaml ${KIND_IMAGE:+--image $KIND_IMAGE} --wait 120s || exit 3
  fi
  $K label node $CLUSTER-worker  zone=a --overwrite
  $K label node $CLUSTER-worker2 zone=b --overwrite
  $K label node $CLUSTER-worker3 zone=c e2e/special=true --overwrite
  $K taint node $CLUSTER-worker3 dedicated=special:NoSchedule --overwrite
  for f in e2e/scenarios/*.yaml; do
    ns=$(basename "$f" | cut -d- -f1)
    $K create namespace "$ns" --dry-run=client -o yaml | $K apply -f - >/dev/null
    $K apply -f "$f" >/dev/null
    if [[ "$f" == *fillers* ]]; then  # must be placed before the pod they squeeze
      $K -n "$ns" wait --for=condition=Ready pod --all --timeout=120s >/dev/null || echo "warning: $ns fillers not Ready"
    fi
  done
  echo "waiting 75s for scheduling attempts, events and image pulls (s16 needs >60s)..."
  sleep 75
fi

mkdir -p $OUT
$K version 2>/dev/null | grep -i server
pass=0; fail=0; known=0; fixed=0
printf '\n%-4s %-10s %-5s %s\n' NS RESULT EXIT FIRST-CAUSE
while IFS=$'\t' read -r ns want_exit want_sub type note; do
  [[ -z "$ns" || "$ns" == \#* ]] && continue
  $BIN --context $CTX -n "$ns" --no-color > $OUT/$ns.txt 2>&1; code=$?
  $BIN --context $CTX -n "$ns" -o json > $OUT/$ns.json 2>/dev/null
  first=$(jq -r '[.[].causes[0].title] | join(" | ")' $OUT/$ns.json 2>/dev/null)
  [[ -z "$first" ]] && first=$(head -1 $OUT/$ns.txt)
  ok=1
  [[ "$code" == "$want_exit" ]] || ok=0
  grep -qiF -- "$want_sub" $OUT/$ns.txt || ok=0
  case $ns in
    s05) jq -e '[.[].nodes[]? | select(.schedulable)] | length == 0' $OUT/$ns.json >/dev/null || ok=0 ;;
    w04) grep -qi 'pod affinity' $OUT/$ns.txt && ok=0 ;;
    s13) jq -e '[.[].causes[] | select(.severity=="blocker" and (.title|test("nodeSelector")))] | length == 0' $OUT/$ns.json >/dev/null && grep -q 'why-pending-e2e-worker3 (dedicated=special:NoSchedule)' $OUT/$ns.txt || ok=0 ;;
    w05) jq -e '[.[].causes[] | select(.severity=="blocker" and (.title|test("PersistentVolumeClaim")))] | length == 0' $OUT/$ns.json >/dev/null || ok=0 ;;
  esac
  if [[ $ok == 1 && $type == works ]]; then r=PASS; ((pass++))
  elif [[ $ok == 1 ]]; then r=FIXED; ((fixed++))
  elif [[ $type == known-bug ]]; then r=KNOWN-BUG; ((known++))
  else r=FAIL; ((fail++)); fi
  printf '%-4s %-10s %-5s %s\n' "$ns" "$r" "$code" "${first:0:90}"
done < e2e/expected.tsv

# Did the raw scheduler event render anywhere? (audit risk 16)
ev=$(grep -l '^  scheduler:' $OUT/s0*.txt 2>/dev/null | wc -l | tr -d ' ')
echo; echo "scheduler event line rendered in $ev of the s* scenarios (risk 16 if 0)"
# -A should still surface the image-pull pod (risk 20)
$BIN --context $CTX -A --no-color > $OUT/all.txt 2>&1
grep -q bad-image $OUT/all.txt && echo "-A shows w03/bad-image: yes" || echo "-A shows w03/bad-image: NO (risk 20)"
echo; echo "pass=$pass fail=$fail known-bug=$known fixed=$fixed   outputs in $OUT/"
[[ $fail == 0 ]]
