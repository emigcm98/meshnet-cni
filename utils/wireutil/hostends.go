package wireutil

import (
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"strings"

	"github.com/containernetworking/plugins/pkg/ns"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Every end of a link in a pod is a veth whose other end stays on the node, so a pod that goes away
// takes only its own pair and its neighbour keeps its interface. The daemon joins the node ends of a
// link with tc redirects, to each other on one node or through a vxlan across nodes, and sets a node
// end down for its pod to see no carrier.
const (
	hostEndPrefix   = "mn"
	hostVxlanPrefix = "mx"
	podTempPrefix   = "mp"
	// LegacyHostVethPrefix names the host veths of the previous wiring, which joined two pods directly.
	LegacyHostVethPrefix = "vnm-"
	vxlanPort            = 4789
	// HostEndMTU is the MTU of every node end, the most a veth takes: the pod ends alone decide the
	// MTU of a link, as with a direct veth or a cable.
	HostEndMTU = 65535
)

func linkHash(kubeNs, pod string, uid int64) string {
	h := fnv.New32a()
	fmt.Fprintf(h, "%s/%s/%d", kubeNs, pod, uid)
	return fmt.Sprintf("%08x", h.Sum32())
}

// HostEndName is the node end of the pod's interface for link uid.
func HostEndName(kubeNs, pod string, uid int64) string {
	return hostEndPrefix + linkHash(kubeNs, pod, uid)
}

// HostVxlanName is the vxlan that carries link uid to the peer's node, on the node of pod.
func HostVxlanName(kubeNs, pod string, uid int64) string {
	return hostVxlanPrefix + linkHash(kubeNs, pod, uid)
}

func podTempName(kubeNs, pod string, uid int64) string {
	return podTempPrefix + linkHash(kubeNs, pod, uid)
}

// IsHostEnd and IsHostVxlan tell the node's links meshnet owns.
func IsHostEnd(name string) bool { return strings.HasPrefix(name, hostEndPrefix) && len(name) == 10 }
func IsHostVxlan(name string) bool {
	return strings.HasPrefix(name, hostVxlanPrefix) && len(name) == 10
}

// HostEndOfVxlan returns the node end a host vxlan belongs to.
func HostEndOfVxlan(name string) string {
	return hostEndPrefix + strings.TrimPrefix(name, hostVxlanPrefix)
}

// PodEnd describes the interface a link gives a pod.
type PodEnd struct {
	NetNS    string
	Name     string // the interface in the pod
	Host     string // its node end, HostEndName
	Temp     string // the name it has on the node before it moves in
	UID      int64
	Peer     string
	Declared Declared
}

// NewPodEnd fills the node-side names of the end of link cfg in the pod's netns, with the link's mtu.
func NewPodEnd(netNS string, cfg PodLinkConfig, mtu int) PodEnd {
	return PodEnd{
		NetNS:    netNS,
		Name:     cfg.LocalIntf,
		Host:     HostEndName(cfg.KubeNs, cfg.PodName, cfg.LinkUID),
		Temp:     podTempName(cfg.KubeNs, cfg.PodName, cfg.LinkUID),
		UID:      cfg.LinkUID,
		Peer:     cfg.PeerPodName,
		Declared: NewDeclared(cfg, mtu),
	}
}

// EnsurePodEnd makes sure the pod has its interface as a veth paired with its node end, and replaces
// whatever else holds the name: a direct pair to another pod or a vxlan from the previous wiring, or
// a leftover. It returns whether it made the pair. A new interface gets all of its declaration before
// it takes its name and comes up, so the workload never sees it half made; from then on its admin
// state belongs to the workload, and Declare applies only what the declaration changes.
func EnsurePodEnd(e PodEnd) (bool, error) {
	podNs, err := ns.GetNS(e.NetNS)
	if err != nil {
		return false, fmt.Errorf("could not open netns %s: %w", e.NetNS, err)
	}
	defer podNs.Close()

	host, hostErr := netlink.LinkByName(e.Host)
	var podIndex int
	var podKind string
	tempInPod := false
	if err := podNs.Do(func(_ ns.NetNS) error {
		if l, err := netlink.LinkByName(e.Name); err == nil {
			podIndex, podKind = l.Attrs().Index, l.Type()
		}
		if _, err := netlink.LinkByName(e.Temp); err == nil {
			tempInPod = true
		}
		return nil
	}); err != nil {
		return false, err
	}

	paired := false
	if hostErr == nil && podIndex != 0 && podKind == "veth" {
		if v, ok := host.(*netlink.Veth); ok {
			if peer, err := netlink.VethPeerIndex(v); err == nil && peer == podIndex {
				paired = true
			}
		}
	}

	created := false
	if !paired && !(tempInPod && hostErr == nil) {
		// Whatever holds the name goes, and so does a node end that is not paired with it.
		if podIndex != 0 {
			if err := podNs.Do(func(_ ns.NetNS) error { return delLinkByName(e.Name) }); err != nil {
				return false, fmt.Errorf("failed to remove %s (%s) from netns %s: %w", e.Name, podKind, e.NetNS, err)
			}
			log.Infof("EnsurePodEnd: replaced %s (%s) in %s with an end paired on the node", e.Name, podKind, e.NetNS)
		}
		if tempInPod {
			_ = podNs.Do(func(_ ns.NetNS) error { return delLinkByName(e.Temp) })
		}
		if hostErr == nil {
			_ = netlink.LinkDel(host)
		}
		if err := makePair(e, podNs); err != nil {
			return false, err
		}
		created, tempInPod = true, true
	}

	// The node end is the daemon's: always at HostEndMTU, which also lifts the node ends of 2.8, and
	// steering each flow to one CPU, which also reaches the node ends made before RPS.
	if h, err := netlink.LinkByName(e.Host); err == nil && h.Attrs().MTU != HostEndMTU {
		if err := netlink.LinkSetMTU(h, HostEndMTU); err != nil {
			log.Warnf("EnsurePodEnd: failed to set MTU %d on node end %s: %v", HostEndMTU, e.Host, err)
		}
	}
	ensureRPS(e.Host)

	if !tempInPod {
		return false, Declare(e.NetNS, e.Name, e.UID, e.Peer, e.Declared, false)
	}
	// Made now or by a pass that failed before the rename: declared in full, then named and up. A
	// declaration that fails takes the pair along, untagged as it is, so nothing is left half made.
	if err := Declare(e.NetNS, e.Temp, e.UID, e.Peer, e.Declared, true); err != nil {
		_ = podNs.Do(func(_ ns.NetNS) error { return delLinkByName(e.Temp) })
		return created, err
	}
	return created, podNs.Do(func(_ ns.NetNS) error {
		l, err := netlink.LinkByName(e.Temp)
		if err != nil {
			return fmt.Errorf("interface %s not found inside netns %s: %w", e.Temp, e.NetNS, err)
		}
		if err := netlink.LinkSetName(l, e.Name); err != nil {
			return fmt.Errorf("failed to rename %s -> %s inside %s: %w", e.Temp, e.Name, e.NetNS, err)
		}
		if err := netlink.LinkSetUp(l); err != nil {
			return fmt.Errorf("failed to set %s up inside %s: %w", e.Name, e.NetNS, err)
		}
		return nil
	})
}

// makePair creates the pod end and its node end on the node and moves the pod end in under its
// temporary name; the node end stays down until the daemon joins it.
func makePair(e PodEnd, podNs ns.NetNS) error {
	mtu := e.Declared.MTU
	if mtu <= 0 {
		mtu = 1500
	}
	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: e.Host, MTU: HostEndMTU, TxQLen: GetLinkTxQLen()},
		PeerName:  e.Temp,
		PeerMTU:   uint32(mtu),
	}
	if err := netlink.LinkAdd(veth); err != nil {
		return fmt.Errorf("failed to create the pair (%s, %s): %w", e.Host, e.Temp, err)
	}
	quietHostLink(e.Host)
	temp, err := netlink.LinkByName(e.Temp)
	if err != nil {
		return fmt.Errorf("failed to find %s after creating it: %w", e.Temp, err)
	}
	if err := netlink.LinkSetNsFd(temp, int(podNs.Fd())); err != nil {
		_ = delLinkByName(e.Host)
		return fmt.Errorf("failed to move %s into netns %s: %w", e.Temp, e.NetNS, err)
	}
	return nil
}

