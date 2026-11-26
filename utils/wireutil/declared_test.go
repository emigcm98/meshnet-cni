package wireutil_test

import (
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/emigcm98/meshnet-cni/utils/wireutil"
	"github.com/safchain/ethtool"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// iface is what a test reads back of an interface in a netns.
type iface struct {
	mac      string
	mtu      int
	txqlen   int
	alias    string
	addrs    map[string]int // CIDR to its address protocol
	offloads bool           // tx-checksum-ip-generic
	tso      bool           // tx-tcp-segmentation
}

// has says whether the interface has address a, whoever put it there.
func (f iface) has(a string) bool {
	_, ok := f.addrs[a]
	return ok
}

// meshnets says whether the interface has address a as meshnet's.
func (f iface) meshnets(a string) bool {
	p, ok := f.addrs[a]
	return ok && p == wireutil.AddrProtocol
}

func readIface(t *testing.T, netNS ns.NetNS, name string) iface {
	t.Helper()
	var got iface
	if err := netNS.Do(func(_ ns.NetNS) error {
		l, err := netlink.LinkByName(name)
		if err != nil {
			return err
		}
		got.mac, got.mtu, got.txqlen, got.alias = l.Attrs().HardwareAddr.String(), l.Attrs().MTU, l.Attrs().TxQLen, l.Attrs().Alias
		addrs, err := netlink.AddrList(l, netlink.FAMILY_ALL)
		if err != nil {
			return err
		}
		got.addrs = map[string]int{}
		for _, a := range addrs {
			if a.IP.IsLinkLocalUnicast() {
				continue
			}
			got.addrs[a.IPNet.String()] = a.Protocol
		}
		h, err := ethtool.NewEthtool()
		if err != nil {
			return err
		}
		defer h.Close()
		features, err := h.Features(name)
		if err != nil {
			return err
		}
		got.offloads, got.tso = features["tx-checksum-ip-generic"], features["tx-tcp-segmentation"]
		return nil
	}); err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return got
}

func boolp(b bool) *bool { return &b }

// in runs cmd in netNS, failing the test on error.
func in(t *testing.T, netNS ns.NetNS, what string, cmd func() error) {
	t.Helper()
	if err := netNS.Do(func(_ ns.NetNS) error { return cmd() }); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func declare(t *testing.T, netNS ns.NetNS, cfg wireutil.PodLinkConfig, mtu int) wireutil.PodEnd {
	t.Helper()
	end := wireutil.NewPodEnd(netNS.Path(), cfg, mtu)
	if _, err := wireutil.EnsurePodEnd(end); err != nil {
		t.Fatalf("EnsurePodEnd(%s): %v", cfg.LocalIntf, err)
	}
	t.Cleanup(func() { _ = wireutil.RemoveHostLink(end.Host) })
	return end
}

// A new interface is born with all of its declaration: both families of addresses, marked as
// meshnet's, the MAC, the MTU, the queue length and the offloads asked for.
func TestDeclareFresh(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges. Run test with sudo")
	}
	netNS := newNS(t)
	cfg := wireutil.PodLinkConfig{PodName: "a", PeerPodName: "b", LinkUID: 1, KubeNs: "decl", LocalIntf: "eth1",
		LocalIP: "10.52.0.1/24", Addresses: []string{"fd52::1/64"}, MAC: "02:52:00:00:00:01", TxQLen: 500, Offloads: boolp(true)}
	declare(t, netNS, cfg, 1400)
	got := readIface(t, netNS, "eth1")
	if got.mac != "02:52:00:00:00:01" || got.mtu != 1400 || got.txqlen != 500 || !got.offloads {
		t.Errorf("eth1 = mac %s, mtu %d, txqueuelen %d, offloads %t; want the declaration", got.mac, got.mtu, got.txqlen, got.offloads)
	}
	for _, a := range []string{"10.52.0.1/24", "fd52::1/64"} {
		if !got.meshnets(a) {
			t.Errorf("address %s: protocol %d (present %t), want meshnet's", a, got.addrs[a], got.has(a))
		}
	}
	if _, _, ok := wireutil.ParseLinkTag(got.alias); !ok || !strings.Contains(got.alias, "u=") {
		t.Errorf("eth1 alias %q, want its tag with the digests", got.alias)
	}

	// Without a declaration of them, the queue length is LINK_TXQUEUELEN and the offloads the kernel's.
	plain := wireutil.PodLinkConfig{PodName: "a", PeerPodName: "b", LinkUID: 2, KubeNs: "decl", LocalIntf: "eth2"}
	declare(t, netNS, plain, 1450)
	if got := readIface(t, netNS, "eth2"); got.txqlen != wireutil.GetLinkTxQLen() || !got.offloads || !got.tso {
		t.Errorf("eth2 = txqueuelen %d, checksum %t, segmentation %t; want %d and the kernel's offloads, on",
			got.txqlen, got.offloads, got.tso, wireutil.GetLinkTxQLen())
	}
}

// A pod end keeps the kernel's tx offloads unless its link says false; dropping false gives them back,
// which is also what an end tagged by an image where absent meant off gets on its first pass, since
// that tag holds the digest of false. In between they are the workload's.
func TestDeclareOffloads(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges. Run test with sudo")
	}
	netNS := newNS(t)
	cfg := wireutil.PodLinkConfig{PodName: "a", PeerPodName: "b", LinkUID: 6, KubeNs: "decl", LocalIntf: "eth1", Offloads: boolp(false)}
	declare(t, netNS, cfg, 1450)
	if got := readIface(t, netNS, "eth1"); got.offloads || got.tso {
		t.Fatalf("offloads: false left checksum %t, segmentation %t; want both off", got.offloads, got.tso)
	}
	cfg.Offloads = nil
	declare(t, netNS, cfg, 1450)
	if got := readIface(t, netNS, "eth1"); !got.offloads || !got.tso {
		t.Fatalf("with offloads dropped: checksum %t, segmentation %t; want the kernel's, on", got.offloads, got.tso)
	}
	in(t, netNS, "the workload switching them off", func() error {
		h, err := ethtool.NewEthtool()
		if err != nil {
			return err
		}
		defer h.Close()
		return h.Change("eth1", map[string]bool{"tx-checksum-ip-generic": false})
	})
	declare(t, netNS, cfg, 1450)
	if got := readIface(t, netNS, "eth1"); got.offloads {
		t.Errorf("an unchanged declaration switched the workload's offloads back on")
	}
	cfg.Offloads = boolp(true)
	declare(t, netNS, cfg, 1450)
	if got := readIface(t, netNS, "eth1"); !got.offloads || !got.tso {
		t.Errorf("offloads: true left checksum %t, segmentation %t; want both on", got.offloads, got.tso)
	}
}

