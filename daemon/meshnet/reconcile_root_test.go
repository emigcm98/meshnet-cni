package meshnet

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	fakeTopology "github.com/emigcm98/meshnet-cni/api/clientset/v1beta1/fake"
	"github.com/emigcm98/meshnet-cni/utils/wireutil"
)

func topologyWithLinks(name, srcIP, netNS string, links ...map[string]interface{}) *unstructured.Unstructured {
	items := make([]interface{}, len(links))
	for i, l := range links {
		items[i] = l
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "networkop.co.uk/v1beta1",
		"kind":       "Topology",
		"metadata":   map[string]interface{}{"name": name, "namespace": "default"},
		"spec":       map[string]interface{}{"links": items},
		"status":     map[string]interface{}{"src_ip": srcIP, "net_ns": netNS, "container_id": "c-" + name},
	}}
}

func toRuntimeObjects(topos []*unstructured.Unstructured) []runtime.Object {
	objs := make([]runtime.Object, len(topos))
	for i, t := range topos {
		objs[i] = t
	}
	return objs
}

func linkOf(netNS ns.NetNS, name string) netlink.Link {
	var link netlink.Link
	_ = netNS.Do(func(_ ns.NetNS) error {
		link, _ = netlink.LinkByName(name)
		return nil
	})
	return link
}

func linkKind(t *testing.T, netNS ns.NetNS, name string) string {
	t.Helper()
	if l := linkOf(netNS, name); l != nil {
		return l.Type()
	}
	return ""
}

func linkIndex(t *testing.T, netNS ns.NetNS, name string) int {
	t.Helper()
	if l := linkOf(netNS, name); l != nil {
		return l.Attrs().Index
	}
	return 0
}

func carrier(netNS ns.NetNS, name string) bool {
	if l := linkOf(netNS, name); l != nil {
		return l.Attrs().RawFlags&unix.IFF_LOWER_UP != 0
	}
	return false
}

func hasAddr(netNS ns.NetNS, name, cidr string) bool {
	found := false
	_ = netNS.Do(func(_ ns.NetNS) error {
		l, err := netlink.LinkByName(name)
		if err != nil {
			return nil
		}
		addrs, _ := netlink.AddrList(l, netlink.FAMILY_ALL)
		for _, a := range addrs {
			if a.IPNet.String() == cidr {
				found = true
			}
		}
		return nil
	})
	return found
}

// eventually polls cond for up to three seconds.
func eventually(cond func() bool) bool {
	for i := 0; i < 30; i++ {
		if cond() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return cond()
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
	return n
}

// removeNS deletes a pod's netns as its sandbox going does, and waits for its links to go with it.
func removeNS(t *testing.T, n ns.NetNS, hostEnds ...string) {
	t.Helper()
	_ = n.Close()
	_ = testutils.UnmountNS(n)
	if !eventually(func() bool {
		for _, h := range hostEnds {
			if wireutil.HostLinkExists(h) {
				return false
			}
		}
		return true
	}) {
		t.Fatalf("the node ends %v outlived their netns", hostEnds)
	}
}

func newTestMeshnet(t *testing.T, topos ...*unstructured.Unstructured) *Meshnet {
	t.Helper()
	client, err := fakeTopology.NewSimpleClientset(toRuntimeObjects(topos)...)
	if err != nil {
		t.Fatalf("fake clientset: %v", err)
	}
	m := &Meshnet{nodeIP: "10.0.0.1", tClient: client, topoCache: NewTopologyCache(), defaultMTU: 1450}
	for _, topo := range topos {
		m.topoCache.Put(topo)
	}
	return m
}

func (m *Meshnet) mustReconcile(t *testing.T, topos ...*unstructured.Unstructured) {
	t.Helper()
	for _, topo := range topos {
		m.topoCache.Put(topo)
	}
	for _, topo := range topos {
		if err := m.ReconcilePodLinks(context.Background(), topo); err != nil {
			t.Fatalf("ReconcilePodLinks(%s): %v", topo.GetName(), err)
		}
	}
}

func dummy(t *testing.T, name string) {
	t.Helper()
	if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: name}}); err != nil {
		t.Fatalf("adding the dummy %s: %v", name, err)
	}
	t.Cleanup(func() { _ = wireutil.RemoveHostLink(name) })
}

