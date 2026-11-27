package wireutil

import (
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	log "github.com/sirupsen/logrus"
)

// A pod end hands every frame to its node end through the backlog of the CPU its sender runs on. A
// sender that moves to another CPU leaves its flow to two backlogs drained at once, and on a NIC with a
// tx queue per CPU the two halves leave by different queues and overtake each other, hundreds of times
// per 10 s of one TCP stream across nodes. RPS on the node end steers each flow to one CPU by its hash,
// so it stays in order whichever CPU sends it.

// RPSEnv names the CPUs the node ends steer flows to, as a CPU list such as 0-3,8-11; "none" steers
// none, and unset means every online CPU.
const RPSEnv = "NODE_END_RPS_CPUS"

const (
	sysClassNet = "/sys/class/net"
	cpuOnline   = "/sys/devices/system/cpu/online"
)

var (
	rpsOnce     sync.Once
	rpsMask     *big.Int
	rpsReadOnly atomic.Bool
)

// RPSMask is the mask of CPUs the node ends steer flows to, settled once from RPSEnv and the CPUs
// online. A setting that names no online CPU falls back to all of them.
func RPSMask() *big.Int {
	rpsOnce.Do(func() {
		online, err := os.ReadFile(cpuOnline)
		if err != nil {
			log.Warnf("RPS: cannot read the CPUs online (%v), node ends steer to CPU 0", err)
			online = []byte("0")
		}
		setting := os.Getenv(RPSEnv)
		m, err := settleRPSMask(setting, strings.TrimSpace(string(online)))
		if err != nil {
			log.Warnf("RPS: %s=%q: %v; every online CPU instead", RPSEnv, setting, err)
			m, _ = settleRPSMask("", strings.TrimSpace(string(online)))
		}
		rpsMask = m
		if m.Sign() == 0 {
			log.Infof("RPS: node ends do not steer flows (%s=none)", RPSEnv)
		} else {
			log.Infof("RPS: node ends steer each flow to one of the CPUs of mask %s", FormatMask(m))
		}
	})
	return rpsMask
}

// settleRPSMask is the mask a setting of RPSEnv gives with the CPUs online, both CPU lists.
func settleRPSMask(setting, online string) (*big.Int, error) {
	all, err := ParseCPUList(online)
	if err != nil {
		return nil, fmt.Errorf("the CPUs online, %q: %w", online, err)
	}
	switch strings.TrimSpace(setting) {
	case "":
		return all, nil
	case "none":
		return new(big.Int), nil
	}
	m, err := ParseCPUList(setting)
	if err != nil {
		return nil, err
	}
	m.And(m, all)
	if m.Sign() == 0 {
		return nil, errors.New("no CPU of it is online")
	}
	return m, nil
}

// ParseCPUList reads a CPU list as the kernel writes one, 0-3,8,10-11, into a mask.
func ParseCPUList(s string) (*big.Int, error) {
	m := new(big.Int)
	for _, part := range strings.Split(strings.TrimSpace(s), ",") {
		lo, hi, isRange := strings.Cut(strings.TrimSpace(part), "-")
		a, err := strconv.Atoi(lo)
		if err != nil || a < 0 {
			return nil, fmt.Errorf("%q is not a CPU list", s)
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil || b < a {
				return nil, fmt.Errorf("%q is not a CPU list", s)
			}
		}
		if b > 8191 {
			return nil, fmt.Errorf("CPU %d is beyond what the kernel numbers", b)
		}
		for c := a; c <= b; c++ {
			m.SetBit(m, c, 1)
		}
	}
	return m, nil
}

// FormatMask writes a mask as sysfs takes and shows a cpumask: hex in groups of 32 bits separated by
// commas, the highest first, such as ff,ffffffff for CPUs 0 to 39.
func FormatMask(m *big.Int) string {
	h := m.Text(16)
	if len(h) <= 8 {
		return h
	}
	h = strings.Repeat("0", (8-len(h)%8)%8) + h
	groups := make([]string, 0, len(h)/8)
	for i := 0; i < len(h); i += 8 {
		groups = append(groups, h[i:i+8])
	}
	groups[0] = strings.TrimLeft(groups[0], "0")
	return strings.Join(groups, ",")
}

// ParseMask reads a cpumask as sysfs shows it, such as 000000ff,ffffffff or 3f.
func ParseMask(s string) (*big.Int, error) {
	h := strings.ReplaceAll(strings.TrimSpace(s), ",", "")
	if h == "" {
		return new(big.Int), nil
	}
	m, ok := new(big.Int).SetString(h, 16)
	if !ok {
		return nil, fmt.Errorf("%q is not a cpumask", s)
	}
	return m, nil
}

func rpsFiles(name string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(sysClassNet, name, "queues", "rx-*", "rps_cpus"))
	if err == nil && len(files) == 0 {
		err = fmt.Errorf("%s has no rx queue in sysfs", name)
	}
	return files, err
}

// SetRPS steers the flows the node link name receives to the CPUs of mask, on every rx queue it has,
// writing only what differs. A read-only sysfs, as a kind node has, leaves the link without RPS: it is
// logged once, and HasRPS tells it link by link.
func SetRPS(name string, mask *big.Int) error {
	files, err := rpsFiles(name)
	if err != nil {
		return err
	}
	want := FormatMask(mask)
	for _, f := range files {
		if raw, err := os.ReadFile(f); err == nil {
			if have, err := ParseMask(string(raw)); err == nil && have.Cmp(mask) == 0 {
				continue
			}
		}
		if err := os.WriteFile(f, []byte(want), 0o644); err != nil {
			if errors.Is(err, syscall.EROFS) && !rpsReadOnly.Swap(true) {
				log.Warnf("RPS: sysfs is read-only here, so node ends stay without RPS and a flow across nodes may reorder; meshnet_link_rps shows it per link")
			}
			return fmt.Errorf("failed to steer the flows of %s to CPUs %s: %w", name, want, err)
		}
	}
	return nil
}

// HasRPS says whether every rx queue of the node link name steers flows to the CPUs of mask, which
// must name some.
func HasRPS(name string, mask *big.Int) bool {
	if mask.Sign() == 0 {
		return false
	}
	files, err := rpsFiles(name)
	if err != nil {
		return false
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return false
		}
		if have, err := ParseMask(string(raw)); err != nil || have.Cmp(mask) != 0 {
			return false
		}
	}
	return true
}

// ensureRPS gives a node link the daemon owns its RPS; a read-only sysfs is already logged once.
func ensureRPS(name string) {
	if err := SetRPS(name, RPSMask()); err != nil && !errors.Is(err, syscall.EROFS) {
		log.Warnf("RPS: %v", err)
	}
}
