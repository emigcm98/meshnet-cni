// Ported from openconfig/kne, third_party/meshnet/daemon/meshnet/controller.go at 5a1676c (Apache 2.0),
// without the gRPC wires, with the macvlan links KNE dropped, and with the VNI the original computes.

package meshnet

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"

	topologyclientv1 "github.com/emigcm98/meshnet-cni/api/clientset/v1beta1"
	"github.com/emigcm98/meshnet-cni/utils/wireutil"
)

const (
	// vxlanBase is added to a link's uid to make its VNI, as the original plugin did, so a node that
	// swaps the DaemonSet keeps talking to one that has not yet.
	vxlanBase = 5000
	// localhost as a link's peer_pod means a macvlan on the node interface named by peer_intf.
	localhost = "localhost"
	// resyncPeriod is how often the informer replays every Topology, which reconciles every local pod.
	resyncPeriod = 60 * time.Second
)

// isNetNSValid returns true if the netns path exists on the host filesystem.
func isNetNSValid(netNS string) bool {
	if netNS == "" {
		return false
	}
	_, err := os.Stat(netNS)
	return err == nil
}

// clearPodAliveStatus removes status.src_ip, status.net_ns, status.container_id, and status.plumbing_error
// from the Topology resource when a local pod container netns becomes invalid.
func (m *Meshnet) clearPodAliveStatus(ctx context.Context, topo *unstructured.Unstructured) error {
	if m.tClient == nil {
		return nil
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latestTopo, err := m.tClient.Topology(topo.GetNamespace()).Unstructured(ctx, topo.GetName(), metav1.GetOptions{})
		if err != nil {
			return err
		}
		unstructured.RemoveNestedField(latestTopo.Object, "status", "src_ip")
		unstructured.RemoveNestedField(latestTopo.Object, "status", "net_ns")
		unstructured.RemoveNestedField(latestTopo.Object, "status", "container_id")
		unstructured.RemoveNestedField(latestTopo.Object, "status", "plumbing_error")

		_, err = m.tClient.Topology(latestTopo.GetNamespace()).Update(ctx, latestTopo, metav1.UpdateOptions{})
		if err == nil && m.topoCache != nil {
			m.topoCache.Put(latestTopo)
		}
		return err
	})
}

// toUnstructured converts any Kubernetes object into *unstructured.Unstructured.
func toUnstructured(obj interface{}) (*unstructured.Unstructured, error) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	if u, ok := obj.(*unstructured.Unstructured); ok {
		return u, nil
	}
	data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{Object: data}, nil
}

// isPodActive checks if a Topology resource has non-empty status.src_ip and status.net_ns.
func isPodActive(topo *unstructured.Unstructured) (srcIP, netNS string, active bool) {
	if topo == nil {
		return "", "", false
	}
	srcIP, _, _ = unstructured.NestedString(topo.Object, "status", "src_ip")
	netNS, _, _ = unstructured.NestedString(topo.Object, "status", "net_ns")
	active = (srcIP != "" && netNS != "")
	return srcIP, netNS, active
}

// parsePodLinks extracts all links from spec.links in a Topology resource.
func parsePodLinks(topo *unstructured.Unstructured) ([]wireutil.PodLinkConfig, error) {
	if topo == nil {
		return nil, nil
	}
	podName := topo.GetName()
	kubeNs := topo.GetNamespace()

	val, found, err := unstructured.NestedFieldNoCopy(topo.Object, "spec", "links")
	if err != nil || !found || val == nil {
		return nil, nil
	}
	remoteLinks, ok := val.([]interface{})
	if !ok {
		return nil, nil
	}

	links := make([]wireutil.PodLinkConfig, 0, len(remoteLinks))
	for _, rlItem := range remoteLinks {
		rl, ok := rlItem.(map[string]interface{})
		if !ok {
			continue
		}
		peerPod, _, _ := unstructured.NestedString(rl, "peer_pod")
		localIntf, _, _ := unstructured.NestedString(rl, "local_intf")
		localIP, _, _ := unstructured.NestedString(rl, "local_ip")
		peerIntf, _, _ := unstructured.NestedString(rl, "peer_intf")
		peerIP, _, _ := unstructured.NestedString(rl, "peer_ip")
		uid, _, _ := unstructured.NestedInt64(rl, "uid")
		mtu, _, _ := unstructured.NestedInt64(rl, "mtu")

		links = append(links, wireutil.PodLinkConfig{
			PodName:     podName,
			PeerPodName: peerPod,
			LinkUID:     uid,
			KubeNs:      kubeNs,
			LocalIntf:   localIntf,
			LocalIP:     localIP,
			PeerIntf:    peerIntf,
			PeerIP:      peerIP,
			MTU:         int(mtu),
		})
	}
	return links, nil
}