// Two pods of one node with a link carry frames between them, and the uplink is a macvlan on a node
// interface.
func TestReconcileSameNodeAndUplink(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges. Run test with sudo")
	}
	InitLogger()
	nsA, nsB := newNS(t), newNS(t)
	defer nsA.Close()
	defer nsB.Close()
	dummy(t, "uplinktest0")

	a := topologyWithLinks("a", "10.0.0.1", nsA.Path(),
		map[string]interface{}{"uid": int64(7), "peer_pod": "b", "local_intf": "eth1", "peer_intf": "eth1", "local_ip": "10.7.0.1/30"},
		map[string]interface{}{"uid": int64(8), "peer_pod": "localhost", "local_intf": "eth9", "peer_intf": "uplinktest0"},
	)
	b := topologyWithLinks("b", "10.0.0.1", nsB.Path(),
		map[string]interface{}{"uid": int64(7), "peer_pod": "a", "local_intf": "eth1", "peer_intf": "eth1", "local_ip": "10.7.0.2/30"},
	)
	m := newTestMeshnet(t, a, b)
	m.mustReconcile(t, a, b)

	if k := linkKind(t, nsA, "eth9"); k != "macvlan" {
		t.Fatalf("a eth9 is %q, want the macvlan of the uplink", k)
	}
	if !carrier(nsA, "eth1") || !carrier(nsB, "eth1") {
		t.Fatalf("carrier a %t, b %t after both are wired, want both", carrier(nsA, "eth1"), carrier(nsB, "eth1"))
	}
	if !udpReaches(t, nsA, nsB, "10.7.0.2") {
		t.Fatalf("a datagram from a did not reach b")
	}
}

// A pod that goes leaves its neighbour's interface in place, with its address and without carrier,
// and when it comes back in a new netns the neighbour's interface, never remade, carries again.
func TestReconcileOffAndBack(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges. Run test with sudo")
	}
	InitLogger()
	nsA, nsB := newNS(t), newNS(t)
	defer nsA.Close()
	linkA := map[string]interface{}{"uid": int64(11), "peer_pod": "b", "local_intf": "eth1", "peer_intf": "eth1", "local_ip": "10.11.0.1/30"}
	linkB := map[string]interface{}{"uid": int64(11), "peer_pod": "a", "local_intf": "eth1", "peer_intf": "eth1", "local_ip": "10.11.0.2/30"}
	a := topologyWithLinks("a", "10.0.0.1", nsA.Path(), linkA)
	b := topologyWithLinks("b", "10.0.0.1", nsB.Path(), linkB)
	m := newTestMeshnet(t, a, b)
	m.mustReconcile(t, a, b)
	kept := linkIndex(t, nsA, "eth1")

	// b goes, as its pod is deleted: its netns with it, its Topology no longer alive.
	removeNS(t, nsB, wireutil.HostEndName("default", "b", 11))
	m.mustReconcile(t, topologyWithLinks("b", "", "", linkB), a)
	if got := linkIndex(t, nsA, "eth1"); got != kept {
		t.Fatalf("a eth1 was remade or lost while b was off (ifindex %d, was %d)", got, kept)
	}
	if !hasAddr(nsA, "eth1", "10.11.0.1/30") {
		t.Errorf("a eth1 lost its address while b was off")
	}
	if carrier(nsA, "eth1") {
		t.Errorf("a eth1 keeps its carrier while b is off")
	}
	if !wireutil.PodEndAdminUp(nsA.Path(), "eth1") {
		t.Errorf("a eth1 must stay admin up while b is off, only its carrier goes")
	}

	// b comes back in a new netns.
	nsB2 := newNS(t)
	defer nsB2.Close()
	m.mustReconcile(t, topologyWithLinks("b", "10.0.0.1", nsB2.Path(), linkB), a)
	if got := linkIndex(t, nsA, "eth1"); got != kept {
		t.Fatalf("a eth1 was remade when b came back (ifindex %d, was %d)", got, kept)
	}
	if !carrier(nsA, "eth1") {
		t.Fatalf("a eth1 has no carrier with b back")
	}
	if !udpReaches(t, nsA, nsB2, "10.11.0.2") {
		t.Fatalf("a datagram from a did not reach the b that came back")
	}
}

