package meshnet

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/emigcm98/meshnet-cni/api/types/v1beta1"
	"github.com/emigcm98/meshnet-cni/utils/wireutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
)

// fillLinkStates completes the status of the links a pass wired with what their interfaces show in
// the pod, read in one visit, and their state: off and unplugged from the join, up or down from the
// carrier. A failed link keeps its state.
func fillLinkStates(netNS string, links []v1beta1.LinkStatus, joins []joinResult) {
	names := make([]string, 0, len(links))
	for _, l := range links {
		names = append(names, l.LocalIntf)
	}
	ends, err := wireutil.ReadEnds(netNS, names)
	if err != nil {
		mnetdLogger.Warnf("fillLinkStates: %v", err)
	}
	for i := range links {
		st, j := &links[i], joins[i]
		if st.State == v1beta1.LinkFailed {
			continue
		}
		st.Transport, st.VNI = j.transport, j.vni
		end, ok := ends[st.LocalIntf]
		if ok {
			st.IfIndex, st.MAC, st.MTU = end.Index, end.MAC, end.MTU
		}
		switch {
		case !ok:
			st.State, st.Error = v1beta1.LinkFailed, "the interface is not in the pod"
		case j.peerOff:
			st.State, st.Transport = v1beta1.LinkOff, ""
		case j.unplugged:
			st.State = v1beta1.LinkUnplugged
		case end.AdminUp && end.Carrier:
			st.State = v1beta1.LinkUp
		default:
			st.State = v1beta1.LinkDown
		}
	}
}

// linksOf reads status.links of topo.
func linksOf(topo *unstructured.Unstructured) []v1beta1.LinkStatus {
	raw, found, _ := unstructured.NestedSlice(topo.Object, "status", "links")
	if !found {
		return nil
	}
	out := make([]v1beta1.LinkStatus, 0, len(raw))
	for _, r := range raw {
		m, ok := r.(map[string]interface{})
		if !ok {
			continue
		}
		var l v1beta1.LinkStatus
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(m, &l); err == nil {
			out = append(out, l)
		}
	}
	return out
}

func setLinks(topo *unstructured.Unstructured, links []v1beta1.LinkStatus) error {
	if len(links) == 0 {
		unstructured.RemoveNestedField(topo.Object, "status", "links")
		return nil
	}
	raw := make([]interface{}, 0, len(links))
	for _, l := range links {
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&l)
		if err != nil {
			return err
		}
		raw = append(raw, m)
	}
	return unstructured.SetNestedSlice(topo.Object, raw, "status", "links")
}

// updateReconcileStatus writes the plumbing error of a pass and, when the pod is this node's, the
// state of its links, in one write and only if something changed; the changes of the links become
// events on the Topology.
func (m *Meshnet) updateReconcileStatus(ctx context.Context, topo *unstructured.Unstructured, errMsg string, res podReconcile) error {
	key := topo.GetNamespace() + "/" + topo.GetName()
	if res.owned {
		m.rememberLinks(key, res.links)
	} else {
		m.forgetLinks(key)
	}
	if m.tClient == nil {
		return fmt.Errorf("topology client not initialized")
	}
	var events []linkEvent
	var written *unstructured.Unstructured
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest, err := m.tClient.Topology(topo.GetNamespace()).Unstructured(ctx, topo.GetName(), metav1.GetOptions{})
		if err != nil {
			return err
		}
		changed := false
		currErr, _, _ := unstructured.NestedString(latest.Object, "status", "plumbing_error")
		if currErr != errMsg {
			changed = true
			if errMsg == "" {
				unstructured.RemoveNestedField(latest.Object, "status", "plumbing_error")
			} else if err := unstructured.SetNestedField(latest.Object, errMsg, "status", "plumbing_error"); err != nil {
				return err
			}
		}
		events = nil
		if res.owned {
			prev := linksOf(latest)
			if !reflect.DeepEqual(prev, res.links) && !(len(prev) == 0 && len(res.links) == 0) {
				changed = true
				events = linkEvents(prev, res.links)
				if err := setLinks(latest, res.links); err != nil {
					return err
				}
			}
		}
		if !changed {
			return nil
		}
		if _, err := m.tClient.Topology(latest.GetNamespace()).Update(ctx, latest, metav1.UpdateOptions{}); err != nil {
			return err
		}
		written = latest
		return nil
	})
	if err != nil {
		return err
	}
	if written != nil && m.topoCache != nil {
		m.topoCache.Put(written)
	}
	if m.recorder != nil && written != nil {
		for _, e := range events {
			kind := corev1.EventTypeNormal
			if e.warning {
				kind = corev1.EventTypeWarning
			}
			m.recorder.Event(written, kind, e.reason, e.message)
		}
	}
	return nil
}

