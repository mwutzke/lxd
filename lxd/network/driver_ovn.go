package network

import (
	"fmt"
	"hash/fnv"
	"io"
	"io/ioutil"
	"math/big"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/mdlayher/netx/eui64"
	"github.com/pkg/errors"

	"github.com/lxc/lxd/lxd/cluster"
	"github.com/lxc/lxd/lxd/db"
	"github.com/lxc/lxd/lxd/dnsmasq"
	"github.com/lxc/lxd/lxd/locking"
	"github.com/lxc/lxd/lxd/network/openvswitch"
	"github.com/lxc/lxd/lxd/project"
	"github.com/lxc/lxd/lxd/revert"
	"github.com/lxc/lxd/lxd/util"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	log "github.com/lxc/lxd/shared/log15"
	"github.com/lxc/lxd/shared/validate"
)

// ovnGeneveTunnelMTU is the MTU that is safe to use when tunneling using geneve.
const ovnGeneveTunnelMTU = 1442

const ovnChassisPriorityMax = 32767
const ovnVolatileParentIPv4 = "volatile.parent.ipv4.address"
const ovnVolatileParentIPv6 = "volatile.parent.ipv6.address"

// ovnParentVars OVN object variables derived from parent network.
type ovnParentVars struct {
	// Router.
	routerExtPortIPv4Net string
	routerExtPortIPv6Net string
	routerExtGwIPv4      net.IP
	routerExtGwIPv6      net.IP

	// External Switch.
	extSwitchProviderName string

	// DNS.
	dnsIPv6 net.IP
	dnsIPv4 net.IP
}

// ovnParentPortBridgeVars parent bridge port variables used for start/stop.
type ovnParentPortBridgeVars struct {
	ovsBridge string
	parentEnd string
	ovsEnd    string
}

// ovn represents a LXD OVN network.
type ovn struct {
	common
}

// Validate network config.
func (n *ovn) Validate(config map[string]string) error {
	rules := map[string]func(value string) error{
		"parent": func(value string) error {
			if err := validInterfaceName(value); err != nil {
				return errors.Wrapf(err, "Invalid network name %q", value)
			}

			return nil
		},
		"bridge.hwaddr": validate.Optional(validate.IsNetworkMAC),
		"bridge.mtu":    validate.Optional(validate.IsNetworkMTU),
		"ipv4.address": func(value string) error {
			if validate.IsOneOf(value, []string{"auto"}) == nil {
				return nil
			}

			return validate.Optional(validate.IsNetworkAddressCIDRV4)(value)
		},
		"ipv6.address": func(value string) error {
			if validate.IsOneOf(value, []string{"auto"}) == nil {
				return nil
			}

			return validate.Optional(validate.IsNetworkAddressCIDRV6)(value)
		},
		"dns.domain": validate.IsAny,
		"dns.search": validate.IsAny,

		// Volatile keys populated automatically as needed.
		ovnVolatileParentIPv4: validate.Optional(validate.IsNetworkAddressV4),
		ovnVolatileParentIPv6: validate.Optional(validate.IsNetworkAddressV6),
	}

	err := n.validate(config, rules)
	if err != nil {
		return err
	}

	return nil
}

// getClient initialises OVN client and returns it.
func (n *ovn) getClient() (*openvswitch.OVN, error) {
	nbConnection, err := cluster.ConfigGetString(n.state.Cluster, "network.ovn.northbound_connection")
	if err != nil {
		return nil, errors.Wrapf(err, "Failed to get OVN northbound connection string")
	}

	client := openvswitch.NewOVN()
	client.SetDatabaseAddress(nbConnection)

	return client, nil
}

// getBridgeMTU returns MTU that should be used for the bridge and instance devices. Will also be used to configure
// the OVN DHCP and IPv6 RA options.
func (n *ovn) getBridgeMTU() uint32 {
	if n.config["bridge.mtu"] != "" {
		mtu, err := strconv.ParseUint(n.config["bridge.mtu"], 10, 32)
		if err != nil {
			return ovnGeneveTunnelMTU
		}

		return uint32(mtu)
	}

	return ovnGeneveTunnelMTU
}

// getNetworkPrefix returns OVN network prefix to use for object names.
func (n *ovn) getNetworkPrefix() string {
	return fmt.Sprintf("lxd-net%d", n.id)
}

// getChassisGroup returns OVN chassis group name to use.
func (n *ovn) getChassisGroupName() openvswitch.OVNChassisGroup {
	return openvswitch.OVNChassisGroup(n.getNetworkPrefix())
}

// getRouterName returns OVN logical router name to use.
func (n *ovn) getRouterName() openvswitch.OVNRouter {
	return openvswitch.OVNRouter(fmt.Sprintf("%s-lr", n.getNetworkPrefix()))
}

// getRouterExtPortName returns OVN logical router external port name to use.
func (n *ovn) getRouterExtPortName() openvswitch.OVNRouterPort {
	return openvswitch.OVNRouterPort(fmt.Sprintf("%s-lrp-ext", n.getRouterName()))
}

// getRouterIntPortName returns OVN logical router internal port name to use.
func (n *ovn) getRouterIntPortName() openvswitch.OVNRouterPort {
	return openvswitch.OVNRouterPort(fmt.Sprintf("%s-lrp-int", n.getRouterName()))
}

