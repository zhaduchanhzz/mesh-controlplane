#!/usr/bin/env bash
# P13f: static checks of the monitoring configuration, with the pinned
# images (no service is started, nothing is scraped):
#   promtool check config / check rules / test rules
#   amtool check-config
#   docker compose config (syntax; secrets files are replaced by empty ones)
#   the Grafana dashboard is valid JSON
# Run on any host with Docker (mc-ci): infra/lab/monitoring/check.sh
set -euo pipefail

cd "$(dirname "$0")"

prom=prom/prometheus:v3.5.0
am=prom/alertmanager:v0.28.1

run() { docker run --rm --network none -v "$PWD:/m:ro" -w /m "$@"; }

echo "== promtool check config (rule files resolved in the container)"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
chmod 0755 "$tmp" # the images run as nobody
sed 's#/etc/prometheus/rules/#/m/prometheus/rules/#' prometheus/prometheus.yml >"$tmp/prometheus.yml"
docker run --rm --network none -v "$PWD:/m:ro" -v "$tmp:/t:ro" --entrypoint promtool "$prom" check config /t/prometheus.yml

echo "== promtool check rules"
run --entrypoint promtool "$prom" check rules prometheus/rules/mesh.rules.yml

echo "== promtool test rules"
run -w /m/prometheus/tests --entrypoint promtool "$prom" test rules mesh.rules.test.yml

echo "== amtool check-config"
mkdir -p "$tmp/am"
cp alertmanager/alertmanager.yml "$tmp/am/alertmanager.yml"
sed -i 's#/etc/alertmanager/secrets/webhook-url#/t/webhook-url#' "$tmp/am/alertmanager.yml"
echo "http://127.0.0.1:9/placeholder" >"$tmp/am/webhook-url"
sed -i 's#/t/webhook-url#/t/am/webhook-url#' "$tmp/am/alertmanager.yml"
chmod -R a+rX "$tmp"
docker run --rm --network none -v "$tmp:/t:ro" --entrypoint amtool "$am" check-config /t/am/alertmanager.yml

echo "== docker compose config"
mkdir -p "$tmp/secrets"
sed "s#/opt/mc-mon/secrets/#$tmp/secrets/#g" docker-compose.yml >"$tmp/docker-compose.yml"
for f in grafana pg-exporter-01 pg-exporter-02 pg-exporter-platform pgbouncer-exporter-01 pgbouncer-exporter-02; do
	: >"$tmp/secrets/$f.env"
done
: >"$tmp/secrets/alertmanager-webhook-url"
if docker compose version >/dev/null 2>&1; then
	docker compose -f "$tmp/docker-compose.yml" --project-directory "$PWD" config -q
else
	echo "SKIP: no docker compose plugin here (yaml syntax only)"
	python3 -c "import sys, yaml; yaml.safe_load(open(sys.argv[1]))" docker-compose.yml
fi

echo "== dashboard JSON"
python3 -m json.tool grafana/dashboards/mesh-control-plane.json >/dev/null

echo "all monitoring checks passed"
