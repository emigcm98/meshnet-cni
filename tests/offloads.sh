#!/bin/bash
# The tx offloads of the pod ends: the kernel's by default, off where a link says offloads: false and
# back when it stops saying it, live; with them on, one TCP stream carries well over twice what it
# does with them off, on one node and across workers. o1 and o2 share worker A, o3 sits on worker B.
# Usage: tests/offloads.sh [worker-a] [worker-b]; KUBECTL_CONTEXT picks the cluster.
A=${1:-meshnet-worker}; B=${2:-meshnet-worker2}
K="kubectl ${KUBECTL_CONTEXT:+--context $KUBECTL_CONTEXT}"
NS=offloads
fails=0

pod() { # name node
cat <<YAML
apiVersion: v1
kind: Pod
metadata: {name: $1, namespace: $NS, labels: {test: offloads}}
spec:
  nodeName: $2
  terminationGracePeriodSeconds: 0
  containers:
  - name: pod
    image: nicolaka/netshoot:v0.16
    command: ["/bin/sh", "-c", "iperf3 -s -D && sleep infinity"]
    securityContext: {capabilities: {add: ["NET_ADMIN"]}}
YAML
}

topologies() {
cat <<YAML
apiVersion: networkop.co.uk/v1beta1
kind: Topology
metadata: {name: o1, namespace: $NS}
spec:
  links:
  - {uid: 4801, peer_pod: o2, local_intf: eth1, peer_intf: eth1, local_ip: 10.48.1.1/24}
  - {uid: 4802, peer_pod: o3, local_intf: eth2, peer_intf: eth1, local_ip: 10.48.2.1/24}
---
apiVersion: networkop.co.uk/v1beta1
kind: Topology
metadata: {name: o2, namespace: $NS}
spec:
  links:
  - {uid: 4801, peer_pod: o1, local_intf: eth1, peer_intf: eth1, local_ip: 10.48.1.2/24}
---
apiVersion: networkop.co.uk/v1beta1
kind: Topology
metadata: {name: o3, namespace: $NS}
spec:
  links:
  - {uid: 4802, peer_pod: o1, local_intf: eth1, peer_intf: eth2, local_ip: 10.48.2.3/24}
YAML
}

check() { # description command...
  local desc=$1; shift
  local t0=$(date +%s)
  for i in $(seq 1 30); do
    if "$@" >/dev/null 2>&1; then echo "[OK ] $desc ($(( $(date +%s) - t0 ))s)"; return; fi
    sleep 1
  done
  fails=$((fails + 1)); echo "[FAIL] $desc ($(( $(date +%s) - t0 ))s)"
}
# offload prints the checksum and TCP segmentation offloads of interface $2 of pod $1, as "on on".
# The features themselves, not ethtool's summary, which newer kernels keep on through AccECN's.
offload() {
  $K -n $NS exec $1 -- ethtool -k $2 | sed -n 's/^[[:space:]]*\(tx-checksum-ip-generic\|tx-tcp-segmentation\): \([a-z]*\).*/\2/p' | paste -sd' '
}
is() { [ "$(offload $1 $2)" = "$3 $3" ]; }
# mbps prints the Mbit/s of one TCP stream for 5 s from pod $1 to address $2.
mbps() { $K -n $NS exec $1 -- iperf3 -c $2 -t 5 -J 2>/dev/null | jq '.end.sum_received.bits_per_second / 1000000 | floor' 2>/dev/null; }
# offloads sets offloads on link index $2 of the Topology of pod $1 to $3, or drops it with null.
offloads() {
  if [ "$3" = null ]; then
    $K -n $NS patch topology $1 --type=json -p "[{\"op\":\"remove\",\"path\":\"/spec/links/$2/offloads\"}]" >/dev/null
  else
    $K -n $NS patch topology $1 --type=json -p "[{\"op\":\"add\",\"path\":\"/spec/links/$2/offloads\",\"value\":$3}]" >/dev/null
  fi
}
faster() { # description on off
  if [ -n "$2" ] && [ -n "$3" ] && [ "$2" -gt $(( 2 * $3 )) ]; then
    echo "[OK ] $1: $2 Mbit/s with the offloads on, $3 off"
  else
    fails=$((fails + 1)); echo "[FAIL] $1: ${2:-none} Mbit/s with the offloads on, ${3:-none} off; want over twice"
  fi
}
start() {
  if ! { pod $1 $2 | $K apply -f - >/dev/null && $K -n $NS wait --timeout=180s --for condition=Ready pod/$1 >/dev/null; }; then
    fails=$((fails + 1)); echo "[FAIL] $1 did not start on $2"
  fi
}

$K delete ns $NS --ignore-not-found --wait=true >/dev/null 2>&1
$K create ns $NS >/dev/null
topologies | $K apply -f - >/dev/null
start o1 $A; start o2 $A; start o3 $B

check "o1 eth1, on one node, keeps the kernel's offloads" eval 'is o1 eth1 on && is o2 eth1 on'
check "o1 eth2, across workers, keeps the kernel's offloads" eval 'is o1 eth2 on && is o3 eth1 on'
check "o1 reaches o2 and o3" eval "$K -n $NS exec o1 -- ping -c 1 -W 1 10.48.1.2 && $K -n $NS exec o1 -- ping -c 1 -W 1 10.48.2.3"
same_on=$(mbps o1 10.48.1.2); across_on=$(mbps o1 10.48.2.3)

offloads o1 0 false; offloads o2 0 false; offloads o1 1 false; offloads o3 0 false
check "offloads: false switches them off on the running ends" eval 'is o1 eth1 off && is o2 eth1 off && is o1 eth2 off && is o3 eth1 off'
same_off=$(mbps o1 10.48.1.2); across_off=$(mbps o1 10.48.2.3)
faster "one TCP stream on one node" "$same_on" "$same_off"
faster "one TCP stream across workers" "$across_on" "$across_off"

offloads o1 0 null; offloads o2 0 null; offloads o1 1 null; offloads o3 0 null
check "with offloads dropped the ends get the kernel's back" eval 'is o1 eth1 on && is o2 eth1 on && is o1 eth2 on && is o3 eth1 on'

$K delete ns $NS --wait=true >/dev/null 2>&1
echo "offloads test: $fails failed"
exit $(( fails > 0 ))