// ReconcilePodLinks reconciles network interface plumbing for an active pod scheduled on this node.
// It wraps reconcilePodLinksInternal and records any configuration error in the Topology resource's status.
func (m *Meshnet) ReconcilePodLinks(ctx context.Context, topo *unstructured.Unstructured) error {
	if topo == nil {
		return nil
	}
	err := m.reconcilePodLinksInternal(ctx, topo)
	if err != nil {
		if statusErr := m.updatePlumbingErrorStatus(ctx, topo, err.Error()); statusErr != nil {
			mnetdLogger.Warnf("ReconcilePodLinks: failed to update plumbing error status: %v", statusErr)
		}
		return err
	}
	if err := m.updatePlumbingErrorStatus(ctx, topo, ""); err != nil {
		mnetdLogger.Warnf("ReconcilePodLinks: failed to clear plumbing error status: %v", err)
	}
	return nil
}

// reconcilePodLinksInternal performs the actual network interface plumbing work.
func (m *Meshnet) reconcilePodLinksInternal(ctx context.Context, topo *unstructured.Unstructured) error {
	if topo == nil {
		return nil
	}
	srcIP, _, _ := unstructured.NestedString(topo.Object, "status", "src_ip")
	currentNetNS, _, _ := unstructured.NestedString(topo.Object, "status", "net_ns")
	if srcIP != "" && m.nodeIP != "" && srcIP == m.nodeIP {
		if currentNetNS != "" && !isNetNSValid(currentNetNS) {
			mnetdLogger.Warnf("ReconcilePodLinks: local pod %s has stale netns path %s; clearing active status", topo.GetName(), currentNetNS)
			_ = m.CleanupPodLinks(ctx, topo)
			_ = m.clearPodAliveStatus(ctx, topo)
			return nil
		}
	}

	srcIP, netNS, active := isPodActive(topo)
	if !active {
		return nil
	}
	if m.nodeIP != "" && srcIP != m.nodeIP {
		return nil
	}

	links, err := parsePodLinks(topo)
	if err != nil {
		return err
	}

	// A link that left the spec goes first, from the pod and from the host, so its name is free for
	// whatever replaces it in the same pass.
	keep := make(map[int64]bool, len(links))
	for _, l := range links {
		keep[l.LinkUID] = true
	}
	if err := m.pruneRemovedLinks(topo, netNS, keep); err != nil {
		return err
	}
	if len(links) == 0 {
		return nil
	}

	// Every link wired in this pass gets its tag at the end, which is what a later prune looks for.
	tags := make(map[string]string, len(links))
	defer func() {
		if err := wireutil.TagLinks(netNS, tags); err != nil {
			mnetdLogger.Warnf("ReconcilePodLinks: failed to tag the links of pod %s: %v", topo.GetName(), err)
		}
	}()

	peerCache := make(map[string]*unstructured.Unstructured)
	for _, link := range links {
		// The external uplink: a macvlan on a node interface, with no peer pod to wait for.
		if link.PeerPodName == localhost {
			if err := wireutil.MakeMacvlan(netNS, link.LocalIntf, link.PeerIntf, link.LocalIP); err != nil {
				mnetdLogger.Errorf("ReconcilePodLinks: failed to create macvlan %s@%s for pod %s: %v", link.LocalIntf, link.PeerIntf, topo.GetName(), err)
				return err
			}
			tags[link.LocalIntf] = wireutil.LinkTag(link.LinkUID, link.PeerPodName)
			continue
		}

		peerTopo, ok := peerCache[link.PeerPodName]
		if !ok {
			peerTopo, _ = m.getPod(ctx, link.PeerPodName, link.KubeNs)
			peerCache[link.PeerPodName] = peerTopo
		}
		// Both ends of a link get the same MTU, whichever end requests it, and without a request the
		// one a vxlan carries, so the interface needs no change if its peer moves to another node.
		mtu := wireutil.LinkMTU(link.MTU, peerLinkMTU(peerTopo, link.LinkUID))
		if mtu <= 0 {
			mtu = m.defaultMTU
		}
		// The pod gets its interface whether its peer is up or not; it has no carrier until it is.
		end := wireutil.NewPodEnd(netNS, link, mtu)
		if _, err := wireutil.EnsurePodEnd(end); err != nil {
			mnetdLogger.Errorf("ReconcilePodLinks: failed to plumb %s of pod %s: %v", link.LocalIntf, topo.GetName(), err)
			return err
		}
		tags[link.LocalIntf] = wireutil.LinkTag(link.LinkUID, link.PeerPodName)
		if err := m.joinLink(link, srcIP, netNS, end.Host, peerTopo); err != nil {
			mnetdLogger.Errorf("ReconcilePodLinks: failed to join %s of pod %s to %s: %v", link.LocalIntf, topo.GetName(), link.PeerPodName, err)
			return err
		}
	}
	return nil
}

