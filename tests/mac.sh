#!/bin/bash
# A pod made again keeps its MACs: h reaches r through b, a pod that bridges its two links, so h's
# end never loses carrier and h keeps r's MAC in its neighbour table while r is away. r is made again
# on its worker, on the other and back; each time its eth1 must have the MAC it was born with, which
# no link declares, and h's ping must get through within a few seconds of r being Ready, where a new
# MAC would leave h sending to the old one until its neighbour entry expired, 20 to 35 s. Usage:
# tests/mac.sh [worker-a] [worker-b]; KUBECTL_CONTEXT picks the cluster.
A=${1:-meshnet-worker}; B=${2:-meshnet-worker2}
K="kubectl ${KUBECTL_CONTEXT:+--context $KUBECTL_CONTEXT}"
NS=mac
WITHIN=${WITHIN:-10}
fails=0; checks=0

pod() { # name node command
cat <<YAML
apiVersion: v1
kind: Pod
metadata: {name: $1, namespace: $NS, labels: {test: mac}}
spec:
  nodeName: $2
  terminationGracePeriodSeconds: 0
  containers:
  - name: pod
    image: alpine:3.24.2
    command: ["/bin/sh", "-c", "$3"]
    securityContext: {capabilities: {add: ["NET_ADMIN"]}}
YAML
}

topologies() {
cat <<YAML
apiVersion: networkop.co.uk/v1beta1
kind: Topology
metadata: {name: h, namespace: $NS}
spec:
  links:
  - {uid: 4901, peer_pod: b, local_intf: eth1, peer_intf: eth1, local_ip: 10.47.0.1/24}
---
apiVersion: networkop.co.uk/v1beta1
kind: Topology
metadata: {name: b, namespace: $NS}
spec:
  links:
  - {uid: 4901, peer_pod: h, local_intf: eth1, peer_intf: eth1}
  - {uid: 4902, peer_pod: r, local_intf: eth2, peer_intf: eth1}
---
apiVersion: networkop.co.uk/v1beta1
kind: Topology
metadata: {name: r, namespace: $NS}
spec:
  links:
  - {uid: 4902, peer_pod: b, local_intf: eth1, peer_intf: eth2, local_ip: 10.47.0.2/24}
YAML
}

check() { # name, then a command that succeeds
  local name=$1; shift
  checks=$((checks + 1))
  if "$@" >/dev/null 2>&1; then echo "[OK ] $name"; else fails=$((fails + 1)); echo "[FAIL] $name"; fi
}
mac_of() { $K -n $NS exec r -- cat /sys/class/net/eth1/address 2>/dev/null; }
status_mac() { $K -n $NS get topology r -o jsonpath='{.status.links[0].mac}' 2>/dev/null; }
same() { [ -n "$1" ] && [ "$1" = "$2" ]; }
# The seconds until h's ping to r gets through, or 60.
first_ping() {
  local t0=$(date +%s)
  while [ $(( $(date +%s) - t0 )) -lt 60 ]; do
    $K -n $NS exec h -- ping -c 1 -W 1 10.47.0.2 >/dev/null 2>&1 && break
  done
  echo $(( $(date +%s) - t0 ))
}

remake() { # name node
  $K -n $NS exec h -- ping -c 2 -W 1 10.47.0.2 >/dev/null 2>&1 # h holds r's MAC as it goes
  $K -n $NS delete pod r --wait=true >/dev/null 2>&1
  pod r $2 "sleep infinity" | $K apply -f - >/dev/null
  $K -n $NS wait --timeout=90s --for condition=Ready pod r >/dev/null 2>&1
  local took=$(first_ping) now=$(mac_of)
  check "$1: r eth1 keeps its MAC ($now, born with $born)" same "$now" "$born"
  check "$1: h reaches r within ${WITHIN}s of r being Ready (${took}s)" test "$took" -le "$WITHIN"
}

$K delete ns $NS --ignore-not-found --wait=true >/dev/null 2>&1
$K create ns $NS >/dev/null
topologies | $K apply -f - >/dev/null
pod h $A "sleep infinity" | $K apply -f - >/dev/null
pod b $A "ip link add br0 type bridge && ip link set eth1 master br0 && ip link set eth2 master br0 && ip link set br0 up; sleep infinity" | $K apply -f - >/dev/null
pod r $A "sleep infinity" | $K apply -f - >/dev/null
$K -n $NS wait --timeout=120s --for condition=Ready pod h b r >/dev/null

check "born: h reaches r through the bridge of b" $K -n $NS exec h -- ping -c 3 -W 2 10.47.0.2
born=$(mac_of)
# Unicast and locally administered: the second digit of the first octet is 2, 6, a or e.
check "born: r eth1 has a locally administered unicast MAC ($born)" eval "echo $born | grep -qE '^[0-9a-f][26ae]:'"
check "born: the status of r's link reports that MAC ($(status_mac))" same "$(status_mac)" "$born"
remake "made again on $A" $A
remake "moved to $B" $B
remake "moved back to $A" $A

$K delete ns $NS --wait=true >/dev/null 2>&1
echo "mac test: $((checks - fails)) of $checks checks passed"
exit $(( fails > 0 ))
