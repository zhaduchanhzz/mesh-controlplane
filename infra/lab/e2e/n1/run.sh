#!/usr/bin/env bash
# Gate N1 real-client E2E (P09): tailnet isolation on cluster mode with real
# Tailscale clients on separate underlays, an own DERP relay with per-cluster
# verify (ADR 0009), incremental netmap OFF or ON (MODE), client version V.
#
#   c1 (mc-cp-c1): mc-cp + mc-c1b behind a transient HAProxy, https://c1.cp.gta5web.online:8443
#                  tailnets A and B (two tailnets, one process, one Noise key)
#   c2 (mc-c2):    mc-c2 + mc-c2b behind mesh-c2-ha, https://c2.cp.gta5web.online:8445
#                  tailnet C
#   relay:         mc-relay 10.77.40.30: derper region 901 (c1, tcp/443, STUN
#                  udp/3478) and 902 (c2, tcp/444), verify = the cluster's
#                  /verify through its public URL with a per-run secret
#   clients:       mc-client-a1/a2 (underlay A, tailnet A), mc-client-b1/b2
#                  (underlay B, tailnet B); tailscaled /opt/tailscale/$V as a
#                  transient unit, fresh state each run
#
# Batch 1 cases: E00 environment (relay, verify secret, underlay isolation),
# S01, S02, S03, S11 (n1probe switch-tenant), S15 (relay region, relay path,
# unknown key refused by derp-check). Batch 2 (cases2.sh): S07, S08, S10,
# S12, S14, S16, S17, S18, M06. Batch 3 (cases3.sh): S04, S05, S06, S09, S13,
# S13b (bootstrap flood; needs BOOT_BIN).
#
# Usage: MODE=off|on V=1.104.7|1.84.3 BIN=… PROBE_BIN=… BOOT_BIN=… infra/lab/e2e/n1/run.sh
# Exit: 0 no FAIL, 1 any FAIL, 2 setup error or cleanup not proven.
# shellcheck disable=SC2029
set -u -o pipefail

MODE=${MODE:-off}
V=${V:-1.104.7}
SSH_KEY=${SSH_KEY:-$HOME/.mesh-lab/mesh-lab}
CP_SSH=mesh@192.168.1.241
C1B_SSH=mesh@192.168.1.245
C2_SSH=mesh@192.168.1.244
C2B_SSH=mesh@192.168.1.246
CI_SSH=mesh@192.168.1.240
PG_SSH=mesh@192.168.1.243
PG_LISTEN=192.168.1.243:6432
MC_DB_ENV=${MC_DB_ENV:-$HOME/.mesh-lab/mc-db.env}
C1_ID=mc-cp-c1
C2_ID=mc-c2
HOST1=c1.cp.gta5web.online
HOST2=c2.cp.gta5web.online
URL1=https://$HOST1:8443
URL2=https://$HOST2:8445
POC_HEALTH=https://a.cp.gta5web.online:8443/health
RELAY=10.77.40.30
A1=10.77.20.11 A2=10.77.20.12 B1=10.77.30.11 B2=10.77.30.12
# CI_BINDIR=<dir on mc-ci holding headscale, n1probe, mesh-bootstrap built
# there> instead of BIN/PROBE_BIN/BOOT_BIN: the lab hosts fetch the binaries
# from mc-ci over the LAN (transient unit n1-binsrv: plain HTTP on mc-ci's LAN
# address, binaries only, stopped after use) and check their sha256; only
# n1probe comes to the laptop once, for the VPS (S13, S13b). The P12b runs
# 20261126T041654Z and T042420Z failed copying 81 MB from the laptop.
CI_BINDIR=${CI_BINDIR:-}
BINPORT=${BINPORT:-18090}
if [ -n "$CI_BINDIR" ]; then
  [[ $CI_BINDIR =~ ^/[A-Za-z0-9/_.-]+$ ]] || { echo "CI_BINDIR: absolute path, [A-Za-z0-9/_.-] only" >&2; exit 2; }
  BIN=- PROBE_BIN=- BOOT_BIN=-
else
  BIN=${BIN:?BIN= linux/amd64 cluster binary (or CI_BINDIR=)}
  PROBE_BIN=${PROBE_BIN:?PROBE_BIN= linux/amd64 n1probe binary}
  BOOT_BIN=${BOOT_BIN:-} # linux/amd64 mesh-bootstrap (S13b); empty: S13b FAIL
fi
KEEP=${KEEP:-0}
TTL=10
PY=${PY:-python}

REPO=$(cd "$(dirname "$0")/../../../.." && pwd)
SHA=$(git -C "$REPO" rev-parse --short HEAD)
RUN_ID=$(date -u +%Y%m%dT%H%M%SZ)
PG_DB=e2e_n1$(tr 'A-Z' 'a-z' <<<"$RUN_ID")
ART_DIR=$REPO/artifacts/$SHA/N1/e2e-$RUN_ID-$MODE-$V
mkdir -p "$ART_DIR/logs"
TMPD=$(mktemp -d) && chmod 700 "$TMPD"
CR=$(printf '\r')

HS=/usr/local/bin/headscale-cluster-n1
CFG=/etc/headscale/cluster/config-n1.yaml
DERPMAP=/etc/headscale/cluster/derpmap-n1.yaml
VSECRET=/etc/headscale/cluster/derp-verify-n1
UNIT=mesh-n1-e2e
TSU=n1-tailscaled
TSS=/run/n1-ts/tailscaled.sock
TSDIR=/var/lib/n1-ts
CLI1="sudo -u headscale $HS -c $CFG cluster --cluster-id $C1_ID"
CLI2="sudo -u headscale $HS -c $CFG cluster --cluster-id $C2_ID"