// getRouterMAC returns OVN router MAC address to use for ports. Uses a stable seed to return stable random MAC.
func (n *ovn) getRouterMAC() (net.HardwareAddr, error) {
	hwAddr := n.config["bridge.hwaddr"]
	if hwAddr == "" {
		// Load server certificate. This is needs to be the same certificate for all nodes in a cluster.
		cert, err := util.LoadCert(n.state.OS.VarDir)
		if err != nil {
			return nil, err
		}

		// Generate the random seed, this uses the server certificate fingerprint (to ensure that multiple
		// standalone nodes on the same external network don't generate the same MAC for their networks).
		// It relies on the certificate being the same for all nodes in a cluster to allow the same MAC to
		// be generated on each bridge interface in the network.
		seed := fmt.Sprintf("%s.%d.%d", cert.Fingerprint(), 0, n.ID())

		// Generate a hash from the randSourceNodeID and network ID to use as seed for random MAC.
		// Use the FNV-1a hash algorithm to convert our seed string into an int64 for use as seed.
		hash := fnv.New64a()
		_, err = io.WriteString(hash, seed)
		if err != nil {
			return nil, err
		}

		// Initialise a non-cryptographic random number generator using the stable seed.
		r := rand.New(rand.NewSource(int64(hash.Sum64())))
		hwAddr = randomHwaddr(r)
		n.logger.Debug("Stable MAC generated", log.Ctx{"seed": seed, "hwAddr": hwAddr})
	}

	mac, err := net.ParseMAC(hwAddr)
	if err != nil {
		return nil, errors.Wrapf(err, "Failed parsing router MAC address %q", mac)
	}

	return mac, nil
}

// getRouterIntPortIPv4Net returns OVN logical router internal port IPv4 address and subnet.
func (n *ovn) getRouterIntPortIPv4Net() string {
	return n.config["ipv4.address"]
}

// getRouterIntPortIPv4Net returns OVN logical router internal port IPv6 address and subnet.
func (n *ovn) getRouterIntPortIPv6Net() string {
	return n.config["ipv6.address"]
}

// getDomainName returns OVN DHCP domain name.
func (n *ovn) getDomainName() string {
	if n.config["dns.domain"] != "" {
		return n.config["dns.domain"]
	}

	return "lxd"
}

// getDNSSearchList returns OVN DHCP DNS search list. If no search list set returns getDomainName() as list.
func (n *ovn) getDNSSearchList() []string {
	if n.config["dns.search"] != "" {
		dnsSearchList := []string{}
		for _, domain := range strings.SplitN(n.config["dns.search"], ",", -1) {
			dnsSearchList = append(dnsSearchList, strings.TrimSpace(domain))
		}

		return dnsSearchList
	}

	return []string{n.getDomainName()}
}

// getExtSwitchName returns OVN  logical external switch name.
func (n *ovn) getExtSwitchName() openvswitch.OVNSwitch {
	return openvswitch.OVNSwitch(fmt.Sprintf("%s-ls-ext", n.getNetworkPrefix()))
}

// getExtSwitchRouterPortName returns OVN logical external switch router port name.
func (n *ovn) getExtSwitchRouterPortName() openvswitch.OVNSwitchPort {
	return openvswitch.OVNSwitchPort(fmt.Sprintf("%s-lsp-router", n.getExtSwitchName()))
}

// getExtSwitchProviderPortName returns OVN logical external switch provider port name.
func (n *ovn) getExtSwitchProviderPortName() openvswitch.OVNSwitchPort {
	return openvswitch.OVNSwitchPort(fmt.Sprintf("%s-lsp-provider", n.getExtSwitchName()))
}

// getIntSwitchName returns OVN logical internal switch name.
func (n *ovn) getIntSwitchName() openvswitch.OVNSwitch {
	return openvswitch.OVNSwitch(fmt.Sprintf("%s-ls-int", n.getNetworkPrefix()))
}

// getIntSwitchRouterPortName returns OVN logical internal switch router port name.
func (n *ovn) getIntSwitchRouterPortName() openvswitch.OVNSwitchPort {
	return openvswitch.OVNSwitchPort(fmt.Sprintf("%s-lsp-router", n.getIntSwitchName()))
}

// getIntSwitchInstancePortPrefix returns OVN logical internal switch instance port name prefix.
func (n *ovn) getIntSwitchInstancePortPrefix() string {
	return fmt.Sprintf("%s-instance", n.getNetworkPrefix())
}

// setupParentPort initialises the parent uplink connection. Returns the derived ovnParentVars settings used
// during the initial creation of the logical network.
func (n *ovn) setupParentPort(routerMAC net.HardwareAddr) (*ovnParentVars, error) {
	parentNet, err := LoadByName(n.state, n.config["parent"])
	if err != nil {
		return nil, errors.Wrapf(err, "Failed loading parent network")
	}

	switch parentNet.Type() {
	case "bridge":
		return n.setupParentPortBridge(parentNet, routerMAC)
	}

	return nil, fmt.Errorf("Network type %q unsupported as OVN parent", parentNet.Type())
}

