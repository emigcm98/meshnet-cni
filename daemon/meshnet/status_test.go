package meshnet

import (
	"context"
	"strings"
	"testing"

	fakeTopology "github.com/emigcm98/meshnet-cni/api/clientset/v1beta1/fake"
	"github.com/emigcm98/meshnet-cni/api/types/v1beta1"
	"github.com/emigcm98/meshnet-cni/utils/wireutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
)

func link(uid int, peer, state, transport string, ifindex int) v1beta1.LinkStatus {
	return v1beta1.LinkStatus{UID: uid, LocalIntf: "eth" + string(rune('0'+uid)), PeerPod: peer, State: state, Transport: transport, IfIndex: ifindex}
}

func reasons(events []linkEvent) string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.reason)
	}
	return strings.Join(out, ",")
}

func TestLinkEvents(t *testing.T) {
	up := link(1, "r2", v1beta1.LinkUp, "veth", 10)
	off := link(1, "r2", v1beta1.LinkOff, "", 10)
	moved := link(1, "r2", v1beta1.LinkUp, "vxlan", 10)
	moved.VNI = 5001
	remade := link(1, "r2", v1beta1.LinkUp, "veth", 11)
	failed := link(1, "r2", v1beta1.LinkFailed, "", 0)
	failed.Error = "boom"
	shapingErr := up
	shapingErr.Error = "shaping from r2: a jitter needs a delay"
	for _, tt := range []struct {
		name      string
		prev, cur []v1beta1.LinkStatus
		want      string
	}{
		{"nothing moved", []v1beta1.LinkStatus{up}, []v1beta1.LinkStatus{up}, ""},
		{"a new link", nil, []v1beta1.LinkStatus{up}, reasonWired},
		{"a new link whose peer is off", nil, []v1beta1.LinkStatus{off}, reasonWired},
		{"the peer goes", []v1beta1.LinkStatus{up}, []v1beta1.LinkStatus{off}, reasonDown},
		{"the peer comes back on another node", []v1beta1.LinkStatus{off}, []v1beta1.LinkStatus{moved}, reasonUp},
		{"moved across nodes while up", []v1beta1.LinkStatus{up}, []v1beta1.LinkStatus{moved}, reasonWired},
		{"made again", []v1beta1.LinkStatus{up}, []v1beta1.LinkStatus{remade}, reasonWired},
		{"removed", []v1beta1.LinkStatus{up}, nil, reasonUnwired},
		{"fails", []v1beta1.LinkStatus{up}, []v1beta1.LinkStatus{failed}, reasonFailed},
		{"fails the same way again", []v1beta1.LinkStatus{failed}, []v1beta1.LinkStatus{failed}, ""},
		{"recovers", []v1beta1.LinkStatus{failed}, []v1beta1.LinkStatus{up}, reasonWired},
		{"a shaping error on a link that carries", []v1beta1.LinkStatus{up}, []v1beta1.LinkStatus{shapingErr}, reasonFailed},
	} {
		if got := reasons(linkEvents(tt.prev, tt.cur)); got != tt.want {
			t.Errorf("%s: events %q, want %q", tt.name, got, tt.want)
		}
	}
	if e := linkEvents([]v1beta1.LinkStatus{up}, []v1beta1.LinkStatus{off}); len(e) != 1 || !strings.Contains(e[0].message, "the peer is off") {
		t.Errorf("a link down must say why: %+v", e)
	}
	if e := linkEvents(nil, []v1beta1.LinkStatus{moved}); len(e) != 1 || !strings.Contains(e[0].message, "VNI 5001") {
		t.Errorf("a link over vxlan must say its VNI: %+v", e)
	}
}

