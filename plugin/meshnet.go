// The register-and-wait ADD and the DEL are ported from openconfig/kne, third_party/meshnet/plugin/meshnet.go
// at 5a1676c (Apache 2.0), without the gRPC wires.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	types100 "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/plugins/pkg/ns"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	mpb "github.com/emigcm98/meshnet-cni/daemon/proto/meshnet/v1beta1"
	"github.com/emigcm98/meshnet-cni/utils/wireutil"
)

const (
	localhost = "localhost"
	// linksWait is how long an ADD waits for the daemon to wire the pod's links before it lets the pod
	// start anyway; a link whose peer is not up yet arrives later, from the daemon's reconcile.
	linksWait = 30 * time.Second
)

var (
	localDaemon = localhost + ":" + fmt.Sprintf("%d", wireutil.GRPCDefaultPort)
)

type netConf struct {
	types.NetConf
	Delegate map[string]interface{} `json:"delegate"`
}

type k8sArgs struct {
	types.CommonArgs
	K8S_POD_NAME               types.UnmarshallableString
	K8S_POD_NAMESPACE          types.UnmarshallableString
	K8S_POD_INFRA_CONTAINER_ID types.UnmarshallableString
}

// -------------------------------------------------------------------------------------------------
func init() {
	// this ensures that main runs only on main thread (thread group leader).
	// since namespace ops (unshare, setns) are done for a single thread, we
	// must ensure that the goroutine does not jump from OS thread to thread
	runtime.LockOSThread()
}

// -------------------------------------------------------------------------------------------------
// loadConf loads information from cni.conf
func loadConf(bytes []byte) (*netConf, *types100.Result, error) {
	n := &netConf{}
	if err := json.Unmarshal(bytes, n); err != nil {
		return nil, nil, fmt.Errorf("failed to load netconf: %v", err)
	}

	// Parse previous result.
	if n.RawPrevResult == nil {
		// return early if there was no previous result, which is allowed for DEL calls
		return n, &types100.Result{}, nil
	}

	// Parse previous result.
	var result *types100.Result
	var err error
	if err = version.ParsePrevResult(&n.NetConf); err != nil {
		return nil, nil, fmt.Errorf("could not parse prevResult: %v", err)
	}

	result, err = types100.NewResultFromResult(n.PrevResult)
	if err != nil {
		return nil, nil, fmt.Errorf("could not convert result to current version: %v", err)
	}
	return n, result, nil
}

// -------------------------------------------------------------------------------------------------
// cmdAdd registers the pod with the local daemon (its netns and the node address it runs on), which
// wires its links from its reconcile, and waits for them to appear before it lets the pod start.
func cmdAdd(args *skel.CmdArgs) error {
	startTime := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log.Info("Parsing cni .conf file")
	n, result, err := loadConf(args.StdinData)
	if err != nil {
		return err
	}

	log.Info("Parsing CNI_ARGS environment variable")
	cniArgs := k8sArgs{}
	if err := types.LoadArgs(args.Args, &cniArgs); err != nil {
		return err
	}
	podName := string(cniArgs.K8S_POD_NAME)
	kubeNs := string(cniArgs.K8S_POD_NAMESPACE)
	log.Infof("Processing ADD POD %s in namespace %s, container_id %s", podName, kubeNs, string(cniArgs.K8S_POD_INFRA_CONTAINER_ID))
	defer log.Infof("DONE > Processing ADD POD %s in namespace %s, container_id %s", podName, kubeNs, string(cniArgs.K8S_POD_INFRA_CONTAINER_ID))

	conn, err := grpc.Dial(localDaemon, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Errorf("Failed to connect to local meshnetd on %s", localDaemon)
		return err
	}
	defer conn.Close()
	meshnetClient := mpb.NewLocalClient(conn)

	// A daemon that is restarting answers Unavailable for a moment; any other error means the pod
	// has no Topology, which is a pod meshnet has nothing to do with.
	var localPod *mpb.Pod
	pollInterval := 100 * time.Millisecond
	deadline := time.Now().Add(linksWait)
	for {
		localPod, err = meshnetClient.Get(ctx, &mpb.PodQuery{Name: podName, KubeNs: kubeNs})
		if err == nil {
			break
		}
		st, ok := status.FromError(err)
		if ok && (st.Code() == codes.Unavailable || st.Code() == codes.DeadlineExceeded) {
			if time.Now().After(deadline) {
				log.Errorf("Add[%s]: Meshnet daemon %s unavailable after timeout: %v", podName, localDaemon, err)
				return fmt.Errorf("meshnet daemon unavailable on %s: %w", localDaemon, err)
			}
			time.Sleep(pollInterval)
			pollInterval = min(pollInterval*3/2, time.Second)
			continue
		}
		log.Infof("Add: Pod %s:%s was not a topology pod returning", kubeNs, podName)
		return types.PrintResult(result, n.CNIVersion)
	}

	srcIP, _, err := wireutil.VxlanSource(localPod.NodeIp, localPod.NodeIntf)
	if err != nil {
		return err
	}

	localPod.NetNs = args.Netns
	localPod.SrcIp = srcIP
	localPod.ContainerId = args.ContainerID
	log.Infof("Add[%s]: Setting pod alive status on meshnet daemon", podName)
	ok, err := meshnetClient.SetAlive(ctx, localPod)
	if err != nil || !ok.Response {
		log.Errorf("Add[%s]: Failed to set pod alive status", podName)
		return err
	}

	if len(localPod.Links) > 0 {
		waitForLinks(args.Netns, podName, localPod.Links, startTime.Add(linksWait))
	}
	return types.PrintResult(result, n.CNIVersion)
}

