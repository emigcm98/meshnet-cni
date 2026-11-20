// Ported from openconfig/kne, third_party/meshnet/daemon/meshnet/mtu.go at 5a1676c (Apache 2.0), without the gRPC wires.

package meshnet

import (
	"github.com/emigcm98/meshnet-cni/utils/wireutil"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// findPodLink returns the link with the given UID in the spec of topo.
func findPodLink(topo *unstructured.Unstructured, uid int64) (wireutil.PodLinkConfig, bool) {
	links, _ := parsePodLinks(topo)
	for _, l := range links {
		if l.LinkUID == uid {
			return l, true
		}
	}
	return wireutil.PodLinkConfig{}, false
}

// peerLinkMTU returns the MTU that the peer pod's Topology requests for the
// link with the given UID, or zero if it requests none.
func peerLinkMTU(peerTopo *unstructured.Unstructured, uid int64) int {
	if l, ok := findPodLink(peerTopo, uid); ok {
		return l.MTU
	}
	return 0
}
