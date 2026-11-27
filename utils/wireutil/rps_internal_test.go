package wireutil

import (
	"testing"
)

// CPU lists and cpumasks as the kernel writes them, over more than 32 CPUs too, and what a setting of
// NODE_END_RPS_CPUS gives with the CPUs online.
func TestRPSMask(t *testing.T) {
	for _, tt := range []struct{ list, mask string }{
		{"0", "1"},
		{"0-5", "3f"},
		{"0-3,8-11", "f0f"},
		{"0-31", "ffffffff"},
		{"0-39", "ff,ffffffff"},
		{"32", "1,00000000"},
		{"0-71", "ff,ffffffff,ffffffff"},
		{"4,40-41,70", "40,00000300,00000010"},
	} {
		m, err := ParseCPUList(tt.list)
		if err != nil {
			t.Fatalf("ParseCPUList(%q): %v", tt.list, err)
		}
		if got := FormatMask(m); got != tt.mask {
			t.Errorf("CPUs %s: mask %q, want %q", tt.list, got, tt.mask)
		}
		back, err := ParseMask(tt.mask)
		if err != nil || back.Cmp(m) != 0 {
			t.Errorf("ParseMask(%q) = %v, %v; want CPUs %s back", tt.mask, back, err, tt.list)
		}
	}
	// sysfs shows a mask zero-padded to the CPUs the kernel numbers.
	for shown, list := range map[string]string{"000000ff,ffffffff": "0-39", "00": "", "3f": "0-5", "0000003f": "0-5"} {
		m, err := ParseMask(shown)
		if err != nil {
			t.Fatalf("ParseMask(%q): %v", shown, err)
		}
		want := "0"
		if list != "" {
			w, _ := ParseCPUList(list)
			want = FormatMask(w)
		}
		if FormatMask(m) != want {
			t.Errorf("ParseMask(%q) = %s, want %s", shown, FormatMask(m), want)
		}
	}
	for _, bad := range []string{"", "a", "3-1", "-1", "1,,2", "0-9000"} {
		if _, err := ParseCPUList(bad); err == nil {
			t.Errorf("ParseCPUList(%q) took it", bad)
		}
	}

	for _, tt := range []struct {
		setting, online, want string
		fails                 bool
	}{
		{"", "0-5", "3f", false},
		{"none", "0-5", "0", false},
		{"2-5", "0-5", "3c", false},
		{"2-5,100", "0-7", "3c", false},
		{"0-63", "0-39", "ff,ffffffff", false},
		{"8-9", "0-5", "", true},
		{"two", "0-5", "", true},
	} {
		m, err := settleRPSMask(tt.setting, tt.online)
		if tt.fails {
			if err == nil {
				t.Errorf("setting %q with CPUs %s online gave %s, want an error", tt.setting, tt.online, FormatMask(m))
			}
			continue
		}
		if err != nil || FormatMask(m) != tt.want {
			t.Errorf("setting %q with CPUs %s online: %v, %v; want %s", tt.setting, tt.online, m, err, tt.want)
		}
	}
}