// With the watcher running, a's carrier follows b's admin state and b's going, at once and without a
// reconcile, as on a cable.
func TestReconcileCarrierFollowsPeer(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges. Run test with sudo")
	}
	InitLogger()
	nsA, nsB := newNS(t), newNS(t)
	defer nsA.Close()
	a := topologyWithLinks("a", "10.0.0.1", nsA.Path(),
		map[string]interface{}{"uid": int64(12), "peer_pod": "b", "local_intf": "eth1", "peer_intf": "eth1"})
	b := topologyWithLinks("b", "10.0.0.1", nsB.Path(),
		map[string]interface{}{"uid": int64(12), "peer_pod": "a", "local_intf": "eth1", "peer_intf": "eth1"})
	m := newTestMeshnet(t, a, b)
	m.mustReconcile(t, a, b)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.watchHostEnds(ctx)
	time.Sleep(200 * time.Millisecond)

	setAdmin := func(n ns.NetNS, up bool) {
		_ = n.Do(func(_ ns.NetNS) error {
			l, err := netlink.LinkByName("eth1")
			if err != nil {
				return err
			}
			if up {
				return netlink.LinkSetUp(l)
			}
			return netlink.LinkSetDown(l)
		})
	}
	setAdmin(nsB, false)
	if !eventually(func() bool { return !carrier(nsA, "eth1") }) {
		t.Fatalf("a eth1 keeps its carrier after b shut its end")
	}
	setAdmin(nsB, true)
	if !eventually(func() bool { return carrier(nsA, "eth1") }) {
		t.Fatalf("a eth1 has no carrier after b brought its end back up")
	}
	removeNS(t, nsB, wireutil.HostEndName("default", "b", 12))
	if !eventually(func() bool { return !carrier(nsA, "eth1") }) {
		t.Fatalf("a eth1 keeps its carrier after b's netns went")
	}
}

// A link whose peer moves from another node onto this one changes transport on the node side only:
// the pod's interface is the same one before and after, and the vxlan goes.
func TestReconcileTransportChangeKeepsInterface(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges. Run test with sudo")
	}
	InitLogger()
	dummy(t, "vteptest0")
	nsA := newNS(t)
	defer nsA.Close()
	linkA := map[string]interface{}{"uid": int64(13), "peer_pod": "c", "local_intf": "eth3", "peer_intf": "eth1", "local_ip": "10.13.0.1/30"}
	linkC := map[string]interface{}{"uid": int64(13), "peer_pod": "a", "local_intf": "eth1", "peer_intf": "eth3", "local_ip": "10.13.0.3/30"}
	a := topologyWithLinks("a", "10.0.0.1", nsA.Path(), linkA)
	c := topologyWithLinks("c", "10.0.0.2", "/var/run/netns/elsewhere", linkC)
	m := newTestMeshnet(t, a, c)
	m.nodeIntf = "vteptest0"
	m.mustReconcile(t, a)

	vx := wireutil.HostVxlanName("default", "a", 13)
	l, err := netlink.LinkByName(vx)
	if err != nil {
		t.Fatalf("no vxlan %s towards c's node: %v", vx, err)
	}
	if v, ok := l.(*netlink.Vxlan); !ok || v.VxlanId != 13+vxlanBase || !v.Group.Equal(net.ParseIP("10.0.0.2")) {
		t.Fatalf("%s is not the vxlan to c's node with VNI %d: %+v", vx, 13+vxlanBase, l)
	}
	if !carrier(nsA, "eth3") {
		t.Fatalf("a eth3 has no carrier with c up on another node")
	}
	kept := linkIndex(t, nsA, "eth3")

	nsC := newNS(t)
	defer nsC.Close()
	m.mustReconcile(t, topologyWithLinks("c", "10.0.0.1", nsC.Path(), linkC), a)
	if wireutil.HostLinkExists(vx) {
		t.Errorf("the vxlan %s outlived the move onto this node", vx)
	}
	if got := linkIndex(t, nsA, "eth3"); got != kept {
		t.Fatalf("a eth3 was remade by c's move (ifindex %d, was %d)", got, kept)
	}
	if !udpReaches(t, nsA, nsC, "10.13.0.3") {
		t.Fatalf("a datagram from a did not reach c on this node")
	}
}