// joinLink joins the node end of a link to its peer's and sets the carrier the pod sees: none while
// the peer is off, the peer's admin state when both are on this node, up across nodes.
func (m *Meshnet) joinLink(link wireutil.PodLinkConfig, srcIP, netNS, hostEnd string, peerTopo *unstructured.Unstructured) error {
	vxName := wireutil.HostVxlanName(link.KubeNs, link.PodName, link.LinkUID)
	peerSrcIP, peerNetNS, peerActive := isPodActive(peerTopo)
	if !peerActive {
		m.forgetJoin(hostEnd)
		_ = wireutil.RemoveHostLink(vxName)
		return wireutil.SetHostEndUp(hostEnd, false)
	}
	if peerSrcIP == srcIP {
		_ = wireutil.RemoveHostLink(vxName)
		peerHost := wireutil.HostEndName(link.KubeNs, link.PeerPodName, link.LinkUID)
		if !wireutil.HostLinkExists(peerHost) {
			// The peer's own reconcile makes its end, and joins the two.
			m.forgetJoin(hostEnd)
			m.enqueueReconcile(fmt.Sprintf("%s/%s", link.KubeNs, link.PeerPodName))
			return wireutil.SetHostEndUp(hostEnd, false)
		}
		if err := wireutil.JoinHostEnds(hostEnd, peerHost); err != nil {
			return err
		}
		m.rememberJoin(hostEnd, hostJoin{peerHost: peerHost, netNS: netNS, intf: link.LocalIntf, peerNetNS: peerNetNS, peerIntf: link.PeerIntf})
		m.syncCarrier(hostEnd)
		return nil
	}
	m.forgetJoin(hostEnd)
	remote := net.ParseIP(peerSrcIP)
	if remote == nil {
		return fmt.Errorf("peer %s has an unusable node address %q", link.PeerPodName, peerSrcIP)
	}
	if err := wireutil.JoinVxlan(hostEnd, vxName, int(link.LinkUID+vxlanBase), remote, m.nodeIntf); err != nil {
		return err
	}
	return wireutil.SetHostEndUp(hostEnd, true)
}