redact() {
  sed -E \
    -e 's/(tskey|hskey)-[A-Za-z0-9_-]+/\1-<redacted>/g' \
    -e 's/secret=[A-Za-z0-9_-]+/secret=<redacted>/g' \
    -e 's#/register/[A-Za-z0-9_-]+#/register/<redacted>#g' \
    -e 's/((mkey|nodekey|discokey|nlpub|privkey):)([0-9a-f]{8})[0-9a-f]{56}/\1\3…/g'
}
log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$ART_DIR/run.log" >&2; }
SSHO=(-i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10)
PX="ProxyCommand=ssh -i $SSH_KEY -o IdentitiesOnly=yes -o BatchMode=yes $CI_SSH nc -q0 %h %p"
ssh_() { local h=$1; shift; ssh "${SSHO[@]}" "$h" "$@"; }
cp_() { ssh_ "$CP_SSH" "$@"; }
pg_() { ssh_ "$PG_SSH" "$@"; }
sshof() { case $1 in cp) echo $CP_SSH;; c1b) echo $C1B_SSH;; c2) echo $C2_SSH;; c2b) echo $C2B_SSH;; esac; }
on() { local n=$1; shift; ssh_ "$(sshof "$n")" "$@"; }
# Binaries. binsha NAME (headscale, n1probe, mesh-bootstrap): its sha256, read
# on mc-ci with CI_BINDIR (binsums), else from the local file.
binlocal() { case $1 in headscale) echo "$BIN";; n1probe) echo "$PROBE_BIN";; mesh-bootstrap) echo "$BOOT_BIN";; esac; }
binsums() {
  ssh_ "$CI_SSH" "cd $CI_BINDIR && sha256sum headscale n1probe mesh-bootstrap" | tr -d '\r' >"$TMPD/binsha" || return 1
  [ "$(awk 'length($1) == 64' "$TMPD/binsha" | wc -l)" = 3 ] || { log "CI_BINDIR: missing binaries"; return 1; }
}
binsha() {
  if [ -n "$CI_BINDIR" ]; then awk -v n="$1" '$2 == n { print $1 }' "$TMPD/binsha"
  else sha256sum "$(binlocal "$1")" | cut -d' ' -f1; fi
}
binsrv_start() {
  [ -n "$CI_BINDIR" ] || return 0
  ssh_ "$CI_SSH" "sudo systemctl stop n1-binsrv 2>/dev/null; sudo systemctl reset-failed n1-binsrv 2>/dev/null;     sudo systemd-run --quiet --collect --unit=n1-binsrv --uid=mesh --gid=mesh -p RuntimeMaxSec=900       python3 -m http.server $BINPORT --bind ${CI_SSH#*@} --directory $CI_BINDIR;     for i in \$(seq 1 20); do curl -fsS -o /dev/null -m 2 http://${CI_SSH#*@}:$BINPORT/ && exit 0; sleep 0.5; done; exit 1" || { log "n1-binsrv not answering"; return 1; }
}
binsrv_stop() { [ -z "$CI_BINDIR" ] || ssh_ "$CI_SSH" "sudo systemctl stop n1-binsrv 2>/dev/null; sudo systemctl reset-failed n1-binsrv 2>/dev/null; ! systemctl is-active -q n1-binsrv"; }
# put NODE NAME DEST: DEST on NODE (cp, c1b, c2, c2b) becomes binary NAME,
# sha256 checked on NODE; with CI_BINDIR NODE fetches it from mc-ci (LAN).
put() {
  local n=$1 name=$2 dst=$3 sum; sum=$(binsha "$2")
  [ ${#sum} = 64 ] || return 1
  if [ -n "$CI_BINDIR" ]; then on "$n" "curl -fsS --retry 3 -m 120 -o $dst http://${CI_SSH#*@}:$BINPORT/$name" || return 1
  else scp -q "${SSHO[@]}" "$(binlocal "$name")" "$(sshof "$n"):$dst" || return 1; fi
  on "$n" "echo '$sum  $dst' | sha256sum -c --quiet" || { log "$n: $name sha256 mismatch"; return 1; }
}
vm() { local ip=$1; shift; ssh "${SSHO[@]}" -o StrictHostKeyChecking=accept-new -o "$PX" "mesh@$ip" "$@"; }
ipof() { case $1 in cp) echo 192.168.1.241;; c1b) echo 192.168.1.245;; c2) echo 192.168.1.244;; c2b) echo 192.168.1.246;; esac; }
clusterof() { case $1 in cp|c1b) echo c1;; *) echo c2;; esac; }
hostof() { [ "$1" = c1 ] && echo $HOST1 || echo $HOST2; }
urlof() { [ "$1" = c1 ] && echo $URL1 || echo $URL2; }
idof() { [ "$1" = c1 ] && echo $C1_ID || echo $C2_ID; }
nodesof() { [ "$1" = c1 ] && echo "cp c1b" || echo "c2 c2b"; }
py() { local code=$1; shift; "$PY" -c "import sys; sys.stdout.reconfigure(newline='\n')
$code" "$@"; }
strip_cr() { local v; v=$(cat); printf '%s' "${v//$CR/}"; }
now_ms() { py 'import time; print(int(time.time()*1000))' | strip_cr; }

FAILS=0
record() {
  printf '%s\t%s\t%s\n' "$1" "$2" "$3" >>"$ART_DIR/results.tsv"
  printf '%-5s %-4s %s\n' "$2" "$1" "$3" | tee -a "$ART_DIR/run.log"
  [ "$2" = FAIL ] && FAILS=$((FAILS + 1))
  return 0
}

# ------------------------------------------------------------- processes ---
direct() {
  local c h; h=$(hostof "$(clusterof "$1")")
  c=$(cp_ "curl -s -o /dev/null -w '%{http_code}' -m 3 --resolve '$h:8443:$(ipof "$1")' 'https://$h:8443$2'" 2>/dev/null)
  echo "${c:-000}"
}
public_ready() { curl -s -o /dev/null -w '%{http_code}' -m 3 "$(urlof "$1")/ready" || true; }
wait_direct_ready() { local end=$(( $(date +%s) + $2 )); while [ "$(date +%s)" -lt "$end" ]; do [ "$(direct "$1" /ready)" = 200 ] && return 0; sleep 0.2; done; return 1; }
wait_public_ready() { local i; for i in $(seq 1 "$2"); do [ "$(public_ready "$1")" = 200 ] && return 0; sleep 1; done; return 1; }
start_proc() {
  on "$1" "sudo systemctl reset-failed $UNIT 2>/dev/null; sudo systemd-run --quiet --unit=$UNIT --uid=headscale --gid=headscale \
    -p RuntimeDirectory=headscale-cluster -p RuntimeDirectoryMode=0770 \
    $HS -c $CFG cluster serve --cluster-id $(idof "$(clusterof "$1")")"
}
stop_proc() { on "$1" "sudo systemctl stop $UNIT 2>/dev/null; sudo systemctl reset-failed $UNIT 2>/dev/null; true"; }
active_of() { local n; for n in $(nodesof "$1"); do [ "$(direct "$n" /ready)" = 200 ] && { echo "$n"; return; }; done; echo none; }
standby_of() { local a n; a=$(active_of "$1"); for n in $(nodesof "$1"); do [ "$n" != "$a" ] && echo "$n"; done; }
cli() { local c=$1; shift; if [ "$c" = c1 ]; then cp_ "$CLI1 $*"; else on c2 "$CLI2 $*"; fi; }

# ---------------------------------------------------------------- clients ---
# Each client VM runs tailscaled $V as a transient unit with its own state.
TS() { local ip=$1; shift; vm "$ip" "sudo /opt/tailscale/$V/tailscale --socket=$TSS $*"; }
ts_start() {
  vm "$1" "sudo systemctl stop $TSU 2>/dev/null; sudo systemctl reset-failed $TSU 2>/dev/null; sudo rm -rf $TSDIR; sudo install -d -m 0700 $TSDIR
    sudo systemd-run --quiet --unit=$TSU -p RuntimeDirectory=n1-ts /opt/tailscale/$V/tailscaled \
      --state=$TSDIR/tailscaled.state --socket=$TSS --port=41641
    for i in \$(seq 1 40); do sudo /opt/tailscale/$V/tailscale --socket=$TSS status >/dev/null 2>&1; [ \$? -le 1 ] && exit 0; sleep 0.25; done; exit 1"
}
# ts_aside IP: park the client's state (its node identity) and start it
# fresh; ts_back IP: restore the parked state, so the client is again the same
# node (no duplicate nodes from re-enrolments).
ts_aside() { vm "$1" "sudo systemctl stop $TSU 2>/dev/null; sudo systemctl reset-failed $TSU 2>/dev/null; sudo rm -rf $TSDIR.keep; sudo mv $TSDIR $TSDIR.keep" && ts_start "$1"; }
ts_back() {
  vm "$1" "sudo systemctl stop $TSU 2>/dev/null; sudo systemctl reset-failed $TSU 2>/dev/null; sudo rm -rf $TSDIR; sudo mv $TSDIR.keep $TSDIR
    sudo systemd-run --quiet --unit=$TSU -p RuntimeDirectory=n1-ts /opt/tailscale/$V/tailscaled --state=$TSDIR/tailscaled.state --socket=$TSS --port=41641
    for i in \$(seq 1 40); do sudo /opt/tailscale/$V/tailscale --socket=$TSS status >/dev/null 2>&1; [ \$? -le 1 ] && exit 0; sleep 0.25; done; exit 1"
}
ts_stop() { vm "$1" "sudo systemctl stop $TSU 2>/dev/null; sudo systemctl reset-failed $TSU 2>/dev/null; sudo rm -rf $TSDIR; true"; }
ts_up() { # IP URL KEYFILE HOSTNAME
  vm "$1" "sudo timeout 60 /opt/tailscale/$V/tailscale --socket=$TSS up --login-server=$2 --auth-key=file:/dev/stdin --hostname=$4 --accept-dns=false" \
    <"$3" 2>&1 | redact >>"$ART_DIR/logs/$4.cli.log"
  return "${PIPESTATUS[0]}"
}
ts_json() { TS "$1" status --json 2>/dev/null; }
field() { ts_json "$1" | py "import json; d=json.load(sys.stdin); print($2)" 2>/dev/null | strip_cr; }
peers() { field "$1" "' '.join(sorted(p['HostName'] for p in (d.get('Peer') or {}).values()))"; }
peerkeys() { field "$1" "' '.join(sorted(p['PublicKey'][:16] for p in (d.get('Peer') or {}).values()))"; }
selfip() { field "$1" "d['Self']['TailscaleIPs'][0]"; }
selfkey() { field "$1" "d['Self']['PublicKey'][:16]"; }
relayof() { field "$1" "d['Self'].get('Relay','')"; }
wait_peers() { local i; for i in $(seq 1 "$3"); do [ "$(peers "$1")" = "$2" ] && return 0; sleep 1; done; return 1; }
ping_via() { TS "$1" "ping -c 5 --timeout 5s $2" 2>&1 | tee -a "$ART_DIR/logs/ping.log" | tail -1; }
ping_ok() { TS "$1" "ping -c 3 --timeout 5s $2" >>"$ART_DIR/logs/ping.log" 2>&1; }

# ------------------------------------------------------------------ relay ---
relay() { vm "$RELAY" "$@"; }

# --------------------------------------------------------------- cleanup ----
CLEAN_OK=1
cleanup() {
  [ "$KEEP" = 1 ] && { log "KEEP=1: leaving everything running"; return; }
  log "cleanup"
  local n ip
  binsrv_stop || { log "n1-binsrv not stopped"; CLEAN_OK=0; }
  for ip in $A1 $A2 $B1 $B2; do
    vm "$ip" "sudo journalctl -u $TSU --no-pager" 2>/dev/null | redact >"$ART_DIR/logs/ts-$ip.log" || true
    vm "$ip" "sudo nft delete table inet n1test 2>/dev/null; true" >/dev/null 2>&1
    ts_stop "$ip" >/dev/null 2>&1 || { log "$ip tailscaled not stopped"; CLEAN_OK=0; }
  done
  vps "rm -f /tmp/n1s13-$RUN_ID-*" >/dev/null 2>&1 || true
  cp_ "sudo systemctl stop mesh-bootstrap-e2e 2>/dev/null; sudo systemctl reset-failed mesh-bootstrap-e2e 2>/dev/null; sudo rm -f /usr/local/bin/mesh-bootstrap; true" >/dev/null 2>&1
  ssh_ $CI_SSH "rm -f /tmp/n1-s13b-kb" >/dev/null 2>&1 || true
  relay "sudo journalctl -u n1-derp-c1 -u n1-derp-c2 --no-pager" 2>/dev/null | redact >"$ART_DIR/logs/derper.log" || true
  relay "for u in n1-derp-c1 n1-derp-c2; do sudo systemctl stop \$u 2>/dev/null; sudo systemctl reset-failed \$u 2>/dev/null; done; \
    sudo rm -rf /run/n1-derp; ! sudo test -e /run/n1-derp" || { log "relay not cleaned"; CLEAN_OK=0; }
  for n in cp c1b c2 c2b; do
    on $n "sudo systemctl kill -s CONT $UNIT 2>/dev/null; sudo systemctl stop $UNIT 2>/dev/null; sudo systemctl reset-failed $UNIT 2>/dev/null; \
      sudo rm -f $CFG $DERPMAP $VSECRET $HS; ! sudo test -e $VSECRET && ! test -e $HS" || { log "$n: run files not removed"; CLEAN_OK=0; }
  done
  cp_ "sudo systemctl stop mesh-c1-ha 2>/dev/null; sudo systemctl reset-failed mesh-c1-ha 2>/dev/null; sudo rm -f /etc/haproxy/mesh-c1-n1.cfg; true"
  on c1b "sudo rm -f /etc/headscale/cluster/pg.pass /var/lib/headscale/cluster/noise_private.key; \
      ! sudo test -e /var/lib/headscale/cluster/noise_private.key" || { log "c1 key not removed on mc-c1b"; CLEAN_OK=0; }
  cp_ "sudo rm -f /etc/headscale/cluster/pg.pass; ! sudo test -e /etc/headscale/cluster/pg.pass && sudo systemctl start headscale-tailnets" \
    || { log "mc-cp POC not restarted"; CLEAN_OK=0; }
  for n in c2 c2b; do on $n "sudo systemctl start headscale-cluster" || { log "$n: long-lived c2 not started"; CLEAN_OK=0; }; done
  [[ $PG_DB =~ ^e2e_[0-9a-z]+$ ]] && pg_ "sudo docker exec mc-pg dropdb -U postgres --if-exists --force $PG_DB && \
    ! sudo docker exec mc-pg psql -At -U postgres -c \"SELECT 1 FROM pg_database WHERE datname = '$PG_DB'\" | grep -q 1" \
    && log "dropped database $PG_DB" || { log "database $PG_DB not dropped"; CLEAN_OK=0; }
  local i p=000 q=000
  for i in $(seq 1 40); do
    p=$(curl -s -o /dev/null -w '%{http_code}' -m 5 "$POC_HEALTH" || true); q=$(public_ready c2)
    [ "$p$q" = 200200 ] && break; sleep 1
  done
  [ "$p$q" = 200200 ] && log "POC and long-lived c2 back (200/200)" || { log "POC $p, c2 $q"; CLEAN_OK=0; }
  rm -rf "$TMPD"
}
trap 'cleanup; exit 2' INT TERM

# ------------------------------------------------------------------ setup ---
setup() {
  local pass i n
  pass=$(grep -E '^MC_DB_APP_PASSWORD=' "$MC_DB_ENV" | head -1 | cut -d= -f2-); pass=${pass%"$CR"}
  [ -n "$pass" ] || return 1
  printf '%s' "$pass" >"$TMPD/pg.pass"; unset pass; chmod 600 "$TMPD/pg.pass"
  # Per-run verify secrets (ADR 0009), one per cluster; never printed.
  for c in c1 c2; do head -c 48 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 48 >"$TMPD/vsecret-$c"; chmod 600 "$TMPD/vsecret-$c"; done
  [ "$(wc -c <"$TMPD/vsecret-c1")" -ge 40 ] || return 1

  log "database $PG_DB"
  pg_ "sudo docker exec mc-pg createdb -U postgres -O headscale $PG_DB" || return 1

  if [ -n "$CI_BINDIR" ]; then
    binsums || return 1
    # The VPS (S13, S13b) is outside the LAN: n1probe comes here once.
    scp -q "${SSHO[@]}" "$CI_SSH:$CI_BINDIR/n1probe" "$TMPD/n1probe" || return 1
    echo "$(binsha n1probe)  $TMPD/n1probe" | sha256sum -c --quiet || { log "n1probe sha256 mismatch"; return 1; }
    PROBE_BIN=$TMPD/n1probe
  fi
  binsrv_start || return 1
  log "binary $HS on the four cluster hosts${CI_BINDIR:+ (fetched from mc-ci $CI_BINDIR over the LAN)}"
  for n in cp c1b c2 c2b; do
    put $n headscale /tmp/n1-hs || return 1
    on $n "sudo install -m 0755 /tmp/n1-hs $HS && rm -f /tmp/n1-hs" || return 1
  done

  log "DERP maps: c1 -> region 901 (tcp/443), c2 -> region 902 (tcp/444); relay $RELAY"
  for c in c1 c2; do
    local rid=901 port=443; [ $c = c2 ] && { rid=902; port=444; }
    cat >"$TMPD/derpmap-$c.yaml" <<EOF
regions:
  $rid:
    regionid: $rid
    regioncode: n1$c
    regionname: N1 relay $c
    nodes:
      - name: ${rid}a
        regionid: $rid
        hostname: relay-$c.cp.gta5web.online
        ipv4: $RELAY
        derpport: $port
        stunport: 3478
EOF
  done

  log "c1 config (e2e database, own relay, incremental=$MODE)"
  awk -v host="${PG_LISTEN%:*}" -v port="${PG_LISTEN##*:}" -v db="$PG_DB" '
    /^database:/ { print "database:\n  type: postgres\n  postgres:\n    host: " host "\n    port: " port \
      "\n    name: " db "\n    user: headscale\n    pass: file:/etc/headscale/cluster/pg.pass\n    ssl: disable"; skip = 1; next }
    skip && /^[^ ]/ { skip = 0 }
    !skip { print }' "$REPO/infra/lab/mc-cp/cluster/config.yaml" >"$TMPD/c1.yaml"
  local inc=0; [ "$MODE" = on ] && inc=1
  sed "s#^listen_addr: .*#listen_addr: 192.168.1.241:8443#" "$TMPD/c1.yaml" >"$TMPD/c1-cp.yaml"
  sed "s#^listen_addr: .*#listen_addr: 0.0.0.0:8443#" "$TMPD/c1.yaml" >"$TMPD/c1-b.yaml"

  log "mc-cp: stop POC, pg.pass, config, verify secret, DERP map"
  cp_ "sudo systemctl stop headscale-tailnets headscale-cluster mesh-bootstrap-e2e 2>/dev/null; \
       sudo install -d -o root -g headscale -m 0750 /etc/headscale/cluster && \
       sudo install -o root -g headscale -m 0640 /dev/stdin /etc/headscale/cluster/pg.pass" <"$TMPD/pg.pass" || return 1
  cp_ "sudo test -s /var/lib/headscale/cluster/noise_private.key" || return 1

  log "mc-c1b: TLS, Noise key of c1, pg.pass (pipes only)"
  cp_ 'sudo cat /etc/headscale/tls/fullchain.pem' | on c1b 'sudo install -o root -g headscale -m 0644 /dev/stdin /etc/headscale/tls/fullchain.pem' || return 1
  cp_ 'sudo cat /etc/headscale/tls/privkey.pem' | on c1b 'sudo install -o root -g headscale -m 0640 /dev/stdin /etc/headscale/tls/privkey.pem' || return 1
  cp_ 'sudo cat /var/lib/headscale/cluster/noise_private.key' | on c1b 'sudo install -o headscale -g headscale -m 0600 /dev/stdin /var/lib/headscale/cluster/noise_private.key' || return 1
  on c1b "sudo install -o root -g headscale -m 0640 /dev/stdin /etc/headscale/cluster/pg.pass" <"$TMPD/pg.pass" || return 1

  for n in cp c1b c2 c2b; do
    local c; c=$(clusterof $n)
    on $n "sudo install -o root -g headscale -m 0640 /dev/stdin $VSECRET" <"$TMPD/vsecret-$c" || return 1
    on $n "sudo install -o root -g headscale -m 0644 /dev/stdin $DERPMAP" <"$TMPD/derpmap-$c.yaml" || return 1
    on $n "cat >/tmp/cfg-n1.py" <"$REPO/infra/lab/e2e/n1/cfg-n1.py" || return 1
  done
  cp_ "cat >/tmp/c1-src.yaml" <"$TMPD/c1-cp.yaml" || return 1
  on c1b "cat >/tmp/c1-src.yaml" <"$TMPD/c1-b.yaml" || return 1
  for n in cp c1b; do
    on $n "set -e; sudo python3 /tmp/cfg-n1.py /tmp/c1-src.yaml /tmp/c1-n1.yaml $DERPMAP $VSECRET $inc
      sudo install -o root -g headscale -m 0640 /tmp/c1-n1.yaml $CFG; sudo rm -f /tmp/c1-src.yaml /tmp/c1-n1.yaml /tmp/cfg-n1.py" || return 1
  done
  log "mc-c2, mc-c2b: stop long-lived c2; run config = theirs on $PG_DB with the relay"
  for n in c2 c2b; do
    on $n "set -e; sudo systemctl stop headscale-cluster
      sudo python3 /tmp/cfg-n1.py /etc/headscale/cluster/config.yaml /tmp/c2-n1.yaml $DERPMAP $VSECRET $inc $PG_DB
      sudo install -o root -g headscale -m 0640 /tmp/c2-n1.yaml $CFG; sudo rm -f /tmp/c2-n1.yaml /tmp/cfg-n1.py
      sudo grep -q '^    name: $PG_DB\$' $CFG" || return 1
  done

  log "HAProxy for c1 on mc-cp"
  cp_ "sudo tee /etc/haproxy/mesh-c1-n1.cfg >/dev/null && sudo haproxy -c -q -f /etc/haproxy/mesh-c1-n1.cfg" <<EOF || return 1
global
  maxconn 4096
defaults
  mode tcp
  timeout connect 3s
  timeout client 1h
  timeout server 1h
  timeout tunnel 1h
frontend c1
  bind 10.99.77.2:8443
  default_backend c1
backend c1
  option httpchk
  http-check send meth GET uri /ready ver HTTP/1.1 hdr Host $HOST1
  http-check expect status 200
  default-server inter 500ms fall 2 rise 2 check check-ssl check-sni $HOST1 verify required ca-file /etc/ssl/certs/ca-certificates.crt on-marked-down shutdown-sessions
  server mc-cp 192.168.1.241:8443
  server mc-c1b 192.168.1.245:8443
EOF
  cp_ "sudo systemctl reset-failed mesh-c1-ha 2>/dev/null; sudo systemd-run --quiet --unit=mesh-c1-ha /usr/sbin/haproxy -Ws -f /etc/haproxy/mesh-c1-n1.cfg" || return 1

  log "start processes"
  start_proc cp && wait_direct_ready cp 40 || { log "c1 on mc-cp not ready"; return 1; }
  start_proc c2 && wait_direct_ready c2 40 || { log "c2 on mc-c2 not ready"; return 1; }
  start_proc c1b && start_proc c2b || return 1
  wait_public_ready c1 30 && wait_public_ready c2 30 || { log "public /ready c1=$(public_ready c1) c2=$(public_ready c2)"; return 1; }

  log "registry: register clusters; tailnets a, b on c1, c on c2"
  cli c1 "register --class shared --max-devices 5000 >/dev/null" || return 1
  cli c2 "register --class shared --max-devices 5000 >/dev/null" || return 1
  local s pf=/var/lib/headscale/cluster/n1-policy.hujson
  for s in a b; do
    cli c1 "tailnet create --slug $s" | py 'import json; print(json.load(sys.stdin)["id"])' | strip_cr >"$TMPD/id-$s" || return 1
    cli c1 "tailnet status --slug $s --set ACTIVE" >/dev/null || return 1
  done
  cli c2 "tailnet create --slug c" | py 'import json; print(json.load(sys.stdin)["id"])' | strip_cr >"$TMPD/id-c" || return 1
  cli c2 "tailnet status --slug c --set ACTIVE" >/dev/null || return 1
  [ -s "$TMPD/id-a" ] && [ -s "$TMPD/id-b" ] || return 1
  # A: grants form; B and C: legacy acls (both policy forms in N1).
  set_policy a '{"grants":[{"src":["*"],"dst":["*"],"ip":["*"]}]}' || return 1
  set_policy b '{"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}' || return 1
  set_policy c '{"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}' || return 1
  for s in a b c; do
    local o=c1; [ $s = c ] && o=c2
    cli $o "tailnet authkey --slug $s --user n1-$s --reusable --expiration 180m" \
      | py 'import json; print(json.load(sys.stdin)["key"])' | strip_cr >"$TMPD/key-$s" || return 1
    chmod 600 "$TMPD/key-$s"; [ -s "$TMPD/key-$s" ] || return 1
  done

  log "relay: wildcard cert from mc-cp, two derper units (verify = cluster /verify, fail closed)"
  relay "sudo install -d -m 0700 /run/n1-derp /run/n1-derp/c1 /run/n1-derp/c2" || return 1
  for c in c1 c2; do
    cp_ 'sudo cat /etc/headscale/tls/fullchain.pem' | relay "sudo install -m 0644 /dev/stdin /run/n1-derp/$c/relay-$c.cp.gta5web.online.crt" || return 1
    cp_ 'sudo cat /etc/headscale/tls/privkey.pem' | relay "sudo install -m 0600 /dev/stdin /run/n1-derp/$c/relay-$c.cp.gta5web.online.key" || return 1
    relay "sudo install -m 0600 /dev/stdin /run/n1-derp/$c.secret" <"$TMPD/vsecret-$c" || return 1
  done
  relay "sudo systemctl reset-failed n1-derp-c1 n1-derp-c2 2>/dev/null
    sudo systemd-run --quiet --unit=n1-derp-c1 /bin/bash -c 'exec /usr/local/bin/derper -a :443 -http-port -1 -hostname relay-c1.cp.gta5web.online \
      -certmode manual -certdir /run/n1-derp/c1 -stun -stun-port 3478 -verify-client-url-fail-open=false \
      -verify-client-url \"$URL1/verify?secret=\$(cat /run/n1-derp/c1.secret)\"'
    sudo systemd-run --quiet --unit=n1-derp-c2 /bin/bash -c 'exec /usr/local/bin/derper -a :444 -http-port -1 -hostname relay-c2.cp.gta5web.online \
      -certmode manual -certdir /run/n1-derp/c2 -stun=false -verify-client-url-fail-open=false \
      -verify-client-url \"$URL2/verify?secret=\$(cat /run/n1-derp/c2.secret)\"'
    sleep 2; systemctl is-active n1-derp-c1 n1-derp-c2" || return 1

  log "probe binary to mc-ci and the client VMs"
  if [ -n "$CI_BINDIR" ]; then ssh_ "$CI_SSH" "cp $CI_BINDIR/n1probe /tmp/n1probe" || return 1
  else scp -q "${SSHO[@]}" "$PROBE_BIN" "$CI_SSH:/tmp/n1probe" || return 1; fi
  ssh_ "$CI_SSH" "echo '$(binsha n1probe)  /tmp/n1probe' | sha256sum -c --quiet" || { log "mc-ci: n1probe sha256 mismatch"; return 1; }
  binsrv_stop || { log "n1-binsrv not stopped"; return 1; }
  for ip in $A1 $B1; do
    ssh_ $CI_SSH "cat /tmp/n1probe" | vm "$ip" "cat >/tmp/n1probe && chmod 0755 /tmp/n1probe" || return 1
  done

  log "clients: tailscaled $V on a1 a2 b1 b2"
  for ip in $A1 $A2 $B1 $B2; do ts_start "$ip" || { log "tailscaled on $ip"; return 1; }; done
}

set_policy() { # SLUG JSON
  local o=c1 pf=/var/lib/headscale/cluster/n1-policy.hujson
  [ "$1" = c ] && o=c2
  if [ $o = c1 ]; then printf '%s\n' "$2" | cp_ "sudo install -o headscale -g headscale -m 0640 /dev/stdin $pf && $CLI1 tailnet policy --slug $1 --file $pf; rc=\$?; sudo rm -f $pf; exit \$rc"
  else printf '%s\n' "$2" | on c2 "sudo install -o headscale -g headscale -m 0640 /dev/stdin $pf && $CLI2 tailnet policy --slug $1 --file $pf; rc=\$?; sudo rm -f $pf; exit \$rc"; fi > >(redact >>"$ART_DIR/logs/policy.log") 2>&1
}

# ------------------------------------------------------------------- cases ---
t_e00() {
  local p="" c r
  # verify: the right secret answers (Allow false for a random key), a wrong one 403, from the relay's path (public URL).
  c=$(curl -s -o /dev/null -w '%{http_code}' -m 8 -X POST -H 'Content-Type: application/json' \
      --data '{"NodePublic":"nodekey:0000000000000000000000000000000000000000000000000000000000000000"}' "$URL1/verify?secret=wrong-secret-wrong-secret-wrong-secret")
  [ "$c" = 403 ] || p="$p; c1 /verify with a wrong secret: $c"
  r=$(py 'import sys; print(open(sys.argv[1]).read())' "$TMPD/vsecret-c1" | strip_cr | curl -s -m 8 -X POST -H 'Content-Type: application/json' \
      --data '{"NodePublic":"nodekey:0000000000000000000000000000000000000000000000000000000000000000"}' --url-query "secret@-" "$URL1/verify" 2>/dev/null)
  case "$r" in *'"Allow":false'*) ;; *) p="$p; c1 /verify with its secret: [$(redact <<<"$r" | head -c 80)]";; esac
  relay "systemctl is-active -q n1-derp-c1 && systemctl is-active -q n1-derp-c2" || p="$p; derper units not active"
  [ "$(vm $A1 "timeout 4 bash -c '</dev/tcp/$B1/22' && echo open || echo blocked" 2>/dev/null)" = blocked ] || p="$p; underlay A->B open"
  [ -z "$p" ] && record E00 PASS "relay 901/902 up; /verify: wrong secret 403, right secret answers; underlay A->B blocked" || record E00 FAIL "${p#; }"
}

