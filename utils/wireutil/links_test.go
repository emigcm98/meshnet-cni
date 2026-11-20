package wireutil_test

import (
	"os"
	"testing"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	"github.com/emigcm98/meshnet-cni/utils/wireutil"
	"github.com/vishvananda/netlink"
)

// A macvlan hung on a dummy of the host, moved into a netns, seen by LinkInfo, left alone by a
// second call, and gone after RemoveLink, which also accepts a name that is not there.
func TestMacvlanLifecycle(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges. Run test with sudo")
	}
	parent := "mvtest0"
	if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: parent}}); err != nil {
		t.Fatalf("adding the dummy parent: %v", err)
	}
	defer func() {
		if l, err := netlink.LinkByName(parent); err == nil {
			_ = netlink.LinkDel(l)
		}
	}()
	podNs, err := testutils.NewNS()
	if err != nil {
		t.Fatalf("creating the netns: %v", err)
	}
	defer podNs.Close()

	if err := wireutil.MakeMacvlan(podNs.Path(), "eth1", parent, "192.0.2.1/24"); err != nil {
		t.Fatalf("MakeMacvlan: %v", err)
	}
	kind, exists, err := wireutil.LinkInfo(podNs.Path(), "eth1")
	if err != nil || !exists || kind != "macvlan" {
		t.Fatalf("after MakeMacvlan: kind %q, exists %t, err %v", kind, exists, err)
	}
	if err := podNs.Do(func(_ ns.NetNS) error {
		l, err := netlink.LinkByName("eth1")
		if err != nil {
			return err
		}
		addrs, err := netlink.AddrList(l, netlink.FAMILY_V4)
		if err != nil || len(addrs) != 1 || addrs[0].IPNet.String() != "192.0.2.1/24" {
			t.Errorf("address on eth1: %v, %v", addrs, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("inspecting eth1: %v", err)
	}
	if err := wireutil.MakeMacvlan(podNs.Path(), "eth1", parent, "192.0.2.1/24"); err != nil {
		t.Fatalf("second MakeMacvlan must leave the existing one alone: %v", err)
	}

	if err := wireutil.RemoveLink(podNs.Path(), "eth1"); err != nil {
		t.Fatalf("RemoveLink: %v", err)
	}
	if _, exists, err := wireutil.LinkInfo(podNs.Path(), "eth1"); err != nil || exists {
		t.Fatalf("after RemoveLink: exists %t, err %v", exists, err)
	}
	if err := wireutil.RemoveLink(podNs.Path(), "eth1"); err != nil {
		t.Fatalf("RemoveLink of a missing link must be fine: %v", err)
	}
}