// hostJoin is a same-node join as seen from one of its node ends.
type hostJoin struct {
	peerHost            string
	netNS, intf         string
	peerNetNS, peerIntf string
}

// rememberJoin records a same-node join under both of its node ends, for the carrier watcher.
func (m *Meshnet) rememberJoin(hostEnd string, j hostJoin) {
	m.joinsMu.Lock()
	defer m.joinsMu.Unlock()
	if m.joins == nil {
		m.joins = make(map[string]hostJoin)
	}
	m.joins[hostEnd] = j
	m.joins[j.peerHost] = hostJoin{peerHost: hostEnd, netNS: j.peerNetNS, intf: j.peerIntf, peerNetNS: j.netNS, peerIntf: j.intf}
}

// forgetJoin drops the same-node join of a node end, if it has one, from both sides.
func (m *Meshnet) forgetJoin(hostEnd string) {
	m.joinsMu.Lock()
	defer m.joinsMu.Unlock()
	if j, ok := m.joins[hostEnd]; ok {
		delete(m.joins, hostEnd)
		if back, ok := m.joins[j.peerHost]; ok && back.peerHost == hostEnd {
			delete(m.joins, j.peerHost)
		}
	}
}

func (m *Meshnet) lookupJoin(hostEnd string) (hostJoin, bool) {
	m.joinsMu.Lock()
	defer m.joinsMu.Unlock()
	j, ok := m.joins[hostEnd]
	return j, ok
}

// syncCarrier gives each pod of a same-node join the carrier the other's admin state says, as a
// cable does: each node end is up exactly when the interface on the other side is.
func (m *Meshnet) syncCarrier(hostEnd string) {
	j, ok := m.lookupJoin(hostEnd)
	if !ok {
		return
	}
	mineUp := wireutil.PodEndAdminUp(j.netNS, j.intf)
	peerUp := wireutil.PodEndAdminUp(j.peerNetNS, j.peerIntf)
	if err := wireutil.SetHostEndUp(hostEnd, peerUp); err != nil {
		mnetdLogger.Debugf("syncCarrier: %s: %v", hostEnd, err)
	}
	if err := wireutil.SetHostEndUp(j.peerHost, mineUp); err != nil {
		mnetdLogger.Debugf("syncCarrier: %s: %v", j.peerHost, err)
	}
}