// quietHostLink keeps the node from speaking on a link of the lab: no IPv6 of its own on it, so no
// router or neighbour discovery from the node reaches the pods.
func quietHostLink(name string) {
	_ = os.WriteFile("/proc/sys/net/ipv6/conf/"+name+"/disable_ipv6", []byte("1"), 0o644)
}

func delLinkByName(name string) error {
	l, err := netlink.LinkByName(name)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if errors.As(err, &notFound) {
			return nil
		}
		return err
	}
	return netlink.LinkDel(l)
}

// RemoveHostLink deletes a link of the node by name; one that is not there is fine.
func RemoveHostLink(name string) error { return delLinkByName(name) }

// HostLinkExists says whether the node has a link of that name.
func HostLinkExists(name string) bool {
	_, err := netlink.LinkByName(name)
	return err == nil
}

// redirect makes everything entering from leave through to, as tc does with
// `matchall action mirred egress redirect`.
func redirect(from, to netlink.Link) error {
	q := &netlink.Clsact{QdiscAttrs: netlink.QdiscAttrs{
		LinkIndex: from.Attrs().Index,
		Handle:    netlink.MakeHandle(0xffff, 0),
		Parent:    netlink.HANDLE_CLSACT,
	}}
	if err := netlink.QdiscReplace(q); err != nil {
		return fmt.Errorf("failed to add clsact to %s: %w", from.Attrs().Name, err)
	}
	// matchall cannot change a filter in place, so one already pointing at to stays and any other
	// is replaced.
	existing, err := netlink.FilterList(from, netlink.HANDLE_MIN_INGRESS)
	if err != nil {
		return fmt.Errorf("failed to list the filters of %s: %w", from.Attrs().Name, err)
	}
	if len(existing) == 1 && redirectsTo(existing[0], to.Attrs().Index) {
		return nil
	}
	for _, f := range existing {
		if err := netlink.FilterDel(f); err != nil {
			return fmt.Errorf("failed to remove an old filter of %s: %w", from.Attrs().Name, err)
		}
	}
	f := &netlink.MatchAll{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: from.Attrs().Index,
			Parent:    netlink.HANDLE_MIN_INGRESS,
			Handle:    netlink.MakeHandle(0, 1),
			Priority:  1,
			Protocol:  unix.ETH_P_ALL,
		},
		Actions: []netlink.Action{netlink.NewMirredAction(to.Attrs().Index)},
	}
	if err := netlink.FilterAdd(f); err != nil {
		return fmt.Errorf("failed to redirect %s to %s: %w", from.Attrs().Name, to.Attrs().Name, err)
	}
	return nil
}

