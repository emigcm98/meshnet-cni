package wireutil

import (
	"fmt"
	"net"
	"strings"

	"github.com/safchain/ethtool"
	"github.com/vishvananda/netlink"
)

// netem drops, duplicates, reorders and corrupts whole frames, and a frame can hold many TCP segments:
// a sender with offloads hands over frames up to 64 KB, and across nodes GRO merges segments on the way
// in. A direction whose shaping does any of the four gets a segmenting hop in front of its node end, a
// veth pair on the node whose entry has no segmentation offload, so the kernel cuts every frame into
// its segments as the entry transmits it, and whose exit redirects the segments into the node end.
const (
	segmenterInPrefix  = "ms"
	segmenterOutPrefix = "mt"
)

// segmenterNames are the entry and the exit of the segmenting hop of a node end.
func segmenterNames(hostEnd string) (string, string) {
	h := strings.TrimPrefix(hostEnd, hostEndPrefix)
	return segmenterInPrefix + h, segmenterOutPrefix + h
}

// SegmenterName is the entry of the segmenting hop of a node end, the link the node keeps of it.
func SegmenterName(hostEnd string) string {
	in, _ := segmenterNames(hostEnd)
	return in
}

// IsSegmenter tells the entry of a segmenting hop, which is what the node keeps of one by name.
func IsSegmenter(name string) bool {
	return strings.HasPrefix(name, segmenterInPrefix) && len(name) == 10
}

// HostEndOfSegmenter returns the node end a segmenting hop serves.
func HostEndOfSegmenter(name string) string {
	return hostEndPrefix + strings.TrimPrefix(name, segmenterInPrefix)
}

// PerPacket says whether s drops, duplicates, reorders or corrupts packets, which netem does per
// packet only on frames already cut into segments.
func (s *Shaping) PerPacket() bool {
	return s != nil && (s.Loss != "" || s.Duplicate != "" || s.Reorder != "" || s.Corrupt != "")
}

// Validate says whether s can be applied, as ShapeHostEnd would find.
func (s *Shaping) Validate() error {
	if s == nil {
		return nil
	}
	_, err := s.BuildQdisc(0)
	return err
}

// SetSegmenter gives the node end hostEnd its segmenting hop, or takes it away. The joins redirect
// into the hop whenever it is there, so it is set before them. A hop left half made is taken away,
// so frames never go into one that does not reach the node end.
func SetSegmenter(hostEnd string, on bool) error {
	if !on {
		return RemoveSegmenter(hostEnd)
	}
	if err := ensureSegmenter(hostEnd); err != nil {
		_ = RemoveSegmenter(hostEnd)
		return err
	}
	return nil
}

// RemoveSegmenter deletes the segmenting hop of a node end, both of its ends; none is fine.
func RemoveSegmenter(hostEnd string) error {
	in, _ := segmenterNames(hostEnd)
	return delLinkByName(in)
}

func ensureSegmenter(hostEnd string) error {
	he, err := netlink.LinkByName(hostEnd)
	if err != nil {
		return fmt.Errorf("node end %s: %w", hostEnd, err)
	}
	in, out := segmenterNames(hostEnd)
	if l, err := netlink.LinkByName(in); err == nil {
		if _, ok := l.(*netlink.Veth); !ok {
			if err := netlink.LinkDel(l); err != nil {
				return fmt.Errorf("failed to remove %s, which is not a segmenting hop: %w", in, err)
			}
		}
	}
	if _, err := netlink.LinkByName(in); err != nil {
		hop := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: in, MTU: HostEndMTU}, PeerName: out, PeerMTU: HostEndMTU}
		if err := netlink.LinkAdd(hop); err != nil {
			return fmt.Errorf("failed to create the segmenting hop (%s, %s): %w", in, out, err)
		}
		quietHostLink(in)
		quietHostLink(out)
	}
	if err := switchOff(in, isSegmentation); err != nil {
		return fmt.Errorf("failed to switch the segmentation offloads of %s off: %w", in, err)
	}
	if err := switchOff(out, func(f string) bool { return f == "rx-gro" }); err != nil {
		return fmt.Errorf("failed to switch GRO of %s off: %w", out, err)
	}
	var lout netlink.Link
	for _, name := range []string{in, out} {
		l, err := netlink.LinkByName(name)
		if err != nil {
			return fmt.Errorf("failed to find %s: %w", name, err)
		}
		if l.Attrs().Flags&net.FlagUp == 0 {
			if err := netlink.LinkSetUp(l); err != nil {
				return fmt.Errorf("failed to set %s up: %w", name, err)
			}
		}
		lout = l
	}
	// The exit hands the segments on through a backlog too, so it keeps each flow on one CPU as well.
	ensureRPS(out)
	return redirect(lout, he)
}

// isSegmentation tells the features that let a device take frames bigger than a segment: every kind
// of segmentation offload, generic segmentation and frag lists.
func isSegmentation(feature string) bool {
	return strings.HasSuffix(feature, "-segmentation") || feature == "tx-gso-list"
}

// switchOff switches off the features of a node link that match and are on, and leaves alone those
// it has not, which differ between kernels; the kernel keeps those it does not let change.
func switchOff(name string, match func(string) bool) error {
	h, err := ethtool.NewEthtool()
	if err != nil {
		return err
	}
	defer h.Close()
	features, err := h.Features(name)
	if err != nil {
		return err
	}
	change := map[string]bool{}
	for f, on := range features {
		if on && match(f) {
			change[f] = false
		}
	}
	if len(change) == 0 {
		return nil
	}
	return h.Change(name, change)
}

// entryOf is where frames for the node end l go in: its segmenting hop when it has one.
func entryOf(l netlink.Link) netlink.Link {
	in, _ := segmenterNames(l.Attrs().Name)
	if hop, err := netlink.LinkByName(in); err == nil {
		return hop
	}
	return l
}
