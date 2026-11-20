package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"runtime"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	types100 "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	mpb "github.com/emigcm98/meshnet-cni/daemon/proto/meshnet/v1beta1"
	"github.com/emigcm98/meshnet-cni/daemon/vxlan"
	"github.com/emigcm98/meshnet-cni/utils/wireutil"
)

const (
	vxlanBase = 5000
	localhost = "localhost"
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

// getVxlanSource uses netlink to get the iface reliably given an IP address.
// when IP and Interface both are present then Interface is going to take preference
// nodeIntf is specified by the user and it's not auto discovered. The user has to be careful that the peer is reachable though this interface otherwise, VxLAN may not work.
// daemonset.yaml meshnet container env required for host_intf override
//
//	       env:
//	         - name: HOST_INTF
//			value: breth2
func getVxlanSource(nodeIP string, nodeIntf string) (string, string, error) {
	if nodeIntf == "" && nodeIP == "" {
		return "", "", fmt.Errorf("meshnetd provided no HOST_IP address: %s or HOST_INTF: %s", nodeIP, nodeIntf)
	}
	nIP := net.ParseIP(nodeIP)
	if nIP == nil && nodeIntf == "" {
		return "", "", fmt.Errorf("parsing failed for meshnetd provided no HOST_IP address: %s and node HOST_INTF: %s", nodeIP, nodeIntf)
	}
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if nodeIntf != "" {
				if i.Name == nodeIntf {
					return ip.String(), nodeIntf, nil
				}
			}
			if nIP != nil && nIP.Equal(ip) {
				log.Infof("Found iface %s for address %s", i.Name, nodeIP)
				return nodeIP, i.Name, nil
			}
		}
	}
	return "", "", fmt.Errorf("no iface found for address %s", nodeIP)
}

// -------------------------------------------------------------------------------------------------
// wireSameHost gives both pods their end of a veth pair. An end that is already there but stands
// alone or is not a veth (the vxlan a pod kept towards the worker its peer has just left) goes
// first, so a pod that comes back on its peer's worker gets wired instead of hitting a name
// collision. The pair is made on the host under deterministic names and each end is moved into
// its pod by its own call, which is idempotent, so a retried ADD finishes what the last one left.
func wireSameHost(kubeNs string, localPod, peerPod *mpb.Pod, link *mpb.Link) error {
	ends := []struct{ netNs, name string }{{localPod.NetNs, link.LocalIntf}, {peerPod.NetNs, link.PeerIntf}}
	veths := 0
	for _, e := range ends {
		kind, exists, err := wireutil.LinkInfo(e.netNs, e.name)
		if err != nil {
			return err
		}
		if exists && kind == "veth" {
			veths++
		}
	}
	if veths < 2 {
		for _, e := range ends {
			if err := wireutil.RemoveLink(e.netNs, e.name); err != nil {
				return err
			}
		}
	}
	mine := wireutil.PodLinkConfig{
		PodName: localPod.Name, PeerPodName: peerPod.Name, LinkUID: link.Uid, KubeNs: kubeNs,
		LocalIntf: link.LocalIntf, LocalIP: link.LocalIp, PeerIntf: link.PeerIntf, PeerIP: link.PeerIp,
	}
	theirs := wireutil.PodLinkConfig{
		PodName: peerPod.Name, PeerPodName: localPod.Name, LinkUID: link.Uid, KubeNs: kubeNs,
		LocalIntf: link.PeerIntf, LocalIP: link.PeerIp, PeerIntf: link.LocalIntf, PeerIP: link.LocalIp,
	}
	if err := wireutil.ConfigurePodLinks(localPod.NetNs, []wireutil.PodLinkConfig{mine}); err != nil {
		return err
	}
	return wireutil.ConfigurePodLinks(peerPod.NetNs, []wireutil.PodLinkConfig{theirs})
}