// watchHostEnds follows the node ends of same-node joins: a pod that shuts its interface takes the
// carrier of its neighbour's at once, and a pod that goes takes it too, before any reconcile runs.
func (m *Meshnet) watchHostEnds(ctx context.Context) {
	for ctx.Err() == nil {
		updates := make(chan netlink.LinkUpdate, 256)
		done := make(chan struct{})
		err := netlink.LinkSubscribeWithOptions(updates, done, netlink.LinkSubscribeOptions{
			ErrorCallback: func(err error) { mnetdLogger.Warnf("watchHostEnds: %v", err) },
		})
		if err != nil {
			mnetdLogger.Errorf("watchHostEnds: cannot follow the node's links: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		for open := true; open; {
			select {
			case <-ctx.Done():
				close(done)
				return
			case u, ok := <-updates:
				if !ok {
					open = false
					break
				}
				name := u.Attrs().Name
				if !wireutil.IsHostEnd(name) {
					continue
				}
				j, joined := m.lookupJoin(name)
				if !joined {
					continue
				}
				if u.Header.Type == unix.RTM_DELLINK {
					m.forgetJoin(name)
					_ = wireutil.SetHostEndUp(j.peerHost, false)
					continue
				}
				m.syncCarrier(name)
			}
		}
		close(done)
	}
}

// pruneRemovedLinks deletes the pod's tagged interfaces whose link is no longer in keep, which takes
// their node ends along, and the vxlans that carried them.
func (m *Meshnet) pruneRemovedLinks(topo *unstructured.Unstructured, netNS string, keep map[int64]bool) error {
	removed, err := wireutil.PruneLinks(netNS, keep)
	if err != nil {
		mnetdLogger.Errorf("ReconcilePodLinks: failed to remove the links that left the spec of pod %s: %v", topo.GetName(), err)
		return err
	}
	for _, r := range removed {
		mnetdLogger.Infof("ReconcilePodLinks: removed %s (link %d to %s) from pod %s, no longer in its spec", r.Name, r.UID, r.Peer, topo.GetName())
		if r.Peer == localhost {
			continue
		}
		m.forgetJoin(wireutil.HostEndName(topo.GetNamespace(), topo.GetName(), r.UID))
		_ = wireutil.RemoveHostLink(wireutil.HostVxlanName(topo.GetNamespace(), topo.GetName(), r.UID))
	}
	return nil
}

// CleanupPodLinks removes what a pod's links keep on the node once the pod is gone: its node ends go
// with its netns, the vxlans that carried them stay and go here.
func (m *Meshnet) CleanupPodLinks(ctx context.Context, topo *unstructured.Unstructured) error {
	if topo == nil {
		return nil
	}
	links, err := parsePodLinks(topo)
	if err != nil {
		return err
	}
	for _, link := range links {
		if link.PeerPodName == localhost {
			continue
		}
		m.forgetJoin(wireutil.HostEndName(link.KubeNs, link.PodName, link.LinkUID))
		_ = wireutil.RemoveHostLink(wireutil.HostVxlanName(link.KubeNs, link.PodName, link.LinkUID))
	}
	return nil
}

// TopologyCache provides a thread-safe in-memory cache of Kubernetes Topology CRs
// and maintains an inverted dependency map (peerPod -> []localPods) for targeted reconciliation.
type TopologyCache struct {
	mu       sync.RWMutex
	topos    map[string]*unstructured.Unstructured // key: "namespace/name"
	peerDeps map[string]map[string]bool            // key: "namespace/peerPodName" -> set of "namespace/dependentPodName"
}

// NewTopologyCache creates a new empty TopologyCache instance.
func NewTopologyCache() *TopologyCache {
	return &TopologyCache{
		topos:    make(map[string]*unstructured.Unstructured),
		peerDeps: make(map[string]map[string]bool),
	}
}

// Put updates or inserts a Topology resource into the cache and indexes its link dependencies.
func (c *TopologyCache) Put(topo *unstructured.Unstructured) {
	if c == nil || topo == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	ns := topo.GetNamespace()
	name := topo.GetName()
	key := fmt.Sprintf("%s/%s", ns, name)

	// Remove old peer dependencies registered by this pod if already in cache
	if oldTopo, exists := c.topos[key]; exists {
		oldLinks, _ := parsePodLinks(oldTopo)
		for _, l := range oldLinks {
			oldPeerKey := fmt.Sprintf("%s/%s", l.KubeNs, l.PeerPodName)
			if deps, ok := c.peerDeps[oldPeerKey]; ok {
				delete(deps, key)
				if len(deps) == 0 {
					delete(c.peerDeps, oldPeerKey)
				}
			}
		}
	}

	c.topos[key] = topo.DeepCopy()

	// Index new peer dependencies
	links, _ := parsePodLinks(topo)
	for _, l := range links {
		peerKey := fmt.Sprintf("%s/%s", l.KubeNs, l.PeerPodName)
		if c.peerDeps[peerKey] == nil {
			c.peerDeps[peerKey] = make(map[string]bool)
		}
		c.peerDeps[peerKey][key] = true
	}
}

// Delete removes a Topology resource from the cache and cleans up its link dependencies.
func (c *TopologyCache) Delete(ns, name string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	key := fmt.Sprintf("%s/%s", ns, name)
	if oldTopo, exists := c.topos[key]; exists {
		oldLinks, _ := parsePodLinks(oldTopo)
		for _, l := range oldLinks {
			oldPeerKey := fmt.Sprintf("%s/%s", l.KubeNs, l.PeerPodName)
			if deps, ok := c.peerDeps[oldPeerKey]; ok {
				delete(deps, key)
				if len(deps) == 0 {
					delete(c.peerDeps, oldPeerKey)
				}
			}
		}
		delete(c.topos, key)
	}
}

// Get retrieves a Topology resource by namespace and name from the cache.
func (c *TopologyCache) Get(ns, name string) *unstructured.Unstructured {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	topo := c.topos[fmt.Sprintf("%s/%s", ns, name)]
	if topo == nil {
		return nil
	}
	return topo.DeepCopy()
}

// List returns all cached Topology resources matching the given namespace (or all if namespace is empty or metav1.NamespaceAll).
func (c *TopologyCache) List(ns string) []*unstructured.Unstructured {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()

	res := make([]*unstructured.Unstructured, 0, len(c.topos))
	for _, topo := range c.topos {
		if ns == "" || ns == metav1.NamespaceAll || topo.GetNamespace() == ns {
			res = append(res, topo.DeepCopy())
		}
	}
	return res
}

// GetDependents returns all pod keys ("namespace/name") that declare links pointing to the given pod.
func (c *TopologyCache) GetDependents(ns, name string) []string {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := fmt.Sprintf("%s/%s", ns, name)
	deps := c.peerDeps[key]
	if len(deps) == 0 {
		return nil
	}
	result := make([]string, 0, len(deps))
	for depKey := range deps {
		result = append(result, depKey)
	}
	return result
}

// ReconcileQueue coalesces and debounces reconciliation requests for individual pods
// or full cluster sync passes.
type ReconcileQueue struct {
	mu          sync.Mutex
	pending     map[string]bool
	fullPending bool
	notifyChan  chan struct{}
	debounceDur time.Duration
	timer       *time.Timer
}

// NewReconcileQueue creates a new ReconcileQueue with the specified debounce duration.
func NewReconcileQueue(debounceDur time.Duration) *ReconcileQueue {
	if debounceDur <= 0 {
		debounceDur = 50 * time.Millisecond
	}
	return &ReconcileQueue{
		pending:     make(map[string]bool),
		notifyChan:  make(chan struct{}, 1),
		debounceDur: debounceDur,
	}
}

// Enqueue adds a pod key ("namespace/name") to the pending reconciliation set and arms the debounce timer.
func (rq *ReconcileQueue) Enqueue(key string) {
	if rq == nil {
		return
	}
	rq.mu.Lock()
	defer rq.mu.Unlock()

	rq.pending[key] = true
	rq.arm()
}

// EnqueueFull requests a full cluster reconciliation pass and arms the debounce timer.
func (rq *ReconcileQueue) EnqueueFull() {
	if rq == nil {
		return
	}
	rq.mu.Lock()
	defer rq.mu.Unlock()

	rq.fullPending = true
	rq.arm()
}

// arm starts the debounce timer unless one is already running; the caller holds the lock.
func (rq *ReconcileQueue) arm() {
	if rq.timer == nil {
		rq.timer = time.AfterFunc(rq.debounceDur, func() {
			select {
			case rq.notifyChan <- struct{}{}:
			default:
			}
		})
	}
}

// Drain clears and returns the currently queued reconciliation tasks.
func (rq *ReconcileQueue) Drain() (bool, []string) {
	if rq == nil {
		return true, nil
	}
	rq.mu.Lock()
	defer rq.mu.Unlock()

	isFull := rq.fullPending
	rq.fullPending = false

	keys := make([]string, 0, len(rq.pending))
	for k := range rq.pending {
		keys = append(keys, k)
	}
	rq.pending = make(map[string]bool)
	rq.timer = nil

	return isFull, keys
}

func parseKey(key string) (string, string) {
	parts := strings.SplitN(key, "/", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return "", parts[0]
}

func (m *Meshnet) enqueueReconcile(key string) {
	if m.reconcileQueue != nil {
		m.reconcileQueue.Enqueue(key)
	}
}

func (m *Meshnet) enqueueFullReconcile() {
	if m.reconcileQueue != nil {
		m.reconcileQueue.EnqueueFull()
	}
}

// isLocal says whether an active topology belongs to a pod on this node.
func (m *Meshnet) isLocal(topo *unstructured.Unstructured) bool {
	srcIP, _, active := isPodActive(topo)
	return active && (m.nodeIP == "" || srcIP == m.nodeIP)
}

// enqueuePodAndDependents queues the pod if it runs here and every local pod with a link to it,
// which is what a change of the pod's spec or status can affect.
func (m *Meshnet) enqueuePodAndDependents(ns, name string) {
	if m.topoCache == nil {
		return
	}
	if topo := m.topoCache.Get(ns, name); topo != nil && m.isLocal(topo) {
		m.enqueueReconcile(fmt.Sprintf("%s/%s", ns, name))
	}
	for _, depKey := range m.topoCache.GetDependents(ns, name) {
		depNS, depName := parseKey(depKey)
		if depTopo := m.topoCache.Get(depNS, depName); depTopo != nil && m.isLocal(depTopo) {
			m.enqueueReconcile(depKey)
		}
	}
}

// CleanupOrphanedHostLinks removes what no pod uses any more from the node: a vxlan whose node end is
// gone (its pod went, or the link moved onto one node) and the host veths of the previous wiring, which
// joined two pods directly and are never made now.
func (m *Meshnet) CleanupOrphanedHostLinks(ctx context.Context) error {
	hostLinks, err := netlink.LinkList()
	if err != nil {
		return err
	}
	present := make(map[string]bool, len(hostLinks))
	for _, l := range hostLinks {
		present[l.Attrs().Name] = true
	}
	for _, l := range hostLinks {
		name := l.Attrs().Name
		orphan := strings.HasPrefix(name, wireutil.LegacyHostVethPrefix) ||
			(wireutil.IsHostVxlan(name) && !present[wireutil.HostEndOfVxlan(name)])
		if orphan {
			mnetdLogger.Infof("CleanupOrphanedHostLinks: removing %s, which no pod uses", name)
			_ = netlink.LinkDel(l)
		}
	}
	return nil
}

// ReconcileAllLocalPods scans all Topology resources and reconciles any active local pod scheduled on this node.
func (m *Meshnet) ReconcileAllLocalPods(ctx context.Context) error {
	topos, err := m.listTopologies(ctx)
	if err != nil {
		return err
	}
	for _, u := range topos {
		_ = m.ReconcilePodLinks(ctx, u)
	}
	return nil
}

// listTopologies returns every Topology from the cache, or from the API while the cache is empty.
func (m *Meshnet) listTopologies(ctx context.Context) ([]*unstructured.Unstructured, error) {
	var topos []*unstructured.Unstructured
	if m.topoCache != nil {
		topos = m.topoCache.List(metav1.NamespaceAll)
	}
	if len(topos) == 0 && m.tClient != nil {
		list, err := m.tClient.Topology(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		for i := range list.Items {
			u, err := toUnstructured(&list.Items[i])
			if err != nil {
				continue
			}
			topos = append(topos, u)
		}
	}
	return topos, nil
}

// runReconcileWorker runs in the background and coalesces incoming reconcile triggers.
// When a trigger is received, it executes a targeted or full local reconciliation pass.
func (m *Meshnet) runReconcileWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.reconcileQueue.notifyChan:
			isFull, keys := m.reconcileQueue.Drain()
			if isFull {
				_ = m.CleanupOrphanedHostLinks(ctx)
				_ = m.ReconcileAllLocalPods(ctx)
			} else {
				for _, key := range keys {
					ns, name := parseKey(key)
					topo, err := m.getPod(ctx, name, ns)
					if err != nil || topo == nil {
						continue
					}
					_ = m.ReconcilePodLinks(ctx, topo)
				}
			}
		}
	}
}

