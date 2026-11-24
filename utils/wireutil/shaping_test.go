package wireutil_test

import (
	"net"
	"os"
	"testing"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/emigcm98/meshnet-cni/utils/wireutil"
	"github.com/vishvananda/netlink"
)

// rootQdisc returns the root qdisc of the node end name, nil for the veth's default.
func rootQdisc(t *testing.T, name string) netlink.Qdisc {
	t.Helper()
	l, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatalf("node end %s: %v", name, err)
	}
	qdiscs, err := netlink.QdiscList(l)
	if err != nil {
		t.Fatalf("qdiscs of %s: %v", name, err)
	}
	for _, q := range qdiscs {
		if q.Attrs().Parent == netlink.HANDLE_ROOT && q.Type() != "noqueue" {
			return q
		}
	}
	return nil
}

// connectTime is how long a TCP handshake from one netns to a listener in the other takes; the SYN
// crosses towards the listener, so a delay on that direction shows in full.
func connectTime(t *testing.T, from, to ns.NetNS, toIP string) time.Duration {
	t.Helper()
	var lis net.Listener
	in(t, to, "listening", func() error {
		var err error
		lis, err = net.Listen("tcp4", toIP+":9997")
		return err
	})
	defer lis.Close()
	go func() {
		if c, err := lis.Accept(); err == nil {
			c.Close()
		}
	}()
	var took time.Duration
	in(t, from, "connecting", func() error {
		start := time.Now()
		c, err := net.DialTimeout("tcp4", toIP+":9997", 5*time.Second)
		took = time.Since(start)
		if err == nil {
			c.Close()
		}
		return err
	})
	return took
}

// The shaping a peer declares lands on the node end of the other side, delays what goes towards its
// pod, changes in place and comes off.
func TestShapeHostEnd(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges. Run test with sudo")
	}
	nsA, nsB := newNS(t), newNS(t)
	a := ensure(t, nsA, "a", "b", "eth1", "10.56.0.1/30", 6)
	b := ensure(t, nsB, "b", "a", "eth1", "10.56.0.2/30", 6)
	if err := wireutil.JoinHostEnds(a.Host, b.Host); err != nil {
		t.Fatalf("JoinHostEnds: %v", err)
	}
	for _, h := range []string{a.Host, b.Host} {
		if err := wireutil.SetHostEndUp(h, true); err != nil {
			t.Fatalf("SetHostEndUp(%s): %v", h, err)
		}
	}
	if took := connectTime(t, nsA, nsB, "10.56.0.2"); took > 50*time.Millisecond {
		t.Fatalf("an unshaped connect took %v", took)
	}

	delay := &wireutil.Shaping{Delay: "100ms"}
	if err := wireutil.ShapeHostEnd(b.Host, delay); err != nil {
		t.Fatalf("ShapeHostEnd(netem): %v", err)
	}
	want, _ := delay.BuildQdisc(0)
	got, ok := rootQdisc(t, b.Host).(*netlink.Netem)
	if !ok || got.Latency != want.(*netlink.Netem).Latency {
		t.Fatalf("node end %s has %+v, want a netem of 100ms", b.Host, rootQdisc(t, b.Host))
	}
	if took := connectTime(t, nsA, nsB, "10.56.0.2"); took < 100*time.Millisecond {
		t.Errorf("a connect towards b took %v through a 100ms delay", took)
	}
	if rootQdisc(t, a.Host) != nil {
		t.Errorf("the other direction got shaped too")
	}
	// The same shaping again is a no-op, a new one replaces it in place.
	if err := wireutil.ShapeHostEnd(b.Host, delay); err != nil {
		t.Fatalf("ShapeHostEnd(the same): %v", err)
	}
	if err := wireutil.ShapeHostEnd(b.Host, &wireutil.Shaping{Qdisc: "tbf", Rate: "1mbit"}); err != nil {
		t.Fatalf("ShapeHostEnd(tbf): %v", err)
	}
	if q := rootQdisc(t, b.Host); q == nil || q.Type() != "tbf" {
		t.Fatalf("node end %s has %+v, want a tbf", b.Host, q)
	}
	if err := wireutil.ShapeHostEnd(b.Host, nil); err != nil {
		t.Fatalf("ShapeHostEnd(nil): %v", err)
	}
	if q := rootQdisc(t, b.Host); q != nil {
		t.Errorf("node end %s keeps %s with its shaping taken off", b.Host, q.Type())
	}
	if took := connectTime(t, nsA, nsB, "10.56.0.2"); took > 50*time.Millisecond {
		t.Errorf("a connect took %v with the shaping off", took)
	}
}
