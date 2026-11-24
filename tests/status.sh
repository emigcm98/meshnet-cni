#!/bin/bash
# The state of each link in its pod's Topology, the events of its changes and the daemon's metrics:
# a link on one node and one across workers up with their transport, a peer off and back, a port
# shut, a cable unplugged, a link that fails while the others stay wired, a link removed, the
# counters of a link, and the status gone with its pod. r1 and r2 share worker A, r3 sits on worker
# B. Usage: tests/status.sh [worker-a] [worker-b]; KUBECTL_CONTEXT picks the cluster.
A=${1:-meshnet-worker}; B=${2:-meshnet-worker2}
K="kubectl ${KUBECTL_CONTEXT:+--context $KUBECTL_CONTEXT}"
NS=status
fails=0

pod() { # name node
cat <<YAML
apiVersion: v1
kind: Pod
metadata: {name: $1, namespace: $NS, labels: {test: status}}
spec:
  nodeName: $2
  terminationGracePeriodSeconds: 0
  containers:
  - name: pod
    image: alpine:3.24.2
    command: ["/bin/sh", "-c", "sleep infinity"]
    securityContext: {capabilities: {add: ["NET_ADMIN"]}}
YAML
}

topologies() {
cat <<YAML
apiVersion: networkop.co.uk/v1beta1
kind: Topology
metadata: {name: r1, namespace: $NS}
spec:
  links:
  - {uid: 4701, peer_pod: r2, local_intf: eth1, peer_intf: eth1, local_ip: 10.47.1.1/24}
  - {uid: 4702, peer_pod: r3, local_intf: eth2, peer_intf: eth1, local_ip: 10.47.2.1/24}
---
apiVersion: networkop.co.uk/v1beta1
kind: Topology
metadata: {name: r2, namespace: $NS}
spec:
  links:
  - {uid: 4701, peer_pod: r1, local_intf: eth1, peer_intf: eth1, local_ip: 10.47.1.2/24}
---
apiVersion: networkop.co.uk/v1beta1
kind: Topology
metadata: {name: r3, namespace: $NS}
spec:
  links:
  - {uid: 4702, peer_pod: r1, local_intf: eth1, peer_intf: eth2, local_ip: 10.47.2.3/24}
YAML
}

check() { # description command...
  local desc=$1; shift
  local t0=$(date +%s)
  for i in $(seq 1 30); do
    if "$@" >/dev/null 2>&1; then echo "[OK ] $desc ($(( $(date +%s) - t0 ))s)"; return; fi
    sleep 1
  done
  fails=$((fails + 1)); echo "[FAIL] $desc ($(( $(date +%s) - t0 ))s): $($K -n $NS get topology -o custom-columns=NAME:.metadata.name,LINKS:.status.links[*].state,ERROR:.status.plumbing_error --no-headers 2>&1 | tr '\n' ';')"
}
# field prints a field of the status of link uid in the Topology of pod $1.
field() { $K -n $NS get topology $1 -o jsonpath="{.status.links[?(@.uid==$2)].$3}"; }
is() { [ "$(field $1 $2 $3)" = "$4" ]; }
has() { field $1 $2 $3 | grep -q "$4"; }
event() { # pod reason text
  $K -n $NS get events --field-selector "involvedObject.kind=Topology,involvedObject.name=$1,reason=$2" -o jsonpath='{.items[*].message}' | grep -q "$3"
}
metrics() { $K -n meshnet exec "$($K -n meshnet get pod -l name=meshnet --field-selector spec.nodeName=$A -o jsonpath='{.items[0].metadata.name}')" -- wget -qO- localhost:51112/metrics; }
counted() { metrics | grep "^meshnet_link_transmit_bytes_total{namespace=\"$NS\",pod=\"r1\",interface=\"eth1\"" | awk '{exit !($2 > 0)}'; }
patch() { $K -n $NS patch topology $1 --type=json -p "$2" >/dev/null; }
start() {
  if ! { pod $1 $2 | $K apply -f - >/dev/null && $K -n $NS wait --timeout=90s --for condition=Ready pod/$1 >/dev/null; }; then
    fails=$((fails + 1)); echo "[FAIL] $1 did not start on $2"
  fi
}
stop() { $K -n $NS delete pod $1 --wait=true >/dev/null 2>&1; }