// setupParentPortBridge allocates external IPs on the parent bridge.
// Returns the derived ovnParentVars settings.
func (n *ovn) setupParentPortBridge(parentNet Network, routerMAC net.HardwareAddr) (*ovnParentVars, error) {
	v := &ovnParentVars{}

	bridgeNet, ok := parentNet.(*bridge)
	if !ok {
		return nil, fmt.Errorf("Network is not bridge type")
	}

	err := bridgeNet.checkClusterWideMACSafe(bridgeNet.config)
	if err != nil {
		return nil, errors.Wrapf(err, "Network %q is not suitable for use as OVN parent", bridgeNet.name)
	}

	parentNetConf := parentNet.Config()

	// Parent derived settings.
	v.extSwitchProviderName = parentNet.Name()

	// Optional parent values.
	parentIPv4, parentIPv4Net, err := net.ParseCIDR(parentNetConf["ipv4.address"])
	if err == nil {
		v.dnsIPv4 = parentIPv4
		v.routerExtGwIPv4 = parentIPv4
	}

	parentIPv6, parentIPv6Net, err := net.ParseCIDR(parentNetConf["ipv6.address"])
	if err == nil {
		v.dnsIPv6 = parentIPv6
		v.routerExtGwIPv6 = parentIPv6
	}

	// Parse existing allocated IPs for this network on the parent network (if not set yet, will be nil).
	routerExtPortIPv4 := net.ParseIP(n.config[ovnVolatileParentIPv4])
	routerExtPortIPv6 := net.ParseIP(n.config[ovnVolatileParentIPv6])

	// Decide whether we need to allocate new IP(s) and go to the expense of retrieving all allocated IPs.
	if (parentIPv4Net != nil && routerExtPortIPv4 == nil) || (parentIPv6Net != nil && routerExtPortIPv6 == nil) {
		err := n.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
			allAllocatedIPv4, allAllocatedIPv6, err := n.parentAllAllocatedIPs(tx, parentNet.Name())
			if err != nil {
				return errors.Wrapf(err, "Failed to get all allocated IPs for parent")
			}

			if parentIPv4Net != nil && routerExtPortIPv4 == nil {
				if parentNetConf["ipv4.ovn.ranges"] == "" {
					return fmt.Errorf(`Missing required "ipv4.ovn.ranges" config key on parent network`)
				}

				ipRanges, err := parseIPRanges(parentNetConf["ipv4.ovn.ranges"], parentNet.DHCPv4Subnet())
				if err != nil {
					return errors.Wrapf(err, "Failed to parse parent IPv4 OVN ranges")
				}

				routerExtPortIPv4, err = n.parentAllocateIP(ipRanges, allAllocatedIPv4)
				if err != nil {
					return errors.Wrapf(err, "Failed to allocate parent IPv4 address")
				}

				n.config[ovnVolatileParentIPv4] = routerExtPortIPv4.String()
			}

			if parentIPv6Net != nil && routerExtPortIPv6 == nil {
				// If IPv6 OVN ranges are specified by the parent, allocate from them.
				if parentNetConf["ipv6.ovn.ranges"] != "" {
					ipRanges, err := parseIPRanges(parentNetConf["ipv6.ovn.ranges"], parentNet.DHCPv6Subnet())
					if err != nil {
						return errors.Wrapf(err, "Failed to parse parent IPv6 OVN ranges")
					}

					routerExtPortIPv6, err = n.parentAllocateIP(ipRanges, allAllocatedIPv6)
					if err != nil {
						return errors.Wrapf(err, "Failed to allocate parent IPv6 address")
					}

				} else {
					// Otherwise use EUI64 derived from MAC address.
					routerExtPortIPv6, err = eui64.ParseMAC(parentIPv6Net.IP, routerMAC)
					if err != nil {
						return err
					}
				}

				n.config[ovnVolatileParentIPv6] = routerExtPortIPv6.String()
			}

			networkID, err := tx.GetNetworkID(n.name)
			if err != nil {
				return errors.Wrapf(err, "Failed to get network ID for network %q", n.name)
			}

			err = tx.UpdateNetwork(networkID, n.description, n.config)
			if err != nil {
				return errors.Wrapf(err, "Failed saving allocated parent network IPs")
			}

			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	// Configure variables needed to configure OVN router.
	if parentIPv4Net != nil && routerExtPortIPv4 != nil {
		routerExtPortIPv4Net := &net.IPNet{
			Mask: parentIPv4Net.Mask,
			IP:   routerExtPortIPv4,
		}
		v.routerExtPortIPv4Net = routerExtPortIPv4Net.String()
	}

	if parentIPv6Net != nil {
		routerExtPortIPv6Net := &net.IPNet{
			Mask: parentIPv6Net.Mask,
			IP:   routerExtPortIPv6,
		}
		v.routerExtPortIPv6Net = routerExtPortIPv6Net.String()
	}

	return v, nil
}

// parentAllAllocatedIPs gets a list of all IPv4 and IPv6 addresses allocated to OVN networks connected to parent.
func (n *ovn) parentAllAllocatedIPs(tx *db.ClusterTx, parentNetName string) ([]net.IP, []net.IP, error) {
	// Get all managed networks.
	networks, err := tx.GetNonPendingNetworks()
	if err != nil {
		return nil, nil, err
	}

	v4IPs := make([]net.IP, 0)
	v6IPs := make([]net.IP, 0)

	for _, netInfo := range networks {
		if netInfo.Type != "ovn" || netInfo.Config["parent"] != parentNetName {
			continue
		}

		for _, k := range []string{ovnVolatileParentIPv4, ovnVolatileParentIPv6} {
			if netInfo.Config[k] != "" {
				ip := net.ParseIP(netInfo.Config[k])
				if ip != nil {
					if ip.To4() != nil {
						v4IPs = append(v4IPs, ip)
					} else {
						v6IPs = append(v6IPs, ip)
					}
				}
			}
		}
	}

	return v4IPs, v6IPs, nil
}

// parentAllocateIP allocates a free IP from one of the IP ranges.
func (n *ovn) parentAllocateIP(ipRanges []*shared.IPRange, allAllocated []net.IP) (net.IP, error) {
	for _, ipRange := range ipRanges {
		inc := big.NewInt(1)

		// Convert IPs in range to native representations to allow incrementing and comparison.
		startIP := ipRange.Start.To4()
		if startIP == nil {
			startIP = ipRange.Start.To16()
		}

		endIP := ipRange.End.To4()
		if endIP == nil {
			endIP = ipRange.End.To16()
		}

		startBig := big.NewInt(0)
		startBig.SetBytes(startIP)
		endBig := big.NewInt(0)
		endBig.SetBytes(endIP)

		// Iterate through IPs in range, return the first unallocated one found.
		for {
			if startBig.Cmp(endBig) > 0 {
				break
			}

			ip := net.IP(startBig.Bytes())

			// Check IP is not already allocated.
			freeIP := true
			for _, allocatedIP := range allAllocated {
				if ip.Equal(allocatedIP) {
					freeIP = false
					break
				}

			}

			if !freeIP {
				startBig.Add(startBig, inc)
				continue
			}

			return ip, nil
		}
	}

	return nil, fmt.Errorf("No free IPs available")
}

// startParentPort performs any network start up logic needed to connect the parent uplink connection to OVN.
func (n *ovn) startParentPort() error {
	parentNet, err := LoadByName(n.state, n.config["parent"])
	if err != nil {
		return errors.Wrapf(err, "Failed loading parent network")
	}

	switch parentNet.Type() {
	case "bridge":
		return n.startParentPortBridge(parentNet)
	}

	return fmt.Errorf("Network type %q unsupported as OVN parent", parentNet.Type())
}

// parentOperationLockName returns the lock name to use for operations on the parent network.
func (n *ovn) parentOperationLockName(parentNet Network) string {
	return fmt.Sprintf("network.ovn.%s", parentNet.Name())
}

// parentPortBridgeVars returns the parent port bridge variables needed for port start/stop.
func (n *ovn) parentPortBridgeVars(parentNet Network) *ovnParentPortBridgeVars {
	ovsBridge := fmt.Sprintf("lxdovn%d", parentNet.ID())

	return &ovnParentPortBridgeVars{
		ovsBridge: ovsBridge,
		parentEnd: fmt.Sprintf("%sa", ovsBridge),
		ovsEnd:    fmt.Sprintf("%sb", ovsBridge),
	}
}

// startParentPortBridge creates veth pair (if doesn't exist), creates OVS bridge (if doesn't exist) and
// connects veth pair to parent bridge and OVS bridge.
func (n *ovn) startParentPortBridge(parentNet Network) error {
	vars := n.parentPortBridgeVars(parentNet)

	// Lock parent network so that if multiple OVN networks are trying to connect to the same parent we don't
	// race each other setting up the connection.
	unlock := locking.Lock(n.parentOperationLockName(parentNet))
	defer unlock()

	// Do this after gaining lock so that on failure we revert before release locking.
	revert := revert.New()
	defer revert.Fail()

	// Create veth pair if needed.
	if !shared.PathExists(fmt.Sprintf("/sys/class/net/%s", vars.parentEnd)) && !shared.PathExists(fmt.Sprintf("/sys/class/net/%s", vars.ovsEnd)) {
		_, err := shared.RunCommand("ip", "link", "add", "dev", vars.parentEnd, "type", "veth", "peer", "name", vars.ovsEnd)
		if err != nil {
			return errors.Wrapf(err, "Failed to create the uplink veth interfaces %q and %q", vars.parentEnd, vars.ovsEnd)
		}

		revert.Add(func() { shared.RunCommand("ip", "link", "delete", vars.parentEnd) })
	}

	// Ensure correct sysctls are set on uplink veth interfaces to avoid getting IPv6 link-local addresses.
	_, err := shared.RunCommand("sysctl",
		fmt.Sprintf("net.ipv6.conf.%s.disable_ipv6=1", vars.parentEnd),
		fmt.Sprintf("net.ipv6.conf.%s.disable_ipv6=1", vars.ovsEnd),
		fmt.Sprintf("net.ipv6.conf.%s.forwarding=0", vars.parentEnd),
		fmt.Sprintf("net.ipv6.conf.%s.forwarding=0", vars.ovsEnd),
	)
	if err != nil {
		return errors.Wrapf(err, "Failed to set configure uplink veth interfaces %q and %q", vars.parentEnd, vars.ovsEnd)
	}

	// Connect parent end of veth pair to parent bridge and bring up.
	_, err = shared.RunCommand("ip", "link", "set", "master", parentNet.Name(), "dev", vars.parentEnd, "up")
	if err != nil {
		return errors.Wrapf(err, "Failed to connect uplink veth interface %q to parent bridge %q", vars.parentEnd, parentNet.Name())
	}

	// Ensure uplink OVS end veth interface is up.
	_, err = shared.RunCommand("ip", "link", "set", "dev", vars.ovsEnd, "up")
	if err != nil {
		return errors.Wrapf(err, "Failed to bring up parent veth interface %q", vars.ovsEnd)
	}

	// Create parent OVS bridge if needed.
	ovs := openvswitch.NewOVS()
	err = ovs.BridgeAdd(vars.ovsBridge, true)
	if err != nil {
		return errors.Wrapf(err, "Failed to create parent uplink OVS bridge %q", vars.ovsBridge)
	}

	// Connect OVS end veth interface to OVS bridge.
	err = ovs.BridgePortAdd(vars.ovsBridge, vars.ovsEnd, true)
	if err != nil {
		return errors.Wrapf(err, "Failed to connect uplink veth interface %q to parent OVS bridge %q", vars.ovsEnd, vars.ovsBridge)
	}

	// Associate OVS bridge to logical OVN provider.
	err = ovs.OVNBridgeMappingAdd(vars.ovsBridge, parentNet.Name())
	if err != nil {
		return errors.Wrapf(err, "Failed to associate parent OVS bridge %q to OVN provider %q", vars.ovsBridge, parentNet.Name())
	}

	revert.Success()
	return nil
}

// deleteParentPort deletes the parent uplink connection.
func (n *ovn) deleteParentPort() error {
	parentNet, err := LoadByName(n.state, n.config["parent"])
	if err != nil {
		return errors.Wrapf(err, "Failed loading parent network")
	}

	switch parentNet.Type() {
	case "bridge":
		return n.deleteParentPortBridge(parentNet)
	}

	return fmt.Errorf("Network type %q unsupported as OVN parent", parentNet.Type())
}

// deleteParentPortBridge deletes the dnsmasq static lease and removes parent uplink OVS bridge if not in use.
func (n *ovn) deleteParentPortBridge(parentNet Network) error {
	err := dnsmasq.RemoveStaticEntry(parentNet.Name(), project.Default, n.getNetworkPrefix())
	if err != nil {
		return err
	}

	// Reload dnsmasq.
	err = dnsmasq.Kill(parentNet.Name(), true)
	if err != nil {
		return err
	}

	// Lock parent network so we don;t race each other networks using the OVS uplink bridge.
	unlock := locking.Lock(n.parentOperationLockName(parentNet))
	defer unlock()

	// Check OVS uplink bridge exists, if it does, check how many ports it has.
	removeVeths := false
	vars := n.parentPortBridgeVars(parentNet)
	if shared.PathExists(fmt.Sprintf("/sys/class/net/%s", vars.ovsBridge)) {
		ovs := openvswitch.NewOVS()
		ports, err := ovs.BridgePortList(vars.ovsBridge)
		if err != nil {
			return err
		}

		// If the OVS bridge has only 1 port (the OVS veth end) or fewer connected then we can delete it.
		if len(ports) <= 1 {
			removeVeths = true

			err = ovs.OVNBridgeMappingDelete(vars.ovsBridge, parentNet.Name())
			if err != nil {
				return err
			}

			err = ovs.BridgeDelete(vars.ovsBridge)
			if err != nil {
				return err
			}
		}
	} else {
		removeVeths = true // Remove the veths if OVS bridge already gone.
	}

	// Remove the veth interfaces if they exist.
	if removeVeths {
		if shared.PathExists(fmt.Sprintf("/sys/class/net/%s", vars.parentEnd)) {
			_, err := shared.RunCommand("ip", "link", "delete", "dev", vars.parentEnd)
			if err != nil {
				return errors.Wrapf(err, "Failed to delete the uplink veth interface %q", vars.parentEnd)
			}
		}

		if shared.PathExists(fmt.Sprintf("/sys/class/net/%s", vars.ovsEnd)) {
			_, err := shared.RunCommand("ip", "link", "delete", "dev", vars.ovsEnd)
			if err != nil {
				return errors.Wrapf(err, "Failed to delete the uplink veth interface %q", vars.ovsEnd)
			}
		}
	}

	return nil
}

// fillConfig fills requested config with any default values.
func (n *ovn) fillConfig(config map[string]string) error {
	if config["ipv4.address"] == "" {
		config["ipv4.address"] = "auto"
	}

	if config["ipv6.address"] == "" {
		content, err := ioutil.ReadFile("/proc/sys/net/ipv6/conf/default/disable_ipv6")
		if err == nil && string(content) == "0\n" {
			config["ipv6.address"] = "auto"
		}
	}

	// Now populate "auto" values where needed.
	if config["ipv4.address"] == "auto" {
		subnet, err := randomSubnetV4()
		if err != nil {
			return err
		}

		config["ipv4.address"] = subnet
	}

	if config["ipv6.address"] == "auto" {
		subnet, err := randomSubnetV6()
		if err != nil {
			return err
		}

		config["ipv6.address"] = subnet
	}

	return nil
}

// Create sets up network in OVN Northbound database.
func (n *ovn) Create(clusterNotification bool) error {
	n.logger.Debug("Create", log.Ctx{"clusterNotification": clusterNotification, "config": n.config})

	// We only need to setup the OVN Northbound database once, not on every clustered node.
	if !clusterNotification {
		err := n.setup(false)
		if err != nil {
			return err
		}
	}

	return nil
}

func (n *ovn) setup(update bool) error {
	// If we are in mock mode, just no-op.
	if n.state.OS.MockMode {
		return nil
	}

	n.logger.Debug("Setting up network")

	revert := revert.New()
	defer revert.Fail()

	client, err := n.getClient()
	if err != nil {
		return err
	}

	var routerExtPortIPv4, routerIntPortIPv4, routerExtPortIPv6, routerIntPortIPv6 net.IP
	var routerExtPortIPv4Net, routerIntPortIPv4Net, routerExtPortIPv6Net, routerIntPortIPv6Net *net.IPNet

	// Get router MAC address.
	routerMAC, err := n.getRouterMAC()
	if err != nil {
		return err
	}

	// Setup parent port (do this first to check parent is suitable).
	parent, err := n.setupParentPort(routerMAC)
	if err != nil {
		return err
	}

	// Parse router IP config.
	if parent.routerExtPortIPv4Net != "" {
		routerExtPortIPv4, routerExtPortIPv4Net, err = net.ParseCIDR(parent.routerExtPortIPv4Net)
		if err != nil {
			return errors.Wrapf(err, "Failed parsing router's external parent port IPv4 Net")
		}
	}

	if parent.routerExtPortIPv6Net != "" {
		routerExtPortIPv6, routerExtPortIPv6Net, err = net.ParseCIDR(parent.routerExtPortIPv6Net)
		if err != nil {
			return errors.Wrapf(err, "Failed parsing router's external parent port IPv6 Net")
		}
	}

	if n.getRouterIntPortIPv4Net() != "" {
		routerIntPortIPv4, routerIntPortIPv4Net, err = net.ParseCIDR(n.getRouterIntPortIPv4Net())
		if err != nil {
			return errors.Wrapf(err, "Failed parsing router's internal port IPv4 Net")
		}
	}

	if n.getRouterIntPortIPv6Net() != "" {
		routerIntPortIPv6, routerIntPortIPv6Net, err = net.ParseCIDR(n.getRouterIntPortIPv6Net())
		if err != nil {
			return errors.Wrapf(err, "Failed parsing router's internal port IPv6 Net")
		}
	}

	// Create chassis group.
	err = client.ChassisGroupAdd(n.getChassisGroupName(), update)
	if err != nil {
		return err
	}

	revert.Add(func() { client.ChassisGroupDelete(n.getChassisGroupName()) })

	// Add local chassis to chassis group.
	ovs := openvswitch.NewOVS()
	chassisID, err := ovs.ChassisID()
	if err != nil {
		return errors.Wrapf(err, "Failed getting OVS Chassis ID")
	}

	var priority uint = ovnChassisPriorityMax
	err = client.ChassisGroupChassisAdd(n.getChassisGroupName(), chassisID, priority)
	if err != nil {
		return errors.Wrapf(err, "Failed adding OVS chassis %q with priority %d to chassis group %q", chassisID, priority, n.getChassisGroupName())
	}

	// Create logical router.
	if update {
		client.LogicalRouterDelete(n.getRouterName())
	}

	err = client.LogicalRouterAdd(n.getRouterName())
	if err != nil {
		return errors.Wrapf(err, "Failed adding router")
	}
	revert.Add(func() { client.LogicalRouterDelete(n.getRouterName()) })

	// Configure logical router.

	// Add default routes.
	if parent.routerExtGwIPv4 != nil {
		err = client.LogicalRouterRouteAdd(n.getRouterName(), &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}, parent.routerExtGwIPv4)
		if err != nil {
			return errors.Wrapf(err, "Failed adding IPv4 default route")
		}
	}

	if parent.routerExtGwIPv6 != nil {
		err = client.LogicalRouterRouteAdd(n.getRouterName(), &net.IPNet{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)}, parent.routerExtGwIPv6)
		if err != nil {
			return errors.Wrapf(err, "Failed adding IPv6 default route")
		}
	}

	// Add SNAT rules.
	if routerIntPortIPv4Net != nil {
		err = client.LogicalRouterSNATAdd(n.getRouterName(), routerIntPortIPv4Net, routerExtPortIPv4)
		if err != nil {
			return err
		}
	}

	if routerIntPortIPv6Net != nil {
		err = client.LogicalRouterSNATAdd(n.getRouterName(), routerIntPortIPv6Net, routerExtPortIPv6)
		if err != nil {
			return err
		}
	}

	// Create external logical switch.
	if update {
		client.LogicalSwitchDelete(n.getExtSwitchName())
	}

	err = client.LogicalSwitchAdd(n.getExtSwitchName(), false)
	if err != nil {
		return errors.Wrapf(err, "Failed adding external switch")
	}
	revert.Add(func() { client.LogicalSwitchDelete(n.getExtSwitchName()) })

	// Generate external router port IPs (in CIDR format).
	extRouterIPs := []*net.IPNet{}
	if routerExtPortIPv4Net != nil {
		extRouterIPs = append(extRouterIPs, &net.IPNet{
			IP:   routerExtPortIPv4,
			Mask: routerExtPortIPv4Net.Mask,
		})
	}

	if routerExtPortIPv6Net != nil {
		extRouterIPs = append(extRouterIPs, &net.IPNet{
			IP:   routerExtPortIPv6,
			Mask: routerExtPortIPv6Net.Mask,
		})
	}

	// Create external router port.
	err = client.LogicalRouterPortAdd(n.getRouterName(), n.getRouterExtPortName(), routerMAC, extRouterIPs...)
	if err != nil {
		return errors.Wrapf(err, "Failed adding external router port")
	}
	revert.Add(func() { client.LogicalRouterPortDelete(n.getRouterExtPortName()) })

	// Associate external router port to chassis group.
	err = client.LogicalRouterPortLinkChassisGroup(n.getRouterExtPortName(), n.getChassisGroupName())
	if err != nil {
		return errors.Wrapf(err, "Failed linking external router port to chassis group")
	}

	// Create external switch port and link to router port.
	err = client.LogicalSwitchPortAdd(n.getExtSwitchName(), n.getExtSwitchRouterPortName(), false)
	if err != nil {
		return errors.Wrapf(err, "Failed adding external switch router port")
	}
	revert.Add(func() { client.LogicalSwitchPortDelete(n.getExtSwitchRouterPortName()) })

	err = client.LogicalSwitchPortLinkRouter(n.getExtSwitchRouterPortName(), n.getRouterExtPortName())
	if err != nil {
		return errors.Wrapf(err, "Failed linking external router port to external switch port")
	}

	// Create external switch port and link to external provider network.
	err = client.LogicalSwitchPortAdd(n.getExtSwitchName(), n.getExtSwitchProviderPortName(), false)
	if err != nil {
		return errors.Wrapf(err, "Failed adding external switch provider port")
	}
	revert.Add(func() { client.LogicalSwitchPortDelete(n.getExtSwitchProviderPortName()) })

	err = client.LogicalSwitchPortLinkProviderNetwork(n.getExtSwitchProviderPortName(), parent.extSwitchProviderName)
	if err != nil {
		return errors.Wrapf(err, "Failed linking external switch provider port to external provider network")
	}

	// Create internal logical switch if not updating.
	err = client.LogicalSwitchAdd(n.getIntSwitchName(), update)
	if err != nil {
		return errors.Wrapf(err, "Failed adding internal switch")
	}
	revert.Add(func() { client.LogicalSwitchDelete(n.getIntSwitchName()) })

	// Setup IP allocation config on logical switch.
	err = client.LogicalSwitchSetIPAllocation(n.getIntSwitchName(), &openvswitch.OVNIPAllocationOpts{
		PrefixIPv4:  routerIntPortIPv4Net,
		PrefixIPv6:  routerIntPortIPv6Net,
		ExcludeIPv4: []shared.IPRange{{Start: routerIntPortIPv4}},
	})
	if err != nil {
		return errors.Wrapf(err, "Failed setting IP allocation settings on internal switch")
	}

	// Find MAC address for internal router port.

	var dhcpv4UUID, dhcpv6UUID string

	if update {
		// Find first existing DHCP options set for IPv4 and IPv6 and update them instead of adding sets.
		existingOpts, err := client.LogicalSwitchDHCPOptionsGet(n.getIntSwitchName())
		if err != nil {
			return errors.Wrapf(err, "Failed getting existing DHCP settings for internal switch")
		}

		for _, existingOpt := range existingOpts {
			if existingOpt.CIDR.IP.To4() == nil {
				if dhcpv6UUID == "" {
					dhcpv6UUID = existingOpt.UUID
				}
			} else {
				if dhcpv4UUID == "" {
					dhcpv4UUID = existingOpt.UUID
				}
			}
		}
	}

	// Create DHCPv4 options for internal switch.
	err = client.LogicalSwitchDHCPv4OptionsSet(n.getIntSwitchName(), dhcpv4UUID, routerIntPortIPv4Net, &openvswitch.OVNDHCPv4Opts{
		ServerID:           routerIntPortIPv4,
		ServerMAC:          routerMAC,
		Router:             routerIntPortIPv4,
		RecursiveDNSServer: parent.dnsIPv4,
		DomainName:         n.getDomainName(),
		LeaseTime:          time.Duration(time.Hour * 1),
		MTU:                n.getBridgeMTU(),
	})
	if err != nil {
		return errors.Wrapf(err, "Failed adding DHCPv4 settings for internal switch")
	}

	// Create DHCPv6 options for internal switch.
	err = client.LogicalSwitchDHCPv6OptionsSet(n.getIntSwitchName(), dhcpv6UUID, routerIntPortIPv6Net, &openvswitch.OVNDHCPv6Opts{
		ServerID:           routerMAC,
		RecursiveDNSServer: parent.dnsIPv6,
		DNSSearchList:      n.getDNSSearchList(),
	})
	if err != nil {
		return errors.Wrapf(err, "Failed adding DHCPv6 settings for internal switch")
	}

	// Generate internal router port IPs (in CIDR format).
	intRouterIPs := []*net.IPNet{}
	if routerIntPortIPv4Net != nil {
		intRouterIPs = append(intRouterIPs, &net.IPNet{
			IP:   routerIntPortIPv4,
			Mask: routerIntPortIPv4Net.Mask,
		})
	}

	if routerIntPortIPv6Net != nil {
		intRouterIPs = append(intRouterIPs, &net.IPNet{
			IP:   routerIntPortIPv6,
			Mask: routerIntPortIPv6Net.Mask,
		})
	}

	// Create internal router port.
	err = client.LogicalRouterPortAdd(n.getRouterName(), n.getRouterIntPortName(), routerMAC, intRouterIPs...)
	if err != nil {
		return errors.Wrapf(err, "Failed adding internal router port")
	}
	revert.Add(func() { client.LogicalRouterPortDelete(n.getRouterIntPortName()) })

	// Set IPv6 router advertisement settings.
	if routerIntPortIPv6Net != nil {
		err = client.LogicalRouterPortSetIPv6Advertisements(n.getRouterIntPortName(), &openvswitch.OVNIPv6RAOpts{
			AddressMode:        openvswitch.OVNIPv6AddressModeSLAAC,
			SendPeriodic:       true,
			DNSSearchList:      n.getDNSSearchList(),
			RecursiveDNSServer: parent.dnsIPv6,
			MTU:                n.getBridgeMTU(),

			// Keep these low until we support DNS search domains via DHCPv4, as otherwise RA DNSSL
			// won't take effect until advert after DHCPv4 has run on instance.
			MinInterval: time.Duration(time.Second * 30),
			MaxInterval: time.Duration(time.Minute * 1),
		})
		if err != nil {
			return errors.Wrapf(err, "Failed setting internal router port IPv6 advertisement settings")
		}
	}

	// Create internal switch port and link to router port.
	err = client.LogicalSwitchPortAdd(n.getIntSwitchName(), n.getIntSwitchRouterPortName(), update)
	if err != nil {
		return errors.Wrapf(err, "Failed adding internal switch router port")
	}
	revert.Add(func() { client.LogicalSwitchPortDelete(n.getIntSwitchRouterPortName()) })

	err = client.LogicalSwitchPortLinkRouter(n.getIntSwitchRouterPortName(), n.getRouterIntPortName())
	if err != nil {
		return errors.Wrapf(err, "Failed linking internal router port to internal switch port")
	}

	revert.Success()
	return nil
}

