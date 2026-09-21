package network

import (
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

type TAPDevice struct {
	Name string
}

type BridgeManager struct {
	BridgeName string
	BridgeIP   string
	Subnet     *net.IPNet
	mu         sync.Mutex
	usedIPs    map[string]bool
	nextTAPID  int
	freedTAPs  []int
}

func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v failed: %w (output: %s)", name, args, err, string(output))
	}
	return nil
}

func runCmdOutput(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %v failed: %w", name, args, err)
	}
	return string(output), nil
}

func NewBridgeManager(bridgeName, cidr string) *BridgeManager {
	_, subnet, err := net.ParseCIDR(cidr)
	if err != nil {
		_, subnet, _ = net.ParseCIDR("172.16.0.0/24")
	}

	gatewayIP := make(net.IP, len(subnet.IP))
	copy(gatewayIP, subnet.IP)
	gatewayIP = gatewayIP.To4()
	if gatewayIP != nil {
		gatewayIP[3]++
	}

	bm := &BridgeManager{
		BridgeName: bridgeName,
		BridgeIP:   gatewayIP.String(),
		Subnet:     subnet,
		usedIPs:    make(map[string]bool),
	}

	bm.usedIPs[gatewayIP.String()] = true
	bm.usedIPs[subnet.IP.String()] = true

	return bm
}

// linkExists reports whether an interface with the given name is present in the
// root network namespace.
func linkExists(name string) bool {
	if name == "" {
		return false
	}
	output, err := runCmdOutput("ip", "link", "show", name)
	return err == nil && strings.Contains(output, name)
}

// tapIndex extracts N from a "tapN" device name. It deliberately rejects names
// such as "tapns0" (used by netns mode) so bridge-mode bookkeeping never treats
// a namespaced TAP as one of its own.
func tapIndex(name string) (int, bool) {
	if !strings.HasPrefix(name, "tap") {
		return 0, false
	}
	idx, err := strconv.Atoi(strings.TrimPrefix(name, "tap"))
	if err != nil || idx < 0 {
		return 0, false
	}
	return idx, true
}

// existingTAPIndexes enumerates the indexes of TAP devices that currently exist
// in the root namespace. It is used to make allocation restart-safe.
func existingTAPIndexes() map[int]bool {
	output, err := runCmdOutput("ip", "-o", "link", "show")
	if err != nil {
		return nil
	}
	indexes := make(map[int]bool)
	for _, line := range strings.Split(output, "\n") {
		for _, field := range strings.Fields(line) {
			if idx, ok := tapIndex(strings.TrimSuffix(field, ":")); ok {
				indexes[idx] = true
			}
		}
	}
	return indexes
}

func (bm *BridgeManager) bridgeExists() bool {
	return linkExists(bm.BridgeName)
}

func (bm *BridgeManager) EnsureBridge() error {
	if bm.bridgeExists() {
		return runCmd("ip", "link", "set", bm.BridgeName, "up")
	}

	if err := runCmd("ip", "link", "add", "name", bm.BridgeName, "type", "bridge"); err != nil {
		return fmt.Errorf("failed to create bridge: %w", err)
	}

	maskSize, _ := bm.Subnet.Mask.Size()
	ipCIDR := fmt.Sprintf("%s/%d", bm.BridgeIP, maskSize)
	if err := runCmd("ip", "addr", "add", ipCIDR, "dev", bm.BridgeName); err != nil {
		return fmt.Errorf("failed to add IP to bridge: %w", err)
	}

	return runCmd("ip", "link", "set", bm.BridgeName, "up")
}

// CreateTAPDevice creates a brand new TAP device. It deliberately refuses to
// reuse an interface that already exists: a pre-existing TAP with the same name
// belongs to another (possibly orphaned) VM, and silently adopting it would make
// two VMs share one device.
func (bm *BridgeManager) CreateTAPDevice(tapName string) (*TAPDevice, error) {
	if linkExists(tapName) {
		return nil, fmt.Errorf("TAP device %q already exists; refusing to adopt an existing interface", tapName)
	}

	if err := runCmd("ip", "tuntap", "add", "mode", "tap", "name", tapName); err != nil {
		return nil, fmt.Errorf("failed to create TAP device: %w", err)
	}

	if err := runCmd("ip", "link", "set", tapName, "up"); err != nil {
		runCmd("ip", "link", "delete", tapName)
		return nil, fmt.Errorf("failed to bring TAP device up: %w", err)
	}

	return &TAPDevice{Name: tapName}, nil
}

func (bm *BridgeManager) AttachTAPToBridge(tapName string) error {
	return runCmd("ip", "link", "set", tapName, "master", bm.BridgeName)
}

func (bm *BridgeManager) DeleteTAPDevice(tapName string) error {
	err := runCmd("ip", "link", "delete", tapName)
	if idx, ok := tapIndex(tapName); ok {
		bm.mu.Lock()
		bm.freedTAPs = append(bm.freedTAPs, idx)
		bm.mu.Unlock()
	}
	return err
}

// AllocateVMIP hands out the next unused guest address. Before allocating it
// marks the address that maps to every TAP device still present on the host as
// in use, so a restarted worker does not reissue an address that an orphaned VM
// (from a worker that was killed) is still using.
func (bm *BridgeManager) AllocateVMIP() (string, error) {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	bm.reserveExistingTAPAddressesLocked()

	ip, err := allocateIP(bm.Subnet, bm.usedIPs)
	if err != nil {
		return "", err
	}

	bm.usedIPs[ip] = true
	return ip, nil
}