$K delete ns $NS --ignore-not-found --wait=true >/dev/null 2>&1
$K create ns $NS >/dev/null
topologies | $K apply -f - >/dev/null
start r1 $A; start r2 $A; start r3 $B

check "r1 eth1 up over veth on $A" eval "is r1 4701 state up && is r1 4701 transport veth && is r1 4701 node $A && has r1 4701 ifindex '[0-9]' && has r1 4701 mac ':'"
check "r1 eth2 up over vxlan, VNI 9702" eval 'is r1 4702 state up && is r1 4702 transport vxlan && is r1 4702 vni 9702'
check "r3 eth1 up over vxlan on $B" eval "is r3 4702 state up && is r3 4702 node $B"
# r3 may start after r1, so eth2 is first wired with its peer off and comes up over vxlan after.
check "events say r1 eth2 was wired and is up over vxlan" eval 'event r1 Wired "eth2 to r3 wired" && { event r1 Wired "eth2 to r3 wired, over vxlan, VNI 9702, up" || event r1 LinkUp "eth2 to r3 up, over vxlan, VNI 9702"; }'

stop r2
check "r2 off: r1 eth1 is off" is r1 4701 state off
check "and an event says why" event r1 LinkDown "the peer is off"
start r2 $A
check "r2 back: r1 eth1 up again, with an event" eval 'is r1 4701 state up && event r1 LinkUp "eth1 to r2 up"'

$K -n $NS exec r2 -- ip link set eth1 down >/dev/null
check "r2 shuts eth1: both ends down at once" eval 'is r1 4701 state down && is r2 4701 state down'
$K -n $NS exec r2 -- ip link set eth1 up >/dev/null
check "r2 opens eth1: both ends up" eval 'is r1 4701 state up && is r2 4701 state up'

patch r3 '[{"op":"add","path":"/spec/links/0/unplugged","value":true}]'
check "unplugged across workers: both ends unplugged" eval 'is r1 4702 state unplugged && is r3 4702 state unplugged'
patch r3 '[{"op":"remove","path":"/spec/links/0/unplugged"}]'
check "plugged back: both ends up" eval 'is r1 4702 state up && is r3 4702 state up'

patch r2 '[{"op":"add","path":"/spec/links/-","value":{"uid":4703,"peer_pod":"r1","local_intf":"eth3","peer_intf":"eth3"}}]'
patch r1 '[{"op":"add","path":"/spec/links/-","value":{"uid":4703,"peer_pod":"r2","local_intf":"eth3","peer_intf":"eth3","local_ip":"not an address"}}]'
check "a link that fails is failed, with its error" eval 'is r1 4703 state failed && has r1 4703 error "invalid CIDR"'
check "the others stay up, the pod's plumbing error names it" eval 'is r1 4701 state up && is r1 4702 state up && $K -n $NS get topology r1 -o jsonpath="{.status.plumbing_error}" | grep -q eth3'
check "a warning event says it failed" event r1 WiringFailed "eth3 to r2"
patch r1 '[{"op":"remove","path":"/spec/links/2"}]'
patch r2 '[{"op":"remove","path":"/spec/links/1"}]'
check "removed: gone from the status, with an event" eval '[ -z "$(field r1 4703 state)" ] && event r1 Unwired "eth3 to r2 unwired"'

$K -n $NS exec r1 -- ping -c 3 -W 1 10.47.1.2 >/dev/null 2>&1
check "the metrics on $A have r1 eth1 up" eval 'metrics | grep -q "^meshnet_link_up{namespace=\"$NS\",pod=\"r1\",interface=\"eth1\",peer_pod=\"r2\",uid=\"4701\"} 1"'
check "and count what r1 sends on it" counted

stop r1
check "r1 gone: its links' status goes with it" eval '[ -z "$($K -n $NS get topology r1 -o jsonpath="{.status.links}")" ]'
check "and r2's link to it is off" is r2 4701 state off

$K delete ns $NS --wait=true >/dev/null 2>&1
echo "status test: $fails failed"
exit $(( fails > 0 ))