// Delete deletes a network.
func (n *ovn) Delete(clusterNotification bool) error {
	n.logger.Debug("Delete", log.Ctx{"clusterNotification": clusterNotification})

	if !clusterNotification {
		client, err := n.getClient()
		if err != nil {
			return err
		}

		err = client.LogicalRouterDelete(n.getRouterName())
		if err != nil {
			return err
		}

		err = client.LogicalSwitchDelete(n.getExtSwitchName())
		if err != nil {
			return err
		}

		err = client.LogicalSwitchDelete(n.getIntSwitchName())
		if err != nil {
			return err
		}

		err = client.LogicalRouterPortDelete(n.getRouterExtPortName())
		if err != nil {
			return err
		}

		err = client.LogicalRouterPortDelete(n.getRouterIntPortName())
		if err != nil {
			return err
		}

		err = client.LogicalSwitchPortDelete(n.getExtSwitchRouterPortName())
		if err != nil {
			return err
		}

		err = client.LogicalSwitchPortDelete(n.getExtSwitchProviderPortName())
		if err != nil {
			return err
		}

		err = client.LogicalSwitchPortDelete(n.getIntSwitchRouterPortName())
		if err != nil {
			return err
		}

		// Must be done after logical router removal.
		err = client.ChassisGroupDelete(n.getChassisGroupName())
		if err != nil {
			return err
		}
	}

	// Delete local parent uplink port.
	err := n.deleteParentPort()
	if err != nil {
		return err
	}

	return n.common.delete(clusterNotification)
}