// Links removed from the spec go from the running pods, with their node ends and vxlans, while the
// link that stays is not touched and an interface of the workload's own is left alone.
func TestReconcilePrunesRemovedLinks(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges. Run test with sudo")
	}
	InitLogger()
	dummy(t, "vteptest1")
	nsA, nsB := newNS(t), newNS(t)
	defer nsA.Close()
	defer nsB.Close()
	if err := nsA.Do(func(_ ns.NetNS) error {
		return netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth5"}})
	}); err != nil {
		t.Fatalf("adding the workload's own interface: %v", err)
	}

	keepAB := map[string]interface{}{"uid": int64(21), "peer_pod": "b", "local_intf": "eth1", "peer_intf": "eth1"}
	dropAB := map[string]interface{}{"uid": int64(22), "peer_pod": "b", "local_intf": "eth2", "peer_intf": "eth2"}
	dropAC := map[string]interface{}{"uid": int64(23), "peer_pod": "c", "local_intf": "eth3", "peer_intf": "eth1"}
	keepBA := map[string]interface{}{"uid": int64(21), "peer_pod": "a", "local_intf": "eth1", "peer_intf": "eth1"}
	dropBA := map[string]interface{}{"uid": int64(22), "peer_pod": "a", "local_intf": "eth2", "peer_intf": "eth2"}
	a := topologyWithLinks("a", "10.0.0.1", nsA.Path(), keepAB, dropAB, dropAC)
	b := topologyWithLinks("b", "10.0.0.1", nsB.Path(), keepBA, dropBA)
	c := topologyWithLinks("c", "10.0.0.2", "/var/run/netns/elsewhere",
		map[string]interface{}{"uid": int64(23), "peer_pod": "a", "local_intf": "eth1", "peer_intf": "eth3"})
	m := newTestMeshnet(t, a, b, c)
	m.nodeIntf = "vteptest1"
	m.mustReconcile(t, a, b)
	vx := wireutil.HostVxlanName("default", "a", 23)
	for _, name := range []string{"eth1", "eth2", "eth3"} {
		if k := linkKind(t, nsA, name); k != "veth" {
			t.Fatalf("a %s is %q after the first pass, want veth", name, k)
		}
	}
	if !wireutil.HostLinkExists(vx) {
		t.Fatalf("no vxlan %s for the link to c", vx)
	}
	keptIndex := linkIndex(t, nsA, "eth1")

	m.mustReconcile(t, topologyWithLinks("a", "10.0.0.1", nsA.Path(), keepAB), topologyWithLinks("b", "10.0.0.1", nsB.Path(), keepBA))
	for _, name := range []string{"eth2", "eth3"} {
		if k := linkKind(t, nsA, name); k != "" {
			t.Errorf("a still has %s (%s) after its link left the spec", name, k)
		}
	}
	if k := linkKind(t, nsB, "eth2"); k != "" {
		t.Errorf("b still has eth2 (%s) after its link left the spec", k)
	}
	if wireutil.HostLinkExists(vx) {
		t.Errorf("the vxlan %s outlived its link", vx)
	}
	if got := linkIndex(t, nsA, "eth1"); got != keptIndex {
		t.Errorf("a eth1 was remade (ifindex %d, was %d), the link that stays must not be touched", got, keptIndex)
	}
	if k := linkKind(t, nsA, "eth5"); k != "dummy" {
		t.Errorf("the workload's own eth5 is %q, it must be left alone", k)
	}
}

