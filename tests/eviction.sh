#!/bin/bash
# Two pods with one link, pinned to two workers, and the first one deleted and recreated on the other worker
# (vxlan to veth), back (veth to vxlan), on the same worker again, and the veth cases. Each step waits for the
# pod to be Ready and pings over the link. Usage: tests/eviction.sh [worker-a] [worker-b]; KUBECTL_CONTEXT picks
# the cluster, the defaults are the workers of the kind that make up creates. The link's uid is the VXLAN VNI,
# which the kernel wants unique per node, so it is one no other test topology uses.
A=${1:-meshnet-worker}; B=${2:-meshnet-worker2}
K="kubectl ${KUBECTL_CONTEXT:+--context $KUBECTL_CONTEXT}"
NS=evict
fails=0

pod() { # name node
cat <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $1
  namespace: $NS
  labels: {test: evict}
spec:
  nodeName: $2
  terminationGracePeriodSeconds: 0
  containers:
  - image: alpine:3.24.2
    name: pod
    command: ["/bin/sh", "-c", "sleep infinity"]
EOF
}

topo() {
cat <<EOF
apiVersion: networkop.co.uk/v1beta1
kind: Topology
metadata: {name: r1, namespace: $NS}
spec:
  links:
  - {uid: 4242, peer_pod: r2, local_intf: eth1, peer_intf: eth1, local_ip: 12.12.12.1/24, peer_ip: 12.12.12.2/24}
---
apiVersion: networkop.co.uk/v1beta1
kind: Topology
metadata: {name: r2, namespace: $NS}
spec:
  links:
  - {uid: 4242, peer_pod: r1, local_intf: eth1, peer_intf: eth1, local_ip: 12.12.12.2/24, peer_ip: 12.12.12.1/24}
EOF
}

step() { # name node-for-r1
  local name=$1 node=$2 t0=$(date +%s)
  $K -n $NS delete pod r1 --ignore-not-found --wait=true >/dev/null 2>&1
  pod r1 $node | $K apply -f - >/dev/null
  if $K -n $NS wait --timeout=90s --for condition=Ready pod r1 >/dev/null 2>&1 && $K -n $NS exec r1 -- ping -c 2 -W 2 12.12.12.2 >/dev/null 2>&1; then
    echo "[OK ] $name ($(( $(date +%s) - t0 ))s)"
  else
    fails=$((fails + 1))
    echo "[FAIL] $name ($(( $(date +%s) - t0 ))s): r1 $($K -n $NS get pod r1 -o jsonpath='{.status.phase}') on $node, r2 eth1: $($K -n $NS exec r2 -- ip link show eth1 2>&1 | head -1 | cut -c1-80)"
    $K -n $NS get events --field-selector involvedObject.name=r1 -o custom-columns=:message --no-headers 2>/dev/null | grep -i "meshnet\|cni\|exists" | tail -2 | cut -c1-200
  fi
}

$K delete ns $NS --ignore-not-found --wait=true >/dev/null 2>&1
$K create ns $NS >/dev/null
topo | $K apply -f - >/dev/null
pod r2 $A | $K apply -f - >/dev/null
$K -n $NS wait --timeout=120s --for condition=Ready pod r2 >/dev/null
step "r1 first on $B, link to r2 on $A over vxlan" $B
step "r1 moved to $A, its peer's worker: vxlan to veth" $A
step "r1 moved back to $B: veth to vxlan" $B
step "r1 recreated on $B again: vxlan to vxlan" $B
step "r1 moved to $A once more: vxlan to veth" $A
step "r1 recreated on $A: veth to veth" $A
$K delete ns $NS --wait=true >/dev/null 2>&1
echo "eviction test: $((6 - fails)) of 6 steps passed"
exit $(( fails > 0 ))
