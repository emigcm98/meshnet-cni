package meshnet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/emigcm98/meshnet-cni/api/types/v1beta1"
	"github.com/emigcm98/meshnet-cni/utils/wireutil"
	"github.com/vishvananda/netlink"
)

// metricsDefaultPort serves the links' metrics, next to the gRPC port; METRICS_PORT=0 turns it off.
const metricsDefaultPort = 51112

func metricsPort() int {
	if v, ok := os.LookupEnv("METRICS_PORT"); ok {
		if p, err := strconv.Atoi(v); err == nil && p >= 0 {
			return p
		}
	}
	return metricsDefaultPort
}

// linkCounters are a link's counters as its pod sees them.
type linkCounters struct {
	rxBytes, rxPackets, rxDrops uint64
	txBytes, txPackets, txDrops uint64
	shapingDrops                uint64
	// rps says whether the node end steers each flow to one CPU, as the daemon wants it.
	rps bool
}

// readNodeEnd reads the counters of a link from its node end: what the node end receives is what the
// pod sent, and what it transmits is what the pod receives, after the shaping of its root qdisc. Its
// RPS is read from sysfs too, so a node where it could not be set shows it.
func readNodeEnd(name string) (linkCounters, bool) {
	l, err := netlink.LinkByName(name)
	if err != nil || l.Attrs().Statistics == nil {
		return linkCounters{}, false
	}
	s := l.Attrs().Statistics
	c := linkCounters{
		rxBytes: s.TxBytes, rxPackets: s.TxPackets, rxDrops: s.TxDropped,
		txBytes: s.RxBytes, txPackets: s.RxPackets, txDrops: s.RxDropped,
	}
	if qdiscs, err := netlink.QdiscList(l); err == nil {
		for _, q := range qdiscs {
			if st := q.Attrs().Statistics; q.Attrs().Parent == netlink.HANDLE_ROOT && st != nil && st.Queue != nil {
				c.shapingDrops = uint64(st.Queue.Drops)
			}
		}
	}
	c.rps = wireutil.HasRPS(name, wireutil.RPSMask())
	return c, true
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func labels(pairs ...string) string {
	parts := make([]string, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, fmt.Sprintf(`%s="%s"`, pairs[i], labelEscaper.Replace(pairs[i+1])))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

type metricFamily struct {
	name, kind, help string
	value            func(c linkCounters) uint64
}

// counterFamilies are the counters of a link that has a node end.
var counterFamilies = []metricFamily{
	{"meshnet_link_receive_bytes_total", "counter", "Bytes the pod received on the link.", func(c linkCounters) uint64 { return c.rxBytes }},
	{"meshnet_link_receive_packets_total", "counter", "Packets the pod received on the link.", func(c linkCounters) uint64 { return c.rxPackets }},
	{"meshnet_link_receive_drops_total", "counter", "Packets towards the pod dropped on the node end.", func(c linkCounters) uint64 { return c.rxDrops }},
	{"meshnet_link_transmit_bytes_total", "counter", "Bytes the pod sent on the link.", func(c linkCounters) uint64 { return c.txBytes }},
	{"meshnet_link_transmit_packets_total", "counter", "Packets the pod sent on the link.", func(c linkCounters) uint64 { return c.txPackets }},
	{"meshnet_link_transmit_drops_total", "counter", "Packets from the pod dropped on the node end.", func(c linkCounters) uint64 { return c.txDrops }},
	{"meshnet_link_shaping_drops_total", "counter", "Packets towards the pod dropped by the link's shaping: loss and full queues.", func(c linkCounters) uint64 { return c.shapingDrops }},
}

// writeMetrics writes the Prometheus text of the local pods' links in podLinks (keyed namespace/pod),
// reading each node end with read; an uplink, which has no node end, gets its state alone.
func writeMetrics(w io.Writer, podLinks map[string][]v1beta1.LinkStatus, read func(string) (linkCounters, bool)) {
	keys := make([]string, 0, len(podLinks))
	for k := range podLinks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	type row struct {
		base, info string
		up         int
		counters   *linkCounters
	}
	var rows []row
	for _, key := range keys {
		ns, pod, _ := strings.Cut(key, "/")
		links := append([]v1beta1.LinkStatus(nil), podLinks[key]...)
		sort.Slice(links, func(i, j int) bool { return links[i].UID < links[j].UID })
		for _, l := range links {
			base := []string{"namespace", ns, "pod", pod, "interface", l.LocalIntf, "peer_pod", l.PeerPod, "uid", strconv.Itoa(l.UID)}
			r := row{
				base: labels(base...),
				info: labels(append(base, "state", l.State, "transport", l.Transport, "node", l.Node)...),
			}
			if l.State == v1beta1.LinkUp {
				r.up = 1
			}
			if l.Transport == "veth" || l.Transport == "vxlan" || l.State == v1beta1.LinkOff || l.State == v1beta1.LinkUnplugged {
				if c, ok := read(wireutil.HostEndName(ns, pod, int64(l.UID))); ok {
					r.counters = &c
				}
			}
			rows = append(rows, r)
		}
	}
	fmt.Fprintln(w, "# HELP meshnet_link_info The state of a link end, as in its Topology's status.")
	fmt.Fprintln(w, "# TYPE meshnet_link_info gauge")
	for _, r := range rows {
		fmt.Fprintf(w, "meshnet_link_info%s 1\n", r.info)
	}
	fmt.Fprintln(w, "# HELP meshnet_link_up Whether the link end is up, with carrier.")
	fmt.Fprintln(w, "# TYPE meshnet_link_up gauge")
	for _, r := range rows {
		fmt.Fprintf(w, "meshnet_link_up%s %d\n", r.base, r.up)
	}
	fmt.Fprintln(w, "# HELP meshnet_link_rps Whether the link's node end steers each flow to one CPU (RPS), which keeps it in order across nodes.")
	fmt.Fprintln(w, "# TYPE meshnet_link_rps gauge")
	for _, r := range rows {
		if r.counters != nil {
			v := 0
			if r.counters.rps {
				v = 1
			}
			fmt.Fprintf(w, "meshnet_link_rps%s %d\n", r.base, v)
		}
	}
	for _, f := range counterFamilies {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", f.name, f.help, f.name, f.kind)
		for _, r := range rows {
			if r.counters != nil {
				fmt.Fprintf(w, "%s%s %d\n", f.name, r.base, f.value(*r.counters))
			}
		}
	}
}

// serveMetrics serves /metrics until ctx ends.
func (m *Meshnet) serveMetrics(ctx context.Context) {
	port := metricsPort()
	if port == 0 {
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		m.linksMu.Lock()
		snapshot := make(map[string][]v1beta1.LinkStatus, len(m.podLinks))
		for k, v := range m.podLinks {
			snapshot[k] = v
		}
		m.linksMu.Unlock()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		writeMetrics(w, snapshot, readNodeEnd)
	})
	srv := &http.Server{Addr: fmt.Sprintf(":%d", port), Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	mnetdLogger.Infof("Metrics on :%d/metrics", port)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		mnetdLogger.Errorf("serveMetrics: %v", err)
	}
}
