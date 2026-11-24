package wireutil

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/vishvananda/netlink"
)

// Shaping is a netem (the default) or a tbf for one direction of a link, with tc's units. It is the
// Topology's, carried as it is declared.
type Shaping struct {
	Qdisc              string
	Delay              string
	Jitter             string
	DelayCorrelation   string
	Loss               string
	LossCorrelation    string
	Duplicate          string
	Corrupt            string
	Reorder            string
	ReorderCorrelation string
	Limit              int
	Rate               string
	Burst              string
	Latency            string
}

// The defaults of a tbf, KubeNDT's: a 32kbit bucket and 50 ms of queue.
const (
	tbfDefaultBurst   = "32kbit"
	tbfDefaultLatency = "50ms"
)

// String is the canonical form of s, for its digest; empty for no shaping.
func (s *Shaping) String() string {
	if s == nil {
		return ""
	}
	q := s.Qdisc
	if q == "" {
		q = "netem"
	}
	return fmt.Sprintf("%s delay=%s jitter=%s dcorr=%s loss=%s lcorr=%s dup=%s corrupt=%s reorder=%s rcorr=%s limit=%d rate=%s burst=%s latency=%s",
		q, s.Delay, s.Jitter, s.DelayCorrelation, s.Loss, s.LossCorrelation, s.Duplicate, s.Corrupt, s.Reorder, s.ReorderCorrelation, s.Limit, s.Rate, s.Burst, s.Latency)
}

// splitUnit cuts a tc value into its number and its unit.
func splitUnit(v string) (float64, string, error) {
	v = strings.TrimSpace(v)
	i := 0
	for i < len(v) && (v[i] >= '0' && v[i] <= '9' || v[i] == '.') {
		i++
	}
	n, err := strconv.ParseFloat(v[:i], 64)
	if err != nil || i == 0 {
		return 0, "", fmt.Errorf("%q is not a number with a unit", v)
	}
	return n, strings.ToLower(v[i:]), nil
}

// parseTime reads a tc time (100ms, 1.5s, 500us) into microseconds.
func parseTime(v string) (uint32, error) {
	n, unit, err := splitUnit(v)
	if err != nil {
		return 0, err
	}
	var us float64
	switch unit {
	case "us", "usec", "usecs":
		us = n
	case "ms", "msec", "msecs":
		us = n * 1e3
	case "s", "sec", "secs":
		us = n * 1e6
	default:
		return 0, fmt.Errorf("%q: the time needs a unit, us, ms or s", v)
	}
	if us > math.MaxUint32 {
		return 0, fmt.Errorf("%q is too long", v)
	}
	return uint32(math.Round(us)), nil
}

// parsePercent reads 1% or 1 into a percentage.
func parsePercent(v string) (float32, error) {
	n, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(v), "%"), 64)
	if err != nil || n < 0 || n > 100 {
		return 0, fmt.Errorf("%q is not a percentage", v)
	}
	return float32(n), nil
}

// parseRate reads a tc rate (10mbit, 100kbps, 1gibit) into bytes per second.
func parseRate(v string) (uint64, error) {
	n, unit, err := splitUnit(v)
	if err != nil {
		return 0, err
	}
	mult := map[string]float64{"": 1, "k": 1e3, "m": 1e6, "g": 1e9, "t": 1e12, "ki": 1 << 10, "mi": 1 << 20, "gi": 1 << 30, "ti": 1 << 40}
	var bits float64
	switch {
	case strings.HasSuffix(unit, "bit"):
		m, ok := mult[strings.TrimSuffix(unit, "bit")]
		if !ok {
			return 0, fmt.Errorf("%q: unknown rate unit", v)
		}
		bits = n * m
	case strings.HasSuffix(unit, "bps"):
		m, ok := mult[strings.TrimSuffix(unit, "bps")]
		if !ok {
			return 0, fmt.Errorf("%q: unknown rate unit", v)
		}
		bits = n * m * 8
	default:
		return 0, fmt.Errorf("%q: the rate needs a unit, as 10mbit", v)
	}
	if bits < 8 {
		return 0, fmt.Errorf("%q is too low a rate", v)
	}
	return uint64(bits / 8), nil
}

