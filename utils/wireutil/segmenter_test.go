package wireutil_test

import (
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/emigcm98/meshnet-cni/utils/wireutil"
	"github.com/safchain/ethtool"
	"github.com/vishvananda/netlink"
)

// sendTCP sends n bytes over one TCP stream from netns from to a listener in to.
func sendTCP(t *testing.T, from, to ns.NetNS, toIP string, n int64) {
	t.Helper()
	var lis net.Listener
	in(t, to, "listening", func() error {
		var err error
		lis, err = net.Listen("tcp4", toIP+":9998")
		return err
	})
	defer lis.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if c, err := lis.Accept(); err == nil {
			_, _ = io.Copy(io.Discard, c)
			c.Close()
		}
	}()
	in(t, from, "sending", func() error {
		c, err := net.DialTimeout("tcp4", toIP+":9998", 5*time.Second)
		if err != nil {
			return err
		}
		_ = c.SetDeadline(time.Now().Add(60 * time.Second))
		_, err = io.CopyN(c, zeros{}, n)
		c.Close()
		return err
	})
	<-done
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// netemCounts reads what the root qdisc of a node end sent, in bytes, and dropped, in frames.
func netemCounts(t *testing.T, name string) (bytes uint64, drops uint32) {
	t.Helper()
	q := rootQdisc(t, name)
	if q == nil || q.Attrs().Statistics == nil || q.Attrs().Statistics.Basic == nil || q.Attrs().Statistics.Queue == nil {
		t.Fatalf("node end %s has no netem with statistics: %+v", name, q)
	}
	st := q.Attrs().Statistics
	return st.Basic.Bytes, st.Queue.Drops
}

// lossPerSegment is the loss netem applies to one TCP stream through the node end name, in drops per
// segment: the frames it dropped against the bytes it let through, over a full segment of the link.
func lossPerSegment(t *testing.T, from, to ns.NetNS, toIP, name string) float64 {
	t.Helper()
	b0, d0 := netemCounts(t, name)
	sendTCP(t, from, to, toIP, 32<<20)
	b1, d1 := netemCounts(t, name)
	const segment = 1450 + 14
	return float64(d1-d0) * segment / float64(b1-b0)
}

// The segmenting hop cuts frames into segments before the netem of a node end, so a loss is per
// packet: without it a TCP stream reaches the netem in frames of many segments, and each drop takes
// them all. Its entry has no segmentation offload left, and the join goes through it while it is
// there and around it once it is gone.
func TestSegmenter(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges. Run test with sudo")
	}
	nsA, nsB := newNS(t), newNS(t)
	a := ensure(t, nsA, "a", "b", "eth1", "10.57.0.1/30", 8)
	b := ensure(t, nsB, "b", "a", "eth1", "10.57.0.2/30", 8)
	t.Cleanup(func() { _ = wireutil.RemoveSegmenter(b.Host) })
	for _, h := range []string{a.Host, b.Host} {
		if err := wireutil.SetHostEndUp(h, true); err != nil {
			t.Fatalf("SetHostEndUp(%s): %v", h, err)
		}
	}
	if err := wireutil.ShapeHostEnd(b.Host, &wireutil.Shaping{Loss: "5%"}); err != nil {
		t.Fatalf("ShapeHostEnd: %v", err)
	}
	if err := wireutil.JoinHostEnds(a.Host, b.Host); err != nil {
		t.Fatalf("JoinHostEnds: %v", err)
	}
	whole := lossPerSegment(t, nsA, nsB, "10.57.0.2", b.Host)

	if err := wireutil.SetSegmenter(b.Host, true); err != nil {
		t.Fatalf("SetSegmenter(on): %v", err)
	}
	if err := wireutil.JoinHostEnds(a.Host, b.Host); err != nil {
		t.Fatalf("JoinHostEnds through the hop: %v", err)
	}
	h, err := ethtool.NewEthtool()
	if err != nil {
		t.Fatalf("ethtool: %v", err)
	}
	features, err := h.Features(wireutil.SegmenterName(b.Host))
	h.Close()
	if err != nil {
		t.Fatalf("features of the hop: %v", err)
	}
	for f, on := range features {
		if on && strings.HasSuffix(f, "-segmentation") {
			t.Errorf("the hop's entry keeps %s on", f)
		}
	}
	cut := lossPerSegment(t, nsA, nsB, "10.57.0.2", b.Host)
	// How big the frames are without the hop is TCP's and the kernel's to say, so it is only logged.
	t.Logf("a 5%% loss drops %.4f per segment through whole frames, %.4f through the hop", whole, cut)
	if cut < 0.03 || cut > 0.08 {
		t.Errorf("through the hop a 5%% loss dropped %.4f per segment, want about 0.05", cut)
	}

	if err := wireutil.SetSegmenter(b.Host, false); err != nil {
		t.Fatalf("SetSegmenter(off): %v", err)
	}
	if wireutil.HostLinkExists(wireutil.SegmenterName(b.Host)) {
		t.Fatalf("the hop of %s is still there", b.Host)
	}
	if err := wireutil.ShapeHostEnd(b.Host, nil); err != nil {
		t.Fatalf("ShapeHostEnd(nil): %v", err)
	}
	if err := wireutil.JoinHostEnds(a.Host, b.Host); err != nil {
		t.Fatalf("JoinHostEnds around the hop: %v", err)
	}
	if !udpReaches(t, nsA, nsB, "10.57.0.2") {
		t.Errorf("a does not reach b once the hop is gone")
	}
	if _, err := netlink.LinkByName(b.Host); err != nil {
		t.Errorf("the node end went with its hop: %v", err)
	}
}