// waitForLinks polls the pod's netns until every link's interface is there or the deadline passes.
func waitForLinks(netNS, podName string, links []*mpb.Link, deadline time.Time) {
	pollInterval := 100 * time.Millisecond
	for {
		present := make(map[string]bool)
		_ = ns.WithNetNSPath(netNS, func(_ ns.NetNS) error {
			if list, err := netlink.LinkList(); err == nil {
				for _, l := range list {
					present[l.Attrs().Name] = true
				}
			}
			return nil
		})
		missing := 0
		for _, link := range links {
			if !present[link.LocalIntf] {
				missing++
			}
		}
		if missing == 0 {
			log.Infof("Add[%s]: All %d interfaces are ready", podName, len(links))
			return
		}
		if time.Now().After(deadline) {
			log.Warnf("Add[%s]: %d of %d interfaces still missing after the wait; the pod starts and the daemon wires them later", podName, missing, len(links))
			return
		}
		time.Sleep(pollInterval)
		pollInterval = min(pollInterval*3/2, time.Second)
	}
}

// -------------------------------------------------------------------------------------------------
// cmdDel marks the pod dead and removes its links; the daemons of its peers rewire their ends when
// it comes back, wherever it lands.
func cmdDel(args *skel.CmdArgs) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cniArgs := k8sArgs{}
	if err := types.LoadArgs(args.Args, &cniArgs); err != nil {
		return err
	}
	podName := string(cniArgs.K8S_POD_NAME)
	kubeNs := string(cniArgs.K8S_POD_NAMESPACE)
	log.Infof("========> Processing DEL POD %s in namespace %s, container_id %s", podName, kubeNs, cniArgs.K8S_POD_INFRA_CONTAINER_ID)
	defer log.Infof("DONE ========>  Processing DEL POD %s in namespace %s, container_id %s", podName, kubeNs, cniArgs.K8S_POD_INFRA_CONTAINER_ID)

	n, result, err := loadConf(args.StdinData)
	if err != nil {
		return err
	}

	conn, err := grpc.Dial(localDaemon, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Errorf("Failed to connect to local meshnetd on %s", localDaemon)
		return err
	}
	defer conn.Close()
	meshnetClient := mpb.NewLocalClient(conn)

	localPod, err := meshnetClient.Get(ctx, &mpb.PodQuery{Name: podName, KubeNs: kubeNs})
	if err != nil {
		log.Infof("Del: Pod %s:%s is not in topology returning. err:%v", kubeNs, podName, err)
		return types.PrintResult(result, n.CNIVersion)
	}
	// if the current containerID is not the topo's current containerID, exit here b/c this is a duplicated DEL call.
	if localPod.ContainerId != args.ContainerID {
		log.Infof("Del: Pod %s:%s is a duplicate; skipping", kubeNs, podName)
		return nil
	}

	// By setting srcIP and NetNS to "" we're marking this POD as dead
	localPod.NetNs = ""
	localPod.SrcIp = ""
	localPod.ContainerId = ""
	if _, err := meshnetClient.SetAlive(ctx, localPod); err != nil {
		return fmt.Errorf("del: could not set alive: %v", err)
	}

	for _, link := range localPod.Links {
		if err := wireutil.RemoveLink(args.Netns, link.LocalIntf); err != nil {
			// instead of failing, just log the error and move on
			log.Errorf("Del: Error removing link %s on pod %s: %v", link.LocalIntf, podName, err)
		}
	}
	return nil
}

// -------------------------------------------------------------------------------------------------
func main() {
	fp, err := os.OpenFile("/var/log/meshnet-cni.log", os.O_APPEND|os.O_CREATE|os.O_RDWR, 0666)
	if err == nil {
		log.SetOutput(fp)
	}

	retCode := 0
	e := skel.PluginMainWithError(cmdAdd, cmdGet, cmdDel, version.All, "CNI plugin meshnet v0.3.0")
	if e != nil {
		log.Errorf("failed to run meshnet cni: %v", e.Print())
		retCode = 1
	}
	fp.Close()
	os.Exit(retCode)
}

func cmdGet(args *skel.CmdArgs) error {
	cniArgs := k8sArgs{}
	if err := types.LoadArgs(args.Args, &cniArgs); err != nil {
		return err
	}
	log.Infof("cmdGet called: %+v", args)
	return fmt.Errorf("not implemented")
}
