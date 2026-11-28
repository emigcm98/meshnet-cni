package wireutil_test

import (
	"net"
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

	// A macvlan as the kernel makes it on this parent, to compare with.
	refNs, err := testutils.NewNS()
	if err != nil {
		t.Fatalf("creating the netns: %v", err)
	}
	defer refNs.Close()
	if err := netlink.LinkAdd(&netlink.Macvlan{LinkAttrs: netlink.LinkAttrs{Name: "mvref0", ParentIndex: parentIndex(t, parent), Namespace: netlink.NsFd(int(refNs.Fd()))},
		Mode: netlink.MACVLAN_MODE_BRIDGE}); err != nil {
		t.Fatalf("adding the reference macvlan: %v", err)
	}
	kernels := readIface(t, refNs, "mvref0")

	cfg := wireutil.PodLinkConfig{LinkUID: 9, PeerPodName: "localhost", LocalIntf: "eth1", LocalIP: "192.0.2.1/24"}
	created, err := wireutil.EnsureUplink(podNs.Path(), "eth1", parent, 9, "localhost", wireutil.NewUplinkDeclared(cfg))
	if err != nil || !created {
		t.Fatalf("EnsureUplink: created %t, %v", created, err)
	}
	// What an uplink does not declare stays the kernel's.
	if got := readIface(t, podNs, "eth1"); got.offloads != kernels.offloads || got.txqlen != kernels.txqlen || got.mtu != kernels.mtu {
		t.Errorf("uplink eth1 = offloads %t, txqueuelen %d, mtu %d; want the kernel's %t, %d, %d",
			got.offloads, got.txqlen, got.mtu, kernels.offloads, kernels.txqlen, kernels.mtu)
	}
	// A macvlan's offloads follow its parent, which the kernel does not let a macvlan on a dummy
	// change, so declaring them off and dropping that again only has to go through.
	off := cfg
	off.Offloads = boolp(false)
	for _, d := range []wireutil.PodLinkConfig{off, cfg} {
		if err := wireutil.Declare(podNs.Path(), "eth1", 9, "localhost", wireutil.NewUplinkDeclared(d), false); err != nil {
			t.Fatalf("Declare(offloads %v): %v", d.Offloads, err)
		}
	}
	kind, exists, err := wireutil.LinkInfo(podNs.Path(), "eth1")
	if err != nil || !exists || kind != "macvlan" {
		t.Fatalf("after EnsureUplink: kind %q, exists %t, err %v", kind, exists, err)
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
	if created, err := wireutil.EnsureUplink(podNs.Path(), "eth1", parent, 9, "localhost", wireutil.NewUplinkDeclared(cfg)); err != nil || created {
		t.Fatalf("second EnsureUplink must leave the existing one alone: created %t, %v", created, err)
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

func parentIndex(t *testing.T, parent string) int {
	t.Helper()
	l, err := netlink.LinkByName(parent)
	if err != nil {
		t.Fatalf("finding %s: %v", parent, err)
	}
	return l.Attrs().Index
}

// An uplink that declares no MAC is born with its default one, and up. A second one with that MAC on
// the same parent, which is what a pod made again asks for while the sandbox it had is still there, is
// refused by the kernel: the pass fails, leaves nothing behind and says the same each time, and the
// next one with the first gone makes it.
func TestUplinkDefaultMAC(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges. Run test with sudo")
	}
	parent := "mvtest1"
	if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: parent}}); err != nil {
		t.Fatalf("adding the dummy parent: %v", err)
	}
	defer func() {
		if l, err := netlink.LinkByName(parent); err == nil {
			_ = netlink.LinkDel(l)
		}
	}()
	if l, err := netlink.LinkByName(parent); err != nil || netlink.LinkSetUp(l) != nil {
		t.Fatalf("setting the parent up: %v", err)
	}
	oldNs, newNs := newNS(t), newNS(t)
	const derived = "02:55:00:00:00:01"
	cfg := wireutil.PodLinkConfig{LinkUID: 10, PeerPodName: "localhost", LocalIntf: "eth1", LocalIP: "192.0.2.1/24", DefaultMAC: derived}
	uplink := func(netNS ns.NetNS) (bool, error) {
		return wireutil.EnsureUplink(netNS.Path(), "eth1", parent, 10, "localhost", wireutil.NewUplinkDeclared(cfg))
	}
	names := func(netNS ns.NetNS) []string {
		var out []string
		in(t, netNS, "listing the links", func() error {
			links, err := netlink.LinkList()
			for _, l := range links {
				if l.Attrs().Name != "lo" {
					out = append(out, l.Attrs().Name)
				}
			}
			return err
		})
		return out
	}

	if created, err := uplink(oldNs); err != nil || !created {
		t.Fatalf("EnsureUplink: created %t, %v", created, err)
	}
	got := readIface(t, oldNs, "eth1")
	up := false
	in(t, oldNs, "reading the uplink", func() error {
		l, err := netlink.LinkByName("eth1")
		if err == nil {
			up = l.Attrs().Flags&net.FlagUp != 0
		}
		return err
	})
	if got.mac != derived || !up || !got.has("192.0.2.1/24") {
		t.Fatalf("uplink eth1 = mac %s, up %t, addresses %v; want %s, up, with its address", got.mac, up, got.addrs, derived)
	}

	created, refused := uplink(newNs)
	if refused == nil || created {
		t.Fatalf("a second uplink with the MAC of one that is up was made: created %t, %v", created, refused)
	}
	if left := names(newNs); len(left) != 0 {
		t.Fatalf("the refused uplink left %v behind", left)
	}
	// Refused again, it says the same: an error that changed with every pass would be written to the
	// status every pass, and each write brings the next pass at once.
	if _, again := uplink(newNs); again == nil || again.Error() != refused.Error() {
		t.Fatalf("the second refusal reads %q, the first %q", again, refused)
	}
	if err := wireutil.RemoveLink(oldNs.Path(), "eth1"); err != nil {
		t.Fatalf("removing the first uplink: %v", err)
	}
	if created, err := uplink(newNs); err != nil || !created {
		t.Fatalf("EnsureUplink with the first gone: created %t, %v", created, err)
	}
	if got := readIface(t, newNs, "eth1"); got.mac != derived {
		t.Errorf("uplink eth1 made again has %s, want %s", got.mac, derived)
	}
}
