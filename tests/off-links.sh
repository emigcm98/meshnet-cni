#!/bin/bash
# A pod switched off and on: r1 keeps its interfaces, never remade and with their addresses, while its
# neighbours are gone, without carrier, and carries again when they are back, also when one comes back
# on r1's node instead of its own. r1's carrier follows r2 shutting its port. r1 and r2 share worker A,
# r3 sits on worker B. Usage: tests/off-links.sh [worker-a] [worker-b]; KUBECTL_CONTEXT picks the
# cluster. The uids are the VXLAN VNIs, which the kernel wants unique per node.
A=${1:-meshnet-worker}; B=${2:-meshnet-worker2}
K="kubectl ${KUBECTL_CONTEXT:+--context $KUBECTL_CONTEXT}"
# Not "off": YAML reads that as false.
NS=offlinks
fails=0

pod() { # name node
cat <<YAML
apiVersion: v1
kind: Pod
metadata: {name: $1, namespace: $NS, labels: {test: offlinks}}
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
  - {uid: 4401, peer_pod: r2, local_intf: eth1, peer_intf: eth1, local_ip: 10.44.1.1/24, peer_ip: 10.44.1.2/24}
  - {uid: 4402, peer_pod: r3, local_intf: eth2, peer_intf: eth1, local_ip: 10.44.2.1/24, peer_ip: 10.44.2.3/24}
---
apiVersion: networkop.co.uk/v1beta1
kind: Topology
metadata: {name: r2, namespace: $NS}
spec:
  links:
  - {uid: 4401, peer_pod: r1, local_intf: eth1, peer_intf: eth1, local_ip: 10.44.1.2/24, peer_ip: 10.44.1.1/24}
---
apiVersion: networkop.co.uk/v1beta1
kind: Topology
metadata: {name: r3, namespace: $NS}
spec:
  links:
  - {uid: 4402, peer_pod: r1, local_intf: eth1, peer_intf: eth2, local_ip: 10.44.2.3/24, peer_ip: 10.44.2.1/24}
YAML
}

check() { # description command...
  local desc=$1; shift
  local t0=$(date +%s)
  for i in $(seq 1 30); do
    if "$@" >/dev/null 2>&1; then echo "[OK ] $desc ($(( $(date +%s) - t0 ))s)"; return; fi
    sleep 1
  done
  fails=$((fails + 1)); echo "[FAIL] $desc ($(( $(date +%s) - t0 ))s): $($K -n $NS exec r1 -- ip addr 2>&1 | grep -A2 'eth[12]' | tr '\n' ' ' | cut -c1-200)"
}
index() { $K -n $NS exec r1 -- cat /sys/class/net/$1/ifindex 2>/dev/null; }
same() { [ -n "$2" ] && [ "$(index $1)" = "$2" ]; }
dark() { same $1 $2 && $K -n $NS exec r1 -- ip link show $1 | grep -q NO-CARRIER && $K -n $NS exec r1 -- ip addr show $1 | grep -q "$3"; }
lit() { same $1 $2 && ! $K -n $NS exec r1 -- ip link show $1 | grep -q NO-CARRIER; }
pings() { $K -n $NS exec r1 -- ping -c 1 -W 1 "$1"; }
start() {
  if ! { pod $1 $2 | $K apply -f - >/dev/null && $K -n $NS wait --timeout=90s --for condition=Ready pod/$1 >/dev/null; }; then
    fails=$((fails + 1)); echo "[FAIL] $1 did not start on $2"
  fi
}
stop() { $K -n $NS delete pod $1 --wait=true >/dev/null 2>&1; }
r1start() { $K -n $NS get pod r1 -o jsonpath='{.status.containerStatuses[0].containerID}/{.status.containerStatuses[0].restartCount}'; }

$K delete ns $NS --ignore-not-found --wait=true >/dev/null 2>&1
$K create ns $NS >/dev/null
topologies | $K apply -f - >/dev/null
start r1 $A; start r2 $A; start r3 $B
check "veth up, r1 ping r2" pings 10.44.1.2
check "vxlan up, r1 ping r3" pings 10.44.2.3
i1=$(index eth1); i2=$(index eth2); before=$(r1start)

stop r2
check "r2 off: r1 eth1 stays, with its address, without carrier" dark eth1 "$i1" 10.44.1.1/24
stop r3
check "r3 off: r1 eth2 stays, with its address, without carrier" dark eth2 "$i2" 10.44.2.1/24

start r2 $A
check "r2 back: r1 eth1 is the same interface and carries" lit eth1 "$i1"
check "r2 back: r1 ping r2" pings 10.44.1.2
start r3 $A
check "r3 back on r1's node: r1 eth2 is the same interface and carries" lit eth2 "$i2"
check "r3 back on r1's node: r1 ping r3" pings 10.44.2.3

$K -n $NS exec r2 -- ip link set eth1 down >/dev/null
check "r2 shuts eth1: r1 eth1 loses carrier" dark eth1 "$i1" 10.44.1.1/24
$K -n $NS exec r2 -- ip link set eth1 up >/dev/null
check "r2 brings eth1 up: r1 eth1 carries again" lit eth1 "$i1"
check "r1 ping r2 after the shutdown" pings 10.44.1.2

if [ -n "$before" ] && [ "$(r1start)" = "$before" ]; then echo "[OK ] r1 never restarted"; else fails=$((fails + 1)); echo "[FAIL] r1 restarted: $before -> $(r1start)"; fi
$K delete ns $NS --wait=true >/dev/null 2>&1
echo "off links test: $fails failed"
exit $(( fails > 0 ))