t_s01() {
  local p="" r
  r=0; ts_up $A1 "$URL1" "$TMPD/key-a" n1-a1 || r=$?; [ $r = 0 ] || p="$p; a1 up rc=$r"
  r=0; ts_up $A2 "$URL1" "$TMPD/key-a" n1-a2 || r=$?; [ $r = 0 ] || p="$p; a2 up rc=$r"
  r=0; ts_up $B1 "$URL1" "$TMPD/key-b" n1-b1 || r=$?; [ $r = 0 ] || p="$p; b1 up rc=$r"
  r=0; ts_up $B2 "$URL1" "$TMPD/key-b" n1-b2 || r=$?; [ $r = 0 ] || p="$p; b2 up rc=$r"
  wait_peers $A1 n1-a2 40 || p="$p; a1 peers [$(peers $A1)]"
  wait_peers $B1 n1-b2 40 || p="$p; b1 peers [$(peers $B1)]"
  [ "$(peers $A2)" = n1-a1 ] || p="$p; a2 peers [$(peers $A2)]"
  [ "$(peers $B2)" = n1-b1 ] || p="$p; b2 peers [$(peers $B2)]"
  ping_ok $A1 n1-a2 || p="$p; a1 cannot ping a2"
  ping_ok $B1 n1-b2 || p="$p; b1 cannot ping b2"
  [ -z "$p" ] && record S01 PASS "a (grants) a1<->a2 and b (acls) b1<->b2 on one c1 process: peers and ping ($V, netmap $MODE)" || record S01 FAIL "${p#; }"
}