// SetAlive on a pod queues it, when it runs here, and the local pods linked to it.
func TestEnqueuePodAndDependents(t *testing.T) {
	m := &Meshnet{nodeIP: "10.0.0.1", topoCache: NewTopologyCache(), reconcileQueue: NewReconcileQueue(0)}
	m.topoCache.Put(createFakePodTopology("here", "default", "10.0.0.1", "/proc/1/ns/net", []string{"moved"}))
	m.topoCache.Put(createFakePodTopology("elsewhere", "default", "10.0.0.2", "/proc/1/ns/net", []string{"moved"}))
	m.topoCache.Put(createFakePodTopology("moved", "default", "10.0.0.1", "/proc/1/ns/net", []string{"here", "elsewhere"}))

	m.enqueuePodAndDependents("default", "moved")
	<-m.reconcileQueue.notifyChan
	_, keys := m.reconcileQueue.Drain()
	got := map[string]bool{}
	for _, k := range keys {
		got[k] = true
	}
	if len(got) != 2 || !got["default/moved"] || !got["default/here"] {
		t.Fatalf("queued %v, want default/moved and default/here (default/elsewhere runs on another node)", keys)
	}
}

// An unplugged link is a pulled cable: both pods keep their interfaces administratively up and see
// no carrier, on one node and across nodes, and plugging it back brings the carrier and the frames.
// Either end declaring it unplugs the link.
func TestReconcileUnplugged(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges. Run test with sudo")
	}
	InitLogger()
	dummy(t, "vteptest1")
	nsA, nsB := newNS(t), newNS(t)
	defer nsA.Close()
	defer nsB.Close()
	linkAB := map[string]interface{}{"uid": int64(21), "peer_pod": "b", "local_intf": "eth1", "peer_intf": "eth1", "local_ip": "10.21.0.1/30"}
	linkBA := map[string]interface{}{"uid": int64(21), "peer_pod": "a", "local_intf": "eth1", "peer_intf": "eth1", "local_ip": "10.21.0.2/30"}
	linkAC := map[string]interface{}{"uid": int64(22), "peer_pod": "c", "local_intf": "eth2", "peer_intf": "eth1", "local_ip": "10.22.0.1/30"}
	linkCA := map[string]interface{}{"uid": int64(22), "peer_pod": "a", "local_intf": "eth1", "peer_intf": "eth2", "local_ip": "10.22.0.3/30"}
	with := func(l map[string]interface{}, unplugged bool) map[string]interface{} {
		out := make(map[string]interface{}, len(l)+1)
		for k, v := range l {
			out[k] = v
		}
		if unplugged {
			out["unplugged"] = true
		}
		return out
	}
	c := topologyWithLinks("c", "10.0.0.2", "/var/run/netns/elsewhere", linkCA)
	topos := func(unplugAB, unplugAC bool) []*unstructured.Unstructured {
		return []*unstructured.Unstructured{
			topologyWithLinks("a", "10.0.0.1", nsA.Path(), linkAB, with(linkAC, unplugAC)),
			// The link to b is unplugged from b's side only: either end pulls the cable.
			topologyWithLinks("b", "10.0.0.1", nsB.Path(), with(linkBA, unplugAB)),
		}
	}
	m := newTestMeshnet(t, append(topos(false, false), c)...)
	m.nodeIntf = "vteptest1"
	m.mustReconcile(t, topos(false, false)...)
	if !carrier(nsA, "eth1") || !carrier(nsB, "eth1") || !carrier(nsA, "eth2") {
		t.Fatalf("plugged links have no carrier")
	}
	kept := linkIndex(t, nsA, "eth1")

	m.mustReconcile(t, topos(true, true)...)
	for _, end := range []struct {
		n    ns.NetNS
		intf string
	}{{nsA, "eth1"}, {nsB, "eth1"}, {nsA, "eth2"}} {
		if carrier(end.n, end.intf) {
			t.Errorf("%s keeps its carrier with the cable unplugged", end.intf)
		}
		if !wireutil.PodEndAdminUp(end.n.Path(), end.intf) {
			t.Errorf("%s must stay admin up with the cable unplugged", end.intf)
		}
	}
	// The carrier watcher's own pass must not plug it back.
	m.syncCarrier(wireutil.HostEndName("default", "a", 21))
	if carrier(nsA, "eth1") || carrier(nsB, "eth1") {
		t.Errorf("syncCarrier plugged an unplugged link back")
	}

	m.mustReconcile(t, topos(false, false)...)
	if got := linkIndex(t, nsA, "eth1"); got != kept {
		t.Fatalf("a eth1 was remade by plugging the cable back (ifindex %d, was %d)", got, kept)
	}
	if !carrier(nsA, "eth1") || !carrier(nsB, "eth1") || !carrier(nsA, "eth2") {
		t.Fatalf("plugged back, the links have no carrier")
	}
	if !udpReaches(t, nsA, nsB, "10.21.0.2") {
		t.Fatalf("a datagram from a did not reach b after plugging the cable back")
	}
}

