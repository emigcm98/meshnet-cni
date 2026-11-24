#!/bin/bash
# The interface as declared: r1's links are born with their addresses of both families, MAC, MTU and
# queue length; a cable unplugged and plugged back on one node and across nodes; a declaration changed
# live; what the workload changes by hand kept until the Topology changes that same property; frames
# larger than the link's MTU once both pods raise theirs; and r1 recreated, born with the last
# declaration. r1 and r2 share worker A, r3 sits on worker B. Usage: tests/declared.sh [worker-a]
# [worker-b]; KUBECTL_CONTEXT picks the cluster. The uids are the VXLAN VNIs, unique per node.
A=${1:-meshnet-worker}; B=${2:-meshnet-worker2}
K="kubectl ${KUBECTL_CONTEXT:+--context $KUBECTL_CONTEXT}"
NS=declared
fails=0

pod() { # name node
cat <<YAML
apiVersion: v1
kind: Pod
metadata: {name: $1, namespace: $NS, labels: {test: declared}}
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
  - {uid: 4501, peer_pod: r2, local_intf: eth1, peer_intf: eth1, local_ip: 10.45.1.1/24, addresses: ["fd45:1::1/64"], mac: "02:45:01:00:00:01", mtu: 1400, txqueuelen: 500}
  - {uid: 4502, peer_pod: r3, local_intf: eth2, peer_intf: eth1, local_ip: 10.45.2.1/24}
---
apiVersion: networkop.co.uk/v1beta1
kind: Topology
metadata: {name: r2, namespace: $NS}
spec:
  links:
  - {uid: 4501, peer_pod: r1, local_intf: eth1, peer_intf: eth1, local_ip: 10.45.1.2/24, addresses: ["fd45:1::2/64"]}
---
apiVersion: networkop.co.uk/v1beta1
kind: Topology
metadata: {name: r3, namespace: $NS}
spec:
  links:
  - {uid: 4502, peer_pod: r1, local_intf: eth1, peer_intf: eth2, local_ip: 10.45.2.3/24}
YAML
}

# check waits up to 30 s for a condition; holds fails if a condition stops being true within 8 s.
check() { # description command...
  local desc=$1; shift
  local t0=$(date +%s)
  for i in $(seq 1 30); do
    if "$@" >/dev/null 2>&1; then echo "[OK ] $desc ($(( $(date +%s) - t0 ))s)"; return; fi
    sleep 1
  done
  fails=$((fails + 1)); echo "[FAIL] $desc ($(( $(date +%s) - t0 ))s)"
}
holds() { # description command...
  local desc=$1; shift
  for i in $(seq 1 8); do
    if ! "$@" >/dev/null 2>&1; then fails=$((fails + 1)); echo "[FAIL] $desc (after ${i}s)"; return; fi
    sleep 1
  done
  echo "[OK ] $desc"
}
x() { local p=$1; shift; $K -n $NS exec $p -- "$@"; }
sys() { x $1 cat /sys/class/net/$2/$3 2>/dev/null; }
is() { [ "$(sys $1 $2 $3)" = "$4" ]; }
has() { x $1 ip addr show $2 | grep -q " $3 "; }
hasnt() { ! has "$@"; }
qlen() { x $1 ip link show $2 | grep -q "qlen $3"; }
dark() { x $1 ip link show $2 | grep -q NO-CARRIER && x $1 ip link show $2 | grep -q "[<,]UP[,>]"; }
lit() { ! x $1 ip link show $2 | grep -q NO-CARRIER; }
pings() { x $1 ping -c 1 -W 1 "${@:2}"; }
set_link() { # topology link-index field json-value
  $K -n $NS patch topology $1 --type=json -p "[{\"op\":\"add\",\"path\":\"/spec/links/$2/$3\",\"value\":$4}]" >/dev/null
}
poke() { $K -n $NS annotate topology $1 test/poke="$(date +%s%N)" --overwrite >/dev/null; }
start() {
  if ! { pod $1 $2 | $K apply -f - >/dev/null && $K -n $NS wait --timeout=90s --for condition=Ready pod/$1 >/dev/null; }; then
    fails=$((fails + 1)); echo "[FAIL] $1 did not start on $2"
  fi
}
r1start() { $K -n $NS get pod r1 -o jsonpath='{.status.containerStatuses[0].containerID}/{.status.containerStatuses[0].restartCount}'; }