t_s02() {
  local p="" ka kb ia ib
  ka="$(selfkey $A1) $(selfkey $A2)"; kb="$(selfkey $B1) $(selfkey $B2)"
  ia="$(selfip $A1) $(selfip $A2)"; ib="$(selfip $B1) $(selfip $B2)"
  for k in $kb; do grep -q "$k" <<<"$(ts_json $A1)$(ts_json $A2)" && p="$p; B key ${k:8:8} visible in A"; done
  for k in $ka; do grep -q "$k" <<<"$(ts_json $B1)$(ts_json $B2)" && p="$p; A key ${k:8:8} visible in B"; done
  grep -qE 'n1-b[12]' <<<"$(ts_json $A1)" && p="$p; B hostname in a1's view"
  grep -qE 'n1-a[12]' <<<"$(ts_json $B1)" && p="$p; A hostname in b1's view"
  # Per-tailnet IPAM: A and B reuse the same addresses (S06), so "b1's IP"
  # seen from a1 is a node of A. What must never happen: an answer from a B
  # node, or B's names resolving in A.
  local pa; pa=$(TS $A1 "ping -c 1 --timeout 3s $(selfip $B1)" 2>&1); printf 'a1 ping %s: %s\n' "$(selfip $B1)" "$pa" >>"$ART_DIR/logs/S02.txt"
  grep -qE 'pong from n1-b' <<<"$pa" && p="$p; a B node answered a1"
  TS $A1 "ping -c 1 --timeout 3s n1-b1" >/dev/null 2>&1 && p="$p; a1 reaches n1-b1 by name"
  printf 'A ips %s; B ips %s\n' "$ia" "$ib" >>"$ART_DIR/logs/S02.txt"
  [ -z "$p" ] && record S02 PASS "no node key or hostname of B in A's view and vice versa; B unreachable from A by name or IP (per-tailnet IPs overlap: A [$ia] B [$ib])" || record S02 FAIL "${p#; }"
}

