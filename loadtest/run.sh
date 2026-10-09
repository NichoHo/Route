#!/usr/bin/env bash
# Run one load-test scenario on a fresh stack and write its report.
#
#   loadtest/run.sh <scenario> [label] [batch-window-ms]
#
#   loadtest/run.sh baseline
#   loadtest/run.sh solver-comparison solver-comparison-window-0ms 0
#
# Results land in loadtest/results/<label>.json and <label>.svg. The stack runs
# under its own compose project name (routeload) and is removed afterwards,
# volumes included, so every run starts from an empty database.
set -euo pipefail
cd "$(dirname "$0")/.."

# Git Bash on Windows rewrites arguments that look like Unix paths (such as
# SIM_PROFILE=/scenarios/x.json) into Windows paths. Turn that off.
export MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL="*"

scenario=${1:?usage: loadtest/run.sh <scenario> [label] [batch-window-ms]}
label=${2:-$scenario}
window_ms=${3:-2000}

[ -f "loadtest/scenarios/$scenario.json" ] || { echo "no such scenario: $scenario" >&2; exit 2; }
mkdir -p loadtest/results

export BENCH_SCENARIO=$scenario BENCH_LABEL=$label BENCH_WINDOW_MS=$window_ms
export BATCH_WINDOW=${window_ms}ms
export BENCH_HARDWARE=${BENCH_HARDWARE:-$(docker info --format 'Docker VM: {{.NCPU}} CPUs, {{.MemTotal}} bytes RAM, {{.OperatingSystem}}')}

DC="docker-compose -p routeload -f deploy/compose/docker-compose.yml -f deploy/compose/docker-compose.load.yml"
cleanup() { $DC down -v >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup

echo "== $label: starting the stack (batch window ${window_ms}ms)"
$DC up -d --build postgres redis redpanda routed >/dev/null
for _ in $(seq 1 90); do
  $DC exec -T routed wget -qO- http://localhost:8080/healthz >/dev/null 2>&1 && break
  sleep 2
done

echo "== $label: starting the watcher"
$DC up -d --no-deps --build routebench >/dev/null

echo "== $label: running the scenario"
$DC run --rm -e "SIM_PROFILE=/scenarios/$scenario.json" routesim 2>&1 | grep -vE "^ Container |^ *$" | tail -45

echo "== $label: waiting for the report"
docker wait "$($DC ps -aq routebench)" >/dev/null
$DC logs --no-color routebench 2>&1 | grep -E "^routebench-1 +\|" | sed 's/^routebench-1 *| //' | tail -15