$K delete ns $NS --ignore-not-found --wait=true >/dev/null 2>&1
$K create ns $NS >/dev/null
topologies | $K apply -f - >/dev/null
start r1 $A; start r2 $A; start r3 $B

check "born: r1 eth1 has its MAC" is r1 eth1 address 02:45:01:00:00:01
check "born: r1 eth1 and r2 eth1 have the link's MTU, 1400" eval 'is r1 eth1 mtu 1400 && is r2 eth1 mtu 1400'
check "born: r1 eth1 has its queue length, 500" qlen r1 eth1 500
check "born: r1 eth1 has both families" eval 'has r1 eth1 10.45.1.1/24 && has r1 eth1 fd45:1::1/64'
check "born: r1 ping r2 over IPv4 and IPv6" eval 'pings r1 10.45.1.2 && pings r1 fd45:1::2'
check "born: r1 ping r3 across workers" pings r1 10.45.2.3
before=$(r1start)

set_link r2 0 unplugged true
check "unplugged from r2's side, one node: both ends without carrier, both admin up" eval 'dark r1 eth1 && dark r2 eth1'
set_link r2 0 unplugged false
check "plugged back, one node: r1 ping r2" pings r1 10.45.1.2
set_link r1 1 unplugged true
check "unplugged across workers: both ends without carrier, both admin up" eval 'dark r1 eth2 && dark r3 eth1'
set_link r1 1 unplugged false
check "plugged back across workers: r1 ping r3" pings r1 10.45.2.3

set_link r1 0 addresses '["fd45:1::11/64"]'
set_link r1 0 mac '"02:45:01:00:00:11"'
set_link r1 0 mtu 1300
check "live: r1 eth1 takes its new MAC" is r1 eth1 address 02:45:01:00:00:11
check "live: the new address list, local_ip kept" eval 'has r1 eth1 fd45:1::11/64 && hasnt r1 eth1 fd45:1::1/64 && has r1 eth1 10.45.1.1/24'
check "live: the new MTU on both ends" eval 'is r1 eth1 mtu 1300 && is r2 eth1 mtu 1300'

x r1 ip addr add 10.99.0.1/24 dev eth1 >/dev/null
x r1 ip link set eth1 mtu 1350 >/dev/null
x r1 ip addr del fd45:1::11/64 dev eth1 >/dev/null
poke r1
holds "by hand: an unchanged declaration keeps what the workload did" eval 'has r1 eth1 10.99.0.1/24 && is r1 eth1 mtu 1350 && hasnt r1 eth1 fd45:1::11/64'
set_link r1 0 mac '"02:45:01:00:00:12"'
check "by hand: a new MAC changes the MAC" is r1 eth1 address 02:45:01:00:00:12
holds "by hand: and nothing else the workload did" eval 'has r1 eth1 10.99.0.1/24 && is r1 eth1 mtu 1350 && hasnt r1 eth1 fd45:1::11/64'

for p in "r1 eth1" "r2 eth1" "r1 eth2" "r3 eth1"; do x ${p% *} ip link set ${p#* } mtu 3000 >/dev/null; done
check "3000-byte frames on one node once both pods raise their MTU" pings r1 -s 2972 10.45.1.2
check "3000-byte frames across workers once both pods raise their MTU" pings r1 -s 2972 10.45.2.3

if [ -n "$before" ] && [ "$(r1start)" = "$before" ]; then echo "[OK ] r1 never restarted"; else fails=$((fails + 1)); echo "[FAIL] r1 restarted: $before -> $(r1start)"; fi
$K -n $NS delete pod r1 --wait=true >/dev/null 2>&1
start r1 $A
check "recreated: r1 eth1 born with the last declaration" eval 'is r1 eth1 address 02:45:01:00:00:12 && is r1 eth1 mtu 1300 && has r1 eth1 fd45:1::11/64 && hasnt r1 eth1 10.99.0.1/24'
check "recreated: r1 ping r2 and r3" eval 'pings r1 10.45.1.2 && pings r1 10.45.2.3'

$K delete ns $NS --wait=true >/dev/null 2>&1
echo "declared test: $fails failed"
exit $(( fails > 0 ))
