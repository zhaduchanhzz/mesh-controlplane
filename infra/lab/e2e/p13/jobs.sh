#!/usr/bin/env bash
# P13 drill helper jobs, copied by lib.sh to mc-ci and started there as
# transient systemd units (systemd-run ... bash jobs.sh MODE ...), like
# infra/lab/e2e/p12b/jobs.sh. Every line starts with the mc-ci clock in epoch
# ms, so the drill compares every probe on one clock.
#
#   jobs.sh ping CONTAINER IP OUT STOPFILE [MAXSEC]
#       one ICMP echo every 200 ms from CONTAINER (docker, through its tun);
#       "<ms> <rc>".
#   jobs.sh ready URL OUT STOPFILE [MAXSEC]
#       GET URL/ready every 250 ms through the public path; "<ms> <http code>".
#   jobs.sh wprobe URL KEYFILE DEVICE_ID TAG OUT STOPFILE [MAXSEC]
#       a control-plane write every 500 ms: POST /api/v2/device/ID/name with
#       the tailnet's API key (a fenced write by the cluster's active
#       process); "<ms> <http code>".
#   jobs.sh sqlw PASSFILE DBNAME HOSTS PORT OUT STOPFILE BASE [MAXSEC]
#       one INSERT into p13_probe (seq BASE+1, BASE+2, ...) every 200 ms through the client DSN
#       (host list, target_session_attrs=read-write, connect_timeout=2), a new
#       connection per write, bounded to 5 s; "<seq> ok <ms> <server>" or
#       "<seq> fail <ms> <first error line>".
set -u

case ${1:-} in
ping)
  c=$2 ip=$3 out=$4 stop=$5 max=${6:-3600}
  end=$(($(date +%s) + max))
  while [ ! -e "$stop" ] && [ "$(date +%s)" -lt "$end" ]; do
    t=$(date +%s%3N)
    timeout 3 docker exec "$c" ping -c 1 -W 1 "$ip" >/dev/null 2>&1
    echo "$t $?"
    sleep 0.2
  done >"$out" 2>&1
  ;;
ready)
  u=$2 out=$3 stop=$4 max=${5:-3600}
  end=$(($(date +%s) + max))
  while [ ! -e "$stop" ] && [ "$(date +%s)" -lt "$end" ]; do
    t=$(date +%s%3N)
    c=$(curl -s -o /dev/null -w '%{http_code}' -m 2 "$u/ready")
    echo "$t ${c:-000}"
    sleep 0.25
  done >"$out" 2>&1
  ;;
wprobe)
  u=$2 kf=$3 id=$4 tag=$5 out=$6 stop=$7 max=${8:-3600}
  end=$(($(date +%s) + max)) i=0
  while [ ! -e "$stop" ] && [ "$(date +%s)" -lt "$end" ]; do
    i=$((i + 1))
    t=$(date +%s%3N)
    c=$(curl -s -o /dev/null -w '%{http_code}' -m 3 -X POST -H "Authorization: Bearer $(cat "$kf")" \
      -H 'Content-Type: application/json' --data "{\"name\":\"$tag-$((i % 2))\"}" "$u/api/v2/device/$id/name")
    echo "$t ${c:-000}"
    sleep 0.5
  done >"$out" 2>&1
  ;;
sqlw)
  pf=$2 db=$3 hosts=$4 port=$5 out=$6 stop=$7 base=$8 max=${9:-3600}
  dsn="host=$hosts port=$port dbname=$db user=headscale target_session_attrs=read-write connect_timeout=2 passfile=$pf"
  end=$(($(date +%s) + max)) i=$base
  while [ ! -e "$stop" ] && [ "$(date +%s)" -lt "$end" ]; do
    i=$((i + 1))
    t=$(date +%s%3N)
    if o=$(timeout 5 psql "$dsn" -X -q -At -v ON_ERROR_STOP=1 \
      -c "INSERT INTO p13_probe(seq, server) VALUES ($i, inet_server_addr()) RETURNING host(server)" 2>&1); then
      echo "$i ok $t $o"
    else
      [ -n "$o" ] || o="timeout"
      echo "$i fail $t $(printf '%s' "$o" | head -1 | tr ' ' '_' | cut -c1-120)"
    fi
    sleep 0.2
  done >"$out" 2>&1
  ;;
*)
  echo "usage: jobs.sh ping|ready|wprobe|sqlw ..." >&2
  exit 2
  ;;
esac
