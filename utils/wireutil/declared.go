package wireutil

import (
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/safchain/ethtool"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// AddrProtocol marks the addresses meshnet puts on an interface (IFA_PROTO, kernel 5.18 and later),
// so a declaration that drops one removes it and never one the workload added.
const AddrProtocol = 0x6d

// The properties of a declaration, each with its digest in the interface's tag.
const (
	propAddresses = "a"
	propMAC       = "m"
	propMTU       = "u"
	propTxQLen    = "q"
	propOffloads  = "o"
)

// Declared is what a link declares of its end in a pod. It is applied when the interface is made and
// whenever the declaration of a property changes; in between the interface is the workload's.
type Declared struct {
	Addresses []string // CIDRs of either family, local_ip first
	MAC       string
	MTU       int  // the link's, already settled between both ends
	TxQLen    int  // the link's, or LINK_TXQUEUELEN
	Offloads  bool // keep the kernel's tx offloads
	// kernelOffloads leaves the offloads as the kernel has them unless Offloads asks to keep them,
	// for an uplink, which meshnet never switched off.
	kernelOffloads bool
}

// NewDeclared settles what cfg declares of its end: mtu is the link's, and the queue length falls
// back to LINK_TXQUEUELEN.
func NewDeclared(cfg PodLinkConfig, mtu int) Declared {
	d := Declared{MAC: cfg.MAC, MTU: mtu, TxQLen: cfg.TxQLen, Offloads: cfg.Offloads}
	if cfg.LocalIP != "" {
		d.Addresses = append(d.Addresses, cfg.LocalIP)
	}
	d.Addresses = append(d.Addresses, cfg.Addresses...)
	if d.TxQLen <= 0 {
		d.TxQLen = GetLinkTxQLen()
	}
	return d
}

// NewUplinkDeclared settles what cfg declares of an uplink: what it does not declare, its MTU, queue
// length and offloads, stays the kernel's.
func NewUplinkDeclared(cfg PodLinkConfig) Declared {
	d := Declared{MAC: cfg.MAC, MTU: cfg.MTU, TxQLen: cfg.TxQLen, Offloads: cfg.Offloads, kernelOffloads: true}
	if cfg.LocalIP != "" {
		d.Addresses = append(d.Addresses, cfg.LocalIP)
	}
	d.Addresses = append(d.Addresses, cfg.Addresses...)
	return d
}

// Digests are the digests of each property of a declaration, by property.
type Digests map[string]string

func digest(s string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum32())
}

// Digests returns the digest of each property of d. The addresses count as a set.
func (d Declared) Digests() Digests {
	addrs := make([]string, 0, len(d.Addresses))
	for _, a := range d.Addresses {
		if parsed, err := netlink.ParseAddr(a); err == nil {
			a = parsed.IPNet.String()
		}
		addrs = append(addrs, a)
	}
	sort.Strings(addrs)
	return Digests{
		propAddresses: digest(strings.Join(addrs, ",")),
		propMAC:       digest(strings.ToLower(d.MAC)),
		propMTU:       digest(strconv.Itoa(d.MTU)),
		propTxQLen:    digest(strconv.Itoa(d.TxQLen)),
		propOffloads:  digest(strconv.FormatBool(d.Offloads)),
	}
}

func (g Digests) String() string {
	keys := make([]string, 0, len(g))
	for k := range g {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+g[k])
	}
	return strings.Join(parts, ",")
}

func parseDigests(s string) Digests {
	g := Digests{}
	for _, part := range strings.Split(s, ",") {
		if k, v, ok := strings.Cut(part, "="); ok && k != "" {
			g[k] = v
		}
	}
	return g
}

// changed says whether property p must be applied, given the digests of the tag. A property the tag
// has no digest for, one added after the interface was tagged, counts as never declared.
func (g Digests) changed(p string, cur Digests) bool {
	prev, ok := g[p]
	if !ok {
		prev = digest("")
	}
	return prev != cur[p]
}

// Declare applies d to the interface name of the netns at nsPath and tags it as the end of link uid
// towards peer. A fresh or untagged interface gets all of d; any other gets each property whose digest
// differs from its tag's, so what the workload changed of the others stays. An interface tagged
// before declarations had digests is adopted: its declared addresses are marked as meshnet's and
// nothing else changes, since the wiring of that time applied the rest.
func Declare(nsPath, name string, uid int64, peer string, d Declared, fresh bool) error {
	podNs, err := ns.GetNS(nsPath)
	if err != nil {
		return fmt.Errorf("could not open netns %s: %w", nsPath, err)
	}
	defer podNs.Close()
	return podNs.Do(func(_ ns.NetNS) error {
		l, err := netlink.LinkByName(name)
		if err != nil {
			return fmt.Errorf("interface %s not found inside netns %s: %w", name, nsPath, err)
		}
		cur := d.Digests()
		_, _, prev, hasDigests, tagged := parseLinkTag(l.Attrs().Alias)
		switch {
		case fresh || !tagged:
			// Untagged, it was never declared: made by a pass that failed before tagging it.
			err = applyDeclared(l, d, nil, cur)
		case !hasDigests:
			err = markAddresses(l, d.Addresses)
		default:
			err = applyDeclared(l, d, prev, cur)
		}
		if err != nil {
			return fmt.Errorf("%s inside %s: %w", name, nsPath, err)
		}
		tag := LinkTagWith(uid, peer, cur)
		if l.Attrs().Alias != tag {
			if err := netlink.LinkSetAlias(l, tag); err != nil {
				return fmt.Errorf("failed to tag %s inside %s: %w", name, nsPath, err)
			}
		}
		return nil
	})
}