t_s03() {
  local p=""
  set_policy a '{"grants":[{"src":["*"],"dst":["*"],"ip":["*"]}],"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}' || p="$p; policy a"
  set_policy b '{"grants":[]}' || p="$p; policy b"
  local i; for i in $(seq 1 30); do [ -z "$(peers $B1)" ] && break; sleep 1; done
  [ -z "$(peers $B1)" ] || p="$p; b1 still sees [$(peers $B1)] under grants []"
  TS $B1 "ping -c 1 --timeout 3s n1-b2" >/dev/null 2>&1 && p="$p; b1 still pings b2"
  [ "$(peers $A1)" = n1-a2 ] || p="$p; a1 under * sees [$(peers $A1)]"
  grep -qE 'n1-b[12]' <<<"$(ts_json $A1)" && p="$p; A's * reaches B"
  set_policy b '{"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}' || p="$p; restore b"
  wait_peers $B1 n1-b2 40 || p="$p; b1 not back [$(peers $B1)]"
  [ -z "$p" ] && record S03 PASS "A with * (grants+acls) stays in A; B grants [] -> no peers, no ping; B restored" || record S03 FAIL "${p#; }"
}

t_s11() {
  local p="" out rc=0
  ssh_ $CI_SSH "umask 077; cat >/tmp/n1-ka" <"$TMPD/key-a" && ssh_ $CI_SSH "umask 077; cat >/tmp/n1-kb" <"$TMPD/key-b" || { record S11 FAIL "keys"; return; }
  out=$(ssh_ $CI_SSH "timeout 300 /tmp/n1probe switch-tenant --url $URL1 --key-a /tmp/n1-ka --key-b /tmp/n1-kb; echo rc=\$?; rm -f /tmp/n1-ka /tmp/n1-kb" 2>&1)
  # A dropped SSH session leaves the probe running and registering (run at
  # 3ad5743): stop it before its nodes are deleted.
  ssh_ $CI_SSH "pkill -f 'n1probe switch-tenant'; rm -f /tmp/n1-ka /tmp/n1-kb; true" >/dev/null 2>&1
  printf '%s\n' "$out" | redact >"$ART_DIR/logs/S11-switch-tenant.jsonl"
  rc=$(grep -o 'rc=[0-9]*' <<<"$out" | tail -1 | cut -d= -f2)
  local n; n=$(grep -c '"pass":true' <<<"$out")
  [ "$rc" = 0 ] || p="rc=$rc, $(grep -c '"pass":false' <<<"$out") case(s) failed"
  drop_nodes c1 a n1probe- && drop_nodes c1 b n1probe- || p="${p:+$p; }probe nodes not deleted"
  for s in a b; do live c1 node list --slug $s 2>/dev/null | grep -q n1probe- && p="${p:+$p; }probe nodes left in $s"; done
  [ -z "$p" ] && record S11 PASS "n1probe switch-tenant against c1 (real cluster, public URL): $n cases refused, no leak (S11 a/b/d)" || record S11 FAIL "$p"
}