// Between changes of its declaration an interface is the workload's: a change of one property
// applies that one alone, and a change of the addresses removes only meshnet's.
func TestDeclarePerProperty(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges. Run test with sudo")
	}
	netNS := newNS(t)
	cfg := wireutil.PodLinkConfig{PodName: "a", PeerPodName: "b", LinkUID: 3, KubeNs: "decl", LocalIntf: "eth1",
		LocalIP: "10.53.0.1/24", Addresses: []string{"fd53::1/64"}, MAC: "02:53:00:00:00:01"}
	declare(t, netNS, cfg, 1450)

	// The workload adds an address of its own, deletes a declared one and lowers the MTU.
	in(t, netNS, "the workload's changes", func() error {
		l, err := netlink.LinkByName("eth1")
		if err != nil {
			return err
		}
		own, _ := netlink.ParseAddr("192.0.2.9/24")
		if err := netlink.AddrAdd(l, own); err != nil {
			return err
		}
		v6, _ := netlink.ParseAddr("fd53::1/64")
		if err := netlink.AddrDel(l, v6); err != nil {
			return err
		}
		return netlink.LinkSetMTU(l, 1300)
	})

	// The same declaration again changes nothing.
	declare(t, netNS, cfg, 1450)
	got := readIface(t, netNS, "eth1")
	if got.mtu != 1300 || got.has("fd53::1/64") || !got.has("192.0.2.9/24") {
		t.Fatalf("an unchanged declaration undid the workload: mtu %d, addresses %v", got.mtu, got.addrs)
	}

	// A new MAC changes the MAC alone.
	cfg.MAC = "02:53:00:00:00:02"
	declare(t, netNS, cfg, 1450)
	got = readIface(t, netNS, "eth1")
	if got.mac != "02:53:00:00:00:02" || got.mtu != 1300 || got.has("fd53::1/64") {
		t.Fatalf("after a new MAC: mac %s, mtu %d, addresses %v; want the MAC alone changed", got.mac, got.mtu, got.addrs)
	}

	// A new address list applies the list: meshnet's old address goes, the workload's stays.
	cfg.LocalIP, cfg.Addresses = "10.53.0.5/24", []string{"fd53::5/64"}
	declare(t, netNS, cfg, 1450)
	got = readIface(t, netNS, "eth1")
	if got.has("10.53.0.1/24") || !got.meshnets("10.53.0.5/24") || !got.meshnets("fd53::5/64") {
		t.Errorf("after a new list, addresses %v; want 10.53.0.5/24 and fd53::5/64 only of meshnet's", got.addrs)
	}
	if !got.has("192.0.2.9/24") || got.mtu != 1300 {
		t.Errorf("after a new list: the workload's address (%v) or MTU (%d) moved", got.addrs, got.mtu)
	}

	// A new MTU for the link changes the MTU.
	declare(t, netNS, cfg, 1400)
	if got := readIface(t, netNS, "eth1"); got.mtu != 1400 {
		t.Errorf("after a new MTU: %d, want 1400", got.mtu)
	}
}