// RunControllerLoop watches the Topology resources through an informer and reconciles the local pods
// a change can affect; the informer's resync replays every Topology each minute, so a link broken by
// hand comes back within that time.
func (m *Meshnet) RunControllerLoop(ctx context.Context) {
	mnetdLogger.Infof("Starting Topology controller loop")
	if m.topoCache == nil {
		m.topoCache = NewTopologyCache()
	}
	if m.reconcileQueue == nil {
		m.reconcileQueue = NewReconcileQueue(50 * time.Millisecond)
	}
	go m.runReconcileWorker(ctx)

	factory := dynamicinformer.NewDynamicSharedInformerFactory(m.dynClient, resyncPeriod)
	informer := factory.ForResource(topologyclientv1.GVR())
	onChange := func(obj interface{}) {
		topo, err := toUnstructured(obj)
		if err != nil || topo == nil {
			return
		}
		m.topoCache.Put(topo)
		m.enqueuePodAndDependents(topo.GetNamespace(), topo.GetName())
	}
	_, err := informer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    onChange,
		UpdateFunc: func(_, newObj interface{}) { onChange(newObj) },
		DeleteFunc: func(obj interface{}) {
			topo, err := toUnstructured(obj)
			if err != nil || topo == nil {
				return
			}
			ns, name := topo.GetNamespace(), topo.GetName()
			dependents := m.topoCache.GetDependents(ns, name)
			m.topoCache.Delete(ns, name)
			// A Topology deleted while its pod keeps running takes the pod's links with it.
			if _, netNS, _ := isPodActive(topo); m.isLocal(topo) && isNetNSValid(netNS) {
				_ = m.pruneRemovedLinks(topo, netNS, nil)
			}
			_ = m.CleanupPodLinks(ctx, topo)
			for _, depKey := range dependents {
				depNS, depName := parseKey(depKey)
				if depTopo := m.topoCache.Get(depNS, depName); depTopo != nil && m.isLocal(depTopo) {
					m.enqueueReconcile(depKey)
				}
			}
		},
	})
	if err != nil {
		mnetdLogger.Errorf("RunControllerLoop: failed to add the Topology event handler: %v", err)
		return
	}

	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), informer.Informer().HasSynced) {
		mnetdLogger.Warnf("Topology informer cache did not sync before shutdown")
		return
	}
	mnetdLogger.Infof("Topology informer cache synced successfully")
	m.enqueueFullReconcile()
	go m.watchHostEnds(ctx)
	// The informer's resync reconciles every pod each minute; what a pod that left this node leaves
	// on it is swept at the same pace.
	ticker := time.NewTicker(resyncPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = m.CleanupOrphanedHostLinks(ctx)
		}
	}
}

