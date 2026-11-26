#!/usr/bin/env bash
# P12b E2E helper jobs, copied by run.sh to mc-ci (and to client VMs) and
# started there as transient systemd units (`systemd-run ... bash jobs.sh`).
# An ssh background job (`nohup ... &`) did not start in the P12 E2E (run at
# 596ce92), and an inline `bash -c` under systemd-run would have its $VARS and
# %specifiers expanded by systemd: hence a script file with plain arguments.
#
#   jobs.sh ping CONTAINER IP OUT STOPFILE [MAXSEC]
#       one ICMP echo every 200 ms from CONTAINER (docker, through its tun) to
#       IP until STOPFILE exists or MAXSEC (default 900) passed; lines
#       "<epoch ms> <rc>" in OUT.
#   jobs.sh rb-docker DIR SLUG BOOT_URL FROM_URL PAR
#       DIR/mesh-rebootstrap for the containers listed in DIR/targets-SLUG,
#       key DIR/key-SLUG; writes DIR/rb-SLUG.json, .log and .done (exit code).
#   jobs.sh rb-vm DIR VER BOOT_URL FROM_URL SLUG
#       DIR/mesh-rebootstrap on a client VM for its second tailscaled
#       (/opt/tailscale/VER, socket DIR/tailscaled.sock, key DIR/key); writes
#       DIR/rb.json, rb.log, rb.done.
set -u

case ${1:-} in
ping)
  c=$2 ip=$3 out=$4 stop=$5 max=${6:-900}
  end=$(($(date +%s) + max))
  while [ ! -e "$stop" ] && [ "$(date +%s)" -lt "$end" ]; do
    t=$(date +%s%3N)
    timeout 3 docker exec "$c" ping -c 1 -W 1 "$ip" >/dev/null 2>&1
    echo "$t $?"
    sleep 0.2
  done >"$out" 2>&1
  ;;
rb-docker)
  d=$2 s=$3 boot=$4 from=$5 par=$6
  "$d/mesh-rebootstrap" -bootstrap "$boot" -authkey-file "$d/key-$s" -tenant "$s" \
    -targets "$d/targets-$s" -concurrency "$par" -timeout 120s -max-wait 5m -poll 1s \
    -from "$from" -out "$d/rb-$s.json" \
    -cmd "docker exec -i {target} timeout 100 tailscale up --login-server={login_server} --auth-key=file:/dev/stdin --hostname={target} --force-reauth <$d/key-$s" \
    >"$d/rb-$s.log" 2>&1
  echo $? >"$d/rb-$s.done"
  ;;
rb-vm)
  d=$2 ver=$3 boot=$4 from=$5 s=$6
  h=$(hostname -s)-p12b
  echo local >"$d/targets"
  "$d/mesh-rebootstrap" -bootstrap "$boot" -authkey-file "$d/key" -tenant "$s" \
    -targets "$d/targets" -concurrency 1 -timeout 120s -max-wait 5m -poll 1s \
    -from "$from" -out "$d/rb.json" \
    -cmd "timeout 100 /opt/tailscale/$ver/tailscale --socket=$d/tailscaled.sock up --login-server={login_server} --auth-key=file:$d/key --hostname=$h --accept-dns=false --netfilter-mode=off --force-reauth" \
    >"$d/rb.log" 2>&1
  echo $? >"$d/rb.done"
  ;;
*)
  echo "usage: jobs.sh ping|rb-docker|rb-vm ..." >&2
  exit 2
  ;;
esac