t_s15() {
  local p="" ra rb
  ra=$(relayof $A1); rb=$(relayof $B1)
  [ "$ra" = n1c1 ] || p="$p; a1 home DERP [$ra]"
  [ "$rb" = n1c1 ] || p="$p; b1 home DERP [$rb]"
  # Force a1<->a2 through the relay: drop direct UDP between them inside a2.
  vm $A2 "sudo nft -f - <<'NFT'
table inet n1test {
  chain input { type filter hook input priority filter; policy accept; ip saddr $A1 udp dport 41641 drop; }
  chain output { type filter hook output priority filter; policy accept; ip daddr $A1 udp dport 41641 drop; }
}
NFT" || p="$p; nft on a2"
  TS $A1 "debug restun" >/dev/null 2>&1 || true
  sleep 3
  local pv; pv=$(ping_via $A1 n1-a2)
  grep -q 'via DERP(n1c1)' "$ART_DIR/logs/ping.log" || p="$p; a1->a2 not relayed via n1c1 [$pv]"
  vm $A2 "sudo nft delete table inet n1test" >/dev/null 2>&1
  # Unknown node keys are refused by the relay (n1probe derp-check), when the probe has it.
  if ssh_ $CI_SSH "/tmp/n1probe derp-check 2>&1 | grep -qi -e usage -e host"; then
    local out; out=$(vm $A1 "/tmp/n1probe derp-check --host relay-c1.cp.gta5web.online --ip $RELAY --port 443 --count 3; echo rc=\$?" 2>&1)
    printf '%s\n' "$out" >"$ART_DIR/logs/S15-derp-check.jsonl"
    grep -q 'rc=0' <<<"$out" || p="$p; unknown key admitted by relay 901 [$(tail -2 <<<"$out" | tr '\n' ' ')]"
  else
    log "S15: n1probe has no derp-check yet; unknown-key case not run"
    p="$p; derp-check not available (unknown-key case not run)"
  fi
  [ -z "$p" ] && record S15 PASS "clients home on own relay region n1c1; a1->a2 relayed via DERP(n1c1) with verify; unknown key refused by the relay" || record S15 FAIL "${p#; }"
}

