package network

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

const (
	nsPrefix       = "aether-"
	guestNetMask   = "255.255.255.252" // /30 point-to-point per instance
	hostsPerSubnet = 4                 // .0 network | .1 host | .2 guest | .3 broadcast
	hostVethFmt    = "vh%d"
	guestVethFmt   = "vg%d"
	tapFmt         = "tapns%d"
)

// InstanceNet is the host-side view of one isolated per-instance network:
// a named netns containing the inner end of a point-to-point veth pair.
type InstanceNet struct {
	NSName    string // handle name under /var/run/netns; Firecracker runs inside it
	TapName   string // created by the Firecracker SDK inside the netns
	VethHost  string // host-side interface of the veth pair
	HostIP    string // gateway address on the host side of the pair
	GuestIP   string // static address passed to the guest kernel cmdline
	GuestMask string
}

// NetnsManager allocates per-instance /30 networks out of a supernet and
// manages their namespace + veth lifecycle. Identical guest configurations
// are safe across instances because each lives in its own namespace.
type NetnsManager struct {
	mu         sync.Mutex
	supernet   *net.IPNet
	nextSubnet int
	freed      []int // reclaimed indexes, reused before advancing
}

func NewNetnsManager(supernetCIDR string) (*NetnsManager, error) {
	_, ipnet, err := net.ParseCIDR(supernetCIDR)
	if err != nil {
		return nil, fmt.Errorf("invalid netns supernet %q: %w", supernetCIDR, err)
	}
	if ones, _ := ipnet.Mask.Size(); ones > 30 {
		return nil, fmt.Errorf("netns supernet %q leaves no room for /30 subnets", supernetCIDR)
	}
	return &NetnsManager{supernet: ipnet}, nil
}

// subnetBase returns the network address of the idx-th /30 within supernet.
func subnetBase(supernet *net.IPNet, idx int) (net.IP, error) {
	base := supernet.IP.To4()
	if base == nil {
		return nil, fmt.Errorf("supernet must be IPv4")
	}
	ones, bits := supernet.Mask.Size()
	total := uint64(1) << uint(bits-ones)
	maxSubnets := total / hostsPerSubnet
	if idx < 0 || uint64(idx) >= maxSubnets {
		return nil, fmt.Errorf("netns pool exhausted: subnet %d/%d", idx, maxSubnets)
	}
	v := uint64(binary.BigEndian.Uint32(base)) + uint64(idx)*hostsPerSubnet
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, uint32(v))
	return ip, nil
}

func addrOffset(base net.IP, off byte) string {
	ip := append(net.IP{}, base...)
	// /30 bases end in .0|.4|.8… so a ≤3 offset never carries into octet three
	ip[3] += off
	return ip.String()
}

func (m *NetnsManager) reserve() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n := len(m.freed); n > 0 {
		idx := m.freed[n-1]
		m.freed = m.freed[:n-1]
		return idx
	}
	idx := m.nextSubnet
	m.nextSubnet++
	return idx
}

func (m *NetnsManager) release(idx int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.freed = append(m.freed, idx)
}

// Setup creates the namespace, the veth pair, addressing and the guest
// default route. The TAP device itself is created by the Firecracker SDK
// inside NSName when the VM launches.
func (m *NetnsManager) Setup(instanceID string) (*InstanceNet, error) {
	idx := m.reserve()

	base, err := subnetBase(m.supernet, idx)
	if err != nil {
		m.release(idx)
		return nil, err
	}

	in := &InstanceNet{
		NSName:    nsPrefix + instanceID,
		TapName:   fmt.Sprintf(tapFmt, idx),
		VethHost:  fmt.Sprintf(hostVethFmt, idx),
		GuestIP:   addrOffset(base, 2),
		GuestMask: guestNetMask,
	}
	in.HostIP = addrOffset(base, 1)
	hostIP := net.ParseIP(in.HostIP)

	fail := func(step string, err error) (*InstanceNet, error) {
		m.Teardown(in)
		m.release(idx)
		return nil, fmt.Errorf("netns setup (%s): %w", step, err)
	}

	nsHandle, err := netns.NewNamed(in.NSName)
	if err != nil {
		return fail("create namespace", err)
	}
	defer nsHandle.Close()

	la := netlink.NewLinkAttrs()
	la.Name = in.VethHost
	veth := &netlink.Veth{LinkAttrs: la, PeerName: fmt.Sprintf(guestVethFmt, idx)}
	if err := netlink.LinkAdd(veth); err != nil {
		return fail("create veth", err)
	}

	peer, err := netlink.LinkByName(fmt.Sprintf(guestVethFmt, idx))
	if err != nil {
		return fail("find veth peer", err)
	}
	if err := netlink.LinkSetNsFd(peer, int(nsHandle)); err != nil {
		return fail("move veth peer into namespace", err)
	}

	if err := netlink.AddrAdd(veth, &netlink.Addr{IPNet: &net.IPNet{IP: hostIP, Mask: net.CIDRMask(30, 32)}}); err != nil {
		return fail("address host veth", err)
	}
	if err := netlink.LinkSetUp(veth); err != nil {
		return fail("raise host veth", err)
	}

	nlh, err := netlink.NewHandleAt(nsHandle)
	if err != nil {
		return fail("open namespace handle", err)
	}
	defer nlh.Close()

	guestLink, err := nlh.LinkByName(fmt.Sprintf(guestVethFmt, idx))
	if err != nil {
		return fail("find peer inside namespace", err)
	}
	if err := nlh.AddrAdd(guestLink, &netlink.Addr{IPNet: &net.IPNet{IP: net.ParseIP(in.GuestIP), Mask: net.CIDRMask(30, 32)}}); err != nil {
		return fail("address guest veth", err)
	}
	if err := nlh.LinkSetUp(guestLink); err != nil {
		return fail("raise guest veth", err)
	}
	if err := nlh.RouteAdd(&netlink.Route{LinkIndex: guestLink.Attrs().Index, Gw: hostIP}); err != nil {
		return fail("guest default route", err)
	}

	return in, nil
}

// Teardown deletes the veth (both ends) and the namespace. Safe to call on
// partially-created networks.
func (m *NetnsManager) Teardown(in *InstanceNet) {
	if in == nil {
		return
	}
	if link, err := netlink.LinkByName(in.VethHost); err == nil {
		netlink.LinkDel(link)
	}
	netns.DeleteNamed(in.NSName)
}
