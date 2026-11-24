// Ported from openconfig/kne, third_party/meshnet/utils/wireutil/mtu_test.go at 5a1676c (Apache 2.0).

package wireutil_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	"github.com/emigcm98/meshnet-cni/utils/wireutil"
	"github.com/vishvananda/netlink"
)

func TestLinkMTU(t *testing.T) {
	tests := []struct {
		desc    string
		a, b    int
		linkMTU string
		want    int
	}{
		{desc: "unset", want: 0},
		{desc: "one end", a: 9216, want: 9216},
		{desc: "other end", b: 9216, want: 9216},
		{desc: "larger wins", a: 1500, b: 9500, want: 9500},
		{desc: "equal", a: 9000, b: 9000, want: 9000},
		{desc: "LINK_MTU when unset", linkMTU: "9500", want: 9500},
		{desc: "request overrides LINK_MTU", a: 1500, linkMTU: "9500", want: 1500},
		{desc: "invalid LINK_MTU", linkMTU: "jumbo", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			t.Setenv("LINK_MTU", tt.linkMTU)
			if got := wireutil.LinkMTU(tt.a, tt.b); got != tt.want {
				t.Errorf("LinkMTU(%d, %d) with LINK_MTU=%q = %d, want %d", tt.a, tt.b, tt.linkMTU, got, tt.want)
			}
		})
	}
}

func TestSetLinkMTU(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges. Run test with sudo")
	}
	podNS, err := testutils.NewNS()
	if err != nil {
		t.Fatalf("Failed to create netns: %v", err)
	}
	defer func() { _ = podNS.Close() }()

	const ifName = "eth1"
	mtuOf := func() (int, error) {
		var mtu int
		err := podNS.Do(func(_ ns.NetNS) error {
			l, err := netlink.LinkByName(ifName)
			if err != nil {
				return err
			}
			mtu = l.Attrs().MTU
			return nil
		})
		return mtu, err
	}
	if err := podNS.Do(func(_ ns.NetNS) error {
		return netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: ifName, MTU: 1500}})
	}); err != nil {
		t.Fatalf("Failed to create %s: %v", ifName, err)
	}

	for _, step := range []struct {
		mtu  int
		want int
	}{
		{mtu: 9500, want: 9500},
		{mtu: 9500, want: 9500}, // unchanged
		{mtu: 0, want: 9500},    // zero leaves the MTU alone
		{mtu: 1500, want: 1500},
	} {
		if err := wireutil.SetLinkMTU(podNS.Path(), ifName, step.mtu); err != nil {
			t.Fatalf("SetLinkMTU(%d) failed: %v", step.mtu, err)
		}
		got, err := mtuOf()
		if err != nil {
			t.Fatalf("Failed to read MTU of %s: %v", ifName, err)
		}
		if got != step.want {
			t.Errorf("After SetLinkMTU(%d), MTU = %d, want %d", step.mtu, got, step.want)
		}
	}

	if err := wireutil.SetLinkMTU(podNS.Path(), "missing0", 9000); err == nil {
		t.Errorf("SetLinkMTU on a missing interface succeeded, want error")
	}
}

func TestEnsurePodEndMTU(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges. Run test with sudo")
	}
	podNS, err := testutils.NewNS()
	if err != nil {
		t.Fatalf("Failed to create the netns: %v", err)
	}
	defer func() { _ = podNS.Close() }()

	tests := []struct {
		uid  int64
		mtu  int
		want int
	}{
		{uid: 1, mtu: 0, want: 1500},
		{uid: 2, mtu: 1450, want: 1450},
		{uid: 3, mtu: 9500, want: 9500},
	}
	for _, tt := range tests {
		cfg := wireutil.PodLinkConfig{PodName: "p1", PeerPodName: "p2", LinkUID: tt.uid, KubeNs: "mtu", LocalIntf: fmt.Sprintf("eth%d", tt.uid)}
		end := wireutil.NewPodEnd(podNS.Path(), cfg, tt.mtu)
		if _, err := wireutil.EnsurePodEnd(end); err != nil {
			t.Fatalf("EnsurePodEnd(%s) failed: %v", cfg.LocalIntf, err)
		}
		defer func() { _ = wireutil.RemoveHostLink(end.Host) }()
		if err := podNS.Do(func(_ ns.NetNS) error {
			l, err := netlink.LinkByName(cfg.LocalIntf)
			if err != nil {
				return err
			}
			if got := l.Attrs().MTU; got != tt.want {
				t.Errorf("MTU of %s = %d, want %d", cfg.LocalIntf, got, tt.want)
			}
			return nil
		}); err != nil {
			t.Fatalf("Failed to inspect %s: %v", cfg.LocalIntf, err)
		}
	}
}
