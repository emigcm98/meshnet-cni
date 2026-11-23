#!/bin/bash
# Links added to and removed from running pods: r1 and r2 share a worker, r3 sits on the other, with a
# veth and a vxlan that stay and a veth and a vxlan that come and go. Each change must show up in the
# pods' interfaces and pings within a few seconds, without any of the pods restarting. Usage:
# tests/live-links.sh [worker-a] [worker-b]; KUBECTL_CONTEXT picks the cluster. The uids are the VXLAN
# VNIs, which the kernel wants unique per node, so they are ones no other test topology uses.
A=${1:-meshnet-worker}; B=${2:-meshnet-worker2}
K="kubectl ${KUBECTL_CONTEXT:+--context $KUBECTL_CONTEXT}"
NS=live
fails=0

pod() { # name node
cat <<YAML
apiVersion: v1
kind: Pod
metadata: {name: $1, namespace: $NS, labels: {test: live}}
spec:
  nodeName: $2
  terminationGracePeriodSeconds: 0
  containers:
  - {image: alpine:3.24.2, name: pod, command: ["/bin/sh", "-c", "sleep infinity"]}
YAML
}

# topologies [with-extra]: the two lasting links always, the two extra ones when asked.
topologies() {
  local r1="  - {uid: 4301, peer_pod: r2, local_intf: eth1, peer_intf: eth1, local_ip: 10.43.1.1/24, peer_ip: 10.43.1.2/24}
  - {uid: 4302, peer_pod: r3, local_intf: eth2, peer_intf: eth1, local_ip: 10.43.2.1/24, peer_ip: 10.43.2.3/24}"
  local r2="  - {uid: 4301, peer_pod: r1, local_intf: eth1, peer_intf: eth1, local_ip: 10.43.1.2/24, peer_ip: 10.43.1.1/24}"
  local r3="  - {uid: 4302, peer_pod: r1, local_intf: eth1, peer_intf: eth2, local_ip: 10.43.2.3/24, peer_ip: 10.43.2.1/24}"
  if [ "$1" = with-extra ]; then
    r1="$r1
  - {uid: 4303, peer_pod: r2, local_intf: eth3, peer_intf: eth2, local_ip: 10.43.3.1/24, peer_ip: 10.43.3.2/24}
  - {uid: 4304, peer_pod: r3, local_intf: eth4, peer_intf: eth2, local_ip: 10.43.4.1/24, peer_ip: 10.43.4.3/24}"
    r2="$r2
  - {uid: 4303, peer_pod: r1, local_intf: eth2, peer_intf: eth3, local_ip: 10.43.3.2/24, peer_ip: 10.43.3.1/24}"
    r3="$r3
  - {uid: 4304, peer_pod: r1, local_intf: eth2, peer_intf: eth4, local_ip: 10.43.4.3/24, peer_ip: 10.43.4.1/24}"
  fi
  for t in r1 r2 r3; do
    printf -- "---\napiVersion: networkop.co.uk/v1beta1\nkind: Topology\nmetadata: {name: %s, namespace: %s}\nspec:\n  links:\n%s\n" "$t" "$NS" "${!t}"
  done
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
has() { $K -n $NS exec "$1" -- ip link show "$2"; }
lacks() { ! $K -n $NS exec "$1" -- ip link show "$2"; }
pings() { $K -n $NS exec "$1" -- ping -c 1 -W 1 "$2"; }
starts() { $K -n $NS get pods -l test=live -o jsonpath='{range .items[*]}{.metadata.name}={.status.containerStatuses[0].containerID}/{.status.containerStatuses[0].restartCount} {end}'; }

$K delete ns $NS --ignore-not-found --wait=true >/dev/null 2>&1
$K create ns $NS >/dev/null
topologies | $K apply -f - >/dev/null
pod r1 $A | $K apply -f - >/dev/null; pod r2 $A | $K apply -f - >/dev/null; pod r3 $B | $K apply -f - >/dev/null
$K -n $NS wait --timeout=120s --for condition=Ready pod/r1 pod/r2 pod/r3 >/dev/null
check "veth that stays, r1 ping r2" pings r1 10.43.1.2
check "vxlan that stays, r1 ping r3" pings r1 10.43.2.3
before=$(starts)

topologies with-extra | $K apply -f - >/dev/null
check "veth added live, r1 ping r2 over eth3" pings r1 10.43.3.2
check "vxlan added live, r1 ping r3 over eth4" pings r1 10.43.4.3

topologies | $K apply -f - >/dev/null
check "veth removed live, gone from r1" lacks r1 eth3
check "veth removed live, gone from r2" lacks r2 eth2
check "vxlan removed live, gone from r1" lacks r1 eth4
check "vxlan removed live, gone from r3" lacks r3 eth2
check "veth that stays still answers" pings r1 10.43.1.2
check "vxlan that stays still answers" pings r1 10.43.2.3

if [ "$(starts)" = "$before" ]; then echo "[OK ] no pod restarted"; else fails=$((fails + 1)); echo "[FAIL] a pod restarted: $before -> $(starts)"; fi
$K delete ns $NS --wait=true >/dev/null 2>&1
echo "live links test: $fails failed"
exit $(( fails > 0 ))