// -------------------------------------------------------------------------------------------------
// Adds interfaces to a POD. the POD name and the POD Namespace is passed as arguments.
func cmdAdd(args *skel.CmdArgs) error {
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

	log.Infof("Attempting to connect to local meshnet daemon")
	conn, err := grpc.Dial(localDaemon, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Errorf("Failed to connect to local meshnetd on %s", localDaemon)
		return err
	}
	defer conn.Close()

	meshnetClient := mpb.NewLocalClient(conn)

	log.Infof("Add[%s]: Retrieving local pod information from meshnet daemon", podName)
	localPod, err := meshnetClient.Get(ctx, &mpb.PodQuery{
		Name:   podName,
		KubeNs: kubeNs,
	})
	if err != nil {
		log.Errorf("Add: Pod %s:%s was not a topology pod returning", kubeNs, podName)
		return types.PrintResult(result, n.CNIVersion)
	}

	// Finding the source IP and interface for VXLAN VTEP
	srcIP, srcIntf, err := getVxlanSource(localPod.NodeIp, localPod.NodeIntf)
	if err != nil {
		return err
	}

	// Marking pod as "alive" by setting its srcIP and NetNS and ContainerId
	localPod.NetNs = args.Netns
	localPod.SrcIp = srcIP
	localPod.ContainerId = args.ContainerID
	log.Infof("Add[%s]: Setting pod alive status on meshnet daemon", podName)
	ok, err := meshnetClient.SetAlive(ctx, localPod)
	if err != nil || !ok.Response {
		log.Errorf("Add[%s]: Failed to set pod alive status", podName)
		return err
	}

	log.Infof("Add[%s]: Starting to traverse all links", podName)
	for _, link := range localPod.Links { // Iterate over each link of the local pod
		// First option is macvlan interface
		if link.PeerPod == localhost {
			log.Infof("Peer link is MacVlan")
			if err := wireutil.MakeMacvlan(args.Netns, link.LocalIntf, link.PeerIntf, link.LocalIp); err != nil {
				log.Errorf("Failed to add macvlan interface: %v", err)
				return err
			}
			log.Infof("macvlan interface %s@%s has been added", link.LocalIntf, link.PeerIntf)
			continue
		}

		// Initialising peer pod's metadata
		log.Infof("Add[%s]: retrieving peer pod %s information from meshnet daemon", podName, link.PeerPod)
		peerPod, err := meshnetClient.Get(ctx, &mpb.PodQuery{
			Name:   link.PeerPod,
			KubeNs: kubeNs,
		})
		if err != nil {
			log.Errorf("Add[%s]: Failed to retrieve peer pod %s:%s topology", podName, kubeNs, link.PeerPod)
			return err
		}

		isAlive := peerPod.SrcIp != "" && peerPod.NetNs != ""
		log.Infof("Add[%s]: Is peer pod %s alive?: %t", podName, peerPod.Name, isAlive)

		if !isAlive { // This means that our peer pod hasn't come up yet
			// Since there's no way of telling if our peer is going to be on this host or another,
			// the only option is to do nothing, assuming that the peer POD will do all the plumbing when it comes up
			log.Infof("Add[%s]: Peer pod %s@%s (link id %d) isn't alive yet, continuing", podName, peerPod.Name, link.PeerIntf, link.Uid)
			// Here we need to set the skipped flag so that our peer can configure VEth interface when it comes up later
			ok, err := meshnetClient.Skip(ctx, &mpb.SkipQuery{
				Pod:    localPod.Name,
				Peer:   peerPod.Name,
				LinkId: link.Uid,
				KubeNs: kubeNs,
			})
			if err != nil || !ok.Response {
				log.Errorf("Add[%s]: Failed to set a skipped flag on peer %s", podName, peerPod.Name)
				return err
			}
			continue
		}

		log.Infof("Add[%s]: Peer pod %s is alive", podName, peerPod.Name)
		if peerPod.SrcIp == localPod.SrcIp { // This means we're on the same host
			log.Infof("Add[%s]: %s and %s are on the same host", podName, localPod.Name, peerPod.Name)
			_, iExist, err := wireutil.LinkInfo(args.Netns, link.LocalIntf)
			if err != nil {
				return err
			}
			_, pExist, err := wireutil.LinkInfo(peerPod.NetNs, link.PeerIntf)
			if err != nil {
				return err
			}
			log.Infof("Add[%s]: Does the link already exist? Local:%t, Peer:%t", podName, iExist, pExist)

			if !iExist && !pExist { // if neither link exists, we have two options
				log.Infof("Add[%s]: Neither link exists. Checking if we've been skipped for interface %s (link id %d)", podName, link.LocalIntf, link.Uid)
				isSkipped, err := meshnetClient.IsSkipped(ctx, &mpb.SkipQuery{
					Pod:    localPod.Name,
					Peer:   peerPod.Name,
					LinkId: link.Uid,
					KubeNs: kubeNs,
				})
				if err != nil {
					log.Errorf("Add[%s]: Failed to read skipped status from our peer", podName)
					return err
				}
				log.Infof("Add[%s]: Have we been skipped for %s (link id %d) by our peer %s? %t", podName, link.LocalIntf, link.Uid, peerPod.Name, isSkipped.Response)

				// Comparing names to determine higher priority
				higherPrio := localPod.Name > peerPod.Name
				log.Infof("Add[%s]: Do we have a higher priority? %t", podName, higherPrio)

				if !isSkipped.Response && !higherPrio { // peerPod has higherPrio and hasn't skipped us
					// In this case we do nothing, since the pod with a higher IP is supposed to connect veth pair
					log.Infof("Add[%s]: Doing nothing, expecting peer pod %s to connect veth pair", podName, peerPod.Name)
					continue
				}
				log.Infof("Add[%s]: Peer POD has skipped us for %s (link id %d) or we have a higher priority", podName, link.LocalIntf, link.Uid)
			}
			// A link with one end missing, or one end that is not a veth, is a leftover of a pod that
			// moved; it is remade here, which is what the stale-link check of the original never did.
			if err := wireSameHost(kubeNs, localPod, peerPod, link); err != nil {
				log.Errorf("Add[%s]: Error when creating the veth pair with %s: %s", podName, peerPod.Name, err)
				return err
			}
		} else { // This means we're on different hosts
			log.Infof("Add[%s]: %s@%s and %s@%s are on different hosts", podName, localPod.Name, localPod.SrcIp, peerPod.Name, peerPod.SrcIp)
			if _, err := vxlan.CreateOrUpdate(&mpb.RemotePod{
				NetNs:    args.Netns,
				IntfName: link.LocalIntf,
				IntfIp:   link.LocalIp,
				PeerVtep: peerPod.SrcIp,
				Vni:      link.Uid + vxlanBase,
				KubeNs:   kubeNs,
				NodeIntf: srcIntf,
			}); err != nil {
				log.Errorf("Add[%s]: Error when creating a Vxlan interface: %s", podName, err)
				return err
			}

			// Now we need to make an API call to update the remote VTEP to point to us
			payload := &mpb.RemotePod{
				NetNs:    peerPod.NetNs,
				IntfName: link.PeerIntf,
				IntfIp:   link.PeerIp,
				PeerVtep: localPod.SrcIp,
				Vni:      link.Uid + vxlanBase,
				KubeNs:   kubeNs,
				NodeIntf: srcIntf,
			}

			url := fmt.Sprintf("%s:%d", peerPod.SrcIp, wireutil.GRPCDefaultPort)
			log.Infof("Add[%s]: Trying to do a remote update on %s", podName, url)

			remote, err := grpc.Dial(url, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				log.Infof("Add[%s]: Failed to dial remote gRPC url %s", podName, url)
				return err
			}
			remoteClient := mpb.NewRemoteClient(remote)
			ok, err := remoteClient.Update(ctx, payload)
			remote.Close()
			if err != nil || !ok.Response {
				log.Infof("Add[%s]: Failed to do a remote update", podName)
				return err
			}
			log.Infof("Add[%s]: Successfully updated remote meshnet daemon", podName)
		}
	}

	return types.PrintResult(result, n.CNIVersion)
}

