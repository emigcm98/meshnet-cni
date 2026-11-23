package meshnet

import (
	"context"
	"os"
	"testing"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	"github.com/vishvananda/netlink"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	fakeTopology "github.com/emigcm98/meshnet-cni/api/clientset/v1beta1/fake"
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

func linkKind(t *testing.T, netNS ns.NetNS, name string) string {
	t.Helper()
	kind := ""
	_ = netNS.Do(func(_ ns.NetNS) error {
		if l, err := netlink.LinkByName(name); err == nil {
			kind = l.Type()
		}
		return nil
	})
	return kind
}

// Two pods of this node with one link: the reconcile wires a veth between them, remakes it when one
// end is something else (what a pod keeps from a link that crossed workers before its peer moved
// here), and hangs the external uplink on a node interface as a macvlan.
func TestReconcileSameNodeAndUplink(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges. Run test with sudo")
	}
	InitLogger()
	nsA, err := testutils.NewNS()
	if err != nil {
		t.Fatalf("creating netns A: %v", err)
	}
	defer nsA.Close()
	nsB, err := testutils.NewNS()
	if err != nil {
		t.Fatalf("creating netns B: %v", err)
	}
	defer nsB.Close()
	uplink := "uplinktest0"
	if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: uplink}}); err != nil {
		t.Fatalf("adding the uplink dummy: %v", err)
	}
	defer func() {
		if l, err := netlink.LinkByName(uplink); err == nil {
			_ = netlink.LinkDel(l)
		}
	}()

	a := topologyWithLinks("a", "10.0.0.1", nsA.Path(),
		map[string]interface{}{"uid": int64(7), "peer_pod": "b", "local_intf": "eth1", "peer_intf": "eth1", "local_ip": "10.7.0.1/30", "peer_ip": "10.7.0.2/30"},
		map[string]interface{}{"uid": int64(8), "peer_pod": "localhost", "local_intf": "eth9", "peer_intf": uplink},
	)
	b := topologyWithLinks("b", "10.0.0.1", nsB.Path(),
		map[string]interface{}{"uid": int64(7), "peer_pod": "a", "local_intf": "eth1", "peer_intf": "eth1", "local_ip": "10.7.0.2/30", "peer_ip": "10.7.0.1/30"},
	)
	client, err := fakeTopology.NewSimpleClientset(a, b)
	if err != nil {
		t.Fatalf("fake clientset: %v", err)
	}
	m := &Meshnet{nodeIP: "10.0.0.1", tClient: client, topoCache: NewTopologyCache()}
	m.topoCache.Put(a)
	m.topoCache.Put(b)
	reconcile := func() {
		t.Helper()
		for _, topo := range []*unstructured.Unstructured{a, b} {
			if err := m.ReconcilePodLinks(context.Background(), topo); err != nil {
				t.Fatalf("ReconcilePodLinks(%s): %v", topo.GetName(), err)
			}
		}
	}

	reconcile()
	if k := linkKind(t, nsA, "eth1"); k != "veth" {
		t.Fatalf("a eth1 is %q after the first pass, want veth", k)
	}
	if k := linkKind(t, nsB, "eth1"); k != "veth" {
		t.Fatalf("b eth1 is %q after the first pass, want veth", k)
	}
	if k := linkKind(t, nsA, "eth9"); k != "macvlan" {
		t.Fatalf("a eth9 is %q, want the macvlan of the uplink", k)
	}

	// b's peer moved away and came back: b's end of the link is now a leftover of another kind and
	// a's end went with a's old netns.
	if err := nsA.Do(func(_ ns.NetNS) error {
		l, err := netlink.LinkByName("eth1")
		if err != nil {
			return err
		}
		return netlink.LinkDel(l)
	}); err != nil {
		t.Fatalf("removing a eth1: %v", err)
	}
	if err := nsB.Do(func(_ ns.NetNS) error {
		return netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth1"}})
	}); err != nil {
		t.Fatalf("planting a leftover in b: %v", err)
	}
	reconcile()
	if k := linkKind(t, nsA, "eth1"); k != "veth" {
		t.Fatalf("a eth1 is %q after the move, want veth", k)
	}
	if k := linkKind(t, nsB, "eth1"); k != "veth" {
		t.Fatalf("b eth1 is %q after the move, want the leftover replaced by a veth", k)
	}
	var peerIndex int
	if err := nsB.Do(func(_ ns.NetNS) error {
		l, err := netlink.LinkByName("eth1")
		if err != nil {
			return err
		}
		peerIndex, err = netlink.VethPeerIndex(l.(*netlink.Veth))
		return err
	}); err != nil {
		t.Fatalf("reading b eth1's peer: %v", err)
	}
	var aIndex int
	_ = nsA.Do(func(_ ns.NetNS) error {
		l, err := netlink.LinkByName("eth1")
		if err == nil {
			aIndex = l.Attrs().Index
		}
		return nil
	})
	if peerIndex != aIndex {
		t.Fatalf("b eth1's peer is ifindex %d, a eth1 is %d: the two ends are not one pair", peerIndex, aIndex)
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

// vethPeerOf returns the ifindex, in the peer's netns, of the other end of name in netNS.
func vethPeerOf(t *testing.T, netNS ns.NetNS, name string) (self, peer int) {
	t.Helper()
	if err := netNS.Do(func(_ ns.NetNS) error {
		l, err := netlink.LinkByName(name)
		if err != nil {
			return err
		}
		v, ok := l.(*netlink.Veth)
		if !ok {
			return nil
		}
		self = l.Attrs().Index
		peer, err = netlink.VethPeerIndex(v)
		return err
	}); err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return self, peer
}

// A pod recreated on its peer's node while its old sandbox is still being torn down: the peer still
// holds its end of the old pair when the new pod's reconcile makes a new one, so the peer must be
// queued, and its reconcile must swap the old end for the new one instead of keeping it until the
// old netns goes and takes the interface with it.
func TestReconcileReplacesOldPairWhenPeerComesBack(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges. Run test with sudo")
	}
	InitLogger()
	oldA, err := testutils.NewNS()
	if err != nil {
		t.Fatalf("creating the old netns of a: %v", err)
	}
	defer oldA.Close()
	nsB, err := testutils.NewNS()
	if err != nil {
		t.Fatalf("creating netns b: %v", err)
	}
	defer nsB.Close()

	linkA := map[string]interface{}{"uid": int64(11), "peer_pod": "b", "local_intf": "eth1", "peer_intf": "eth1", "local_ip": "10.11.0.1/30"}
	linkB := map[string]interface{}{"uid": int64(11), "peer_pod": "a", "local_intf": "eth1", "peer_intf": "eth1", "local_ip": "10.11.0.2/30"}
	a := topologyWithLinks("a", "10.0.0.1", oldA.Path(), linkA)
	b := topologyWithLinks("b", "10.0.0.1", nsB.Path(), linkB)
	client, err := fakeTopology.NewSimpleClientset(a, b)
	if err != nil {
		t.Fatalf("fake clientset: %v", err)
	}
	m := &Meshnet{nodeIP: "10.0.0.1", tClient: client, topoCache: NewTopologyCache(), reconcileQueue: NewReconcileQueue(0)}
	m.topoCache.Put(a)
	m.topoCache.Put(b)
	for _, topo := range []*unstructured.Unstructured{a, b} {
		if err := m.ReconcilePodLinks(context.Background(), topo); err != nil {
			t.Fatalf("first pass of %s: %v", topo.GetName(), err)
		}
	}
	_, oldPeer := vethPeerOf(t, nsB, "eth1")
	if oldSelf, _ := vethPeerOf(t, oldA, "eth1"); oldPeer != oldSelf {
		t.Fatalf("after the first pass b eth1 is not paired with a eth1")
	}
	m.reconcileQueue.Drain()

	// a comes back in a new netns; its old one is still alive, and with it b's end of the old pair.
	newA, err := testutils.NewNS()
	if err != nil {
		t.Fatalf("creating the new netns of a: %v", err)
	}
	defer newA.Close()
	a = topologyWithLinks("a", "10.0.0.1", newA.Path(), linkA)
	m.topoCache.Put(a)
	if err := m.ReconcilePodLinks(context.Background(), a); err != nil {
		t.Fatalf("reconcile of the new a: %v", err)
	}
	<-m.reconcileQueue.notifyChan
	if _, keys := m.reconcileQueue.Drain(); len(keys) != 1 || keys[0] != "default/b" {
		t.Fatalf("queued %v after a made a new pair, want default/b", keys)
	}
	if err := m.ReconcilePodLinks(context.Background(), b); err != nil {
		t.Fatalf("reconcile of b: %v", err)
	}
	newSelf, _ := vethPeerOf(t, newA, "eth1")
	_, bPeer := vethPeerOf(t, nsB, "eth1")
	if newSelf == 0 || bPeer != newSelf {
		t.Fatalf("b eth1's peer is ifindex %d, the new a eth1 is %d: b kept the old pair", bPeer, newSelf)
	}
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

// Links removed from the spec go from the running pods, a veth from both ends and a vxlan from its
// own, while the link that stays is not touched and an interface of the workload's own is left alone.
func TestReconcilePrunesRemovedLinks(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges. Run test with sudo")
	}
	InitLogger()
	nsA, err := testutils.NewNS()
	if err != nil {
		t.Fatalf("creating netns a: %v", err)
	}
	defer nsA.Close()
	nsB, err := testutils.NewNS()
	if err != nil {
		t.Fatalf("creating netns b: %v", err)
	}
	defer nsB.Close()
	vtep := "vteptest0"
	if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: vtep}}); err != nil {
		t.Fatalf("adding the vtep dummy: %v", err)
	}
	defer func() {
		if l, err := netlink.LinkByName(vtep); err == nil {
			_ = netlink.LinkDel(l)
		}
	}()
	if err := nsA.Do(func(_ ns.NetNS) error {
		return netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth5"}})
	}); err != nil {
		t.Fatalf("adding the workload's own interface: %v", err)
	}

	keepAB := map[string]interface{}{"uid": int64(21), "peer_pod": "b", "local_intf": "eth1", "peer_intf": "eth1"}
	dropAB := map[string]interface{}{"uid": int64(22), "peer_pod": "b", "local_intf": "eth2", "peer_intf": "eth2"}
	dropAC := map[string]interface{}{"uid": int64(23), "peer_pod": "c", "local_intf": "eth3", "peer_intf": "eth1"}
	dropBA := map[string]interface{}{"uid": int64(22), "peer_pod": "a", "local_intf": "eth2", "peer_intf": "eth2"}
	keepBA := map[string]interface{}{"uid": int64(21), "peer_pod": "a", "local_intf": "eth1", "peer_intf": "eth1"}
	a := topologyWithLinks("a", "10.0.0.1", nsA.Path(), keepAB, dropAB, dropAC)
	b := topologyWithLinks("b", "10.0.0.1", nsB.Path(), keepBA, dropBA)
	c := topologyWithLinks("c", "10.0.0.2", "/var/run/netns/elsewhere",
		map[string]interface{}{"uid": int64(23), "peer_pod": "a", "local_intf": "eth1", "peer_intf": "eth3"})
	client, err := fakeTopology.NewSimpleClientset(a, b, c)
	if err != nil {
		t.Fatalf("fake clientset: %v", err)
	}
	m := &Meshnet{nodeIP: "10.0.0.1", nodeIntf: vtep, tClient: client, topoCache: NewTopologyCache()}
	for _, topo := range []*unstructured.Unstructured{a, b, c} {
		m.topoCache.Put(topo)
	}
	for _, topo := range []*unstructured.Unstructured{a, b} {
		if err := m.ReconcilePodLinks(context.Background(), topo); err != nil {
			t.Fatalf("first pass of %s: %v", topo.GetName(), err)
		}
	}
	for name, want := range map[string]string{"eth1": "veth", "eth2": "veth", "eth3": "vxlan"} {
		if k := linkKind(t, nsA, name); k != want {
			t.Fatalf("a %s is %q after the first pass, want %s", name, k, want)
		}
	}
	keptIndex := linkIndex(t, nsA, "eth1")

	a = topologyWithLinks("a", "10.0.0.1", nsA.Path(), keepAB)
	b = topologyWithLinks("b", "10.0.0.1", nsB.Path(), keepBA)
	m.topoCache.Put(a)
	m.topoCache.Put(b)
	for _, topo := range []*unstructured.Unstructured{a, b} {
		if err := m.ReconcilePodLinks(context.Background(), topo); err != nil {
			t.Fatalf("pass of %s after the removal: %v", topo.GetName(), err)
		}
	}
	for _, name := range []string{"eth2", "eth3"} {
		if k := linkKind(t, nsA, name); k != "" {
			t.Errorf("a still has %s (%s) after its link left the spec", name, k)
		}
	}
	if k := linkKind(t, nsB, "eth2"); k != "" {
		t.Errorf("b still has eth2 (%s) after its link left the spec", k)
	}
	if got := linkIndex(t, nsA, "eth1"); got != keptIndex {
		t.Errorf("a eth1 was remade (ifindex %d, was %d), the link that stays must not be touched", got, keptIndex)
	}
	if k := linkKind(t, nsA, "eth5"); k != "dummy" {
		t.Errorf("the workload's own eth5 is %q, it must be left alone", k)
	}
}
