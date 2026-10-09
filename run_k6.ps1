$ErrorActionPreference = "Stop"

$token = if ($env:ROUTE_API_TOKEN) { $env:ROUTE_API_TOKEN } else { "dev-only-token-change-me" }

docker run --rm -e "ROUTE_API_TOKEN=$token" -v "${PWD}/loadtest:/loadtest" grafana/k6 run /loadtest/scenarios/k6_http.js --out json=/loadtest/results/baseline.json
