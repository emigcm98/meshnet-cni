package wireutil_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/emigcm98/meshnet-cni/utils/wireutil"
)

// A node end steers each flow to one CPU from the start, on every rx queue; one that lost it, as the
// node ends made before RPS, gets it back on the next pass without being made again; and the exit of
// a segmenting hop steers too.
func TestNodeEndRPS(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges. Run test with sudo")
	}
	mask := wireutil.RPSMask()
	if mask.Sign() == 0 {
		t.Fatalf("no CPU to steer to: %s=%q", wireutil.RPSEnv, os.Getenv(wireutil.RPSEnv))
	}
	netNS := newNS(t)
	cfg := wireutil.PodLinkConfig{PodName: "a", PeerPodName: "b", LinkUID: 9, KubeNs: "rps", LocalIntf: "eth1"}
	end := wireutil.NewPodEnd(netNS.Path(), cfg, 1450)
	if _, err := wireutil.EnsurePodEnd(end); err != nil {
		t.Fatalf("EnsurePodEnd: %v", err)
	}
	t.Cleanup(func() { _ = wireutil.RemoveHostLink(end.Host) })
	if !wireutil.HasRPS(end.Host, mask) {
		t.Fatalf("a new node end %s does not steer to %s", end.Host, wireutil.FormatMask(mask))
	}
	index := linkIndex(t, netNS, "eth1")
	queues, _ := filepath.Glob("/sys/class/net/" + end.Host + "/queues/rx-*/rps_cpus")
	for _, q := range queues {
		if err := os.WriteFile(q, []byte("0"), 0o644); err != nil {
			t.Fatalf("clearing %s: %v", q, err)
		}
	}
	if wireutil.HasRPS(end.Host, mask) {
		t.Fatalf("HasRPS still says yes with RPS cleared")
	}
	if _, err := wireutil.EnsurePodEnd(end); err != nil {
		t.Fatalf("EnsurePodEnd again: %v", err)
	}
	if !wireutil.HasRPS(end.Host, mask) {
		t.Errorf("a node end without RPS did not get it on the next pass")
	}
	if got := linkIndex(t, netNS, "eth1"); got != index {
		t.Errorf("the pod end was made again (ifindex %d, was %d) to set RPS", got, index)
	}

	if err := wireutil.SetSegmenter(end.Host, true); err != nil {
		t.Fatalf("SetSegmenter: %v", err)
	}
	t.Cleanup(func() { _ = wireutil.RemoveSegmenter(end.Host) })
	exit := "mt" + end.Host[2:]
	if !wireutil.HasRPS(exit, mask) {
		t.Errorf("the hop's exit %s does not steer to %s", exit, wireutil.FormatMask(mask))
	}
}
