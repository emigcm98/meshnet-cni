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

// LinkTag is the alias of the interface of link uid towards peer.
func LinkTag(uid int64, peer string) string {
	return fmt.Sprintf("%s%d/%s", linkTagPrefix, uid, peer)
}

// ParseLinkTag reads a LinkTag back; ok is false for any other alias.
func ParseLinkTag(alias string) (uid int64, peer string, ok bool) {
	rest, found := strings.CutPrefix(alias, linkTagPrefix)
	if !found {
		return 0, "", false
	}
	uidStr, peer, found := strings.Cut(rest, "/")
	if !found {
		return 0, "", false
	}
	uid, err := strconv.ParseInt(uidStr, 10, 64)
	if err != nil {
		return 0, "", false
	}
	return uid, peer, true
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

// TagLinks writes tags (interface name to tag) on the interfaces of the netns at nsPath that exist
// and do not carry them yet; a link made before tags existed is adopted this way.
func TagLinks(nsPath string, tags map[string]string) error {
	if len(tags) == 0 {
		return nil
	}
	podNs, err := ns.GetNS(nsPath)
	if err != nil {
		return fmt.Errorf("could not open netns %s: %w", nsPath, err)
	}
	defer podNs.Close()
	return podNs.Do(func(_ ns.NetNS) error {
		for name, tag := range tags {
			l, err := netlink.LinkByName(name)
			if err != nil || l.Attrs().Alias == tag {
				continue
			}
			if err := netlink.LinkSetAlias(l, tag); err != nil {
				return fmt.Errorf("failed to tag %s: %w", name, err)
			}
		}
		return nil
	})
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
// netns at nsPath as name, with ip when given. A name already there is left as it is.
func MakeMacvlan(nsPath, name, parent, ip string) error {
	if _, exists, err := LinkInfo(nsPath, name); err != nil || exists {
		return err
	}
	parentLink, err := netlink.LinkByName(parent)
	if err != nil {
		return fmt.Errorf("failed to find the parent interface %s: %w", parent, err)
	}
	var addr *netlink.Addr
	if ip != "" {
		if addr, err = netlink.ParseAddr(ip); err != nil {
			return fmt.Errorf("invalid CIDR %q for %s: %w", ip, name, err)
		}
	}

	// Made under a temporary name on the node, since the final one may already be taken there.
	tmp := fmt.Sprintf("mv-%08x", rand.Uint32())
	mv := &netlink.Macvlan{
		LinkAttrs: netlink.LinkAttrs{Name: tmp, ParentIndex: parentLink.Attrs().Index},
		Mode:      netlink.MACVLAN_MODE_BRIDGE,
	}
	if err := netlink.LinkAdd(mv); err != nil {
		return fmt.Errorf("failed to add macvlan on %s: %w", parent, err)
	}
	link, err := netlink.LinkByName(tmp)
	if err != nil {
		return fmt.Errorf("cannot get %s: %w", tmp, err)
	}
	podNs, err := ns.GetNS(nsPath)
	if err != nil {
		_ = netlink.LinkDel(link)
		return fmt.Errorf("could not open netns %s: %w", nsPath, err)
	}
	defer podNs.Close()
	if err := netlink.LinkSetNsFd(link, int(podNs.Fd())); err != nil {
		_ = netlink.LinkDel(link)
		return fmt.Errorf("failed to move %s into netns %s: %w", tmp, nsPath, err)
	}
	return podNs.Do(func(_ ns.NetNS) error {
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
		if addr != nil {
			if err := netlink.AddrAdd(l, addr); err != nil {
				return fmt.Errorf("failed to add %s to %s inside %s: %w", ip, name, nsPath, err)
			}
		}
		return nil
	})
}
