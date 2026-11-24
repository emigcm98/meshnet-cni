package wireutil_test

import (
	"os"
	"strings"
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

	created, err := wireutil.MakeMacvlan(podNs.Path(), "eth1", parent)
	if err != nil || !created {
		t.Fatalf("MakeMacvlan: created %t, %v", created, err)
	}
	kernels := readIface(t, podNs, "eth1")
	cfg := wireutil.PodLinkConfig{LinkUID: 9, PeerPodName: "localhost", LocalIntf: "eth1", LocalIP: "192.0.2.1/24"}
	if err := wireutil.Declare(podNs.Path(), "eth1", 9, "localhost", wireutil.NewUplinkDeclared(cfg), true); err != nil {
		t.Fatalf("Declare: %v", err)
	}
	// What an uplink does not declare stays the kernel's.
	if got := readIface(t, podNs, "eth1"); got.offloads != kernels.offloads || got.txqlen != kernels.txqlen || got.mtu != kernels.mtu {
		t.Errorf("uplink eth1 = offloads %t, txqueuelen %d, mtu %d; want the kernel's %t, %d, %d",
			got.offloads, got.txqlen, got.mtu, kernels.offloads, kernels.txqlen, kernels.mtu)
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
	if created, err := wireutil.MakeMacvlan(podNs.Path(), "eth1", parent); err != nil || created {
		t.Fatalf("second MakeMacvlan must leave the existing one alone: created %t, %v", created, err)
	}

	// An uplink has no node end: its shaping goes on the interface in the pod, and comes off with it.
	rootKind := func() string {
		kind := ""
		in(t, podNs, "reading the uplink's qdisc", func() error {
			l, err := netlink.LinkByName("eth1")
			if err != nil {
				return err
			}
			qdiscs, err := netlink.QdiscList(l)
			for _, q := range qdiscs {
				if q.Attrs().Parent == netlink.HANDLE_ROOT && q.Type() != "noqueue" {
					kind = q.Type()
				}
			}
			return err
		})
		return kind
	}
	cfg.Shaping = &wireutil.Shaping{Delay: "10ms"}
	if err := wireutil.Declare(podNs.Path(), "eth1", 9, "localhost", wireutil.NewUplinkDeclared(cfg), false); err != nil {
		t.Fatalf("Declare with a shaping: %v", err)
	}
	if got := rootKind(); got != "netem" {
		t.Errorf("uplink eth1 root qdisc %q, want the declared netem", got)
	}
	cfg.Shaping = nil
	if err := wireutil.Declare(podNs.Path(), "eth1", 9, "localhost", wireutil.NewUplinkDeclared(cfg), false); err != nil {
		t.Fatalf("Declare without a shaping: %v", err)
	}
	if got := rootKind(); got != "" {
		t.Errorf("uplink eth1 keeps %q with its shaping dropped", got)
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

func TestLinkTag(t *testing.T) {
	tag := wireutil.LinkTag(4242, "r2")
	uid, peer, ok := wireutil.ParseLinkTag(tag)
	if !ok || uid != 4242 || peer != "r2" {
		t.Fatalf("ParseLinkTag(%q) = %d, %q, %t", tag, uid, peer, ok)
	}
	with := wireutil.LinkTagWith(4242, "r2", wireutil.Declared{MTU: 1450}.Digests())
	if uid, peer, ok := wireutil.ParseLinkTag(with); !ok || uid != 4242 || peer != "r2" {
		t.Fatalf("ParseLinkTag(%q) = %d, %q, %t", with, uid, peer, ok)
	}
	if long := wireutil.LinkTagWith(1, strings.Repeat("p", 240), wireutil.Declared{}.Digests()); long != wireutil.LinkTag(1, strings.Repeat("p", 240)) {
		t.Errorf("a tag too long for an alias must go without its digests, got %d characters", len(long))
	}
	for _, alias := range []string{"", "uplink to the lab", "meshnet/", "meshnet/x/r2", "meshnet/12"} {
		if _, _, ok := wireutil.ParseLinkTag(alias); ok {
			t.Errorf("ParseLinkTag(%q) took a foreign alias for a tag", alias)
		}
	}
}
