package wireutil

import (
	"errors"
	"fmt"
	"math/rand/v2"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
)

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
