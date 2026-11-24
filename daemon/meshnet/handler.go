package meshnet

import (
	"context"
	"fmt"
	"os"

	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/retry"

	mpb "github.com/emigcm98/meshnet-cni/daemon/proto/meshnet/v1beta1"
)

// getPod reads a Topology from the informer's cache, or from the API when the cache does not have it yet.
func (m *Meshnet) getPod(ctx context.Context, name, ns string) (*unstructured.Unstructured, error) {
	if m.topoCache != nil {
		if topo := m.topoCache.Get(ns, name); topo != nil {
			return topo, nil
		}
	}
	return m.getPodFresh(ctx, name, ns)
}

// getPodFresh reads a Topology from the API. Read-modify-write paths use it, since an update built
// on a cached copy that lags fails on its resourceVersion every time the retry reads it again.
func (m *Meshnet) getPodFresh(ctx context.Context, name, ns string) (*unstructured.Unstructured, error) {
	if m.tClient == nil {
		return nil, fmt.Errorf("topology client not initialized")
	}
	mnetdLogger.Debugf("Reading pod %s from K8s", name)
	topo, err := m.tClient.Topology(ns).Unstructured(ctx, name, metav1.GetOptions{})
	if err == nil && topo != nil && m.topoCache != nil {
		m.topoCache.Put(topo)
	}
	return topo, err
}

func (m *Meshnet) updateStatus(ctx context.Context, obj *unstructured.Unstructured, ns string) error {
	if m.tClient == nil {
		return fmt.Errorf("topology client not initialized")
	}
	mnetdLogger.Infof("Update pod status %s from K8s", obj.GetName())
	updated, err := m.tClient.Topology(ns).Update(ctx, obj, metav1.UpdateOptions{})
	if err == nil && m.topoCache != nil && updated != nil {
		if u, err := toUnstructured(updated); err == nil {
			m.topoCache.Put(u)
		}
	}
	return err
}

// Get is ported from openconfig/kne, third_party/meshnet/daemon/meshnet/handler.go at 5a1676c (Apache 2.0):
// a Topology without links is a pod with nothing to wire, not an error.
func (m *Meshnet) Get(ctx context.Context, pod *mpb.PodQuery) (*mpb.Pod, error) {
	mnetdLogger.Infof("Retrieving %s's metadata from K8s...", pod.Name)

	result, err := m.getPod(ctx, pod.Name, pod.KubeNs)
	if err != nil {
		mnetdLogger.Errorf("Failed to read pod %s from K8s", pod.Name)
		return nil, err
	}

	var links []*mpb.Link
	val, found, err := unstructured.NestedFieldNoCopy(result.Object, "spec", "links")
	if err == nil && found && val != nil {
		remoteLinks, ok := val.([]interface{})
		if !ok {
			mnetdLogger.Errorf("Unrecognised 'links' field in pod %s spec", pod.Name)
			return nil, fmt.Errorf("unrecognised 'links' field in pod %s spec", pod.Name)
		}
		links = make([]*mpb.Link, len(remoteLinks))
		for i := range links {
			remoteLink, ok := remoteLinks[i].(map[string]interface{})
			if !ok {
				mnetdLogger.Errorf("Unrecognised 'Link' structure")
				return nil, fmt.Errorf("unrecognised 'Link' structure")
			}
			newLink := &mpb.Link{}
			newLink.PeerPod, _, _ = unstructured.NestedString(remoteLink, "peer_pod")
			newLink.PeerIntf, _, _ = unstructured.NestedString(remoteLink, "peer_intf")
			newLink.LocalIntf, _, _ = unstructured.NestedString(remoteLink, "local_intf")
			newLink.LocalIp, _, _ = unstructured.NestedString(remoteLink, "local_ip")
			newLink.PeerIp, _, _ = unstructured.NestedString(remoteLink, "peer_ip")
			newLink.Uid, _, _ = unstructured.NestedInt64(remoteLink, "uid")
			links[i] = newLink
		}
	}

	srcIP, _, _ := unstructured.NestedString(result.Object, "status", "src_ip")
	netNs, _, _ := unstructured.NestedString(result.Object, "status", "net_ns")
	containerId, _, _ := unstructured.NestedString(result.Object, "status", "container_id")
	nodeIP := os.Getenv("HOST_IP")
	nodeIntf := os.Getenv("HOST_INTF")

	return &mpb.Pod{
		Name:        pod.Name,
		SrcIp:       srcIP,
		NetNs:       netNs,
		KubeNs:      pod.KubeNs,
		Links:       links,
		NodeIp:      nodeIP,
		NodeIntf:    nodeIntf,
		ContainerId: containerId,
	}, nil
}

// SetAlive records where a pod runs (or that it no longer does) and has the pod and the local pods
// linked to it reconciled at once, without waiting for the informer to echo the change.
func (m *Meshnet) SetAlive(ctx context.Context, pod *mpb.Pod) (*mpb.BoolResponse, error) {
	mnetdLogger.Infof("Setting %s's SrcIp=%s and NetNs=%s", pod.Name, pod.SrcIp, pod.NetNs)

	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		result, err := m.getPodFresh(ctx, pod.Name, pod.KubeNs)
		if err != nil {
			mnetdLogger.Errorf("Failed to read pod %s from K8s", pod.Name)
			return err
		}

		if err = unstructured.SetNestedField(result.Object, pod.SrcIp, "status", "src_ip"); err != nil {
			mnetdLogger.Errorf("Failed to update pod's src_ip")
		}

		if err = unstructured.SetNestedField(result.Object, pod.NetNs, "status", "net_ns"); err != nil {
			mnetdLogger.Errorf("Failed to update pod's net_ns")
		}

		if err = unstructured.SetNestedField(result.Object, pod.ContainerId, "status", "container_id"); err != nil {
			mnetdLogger.Errorf("Failed to update pod's container_id")
		}
		// A pod that goes takes its interfaces, and with them the state of its links.
		if pod.NetNs == "" {
			unstructured.RemoveNestedField(result.Object, "status", "links")
		}

		return m.updateStatus(ctx, result, pod.KubeNs)
	})

	if retryErr != nil {
		log.WithFields(log.Fields{
			"daemon":   "meshnetd",
			"err":      retryErr,
			"function": "SetAlive",
		}).Errorf("Failed to update pod %s alive status", pod.Name)
		return &mpb.BoolResponse{Response: false}, retryErr
	}

	if pod.NetNs == "" {
		m.forgetLinks(pod.KubeNs + "/" + pod.Name)
	}
	m.enqueuePodAndDependents(pod.KubeNs, pod.Name)
	return &mpb.BoolResponse{Response: true}, nil
}