// reserveExistingTAPAddressesLocked correlates the existing TAP indexes with
// guest addresses using the same "tapN <-> first guest address + N" convention
// that drives allocation, and reserves them. Callers must hold bm.mu.
func (bm *BridgeManager) reserveExistingTAPAddressesLocked() {
	indexes := existingTAPIndexes()
	if len(indexes) == 0 {
		return
	}
	base := ipToUint32(bm.Subnet.IP)
	for idx := range indexes {
		candidate := uint32ToIP(base + 2 + uint32(idx))
		if bm.Subnet.Contains(candidate) {
			bm.usedIPs[candidate.String()] = true
		}
	}
}

func (bm *BridgeManager) ReleaseVMIP(ip string) {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	delete(bm.usedIPs, ip)
}

func (bm *BridgeManager) GetGatewayIP() string {
	return bm.BridgeIP
}

// NextTAPName returns the next free TAP device name, skipping indexes whose
// interface already exists (e.g. left behind by a previous worker) and reusing
// indexes released by DeleteTAPDevice. Reusing released indexes keeps TAP
// indexes aligned with the guest addresses derived from them.
func (bm *BridgeManager) NextTAPName() string {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	for len(bm.freedTAPs) > 0 {
		idx := bm.freedTAPs[len(bm.freedTAPs)-1]
		bm.freedTAPs = bm.freedTAPs[:len(bm.freedTAPs)-1]
		if name := fmt.Sprintf("tap%d", idx); !linkExists(name) {
			return name
		}
	}

	// Bounded probing keeps a pathological environment from looping forever;
	// if every probe is taken the returned name fails loudly at creation time.
	const maxProbe = 1 << 16
	for attempt := 0; attempt < maxProbe; attempt++ {
		idx := bm.nextTAPID
		bm.nextTAPID++
		if name := fmt.Sprintf("tap%d", idx); !linkExists(name) {
			return name
		}
	}

	name := fmt.Sprintf("tap%d", bm.nextTAPID)
	bm.nextTAPID++
	return name
}

// SetupNAT installs the egress NAT rules for the manager's subnet.
//
// Bridge mode (BridgeName set) scopes the FORWARD rules to that bridge. Netns
// mode (empty bridge name) has no single bridge: per-instance traffic arrives
// on host-side veths, so the rules are scoped to the subnet instead. Passing an
// empty bridge name therefore produces no malformed "-i \"\"" rules.
//
// All rules are checked before being added, so this is idempotent and safe to
// call on every worker start. Errors are returned (never silently dropped).
func (bm *BridgeManager) SetupNAT(externalInterface string) error {
	if externalInterface == "" {
		return fmt.Errorf("cannot configure egress NAT without an external interface")
	}
	if bm.Subnet == nil {
		return fmt.Errorf("cannot configure egress NAT without a subnet")
	}

	if err := runCmd("sysctl", "-w", "net.ipv4.ip_forward=1"); err != nil {
		return fmt.Errorf("failed to enable IP forwarding: %w", err)
	}

	subnet := bm.Subnet.String()
	var errs []error

	if err := iptablesEnsure("nat", "POSTROUTING",
		"-s", subnet, "-o", externalInterface, "-j", "MASQUERADE"); err != nil {
		errs = append(errs, fmt.Errorf("masquerade rule: %w", err))
	}

	if bm.BridgeName != "" {
		if err := iptablesEnsure("", "FORWARD",
			"-i", bm.BridgeName, "-o", externalInterface, "-j", "ACCEPT"); err != nil {
			errs = append(errs, fmt.Errorf("egress forward rule: %w", err))
		}
		if err := iptablesEnsure("", "FORWARD",
			"-i", externalInterface, "-o", bm.BridgeName,
			"-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"); err != nil {
			errs = append(errs, fmt.Errorf("return forward rule: %w", err))
		}
	} else {
		if err := iptablesEnsure("", "FORWARD",
			"-s", subnet, "-o", externalInterface, "-j", "ACCEPT"); err != nil {
			errs = append(errs, fmt.Errorf("egress forward rule: %w", err))
		}
		if err := iptablesEnsure("", "FORWARD",
			"-i", externalInterface, "-d", subnet,
			"-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"); err != nil {
			errs = append(errs, fmt.Errorf("return forward rule: %w", err))
		}
	}

	return errors.Join(errs...)
}

// iptablesEnsure is the idempotent form of an iptables append: the rule is only
// added when an identical one is not already present. Failures carry the full
// command and its output (via runCmd) so callers can surface them.
func iptablesEnsure(table, chain string, rule ...string) error {
	prefix := make([]string, 0, 2)
	if table != "" {
		prefix = append(prefix, "-t", table)
	}

	checkArgs := append(append(append([]string{}, prefix...), "-C", chain), rule...)
	if err := runCmd("iptables", checkArgs...); err == nil {
		return nil
	}

	addArgs := append(append(append([]string{}, prefix...), "-A", chain), rule...)
	return runCmd("iptables", addArgs...)
}

func GetDefaultInterface() (string, error) {
	output, err := runCmdOutput("ip", "route", "show", "default")
	if err != nil {
		return "", fmt.Errorf("failed to get default route: %w", err)
	}

	fields := strings.Fields(output)
	for i, field := range fields {
		if field == "dev" && i+1 < len(fields) {
			return fields[i+1], nil
		}
	}

	return "", fmt.Errorf("could not determine default interface")
}
