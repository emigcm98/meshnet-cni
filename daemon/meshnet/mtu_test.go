// Ported from openconfig/kne, third_party/meshnet/daemon/meshnet/mtu_test.go at 5a1676c (Apache 2.0), without the gRPC wire case.

package meshnet

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// withLinkMTU sets the mtu of the link with the given UID in topo.
func withLinkMTU(t *testing.T, topo *unstructured.Unstructured, uid int64, mtu int64) *unstructured.Unstructured {
	t.Helper()
	links, _, _ := unstructured.NestedSlice(topo.Object, "spec", "links")
	for _, l := range links {
		lm := l.(map[string]interface{})
		if lm["uid"] == uid {
			lm["mtu"] = mtu
		}
	}
	if err := unstructured.SetNestedSlice(topo.Object, links, "spec", "links"); err != nil {
		t.Fatalf("SetNestedSlice failed: %v", err)
	}
	return topo
}

func TestParsePodLinks_MTU(t *testing.T) {
	pod := createFakePodTopology("p1", "default", "10.0.0.1", "/proc/1/ns/net", []string{"p2", "p3"})
	withLinkMTU(t, pod, 2, 9216)
	links, err := parsePodLinks(pod)
	if err != nil {
		t.Fatalf("parsePodLinks failed: %v", err)
	}
	if len(links) != 2 {
		t.Fatalf("expected 2 links, got %d", len(links))
	}
	if links[0].MTU != 0 {
		t.Errorf("MTU of link without mtu = %d, want 0", links[0].MTU)
	}
	if links[1].MTU != 9216 {
		t.Errorf("MTU of link with mtu = %d, want 9216", links[1].MTU)
	}
}

func TestPeerLinkMTU(t *testing.T) {
	peer := createFakePodTopology("p2", "default", "10.0.0.2", "/proc/1/ns/net", []string{"p1", "p3"})
	withLinkMTU(t, peer, 1, 9500)
	tests := []struct {
		desc string
		topo *unstructured.Unstructured
		uid  int64
		want int
	}{
		{desc: "set", topo: peer, uid: 1, want: 9500},
		{desc: "unset", topo: peer, uid: 2, want: 0},
		{desc: "missing link", topo: peer, uid: 7, want: 0},
		{desc: "nil topology", uid: 1, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			if got := peerLinkMTU(tt.topo, tt.uid); got != tt.want {
				t.Errorf("peerLinkMTU(uid %d) = %d, want %d", tt.uid, got, tt.want)
			}
		})
	}
}