// applyDeclared applies every property of d when prev is nil, and otherwise those whose digest moved.
func applyDeclared(l netlink.Link, d Declared, prev, cur Digests) error {
	apply := func(p string) bool { return prev == nil || prev.changed(p, cur) }
	if apply(propMTU) && d.MTU > 0 && l.Attrs().MTU != d.MTU {
		if err := netlink.LinkSetMTU(l, d.MTU); err != nil {
			return fmt.Errorf("failed to set MTU %d: %w", d.MTU, err)
		}
	}
	if apply(propMAC) && d.MAC != "" {
		mac, err := net.ParseMAC(d.MAC)
		if err != nil {
			return fmt.Errorf("invalid MAC %q: %w", d.MAC, err)
		}
		if l.Attrs().HardwareAddr.String() != mac.String() {
			if err := netlink.LinkSetHardwareAddr(l, mac); err != nil {
				return fmt.Errorf("failed to set MAC %s: %w", mac, err)
			}
		}
	}
	if apply(propTxQLen) && d.TxQLen > 0 && l.Attrs().TxQLen != d.TxQLen {
		if err := netlink.LinkSetTxQLen(l, d.TxQLen); err != nil {
			log.Warnf("Declare: failed to set txqueuelen %d on %s: %v", d.TxQLen, l.Attrs().Name, err)
		}
	}
	if apply(propOffloads) && !(d.kernelOffloads && !d.Offloads) {
		if h, _ := ethtool.NewEthtool(); h != nil {
			if err := setTxOffload(h, l.Attrs().Name, d.Offloads); err != nil {
				log.Warnf("Declare: failed to switch the tx offloads of %s %s: %v", l.Attrs().Name, onOff(d.Offloads), err)
			}
			h.Close()
		}
	}
	if apply(propAddresses) {
		return setAddresses(l, d.Addresses)
	}
	return nil
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func parseAddrs(addrs []string) ([]*netlink.Addr, error) {
	out := make([]*netlink.Addr, 0, len(addrs))
	for _, a := range addrs {
		parsed, err := netlink.ParseAddr(a)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %w", a, err)
		}
		parsed.Protocol = AddrProtocol
		if parsed.IP.To4() == nil {
			// Usable at once: a routing daemon that starts with the pod cannot bind a tentative address.
			parsed.Flags |= unix.IFA_F_NODAD
		}
		out = append(out, parsed)
	}
	return out, nil
}

// setAddresses leaves on l exactly the declared addresses among those meshnet put there, and never
// touches the workload's own.
func setAddresses(l netlink.Link, declared []string) error {
	want, err := parseAddrs(declared)
	if err != nil {
		return err
	}
	keep := make(map[string]bool, len(want))
	for _, a := range want {
		keep[a.IPNet.String()] = true
	}
	have, err := netlink.AddrList(l, netlink.FAMILY_ALL)
	if err != nil {
		return fmt.Errorf("failed to list the addresses: %w", err)
	}
	for _, a := range have {
		if a.Protocol != AddrProtocol || keep[a.IPNet.String()] {
			continue
		}
		if a.IP.To4() != nil {
			// Removing a primary address takes the secondaries of its subnet along unless promoted.
			promoteSecondaries(l.Attrs().Name)
		}
		if err := netlink.AddrDel(l, &a); err != nil {
			return fmt.Errorf("failed to remove %s: %w", a.IPNet, err)
		}
	}
	for _, a := range want {
		if err := netlink.AddrReplace(l, a); err != nil {
			return fmt.Errorf("failed to add %s: %w", a.IPNet, err)
		}
	}
	return nil
}

// markAddresses marks the declared addresses as meshnet's, adding any that is missing; replacing an
// address that is there changes nothing else of it.
func markAddresses(l netlink.Link, declared []string) error {
	want, err := parseAddrs(declared)
	if err != nil {
		return err
	}
	for _, a := range want {
		if err := netlink.AddrReplace(l, a); err != nil {
			return fmt.Errorf("failed to mark %s: %w", a.IPNet, err)
		}
	}
	return nil
}

// promoteSecondaries is written from inside the netns of the interface, where /proc/sys/net is its.
func promoteSecondaries(name string) {
	_ = os.WriteFile(fmt.Sprintf("/proc/sys/net/ipv4/conf/%s/promote_secondaries", name), []byte("1"), 0o644)
}

func setTxOffload(h *ethtool.Ethtool, name string, on bool) error {
	if !on {
		return disableTxOffload(h, name)
	}
	features := make(map[string]bool, len(txOffloadDisabledMap))
	for k := range txOffloadDisabledMap {
		features[k] = true
	}
	// Features the driver cannot change are masked by the kernel, the rest applied.
	return h.Change(name, features)
}