// parseSize reads a tc size (32kbit, 4kb, 4096) into bytes.
func parseSize(v string) (uint32, error) {
	n, unit, err := splitUnit(v)
	if err != nil {
		return 0, err
	}
	mult := map[string]float64{"": 1, "b": 1, "k": 1 << 10, "kb": 1 << 10, "m": 1 << 20, "mb": 1 << 20, "g": 1 << 30, "gb": 1 << 30,
		"kbit": 1 << 7, "mbit": 1 << 17, "gbit": 1 << 27}
	m, ok := mult[unit]
	if !ok {
		return 0, fmt.Errorf("%q: unknown size unit", v)
	}
	if n*m > math.MaxUint32 {
		return 0, fmt.Errorf("%q is too large", v)
	}
	return uint32(n * m), nil
}

// optional parses v with parse when it is set.
func optional[T any](errs *[]error, field, v string, parse func(string) (T, error)) T {
	var zero T
	if v == "" {
		return zero
	}
	out, err := parse(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %w", field, err))
	}
	return out
}

// BuildQdisc builds the root qdisc s describes on the link with index linkIndex.
func (s *Shaping) BuildQdisc(linkIndex int) (netlink.Qdisc, error) {
	attrs := netlink.QdiscAttrs{LinkIndex: linkIndex, Handle: netlink.MakeHandle(1, 0), Parent: netlink.HANDLE_ROOT}
	var errs []error
	netemOnly := map[string]string{"delay": s.Delay, "jitter": s.Jitter, "delay_correlation": s.DelayCorrelation, "loss": s.Loss,
		"loss_correlation": s.LossCorrelation, "duplicate": s.Duplicate, "corrupt": s.Corrupt, "reorder": s.Reorder, "reorder_correlation": s.ReorderCorrelation}
	switch s.Qdisc {
	case "", "netem":
		if s.Burst != "" || s.Latency != "" {
			errs = append(errs, errors.New("burst and latency are a tbf's"))
		}
		n := netlink.NetemQdiscAttrs{
			Latency:     optional(&errs, "delay", s.Delay, parseTime),
			Jitter:      optional(&errs, "jitter", s.Jitter, parseTime),
			DelayCorr:   optional(&errs, "delay_correlation", s.DelayCorrelation, parsePercent),
			Loss:        optional(&errs, "loss", s.Loss, parsePercent),
			LossCorr:    optional(&errs, "loss_correlation", s.LossCorrelation, parsePercent),
			Duplicate:   optional(&errs, "duplicate", s.Duplicate, parsePercent),
			CorruptProb: optional(&errs, "corrupt", s.Corrupt, parsePercent),
			ReorderProb: optional(&errs, "reorder", s.Reorder, parsePercent),
			ReorderCorr: optional(&errs, "reorder_correlation", s.ReorderCorrelation, parsePercent),
			Rate64:      optional(&errs, "rate", s.Rate, parseRate),
		}
		if s.Limit < 0 {
			errs = append(errs, fmt.Errorf("limit %d is not a queue length", s.Limit))
		}
		n.Limit = uint32(max(s.Limit, 0))
		if n.Jitter > 0 && n.Latency == 0 {
			errs = append(errs, errors.New("a jitter needs a delay"))
		}
		if s.DelayCorrelation != "" && n.Jitter == 0 {
			errs = append(errs, errors.New("a delay correlation needs a jitter"))
		}
		if n.ReorderProb > 0 && n.Latency == 0 {
			errs = append(errs, errors.New("reordering needs a delay"))
		}
		if len(errs) > 0 {
			return nil, errors.Join(errs...)
		}
		return netlink.NewNetem(attrs, n), nil
	case "tbf":
		for field, v := range netemOnly {
			if v != "" {
				errs = append(errs, fmt.Errorf("%s is a netem's", field))
			}
		}
		if s.Limit != 0 {
			errs = append(errs, errors.New("limit is a netem's; a tbf's queue is its latency"))
		}
		if s.Rate == "" {
			errs = append(errs, errors.New("a tbf needs a rate"))
		}
		burstV, latencyV := s.Burst, s.Latency
		if burstV == "" {
			burstV = tbfDefaultBurst
		}
		if latencyV == "" {
			latencyV = tbfDefaultLatency
		}
		rate := optional(&errs, "rate", s.Rate, parseRate)
		burst := optional(&errs, "burst", burstV, parseSize)
		latency := optional(&errs, "latency", latencyV, parseTime)
		if len(errs) > 0 {
			return nil, errors.Join(errs...)
		}
		// As tc does: the bucket in ticks, and the queue whatever the rate drains in the latency.
		limit := float64(rate)*float64(latency)/1e6 + float64(burst)
		return &netlink.Tbf{
			QdiscAttrs: attrs,
			Rate:       rate,
			Buffer:     netlink.Xmittime(rate, burst),
			Limit:      uint32(min(limit, math.MaxUint32)),
		}, nil
	default:
		return nil, fmt.Errorf("qdisc %q: netem or tbf", s.Qdisc)
	}
}

