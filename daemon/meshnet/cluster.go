package meshnet

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// clusterNamespace is the namespace whose UID stands for the cluster: every cluster has it, and it
// lives as long as the cluster does.
const clusterNamespace = "kube-system"

var clusterRetry = 30 * time.Second

// cluster is the identity the MACs of the link ends derive from, empty until it is read.
func (m *Meshnet) cluster() string {
	if id := m.clusterID.Load(); id != nil {
		return *id
	}
	return ""
}

// settleCluster reads the cluster's identity before the first pass and, when it cannot, keeps trying:
// until it has it, a link end that declares no MAC is born with the kernel's random one, since a MAC
// derived without it could meet its twin from another cluster on a segment both reach.
func (m *Meshnet) settleCluster(ctx context.Context) {
	if m.readCluster(ctx, true) {
		return
	}
	go func() {
		t := time.NewTicker(clusterRetry)
		defer t.Stop()
		for tries := 1; ; tries++ {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				// Said again every ten tries, so the log keeps it without filling with it.
				if m.readCluster(ctx, tries%10 == 0) {
					return
				}
			}
		}
	}()
}

func (m *Meshnet) readCluster(ctx context.Context, warn bool) bool {
	if m.kClient == nil {
		return false
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	kns, err := m.kClient.CoreV1().Namespaces().Get(rctx, clusterNamespace, metav1.GetOptions{})
	if err != nil {
		if warn {
			mnetdLogger.Errorf("MAC: cannot read the namespace %s, the cluster's identity: %v; link ends that declare no MAC are born with a random one until it can (the daemon's ClusterRole needs get on that namespace)", clusterNamespace, err)
		}
		return false
	}
	id := string(kns.UID)
	m.clusterID.Store(&id)
	mnetdLogger.Infof("MAC: link ends that declare no MAC derive it from cluster %s, their namespace, pod and interface", id)
	return true
}
