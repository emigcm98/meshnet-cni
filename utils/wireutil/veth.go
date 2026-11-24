// The link description and the offloads switched off are from openconfig/kne,
// third_party/meshnet/utils/wireutil/veth.go at 5a1676c (Apache 2.0); the wiring itself is hostends.go.

package wireutil

import (
	"github.com/safchain/ethtool"
)

// PodLinkConfig describes a single interface to be configured inside a pod network namespace.
type PodLinkConfig struct {
	PodName     string // Local pod name (e.g. "p1")
	PeerPodName string // Peer pod name (e.g. "p2")
	LinkUID     int64  // Unique link ID in topology (e.g. 14)
	KubeNs      string // Kubernetes namespace (e.g. "1500links")
	LocalIntf   string // Target interface name inside local pod (e.g. "eth14")
	LocalIP     string // Local CIDR (e.g. "10.10.0.1/30")
	PeerIntf    string // Target interface name inside peer pod (e.g. "eth14")
	PeerIP      string // Peer CIDR (e.g. "10.10.0.2/30")
	MTU         int    // Interface MTU, zero when the link asks for none
}

var txOffloadDisabledMap = map[string]bool{
	"tx-checksum-ipv4":             false,
	"tx-checksum-ipv6":             false,
	"tx-checksum-ip-generic":       false,
	"tx-tcp-segmentation":          false,
	"tx-tcp6-segmentation":         false,
	"tx-checksum-fcoe-crc":         false,
	"tx-checksum-sctp":             false,
	"tx-tcp-ecn-segmentation":      false,
	"tx-tcp-mangleid-segmentation": false,
}

func disableTxOffload(etlHndl *ethtool.Ethtool, intfName string) error {
	if etlHndl == nil {
		return nil
	}
	return etlHndl.Change(intfName, txOffloadDisabledMap)
}