// An interface tagged by the wiring before declarations had digests is adopted: its declared
// addresses become meshnet's, nothing else is touched, and a later change applies as for any other.
func TestDeclareAdoptsOldTag(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges. Run test with sudo")
	}
	netNS := newNS(t)
	cfg := wireutil.PodLinkConfig{PodName: "a", PeerPodName: "b", LinkUID: 4, KubeNs: "decl", LocalIntf: "eth1", LocalIP: "10.54.0.1/24"}
	declare(t, netNS, cfg, 1450)
	// What the previous wiring left: the old tag, the address without a protocol, and the workload's MTU.
	in(t, netNS, "the previous wiring", func() error {
		l, err := netlink.LinkByName("eth1")
		if err != nil {
			return err
		}
		if err := netlink.LinkSetAlias(l, wireutil.LinkTag(4, "b")); err != nil {
			return err
		}
		a, _ := netlink.ParseAddr("10.54.0.1/24")
		if err := netlink.AddrDel(l, a); err != nil {
			return err
		}
		if err := netlink.AddrAdd(l, a); err != nil {
			return err
		}
		return netlink.LinkSetMTU(l, 1300)
	})

	declare(t, netNS, cfg, 1450)
	got := readIface(t, netNS, "eth1")
	if !got.meshnets("10.54.0.1/24") || got.mtu != 1300 || !strings.Contains(got.alias, "u=") {
		t.Fatalf("after adoption: addresses %v, mtu %d, alias %q; want the address marked and nothing else moved", got.addrs, got.mtu, got.alias)
	}
	cfg.LocalIP = "10.54.0.2/24"
	declare(t, netNS, cfg, 1450)
	if got := readIface(t, netNS, "eth1"); got.has("10.54.0.1/24") || !got.has("10.54.0.2/24") {
		t.Errorf("a change after adoption: addresses %v, want 10.54.0.2/24 in place of 10.54.0.1/24", got.addrs)
	}
}

// Pod ends raised by the workload above the link's MTU carry the larger frames through their node
// ends, as a direct veth would: the node ends never limit a link.
func TestNodeEndsCarryLargerFrames(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges. Run test with sudo")
	}
	nsA, nsB := newNS(t), newNS(t)
	a := ensure(t, nsA, "a", "b", "eth1", "10.55.0.1/30", 5)
	b := ensure(t, nsB, "b", "a", "eth1", "10.55.0.2/30", 5)
	if err := wireutil.JoinHostEnds(a.Host, b.Host); err != nil {
		t.Fatalf("JoinHostEnds: %v", err)
	}
	for _, h := range []string{a.Host, b.Host} {
		if err := wireutil.SetHostEndUp(h, true); err != nil {
			t.Fatalf("SetHostEndUp(%s): %v", h, err)
		}
	}
	for _, n := range []ns.NetNS{nsA, nsB} {
		in(t, n, "raising the MTU", func() error {
			l, err := netlink.LinkByName("eth1")
			if err != nil {
				return err
			}
			return netlink.LinkSetMTU(l, 3000)
		})
	}
	if !udpSizeReaches(t, nsA, nsB, "10.55.0.2", 2900) {
		t.Fatalf("a 2900-byte datagram did not cross pod ends at 3000")
	}
}

// udpSizeReaches is udpReaches with a payload of size bytes and fragmentation forbidden, so it only
// arrives as one frame.
func udpSizeReaches(t *testing.T, from, to ns.NetNS, toIP string, size int) bool {
	t.Helper()
	var conn *net.UDPConn
	in(t, to, "listening", func() error {
		var err error
		conn, err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(toIP), Port: 9998})
		return err
	})
	defer conn.Close()
	got := make(chan bool, 1)
	go func() {
		buf := make([]byte, size+16)
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		n, _, err := conn.ReadFromUDP(buf)
		got <- err == nil && n == size
	}()
	_ = from.Do(func(_ ns.NetNS) error {
		c, err := net.Dial("udp4", toIP+":9998")
		if err != nil {
			return err
		}
		defer c.Close()
		if raw, err := c.(*net.UDPConn).SyscallConn(); err == nil {
			_ = raw.Control(func(fd uintptr) { setDontFragment(int(fd)) })
		}
		payload := make([]byte, size)
		for i := 0; i < 10; i++ {
			_, _ = c.Write(payload)
			time.Sleep(100 * time.Millisecond)
		}
		return nil
	})
	return <-got
}

func setDontFragment(fd int) {
	_ = unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_MTU_DISCOVER, unix.IP_PMTUDISC_DO)
}
