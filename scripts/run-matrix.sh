#!/usr/bin/env bash
# Run the canonical seven-run test matrix in sequence.
#
# Usage:
#   scripts/run-matrix.sh                # full duration (10 min each)
#   DURATION=60s scripts/run-matrix.sh   # short smoke run
#   ONLY="run1-baseline-64core run2-headline" scripts/run-matrix.sh
set -euo pipefail

cd "$(dirname "$0")/.."

BIN_DIR=${BIN_DIR:-bin}
RESULTS_DIR=${RESULTS_DIR:-results}
DURATION=${DURATION:-}
ONLY=${ONLY:-}

mkdir -p "$RESULTS_DIR"

RUNS=(
  run1-baseline-64core
  run2-headline
  run3-sync-worst
  run4-mitigation
  run5-mitigation-sync
  run6-future-bloat
  run7-go125-no-cgroup
)

run_id_match() {
  local id=$1
  if [[ -z "$ONLY" ]]; then
    return 0
  fi
  for w in $ONLY; do
    if [[ "$w" == "$id" ]]; then
      return 0
    fi
  done
  return 1
}

for id in "${RUNS[@]}"; do
  run_id_match "$id" || continue
  cfg="test-matrix/${id}.yaml"
  if [[ ! -f "$cfg" ]]; then
    echo "matrix: missing $cfg; skipping" >&2
    continue
  fi
  override=""
  if [[ -n "$DURATION" ]]; then
    override_cfg=$(mktemp -t "${id}.XXXXXX.yaml")
    sed "s/^duration:.*/duration: ${DURATION}/" "$cfg" > "$override_cfg"
    cfg="$override_cfg"
  fi
  bin="$BIN_DIR/chaos-launcher"
  if [[ "$id" == "run7-go125-no-cgroup" && -x "$BIN_DIR/chaos-worker-go125" ]]; then
    # For run7, point launcher at a per-run bin dir that has the Go 1.25 worker.
    workdir=$(mktemp -d)
    ln -s "$(realpath "$BIN_DIR/chaos-worker-go125")" "$workdir/chaos-worker"
    for b in chaos-launcher chaos-victim chaos-observer; do
      ln -s "$(realpath "$BIN_DIR/$b")" "$workdir/$b"
    done
    bin="$workdir/chaos-launcher"
  fi
  echo
  echo "=========================="
  echo "matrix: launching $id"
  echo "  config: $cfg"
  echo "  bin:    $bin"
  echo "=========================="
  "$bin" --config "$cfg" --output-dir "$RESULTS_DIR" --bin-dir "$(dirname "$bin")" || {
    echo "matrix: $id failed" >&2
    exit 1
  }
  if [[ -n "$DURATION" ]]; then
    rm -f "$cfg"
  fi
done

echo
echo "matrix: all done. summaries in $RESULTS_DIR/<run_id>/summary.md"
