package network

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"runtime"
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
	nsBridgeFmt    = "vnb%d"

	// transportSupernetCIDR is a dedicated link-local pool for the host<->netns
	// veth transport. Keeping it separate from the guest-facing supernet means
	// the guest /30 is never consumed by infrastructure addressing, and the
	// guest-visible gateway/guest pair (hostsPerSubnet indexed) stays intact.
	transportSupernetCIDR = "169.254.0.0/16"

	forwardSysctlPath = "/proc/sys/net/ipv4/ip_forward"
)

// InstanceNet is the host-side view of one isolated per-instance network:
// a named netns containing the inner end of a point-to-point veth pair, an
// internal bridge carrying the guest /30, and the guest TAP device.
type InstanceNet struct {
	NSName    string // handle name under /var/run/netns; Firecracker runs inside it
	TapName   string // created inside the netns (Firecracker SDK attaches it)
	VethHost  string // host-side interface of the veth pair
	HostIP    string // guest-visible gateway address (on the netns-internal bridge)
	GuestIP   string // static address passed to the guest kernel cmdline
	GuestMask string
}

// NetnsManager allocates per-instance /30 networks out of a supernet and
// manages their namespace + veth lifecycle. Identical guest configurations
// are safe across instances because each lives in its own namespace.
type NetnsManager struct {
	mu         sync.Mutex
	supernet   *net.IPNet
	transport  *net.IPNet
	total      int
	nextSubnet int
	freed      []int // reclaimed indexes, reused before advancing
}

func NewNetnsManager(supernetCIDR string) (*NetnsManager, error) {
	_, ipnet, err := net.ParseCIDR(supernetCIDR)
	if err != nil {
		return nil, fmt.Errorf("invalid netns supernet %q: %w", supernetCIDR, err)
	}
	ones, bits := ipnet.Mask.Size()
	if ones > 30 {
		return nil, fmt.Errorf("netns supernet %q leaves no room for /30 subnets", supernetCIDR)
	}
	_, transport, err := net.ParseCIDR(transportSupernetCIDR)
	if err != nil {
		return nil, fmt.Errorf("invalid built-in transport supernet %q: %w", transportSupernetCIDR, err)
	}
	total := int((uint64(1) << uint(bits-ones)) / hostsPerSubnet)
	return &NetnsManager{supernet: ipnet, transport: transport, total: total}, nil
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

// instanceAddressPlan is the pure addressing decision for one instance index:
// the guest /30 (gateway + guest address) and the separate transport /30
// (root-side + netns-side veth addresses).
type instanceAddressPlan struct {
	GuestBase     net.IP
	HostIP        string // guest-visible gateway, on the netns-internal bridge
	GuestIP       string // guest address
	TransportHost string // root-side veth address
	TransportNS   string // netns-side veth address
}

func planInstanceAddresses(supernet, transport *net.IPNet, idx int) (*instanceAddressPlan, error) {
	guestBase, err := subnetBase(supernet, idx)
	if err != nil {
		return nil, err
	}
	transportBase, err := subnetBase(transport, idx)
	if err != nil {
		return nil, fmt.Errorf("transport pool: %w", err)
	}
	return &instanceAddressPlan{
		GuestBase:     guestBase,
		HostIP:        addrOffset(guestBase, 1),
		GuestIP:       addrOffset(guestBase, 2),
		TransportHost: addrOffset(transportBase, 1),
		TransportNS:   addrOffset(transportBase, 2),
	}, nil
}

// indexInUse reports whether the interfaces belonging to the idx-th instance
// still exist. Orphaned interfaces from a killed worker are skipped so a
// restarted worker does not collide with them (LinkAdd would otherwise fail
// with EEXIST forever).
func indexInUse(idx int) bool {
	for _, name := range []string{fmt.Sprintf(hostVethFmt, idx), fmt.Sprintf(guestVethFmt, idx)} {
		if _, err := netlink.LinkByName(name); err == nil {
			return true
		}
	}
	return false
}

func (m *NetnsManager) reserve() (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for {
		var idx int
		if n := len(m.freed); n > 0 {
			idx = m.freed[n-1]
			m.freed = m.freed[:n-1]
		} else {
			if m.nextSubnet >= m.total {
				return 0, fmt.Errorf("netns pool exhausted: %d subnets", m.total)
			}
			idx = m.nextSubnet
			m.nextSubnet++
		}
		if indexInUse(idx) {
			continue
		}
		return idx, nil
	}
}

func (m *NetnsManager) release(idx int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.freed = append(m.freed, idx)
}

// vethIndex extracts N from a "vhN" host-veth name.
func vethIndex(name string) (int, bool) {
	if len(name) < 3 || name[:2] != "vh" {
		return 0, false
	}
	var idx int
	if n, err := fmt.Sscanf(name[2:], "%d", &idx); err != nil || n != 1 || idx < 0 {
		return 0, false
	}
	return idx, true
}

// enableForwardingInNamespace turns on net.ipv4.ip_forward inside ns. The
// sysctl is per-network-namespace, so the calling thread is temporarily moved
// into the namespace (and restored) while the procfs entry is written.
func enableForwardingInNamespace(ns netns.NsHandle) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	orig, err := netns.Get()
	if err != nil {
		return fmt.Errorf("read current network namespace: %w", err)
	}
	defer orig.Close()

	if err := netns.Set(ns); err != nil {
		return fmt.Errorf("enter instance network namespace: %w", err)
	}
	defer netns.Set(orig)

	if err := os.WriteFile(forwardSysctlPath, []byte("1"), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", forwardSysctlPath, err)
	}
	return nil
}

