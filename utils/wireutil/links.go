package wireutil

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// linkTagPrefix starts the alias meshnet writes on every interface it wires in a pod, so a link
// removed from the spec can be found and deleted without ever touching the workload's own interfaces.
const linkTagPrefix = "meshnet/"

// maxAlias is the longest alias the kernel keeps (IFALIASZ less the terminating zero).
const maxAlias = 255

// LinkTag is the alias of the interface of link uid towards peer.
func LinkTag(uid int64, peer string) string {
	return fmt.Sprintf("%s%d/%s", linkTagPrefix, uid, peer)
}

// LinkTagWith is LinkTag followed by the digests of what was applied of the link's declaration. A
// tag that would not fit in an alias goes without them, and the interface is adopted again.
func LinkTagWith(uid int64, peer string, g Digests) string {
	tag := LinkTag(uid, peer) + "/" + g.String()
	if len(tag) > maxAlias {
		return LinkTag(uid, peer)
	}
	return tag
}

// ParseLinkTag reads a LinkTag back; ok is false for any other alias.
func ParseLinkTag(alias string) (uid int64, peer string, ok bool) {
	uid, peer, _, _, ok = parseLinkTag(alias)
	return uid, peer, ok
}

// parseLinkTag reads a tag with its digests, if it has them.
func parseLinkTag(alias string) (uid int64, peer string, g Digests, hasDigests, ok bool) {
	rest, found := strings.CutPrefix(alias, linkTagPrefix)
	if !found {
		return 0, "", nil, false, false
	}
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 {
		return 0, "", nil, false, false
	}
	uid, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, "", nil, false, false
	}
	if len(parts) == 3 {
		return uid, parts[1], parseDigests(parts[2]), true, true
	}
	return uid, parts[1], nil, false, true
}

// TaggedLink is an interface meshnet wired, as its tag describes it.
type TaggedLink struct {
	Name string
	UID  int64
	Peer string
}

// PruneLinks deletes from the netns at nsPath every interface tagged for a link whose uid is not in
// keep, and returns what it deleted. Matching on the uid rather than the name leaves alone an
// interface the workload renamed, and an untagged interface is never deleted.
func PruneLinks(nsPath string, keep map[int64]bool) ([]TaggedLink, error) {
	podNs, err := ns.GetNS(nsPath)
	if err != nil {
		return nil, fmt.Errorf("could not open netns %s: %w", nsPath, err)
	}
	defer podNs.Close()
	var removed []TaggedLink
	err = podNs.Do(func(_ ns.NetNS) error {
		list, err := netlink.LinkList()
		if err != nil {
			return err
		}
		for _, l := range list {
			uid, peer, ok := ParseLinkTag(l.Attrs().Alias)
			if !ok || keep[uid] {
				continue
			}
			if err := netlink.LinkDel(l); err != nil {
				var notFound netlink.LinkNotFoundError
				if errors.As(err, &notFound) {
					// The other end of a veth pair deleted just before took it along.
					continue
				}
				return fmt.Errorf("failed to delete %s of removed link %d: %w", l.Attrs().Name, uid, err)
			}
			removed = append(removed, TaggedLink{Name: l.Attrs().Name, UID: uid, Peer: peer})
		}
		return nil
	})
	return removed, err
}

// LinkInfo says whether name exists in the netns at nsPath and what kind of link it is.
func LinkInfo(nsPath, name string) (kind string, exists bool, err error) {
	podNs, err := ns.GetNS(nsPath)
	if err != nil {
		return "", false, fmt.Errorf("could not open netns %s: %w", nsPath, err)
	}
	defer podNs.Close()
	err = podNs.Do(func(_ ns.NetNS) error {
		l, err := netlink.LinkByName(name)
		if err != nil {
			var notFound netlink.LinkNotFoundError
			if errors.As(err, &notFound) {
				return nil
			}
			return err
		}
		kind, exists = l.Type(), true
		return nil
	})
	return kind, exists, err
}

// RemoveLink deletes name from the netns at nsPath; a name that is not there is fine.
func RemoveLink(nsPath, name string) error {
	podNs, err := ns.GetNS(nsPath)
	if err != nil {
		return fmt.Errorf("could not open netns %s: %w", nsPath, err)
	}
	defer podNs.Close()
	return podNs.Do(func(_ ns.NetNS) error {
		l, err := netlink.LinkByName(name)
		if err != nil {
			var notFound netlink.LinkNotFoundError
			if errors.As(err, &notFound) {
				return nil
			}
			return err
		}
		return netlink.LinkDel(l)
	})
}

