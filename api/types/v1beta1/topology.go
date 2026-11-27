// Copyright 2021 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package v1beta1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

//go:generate controller-gen object paths=$GOFILE

type TopologySpec struct {
	metav1.TypeMeta `json:",inline"`
	Links           []Link `json:"links"`
}

type TopologyStatus struct {
	metav1.TypeMeta `json:",inline"`
	// Deprecated: the daemon's reconcile decides who wires a link; kept so old writers are not pruned.
	Skipped     []Skipped `json:"skipped"`
	SrcIP       string    `json:"src_ip"`
	NetNS       string    `json:"net_ns"`
	ContainerID string    `json:"container_id"`
	// PlumbingError is the error of the last reconcile of the pod's links, empty when they are wired.
	PlumbingError string `json:"plumbing_error,omitempty"`
	// Links is the state of each of the pod's links as the daemon of its node last saw it.
	Links []LinkStatus `json:"links,omitempty"`
}

// The states of a link end in LinkStatus.
const (
	LinkUp        = "up"        // wired, with carrier
	LinkDown      = "down"      // wired, without carrier: this port or the peer's is shut
	LinkOff       = "off"       // the peer is not running; the interface waits for it
	LinkUnplugged = "unplugged" // the cable pulled by either end's declaration
	LinkFailed    = "failed"    // could not be wired; Error says why
)

// LinkStatus is the state of one end of a link, in the Topology of the pod that has it.
type LinkStatus struct {
	UID       int    `json:"uid"`
	LocalIntf string `json:"local_intf"`
	PeerPod   string `json:"peer_pod"`
	State     string `json:"state"`
	// Transport is veth (both ends on one node), vxlan (across nodes) or macvlan (an uplink); empty
	// while the peer is off.
	Transport string `json:"transport,omitempty"`
	Node      string `json:"node,omitempty"`
	IfIndex   int    `json:"ifindex,omitempty"`
	MAC       string `json:"mac,omitempty"`
	MTU       int    `json:"mtu,omitempty"`
	VNI       int    `json:"vni,omitempty"`
	Error     string `json:"error,omitempty"`
}

type Skipped struct {
	PodName string `json:"pod_name"`
	LinkId  int64  `json:"link_id"`
}

type Link struct {
	LocalIntf string `json:"local_intf"`
	LocalIP   string `json:"local_ip"`
	PeerIntf  string `json:"peer_intf"`
	PeerIP    string `json:"peer_ip"`
	PeerPod   string `json:"peer_pod"`
	UID       int    `json:"uid"`
	// MTU of the link; both ends get the larger of the two requests, LINK_MTU when neither asks, and
	// otherwise what a vxlan over the node's interface carries.
	MTU int `json:"mtu,omitempty"`
	// Addresses of the local end beyond LocalIP, of either family, in CIDR notation.
	Addresses []string `json:"addresses,omitempty"`
	// MAC of the local end; the kernel's random one when empty.
	MAC string `json:"mac,omitempty"`
	// TxQueueLen of the local end; LINK_TXQUEUELEN when zero.
	TxQueueLen int `json:"txqueuelen,omitempty"`
	// Offloads switches the tx offloads of the local end on (true) or off (false); absent, the end
	// keeps the kernel's, which are on for a veth.
	Offloads *bool `json:"offloads,omitempty"`
	// Unplugged pulls the cable: both ends lose carrier and keep their configuration. Either end
	// declaring it unplugs the link.
	Unplugged bool `json:"unplugged,omitempty"`
	// Shaping of the traffic the local end sends, applied on the way to the peer.
	Shaping *Shaping `json:"shaping,omitempty"`
}

// Shaping is a netem (the default) or a tbf for one direction of a link, with tc's units: times as
// 100ms, percentages as 1%, rates as 10mbit, sizes as 32kbit.
type Shaping struct {
	Qdisc string `json:"qdisc,omitempty"`
	// netem
	Delay              string `json:"delay,omitempty"`
	Jitter             string `json:"jitter,omitempty"`
	DelayCorrelation   string `json:"delay_correlation,omitempty"`
	Loss               string `json:"loss,omitempty"`
	LossCorrelation    string `json:"loss_correlation,omitempty"`
	Duplicate          string `json:"duplicate,omitempty"`
	Corrupt            string `json:"corrupt,omitempty"`
	Reorder            string `json:"reorder,omitempty"`
	ReorderCorrelation string `json:"reorder_correlation,omitempty"`
	Limit              int    `json:"limit,omitempty"`
	// Rate is netem's link rate, or tbf's rate, which tbf requires.
	Rate string `json:"rate,omitempty"`
	// tbf
	Burst   string `json:"burst,omitempty"`
	Latency string `json:"latency,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type Topology struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Status TopologyStatus `json:"status"`
	Spec   TopologySpec   `json:"spec"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type TopologyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Topology `json:"items"`
}
