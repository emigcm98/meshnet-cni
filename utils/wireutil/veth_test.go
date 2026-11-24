package wireutil_test

import (
	"net"
	"os"
	"testing"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	"github.com/emigcm98/meshnet-cni/utils/wireutil"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestHostEndNames(t *testing.T) {
	a := wireutil.HostEndName("ns", "r1", 7)
	if a != wireutil.HostEndName("ns", "r1", 7) {
		t.Fatalf("HostEndName is not deterministic")
	}
	for _, other := range []string{wireutil.HostEndName("ns", "r2", 7), wireutil.HostEndName("ns", "r1", 8), wireutil.HostEndName("other", "r1", 7)} {
		if other == a {
			t.Fatalf("two different link ends got the same node end %s", a)
		}
	}
	vx := wireutil.HostVxlanName("ns", "r1", 7)
	if !wireutil.IsHostEnd(a) || wireutil.IsHostEnd(vx) || !wireutil.IsHostVxlan(vx) || wireutil.HostEndOfVxlan(vx) != a {
		t.Fatalf("names %s and %s are not told apart or matched", a, vx)
	}
	if len(a) > unix.IFNAMSIZ-1 || len(vx) > unix.IFNAMSIZ-1 {
		t.Fatalf("names %s and %s do not fit an interface name", a, vx)
	}
}

func linkFlags(t *testing.T, netNS ns.NetNS, name string) uint32 {
	t.Helper()
	var flags uint32
	if err := netNS.Do(func(_ ns.NetNS) error {
		l, err := netlink.LinkByName(name)
		if err != nil {
			return err
		}
		flags = l.Attrs().RawFlags
		return nil
	}); err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return flags
}

func linkIndex(t *testing.T, netNS ns.NetNS, name string) int {
	t.Helper()
	index := 0
	_ = netNS.Do(func(_ ns.NetNS) error {
		if l, err := netlink.LinkByName(name); err == nil {
			index = l.Attrs().Index
		}
		return nil
	})
	return index
}

func setAdmin(t *testing.T, netNS ns.NetNS, name string, up bool) {
	t.Helper()
	if err := netNS.Do(func(_ ns.NetNS) error {
		l, err := netlink.LinkByName(name)
		if err != nil {
			return err
		}
		if up {
			return netlink.LinkSetUp(l)
		}
		return netlink.LinkSetDown(l)
	}); err != nil {
		t.Fatalf("setting %s admin up=%t: %v", name, up, err)
	}
}

// udpReaches says whether a datagram sent from the netns from to toIP arrives in the netns to.
func udpReaches(t *testing.T, from, to ns.NetNS, toIP string) bool {
	t.Helper()
	var conn *net.UDPConn
	if err := to.Do(func(_ ns.NetNS) error {
		var err error
		conn, err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(toIP), Port: 9999})
		return err
	}); err != nil {
		t.Fatalf("listening on %s: %v", toIP, err)
	}
	defer conn.Close()
	got := make(chan bool, 1)
	go func() {
		buf := make([]byte, 16)
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, _, err := conn.ReadFromUDP(buf)
		got <- err == nil
	}()
	_ = from.Do(func(_ ns.NetNS) error {
		c, err := net.Dial("udp4", toIP+":9999")
		if err != nil {
			return err
		}
		defer c.Close()
		// The first ones may go while the neighbour is being resolved.
		for i := 0; i < 10; i++ {
			_, _ = c.Write([]byte("hello"))
			time.Sleep(100 * time.Millisecond)
		}
		return nil
	})
	return <-got
}

func newNS(t *testing.T) ns.NetNS {
	t.Helper()
	n, err := testutils.NewNS()
	if err != nil {
		t.Fatalf("creating a netns: %v", err)
	}
	t.Cleanup(func() { _ = n.Close() })
	return n
}

func ensure(t *testing.T, netNS ns.NetNS, pod, peer, intf, ip string, uid int64) wireutil.PodEnd {
	t.Helper()
	cfg := wireutil.PodLinkConfig{PodName: pod, PeerPodName: peer, LinkUID: uid, KubeNs: "ends", LocalIntf: intf, LocalIP: ip}
	end := wireutil.NewPodEnd(netNS.Path(), cfg, 1450)
	if _, err := wireutil.EnsurePodEnd(end); err != nil {
		t.Fatalf("EnsurePodEnd(%s/%s): %v", pod, intf, err)
	}
	t.Cleanup(func() { _ = wireutil.RemoveHostLink(end.Host) })
	return end
}

