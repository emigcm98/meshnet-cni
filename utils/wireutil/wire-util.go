package wireutil

import (
	"fmt"
	"net"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/safchain/ethtool"
)

const GRPCDefaultPort = 51111

func SetTxChecksumOff(intfName, nsName string) error {
	var vethNs ns.NetNS
	var err error

	if vethNs, err = ns.GetNS(nsName); err != nil {
		return fmt.Errorf("could not get required ns %s: %v", nsName, err)
	}
	defer vethNs.Close()

	err = vethNs.Do(func(_ ns.NetNS) error {
		etlHndl, err := ethtool.NewEthtool()
		if err != nil {
			return fmt.Errorf("could not open ethtool handle: %v", err)
		}
		defer etlHndl.Close()

		etlConf := map[string]bool{
			"tx-checksum-ipv4":             false,
			"tx-checksum-ipv6":             false,
			"tx-checksum-ip-generic":       false,
			"tx-tcp-segmentation":          false,
			"tx-tcp6-segmentation":         false,
			"tx-checksum-fcoe-crc":         false,
			"tx-checksum-sctp":             false,
			"tx-tcp-ecn-segmentation":      false,
			"tx-tcp-mangleid-segmentation": false,
		}

		err = etlHndl.Change(intfName, etlConf)
		if err != nil {
			return fmt.Errorf("could not set tx checksum on interface %s, ns %s: %v", intfName, nsName, err)
		}
		return nil
	})
	return err
}

// VxlanSource returns the address and the interface the node's VXLAN tunnels start from: the
// interface named by nodeIntf (HOST_INTF) when set, otherwise the one holding nodeIP (HOST_IP).
// The plugin records the address as the pod's src_ip and the daemon compares it with its own,
// so both call this with the same values.
func VxlanSource(nodeIP string, nodeIntf string) (string, string, error) {
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
			if nodeIntf != "" && i.Name == nodeIntf {
				return ip.String(), nodeIntf, nil
			}
			if nIP != nil && nIP.Equal(ip) {
				return nodeIP, i.Name, nil
			}
		}
	}
	return "", "", fmt.Errorf("no iface found for address %s", nodeIP)
}
