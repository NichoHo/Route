#!/usr/bin/env bash
# Run the load-test scenarios one after another, then build the comparison and
# the summary.
#
#   loadtest/run-all.sh                      every scenario in the spec's table (about 45 minutes)
#   loadtest/run-all.sh baseline hotspot-spike    just these
#
# "solver-comparison" runs four times, once per batch window.
set -euo pipefail
cd "$(dirname "$0")/.."
export MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL="*"

scenarios=("$@")
[ ${#scenarios[@]} -gt 0 ] || scenarios=(baseline throughput ping-flood hotspot-spike contention solver-comparison)

for s in "${scenarios[@]}"; do
  if [ "$s" = "solver-comparison" ]; then
    for ms in 0 1000 2000 4000; do
      loadtest/run.sh solver-comparison "solver-comparison-window-${ms}ms" "$ms"
    done
  else
    loadtest/run.sh "$s"
  fi
done

# The report tool is built into the routebench image by run.sh.
tool() { docker run --rm -v "$PWD/loadtest/results:/results" --entrypoint /routebench routeload-routebench "$@" /results; }
case " ${scenarios[*]} " in *" solver-comparison "*) tool compare ;; esac
tool summary