// linkEvent is one change of a link, as an event on its pod's Topology.
type linkEvent struct {
	warning         bool
	reason, message string
}

// The reasons of the events of a link.
const (
	reasonWired   = "Wired"
	reasonUnwired = "Unwired"
	reasonUp      = "LinkUp"
	reasonDown    = "LinkDown"
	reasonFailed  = "WiringFailed"
)

func linkName(l v1beta1.LinkStatus) string {
	return fmt.Sprintf("%s to %s", l.LocalIntf, l.PeerPod)
}

// over says how a link is carried.
func over(l v1beta1.LinkStatus) string {
	switch l.Transport {
	case "vxlan":
		return fmt.Sprintf("over vxlan, VNI %d", l.VNI)
	case "":
		return ""
	default:
		return "over " + l.Transport
	}
}

// why says why a link has no carrier.
func why(l v1beta1.LinkStatus) string {
	switch l.State {
	case v1beta1.LinkOff:
		return "the peer is off"
	case v1beta1.LinkUnplugged:
		return "unplugged"
	default:
		return "no carrier, a port is shut"
	}
}

func joinWords(words ...string) string {
	out := make([]string, 0, len(words))
	for _, w := range words {
		if w != "" {
			out = append(out, w)
		}
	}
	return strings.Join(out, ", ")
}

// linkEvents tells what changed between two statuses of a pod's links: links wired, rewired (made
// again or moved to another transport), unwired, failed, up or down.
func linkEvents(prev, cur []v1beta1.LinkStatus) []linkEvent {
	before := make(map[int]v1beta1.LinkStatus, len(prev))
	for _, l := range prev {
		before[l.UID] = l
	}
	var out []linkEvent
	seen := make(map[int]bool, len(cur))
	for _, l := range cur {
		seen[l.UID] = true
		p, had := before[l.UID]
		switch {
		case l.State == v1beta1.LinkFailed:
			if !had || p.State != v1beta1.LinkFailed || p.Error != l.Error {
				out = append(out, linkEvent{warning: true, reason: reasonFailed, message: fmt.Sprintf("%s: %s", linkName(l), l.Error)})
			}
		case !had || p.State == v1beta1.LinkFailed:
			state := "up"
			if l.State != v1beta1.LinkUp {
				state = why(l)
			}
			out = append(out, linkEvent{reason: reasonWired, message: joinWords(linkName(l)+" wired", over(l), state)})
		default:
			if p.IfIndex != l.IfIndex || (p.Transport != "" && l.Transport != "" && p.Transport != l.Transport) {
				out = append(out, linkEvent{reason: reasonWired, message: joinWords(linkName(l)+" rewired", over(l))})
			}
			if p.State != l.State {
				if l.State == v1beta1.LinkUp {
					out = append(out, linkEvent{reason: reasonUp, message: joinWords(linkName(l)+" up", over(l))})
				} else {
					out = append(out, linkEvent{reason: reasonDown, message: fmt.Sprintf("%s down: %s", linkName(l), why(l))})
				}
			}
		}
		if l.State != v1beta1.LinkFailed && l.Error != "" && (!had || p.Error != l.Error) {
			out = append(out, linkEvent{warning: true, reason: reasonFailed, message: fmt.Sprintf("%s: %s", linkName(l), l.Error)})
		}
	}
	for _, p := range prev {
		if !seen[p.UID] {
			out = append(out, linkEvent{reason: reasonUnwired, message: linkName(p) + " unwired"})
		}
	}
	return out
}

// rememberLinks keeps the last status of a local pod's links, for the metrics.
func (m *Meshnet) rememberLinks(key string, links []v1beta1.LinkStatus) {
	m.linksMu.Lock()
	defer m.linksMu.Unlock()
	if m.podLinks == nil {
		m.podLinks = make(map[string][]v1beta1.LinkStatus)
	}
	m.podLinks[key] = links
}

func (m *Meshnet) forgetLinks(key string) {
	m.linksMu.Lock()
	defer m.linksMu.Unlock()
	delete(m.podLinks, key)
}
