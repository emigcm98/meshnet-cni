package meshnet

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// The cluster's identity is the UID of kube-system. A daemon that cannot read it has none, so its
// link ends are born with the kernel's MACs, and keeps trying until it can.
func TestSettleCluster(t *testing.T) {
	InitLogger()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	kubeSystem := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: clusterNamespace, UID: "4f2b9d6e-1c1a-4b55-9a57-0d2f6f2b7c11"}}

	m := &Meshnet{kClient: fake.NewClientset(kubeSystem)}
	m.settleCluster(ctx)
	if got := m.cluster(); got != string(kubeSystem.UID) {
		t.Fatalf("cluster() = %q, want the UID of %s", got, clusterNamespace)
	}

	defer func(d time.Duration) { clusterRetry = d }(clusterRetry)
	clusterRetry = 20 * time.Millisecond
	client := fake.NewClientset()
	m = &Meshnet{kClient: client}
	m.settleCluster(ctx)
	if got := m.cluster(); got != "" {
		t.Fatalf("cluster() = %q with %s unreadable, want none", got, clusterNamespace)
	}
	if _, err := client.CoreV1().Namespaces().Create(ctx, kubeSystem, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating %s: %v", clusterNamespace, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for m.cluster() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := m.cluster(); got != string(kubeSystem.UID) {
		t.Fatalf("cluster() = %q after %s became readable, want its UID", got, clusterNamespace)
	}
}

// A pass that fails is tried again after each of retryDelays in turn, one try set at a time whatever
// passes come in between, until they run out; a pass that succeeds, or the pod made again in another
// netns, starts over.
func TestPassRetry(t *testing.T) {
	m := &Meshnet{}
	failed := context.DeadlineExceeded
	for i, want := range retryDelays {
		got, ok := m.passDone("lab/r1", "/netns/a", failed)
		if !ok || got != want {
			t.Fatalf("failure %d: retry after %v (%t), want %v", i+1, got, ok, want)
		}
		if _, ok := m.passDone("lab/r1", "/netns/a", failed); ok {
			t.Fatalf("failure %d: a second try was set while one was pending", i+1)
		}
		m.retryDue("lab/r1")
	}
	if _, ok := m.passDone("lab/r1", "/netns/a", failed); ok {
		t.Fatalf("a try was set past the last delay, which is the resync's")
	}
	if got, ok := m.passDone("lab/r1", "/netns/b", failed); !ok || got != retryDelays[0] {
		t.Fatalf("the pod made again: retry after %v (%t), want to start over at %v", got, ok, retryDelays[0])
	}
	m.retryDue("lab/r1")
	if _, ok := m.passDone("lab/r1", "/netns/b", nil); ok {
		t.Fatalf("a try was set after a pass that succeeded")
	}
	if got, ok := m.passDone("lab/r1", "/netns/b", failed); !ok || got != retryDelays[0] {
		t.Fatalf("after a success: retry after %v (%t), want to start over at %v", got, ok, retryDelays[0])
	}
	if got, ok := m.passDone("lab/r2", "/netns/c", failed); !ok || got != retryDelays[0] {
		t.Fatalf("another pod: retry after %v (%t), want its own %v", got, ok, retryDelays[0])
	}
}