// shapingAliasPrefix starts the alias of a node end, which carries the digest of its shaping.
const shapingAliasPrefix = "meshnet-shaping/"

// rootQdiscIs says whether the root qdisc of l is of kind.
func rootQdiscIs(l netlink.Link, kind string) bool {
	q, err := rootQdisc(l)
	return err == nil && q != nil && q.Type() == kind
}

// rootQdisc returns the root qdisc of l other than the default, nil when it has none.
func rootQdisc(l netlink.Link) (netlink.Qdisc, error) {
	qdiscs, err := netlink.QdiscList(l)
	if err != nil {
		return nil, err
	}
	for _, q := range qdiscs {
		if q.Attrs().Parent == netlink.HANDLE_ROOT && q.Type() != "noqueue" {
			return q, nil
		}
	}
	return nil, nil
}

// setRootShaping puts s as the root qdisc of l, or takes a netem or tbf off when s is nil. A qdisc
// of the same kind is changed in place, keeping its queue; one of another kind goes first, since
// the kernel does not replace a qdisc with one of another kind under the same handle.
func setRootShaping(l netlink.Link, s *Shaping) error {
	cur, err := rootQdisc(l)
	if err != nil {
		return err
	}
	if s == nil {
		if cur != nil && (cur.Type() == "netem" || cur.Type() == "tbf") {
			return netlink.QdiscDel(cur)
		}
		return nil
	}
	q, err := s.BuildQdisc(l.Attrs().Index)
	if err != nil {
		return err
	}
	if cur != nil && (cur.Type() != q.Type() || cur.Attrs().Handle != q.Attrs().Handle) {
		if err := netlink.QdiscDel(cur); err != nil {
			return fmt.Errorf("failed to remove %s: %w", cur.Type(), err)
		}
	}
	if err := netlink.QdiscReplace(q); err != nil {
		return fmt.Errorf("failed to set %s: %w", q.Type(), err)
	}
	return nil
}

// ShapeHostEnd shapes the traffic that leaves the node end name towards its pod, the direction its
// peer declares, or takes the shaping off when s is nil. The node end is the daemon's, so it is put
// back on every pass; the digest in its alias saves the work when nothing moved.
func ShapeHostEnd(name string, s *Shaping) error {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return err
	}
	want := shapingAliasPrefix + digest(s.String())
	if l.Attrs().Alias == want && (s == nil || rootQdiscIs(l, qdiscKind(s))) {
		return nil
	}
	if err := setRootShaping(l, s); err != nil {
		return fmt.Errorf("node end %s: %w", name, err)
	}
	return netlink.LinkSetAlias(l, want)
}

func qdiscKind(s *Shaping) string {
	if s.Qdisc == "" {
		return "netem"
	}
	return s.Qdisc
}