// -------------------------------------------------------------------------------------------------
// Deletes interfaces from a POD
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

	log.WithFields(log.Fields{
		"args": fmt.Sprintf("%+v", args),
	}).Info("DEL request arguments")
	log.Info("Parsing cni .conf file")
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

	log.Infof("Del: Retrieving pod's (%s@%s) metadata from meshnet daemon", podName, kubeNs)
	localPod, err := meshnetClient.Get(ctx, &mpb.PodQuery{
		Name:   podName,
		KubeNs: kubeNs,
	})
	if err != nil {
		log.Infof("Del: Pod %s:%s is not in topology returning. err:%v", kubeNs, podName, err)
		return types.PrintResult(result, n.CNIVersion)
	}
	// if the current containerID is not the topo's current containerID, exit here b/c this is a duplicated DEL call.
	if localPod.ContainerId != args.ContainerID {
		log.Infof("Del: Pod %s:%s is a duplicate; skipping", kubeNs, podName)
		return nil
	}

	localPodSrcIp := localPod.SrcIp
	log.Infof("Del: Topology data still exists in CRs, cleaning up it's status")
	// By setting srcIP and NetNS to "" we're marking this POD as dead
	localPod.NetNs = ""
	localPod.SrcIp = ""
	localPod.ContainerId = ""
	_, err = meshnetClient.SetAlive(ctx, localPod)
	if err != nil {
		return fmt.Errorf("del: could not set alive: %v", err)
	}

	log.Infof("Del: Iterating over each link for clean-up")
	for _, link := range localPod.Links { // Iterate over each link of the local pod
		linkType := "veth"
		// The peer tells whether the link was a veth or a vxlan, which only matters for the log.
		log.Infof("Del: Pod %s is retrieving peer pod %s information from meshnet daemon", podName, link.PeerPod)
		peerPod, _ := meshnetClient.Get(ctx, &mpb.PodQuery{
			Name:   link.PeerPod,
			KubeNs: kubeNs,
		})
		if peerPod != nil && peerPod.SrcIp != localPodSrcIp {
			linkType = "vxlan"
		}

		log.Infof("Del: Removing link %s", link.LocalIntf)
		if err := wireutil.RemoveLink(args.Netns, link.LocalIntf); err != nil {
			// instead of failing, just log the error and move on
			log.Errorf("Del: Error removing link %s (%s) on pod %s: %v", link.LocalIntf, linkType, localPod.Name, err)
		}

		// Setting reversed skipped flag so that this pod will try to connect veth pair on restart
		log.Infof("Del: Setting skip-reverse flag on peer %s@%s(link id %d) for local interface %s", link.PeerPod, link.PeerIntf, link.Uid, link.LocalIntf)
		ok, err := meshnetClient.SkipReverse(ctx, &mpb.SkipQuery{
			Pod:    localPod.Name,
			Peer:   link.PeerPod,
			LinkId: link.Uid,
			KubeNs: kubeNs,
		})
		if err != nil || !ok.Response {
			log.Errorf("Del: Failed to set skip reversed flag on our peer %s", link.PeerPod)
			return err
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
	log.Infof("K8S invoked meshnet cni")
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