// A pod end is made once, paired with its node end, kept on later calls, and made again when the
// name is held by something else: a direct pair to another pod (the previous wiring) or a leftover.
func TestEnsurePodEnd(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges. Run test with sudo")
	}
	nsA, nsB := newNS(t), newNS(t)

	end := ensure(t, nsA, "a", "b", "eth1", "10.50.0.1/30", 1)
	first := linkIndex(t, nsA, "eth1")
	if first == 0 || !wireutil.HostLinkExists(end.Host) {
		t.Fatalf("after the first call: eth1 ifindex %d, node end %s present %t", first, end.Host, wireutil.HostLinkExists(end.Host))
	}
	if got := linkFlags(t, nsA, "eth1") & unix.IFF_UP; got == 0 {
		t.Errorf("a new pod end must come up")
	}
	if got := linkFlags(t, nsA, "eth1") & unix.IFF_LOWER_UP; got != 0 {
		t.Errorf("a pod end must have no carrier while its node end is down")
	}
	ensure(t, nsA, "a", "b", "eth1", "10.50.0.1/30", 1)
	if got := linkIndex(t, nsA, "eth1"); got != first {
		t.Errorf("a second call remade eth1 (ifindex %d, was %d)", got, first)
	}

	// The workload shuts the port; a later call must leave it down.
	setAdmin(t, nsA, "eth1", false)
	ensure(t, nsA, "a", "b", "eth1", "10.50.0.1/30", 1)
	if got := linkFlags(t, nsA, "eth1") & unix.IFF_UP; got != 0 {
		t.Errorf("a call brought up a port the workload had shut")
	}

	// The previous wiring: a direct pair between the two pods.
	if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "oldx"}, PeerName: "oldy"}); err != nil {
		t.Fatalf("making the old pair: %v", err)
	}
	for name, target := range map[string]ns.NetNS{"oldx": nsA, "oldy": nsB} {
		l, _ := netlink.LinkByName(name)
		if err := netlink.LinkSetNsFd(l, int(target.Fd())); err != nil {
			t.Fatalf("moving %s: %v", name, err)
		}
		if err := target.Do(func(_ ns.NetNS) error {
			l, err := netlink.LinkByName(name)
			if err != nil {
				return err
			}
			return netlink.LinkSetName(l, "eth2")
		}); err != nil {
			t.Fatalf("renaming %s: %v", name, err)
		}
	}
	end2 := ensure(t, nsA, "a", "b", "eth2", "", 2)
	if v, err := netlink.LinkByName(end2.Host); err != nil {
		t.Fatalf("node end of eth2: %v", err)
	} else if peer, err := netlink.VethPeerIndex(v.(*netlink.Veth)); err != nil || peer != linkIndex(t, nsA, "eth2") {
		t.Errorf("eth2 is not paired with its node end (peer %d, eth2 %d, %v)", peer, linkIndex(t, nsA, "eth2"), err)
	}
	if linkIndex(t, nsB, "eth2") != 0 {
		t.Errorf("the other end of the old pair is still in b")
	}

	// A leftover of another kind under the name.
	if err := nsA.Do(func(_ ns.NetNS) error {
		return netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth3"}})
	}); err != nil {
		t.Fatalf("planting a leftover: %v", err)
	}
	ensure(t, nsA, "a", "b", "eth3", "", 3)
	var kind string
	_ = nsA.Do(func(_ ns.NetNS) error {
		l, err := netlink.LinkByName("eth3")
		if err == nil {
			kind = l.Type()
		}
		return nil
	})
	if kind != "veth" {
		t.Errorf("eth3 is %q, want the leftover replaced by a veth", kind)
	}
}

// Two pod ends on one node joined through their node ends carry frames, and each pod's carrier is
// its node end's admin state, which is what the daemon sets.
func TestJoinHostEnds(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges. Run test with sudo")
	}
	nsA, nsB := newNS(t), newNS(t)
	a := ensure(t, nsA, "a", "b", "eth1", "10.51.0.1/30", 1)
	b := ensure(t, nsB, "b", "a", "eth1", "10.51.0.2/30", 1)
	if err := wireutil.JoinHostEnds(a.Host, b.Host); err != nil {
		t.Fatalf("JoinHostEnds: %v", err)
	}
	for _, h := range []string{a.Host, b.Host} {
		if err := wireutil.SetHostEndUp(h, true); err != nil {
			t.Fatalf("SetHostEndUp(%s): %v", h, err)
		}
	}
	if got := linkFlags(t, nsA, "eth1") & unix.IFF_LOWER_UP; got == 0 {
		t.Fatalf("a eth1 has no carrier with its node end up")
	}
	if !udpReaches(t, nsA, nsB, "10.51.0.2") {
		t.Fatalf("a datagram from a did not reach b through the joined node ends")
	}
	if !udpReaches(t, nsB, nsA, "10.51.0.1") {
		t.Fatalf("a datagram from b did not reach a through the joined node ends")
	}
	if err := wireutil.SetHostEndUp(a.Host, false); err != nil {
		t.Fatalf("SetHostEndUp(down): %v", err)
	}
	if got := linkFlags(t, nsA, "eth1") & unix.IFF_LOWER_UP; got != 0 {
		t.Errorf("a eth1 keeps its carrier with its node end down")
	}
	if !wireutil.PodEndAdminUp(nsA.Path(), "eth1") {
		t.Errorf("a eth1 must stay admin up, only its carrier goes")
	}
}