// Setup creates the namespace, the veth transport, the guest-facing bridge and
// the guest TAP device.
//
// Addressing for index i (all addresses are the "+1"/"+2" hosts of a /30):
//
//	root namespace:  vh<i> owns transportHost, route <guest /30> dev vh<i>
//	inside netns:    vg<i> owns transportNS, default route via transportHost,
//	                 bridge vnb<i> owns HostIP (= guest gateway), TAP attached.
//
// Host->guest traffic to GuestIP is routed by the root namespace into the
// netns, which forwards it out the internal bridge to the TAP. The guest's
// default route points at HostIP on that same bridge. Guest egress leaves via
// the netns default route, crosses the transport veth, and is NATed by the
// host (see BridgeManager.SetupNAT).
func (m *NetnsManager) Setup(instanceID string) (*InstanceNet, error) {
	idx, err := m.reserve()
	if err != nil {
		return nil, err
	}

	plan, err := planInstanceAddresses(m.supernet, m.transport, idx)
	if err != nil {
		m.release(idx)
		return nil, err
	}

	guestVethName := fmt.Sprintf(guestVethFmt, idx)
	bridgeName := fmt.Sprintf(nsBridgeFmt, idx)
	in := &InstanceNet{
		NSName:    nsPrefix + instanceID,
		TapName:   fmt.Sprintf(tapFmt, idx),
		VethHost:  fmt.Sprintf(hostVethFmt, idx),
		HostIP:    plan.HostIP,
		GuestIP:   plan.GuestIP,
		GuestMask: guestNetMask,
	}

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
	veth := &netlink.Veth{LinkAttrs: la, PeerName: guestVethName}
	if err := netlink.LinkAdd(veth); err != nil {
		return fail("create veth", err)
	}

	peer, err := netlink.LinkByName(guestVethName)
	if err != nil {
		return fail("find veth peer", err)
	}
	if err := netlink.LinkSetNsFd(peer, int(nsHandle)); err != nil {
		return fail("move veth peer into namespace", err)
	}

	if err := netlink.AddrAdd(veth, &netlink.Addr{IPNet: &net.IPNet{IP: net.ParseIP(plan.TransportHost), Mask: net.CIDRMask(30, 32)}}); err != nil {
		return fail("address host veth", err)
	}
	if err := netlink.LinkSetUp(veth); err != nil {
		return fail("raise host veth", err)
	}
	// Root-side route for the guest /30: send it down the veth so the netns
	// forwards it to the TAP. RouteReplace keeps retries idempotent.
	if err := netlink.RouteReplace(&netlink.Route{
		LinkIndex: veth.Attrs().Index,
		Dst:       &net.IPNet{IP: plan.GuestBase, Mask: net.CIDRMask(30, 32)},
	}); err != nil {
		return fail("route guest subnet via host veth", err)
	}

	nlh, err := netlink.NewHandleAt(nsHandle)
	if err != nil {
		return fail("open namespace handle", err)
	}
	defer nlh.Close()

	nsVeth, err := nlh.LinkByName(guestVethName)
	if err != nil {
		return fail("find peer inside namespace", err)
	}
	if err := nlh.AddrAdd(nsVeth, &netlink.Addr{IPNet: &net.IPNet{IP: net.ParseIP(plan.TransportNS), Mask: net.CIDRMask(30, 32)}}); err != nil {
		return fail("address peer inside namespace", err)
	}
	if err := nlh.LinkSetUp(nsVeth); err != nil {
		return fail("raise peer inside namespace", err)
	}
	if err := nlh.RouteReplace(&netlink.Route{LinkIndex: nsVeth.Attrs().Index, Gw: net.ParseIP(plan.TransportHost)}); err != nil {
		return fail("namespace default route", err)
	}

	brAttrs := netlink.NewLinkAttrs()
	brAttrs.Name = bridgeName
	nbr := &netlink.Bridge{LinkAttrs: brAttrs}
	if err := nlh.LinkAdd(nbr); err != nil {
		return fail("create namespace bridge", err)
	}
	if err := nlh.AddrAdd(nbr, &netlink.Addr{IPNet: &net.IPNet{IP: net.ParseIP(plan.HostIP), Mask: net.CIDRMask(30, 32)}}); err != nil {
		return fail("address namespace bridge", err)
	}
	if err := nlh.LinkSetUp(nbr); err != nil {
		return fail("raise namespace bridge", err)
	}

	tapAttrs := netlink.NewLinkAttrs()
	tapAttrs.Name = in.TapName
	tap := &netlink.Tuntap{LinkAttrs: tapAttrs, Mode: netlink.TUNTAP_MODE_TAP}
	if err := nlh.LinkAdd(tap); err != nil {
		return fail("create TAP inside namespace", err)
	}
	if err := nlh.LinkSetMaster(tap, nbr); err != nil {
		return fail("attach TAP to namespace bridge", err)
	}
	if err := nlh.LinkSetUp(tap); err != nil {
		return fail("raise TAP inside namespace", err)
	}

	if err := enableForwardingInNamespace(nsHandle); err != nil {
		return fail("enable forwarding inside namespace", err)
	}

	return in, nil
}

// Teardown deletes the veth (both ends) and the namespace; the bridge and TAP
// live inside the namespace so they are removed with it. Safe to call on
// partially-created networks.
func (m *NetnsManager) Teardown(in *InstanceNet) {
	if in == nil {
		return
	}
	if link, err := netlink.LinkByName(in.VethHost); err == nil {
		netlink.LinkDel(link)
	} else if idx, ok := vethIndex(in.VethHost); ok {
		// Partial setup: the host end may already be gone while the peer is
		// still in the root namespace.
		if peer, peerErr := netlink.LinkByName(fmt.Sprintf(guestVethFmt, idx)); peerErr == nil {
			netlink.LinkDel(peer)
		}
	}
	netns.DeleteNamed(in.NSName)
}
