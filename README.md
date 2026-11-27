# meshnet CNI

**meshnet** is a Kubernetes CNI plugin that wires point-to-point links between pods from a topology stored in the cluster. Two pods on the same node get a veth pair, two pods on different nodes get a VXLAN tunnel, and a pod can be attached to an external device through a macvlan interface. Every pod keeps its first interface from the cluster's primary CNI; meshnet chains itself after it and only adds interfaces to pods that have a `Topology` resource.

This is the fork that [KubeNDT](https://github.com/emigcm98/kubendt) runs. It descends from the archived [networkop/meshnet-cni](https://github.com/networkop/meshnet-cni) (BSD 3-Clause, Michael Kashin) and takes pieces from the fork maintained inside [openconfig/kne](https://github.com/openconfig/kne/tree/main/third_party/meshnet) (Apache 2.0), each attributed in the commit that brings it. The CNI binary, the NetworkAttachmentDefinition and the CRD keep their original names, so topologies written for the original line work unchanged.

## How it works

The `meshnet` binary is a chained CNI plugin: when a pod with a `Topology` starts, it tells the local `meshnetd` the pod's network namespace and node address, then waits up to 30 s for the pod's interfaces. `meshnetd` runs on every node and watches the `Topology` resources; it reconciles a local pod when the pod or one of its peers changes and once a minute, creating what is missing and replacing what is of the wrong kind, so a pod that moves to another node, or a link deleted by hand, is rewired without a restart. Each end of a link in a pod is a veth whose other end stays on the node, named `mn<hash>`; the daemon joins the node ends of a link with tc redirects when both pods are on one node, and through a vxlan of the node (`mx<hash>`) when they are not; a direction whose shaping drops, duplicates, reorders or corrupts packets also gets a segmenting hop, the veth pair `ms<hash>` and `mt<hash>` in front of the node end. A pod that goes takes only its own pair: its neighbours keep their interfaces and their configuration, without carrier, until it is back, and their interfaces are the same ones whichever node it comes back on. The carrier a pod sees is set by the daemon on the node end: none while the peer is off or the link is unplugged, the peer's admin state when both are on one node, up across nodes. Node ends are at the largest MTU a veth takes, so the pod ends alone decide the MTU of a link, as with a direct veth. Links added to or removed from a `Topology` are wired or removed on the running pods. What a link declares of an interface (addresses, MAC, MTU, queue length, offloads) is applied when the interface is made and whenever that declaration changes; in between the interface is the workload's, so an address, an MTU or a shut port set by hand inside the pod stays until the `Topology` changes that same property or the pod is recreated. Every interface meshnet wires carries `meshnet/<uid>/<peer>/<digests>` as its alias, the digests being those of the declaration it last applied, and only tagged interfaces are ever deleted, so the workload's own interfaces are safe; likewise the addresses meshnet puts carry their own address protocol (`0x6d`, kernel 5.18 or later; on an older kernel an address dropped from a declaration stays), and only those are ever removed. The error of the last reconcile, if any, is in the pod's `status.plumbing_error`.

`LINK_MTU` on the DaemonSet sets the MTU of links that do not ask for one; without it they get the MTU of the node's interface less the 50 bytes of a vxlan, 1450 on a 1500 network, and `LINK_TXQUEUELEN` the queue length of link ends that do not ask for one (10000 by default). Every node end steers each flow it receives to one CPU (RPS), since a pod end hands frames to it through the backlog of the CPU its sender runs on, and a sender that moves between CPUs would otherwise have its flow drained by two at once and, on a NIC with a tx queue per CPU, reordered across nodes; `NODE_END_RPS_CPUS` on the DaemonSet picks those CPUs as a list such as `2-5` (all online ones by default, `none` for no steering), and `meshnet_link_rps` tells which node ends have it, since it needs a writable sysfs, which a kind node does not give.

## Topology

One `Topology` per pod, in the pod's namespace, listing its links. A link appears in both pods' Topologies with the same `uid`, which is the VXLAN VNI when the link crosses nodes. Each Topology describes its own pod's end: what has no `peer_` in its name is that end's, so r1's `eth1` below gets `12.12.12.1/24`, `2001:db8:12::1/64` and its MAC from r1's Topology, and r2's `eth1` gets its own from r2's; `peer_ip` is informative and meshnet puts it nowhere.

```yaml
apiVersion: networkop.co.uk/v1beta1
kind: Topology
metadata:
  name: r1
spec:
  links:
  - uid: 1
    peer_pod: r2
    local_intf: eth1
    local_ip: 12.12.12.1/24
    peer_intf: eth1
    peer_ip: 12.12.12.2/24
    addresses: ["2001:db8:12::1/64"]
    mac: "02:00:00:00:12:01"
    mtu: 1400
---
apiVersion: networkop.co.uk/v1beta1
kind: Topology
metadata:
  name: r2
spec:
  links:
  - uid: 1
    peer_pod: r1
    local_intf: eth1
    local_ip: 12.12.12.2/24
    peer_intf: eth1
    peer_ip: 12.12.12.1/24
    addresses: ["2001:db8:12::2/64"]
    mac: "02:00:00:00:12:02"
```

A link whose `peer_pod` is `localhost` is a macvlan on the node interface named by `peer_intf`. The `uid` must be unique on a node across namespaces, since the kernel refuses a second VXLAN with a VNI already in use. The rest is optional and describes the local end, from the pod's own `Topology`: `addresses`, a list of either family beyond `local_ip`; `mac`; `mtu`, where both ends get the larger of the two requests; `txqueuelen`; `offloads`, the transmit offloads of the end, which keeps the kernel's when absent, all on for a veth as on a NIC, while `false` switches off checksumming and segmentation and `true` switches them on (an uplink's follow its parent NIC as far as the kernel lets a macvlan change them). With them on, a sender hands over frames with partial checksums and up to 64 KB, which a pod's kernel takes as they are; a workload that reads raw frames from its interface, as a software switch does, has to take care of them itself, since across workers frames merged by GRO reach it whatever the sender declares; and `unplugged: true`, the cable pulled, which either end can declare: both pods keep their interfaces administratively up, with their configuration, and lose carrier until it is plugged back, on one node as across nodes. An uplink has no cable to unplug. `shaping` is a netem or a tbf for the traffic the local end sends, with tc's units, as in `shaping: {delay: 20ms, jitter: 2ms, loss: 0.5%, rate: 10mbit}` or `shaping: {qdisc: tbf, rate: 10mbit, burst: 32kbit, latency: 50ms}`; the netem fields are `delay`, `jitter`, `delay_correlation`, `loss`, `loss_correlation`, `duplicate`, `corrupt`, `reorder`, `reorder_correlation`, `limit` and `rate`. It is put on the node end of the other side, so it shapes the link in that direction alone, on one node as across nodes, out of the workload's sight and apart from its own qdiscs; the daemon keeps it there, changes it in place when the declaration changes, and puts it back on a recreated pod's new node end. netem drops, duplicates, reorders and corrupts whole frames, and a frame can hold many TCP segments: a sender with its offloads on hands them over that way, and across workers the receiving node merges segments on the way in. So a shaping with `loss`, `duplicate`, `reorder` or `corrupt` gets a segmenting hop on the node in front of that node end, a veth pair without segmentation offloads that cuts every frame into its segments, and a TCP stream through `loss: 1%` loses 1 % of its packets one by one rather than the same share in bursts. netem's `limit`, 1000 by default, then counts segments, so a long delay on a fast link may need a larger one. An uplink's shaping sits on its interface in the pod, before any segmentation, so there they still act on whole frames. An uplink's shaping goes on the interface in the pod, as part of its declaration. An invalid shaping is reported in `status.plumbing_error` and leaves the last valid one in place, without keeping the link from being wired. `tests/` holds topologies for two, three and five nodes, one with several links between the same pair, and the macvlan case.

## Status, events and metrics

The daemon of a pod's node writes the state of each of its links in `status.links` of its Topology, only when it changes: `state` (`up`; `down`, a port shut on either end; `off`, the peer not running; `unplugged`; `failed`), `transport` (`veth` on one node, `vxlan` across nodes, `macvlan` for an uplink), `node`, `ifindex`, `mac`, `mtu`, `vni` and `error`. A link that fails is `failed` with its error, and the pod's other links are wired all the same; `status.plumbing_error` gathers the errors of the last pass. `kubectl get topology` shows each pod's node address and the states of its links. The status goes with the pod.

Each change of a link is an event on its Topology: `Wired` (a link made, made again or moved to another transport), `Unwired`, `LinkUp`, `LinkDown` with its cause, and the warning `WiringFailed`, so `kubectl get events --field-selector involvedObject.kind=Topology` is the history of a lab's links.

`meshnetd` serves Prometheus metrics on port 51112 of each node, `/metrics` (`METRICS_PORT` on the DaemonSet moves it, 0 turns it off): `meshnet_link_info` with the state, transport and node of each link end, `meshnet_link_up`, and from the node ends the bytes, packets and drops each pod received and sent on each link, and the packets the link's shaping dropped (`meshnet_link_*_total`), and `meshnet_link_rps`, 1 when the node end steers each flow to one CPU, read from sysfs, all labelled by namespace, pod, interface, peer and uid. An uplink, which has no node end, has its state alone.

## Install

The manifests create the `meshnet` namespace, the CRD, the RBAC and a DaemonSet that runs `meshnetd` on every node and drops the `meshnet` binary and its CNI configuration there. There is no release yet: the first one, `v1.0.0`, comes with the KubeNDT release that adopts this fork, and from then on a tag pins the manifests and the image of the same version. Until then, apply `master` and set the image to the `sha-<commit>` of the commit you want:

```sh
kubectl apply -k "github.com/emigcm98/meshnet-cni/manifests/base?ref=master"
kubectl -n meshnet set image ds/meshnet meshnet=ghcr.io/emigcm98/meshnet:sha-<commit>
```

Images live at `ghcr.io/emigcm98/meshnet`: every merge to `master` publishes `sha-<commit>`, and every `v*` tag will publish its version and `latest`.

## Develop

```sh
make test        # go test ./...
make test-root   # the tests that make network namespaces, as root
make docker      # the image, tagged with the commit
make up          # a local kind cluster of one control plane and two workers
make install     # loads the image into it and applies manifests/overlays/e2e
make e2e         # the three-node topology of tests/3node.yml and pings over its links
make eviction    # tests/eviction.sh: a pod recreated on its peer's worker and back, a ping per step
make live-links  # tests/live-links.sh: links added to and removed from running pods
make off-links   # tests/off-links.sh: neighbours switched off and on, a port shut, the survivor's interfaces kept
make declared    # tests/declared.sh: the interface as declared, a cable unplugged, live changes, the workload's kept
make shaping     # tests/shaping.sh: delay, loss and rate per direction, on one node and across nodes, changed live
make status      # tests/status.sh: each link's state in its Topology, its events and the metrics
make offloads    # tests/offloads.sh: the kernel's tx offloads by default, off and back live, one TCP stream with and without, loss per packet
make down
```

The gRPC code under `daemon/proto` is generated and committed; `make proto` regenerates it with buf. Daemon logs come from `kubectl logs -n meshnet -l name=meshnet`; the plugin writes to `/var/log/meshnet-cni.log` on each node.

## License

BSD 3-Clause, see `LICENSE`.
