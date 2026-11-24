package wireutil

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
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

// MakeMacvlan hangs a macvlan in bridge mode on the node interface parent and moves it into the
// netns at nsPath as name, up, and returns whether it made it. A name already there is left as it is;
// the declaration of the link is Declare's.
func MakeMacvlan(nsPath, name, parent string) (bool, error) {
	if _, exists, err := LinkInfo(nsPath, name); err != nil || exists {
		return false, err
	}
	parentLink, err := netlink.LinkByName(parent)
	if err != nil {
		return false, fmt.Errorf("failed to find the parent interface %s: %w", parent, err)
	}

	// Made under a temporary name on the node, since the final one may already be taken there.
	tmp := fmt.Sprintf("mv-%08x", rand.Uint32())
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
	podNs, err := ns.GetNS(nsPath)
	if err != nil {
		_ = netlink.LinkDel(link)
		return false, fmt.Errorf("could not open netns %s: %w", nsPath, err)
	}
	defer podNs.Close()
	if err := netlink.LinkSetNsFd(link, int(podNs.Fd())); err != nil {
		_ = netlink.LinkDel(link)
		return false, fmt.Errorf("failed to move %s into netns %s: %w", tmp, nsPath, err)
	}
	return true, podNs.Do(func(_ ns.NetNS) error {
		l, err := netlink.LinkByName(tmp)
		if err != nil {
			return fmt.Errorf("failed to find %s inside netns %s: %w", tmp, nsPath, err)
		}
		if err := netlink.LinkSetName(l, name); err != nil {
			return fmt.Errorf("failed to rename %s -> %s inside %s: %w", tmp, name, nsPath, err)
		}
		if err := netlink.LinkSetUp(l); err != nil {
			return fmt.Errorf("failed to set %s up inside %s: %w", name, nsPath, err)
		}
		return nil
	})
}