// Rename renames a network.
func (n *ovn) Rename(newName string) error {
	n.logger.Debug("Rename", log.Ctx{"newName": newName})

	// Sanity checks.
	inUse, err := n.IsUsed()
	if err != nil {
		return err
	}

	if inUse {
		return fmt.Errorf("The network is currently in use")
	}

	// Rename common steps.
	err = n.common.rename(newName)
	if err != nil {
		return err
	}

	return nil
}

// Start starts configures the local OVS parent uplink port.
func (n *ovn) Start() error {
	if n.status == api.NetworkStatusPending {
		return fmt.Errorf("Cannot start pending network")
	}

	err := n.startParentPort()
	if err != nil {
		return err
	}

	return nil
}

// Stop stops is a no-op.
func (n *ovn) Stop() error {
	return nil
}

// Update updates the network. Accepts notification boolean indicating if this update request is coming from a
// cluster notification, in which case do not update the database, just apply local changes needed.
func (n *ovn) Update(newNetwork api.NetworkPut, targetNode string, clusterNotification bool) error {
	n.logger.Debug("Update", log.Ctx{"clusterNotification": clusterNotification, "newNetwork": newNetwork})

	// Populate default values if they are missing.
	err := n.fillConfig(newNetwork.Config)
	if err != nil {
		return err
	}

	dbUpdateNeeeded, changedKeys, oldNetwork, err := n.common.configChanged(newNetwork)
	if err != nil {
		return err
	}

	if !dbUpdateNeeeded {
		return nil // Nothing changed.
	}

	revert := revert.New()
	defer revert.Fail()

	// Define a function which reverts everything.
	revert.Add(func() {
		// Reset changes to all nodes and database.
		n.common.update(oldNetwork, targetNode, clusterNotification)

		// Reset any change that was made to logical network.
		if !clusterNotification {
			n.setup(true)
		}
	})

	// Apply changes to database.
	err = n.common.update(newNetwork, targetNode, clusterNotification)
	if err != nil {
		return err
	}

	// Restart the logical network if needed.
	if len(changedKeys) > 0 && !clusterNotification {
		err = n.setup(true)
		if err != nil {
			return err
		}
	}

	revert.Success()
	return nil
}