// The declaration of a link reaches a running pod: a new MAC and a new address list are applied
// by the next reconcile, and what the workload changed of the rest stays.
func TestReconcileDeclaredLive(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges. Run test with sudo")
	}
	InitLogger()
	nsA, nsB := newNS(t), newNS(t)
	defer nsA.Close()
	defer nsB.Close()
	linkA := map[string]interface{}{"uid": int64(23), "peer_pod": "b", "local_intf": "eth1", "peer_intf": "eth1", "local_ip": "10.23.0.1/30",
		"addresses": []interface{}{"fd23::1/64"}, "mac": "02:23:00:00:00:01", "mtu": int64(1400)}
	linkB := map[string]interface{}{"uid": int64(23), "peer_pod": "a", "local_intf": "eth1", "peer_intf": "eth1", "local_ip": "10.23.0.2/30"}
	a := topologyWithLinks("a", "10.0.0.1", nsA.Path(), linkA)
	b := topologyWithLinks("b", "10.0.0.1", nsB.Path(), linkB)
	m := newTestMeshnet(t, a, b)
	m.mustReconcile(t, a, b)
	l := linkOf(nsA, "eth1")
	if l == nil || l.Attrs().HardwareAddr.String() != "02:23:00:00:00:01" || l.Attrs().MTU != 1400 || !hasAddr(nsA, "eth1", "fd23::1/64") {
		t.Fatalf("a eth1 was not born as declared: %+v", l)
	}
	if lb := linkOf(nsB, "eth1"); lb == nil || lb.Attrs().MTU != 1400 {
		t.Fatalf("b eth1 does not have the MTU a declared for the link")
	}

	// The workload lowers a's MTU by hand.
	if err := nsA.Do(func(_ ns.NetNS) error { return netlink.LinkSetMTU(linkOf(nsA, "eth1"), 1300) }); err != nil {
		t.Fatalf("lowering the MTU: %v", err)
	}
	linkA2 := map[string]interface{}{"uid": int64(23), "peer_pod": "b", "local_intf": "eth1", "peer_intf": "eth1", "local_ip": "10.23.0.1/30",
		"addresses": []interface{}{"fd23::5/64"}, "mac": "02:23:00:00:00:05", "mtu": int64(1400)}
	m.mustReconcile(t, topologyWithLinks("a", "10.0.0.1", nsA.Path(), linkA2), b)
	l = linkOf(nsA, "eth1")
	if l.Attrs().HardwareAddr.String() != "02:23:00:00:00:05" || hasAddr(nsA, "eth1", "fd23::1/64") || !hasAddr(nsA, "eth1", "fd23::5/64") {
		t.Errorf("a eth1 did not take its new MAC and addresses: mac %s, fd23::1 %t, fd23::5 %t",
			l.Attrs().HardwareAddr, hasAddr(nsA, "eth1", "fd23::1/64"), hasAddr(nsA, "eth1", "fd23::5/64"))
	}
	if l.Attrs().MTU != 1300 {
		t.Errorf("a eth1 MTU %d: the workload's 1300 must stay while the declared MTU does not change", l.Attrs().MTU)
	}
}

