#!/bin/bash
# Shaping at wiring: what each end declares it sends is delayed, lost or rate-limited on the way to
# its peer, per direction, on one node and across nodes; a change applies live, a recreated pod gets
# it back, and an invalid one is reported in the plumbing error without unwiring the link. r1 and r2
# share worker A, r3 sits on worker B. Usage: tests/shaping.sh [worker-a] [worker-b]; KUBECTL_CONTEXT
# picks the cluster. The uids are the VXLAN VNIs, unique per node.
A=${1:-meshnet-worker}; B=${2:-meshnet-worker2}
K="kubectl ${KUBECTL_CONTEXT:+--context $KUBECTL_CONTEXT}"
NS=shaping
fails=0

pod() { # name node
cat <<YAML
apiVersion: v1
kind: Pod
metadata: {name: $1, namespace: $NS, labels: {test: shaping}}
spec:
  nodeName: $2
  terminationGracePeriodSeconds: 0
  containers:
  - name: pod
    image: alpine:3.24.2
    command: ["/bin/sh", "-c", "sleep infinity"]
YAML
}

topologies() {
cat <<YAML
apiVersion: networkop.co.uk/v1beta1
kind: Topology
metadata: {name: r1, namespace: $NS}
spec:
  links:
  - {uid: 4601, peer_pod: r2, local_intf: eth1, peer_intf: eth1, local_ip: 10.46.1.1/24}
  - {uid: 4602, peer_pod: r3, local_intf: eth2, peer_intf: eth1, local_ip: 10.46.2.1/24}
---
apiVersion: networkop.co.uk/v1beta1
kind: Topology
metadata: {name: r2, namespace: $NS}
spec:
  links:
  - {uid: 4601, peer_pod: r1, local_intf: eth1, peer_intf: eth1, local_ip: 10.46.1.2/24}
---
apiVersion: networkop.co.uk/v1beta1
kind: Topology
metadata: {name: r3, namespace: $NS}
spec:
  links:
  - {uid: 4602, peer_pod: r1, local_intf: eth1, peer_intf: eth2, local_ip: 10.46.2.3/24}
YAML
}

check() { # description command...
  local desc=$1; shift
  local t0=$(date +%s)
  for i in $(seq 1 30); do
    if "$@" >/dev/null 2>&1; then echo "[OK ] $desc ($(( $(date +%s) - t0 ))s)"; return; fi
    sleep 1
  done
  fails=$((fails + 1)); echo "[FAIL] $desc ($(( $(date +%s) - t0 ))s): rtt $(rtt "${last[@]}")"
}
# rtt prints the average round trip in ms of three pings from pod $1 to $2, with ping's further args.
rtt() { $K -n $NS exec $1 -- ping -c 3 -W 2 "${@:3}" $2 2>/dev/null | sed -n 's|.*= [0-9.]*/\([0-9.]*\)/.*|\1|p'; }
# rtt_in passes when that average sits between $3 and $4 ms.
rtt_in() { last=("$1" "$2" "${@:5}"); local r; r=$(rtt "$1" "$2" "${@:5}"); [ -n "$r" ] && awk -v r="$r" -v lo="$3" -v hi="$4" 'BEGIN { exit !(r >= lo && r <= hi) }'; }
silent() { ! $K -n $NS exec $1 -- ping -c 3 -W 1 $2 >/dev/null 2>&1; }
shape() { # topology link-index json-shaping, or null to drop it
  if [ "$3" = null ]; then
    $K -n $NS patch topology $1 --type=json -p "[{\"op\":\"remove\",\"path\":\"/spec/links/$2/shaping\"}]" >/dev/null
  else
    $K -n $NS patch topology $1 --type=json -p "[{\"op\":\"add\",\"path\":\"/spec/links/$2/shaping\",\"value\":$3}]" >/dev/null
  fi
}
plumbing() { $K -n $NS get topology $1 -o jsonpath='{.status.plumbing_error}' | grep -q "$2"; }
start() {
  if ! { pod $1 $2 | $K apply -f - >/dev/null && $K -n $NS wait --timeout=90s --for condition=Ready pod/$1 >/dev/null; }; then
    fails=$((fails + 1)); echo "[FAIL] $1 did not start on $2"
  fi
}

$K delete ns $NS --ignore-not-found --wait=true >/dev/null 2>&1
$K create ns $NS >/dev/null
topologies | $K apply -f - >/dev/null
start r1 $A; start r2 $A; start r3 $B

check "unshaped: r1 to r2 under 10 ms" rtt_in r1 10.46.1.2 0 10
shape r1 0 '{"delay": "20ms"}'
shape r2 0 '{"delay": "30ms"}'
check "per direction on one node: 20 ms out, 30 ms back, about 50 ms" rtt_in r1 10.46.1.2 48 70
shape r1 0 '{"delay": "5ms"}'
check "changed live: 5 ms out, 30 ms back, about 35 ms" rtt_in r1 10.46.1.2 33 50

shape r3 0 '{"loss": "100%"}'
check "across workers: what r3 sends is all lost" silent r1 10.46.2.3
shape r3 0 '{"rate": "1mbit"}'
check "across workers: 1400 bytes back at 1 Mbit/s take 11 ms more" rtt_in r1 10.46.2.3 10 40 -s 1400
shape r3 0 null
shape r1 1 '{"qdisc": "tbf", "rate": "1mbit"}'
check "a tbf of 1 Mbit/s on what r1 sends: 8000 bytes past its bucket wait 30 ms" rtt_in r1 10.46.2.3 25 80 -s 8000
shape r1 1 null
check "shaping dropped across workers: under 10 ms" rtt_in r1 10.46.2.3 0 10 -s 1400

$K -n $NS delete pod r2 --wait=true >/dev/null 2>&1
start r2 $A
check "r2 recreated gets its node end shaped again, about 35 ms" rtt_in r1 10.46.1.2 33 50

shape r2 0 '{"jitter": "5ms"}'
check "an invalid shaping is r1's plumbing error" plumbing r1 "a jitter needs a delay"
check "and the link still carries, with the last valid shaping, about 35 ms" rtt_in r1 10.46.1.2 33 50
shape r2 0 null
shape r1 0 null
check "shaping dropped on one node: under 10 ms, the error gone" eval 'rtt_in r1 10.46.1.2 0 10 && ! plumbing r1 jitter'

$K delete ns $NS --wait=true >/dev/null 2>&1
echo "shaping test: $fails failed"
exit $(( fails > 0 ))
