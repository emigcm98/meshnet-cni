# meshnet CNI

**meshnet** is a Kubernetes CNI plugin that wires point-to-point links between pods from a topology stored in the cluster. Two pods on the same node get a veth pair, two pods on different nodes get a VXLAN tunnel, and a pod can be attached to an external device through a macvlan interface. Every pod keeps its first interface from the cluster's primary CNI; meshnet chains itself after it and only adds interfaces to pods that have a `Topology` resource.

This is the fork that [KubeNDT](https://github.com/emigcm98/kubendt) runs. It descends from the archived [networkop/meshnet-cni](https://github.com/networkop/meshnet-cni) (BSD 3-Clause, Michael Kashin) and takes pieces from the fork maintained inside [openconfig/kne](https://github.com/openconfig/kne/tree/main/third_party/meshnet) (Apache 2.0), each attributed in the commit that brings it. The CNI binary, the NetworkAttachmentDefinition and the CRD keep their original names, so topologies written for the original line work unchanged.

## How it works

The `meshnet` binary is a chained CNI plugin: when a pod with a `Topology` starts, it tells the local `meshnetd` the pod's network namespace and node address, then waits up to 30 s for the pod's interfaces. `meshnetd` runs on every node and watches the `Topology` resources; it reconciles a local pod when the pod or one of its peers changes and once a minute, creating what is missing and replacing what is of the wrong kind, so a pod that moves to another node, or a link deleted by hand, is rewired without a restart. Each end of a link in a pod is a veth whose other end stays on the node, named `mn<hash>`; the daemon joins the node ends of a link with tc redirects when both pods are on one node, and through a vxlan of the node (`mx<hash>`) when they are not. A pod that goes takes only its own pair: its neighbours keep their interfaces and their configuration, without carrier, until it is back, and their interfaces are the same ones whichever node it comes back on. The carrier a pod sees is set by the daemon on the node end: none while the peer is off or the link is unplugged, the peer's admin state when both are on one node, up across nodes. Node ends are at the largest MTU a veth takes, so the pod ends alone decide the MTU of a link, as with a direct veth. Links added to or removed from a `Topology` are wired or removed on the running pods. What a link declares of an interface (addresses, MAC, MTU, queue length, offloads) is applied when the interface is made and whenever that declaration changes; in between the interface is the workload's, so an address, an MTU or a shut port set by hand inside the pod stays until the `Topology` changes that same property or the pod is recreated. Every interface meshnet wires carries `meshnet/<uid>/<peer>/<digests>` as its alias, the digests being those of the declaration it last applied, and only tagged interfaces are ever deleted, so the workload's own interfaces are safe; likewise the addresses meshnet puts carry their own address protocol (`0x6d`, kernel 5.18 or later; on an older kernel an address dropped from a declaration stays), and only those are ever removed. The error of the last reconcile, if any, is in the pod's `status.plumbing_error`.

`LINK_MTU` on the DaemonSet sets the MTU of links that do not ask for one; without it they get the MTU of the node's interface less the 50 bytes of a vxlan, 1450 on a 1500 network, and `LINK_TXQUEUELEN` the queue length of link ends that do not ask for one (10000 by default).

## Topology

One `Topology` per pod, in the pod's namespace, listing its links. The `uid` identifies the link on both ends and is the VXLAN VNI when the link crosses nodes; the addresses are optional.

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
```

A link whose `peer_pod` is `localhost` is a macvlan on the node interface named by `peer_intf`. The `uid` must be unique on a node across namespaces, since the kernel refuses a second VXLAN with a VNI already in use. The rest is optional and describes the local end, from the pod's own `Topology`: `addresses`, a list of either family beyond `local_ip`; `mac`; `mtu`, where both ends get the larger of the two requests; `txqueuelen`; `offloads: true` to keep the kernel's transmit offloads, switched off otherwise; and `unplugged: true`, the cable pulled, which either end can declare: both pods keep their interfaces administratively up, with their configuration, and lose carrier until it is plugged back, on one node as across nodes. An uplink has no cable to unplug. `tests/` holds topologies for two, three and five nodes, one with several links between the same pair, and the macvlan case.

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
make down
```

The gRPC code under `daemon/proto` is generated and committed; `make proto` regenerates it with buf. Daemon logs come from `kubectl logs -n meshnet -l name=meshnet`; the plugin writes to `/var/log/meshnet-cni.log` on each node.

## License

BSD 3-Clause, see `LICENSE`.
