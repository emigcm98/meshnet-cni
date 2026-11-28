package wireutil

import (
	"crypto/sha256"
	"net"
)

// DerivedMAC is the MAC a link end is born with when its link declares none: the first six bytes of
// the SHA-256 of the cluster's identity, the namespace, the pod and the interface joined by "/", made
// unicast and locally administered. A pod made again, on its node or on another, so comes back with
// the MACs its neighbours know, and an uplink's stays its own on a segment that clusters share.
// Without a cluster identity it is empty, for the kernel's random one. The formula is a contract:
// changing it changes every MAC at its next birth.
func DerivedMAC(cluster, kubeNs, pod, intf string) string {
	if cluster == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(cluster + "/" + kubeNs + "/" + pod + "/" + intf))
	mac := net.HardwareAddr(sum[:6])
	mac[0] = mac[0]&0xfe | 0x02
	return mac.String()
}