// rootQdiscType is the kind of the root qdisc of the node end name, empty for the veth's default.
func rootQdiscType(t *testing.T, name string) string {
	t.Helper()
	l, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatalf("node end %s: %v", name, err)
	}
	qdiscs, _ := netlink.QdiscList(l)
	for _, q := range qdiscs {
		if q.Attrs().Parent == netlink.HANDLE_ROOT && q.Type() != "noqueue" {
			return q.Type()
		}
	}
	return ""
}

// What an end declares it sends is shaped on the node end of the other side, on one node and across
// nodes, follows the declaration, and an invalid one is reported without keeping the link unwired.
func TestReconcileShaping(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges. Run test with sudo")
	}
	InitLogger()
	dummy(t, "vteptest2")
	nsA, nsB := newNS(t), newNS(t)
	defer nsA.Close()
	defer nsB.Close()
	link := func(uid int64, peer, local, peerIntf, ip string, shaping map[string]interface{}) map[string]interface{} {
		l := map[string]interface{}{"uid": uid, "peer_pod": peer, "local_intf": local, "peer_intf": peerIntf, "local_ip": ip}
		if shaping != nil {
			l["shaping"] = shaping
		}
		return l
	}
	c := topologyWithLinks("c", "10.0.0.2", "/var/run/netns/elsewhere",
		link(25, "a", "eth1", "eth2", "10.25.0.3/30", map[string]interface{}{"loss": "5%"}))
	topos := func(bShaping map[string]interface{}) []*unstructured.Unstructured {
		return []*unstructured.Unstructured{
			topologyWithLinks("a", "10.0.0.1", nsA.Path(), link(24, "b", "eth1", "eth1", "10.24.0.1/30", nil), link(25, "c", "eth2", "eth1", "10.25.0.1/30", nil)),
			topologyWithLinks("b", "10.0.0.1", nsB.Path(), link(24, "a", "eth1", "eth1", "10.24.0.2/30", bShaping)),
		}
	}
	m := newTestMeshnet(t, append(topos(map[string]interface{}{"delay": "20ms"}), c)...)
	m.nodeIntf = "vteptest2"
	m.mustReconcile(t, topos(map[string]interface{}{"delay": "20ms"})...)
	hostA24, hostB24 := wireutil.HostEndName("default", "a", 24), wireutil.HostEndName("default", "b", 24)
	hostA25 := wireutil.HostEndName("default", "a", 25)
	if got := rootQdiscType(t, hostA24); got != "netem" {
		t.Errorf("what b sends towards a is not shaped on a's node end: %q", got)
	}
	if got := rootQdiscType(t, hostB24); got != "" {
		t.Errorf("b's node end got %q, but a declares nothing for what it sends", got)
	}
	if got := rootQdiscType(t, hostA25); got != "netem" {
		t.Errorf("what c sends from another node is not shaped on a's node end: %q", got)
	}

	m.mustReconcile(t, topos(map[string]interface{}{"qdisc": "tbf", "rate": "1mbit"})...)
	if got := rootQdiscType(t, hostA24); got != "tbf" {
		t.Errorf("b's new shaping did not reach a's node end: %q", got)
	}
	m.mustReconcile(t, topos(nil)...)
	if got := rootQdiscType(t, hostA24); got != "" {
		t.Errorf("a's node end keeps %q after b dropped its shaping", got)
	}

	// An invalid shaping is in the pod's plumbing error, and the link is wired all the same.
	bad := topos(map[string]interface{}{"jitter": "5ms"})
	m.topoCache.Put(bad[1])
	err := m.ReconcilePodLinks(context.Background(), bad[0])
	if err == nil || !strings.Contains(err.Error(), "a jitter needs a delay") {
		t.Errorf("an invalid shaping reconciled with %v, want its error", err)
	}
	if !carrier(nsA, "eth1") || !carrier(nsA, "eth2") {
		t.Errorf("an invalid shaping kept the links of a from being wired")
	}
}
