#!/usr/bin/env bash
# Idle cost of N tailnet runtimes in one serve-tailnets process (P02 POC binary).
# Usage: copy a serve-tailnets binary to /tmp/headscale on a Linux host, then: bash runtime-idle-cost.sh
# Results 2026-11-22 (mc-ci): see design/adr/0004-tailnet-database-per-runtime.md
set -u
B=/tmp/headscale
for N in 1 10 50; do
  D=$(mktemp -d /tmp/rtb.XXXX); cd $D
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj /CN=bench -keyout k.pem -out c.pem >/dev/null 2>&1
  printf "regions:\n  900:\n    regionid: 900\n    regioncode: b\n    regionname: bench\n    nodes:\n      - name: 900a\n        regionid: 900\n        hostname: derp.invalid\n        stunport: -1\n" > derp.yaml
  echo "{\"grants\":[]}" > pol.hujson
  ARGS=()
  for i in $(seq 1 $N); do
    id=$(python3 -c "import uuid;print(uuid.uuid4())"); mkdir -p t$i
    m=""; [ $i = 1 ] && m="127.0.0.1:19090"
    cat > t$i.yaml <<Y
server_url: https://t$i.bench.test:18443
listen_addr: 127.0.0.1:$((20000+i))
metrics_listen_addr: "$m"
unix_socket: $D/t$i.sock
disable_check_updates: true
noise: {private_key_path: $D/t$i/noise.key}
prefixes: {v4: 100.64.0.0/10, v6: "fd7a:115c:a1e0::/48"}
policy: {mode: file, path: $D/pol.hujson}
derp: {server: {enabled: false}, urls: [], paths: [$D/derp.yaml], auto_update_enabled: false}
database: {type: sqlite, sqlite: {path: $D/t$i/db.sqlite}}
dns: {magic_dns: false, override_local_dns: false}
log: {level: warn}
Y
    ARGS+=(--tailnet "id=$id;hosts=t$i.bench.test;config=$D/t$i.yaml")
  done
  $B -c $D/t1.yaml serve-tailnets --listen 127.0.0.1:18443 --tls-cert c.pem --tls-key k.pem "${ARGS[@]}" >$D/log 2>&1 &
  P=$!; sleep 25
  if kill -0 $P 2>/dev/null; then
    rss=$(awk "/VmRSS/{print \$2}" /proc/$P/status); th=$(awk "/Threads/{print \$2}" /proc/$P/status)
    gr=$(curl -s 127.0.0.1:19090/metrics | awk "/^go_goroutines /{print \$2}")
    heap=$(curl -s 127.0.0.1:19090/metrics | awk "/^go_memstats_heap_inuse_bytes /{printf \"%.1f\", \$2/1048576}")
    fds=$(ls /proc/$P/fd | wc -l)
    echo "N=$N rss_kb=$rss heap_mib=$heap goroutines=$gr threads=$th fds=$fds"
  else
    echo "N=$N FAILED: $(tail -3 $D/log)"
  fi
  kill $P 2>/dev/null; wait $P 2>/dev/null; cd /; rm -rf $D
done