// EnsureUplink makes sure the netns at nsPath has the uplink name, a macvlan in bridge mode on the node
// interface parent, declared as d, and returns whether it made it. A new one gets all of its
// declaration under a temporary name before it takes its own and comes up, so it never shows on the
// segment with another MAC than its own, and one that fails on the way is removed, for the next pass
// to make again. A name already there is left as it is but for what its declaration changes.
func EnsureUplink(nsPath, name, parent string, uid int64, peer string, d Declared) (bool, error) {
	if _, exists, err := LinkInfo(nsPath, name); err != nil {
		return false, err
	} else if exists {
		return false, Declare(nsPath, name, uid, peer, d, false)
	}
	parentLink, err := netlink.LinkByName(parent)
	if err != nil {
		return false, fmt.Errorf("failed to find the parent interface %s: %w", parent, err)
	}
	podNs, err := ns.GetNS(nsPath)
	if err != nil {
		return false, fmt.Errorf("could not open netns %s: %w", nsPath, err)
	}
	defer podNs.Close()

	// Made under a temporary name on the node, since the final one may already be taken there: one
	// per netns and interface, so a pass that fails reports the same error every time, which the
	// status then holds without a write per pass, and what a pass that died left is found and removed.
	tmp := "mv-" + digest(nsPath+"/"+name)
	_ = delLinkByName(tmp)
	_ = podNs.Do(func(_ ns.NetNS) error { return delLinkByName(tmp) })
	mv := &netlink.Macvlan{
		LinkAttrs: netlink.LinkAttrs{Name: tmp, ParentIndex: parentLink.Attrs().Index},
		Mode:      netlink.MACVLAN_MODE_BRIDGE,
	}
	if err := netlink.LinkAdd(mv); err != nil {
		return false, fmt.Errorf("failed to add macvlan on %s: %w", parent, err)
	}
	link, err := netlink.LinkByName(tmp)
	if err != nil {
		return false, fmt.Errorf("cannot get %s: %w", tmp, err)
	}
	if err := netlink.LinkSetNsFd(link, int(podNs.Fd())); err != nil {
		_ = netlink.LinkDel(link)
		return false, fmt.Errorf("failed to move %s into netns %s: %w", tmp, nsPath, err)
	}
	if err := Declare(nsPath, tmp, uid, peer, d, true); err != nil {
		_ = podNs.Do(func(_ ns.NetNS) error { return delLinkByName(tmp) })
		return false, err
	}
	err = podNs.Do(func(_ ns.NetNS) error {
		l, err := netlink.LinkByName(tmp)
		if err != nil {
			return fmt.Errorf("failed to find %s inside netns %s: %w", tmp, nsPath, err)
		}
		if err := netlink.LinkSetName(l, name); err != nil {
			_ = netlink.LinkDel(l)
			return fmt.Errorf("failed to rename %s -> %s inside %s: %w", tmp, name, nsPath, err)
		}
		// The kernel refuses a MAC that another macvlan up on the parent has, which is what a pod made
		// again finds while the sandbox it had is still there.
		if err := netlink.LinkSetUp(l); err != nil {
			_ = netlink.LinkDel(l)
			return fmt.Errorf("failed to set %s up inside %s: %w", name, nsPath, err)
		}
		return nil
	})
	return err == nil, err
}

// EndState is what the daemon reports of an interface in a pod.
type EndState struct {
	Index   int
	MAC     string
	MTU     int
	AdminUp bool
	Carrier bool
}

// ReadEnds reads names from the netns at nsPath in one visit; a name that is not there is left out.
func ReadEnds(nsPath string, names []string) (map[string]EndState, error) {
	podNs, err := ns.GetNS(nsPath)
	if err != nil {
		return nil, fmt.Errorf("could not open netns %s: %w", nsPath, err)
	}
	defer podNs.Close()
	out := make(map[string]EndState, len(names))
	err = podNs.Do(func(_ ns.NetNS) error {
		for _, name := range names {
			l, err := netlink.LinkByName(name)
			if err != nil {
				continue
			}
			a := l.Attrs()
			out[name] = EndState{
				Index:   a.Index,
				MAC:     a.HardwareAddr.String(),
				MTU:     a.MTU,
				AdminUp: a.Flags&net.FlagUp != 0,
				Carrier: a.RawFlags&unix.IFF_LOWER_UP != 0,
			}
		}
		return nil
	})
	return out, err
}
