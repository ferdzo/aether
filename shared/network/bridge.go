package network

import (
	"fmt"
	"net"
	"os/exec"
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

	// Gateway IP is network address + 1
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

func (bm *BridgeManager) bridgeExists() bool {
	output, err := runCmdOutput("ip", "link", "show", bm.BridgeName)
	return err == nil && strings.Contains(output, bm.BridgeName)
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

func (bm *BridgeManager) CreateTAPDevice(tapName string) (*TAPDevice, error) {
	output, err := runCmdOutput("ip", "link", "show", tapName)
	if err == nil && strings.Contains(output, tapName) {
		runCmd("ip", "link", "set", tapName, "up")
		return &TAPDevice{Name: tapName}, nil
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
	return runCmd("ip", "link", "delete", tapName)
}

func (bm *BridgeManager) AllocateVMIP() (string, error) {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	ip, err := allocateIP(bm.Subnet, bm.usedIPs)
	if err != nil {
		return "", err
	}

	bm.usedIPs[ip] = true
	return ip, nil
}

func (bm *BridgeManager) ReleaseVMIP(ip string) {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	delete(bm.usedIPs, ip)
}

func (bm *BridgeManager) GetGatewayIP() string {
	return bm.BridgeIP
}

func (bm *BridgeManager) NextTAPName() string {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	for i := 0; i < 256; i++ {
		tapName := fmt.Sprintf("tap%d", i)
		if _, err := runCmdOutput("ip", "link", "show", tapName); err != nil {
			return tapName
		}
	}
	return "tap0"
}

func (bm *BridgeManager) SetupNAT(externalInterface string) error {
	if err := runCmd("sysctl", "-w", "net.ipv4.ip_forward=1"); err != nil {
		return fmt.Errorf("failed to enable IP forwarding: %w", err)
	}

	runCmd("iptables", "-t", "nat", "-A", "POSTROUTING",
		"-s", bm.Subnet.String(), "-o", externalInterface, "-j", "MASQUERADE")

	runCmd("iptables", "-A", "FORWARD",
		"-i", bm.BridgeName, "-o", externalInterface, "-j", "ACCEPT")

	runCmd("iptables", "-A", "FORWARD",
		"-i", externalInterface, "-o", bm.BridgeName,
		"-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT")

	return nil
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