func redirectsTo(f netlink.Filter, ifindex int) bool {
	m, ok := f.(*netlink.MatchAll)
	if !ok || len(m.Actions) != 1 {
		return false
	}
	a, ok := m.Actions[0].(*netlink.MirredAction)
	return ok && a.MirredAction == netlink.TCA_EGRESS_REDIR && a.Ifindex == ifindex
}

// JoinHostEnds joins two node ends of one node both ways, through the segmenting hop of a node end
// that has one.
func JoinHostEnds(a, b string) error {
	la, err := netlink.LinkByName(a)
	if err != nil {
		return fmt.Errorf("node end %s: %w", a, err)
	}
	lb, err := netlink.LinkByName(b)
	if err != nil {
		return fmt.Errorf("node end %s: %w", b, err)
	}
	if err := redirect(la, entryOf(lb)); err != nil {
		return err
	}
	return redirect(lb, entryOf(la))
}

// JoinVxlan joins a node end to the vxlan that carries its link to the peer's node, making or
// re-pointing the vxlan as needed; what arrives goes through the node end's segmenting hop if it has one.
func JoinVxlan(hostEnd, vxName string, vni int, remote net.IP, parent string) error {
	he, err := netlink.LinkByName(hostEnd)
	if err != nil {
		return fmt.Errorf("node end %s: %w", hostEnd, err)
	}
	parentLink, err := netlink.LinkByName(parent)
	if err != nil {
		return fmt.Errorf("the VXLAN source interface %s: %w", parent, err)
	}
	if l, err := netlink.LinkByName(vxName); err == nil {
		vx, ok := l.(*netlink.Vxlan)
		if !ok || vx.VxlanId != vni || !vx.Group.Equal(remote) || vx.VtepDevIndex != parentLink.Attrs().Index {
			if err := netlink.LinkDel(l); err != nil {
				return fmt.Errorf("failed to remove the outdated %s: %w", vxName, err)
			}
		}
	}
	vx, err := netlink.LinkByName(vxName)
	if err != nil {
		if err := netlink.LinkAdd(&netlink.Vxlan{
			LinkAttrs:    netlink.LinkAttrs{Name: vxName, TxQLen: GetLinkTxQLen()},
			VxlanId:      vni,
			VtepDevIndex: parentLink.Attrs().Index,
			Group:        remote,
			Port:         vxlanPort,
		}); err != nil {
			return fmt.Errorf("failed to add %s (VNI %d to %s): %w", vxName, vni, remote, err)
		}
		quietHostLink(vxName)
		if vx, err = netlink.LinkByName(vxName); err != nil {
			return err
		}
	}
	if vx.Attrs().Flags&net.FlagUp == 0 {
		if err := netlink.LinkSetUp(vx); err != nil {
			return fmt.Errorf("failed to set %s up: %w", vxName, err)
		}
	}
	if err := redirect(he, vx); err != nil {
		return err
	}
	return redirect(vx, entryOf(he))
}

// SetHostEndUp sets the admin state of a node end, which is the carrier its pod's interface sees.
// It does nothing when the state is already that one.
func SetHostEndUp(name string, up bool) error {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return err
	}
	if (l.Attrs().Flags&net.FlagUp != 0) == up {
		return nil
	}
	if up {
		return netlink.LinkSetUp(l)
	}
	return netlink.LinkSetDown(l)
}

// PodEndAdminUp says whether the workload has its interface name in the netns at nsPath up; a pod
// or an interface that is gone counts as down.
func PodEndAdminUp(nsPath, name string) bool {
	if nsPath == "" {
		return false
	}
	podNs, err := ns.GetNS(nsPath)
	if err != nil {
		return false
	}
	defer podNs.Close()
	up := false
	_ = podNs.Do(func(_ ns.NetNS) error {
		if l, err := netlink.LinkByName(name); err == nil {
			up = l.Attrs().Flags&net.FlagUp != 0
		}
		return nil
	})
	return up
}