// getInstanceDevicePortName returns the switch port name to use for an instance device.
func (n *ovn) getInstanceDevicePortName(instanceID int, deviceName string) openvswitch.OVNSwitchPort {
	return openvswitch.OVNSwitchPort(fmt.Sprintf("%s-%d-%s", n.getIntSwitchInstancePortPrefix(), instanceID, deviceName))
}

// instanceDevicePortAdd adds an instance device port to the internal logical switch and returns the port name.
func (n *ovn) instanceDevicePortAdd(instanceID int, deviceName string, mac net.HardwareAddr, ips []net.IP) (openvswitch.OVNSwitchPort, error) {
	var dhcpV4ID, dhcpv6ID string

	revert := revert.New()
	defer revert.Fail()

	client, err := n.getClient()
	if err != nil {
		return "", err
	}

	// Get DHCP options IDs.
	if n.getRouterIntPortIPv4Net() != "" {
		_, routerIntPortIPv4Net, err := net.ParseCIDR(n.getRouterIntPortIPv4Net())
		if err != nil {
			return "", err
		}

		dhcpV4ID, err = client.LogicalSwitchDHCPOptionsGetID(n.getIntSwitchName(), routerIntPortIPv4Net)
		if err != nil {
			return "", err
		}
	}

	if n.getRouterIntPortIPv6Net() != "" {
		_, routerIntPortIPv6Net, err := net.ParseCIDR(n.getRouterIntPortIPv6Net())
		if err != nil {
			return "", err
		}

		dhcpv6ID, err = client.LogicalSwitchDHCPOptionsGetID(n.getIntSwitchName(), routerIntPortIPv6Net)
		if err != nil {
			return "", err
		}
	}

	instancePortName := n.getInstanceDevicePortName(instanceID, deviceName)

	// Add port with mayExist set to true, so that if instance port exists, we don't fail and continue below
	// to configure the port as needed. This is required in case the OVN northbound database was unavailable
	// when the instance NIC was stopped and was unable to remove the port on last stop, which would otherwise
	// prevent future NIC starts.
	err = client.LogicalSwitchPortAdd(n.getIntSwitchName(), instancePortName, true)
	if err != nil {
		return "", err
	}

	revert.Add(func() { client.LogicalSwitchPortDelete(instancePortName) })

	err = client.LogicalSwitchPortSet(instancePortName, &openvswitch.OVNSwitchPortOpts{
		DHCPv4OptsID: dhcpV4ID,
		DHCPv6OptsID: dhcpv6ID,
		MAC:          mac,
		IPs:          ips,
	})
	if err != nil {
		return "", err
	}

	revert.Success()
	return instancePortName, nil
}

// instanceDevicePortDelete deletes an instance device port from the internal logical switch.
func (n *ovn) instanceDevicePortDelete(instanceID int, deviceName string) error {
	instancePortName := n.getInstanceDevicePortName(instanceID, deviceName)

	client, err := n.getClient()
	if err != nil {
		return err
	}

	err = client.LogicalSwitchPortDelete(instancePortName)
	if err != nil {
		return err
	}

	return nil
}
