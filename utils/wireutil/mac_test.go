package wireutil_test

import (
	"net"
	"testing"

	"github.com/emigcm98/meshnet-cni/utils/wireutil"
)

// The derived MAC is a contract: these do not change, or every link end changes its MAC at its next
// birth. Each of the cluster, the namespace, the pod and the interface makes it another, and it is
// always unicast and locally administered.
func TestDerivedMAC(t *testing.T) {
	const c1, c2 = "4f2b9d6e-1c1a-4b55-9a57-0d2f6f2b7c11", "9d0c6a52-77f3-4e0b-8a3e-5b1f0f4f8a02"
	for _, tc := range []struct{ cluster, ns, pod, intf, want string }{
		{c1, "lab", "r1", "eth1", "36:58:d6:a0:80:bd"},
		{c1, "lab", "r1", "eth2", "6e:ef:90:97:76:29"},
		{c1, "lab", "r2", "eth1", "a2:9c:dc:8f:a5:6c"},
		{c1, "lab2", "r1", "eth1", "5a:a9:fc:a2:af:1e"},
		{c2, "lab", "r1", "eth1", "16:2c:f9:2c:91:50"},
	} {
		got := wireutil.DerivedMAC(tc.cluster, tc.ns, tc.pod, tc.intf)
		if got != tc.want {
			t.Errorf("DerivedMAC(%s, %s, %s, %s) = %s, want %s", tc.cluster, tc.ns, tc.pod, tc.intf, got, tc.want)
		}
		mac, err := net.ParseMAC(got)
		if err != nil || mac[0]&0x01 != 0 || mac[0]&0x02 == 0 {
			t.Errorf("%s (%v) is not a unicast, locally administered MAC", got, err)
		}
	}
	// Without the cluster's identity there is none, and the end keeps the kernel's.
	if got := wireutil.DerivedMAC("", "lab", "r1", "eth1"); got != "" {
		t.Errorf("DerivedMAC without a cluster = %q, want none", got)
	}
}