func TestWriteMetrics(t *testing.T) {
	links := map[string][]v1beta1.LinkStatus{
		"lab/r1": {
			{UID: 7, LocalIntf: "eth1", PeerPod: "r2", State: v1beta1.LinkUp, Transport: "veth", Node: "w1"},
			{UID: 8, LocalIntf: "eth9", PeerPod: "localhost", State: v1beta1.LinkUp, Transport: "macvlan", Node: "w1"},
		},
	}
	read := func(name string) (linkCounters, bool) {
		if name != wireutil.HostEndName("lab", "r1", 7) {
			return linkCounters{}, false
		}
		return linkCounters{rxBytes: 100, txBytes: 200, shapingDrops: 3, rps: true}, true
	}
	var b strings.Builder
	writeMetrics(&b, links, read)
	out := b.String()
	for _, want := range []string{
		`meshnet_link_up{namespace="lab",pod="r1",interface="eth1",peer_pod="r2",uid="7"} 1`,
		`meshnet_link_info{namespace="lab",pod="r1",interface="eth9",peer_pod="localhost",uid="8",state="up",transport="macvlan",node="w1"} 1`,
		`meshnet_link_receive_bytes_total{namespace="lab",pod="r1",interface="eth1",peer_pod="r2",uid="7"} 100`,
		`meshnet_link_transmit_bytes_total{namespace="lab",pod="r1",interface="eth1",peer_pod="r2",uid="7"} 200`,
		`meshnet_link_shaping_drops_total{namespace="lab",pod="r1",interface="eth1",peer_pod="r2",uid="7"} 3`,
		`meshnet_link_rps{namespace="lab",pod="r1",interface="eth1",peer_pod="r2",uid="7"} 1`,
		"# TYPE meshnet_link_receive_bytes_total counter",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics lack %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, `meshnet_link_receive_bytes_total{namespace="lab",pod="r1",interface="eth9"`) {
		t.Errorf("an uplink has no node end and must have no counters:\n%s", out)
	}
}

// The links' status is written with the plumbing error, only when it changes, and each change is an
// event on the Topology.
func TestUpdateReconcileStatusLinks(t *testing.T) {
	InitLogger()
	p1 := createFakePodTopology("p1", "default", "10.0.0.1", "/proc/1/ns/net", []string{"p2"})
	client, err := fakeTopology.NewSimpleClientset(p1)
	if err != nil {
		t.Fatalf("fake clientset: %v", err)
	}
	rec := record.NewFakeRecorder(10)
	m := &Meshnet{tClient: client, topoCache: NewTopologyCache(), recorder: rec}
	m.topoCache.Put(p1)
	ctx := context.Background()
	drain := func() []string {
		var got []string
		for {
			select {
			case e := <-rec.Events:
				got = append(got, e)
			default:
				return got
			}
		}
	}
	read := func() []v1beta1.LinkStatus {
		u, err := client.Topology("default").Unstructured(ctx, "p1", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("reading p1: %v", err)
		}
		return linksOf(u)
	}

	up := []v1beta1.LinkStatus{{UID: 1, LocalIntf: "eth1", PeerPod: "p2", State: v1beta1.LinkUp, Transport: "veth", IfIndex: 5}}
	if err := m.updateReconcileStatus(ctx, p1, "", podReconcile{owned: true, links: up}); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if got := read(); len(got) != 1 || got[0].State != v1beta1.LinkUp {
		t.Fatalf("status.links = %+v", got)
	}
	if ev := drain(); len(ev) != 1 || !strings.HasPrefix(ev[0], "Normal Wired") {
		t.Fatalf("events after the first write: %v", ev)
	}
	if err := m.updateReconcileStatus(ctx, p1, "", podReconcile{owned: true, links: up}); err != nil {
		t.Fatalf("same write: %v", err)
	}
	if ev := drain(); len(ev) != 0 {
		t.Errorf("an unchanged status made events: %v", ev)
	}
	off := []v1beta1.LinkStatus{{UID: 1, LocalIntf: "eth1", PeerPod: "p2", State: v1beta1.LinkOff, IfIndex: 5}}
	if err := m.updateReconcileStatus(ctx, p1, "", podReconcile{owned: true, links: off}); err != nil {
		t.Fatalf("off: %v", err)
	}
	if ev := drain(); len(ev) != 1 || !strings.HasPrefix(ev[0], "Normal LinkDown") {
		t.Errorf("events after the peer went: %v", ev)
	}
	// A pass that does not own the pod leaves its links alone.
	if err := m.updateReconcileStatus(ctx, p1, "", podReconcile{}); err != nil {
		t.Fatalf("not owned: %v", err)
	}
	if got := read(); len(got) != 1 || got[0].State != v1beta1.LinkOff {
		t.Errorf("a pass that does not own the pod changed its links: %+v", got)
	}
}
