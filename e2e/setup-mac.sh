#!/usr/bin/env bash
# One-time setup + full e2e run on an Apple Silicon Mac (meant for the Mac mini).
#   ./e2e/setup-mac.sh
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH=/opt/homebrew/bin:$PATH
for pkg in go kind kubectl jq colima docker; do
  command -v "$pkg" >/dev/null || brew install "$pkg"
done
# kind needs a roomier VM than Colima's 2 CPU / 2 GB default (4 nodes).
if ! colima status >/dev/null 2>&1; then
  colima start --cpu 4 --memory 8 --disk 40
fi
docker info >/dev/null
./e2e/run.sh
echo
echo "Done. Re-check only: ./e2e/run.sh --check    Tear down: ./e2e/run.sh --down && colima stop"