# Batch 2 and 3 cases.
# shellcheck source=cases2.sh
. "$(dirname "$0")/cases2.sh"
# shellcheck source=cases3.sh
. "$(dirname "$0")/cases3.sh"
# shellcheck source=cases4.sh
. "$(dirname "$0")/cases4.sh"

# --------------------------------------------------------------------- main ---
T0=$(date +%s)
log "N1 E2E $RUN_ID at $SHA: MODE=$MODE V=$V (artifacts: ${ART_DIR#"$REPO"/})"
if ! setup 2> >(redact >>"$ART_DIR/logs/setup.log"); then
  log "setup failed (see logs/setup.log)"; cleanup; exit 2
fi
{ echo "client: $V"; echo "incremental_netmap: $MODE"; echo "commit: $SHA"; echo "database: $PG_DB"
  if [ -n "$CI_BINDIR" ]; then echo "binaries: built on mc-ci in $CI_BINDIR, fetched over the LAN"; else echo "binaries: copied from the laptop"; fi
  echo "cluster binary: $(binsha headscale | cut -c1-16)"; echo "n1probe: $(binsha n1probe | cut -c1-16)"; } >>"$ART_DIR/env.txt"
t_e00
t_s01
t_s02
t_s03
t_s11
t_s15
t_s07
t_s08
t_s08b
t_s10
t_s12
t_s14
t_m06
t_s05
t_s06
t_s09
t_s04
t_s17 c1 "${F02_ROUNDS:-3}"
t_s17 c2 "${F02_ROUNDS:-3}"
t_s16 S16 c1
t_s18
t_s13
t_s13b
for n in cp c1b c2 c2b; do on $n "sudo journalctl -u $UNIT --since -$(( $(date +%s) - T0 + 5 ))s --no-pager" 2>/dev/null | redact >"$ART_DIR/logs/$n-process.log"; done
hits=$(grep -rlE 'hskey-(auth|api)-[A-Za-z0-9]{6,}|privkey:[0-9a-f]{64}|nodekey:[0-9a-f]{64}|mkey:[0-9a-f]{64}|BEGIN [A-Z ]*PRIVATE KEY|secret=[A-Za-z0-9]{20,}' "$ART_DIR" 2>/dev/null | grep -v results.tsv)
[ -z "$hits" ] && record S99 PASS "no auth key, private key, full key or verify secret in the artifacts" || record S99 FAIL "unredacted in: $hits"
cleanup
log "done in $(( $(date +%s) - T0 ))s: $FAILS FAIL, cleanup_ok=$CLEAN_OK"
[ "$CLEAN_OK" = 1 ] || exit 2
[ "$FAILS" = 0 ] || exit 1
exit 0