// updatePlumbingErrorStatus writes the provided plumbing error message (or clears it if empty)
// to the Topology resource's status.plumbing_error field.
func (m *Meshnet) updatePlumbingErrorStatus(ctx context.Context, topo *unstructured.Unstructured, errMsg string) error {
	if m.tClient == nil {
		return fmt.Errorf("topology client not initialized")
	}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latestTopo, err := m.tClient.Topology(topo.GetNamespace()).Unstructured(ctx, topo.GetName(), metav1.GetOptions{})
		if err != nil {
			return err
		}

		currErr, found, _ := unstructured.NestedString(latestTopo.Object, "status", "plumbing_error")
		if errMsg == "" {
			if !found || currErr == "" {
				return nil
			}
			unstructured.RemoveNestedField(latestTopo.Object, "status", "plumbing_error")
		} else {
			if found && currErr == errMsg {
				return nil
			}
			if err := unstructured.SetNestedField(latestTopo.Object, errMsg, "status", "plumbing_error"); err != nil {
				return err
			}
		}

		_, err = m.tClient.Topology(latestTopo.GetNamespace()).Update(ctx, latestTopo, metav1.UpdateOptions{})
		if err == nil && m.topoCache != nil {
			m.topoCache.Put(latestTopo)
		}
		return err
	})
}
