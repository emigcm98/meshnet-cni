# meshnet CNI

**meshnet** is a Kubernetes CNI plugin that wires point-to-point links between pods from a topology stored in the cluster. Two pods on the same node get a veth pair, two pods on different nodes get a VXLAN tunnel, and a pod can be attached to an external device through a macvlan interface. Every pod keeps its first interface from the cluster's primary CNI; meshnet chains itself after it and only adds interfaces to pods that have a `Topology` resource.

This is the fork that [KubeNDT](https://github.com/emigcm98/kubendt) runs. It descends from the archived [networkop/meshnet-cni](https://github.com/networkop/meshnet-cni) (BSD 3-Clause, Michael Kashin) and takes pieces from the fork maintained inside [openconfig/kne](https://github.com/openconfig/kne/tree/main/third_party/meshnet) (Apache 2.0), each attributed in the commit that brings it. The CNI binary, the NetworkAttachmentDefinition and the CRD keep their original names, so topologies written for the original line work unchanged.

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
```

A link whose `peer_pod` is `localhost` is a macvlan on the node interface named by `peer_intf`. `tests/` holds topologies for two, three and five nodes, one with several links between the same pair, and the macvlan case.

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
make up          # a local kind cluster of one control plane and three workers
make install     # loads the image into it and applies manifests/overlays/e2e
make e2e         # the three-node topology of tests/3node.yml and pings over its links
make eviction    # tests/eviction.sh: a pod recreated on its peer's worker and back, a ping per step
make down
```

The gRPC code under `daemon/proto` is generated and committed; `make proto` regenerates it with buf. Daemon logs come from `kubectl logs -n meshnet -l name=meshnet`; the plugin writes to `/var/log/meshnet-cni.log` on each node.

## License

BSD 3-Clause, see `LICENSE`.
