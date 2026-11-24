package wireutil

import (
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
)

func TestParseUnits(t *testing.T) {
	times := map[string]uint32{"100ms": 100000, "1.5s": 1500000, "500us": 500, "2msec": 2000, "1sec": 1000000}
	for v, want := range times {
		if got, err := parseTime(v); err != nil || got != want {
			t.Errorf("parseTime(%q) = %d, %v; want %d", v, got, err, want)
		}
	}
	for _, v := range []string{"100", "10m", "ms", ""} {
		if _, err := parseTime(v); err == nil {
			t.Errorf("parseTime(%q) took a value without a time unit", v)
		}
	}
	percents := map[string]float32{"1%": 1, "0.5%": 0.5, "25": 25}
	for v, want := range percents {
		if got, err := parsePercent(v); err != nil || got != want {
			t.Errorf("parsePercent(%q) = %v, %v; want %v", v, got, err, want)
		}
	}
	if _, err := parsePercent("150%"); err == nil {
		t.Errorf("parsePercent took 150%%")
	}
	// Rates in bytes per second, as the kernel wants them: k is 1000, ki is 1024, bps is bytes.
	rates := map[string]uint64{"10mbit": 1250000, "1gbit": 125000000, "100kbps": 100000, "8kibit": 1024, "64bit": 8}
	for v, want := range rates {
		if got, err := parseRate(v); err != nil || got != want {
			t.Errorf("parseRate(%q) = %d, %v; want %d", v, got, err, want)
		}
	}
	for _, v := range []string{"10", "10m", "10mb", "1bit"} {
		if _, err := parseRate(v); err == nil {
			t.Errorf("parseRate(%q) took a bad rate", v)
		}
	}
	// Sizes as tc reads them: k is 1024 bytes, kbit 1024 bits.
	sizes := map[string]uint32{"32kbit": 4096, "4096": 4096, "4k": 4096, "4kb": 4096, "1mbit": 131072, "100b": 100}
	for v, want := range sizes {
		if got, err := parseSize(v); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", v, got, err, want)
		}
	}
}

func TestBuildQdisc(t *testing.T) {
	q, err := (&Shaping{Delay: "20ms", Jitter: "2ms", Loss: "1%", Rate: "10mbit", Limit: 500}).BuildQdisc(7)
	if err != nil {
		t.Fatalf("netem: %v", err)
	}
	n, ok := q.(*netlink.Netem)
	if !ok || n.Attrs().LinkIndex != 7 || n.Attrs().Parent != netlink.HANDLE_ROOT || n.Limit != 500 || n.Rate64 != 1250000 || n.Loss == 0 || n.Latency == 0 || n.Jitter == 0 {
		t.Errorf("netem built as %+v", q)
	}
	q, err = (&Shaping{Qdisc: "tbf", Rate: "10mbit"}).BuildQdisc(7)
	if err != nil {
		t.Fatalf("tbf: %v", err)
	}
	// The defaults: a 32kbit bucket and 50 ms of queue at 1.25 MB/s.
	if f, ok := q.(*netlink.Tbf); !ok || f.Rate != 1250000 || f.Limit != 1250000/20+4096 || f.Buffer != netlink.Xmittime(1250000, 4096) {
		t.Errorf("tbf built as %+v", q)
	}

	bad := map[string]*Shaping{
		"a jitter needs a delay":             {Jitter: "2ms"},
		"a delay correlation needs a jitter": {Delay: "10ms", DelayCorrelation: "25%"},
		"reordering needs a delay":           {Reorder: "5%"},
		"burst and latency are a tbf's":      {Delay: "10ms", Burst: "32kbit"},
		"a tbf needs a rate":                 {Qdisc: "tbf"},
		"delay is a netem's":                 {Qdisc: "tbf", Rate: "1mbit", Delay: "10ms"},
		"netem or tbf":                       {Qdisc: "htb"},
		"the time needs a unit":              {Delay: "100"},
	}
	for want, s := range bad {
		if _, err := s.BuildQdisc(1); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: error %v, want one saying %q", s, err, want)
		}
	}
}

func TestShapingDigest(t *testing.T) {
	var none *Shaping
	if none.String() != "" {
		t.Errorf("no shaping must have an empty canonical form")
	}
	a, b := &Shaping{Delay: "10ms"}, &Shaping{Qdisc: "netem", Delay: "10ms"}
	if a.String() != b.String() {
		t.Errorf("netem by default and netem said aloud must digest the same: %q, %q", a, b)
	}
	if a.String() == (&Shaping{Delay: "20ms"}).String() {
		t.Errorf("two delays digest the same")
	}
}
