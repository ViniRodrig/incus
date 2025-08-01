package network

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/flosch/pongo2/v6"
	"github.com/mdlayher/netx/eui64"
	ovsClient "github.com/ovn-org/libovsdb/client"
	ovsdbModel "github.com/ovn-org/libovsdb/model"

	incus "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/internal/iprange"
	"github.com/lxc/incus/v6/internal/server/cluster"
	"github.com/lxc/incus/v6/internal/server/cluster/request"
	"github.com/lxc/incus/v6/internal/server/db"
	dbCluster "github.com/lxc/incus/v6/internal/server/db/cluster"
	deviceConfig "github.com/lxc/incus/v6/internal/server/device/config"
	"github.com/lxc/incus/v6/internal/server/dnsmasq/dhcpalloc"
	"github.com/lxc/incus/v6/internal/server/instance"
	"github.com/lxc/incus/v6/internal/server/ip"
	"github.com/lxc/incus/v6/internal/server/locking"
	"github.com/lxc/incus/v6/internal/server/network/acl"
	addressset "github.com/lxc/incus/v6/internal/server/network/address-set"
	networkOVN "github.com/lxc/incus/v6/internal/server/network/ovn"
	ovnSB "github.com/lxc/incus/v6/internal/server/network/ovn/schema/ovn-sb"
	"github.com/lxc/incus/v6/internal/server/network/ovs"
	"github.com/lxc/incus/v6/internal/server/project"
	"github.com/lxc/incus/v6/internal/server/state"
	localUtil "github.com/lxc/incus/v6/internal/server/util"
	internalUtil "github.com/lxc/incus/v6/internal/util"
	"github.com/lxc/incus/v6/shared/api"
	"github.com/lxc/incus/v6/shared/logger"
	"github.com/lxc/incus/v6/shared/revert"
	"github.com/lxc/incus/v6/shared/util"
	"github.com/lxc/incus/v6/shared/validate"
)

const (
	ovnChassisPriorityMax = 32767
	ovnVolatileUplinkIPv4 = "volatile.network.ipv4.address"
	ovnVolatileUplinkIPv6 = "volatile.network.ipv6.address"
)

const (
	ovnRouterPolicyPeerAllowPriority = 600
	ovnRouterPolicyPeerDropPriority  = 500
)

// ovnUplinkVars OVN object variables derived from uplink network.
type ovnUplinkVars struct {
	// Router.
	routerExtPortIPv4Net string
	routerExtPortIPv6Net string
	routerExtGwIPv4      net.IP
	routerExtGwIPv6      net.IP

	// External Switch.
	extSwitchProviderName string

	// DNS.
	dnsIPv6 []net.IP
	dnsIPv4 []net.IP
}

// ovnUplinkPortBridgeVars uplink bridge port variables used for start/stop.
type ovnUplinkPortBridgeVars struct {
	ovsBridge string
	uplinkEnd string
	ovsEnd    string
}

// OVNInstanceNICSetupOpts options for starting an OVN Instance NIC.
type OVNInstanceNICSetupOpts struct {
	InstanceUUID string
	DeviceName   string
	DeviceConfig deviceConfig.Device
	UplinkConfig map[string]string
	DNSName      string
	LastStateIPs []net.IP
}

// OVNInstanceNICStopOpts options for stopping an OVN Instance NIC.
type OVNInstanceNICStopOpts struct {
	InstanceUUID string
	DeviceName   string
	DeviceConfig deviceConfig.Device
}

// ovn represents an OVN network.
type ovn struct {
	common

	ovnnb *networkOVN.NB
	ovnsb *networkOVN.SB
}

func (n *ovn) init(s *state.State, id int64, projectName string, netInfo *api.Network, netNodes map[int64]db.NetworkNode) error {
	// Check that OVN is available.
	if s != nil {
		ovnnb, ovnsb, err := s.OVN()
		if err != nil {
			return err
		}

		n.ovnnb = ovnnb
		n.ovnsb = ovnsb
	}

	return n.common.init(s, id, projectName, netInfo, netNodes)
}

// DBType returns the network type DB ID.
func (n *ovn) DBType() db.NetworkType {
	return db.NetworkTypeOVN
}

// Config returns the network driver info.
func (n *ovn) Info() Info {
	info := n.common.Info()
	info.Projects = true
	info.NodeSpecificConfig = false
	info.AddressForwards = true
	info.LoadBalancers = true
	info.Peering = true

	return info
}

func (n *ovn) State() (*api.NetworkState, error) {
	// Get the addresses.
	var addresses []api.NetworkStateAddress
	IPv4Net, err := ParseIPCIDRToNet(n.config["ipv4.address"])
	if err == nil {
		ones, _ := IPv4Net.Mask.Size()
		addresses = append(addresses, api.NetworkStateAddress{
			Family:  "inet",
			Address: IPv4Net.IP.String(),
			Netmask: strconv.Itoa(ones),
			Scope:   "link",
		})
	}

	IPv6Net, err := ParseIPCIDRToNet(n.config["ipv6.address"])
	if err == nil {
		ones, _ := IPv6Net.Mask.Size()
		addresses = append(addresses, api.NetworkStateAddress{
			Family:  "inet6",
			Address: IPv6Net.IP.String(),
			Netmask: strconv.Itoa(ones),
			Scope:   "link",
		})
	}

	var chassis string
	var hwaddr string
	var uplinkIPv4 string
	var uplinkIPv6 string

	logicalRouterName := n.getRouterName()
	logicalSwitchName := n.getIntSwitchName()

	// Check if an uplink network is present.
	if n.config["network"] != "none" {
		// Get the current active chassis.
		chassis, err = n.ovnsb.GetLogicalRouterPortActiveChassisHostname(context.TODO(), n.getRouterExtPortName())
		if err != nil {
			return nil, err
		}

		// Get the IPv4 and IPv6 addresses on the uplink.
		if n.config[ovnVolatileUplinkIPv4] != "" {
			uplinkIPv4 = n.config[ovnVolatileUplinkIPv4]
		}

		if n.config[ovnVolatileUplinkIPv6] != "" {
			uplinkIPv6 = n.config[ovnVolatileUplinkIPv6]
		}
	} else if n.config["ipv4.address"] == "none" && n.config["ipv6.address"] == "none" {
		// Networks with no uplink and no IP addresses will not have a router.
		logicalRouterName = ""
	}

	// Add the gateway MAC address if one is present.
	if logicalRouterName != "" {
		var ok bool

		hwaddr, ok = n.config["bridge.hwaddr"]
		if !ok {
			hwaddr, err = n.ovnnb.GetLogicalRouterPortHardwareAddress(context.TODO(), n.getRouterIntPortName())
			if err != nil {
				return nil, err
			}
		}
	}

	// Get the switch MTU.
	mtu := int(n.getBridgeMTU())
	if mtu == 0 {
		mtu = 1500
	}

	return &api.NetworkState{
		Addresses: addresses,
		Hwaddr:    hwaddr,
		Mtu:       mtu,
		State:     "up",
		Type:      "broadcast",
		OVN: &api.NetworkStateOVN{
			Chassis:       chassis,
			LogicalRouter: string(logicalRouterName),
			LogicalSwitch: string(logicalSwitchName),
			UplinkIPv4:    uplinkIPv4,
			UplinkIPv6:    uplinkIPv6,
		},
	}, nil
}

// uplinkRoutes parses ipv4.routes and ipv6.routes settings for an uplink network into a slice of *net.IPNet.
func (n *ovn) uplinkRoutes(uplink *api.Network) ([]*net.IPNet, error) {
	var err error
	var uplinkRoutes []*net.IPNet
	for _, k := range []string{"ipv4.routes", "ipv6.routes"} {
		if uplink.Config[k] == "" {
			continue
		}

		uplinkRoutes, err = SubnetParseAppend(uplinkRoutes, util.SplitNTrimSpace(uplink.Config[k], ",", -1, false)...)
		if err != nil {
			return nil, err
		}
	}

	return uplinkRoutes, nil
}

// projectRestrictedSubnets parses the restrict.networks.subnets project setting and returns slice of *net.IPNet.
// Returns nil slice if no project restrictions, or empty slice if no allowed subnets.
func (n *ovn) projectRestrictedSubnets(p *api.Project, uplinkNetworkName string) ([]*net.IPNet, error) {
	// Parse project's restricted subnets.
	var projectRestrictedSubnets []*net.IPNet // Nil value indicates not restricted.
	if util.IsTrue(p.Config["restricted"]) && p.Config["restricted.networks.subnets"] != "" {
		projectRestrictedSubnets = []*net.IPNet{} // Empty slice indicates no allowed subnets.

		for _, subnetRaw := range util.SplitNTrimSpace(p.Config["restricted.networks.subnets"], ",", -1, false) {
			subnetParts := strings.SplitN(subnetRaw, ":", 2)
			if len(subnetParts) != 2 {
				return nil, fmt.Errorf(`Project subnet %q invalid, must be in the format of "<uplink network>:<subnet>"`, subnetRaw)
			}

			subnetUplinkName := subnetParts[0]
			subnetStr := subnetParts[1]

			if subnetUplinkName != uplinkNetworkName {
				continue // Only include subnets for our uplink.
			}

			_, restrictedSubnet, err := net.ParseCIDR(subnetStr)
			if err != nil {
				return nil, err
			}

			projectRestrictedSubnets = append(projectRestrictedSubnets, restrictedSubnet)
		}
	}

	return projectRestrictedSubnets, nil
}

// validateExternalSubnet checks the supplied ipNet is allowed within the uplink routes and project
// restricted subnets. If projectRestrictedSubnets is nil, then it is not checked as this indicates project has
// no restrictions. Whereas if uplinkRoutes is nil/empty then this will always return an error.
func (n *ovn) validateExternalSubnet(uplink *api.Network, projectRestrictedSubnets []*net.IPNet, ipNet *net.IPNet) error {
	// Check that the IP network is within the project's restricted subnets if restricted.
	if projectRestrictedSubnets != nil {
		foundMatch := false
		for _, projectRestrictedSubnet := range projectRestrictedSubnets {
			if SubnetContains(projectRestrictedSubnet, ipNet) {
				foundMatch = true
				break
			}
		}

		if !foundMatch {
			return fmt.Errorf("Project doesn't contain %q in its restricted uplink subnets", ipNet.String())
		}
	}

	// Check if the IP network is within the uplink network's routes.
	uplinkRoutes, err := n.uplinkRoutes(uplink)
	if err != nil {
		return err
	}

	for _, uplinkRoute := range uplinkRoutes {
		if SubnetContains(uplinkRoute, ipNet) {
			return nil
		}
	}

	// Load uplink network details.
	uplinkIPv4CIDR := uplink.Config["ipv4.address"]
	if uplinkIPv4CIDR == "" {
		uplinkIPv4CIDR = uplink.Config["ipv4.gateway"]
	}

	uplinkIPv6CIDR := uplink.Config["ipv6.address"]
	if uplinkIPv6CIDR == "" {
		uplinkIPv6CIDR = uplink.Config["ipv6.gateway"]
	}

	uplinkIPv4, uplinkIPv4Net, _ := net.ParseCIDR(uplinkIPv4CIDR)
	uplinkIPv6, uplinkIPv6Net, _ := net.ParseCIDR(uplinkIPv6CIDR)

	// Check if the IP network is within the uplink network.
	if uplinkIPv4Net != nil && SubnetContains(uplinkIPv4Net, ipNet) {
		if ipNet.Contains(uplinkIPv4) {
			return api.StatusErrorf(http.StatusBadRequest, "Requested subnet %q would mask the uplink gateway", ipNet.String())
		}

		return nil
	}

	if uplinkIPv6Net != nil && SubnetContains(uplinkIPv6Net, ipNet) {
		if ipNet.Contains(uplinkIPv6) {
			return api.StatusErrorf(http.StatusBadRequest, "Requested subnet %q would mask the uplink gateway", ipNet.String())
		}

		return nil
	}

	return api.StatusErrorf(http.StatusBadRequest, "Uplink network doesn't contain %q in its routes", ipNet.String())
}

// getExternalSubnetInUse returns information about usage of external subnets by networks and NICs connected to,
// or used by, the specified uplinkNetworkName.
func (n *ovn) getExternalSubnetInUse(uplinkNetworkName string) ([]externalSubnetUsage, error) {
	var err error
	var projectNetworks map[string]map[int64]api.Network
	var externalSubnets []externalSubnetUsage

	err = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		// Get all managed networks across all projects.
		projectNetworks, err = tx.GetCreatedNetworks(ctx)
		if err != nil {
			return fmt.Errorf("Failed to load all networks: %w", err)
		}

		externalSubnets, err = n.common.getExternalSubnetInUse(ctx, tx, uplinkNetworkName, false)
		if err != nil {
			return fmt.Errorf("Failed getting external subnets in use: %w", err)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// Get OVN networks that use the same uplink as us.
	ovnProjectNetworksWithOurUplink := n.ovnProjectNetworksWithUplink(uplinkNetworkName, projectNetworks)

	// Get external subnets used by other OVN networks using our uplink.
	ovnNetworkExternalSubnets, err := n.ovnNetworkExternalSubnets(ovnProjectNetworksWithOurUplink)
	if err != nil {
		return nil, err
	}

	// Get external routes configured on OVN NICs using networks that use our uplink.
	ovnNICExternalRoutes, err := n.ovnNICExternalRoutes(ovnProjectNetworksWithOurUplink)
	if err != nil {
		return nil, err
	}

	externalSubnets = append(externalSubnets, ovnNetworkExternalSubnets...)
	externalSubnets = append(externalSubnets, ovnNICExternalRoutes...)

	return externalSubnets, nil
}

// Validate network config.
func (n *ovn) Validate(config map[string]string) error {
	rules := map[string]func(value string) error{
		// gendoc:generate(entity=network_ovn, group=common, key=network)
		//
		// ---
		//  type: string
		//  shortdesc: Uplink network to use for external network access or `none` to keep isolated
		"network": validate.IsAny,

		// gendoc:generate(entity=network_ovn, group=common, key=bridge.hwaddr)
		//
		// ---
		//  type: string
		//  shortdesc: MAC address for the virtual bridge interface

		"bridge.hwaddr": validate.Optional(validate.IsNetworkMAC),
		// gendoc:generate(entity=network_ovn, group=common, key=bridge.mtu)
		//
		// ---
		//  type: integer
		//  shortdesc: Bridge MTU (default allows host to host Geneve tunnels)
		//  default: `1442`

		"bridge.mtu": validate.Optional(validate.IsNetworkMTU),
		// gendoc:generate(entity=network_ovn, group=common, key=bridge.external_interfaces)
		//
		// ---
		//  type: string
		//  shortdesc: Comma-separated list of unconfigured network interfaces to include in the bridge

		"bridge.external_interfaces": validate.Optional(validateExternalInterfaces),
		// gendoc:generate(entity=network_ovn, group=common, key=ipv4.address)
		//
		// ---
		//  type: string
		//  shortdesc: IPv4 address for the bridge (use `none` to turn off IPv4 or `auto` to generate a new random unused subnet) (CIDR)
		//  condition: standard mode
		//  default: (initial value on creation: `auto`)
		"ipv4.address": validate.Optional(func(value string) error {
			if validate.IsOneOf("none", "auto")(value) == nil {
				return nil
			}

			return validate.IsNetworkAddressCIDRV4(value)
		}),

		// gendoc:generate(entity=network_ovn, group=common, key=ipv4.dhcp)
		//
		// ---
		//  type: bool
		//  shortdesc: Whether to allocate addresses using DHCP
		//  condition: IPv4 address
		//  default: `true`
		"ipv4.dhcp": validate.Optional(validate.IsBool),

		// gendoc:generate(entity=network_ovn, group=common, key=ipv4.dhcp.expiry)
		//
		// ---
		//  type: string
		//  shortdesc: When to expire DHCP leases
		//  condition: IPv4 DHCP
		//  default: `1h`
		"ipv4.dhcp.expiry": validate.Optional(func(value string) error {
			_, err := time.ParseDuration(value)
			return err
		}),

		// gendoc:generate(entity=network_ovn, group=common, key=ipv4.dhcp.ranges)
		//
		// ---
		//  type: string
		//  condition: IPv4 DHCP
		//  default: all addresses
		//  shortdesc: Comma-separated list of IP ranges to use for DHCP (FIRST-LAST format)
		"ipv4.dhcp.ranges": validate.Optional(validate.IsListOf(validate.IsNetworkRangeV4)),

		// gendoc:generate(entity=network_ovn, group=common, key=ipv4.dhcp.routes)
		//
		// ---
		//  type: string
		//  condition: IPv4 DHCP
		//  shortdesc: Static routes to provide via DHCP option 121, as a comma-separated list of alternating subnets (CIDR) and gateway addresses (same syntax as dnsmasq and OVN)
		"ipv4.dhcp.routes": validate.Optional(validate.IsDHCPRouteList),

		// gendoc:generate(entity=network_ovn, group=common, key=ipv6.address)
		//
		// ---
		//  type: string
		//  shortdesc: IPv6 address for the bridge (use `none` to turn off IPv6 or `auto` to generate a new random unused subnet) (CIDR)
		//  condition: standard mode
		//  default: (initial value on creation: `auto`)
		"ipv6.address": validate.Optional(func(value string) error {
			if validate.IsOneOf("none", "auto")(value) == nil {
				return nil
			}

			return validate.IsNetworkAddressCIDRV6(value)
		}),

		// gendoc:generate(entity=network_ovn, group=common, key=ipv6.dhcp)
		//
		// ---
		//  type: bool
		//  condition: IPv6 address
		//  shortdesc: Whether to provide additional network configuration over DHCP
		//  default: `true`
		"ipv6.dhcp": validate.Optional(validate.IsBool),

		// gendoc:generate(entity=network_ovn, group=common, key=ipv6.dhcp.stateful)
		//
		// ---
		//  type: bool
		//  condition: IPv6 DHCP
		//  shortdesc: Whether to allocate addresses using DHCP
		//  default: `false`
		"ipv6.dhcp.stateful": validate.Optional(validate.IsBool),

		// gendoc:generate(entity=network_ovn, group=common, key=ipv4.nat)
		//
		// ---
		//  type: bool
		//  shortdesc: Whether to NAT
		//  condition: IPv4 address
		//  default: `false` initial value on creation if `ipv4.address` is set to `auto: true`)
		"ipv4.nat": validate.Optional(validate.IsBool),

		// gendoc:generate(entity=network_ovn, group=common, key=ipv4.nat.address)
		//
		// ---
		//  type: string
		//  shortdesc: The source address used for outbound traffic from the network (requires uplink `ovn.ingress_mode=routed`)
		//  condition: IPv4 address
		"ipv4.nat.address": validate.Optional(validate.IsNetworkAddressV4),

		// gendoc:generate(entity=network_ovn, group=common, key=ipv6.nat)
		//
		// ---
		//  type: bool
		//  condition: IPv6 address
		//  shortdesc: Whether to NAT
		//  default: `false` (initial value on creation if `ipv6.address` is set to `auto: true`)
		"ipv6.nat": validate.Optional(validate.IsBool),

		// gendoc:generate(entity=network_ovn, group=common, key=ipv6.nat.address)
		//
		// ---
		//  type: string
		//  condition: IPv6 address
		//  shortdesc: The source address used for outbound traffic from the network (requires uplink `ovn.ingress_mode=routed`)
		"ipv6.nat.address": validate.Optional(validate.IsNetworkAddressV6),

		// gendoc:generate(entity=network_ovn, group=common, key=ipv4.l3only)
		//
		// ---
		//  type: bool
		//  shortdesc: Whether to enable layer 3 only mode.
		//  condition: IPv4 address
		//  default: `false`
		"ipv4.l3only": validate.Optional(validate.IsBool),

		// gendoc:generate(entity=network_ovn, group=common, key=ipv6.l3only)
		//
		// ---
		//  type: bool
		//  condition: IPv6 DHCP stateful
		//  shortdesc: Whether to enable layer 3 only mode.
		//  default: `false`
		"ipv6.l3only": validate.Optional(validate.IsBool),

		// gendoc:generate(entity=network_ovn, group=common, key=dns.nameservers)
		//
		// ---
		//  type: string
		//  shortdesc: DNS server IPs to advertise to DHCP clients and via Router Advertisements. Both IPv4 and IPv6 addresses get pushed via DHCP, and the first IPv6 address is also advertised as RDNSS via RA.
		//  default: Uplink DNS servers (IPv4 and IPv6 address if no uplink is configured)
		"dns.nameservers": validate.Optional(validate.IsListOf(validate.IsNetworkAddress)),

		// gendoc:generate(entity=network_ovn, group=common, key=dns.domain)
		//
		// ---
		//  type: string
		//  default: `incus`
		//  shortdesc: Domain to advertise to DHCP clients and use for DNS resolution
		"dns.domain": validate.IsAny,

		// gendoc:generate(entity=network_ovn, group=common, key=dns.search)
		//
		// ---
		//  type: string
		//  shortdesc: Full comma-separated domain search list, defaulting to `dns.domain` value
		"dns.search": validate.IsAny,

		// gendoc:generate(entity=network_ovn, group=common, key=dns.zone.forward)
		//
		// ---
		//  type: string
		//  shortdesc: Comma-separated list of DNS zone names for forward DNS records
		"dns.zone.forward": validate.IsAny,

		// gendoc:generate(entity=network_ovn, group=common, key=dns.zone.reverse.ipv4)
		//
		// ---
		//  type: string
		//  shortdesc: DNS zone name for IPv4 reverse DNS records
		"dns.zone.reverse.ipv4": validate.IsAny,

		// gendoc:generate(entity=network_ovn, group=common, key=dns.zone.reverse.ipv6)
		//
		// ---
		//  type: string
		//  shortdesc: DNS zone name for IPv6 reverse DNS records
		"dns.zone.reverse.ipv6": validate.IsAny,

		// gendoc:generate(entity=network_ovn, group=common, key=security.acls)
		//
		// ---
		//  type: string
		//  shortdesc: Comma-separated list of Network ACLs to apply to NICs connected to this network
		"security.acls": validate.IsAny,

		// gendoc:generate(entity=network_ovn, group=common, key=security.acls.default.ingress.action)
		//
		// ---
		//  type: string
		//  condition: `security.acls`
		//  shortdesc: Action to use for ingress traffic that doesn't match any ACL rule
		//  default: `reject`
		"security.acls.default.ingress.action": validate.Optional(validate.IsOneOf(acl.ValidActions...)),

		// gendoc:generate(entity=network_ovn, group=common, key=security.acls.default.egress.action)
		//
		// ---
		//  type: string
		//  shortdesc: Action to use for egress traffic that doesn't match any ACL rule
		//  default: `reject`
		//  condition: `security.acls`
		"security.acls.default.egress.action": validate.Optional(validate.IsOneOf(acl.ValidActions...)),

		// gendoc:generate(entity=network_ovn, group=common, key=security.acls.default.ingress.logged)
		//
		// ---
		//  type: bool
		//  condition: `security.acls`
		//  shortdesc: Whether to log ingress traffic that doesn't match any ACL rule
		//  default: `false`
		"security.acls.default.ingress.logged": validate.Optional(validate.IsBool),

		// gendoc:generate(entity=network_ovn, group=common, key=security.acls.default.egress.logged)
		//
		// ---
		//  type: bool
		//  shortdesc: Whether to log egress traffic that doesn't match any ACL rule
		//  default: `false`
		//  condition: `security.acls`
		"security.acls.default.egress.logged": validate.Optional(validate.IsBool),

		// gendoc:generate(entity=network_ovn, group=common, key=user.*)
		//
		// ---
		//  type: string
		//  shortdesc: User-provided free-form key/value pairs

		// Volatile keys populated automatically as needed.
		ovnVolatileUplinkIPv4: validate.Optional(validate.IsNetworkAddressV4),
		ovnVolatileUplinkIPv6: validate.Optional(validate.IsNetworkAddressV6),
	}

	err := n.validate(config, rules)
	if err != nil {
		return err
	}

	// Perform composite key checks after per-key validation.

	// Validate DNS zone names.
	err = n.validateZoneNames(config)
	if err != nil {
		return err
	}

	if config["ipv4.address"] != "" {
		ipv4Addr, ipv4Net, _ := net.ParseCIDR(config["ipv4.address"])
		if ipv4Net != nil {
			ovnRouter, err := netip.ParseAddr(dhcpalloc.GetIP(ipv4Net, -2).String())
			if err != nil {
				return err
			}

			addr, err := netip.ParseAddr(ipv4Addr.String())
			if err != nil {
				return err
			}

			if ovnRouter.Compare(addr) == 0 {
				return fmt.Errorf("'ipv4.address' cannot be set to %s because it is reserved for OVN load-balancer health checks", ovnRouter.String())
			}
		}
	}

	// Check that if IPv6 enabled then the network size must be at least a /64 as both RA and DHCPv6
	// in OVN (as it generates addresses using EUI64) require at least a /64 subnet to operate.
	_, ipv6Net, _ := net.ParseCIDR(config["ipv6.address"])
	if ipv6Net != nil {
		ones, _ := ipv6Net.Mask.Size()
		if ones > 64 {
			return errors.New("IPv6 subnet must be at least a /64")
		}
	}

	// Load the project to get uplink network restrictions.
	var p *api.Project
	err = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		project, err := dbCluster.GetProject(ctx, tx.Tx(), n.project)
		if err != nil {
			return err
		}

		p, err = project.ToAPI(ctx, tx.Tx())

		return err
	})
	if err != nil {
		return fmt.Errorf("Failed to load network restrictions from project %q: %w", n.project, err)
	}

	// Check uplink network is valid and allowed in project.
	var uplink *api.Network
	projectRestrictedSubnets := []*net.IPNet{}

	if n.config["network"] != "none" {
		uplinkNetworkName, err := n.validateUplinkNetwork(p, config["network"])
		if err != nil {
			return err
		}

		err = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
			// Get uplink routes.
			_, uplink, _, err = tx.GetNetworkInAnyState(ctx, api.ProjectDefaultName, uplinkNetworkName)

			return err
		})
		if err != nil {
			return fmt.Errorf("Failed to load uplink network %q: %w", uplinkNetworkName, err)
		}

		// Get project restricted routes.
		projectRestrictedSubnets, err = n.projectRestrictedSubnets(p, uplinkNetworkName)
		if err != nil {
			return err
		}
	}

	// Parse the network's address subnets for further checks.
	netSubnets := make(map[string]*net.IPNet)
	for _, keyPrefix := range []string{"ipv4", "ipv6"} {
		addressKey := fmt.Sprintf("%s.address", keyPrefix)
		if validate.IsOneOf("", "none", "auto")(config[addressKey]) != nil {
			_, ipNet, err := net.ParseCIDR(config[addressKey])
			if err != nil {
				return fmt.Errorf("Failed parsing %q: %w", addressKey, err)
			}

			netSubnets[addressKey] = ipNet
		}
	}

	// Check Security ACLs exist.
	if config["security.acls"] != "" {
		err = acl.Exists(n.state, n.project, util.SplitNTrimSpace(config["security.acls"], ",", -1, true)...)
		if err != nil {
			return err
		}
	}

	// Check that ipv6.l3only mode is used with ipvp.dhcp.stateful.
	// As otherwise the router advertisements will configure an address using the subnet's mask.
	if util.IsTrue(config["ipv6.l3only"]) && util.IsTrueOrEmpty(config["ipv6.dhcp"]) && util.IsFalseOrEmpty(config["ipv6.dhcp.stateful"]) {
		return errors.New("The ipv6.dhcp.stateful setting must be enabled when using ipv6.l3only mode with ipv6.dhcp enabled")
	}

	// All tests below are related to the uplink network, skip if we don't have one.
	if uplink == nil {
		return nil
	}

	// If NAT disabled, parse the external subnets that are being requested.
	var externalSubnets []*net.IPNet // Subnets to check for conflicts with other networks/NICs.
	for _, keyPrefix := range []string{"ipv4", "ipv6"} {
		addressKey := fmt.Sprintf("%s.address", keyPrefix)
		netSubnet := netSubnets[addressKey]

		if util.IsFalseOrEmpty(config[fmt.Sprintf("%s.nat", keyPrefix)]) && netSubnet != nil {
			// Add to list to check for conflicts.
			externalSubnets = append(externalSubnets, netSubnet)
		}
	}

	// Check SNAT addresses specified are allowed to be used based on uplink's ovn.ingress_mode setting.
	var externalSNATSubnets []*net.IPNet // Subnets to check for conflicts with other networks/NICs.
	for _, keyPrefix := range []string{"ipv4", "ipv6"} {
		snatAddressKey := fmt.Sprintf("%s.nat.address", keyPrefix)
		if config[snatAddressKey] != "" {
			if uplink.Config["ovn.ingress_mode"] != "routed" {
				return fmt.Errorf(`Cannot specify %q when uplink ovn.ingress_mode is not "routed"`, snatAddressKey)
			}

			subnetSize := 128
			if keyPrefix == "ipv4" {
				subnetSize = 32
			}

			_, snatIPNet, err := net.ParseCIDR(fmt.Sprintf("%s/%d", config[snatAddressKey], subnetSize))
			if err != nil {
				return fmt.Errorf("Failed parsing %q: %w", snatAddressKey, err)
			}

			// Add to list to check for conflicts.
			externalSNATSubnets = append(externalSNATSubnets, snatIPNet)
		}
	}

	if len(externalSubnets) > 0 || len(externalSNATSubnets) > 0 {
		externalSubnetsInUse, err := n.getExternalSubnetInUse(config["network"])
		if err != nil {
			return err
		}

		// Check if uplink has routed ingress anycast mode enabled, as this relaxes the overlap checks.
		ipv4UplinkAnycast := n.uplinkHasIngressRoutedAnycastIPv4(uplink)
		ipv6UplinkAnycast := n.uplinkHasIngressRoutedAnycastIPv6(uplink)

		for _, externalSubnet := range externalSubnets {
			// Check the external subnet is allowed within both the uplink's external routes and any
			// project restricted subnets.
			err = n.validateExternalSubnet(uplink, projectRestrictedSubnets, externalSubnet)
			if err != nil {
				return err
			}

			// Skip overlap checks if external subnet's protocol has anycast mode enabled on uplink.
			if externalSubnet.IP.To4() == nil {
				if ipv6UplinkAnycast {
					continue
				}
			} else if ipv4UplinkAnycast {
				continue
			}

			// Check the external subnet doesn't fall within any existing OVN network external subnets.
			for _, externalSubnetUser := range externalSubnetsInUse {
				// Skip our own network (but not NIC devices on our own network).
				if externalSubnetUser.usageType != subnetUsageInstance && externalSubnetUser.networkProject == n.project && externalSubnetUser.networkName == n.name {
					continue
				}

				if SubnetContains(&externalSubnetUser.subnet, externalSubnet) || SubnetContains(externalSubnet, &externalSubnetUser.subnet) {
					// This error is purposefully vague so that it doesn't reveal any names of
					// resources potentially outside of the network's project.
					return fmt.Errorf("External subnet %q overlaps with another network or NIC", externalSubnet.String())
				}
			}
		}

		for _, externalSNATSubnet := range externalSNATSubnets {
			// Check the external subnet is allowed within both the uplink's external routes and any
			// project restricted subnets.
			err = n.validateExternalSubnet(uplink, projectRestrictedSubnets, externalSNATSubnet)
			if err != nil {
				return err
			}

			// Skip overlap checks if external subnet's protocol has anycast mode enabled on uplink.
			if externalSNATSubnet.IP.To4() == nil {
				if ipv6UplinkAnycast {
					continue
				}
			} else if ipv4UplinkAnycast {
				continue
			}

			// Check the external subnet doesn't fall within any existing OVN network external subnets.
			for _, externalSubnetUser := range externalSubnetsInUse {
				// Skip our own network (including NIC devices on our own network).
				// Because we may want to specify the SNAT address as the same address as one of
				// the NICs in our network.
				if externalSubnetUser.networkProject == n.project && externalSubnetUser.networkName == n.name {
					continue
				}

				if SubnetContains(&externalSubnetUser.subnet, externalSNATSubnet) || SubnetContains(externalSNATSubnet, &externalSubnetUser.subnet) {
					// This error is purposefully vague so that it doesn't reveal any names of
					// resources potentially outside of the network's project.
					return fmt.Errorf("NAT address %q overlaps with another OVN network or NIC", externalSNATSubnet.IP.String())
				}
			}
		}
	}

	// Check any existing network forward target addresses are suitable for this network's subnet.
	var forwards map[int64]*api.NetworkForward
	err = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		networkID := n.ID()
		dbRecords, err := dbCluster.GetNetworkForwards(ctx, tx.Tx(), dbCluster.NetworkForwardFilter{
			NetworkID: &networkID,
		})
		if err != nil {
			return err
		}

		forwards = make(map[int64]*api.NetworkForward)
		for _, dbRecord := range dbRecords {
			forward, err := dbRecord.ToAPI(ctx, tx.Tx())
			if err != nil {
				return err
			}

			forwards[dbRecord.ID] = forward
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("Failed loading network forwards: %w", err)
	}

	for _, forward := range forwards {
		if forward.Config["target_address"] != "" {
			defaultTargetIP := net.ParseIP(forward.Config["target_address"])

			netSubnet := netSubnets["ipv4.address"]
			if defaultTargetIP.To4() == nil {
				netSubnet = netSubnets["ipv6.address"]
			}

			if !SubnetContainsIP(netSubnet, defaultTargetIP) {
				return api.StatusErrorf(http.StatusBadRequest, "Network forward for %q has a default target address %q that is not within the network subnet", forward.ListenAddress, defaultTargetIP.String())
			}
		}

		for _, port := range forward.Ports {
			targetIP := net.ParseIP(port.TargetAddress)

			netSubnet := netSubnets["ipv4.address"]
			if targetIP.To4() == nil {
				netSubnet = netSubnets["ipv6.address"]
			}

			if !SubnetContainsIP(netSubnet, targetIP) {
				return api.StatusErrorf(http.StatusBadRequest, "Network forward for %q has a port target address %q that is not within the network subnet", forward.ListenAddress, targetIP.String())
			}
		}
	}

	var dbLoadBalancers []dbCluster.NetworkLoadBalancer
	err = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		networkID := n.ID()

		// Get the load balancers.
		dbLoadBalancers, err = dbCluster.GetNetworkLoadBalancers(ctx, tx.Tx(), dbCluster.NetworkLoadBalancerFilter{
			NetworkID: &networkID,
		})
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("Failed loading network load balancers: %w", err)
	}

	for _, dbLoadBalancer := range dbLoadBalancers {
		for _, port := range dbLoadBalancer.Backends {
			targetIP := net.ParseIP(port.TargetAddress)

			netSubnet := netSubnets["ipv4.address"]
			if targetIP.To4() == nil {
				netSubnet = netSubnets["ipv6.address"]
			}

			if !SubnetContainsIP(netSubnet, targetIP) {
				return api.StatusErrorf(http.StatusBadRequest, "Network load balancer for %q has a backend target address %q that is not within the network subnet", dbLoadBalancer.ListenAddress, targetIP.String())
			}
		}
	}

	return nil
}

// getBridgeMTU returns MTU that should be used for the bridge and instance devices.
// Will also be used to configure the OVN DHCP and IPv6 RA options. Returns 0 if the bridge.mtu is not set/invalid.
func (n *ovn) getBridgeMTU() uint32 {
	if n.config["bridge.mtu"] != "" {
		mtu, err := strconv.ParseUint(n.config["bridge.mtu"], 10, 32)
		if err != nil {
			return 0
		}

		return uint32(mtu)
	}

	return 0
}

// getUnderlayInfo returns the MTU for the underlay network interface and the enscapsulation IP for OVN tunnels.
func (n *ovn) getUnderlayInfo() (uint32, net.IP, error) {
	// findMTUFromIP searches all interfaces on the host looking for one that has specified IP.
	findMTUFromIP := func(findIP net.IP) (uint32, error) {
		// Look for interface that has the OVN enscapsulation IP assigned.
		ifaces, err := net.Interfaces()
		if err != nil {
			return 0, fmt.Errorf("Failed getting local network interfaces: %w", err)
		}

		for _, iface := range ifaces {
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}

			for _, addr := range addrs {
				ip, _, err := net.ParseCIDR(addr.String())
				if err != nil {
					continue
				}

				if ip.Equal(findIP) {
					underlayMTU, err := GetDevMTU(iface.Name)
					if err != nil {
						return 0, fmt.Errorf("Failed getting MTU for %q: %w", iface.Name, err)
					}

					return underlayMTU, nil // Found what we were looking for.
				}
			}
		}

		return 0, fmt.Errorf("No matching interface found for OVN enscapsulation IP %q", findIP.String())
	}

	vswitch, err := n.state.OVS()
	if err != nil {
		return 0, nil, fmt.Errorf("Failed to connect to OVS: %w", err)
	}

	encapIP, err := vswitch.GetOVNEncapIP(context.TODO())
	if err != nil {
		return 0, nil, fmt.Errorf("Failed getting OVN enscapsulation IP from OVS: %w", err)
	}

	underlayMTU, err := findMTUFromIP(encapIP)
	if err != nil {
		return 0, nil, err
	}

	return underlayMTU, encapIP, nil
}

// getOptimalBridgeMTU returns the MTU that can be used for the bridge and instance devices based on the MTU value
// of the OVN underlay network interface. This assumes that the OVN tunnel mechanism used is geneve and that the
// same underlying network settings (MTU and encapsulation IP family) are used on all OVN nodes.
func (n *ovn) getOptimalBridgeMTU() (uint32, error) {
	// Get underlay MTU and encapsulation IP.
	underlayMTU, encapIP, err := n.getUnderlayInfo()
	if err != nil {
		return 0, fmt.Errorf("Failed getting OVN underlay info: %w", err)
	}

	// Encapsulation family is IPv6.
	if encapIP.To4() == nil {
		// If the underlay's MTU is large enough to accommodate a 1500 overlay MTU and the geneve tunnel
		// overhead of 78 bytes (when used with IPv6 encapsulation) then indicate 1500 MTU can be used.
		if underlayMTU >= 1578 {
			return 1500, nil
		}

		// Default to 1422 which can work with an underlay MTU of 1500.
		return 1422, nil
	}

	// If the underlay's MTU is large enough to accommodate a 1500 overlay MTU and the geneve tunnel
	// overhead of 58 bytes (when used with IPv4 encapsulation) then indicate 1500 MTU can be used.
	if underlayMTU >= 1558 {
		return 1500, nil
	}

	// Default to 1442 which can work with underlay MTU of 1500.
	return 1442, nil
}

// getNetworkPrefix returns OVN network prefix to use for object names.
func (n *ovn) getNetworkPrefix() string {
	return acl.OVNNetworkPrefix(n.id)
}

// getChassisGroup returns OVN chassis group name to use.
func (n *ovn) getChassisGroupName() networkOVN.OVNChassisGroup {
	return networkOVN.OVNChassisGroup(n.getNetworkPrefix())
}

// getRouterName returns OVN logical router name to use.
func (n *ovn) getRouterName() networkOVN.OVNRouter {
	return networkOVN.OVNRouter(fmt.Sprintf("%s-lr", n.getNetworkPrefix()))
}

// getRouterExtPortName returns OVN logical router external port name to use.
func (n *ovn) getRouterExtPortName() networkOVN.OVNRouterPort {
	return networkOVN.OVNRouterPort(fmt.Sprintf("%s-lrp-ext", n.getRouterName()))
}

// getRouterIntPortName returns OVN logical router internal port name to use.
func (n *ovn) getRouterIntPortName() networkOVN.OVNRouterPort {
	return networkOVN.OVNRouterPort(fmt.Sprintf("%s-lrp-int", n.getRouterName()))
}

// getRouterMAC returns OVN router MAC address to use for ports. Uses a stable seed to return stable random MAC.
func (n *ovn) getRouterMAC() (net.HardwareAddr, error) {
	hwAddr := n.config["bridge.hwaddr"]
	if hwAddr == "" {
		// Load server certificate. This is needs to be the same certificate for all nodes in a cluster.
		cert, err := internalUtil.LoadCert(n.state.OS.VarDir)
		if err != nil {
			return nil, err
		}

		// Generate the random seed, this uses the server certificate fingerprint (to ensure that multiple
		// standalone nodes on the same external network don't generate the same MAC for their networks).
		// It relies on the certificate being the same for all nodes in a cluster to allow the same MAC to
		// be generated on each bridge interface in the network.
		seed := fmt.Sprintf("%s.%d.%d", cert.Fingerprint(), 0, n.ID())
		r, err := localUtil.GetStableRandomGenerator(seed)
		if err != nil {
			return nil, fmt.Errorf("Failed generating stable random router MAC: %w", err)
		}

		hwAddr = randomHwaddr(r)
		n.logger.Debug("Stable MAC generated", logger.Ctx{"seed": seed, "hwAddr": hwAddr})
	}

	mac, err := net.ParseMAC(hwAddr)
	if err != nil {
		return nil, fmt.Errorf("Failed parsing router MAC address %q: %w", mac, err)
	}

	return mac, nil
}

// getRouterIntPortIPv4Net returns OVN logical router internal port IPv4 address and subnet.
func (n *ovn) getRouterIntPortIPv4Net() string {
	return n.config["ipv4.address"]
}

// parseRouterIntPortIPv4Net returns OVN logical router internal port IPv4 address and subnet parsed (if set).
func (n *ovn) parseRouterIntPortIPv4Net() (net.IP, *net.IPNet, error) {
	ipNet := n.getRouterIntPortIPv4Net()

	if validate.IsOneOf("none", "")(ipNet) != nil {
		routerIntPortIPv4, routerIntPortIPv4Net, err := net.ParseCIDR(ipNet)
		if err != nil {
			return nil, nil, fmt.Errorf("Failed parsing router's internal port IPv4 Net: %w", err)
		}

		return routerIntPortIPv4, routerIntPortIPv4Net, nil
	}

	return nil, nil, nil
}

// getRouterIntPortIPv4Net returns OVN logical router internal port IPv6 address and subnet.
func (n *ovn) getRouterIntPortIPv6Net() string {
	return n.config["ipv6.address"]
}

// parseRouterIntPortIPv6Net returns OVN logical router internal port IPv6 address and subnet parsed (if set).
func (n *ovn) parseRouterIntPortIPv6Net() (net.IP, *net.IPNet, error) {
	ipNet := n.getRouterIntPortIPv6Net()

	if validate.IsOneOf("none", "")(ipNet) != nil {
		routerIntPortIPv4, routerIntPortIPv4Net, err := net.ParseCIDR(ipNet)
		if err != nil {
			return nil, nil, fmt.Errorf("Failed parsing router's internal port IPv6 Net: %w", err)
		}

		return routerIntPortIPv4, routerIntPortIPv4Net, nil
	}

	return nil, nil, nil
}

// getDomainName returns OVN DHCP domain name.
func (n *ovn) getDomainName() string {
	if n.config["dns.domain"] != "" {
		return n.config["dns.domain"]
	}

	return "incus"
}

// getDNSSearchList returns OVN DHCP DNS search list. If no search list set returns getDomainName() as list.
func (n *ovn) getDNSSearchList() []string {
	if n.config["dns.search"] != "" {
		return util.SplitNTrimSpace(n.config["dns.search"], ",", -1, false)
	}

	return []string{n.getDomainName()}
}

// getExtSwitchName returns OVN  logical external switch name.
func (n *ovn) getExtSwitchName() networkOVN.OVNSwitch {
	return networkOVN.OVNSwitch(fmt.Sprintf("%s-ls-ext", n.getNetworkPrefix()))
}

// getExtSwitchRouterPortName returns OVN logical external switch router port name.
func (n *ovn) getExtSwitchRouterPortName() networkOVN.OVNSwitchPort {
	return networkOVN.OVNSwitchPort(fmt.Sprintf("%s-lsp-router", n.getExtSwitchName()))
}

// getExtSwitchProviderPortName returns OVN logical external switch provider port name.
func (n *ovn) getExtSwitchProviderPortName() networkOVN.OVNSwitchPort {
	return networkOVN.OVNSwitchPort(fmt.Sprintf("%s-lsp-provider", n.getExtSwitchName()))
}

// getIntSwitchName returns OVN logical internal switch name.
func (n *ovn) getIntSwitchName() networkOVN.OVNSwitch {
	return acl.OVNIntSwitchName(n.id)
}

// getIntSwitchRouterPortName returns OVN logical internal switch router port name.
func (n *ovn) getIntSwitchRouterPortName() networkOVN.OVNSwitchPort {
	return acl.OVNIntSwitchRouterPortName(n.id)
}

// getIntSwitchInstancePortPrefix returns OVN logical internal switch instance port name prefix.
func (n *ovn) getIntSwitchInstancePortPrefix() string {
	return fmt.Sprintf("%s-instance", n.getNetworkPrefix())
}

// getLoadBalancerName returns OVN load balancer name to use for a listen address.
func (n *ovn) getLoadBalancerName(listenAddress string) networkOVN.OVNLoadBalancer {
	return networkOVN.OVNLoadBalancer(fmt.Sprintf("%s-lb-%s", n.getNetworkPrefix(), listenAddress))
}

// getLogicalRouterPeerPortName returns OVN logical router port name to use for a peer connection.
func (n *ovn) getLogicalRouterPeerPortName(peerNetworkID int64) networkOVN.OVNRouterPort {
	return networkOVN.OVNRouterPort(fmt.Sprintf("%s-lrp-peer-net%d", n.getRouterName(), peerNetworkID))
}

// setupUplinkPort initializes the uplink connection. Returns the derived ovnUplinkVars settings used
// during the initial creation of the logical network.
func (n *ovn) setupUplinkPort(routerMAC net.HardwareAddr) (*ovnUplinkVars, error) {
	if n.config["network"] == "none" {
		return nil, nil
	}

	// Uplink network must be in default project.
	uplinkNet, err := LoadByName(n.state, api.ProjectDefaultName, n.config["network"])
	if err != nil {
		return nil, fmt.Errorf("Failed loading uplink network %q: %w", n.config["network"], err)
	}

	switch uplinkNet.Type() {
	case "bridge":
		return n.setupUplinkPortBridge(uplinkNet, routerMAC)
	case "physical":
		return n.setupUplinkPortPhysical(uplinkNet, routerMAC)
	}

	return nil, fmt.Errorf("Failed setting up uplink port, network type %q unsupported as OVN uplink", uplinkNet.Type())
}

// setupUplinkPortBridge allocates external IPs on the uplink bridge.
// Returns the derived ovnUplinkVars settings.
func (n *ovn) setupUplinkPortBridge(uplinkNet Network, routerMAC net.HardwareAddr) (*ovnUplinkVars, error) {
	bridgeNet, ok := uplinkNet.(*bridge)
	if !ok {
		return nil, errors.New("Network is not bridge type")
	}

	err := bridgeNet.checkClusterWideMACSafe(bridgeNet.config)
	if err != nil {
		return nil, fmt.Errorf("Network %q is not suitable for use as OVN uplink: %w", bridgeNet.name, err)
	}

	v, err := n.allocateUplinkPortIPs(uplinkNet, routerMAC)
	if err != nil {
		return nil, fmt.Errorf("Failed allocating uplink port IPs on network %q: %w", uplinkNet.Name(), err)
	}

	return v, nil
}

// setupUplinkPortPhysical allocates external IPs on the uplink network.
// Returns the derived ovnUplinkVars settings.
func (n *ovn) setupUplinkPortPhysical(uplinkNet Network, routerMAC net.HardwareAddr) (*ovnUplinkVars, error) {
	v, err := n.allocateUplinkPortIPs(uplinkNet, routerMAC)
	if err != nil {
		return nil, fmt.Errorf("Failed allocating uplink port IPs on network %q: %w", uplinkNet.Name(), err)
	}

	return v, nil
}

// allocateUplinkPortIPs attempts to find a free IP in the uplink network's OVN ranges and then stores it in
// ovnVolatileUplinkIPv4 and ovnVolatileUplinkIPv6 config keys on this network. Returns ovnUplinkVars settings.
func (n *ovn) allocateUplinkPortIPs(uplinkNet Network, routerMAC net.HardwareAddr) (*ovnUplinkVars, error) {
	v := &ovnUplinkVars{}

	uplinkNetConf := uplinkNet.Config()

	// Uplink derived settings.
	v.extSwitchProviderName = uplinkNet.Name()

	// Detect uplink gateway setting.
	uplinkIPv4CIDR := uplinkNetConf["ipv4.address"]
	if uplinkIPv4CIDR == "" {
		uplinkIPv4CIDR = uplinkNetConf["ipv4.gateway"]
	}

	uplinkIPv6CIDR := uplinkNetConf["ipv6.address"]
	if uplinkIPv6CIDR == "" {
		uplinkIPv6CIDR = uplinkNetConf["ipv6.gateway"]
	}

	// Optional uplink values.
	uplinkIPv4, uplinkIPv4Net, err := net.ParseCIDR(uplinkIPv4CIDR)
	if err == nil {
		v.dnsIPv4 = []net.IP{uplinkIPv4}
		v.routerExtGwIPv4 = uplinkIPv4
	}

	uplinkIPv6, uplinkIPv6Net, err := net.ParseCIDR(uplinkIPv6CIDR)
	if err == nil {
		v.dnsIPv6 = []net.IP{uplinkIPv6}
		v.routerExtGwIPv6 = uplinkIPv6
	}

	// Detect optional DNS server list.
	if uplinkNetConf["dns.nameservers"] != "" {
		// Reset nameservers.
		v.dnsIPv4 = nil
		v.dnsIPv6 = nil

		nsList := util.SplitNTrimSpace(uplinkNetConf["dns.nameservers"], ",", -1, false)
		for _, ns := range nsList {
			nsIP := net.ParseIP(ns)
			if nsIP == nil {
				return nil, errors.New("Invalid uplink nameserver")
			}

			if nsIP.To4() == nil {
				v.dnsIPv6 = append(v.dnsIPv6, nsIP)
			} else {
				v.dnsIPv4 = append(v.dnsIPv4, nsIP)
			}
		}
	}

	// Parse existing allocated IPs for this network on the uplink network (if not set yet, will be nil).
	routerExtPortIPv4 := net.ParseIP(n.config[ovnVolatileUplinkIPv4])
	routerExtPortIPv6 := net.ParseIP(n.config[ovnVolatileUplinkIPv6])

	// Check if uplink is viable at all.
	if uplinkIPv4Net == nil && uplinkIPv6Net == nil {
		return nil, errors.New("Uplink network doesn't have IPv4 or IPv6 configured")
	}

	// Decide whether we need to allocate new IP(s) and go to the expense of retrieving all allocated IPs.
	if (uplinkIPv4Net != nil && routerExtPortIPv4 == nil) || (uplinkIPv6Net != nil && routerExtPortIPv6 == nil) {
		err := n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
			allAllocatedIPv4, allAllocatedIPv6, err := n.uplinkAllAllocatedIPs(ctx, tx, uplinkNet.Name())
			if err != nil {
				return fmt.Errorf("Failed to get all allocated IPs for uplink: %w", err)
			}

			if uplinkIPv4Net != nil && routerExtPortIPv4 == nil {
				if uplinkNetConf["ipv4.ovn.ranges"] == "" {
					return errors.New(`Missing required "ipv4.ovn.ranges" config key on uplink network`)
				}

				dhcpSubnet := uplinkNet.DHCPv4Subnet()
				allowedNets := []*net.IPNet{}

				if dhcpSubnet != nil {
					allowedNets = append(allowedNets, dhcpSubnet)
				} else {
					allowedNets = append(allowedNets, uplinkIPv4Net)
				}

				ipRanges, err := parseIPRanges(uplinkNetConf["ipv4.ovn.ranges"], allowedNets...)
				if err != nil {
					return fmt.Errorf("Failed to parse uplink IPv4 OVN ranges: %w", err)
				}

				routerExtPortIPv4, err = n.uplinkAllocateIP(ipRanges, allAllocatedIPv4)
				if err != nil {
					return fmt.Errorf("Failed to allocate uplink IPv4 address: %w", err)
				}

				n.config[ovnVolatileUplinkIPv4] = routerExtPortIPv4.String()
			}

			if uplinkIPv6Net != nil && routerExtPortIPv6 == nil {
				// If IPv6 OVN ranges are specified by the uplink, allocate from them.
				if uplinkNetConf["ipv6.ovn.ranges"] != "" {
					dhcpSubnet := uplinkNet.DHCPv6Subnet()
					allowedNets := []*net.IPNet{}

					if dhcpSubnet != nil {
						allowedNets = append(allowedNets, dhcpSubnet)
					} else {
						allowedNets = append(allowedNets, uplinkIPv6Net)
					}

					ipRanges, err := parseIPRanges(uplinkNetConf["ipv6.ovn.ranges"], allowedNets...)
					if err != nil {
						return fmt.Errorf("Failed to parse uplink IPv6 OVN ranges: %w", err)
					}

					routerExtPortIPv6, err = n.uplinkAllocateIP(ipRanges, allAllocatedIPv6)
					if err != nil {
						return fmt.Errorf("Failed to allocate uplink IPv6 address: %w", err)
					}
				} else {
					// Otherwise use EUI64 derived from MAC address.
					routerExtPortIPv6, err = eui64.ParseMAC(uplinkIPv6Net.IP, routerMAC)
					if err != nil {
						return err
					}
				}

				n.config[ovnVolatileUplinkIPv6] = routerExtPortIPv6.String()
			}

			err = tx.UpdateNetwork(ctx, n.project, n.name, n.description, n.config)
			if err != nil {
				return fmt.Errorf("Failed saving allocated uplink network IPs: %w", err)
			}

			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	// Configure variables needed to configure OVN router.
	if uplinkIPv4Net != nil && routerExtPortIPv4 != nil {
		routerExtPortIPv4Net := &net.IPNet{
			Mask: uplinkIPv4Net.Mask,
			IP:   routerExtPortIPv4,
		}

		v.routerExtPortIPv4Net = routerExtPortIPv4Net.String()
	}

	if uplinkIPv6Net != nil {
		routerExtPortIPv6Net := &net.IPNet{
			Mask: uplinkIPv6Net.Mask,
			IP:   routerExtPortIPv6,
		}

		v.routerExtPortIPv6Net = routerExtPortIPv6Net.String()
	}

	return v, nil
}

// uplinkAllAllocatedIPs gets a list of all IPv4 and IPv6 addresses allocated to OVN networks connected to uplink.
func (n *ovn) uplinkAllAllocatedIPs(ctx context.Context, tx *db.ClusterTx, uplinkNetName string) ([]net.IP, []net.IP, error) {
	// Get all managed networks across all projects.
	projectNetworks, err := tx.GetCreatedNetworks(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("Failed to load all networks: %w", err)
	}

	v4IPs := make([]net.IP, 0)
	v6IPs := make([]net.IP, 0)

	for _, networks := range projectNetworks {
		for _, netInfo := range networks {
			if netInfo.Type != "ovn" || netInfo.Config["network"] != uplinkNetName {
				continue
			}

			for _, k := range []string{ovnVolatileUplinkIPv4, ovnVolatileUplinkIPv6} {
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
	}

	return v4IPs, v6IPs, nil
}

// uplinkAllocateIP allocates a free IP from one of the IP ranges.
func (n *ovn) uplinkAllocateIP(ipRanges []*iprange.Range, allAllocated []net.IP) (net.IP, error) {
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
			if slices.ContainsFunc(allAllocated, ip.Equal) {
				freeIP = false
			}

			if !freeIP {
				startBig.Add(startBig, inc)
				continue
			}

			return ip, nil
		}
	}

	return nil, errors.New("No free IPs available")
}

// startUplinkPort performs any network start up logic needed to connect the uplink connection to OVN.
func (n *ovn) startUplinkPort() error {
	if n.config["network"] == "none" {
		return nil
	}

	// Uplink network must be in default project.
	uplinkNet, err := LoadByName(n.state, api.ProjectDefaultName, n.config["network"])
	if err != nil {
		return fmt.Errorf("Failed loading uplink network %q: %w", n.config["network"], err)
	}

	// Uplink will not do anything 
	if n.config["parent"] == "none" && uplinkNet.Type() == "physical"{
		return nil
	}

	// Lock uplink network so that if multiple OVN networks are trying to connect to the same uplink we don't
	// race each other setting up the connection.
	unlock, err := locking.Lock(context.TODO(), n.uplinkOperationLockName(uplinkNet))
	if err != nil {
		return err
	}

	defer unlock()

	switch uplinkNet.Type() {
	case "bridge":
		return n.startUplinkPortBridge(uplinkNet)
	case "physical":
		return n.startUplinkPortPhysical(uplinkNet)
	}

	return fmt.Errorf("Failed starting uplink port, network type %q unsupported as OVN uplink", uplinkNet.Type())
}

// uplinkOperationLockName returns the lock name to use for operations on the uplink network.
func (n *ovn) uplinkOperationLockName(uplinkNet Network) string {
	return fmt.Sprintf("network.ovn.%s", uplinkNet.Name())
}

// uplinkPortBridgeVars returns the uplink port bridge variables needed for port start/stop.
func (n *ovn) uplinkPortBridgeVars(uplinkNet Network) *ovnUplinkPortBridgeVars {
	ovsBridge := fmt.Sprintf("incusovn%d", uplinkNet.ID())

	return &ovnUplinkPortBridgeVars{
		ovsBridge: ovsBridge,
		uplinkEnd: fmt.Sprintf("%sa", ovsBridge),
		ovsEnd:    fmt.Sprintf("%sb", ovsBridge),
	}
}

// startUplinkPortBridge creates veth pair (if doesn't exist), creates OVS bridge (if doesn't exist) and
// connects veth pair to uplink bridge and OVS bridge.
func (n *ovn) startUplinkPortBridge(uplinkNet Network) error {
	if uplinkNet.Config()["bridge.driver"] != "openvswitch" {
		return n.startUplinkPortBridgeNative(uplinkNet, uplinkNet.Name())
	}

	return n.startUplinkPortBridgeOVS(uplinkNet, uplinkNet.Name())
}

// startUplinkPortBridgeNative connects an OVN logical router to an uplink native bridge.
func (n *ovn) startUplinkPortBridgeNative(uplinkNet Network, bridgeDevice string) error {
	// Do this after gaining lock so that on failure we revert before release locking.
	reverter := revert.New()
	defer reverter.Fail()

	// If uplink is a native bridge, then use a separate OVS bridge with veth pair connection to native bridge.
	vars := n.uplinkPortBridgeVars(uplinkNet)

	// Create veth pair if needed.
	if !InterfaceExists(vars.uplinkEnd) && !InterfaceExists(vars.ovsEnd) {
		veth := &ip.Veth{
			Link: ip.Link{
				Name: vars.uplinkEnd,
			},
			Peer: ip.Link{
				Name: vars.ovsEnd,
			},
		}

		err := veth.Add()
		if err != nil {
			return fmt.Errorf("Failed to create the uplink veth interfaces %q and %q: %w", vars.uplinkEnd, vars.ovsEnd, err)
		}

		reverter.Add(func() { _ = veth.Delete() })
	}

	// Ensure that the veth interfaces inherit the uplink bridge's MTU (which the OVS bridge also inherits).
	uplinkNetConfig := uplinkNet.Config()

	// Uplink may have type "bridge" or "physical"
	uplinkNetMTU, hasBridgeMTU := uplinkNetConfig["bridge.mtu"]
	if !hasBridgeMTU {
		uplinkNetMTU = uplinkNetConfig["mtu"]
	}

	if uplinkNetMTU != "" {
		mtu, err := strconv.ParseUint(uplinkNetMTU, 10, 32)
		if err != nil {
			return fmt.Errorf("Invalid uplink MTU %q: %w", uplinkNetMTU, err)
		}

		uplinkEndLink := &ip.Link{Name: vars.uplinkEnd}
		err = uplinkEndLink.SetMTU(uint32(mtu))
		if err != nil {
			return fmt.Errorf("Failed setting MTU %q on %q: %w", uplinkNetMTU, uplinkEndLink.Name, err)
		}

		ovsEndLink := &ip.Link{Name: vars.ovsEnd}
		err = ovsEndLink.SetMTU(uint32(mtu))
		if err != nil {
			return fmt.Errorf("Failed setting MTU %q on %q: %w", uplinkNetMTU, ovsEndLink.Name, err)
		}
	}

	// Ensure correct sysctls are set on uplink veth interfaces to avoid getting IPv6 link-local addresses.
	if util.PathExists("/proc/sys/net/ipv6") {
		err := localUtil.SysctlSet(
			fmt.Sprintf("net/ipv6/conf/%s/disable_ipv6", vars.uplinkEnd), "1",
			fmt.Sprintf("net/ipv6/conf/%s/disable_ipv6", vars.ovsEnd), "1",
			fmt.Sprintf("net/ipv6/conf/%s/forwarding", vars.uplinkEnd), "0",
			fmt.Sprintf("net/ipv6/conf/%s/forwarding", vars.ovsEnd), "0",
		)
		if err != nil {
			return fmt.Errorf("Failed to configure uplink veth interfaces %q and %q: %w", vars.uplinkEnd, vars.ovsEnd, err)
		}
	}

	// Connect uplink end of veth pair to uplink bridge and bring up.
	link := &ip.Link{Name: vars.uplinkEnd}
	err := link.SetMaster(bridgeDevice)
	if err != nil {
		return fmt.Errorf("Failed to connect uplink veth interface %q to uplink bridge %q: %w", vars.uplinkEnd, bridgeDevice, err)
	}

	link = &ip.Link{Name: vars.uplinkEnd}
	err = link.SetUp()
	if err != nil {
		return fmt.Errorf("Failed to bring up uplink veth interface %q: %w", vars.uplinkEnd, err)
	}

	// Ensure uplink OVS end veth interface is up.
	link = &ip.Link{Name: vars.ovsEnd}
	err = link.SetUp()
	if err != nil {
		return fmt.Errorf("Failed to bring up uplink veth interface %q: %w", vars.ovsEnd, err)
	}

	// Create uplink OVS bridge if needed.
	vswitch, err := n.state.OVS()
	if err != nil {
		return fmt.Errorf("Failed to connect to OVS: %w", err)
	}

	err = vswitch.CreateBridge(context.TODO(), vars.ovsBridge, true, nil, 0)
	if err != nil {
		return fmt.Errorf("Failed to create uplink OVS bridge %q: %w", vars.ovsBridge, err)
	}

	// Connect OVS end veth interface to OVS bridge.
	err = vswitch.CreateBridgePort(context.TODO(), vars.ovsBridge, vars.ovsEnd, true)
	if err != nil {
		return fmt.Errorf("Failed to connect uplink veth interface %q to uplink OVS bridge %q: %w", vars.ovsEnd, vars.ovsBridge, err)
	}

	// Associate OVS bridge to logical OVN provider.
	err = vswitch.AddOVNBridgeMapping(context.TODO(), vars.ovsBridge, uplinkNet.Name())
	if err != nil {
		return fmt.Errorf("Failed to associate uplink OVS bridge %q to OVN provider %q: %w", vars.ovsBridge, uplinkNet.Name(), err)
	}

	// Attempt to learn uplink MAC.
	n.pingOVNRouter()

	reverter.Success()

	return nil
}

// startUplinkPortBridgeOVS connects an OVN logical router to an uplink OVS bridge.
func (n *ovn) startUplinkPortBridgeOVS(uplinkNet Network, bridgeDevice string) error {
	// Do this after gaining lock so that on failure we revert before release locking.
	reverter := revert.New()
	defer reverter.Fail()

	// If uplink is an openvswitch bridge, have OVN logical provider connect directly to it.
	vswitch, err := n.state.OVS()
	if err != nil {
		return fmt.Errorf("Failed to connect to OVS: %w", err)
	}

	err = vswitch.AddOVNBridgeMapping(context.TODO(), bridgeDevice, uplinkNet.Name())
	if err != nil {
		return fmt.Errorf("Failed to associate uplink OVS bridge %q to OVN provider %q: %w", bridgeDevice, uplinkNet.Name(), err)
	}

	// Attempt to learn uplink MAC.
	n.pingOVNRouter()

	reverter.Success()

	return nil
}

// pingOVNRouter pings the OVN router's external IP addresses to attempt to trigger MAC learning on uplink.
// This is to work around a bug in some versions of OVN.
func (n *ovn) pingOVNRouter() {
	var ips []net.IP

	for _, key := range []string{ovnVolatileUplinkIPv4, ovnVolatileUplinkIPv6} {
		ip := net.ParseIP(n.config[key])
		if ip != nil {
			ips = append(ips, ip)
		}
	}

	for i := range ips {
		ip := ips[i] // Local var

		// Now that the OVN router is connected to the uplink bridge, attempt to ping the OVN
		// router's external IPv6 from the host running the uplink bridge in an attempt to trigger the
		// OVN router to learn the uplink gateway's MAC address. This is to work around a bug in
		// older versions of OVN that meant that the OVN router would not attempt to learn the external
		// uplink IPv6 gateway MAC address when using SNAT, meaning that external IPv6 connectivity
		// wouldn't work until the next router advertisement was sent (which could be several minutes).
		// By pinging the OVN router's external IP this will trigger an NDP request from the uplink bridge
		// which will cause the OVN router to learn its MAC address.
		go func() {
			var err error

			// Try several attempts as it can take a few seconds for the network to come up.
			for range 5 {
				err = pingIP(context.TODO(), ip)
				if err == nil {
					n.logger.Debug("OVN router external IP address reachable", logger.Ctx{"ip": ip.String()})
					return
				}

				time.Sleep(time.Second)
			}

			// We would expect this on a chassis node that isn't the active router gateway, it doesn't
			// always indicate a problem.
			n.logger.Debug("OVN router external IP address unreachable", logger.Ctx{"ip": ip.String(), "err": err})
		}()
	}
}

// startUplinkPortPhysical creates OVS bridge (if doesn't exist) and connects uplink interface to the OVS bridge.
func (n *ovn) startUplinkPortPhysical(uplinkNet Network) error {
	// Do this after gaining lock so that on failure we revert before release locking.
	reverter := revert.New()
	defer reverter.Fail()

	uplinkConfig := uplinkNet.Config()
	uplinkHostName := GetHostDevice(uplinkConfig["parent"], uplinkConfig["vlan"])

	if !InterfaceExists(uplinkHostName) {
		return fmt.Errorf("Uplink network %q is not started (interface %q is missing)", uplinkNet.Name(), uplinkHostName)
	}

	// Detect if uplink interface is a native bridge.
	if IsNativeBridge(uplinkHostName) {
		return n.startUplinkPortBridgeNative(uplinkNet, uplinkHostName)
	}

	// Detect if uplink interface is a OVS bridge.
	vswitch, err := n.state.OVS()
	if err != nil {
		return fmt.Errorf("Failed to connect to OVS: %w", err)
	}

	_, err = vswitch.GetBridge(context.TODO(), uplinkHostName)
	if err != nil && !errors.Is(err, ovs.ErrNotFound) {
		return err
	} else if err == nil {
		return n.startUplinkPortBridgeOVS(uplinkNet, uplinkHostName)
	}

	// If uplink is a normal physical interface, then use a separate OVS bridge and connect uplink to it.
	vars := n.uplinkPortBridgeVars(uplinkNet)

	// Check no global unicast IPs defined on uplink, as that may indicate it is in use by another application.
	addresses, _, err := InterfaceStatus(uplinkHostName)
	if err != nil {
		return fmt.Errorf("Failed getting interface status for %q: %w", uplinkHostName, err)
	}

	if len(addresses) > 0 {
		return fmt.Errorf("Cannot start network as uplink network interface %q has one or more IP addresses configured on it", uplinkHostName)
	}

	// Ensure correct sysctls are set on uplink interface to avoid getting IPv6 link-local addresses.
	err = localUtil.SysctlSet(
		fmt.Sprintf("net/ipv6/conf/%s/disable_ipv6", uplinkHostName), "1",
		fmt.Sprintf("net/ipv6/conf/%s/forwarding", uplinkHostName), "0",
	)
	if err != nil {
		return fmt.Errorf("Failed to configure uplink interface %q: %w", uplinkHostName, err)
	}

	// Create uplink OVS bridge if needed.
	err = vswitch.CreateBridge(context.TODO(), vars.ovsBridge, true, nil, 0)
	if err != nil {
		return fmt.Errorf("Failed to create uplink OVS bridge %q: %w", vars.ovsBridge, err)
	}

	// Connect OVS end veth interface to OVS bridge.
	err = vswitch.CreateBridgePort(context.TODO(), vars.ovsBridge, uplinkHostName, true)
	if err != nil {
		return fmt.Errorf("Failed to connect uplink interface %q to uplink OVS bridge %q: %w", uplinkHostName, vars.ovsBridge, err)
	}

	// Associate OVS bridge to logical OVN provider.
	err = vswitch.AddOVNBridgeMapping(context.TODO(), vars.ovsBridge, uplinkNet.Name())
	if err != nil {
		return fmt.Errorf("Failed to associate uplink OVS bridge %q to OVN provider %q: %w", vars.ovsBridge, uplinkNet.Name(), err)
	}

	// Bring uplink interface up.
	link := &ip.Link{Name: uplinkHostName}
	err = link.SetUp()
	if err != nil {
		return fmt.Errorf("Failed to bring up uplink interface %q: %w", uplinkHostName, err)
	}

	// Attempt to learn uplink MAC.
	n.pingOVNRouter()

	reverter.Success()

	return nil
}

// checkUplinkUse checks if uplink network is used by another OVN network.
func (n *ovn) checkUplinkUse() (bool, error) {
	if n.config["network"] == "none" {
		return false, nil
	}

	// Get all managed networks across all projects.
	var err error
	var projectNetworks map[string]map[int64]api.Network

	err = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		projectNetworks, err = tx.GetCreatedNetworks(ctx)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("Failed to load all networks: %w", err)
	}

	for projectName, networks := range projectNetworks {
		for _, network := range networks {
			if (projectName == n.project && network.Name == n.name) || network.Type != "ovn" {
				continue // Ignore our own DB record or non OVN networks.
			}

			// Check if another network is using our uplink.
			if network.Config["network"] == n.config["network"] {
				return true, nil
			}
		}
	}

	return false, nil
}

// deleteUplinkPort deletes the uplink connection.
func (n *ovn) deleteUplinkPort() error {
	if n.config["network"] == "none" {
		return nil
	}

	// Uplink network must be in default project.
	if n.config["network"] != "" {
		uplinkNet, err := LoadByName(n.state, api.ProjectDefaultName, n.config["network"])
		if err != nil {
			return fmt.Errorf("Failed loading uplink network %q: %w", n.config["network"], err)
		}

		// Lock uplink network so we don't race each other networks using the OVS uplink bridge.
		unlock, err := locking.Lock(context.TODO(), n.uplinkOperationLockName(uplinkNet))
		if err != nil {
			return err
		}

		defer unlock()

		switch uplinkNet.Type() {
		case "bridge":
			return n.deleteUplinkPortBridge(uplinkNet)
		case "physical":
			return n.deleteUplinkPortPhysical(uplinkNet)
		}

		return fmt.Errorf("Failed deleting uplink port, network type %q unsupported as OVN uplink", uplinkNet.Type())
	}

	return nil
}

// deleteUplinkPortBridge disconnects the uplink port from the bridge and performs any cleanup.
func (n *ovn) deleteUplinkPortBridge(uplinkNet Network) error {
	if uplinkNet.Config()["bridge.driver"] != "openvswitch" {
		return n.deleteUplinkPortBridgeNative(uplinkNet)
	}

	return n.deleteUplinkPortBridgeOVS(uplinkNet, uplinkNet.Name())
}

// deleteUplinkPortBridge deletes uplink OVS bridge, OVN bridge mappings and veth interfaces if not in use.
func (n *ovn) deleteUplinkPortBridgeNative(uplinkNet Network) error {
	// Check OVS uplink bridge exists, if it does, check whether the uplink network is in use.
	removeVeths := false
	vars := n.uplinkPortBridgeVars(uplinkNet)
	if InterfaceExists(vars.ovsBridge) {
		uplinkUsed, err := n.checkUplinkUse()
		if err != nil {
			return err
		}

		// Remove OVS bridge if the uplink network isn't used by any other OVN networks.
		if !uplinkUsed {
			removeVeths = true

			vswitch, err := n.state.OVS()
			if err != nil {
				return fmt.Errorf("Failed to connect to OVS: %w", err)
			}

			err = vswitch.RemoveOVNBridgeMapping(context.TODO(), vars.ovsBridge, uplinkNet.Name())
			if err != nil {
				return err
			}

			err = vswitch.DeleteBridge(context.TODO(), vars.ovsBridge)
			if err != nil {
				return err
			}
		}
	} else {
		removeVeths = true // Remove the veths if OVS bridge already gone.
	}

	// Remove the veth interfaces if they exist.
	if removeVeths {
		if InterfaceExists(vars.uplinkEnd) {
			link := &ip.Link{Name: vars.uplinkEnd}
			err := link.Delete()
			if err != nil {
				return fmt.Errorf("Failed to delete the uplink veth interface %q: %w", vars.uplinkEnd, err)
			}
		}

		if InterfaceExists(vars.ovsEnd) {
			link := &ip.Link{Name: vars.ovsEnd}
			err := link.Delete()
			if err != nil {
				return fmt.Errorf("Failed to delete the uplink veth interface %q: %w", vars.ovsEnd, err)
			}
		}
	}

	return nil
}

// deleteUplinkPortBridge deletes OVN bridge mappings if not in use.
func (n *ovn) deleteUplinkPortBridgeOVS(uplinkNet Network, ovsBridge string) error {
	uplinkUsed, err := n.checkUplinkUse()
	if err != nil {
		return err
	}

	// Remove uplink OVS bridge mapping if not in use by other OVN networks.
	if !uplinkUsed {
		vswitch, err := n.state.OVS()
		if err != nil {
			return fmt.Errorf("Failed to connect to OVS: %w", err)
		}

		err = vswitch.RemoveOVNBridgeMapping(context.TODO(), ovsBridge, uplinkNet.Name())
		if err != nil {
			return err
		}
	}

	return nil
}

// deleteUplinkPortPhysical deletes uplink OVS bridge and OVN bridge mappings if not in use.
func (n *ovn) deleteUplinkPortPhysical(uplinkNet Network) error {
	uplinkConfig := uplinkNet.Config()
	uplinkHostName := GetHostDevice(uplinkConfig["parent"], uplinkConfig["vlan"])

	// Detect if uplink interface is a native bridge.
	if IsNativeBridge(uplinkHostName) {
		return n.deleteUplinkPortBridgeNative(uplinkNet)
	}

	// Detect if uplink interface is a OVS bridge.
	vswitch, err := n.state.OVS()
	if err != nil {
		return fmt.Errorf("Failed to connect to OVS: %w", err)
	}

	_, err = vswitch.GetBridge(context.TODO(), uplinkHostName)
	if err != nil && !errors.Is(err, ovs.ErrNotFound) {
		return err
	} else if err == nil {
		return n.deleteUplinkPortBridgeOVS(uplinkNet, uplinkHostName)
	}

	// Otherwise if uplink is normal physical interface, attempt cleanup of OVS bridge.

	// Check OVS uplink bridge exists, if it does, check whether the uplink network is in use.
	releaseIF := false
	vars := n.uplinkPortBridgeVars(uplinkNet)
	if InterfaceExists(vars.ovsBridge) {
		uplinkUsed, err := n.checkUplinkUse()
		if err != nil {
			return err
		}

		// Remove OVS bridge if the uplink network isn't used by any other OVN networks.
		if !uplinkUsed {
			releaseIF = true

			err = vswitch.RemoveOVNBridgeMapping(context.TODO(), vars.ovsBridge, uplinkNet.Name())
			if err != nil {
				return err
			}

			err = vswitch.DeleteBridge(context.TODO(), vars.ovsBridge)
			if err != nil {
				return err
			}
		}
	} else {
		releaseIF = true // Bring uplink interface down if not needed.
	}

	// Bring down uplink interface if not used and exists.
	if releaseIF && InterfaceExists(uplinkHostName) {
		link := &ip.Link{Name: uplinkHostName}
		err := link.SetDown()
		if err != nil {
			return fmt.Errorf("Failed to bring down uplink interface %q: %w", uplinkHostName, err)
		}
	}

	return nil
}

// FillConfig fills requested config with any default values.
func (n *ovn) FillConfig(config map[string]string) error {
	if config["ipv4.address"] == "" {
		config["ipv4.address"] = "auto"
	}

	if config["ipv6.address"] == "" {
		content, err := os.ReadFile("/proc/sys/net/ipv6/conf/default/disable_ipv6")
		if err == nil && string(content) == "0\n" {
			config["ipv6.address"] = "auto"
		}
	}

	// Now replace any "auto" keys with generated values.
	err := n.populateAutoConfig(config)
	if err != nil {
		return fmt.Errorf("Failed generating auto config: %w", err)
	}

	return nil
}

// populateAutoConfig replaces "auto" in config with generated values.
func (n *ovn) populateAutoConfig(config map[string]string) error {
	changedConfig := false

	if config["ipv4.address"] == "auto" {
		subnet, err := randomSubnetV4()
		if err != nil {
			return err
		}

		config["ipv4.address"] = subnet

		if config["ipv4.nat"] == "" {
			config["ipv4.nat"] = "true"
		}

		changedConfig = true
	}

	if config["ipv6.address"] == "auto" {
		subnet, err := randomSubnetV6()
		if err != nil {
			return err
		}

		config["ipv6.address"] = subnet

		if config["ipv6.nat"] == "" {
			config["ipv6.nat"] = "true"
		}

		changedConfig = true
	}

	// Re-validate config if changed.
	if changedConfig && n.state != nil {
		return n.Validate(config)
	}

	return nil
}

// Create sets up network in OVN Northbound database.
func (n *ovn) Create(clientType request.ClientType) error {
	n.logger.Debug("Create", logger.Ctx{"clientType": clientType, "config": n.config})

	// We only need to setup the OVN Northbound database once, not on every clustered node.
	if clientType == request.ClientTypeNormal {
		err := n.setup(false)
		if err != nil {
			return err
		}
	}

	return nil
}

// allowedUplinkNetworks returns a list of allowed networks to use as uplinks based on project restrictions.
func (n *ovn) allowedUplinkNetworks(p *api.Project) ([]string, error) {
	var uplinkNetworkNames []string

	err := n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		// Uplink networks are always from the default project.
		networks, err := tx.GetCreatedNetworksByProject(ctx, api.ProjectDefaultName)
		if err != nil {
			return fmt.Errorf("Failed getting uplink networks: %w", err)
		}

		// Add any compatible networks to the uplink network list.
		for _, network := range networks {
			if network.Type == "bridge" || network.Type == "physical" {
				uplinkNetworkNames = append(uplinkNetworkNames, network.Name)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// If project is not restricted, return full network list.
	if util.IsFalseOrEmpty(p.Config["restricted"]) {
		return uplinkNetworkNames, nil
	}

	allowedUplinkNetworkNames := []string{}

	// There are no allowed networks if restricted.networks.uplinks is not set.
	if p.Config["restricted.networks.uplinks"] == "" {
		return allowedUplinkNetworkNames, nil
	}

	// Parse the allowed uplinks and return any that are present in the actual defined networks.
	allowedRestrictedUplinks := util.SplitNTrimSpace(p.Config["restricted.networks.uplinks"], ",", -1, false)

	for _, allowedRestrictedUplink := range allowedRestrictedUplinks {
		if slices.Contains(uplinkNetworkNames, allowedRestrictedUplink) {
			allowedUplinkNetworkNames = append(allowedUplinkNetworkNames, allowedRestrictedUplink)
		}
	}

	return allowedUplinkNetworkNames, nil
}

// validateUplinkNetwork checks if uplink network is allowed, and if empty string is supplied then tries to select
// an uplink network from the allowedUplinkNetworks() list if there is only one allowed network.
// Returns chosen uplink network name to use.
func (n *ovn) validateUplinkNetwork(p *api.Project, uplinkNetworkName string) (string, error) {
	allowedUplinkNetworks, err := n.allowedUplinkNetworks(p)
	if err != nil {
		return "", err
	}

	if uplinkNetworkName != "" {
		if !slices.Contains(allowedUplinkNetworks, uplinkNetworkName) {
			return "", fmt.Errorf(`Option "network" value %q is not one of the allowed uplink networks in project`, uplinkNetworkName)
		}

		return uplinkNetworkName, nil
	}

	allowedNetworkCount := len(allowedUplinkNetworks)
	if allowedNetworkCount == 0 {
		return "", errors.New(`No allowed uplink networks in project`)
	} else if allowedNetworkCount == 1 {
		// If there is only one allowed uplink network then use it if not specified by user.
		return allowedUplinkNetworks[0], nil
	}

	return "", errors.New(`Option "network" is required`)
}

// getDHCPv4Reservations returns list DHCP IPv4 reservations from NICs connected to this network.
func (n *ovn) getDHCPv4Reservations() ([]iprange.Range, error) {
	routerIntPortIPv4, ipv4Net, err := n.parseRouterIntPortIPv4Net()
	if err != nil {
		return nil, fmt.Errorf("Failed parsing router's internal port IPv4 Net for DHCP reservation: %w", err)
	}

	var dhcpReserveIPv4s []iprange.Range
	if routerIntPortIPv4 != nil {
		if n.config["ipv4.dhcp.ranges"] == "" {
			dhcpReserveIPv4s = []iprange.Range{{Start: routerIntPortIPv4}, {Start: dhcpalloc.GetIP(ipv4Net, -2)}}
		} else {
			allowedNets := []*net.IPNet{n.DHCPv4Subnet()}
			dhcpRanges, err := parseIPRanges(n.config["ipv4.dhcp.ranges"], allowedNets...)
			if err != nil {
				return nil, err
			}

			sort.Slice(dhcpRanges, func(i, j int) bool {
				return bytes.Compare(dhcpRanges[i].Start, dhcpRanges[j].Start) < 0
			})

			reserverdIPs, err := complementRanges(dhcpRanges, ipv4Net)
			if err != nil {
				return nil, err
			}

			dhcpReserveIPv4s = append(dhcpReserveIPv4s, reserverdIPs...)

			if !ipInRanges(routerIntPortIPv4, dhcpReserveIPv4s) {
				dhcpReserveIPv4s = append(dhcpReserveIPv4s, iprange.Range{Start: routerIntPortIPv4})
			}

			// Convert the 4-byte IPv4 address returned by 'dhcpalloc.GetIP' to a 16-byte form
			// using net.ParseIP, to ensure compatibility with other IPs stored in 16-byte format.
			// This is necessary because direct comparison with 4-byte IPs would fail.
			ovnRouter := net.ParseIP(dhcpalloc.GetIP(ipv4Net, -2).String())
			if !ipInRanges(ovnRouter, dhcpReserveIPv4s) {
				dhcpReserveIPv4s = append(dhcpReserveIPv4s, iprange.Range{Start: ovnRouter})
			}
		}
	}

	err = UsedByInstanceDevices(n.state, n.Project(), n.Name(), n.Type(), func(inst db.InstanceArgs, nicName string, nicConfig map[string]string) error {
		ip := net.ParseIP(nicConfig["ipv4.address"])
		if ip != nil {
			if !ipInRanges(ip, dhcpReserveIPv4s) {
				dhcpReserveIPv4s = append(dhcpReserveIPv4s, iprange.Range{Start: ip})
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return dhcpReserveIPv4s, nil
}

func (n *ovn) setup(update bool) error {
	// If we are in mock mode, just no-op.
	if n.state.OS.MockMode {
		return nil
	}

	n.logger.Debug("Setting up network")

	reverter := revert.New()
	defer reverter.Fail()

	var routerExtPortIPv4, routerExtPortIPv6 net.IP
	var routerExtPortIPv4Net, routerExtPortIPv6Net *net.IPNet

	// Record updated config so we can store back into DB and n.config variable.
	updatedConfig := make(map[string]string)

	// Load the project to get uplink network restrictions.
	var p *api.Project
	var projectID int64
	err := n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		project, err := dbCluster.GetProject(ctx, tx.Tx(), n.project)
		if err != nil {
			return err
		}

		projectID = int64(project.ID)

		p, err = project.ToAPI(ctx, tx.Tx())

		return err
	})
	if err != nil {
		return fmt.Errorf("Failed to load network restrictions from project %q: %w", n.project, err)
	}

	// Check project restrictions and get uplink network to use.
	uplinkNetwork := "none"
	if n.config["network"] != "none" {
		uplinkNetwork, err = n.validateUplinkNetwork(p, n.config["network"])
		if err != nil {
			return err
		}
	}

	// Ensure automatically selected uplink network is saved into "network" key.
	if uplinkNetwork != n.config["network"] {
		updatedConfig["network"] = uplinkNetwork
	}

	// Get bridge MTU to use.
	bridgeMTU := n.getBridgeMTU()
	if bridgeMTU == 0 {
		// If no manual bridge MTU specified, derive it from the underlay network.
		bridgeMTU, err = n.getOptimalBridgeMTU()
		if err != nil {
			return fmt.Errorf("Failed getting optimal bridge MTU: %w", err)
		}

		// Save to config so the value can be read by instances connecting to network.
		updatedConfig["bridge.mtu"] = fmt.Sprintf("%d", bridgeMTU)
	}

	// Get a list of all NICs connected to this network that have static DHCP IPv4 reservations.
	dhcpReserveIPv4s, err := n.getDHCPv4Reservations()
	if err != nil {
		return fmt.Errorf("Failed getting DHCPv4 IP reservations: %w", err)
	}

	// Apply any config dynamically generated to the current config and store back to DB in single transaction.
	if len(updatedConfig) > 0 {
		maps.Copy(n.config, updatedConfig)

		err := n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
			err = tx.UpdateNetwork(ctx, n.project, n.name, n.description, n.config)
			if err != nil {
				return fmt.Errorf("Failed saving updated network config: %w", err)
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	// Get router MAC address.
	routerMAC, err := n.getRouterMAC()
	if err != nil {
		return err
	}

	// Setup uplink port (do this first to check uplink is suitable).
	uplinkNet, err := n.setupUplinkPort(routerMAC)
	if err != nil {
		return err
	}

	// Parse router IP config.
	if uplinkNet != nil && uplinkNet.routerExtPortIPv4Net != "" {
		routerExtPortIPv4, routerExtPortIPv4Net, err = net.ParseCIDR(uplinkNet.routerExtPortIPv4Net)
		if err != nil {
			return fmt.Errorf("Failed parsing router's external uplink port IPv4 Net: %w", err)
		}
	}

	if uplinkNet != nil && uplinkNet.routerExtPortIPv6Net != "" {
		routerExtPortIPv6, routerExtPortIPv6Net, err = net.ParseCIDR(uplinkNet.routerExtPortIPv6Net)
		if err != nil {
			return fmt.Errorf("Failed parsing router's external uplink port IPv6 Net: %w", err)
		}
	}

	routerIntPortIPv4, routerIntPortIPv4Net, err := n.parseRouterIntPortIPv4Net()
	if err != nil {
		return fmt.Errorf("Failed parsing router's internal port IPv4 Net: %w", err)
	}

	routerIntPortIPv6, routerIntPortIPv6Net, err := n.parseRouterIntPortIPv6Net()
	if err != nil {
		return fmt.Errorf("Failed parsing router's internal port IPv6 Net: %w", err)
	}

	if n.config["network"] != "none" && routerIntPortIPv4 == nil && routerIntPortIPv6 == nil {
		return errors.New("IPv4 or IPv6 subnets must be specified on a non-isolated OVN network")
	}

	// Create chassis group.
	err = n.ovnnb.CreateChassisGroup(context.TODO(), n.getChassisGroupName(), update)
	if err != nil {
		return err
	}

	if !update {
		reverter.Add(func() { _ = n.ovnnb.DeleteChassisGroup(context.TODO(), n.getChassisGroupName()) })
	}

	// Configure logical router.
	if routerIntPortIPv4 != nil || routerIntPortIPv6 != nil {
		// Create logical router.
		err = n.ovnnb.CreateLogicalRouter(context.TODO(), n.getRouterName(), update)
		if err != nil {
			return fmt.Errorf("Failed adding router: %w", err)
		}

		if !update {
			reverter.Add(func() { _ = n.ovnnb.DeleteLogicalRouter(context.TODO(), n.getRouterName()) })
		}
	} else {
		err := n.ovnnb.DeleteLogicalRouter(context.TODO(), n.getRouterName())
		if err != nil && !errors.Is(err, networkOVN.ErrNotFound) {
			return fmt.Errorf("Failed deleting router: %w", err)
		}
	}

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

	if len(extRouterIPs) > 0 {
		err = n.ovnnb.CreateLogicalSwitch(context.TODO(), n.getExtSwitchName(), update)
		if err != nil {
			return fmt.Errorf("Failed adding external switch: %w", err)
		}

		if !update {
			reverter.Add(func() { _ = n.ovnnb.DeleteLogicalSwitch(context.TODO(), n.getExtSwitchName()) })
		}

		// Create external router port.
		err = n.ovnnb.CreateLogicalRouterPort(context.TODO(), n.getRouterName(), n.getRouterExtPortName(), routerMAC, bridgeMTU, extRouterIPs, n.getChassisGroupName(), update)
		if err != nil {
			return fmt.Errorf("Failed adding external router port: %w", err)
		}

		if !update {
			reverter.Add(func() {
				_ = n.ovnnb.DeleteLogicalRouterPort(context.TODO(), n.getRouterName(), n.getRouterExtPortName())
			})
		}

		// Create external switch port and link to router port.
		err = n.ovnnb.CreateLogicalSwitchPort(context.TODO(), n.getExtSwitchName(), n.getExtSwitchRouterPortName(), nil, update)
		if err != nil {
			return fmt.Errorf("Failed adding external switch router port: %w", err)
		}

		if !update {
			reverter.Add(func() {
				_ = n.ovnnb.DeleteLogicalSwitchPort(context.TODO(), n.getExtSwitchName(), n.getExtSwitchRouterPortName())
			})
		}

		err = n.ovnnb.UpdateLogicalSwitchPortLinkRouter(context.TODO(), n.getExtSwitchRouterPortName(), n.getRouterExtPortName())
		if err != nil {
			return fmt.Errorf("Failed linking external router port to external switch port: %w", err)
		}

		// Create external switch port and link to external provider network.
		err = n.ovnnb.CreateLogicalSwitchPort(context.TODO(), n.getExtSwitchName(), n.getExtSwitchProviderPortName(), nil, update)
		if err != nil {
			return fmt.Errorf("Failed adding external switch provider port: %w", err)
		}

		if !update {
			reverter.Add(func() {
				_ = n.ovnnb.DeleteLogicalSwitchPort(context.TODO(), n.getExtSwitchName(), n.getExtSwitchProviderPortName())
			})
		}

		if uplinkNet != nil {
			err = n.ovnnb.UpdateLogicalSwitchPortLinkProviderNetwork(context.TODO(), n.getExtSwitchProviderPortName(), uplinkNet.extSwitchProviderName)
			if err != nil {
				return fmt.Errorf("Failed linking external switch provider port to external provider network: %w", err)
			}
		}

		// Remove any existing SNAT rules on update. As currently these are only defined from the network
		// config rather than from any instance NIC config, so we can re-create the active config below.
		if update {
			err = n.ovnnb.DeleteLogicalRouterNAT(context.TODO(), n.getRouterName(), "snat", true)
			if err != nil {
				return fmt.Errorf("Failed removing existing router SNAT rules: %w", err)
			}
		}

		// Add SNAT rules.
		if util.IsTrue(n.config["ipv4.nat"]) && routerIntPortIPv4Net != nil && routerExtPortIPv4 != nil {
			snatIP := routerExtPortIPv4

			if n.config["ipv4.nat.address"] != "" {
				snatIP = net.ParseIP(n.config["ipv4.nat.address"])
				if snatIP == nil {
					return fmt.Errorf("Failed parsing %q", "ipv4.nat.address")
				}
			}

			err = n.ovnnb.CreateLogicalRouterNAT(context.TODO(), n.getRouterName(), "snat", routerIntPortIPv4Net, snatIP, nil, false, update)
			if err != nil {
				return fmt.Errorf("Failed adding router IPv4 SNAT rule: %w", err)
			}
		}

		if util.IsTrue(n.config["ipv6.nat"]) && routerIntPortIPv6Net != nil && routerExtPortIPv6 != nil {
			snatIP := routerExtPortIPv6

			if n.config["ipv6.nat.address"] != "" {
				snatIP = net.ParseIP(n.config["ipv6.nat.address"])
				if snatIP == nil {
					return fmt.Errorf("Failed parsing %q", "ipv6.nat.address")
				}
			}

			err = n.ovnnb.CreateLogicalRouterNAT(context.TODO(), n.getRouterName(), "snat", routerIntPortIPv6Net, snatIP, nil, false, update)
			if err != nil {
				return fmt.Errorf("Failed adding router IPv6 SNAT rule: %w", err)
			}
		}

		// Check if uplink network states its gateway mac for static MAC binding.
		if uplinkNet != nil && n.config["network"] != "none" {
			// Load the uplink network.
			uplinkNetworkObj, err := LoadByName(n.state, api.ProjectDefaultName, n.config["network"])
			if err != nil {
				return fmt.Errorf("Failed loading uplink network %q: %w", n.config["network"], err)
			}

			uplinkConfig := uplinkNetworkObj.Config()

			// Handle IPv4 MAC.
			if uplinkConfig["ipv4.gateway.hwaddr"] != "" {
				// Set a static MAc binding for the gateway's IP and MAC.
				uplinkGatewayIP, _, err := net.ParseCIDR(uplinkConfig["ipv4.gateway"])
				if err != nil {
					return err
				}

				uplinkGatewayMAC, err := net.ParseMAC(uplinkConfig["ipv4.gateway.hwaddr"])
				if err != nil {
					return err
				}

				err = n.ovnnb.CreateStaticMACBinding(context.TODO(), n.getRouterExtPortName(), uplinkGatewayIP, uplinkGatewayMAC, true)
				if err != nil {
					return err
				}
			}

			// Handle IPv6 MAC.
			if uplinkConfig["ipv6.gateway.hwaddr"] != "" {
				// Set a static MAc binding for the gateway's IP and MAC.
				uplinkGatewayIP, _, err := net.ParseCIDR(uplinkConfig["ipv6.gateway"])
				if err != nil {
					return err
				}

				uplinkGatewayMAC, err := net.ParseMAC(uplinkConfig["ipv6.gateway.hwaddr"])
				if err != nil {
					return err
				}

				err = n.ovnnb.CreateStaticMACBinding(context.TODO(), n.getRouterExtPortName(), uplinkGatewayIP, uplinkGatewayMAC, true)
				if err != nil {
					return err
				}
			}

			// Clear any leftover MAC binding.
			err = n.ovnnb.DeleteStaticMACBindings(context.TODO(), n.getRouterExtPortName(), uplinkConfig["ipv4.gateway.hwaddr"] == "", uplinkConfig["ipv6.gateway.hwaddr"] == "")
			if err != nil {
				return err
			}
		}

		// Clear default routes (if existing) and re-apply based on current config.
		defaultIPv4Route := net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}
		defaultIPv6Route := net.IPNet{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)}
		deleteRoutes := []net.IPNet{defaultIPv4Route, defaultIPv6Route}
		defaultRoutes := make([]networkOVN.OVNRouterRoute, 0, 2)

		if routerIntPortIPv4Net != nil {
			// If l3only mode is enabled then each instance IPv4 will get its own /32 route added when
			// the instance NIC starts. However to stop packets toward unknown IPs within the internal
			// subnet escaping onto the uplink network we add a less specific discard route for the
			// whole internal subnet.
			if util.IsTrue(n.config["ipv4.l3only"]) {
				defaultRoutes = append(defaultRoutes, networkOVN.OVNRouterRoute{
					Prefix:  *routerIntPortIPv4Net,
					Discard: true,
				})
			} else {
				deleteRoutes = append(deleteRoutes, *routerIntPortIPv4Net)
			}
		}

		if routerIntPortIPv6Net != nil {
			// If l3only mode is enabled then each instance IPv6 will get its own /128 route added when
			// the instance NIC starts. However to stop packets toward unknown IPs within the internal
			// subnet escaping onto the uplink network we add a less specific discard route for the
			// whole internal subnet.
			if util.IsTrue(n.config["ipv6.l3only"]) {
				defaultRoutes = append(defaultRoutes, networkOVN.OVNRouterRoute{
					Prefix:  *routerIntPortIPv6Net,
					Discard: true,
				})
			} else {
				deleteRoutes = append(deleteRoutes, *routerIntPortIPv6Net)
			}
		}

		if uplinkNet.routerExtGwIPv4 != nil {
			defaultRoutes = append(defaultRoutes, networkOVN.OVNRouterRoute{
				Prefix:  defaultIPv4Route,
				NextHop: uplinkNet.routerExtGwIPv4,
				Port:    n.getRouterExtPortName(),
			})
		}

		if uplinkNet.routerExtGwIPv6 != nil {
			defaultRoutes = append(defaultRoutes, networkOVN.OVNRouterRoute{
				Prefix:  defaultIPv6Route,
				NextHop: uplinkNet.routerExtGwIPv6,
				Port:    n.getRouterExtPortName(),
			})
		}

		if len(deleteRoutes) > 0 {
			err = n.ovnnb.DeleteLogicalRouterRoute(context.TODO(), n.getRouterName(), deleteRoutes...)
			if err != nil {
				return fmt.Errorf("Failed removing default routes: %w", err)
			}
		}

		if len(defaultRoutes) > 0 {
			err = n.ovnnb.CreateLogicalRouterRoute(context.TODO(), n.getRouterName(), update, defaultRoutes...)
			if err != nil {
				return fmt.Errorf("Failed adding default routes: %w", err)
			}
		}
	}

	// Gather internal router port IPs (in CIDR format).
	intRouterIPs := []*net.IPNet{}
	intSubnets := []net.IPNet{}

	if routerIntPortIPv4Net != nil {
		intRouterIPNet := &net.IPNet{
			IP:   routerIntPortIPv4,
			Mask: routerIntPortIPv4Net.Mask,
		}

		// In l3only mode the router's internal IP has a /32 mask instead of the internal subnet's mask.
		if util.IsTrue(n.config["ipv4.l3only"]) {
			intRouterIPNet.Mask = net.CIDRMask(32, 32)
		}

		intRouterIPs = append(intRouterIPs, intRouterIPNet)
		intSubnets = append(intSubnets, *routerIntPortIPv4Net)
	}

	if routerIntPortIPv6Net != nil {
		intRouterIPNet := &net.IPNet{
			IP:   routerIntPortIPv6,
			Mask: routerIntPortIPv6Net.Mask,
		}

		// In l3only mode the router's internal IP has a /128 mask instead of the internal subnet's mask.
		if util.IsTrue(n.config["ipv6.l3only"]) {
			intRouterIPNet.Mask = net.CIDRMask(128, 128)
		}

		intRouterIPs = append(intRouterIPs, intRouterIPNet)
		intSubnets = append(intSubnets, *routerIntPortIPv6Net)
	}

	// Create internal logical switch if not updating.
	err = n.ovnnb.CreateLogicalSwitch(context.TODO(), n.getIntSwitchName(), update)
	if err != nil {
		return fmt.Errorf("Failed adding internal switch: %w", err)
	}

	if !update {
		reverter.Add(func() { _ = n.ovnnb.DeleteLogicalSwitch(context.TODO(), n.getIntSwitchName()) })
	}

	// Add any listed existing external interface.
	if n.config["bridge.external_interfaces"] != "" {
		for _, entry := range strings.Split(n.config["bridge.external_interfaces"], ",") {
			entry = strings.TrimSpace(entry)

			// Test for extended configuration of external interface.
			entryParts := strings.Split(entry, "/")
			ifParent := ""
			vlanID := 0

			if len(entryParts) == 3 {
				vlanID, err = strconv.Atoi(entryParts[2])
				if err != nil || vlanID < 1 || vlanID > 4094 {
					vlanID = 0
					n.logger.Warn("Ignoring invalid VLAN ID", logger.Ctx{"interface": entry, "vlanID": entryParts[2]})
				} else {
					entry = strings.TrimSpace(entryParts[0])
					ifParent = strings.TrimSpace(entryParts[1])
				}
			}

			iface, err := net.InterfaceByName(entry)
			if err != nil {
				if vlanID == 0 {
					n.logger.Warn("Skipping attaching missing external interface", logger.Ctx{"interface": entry})
					continue
				}

				// If the interface doesn't exist and VLAN ID was provided, create the missing interface.
				ok, err := VLANInterfaceCreate(ifParent, entry, strconv.Itoa(vlanID), false)
				if ok {
					iface, err = net.InterfaceByName(entry)
				}

				if !ok || err != nil {
					return fmt.Errorf("Failed to create external interface %q", entry)
				}
			} else if vlanID > 0 {
				// If the interface exists and VLAN ID was provided, ensure it has the same parent and VLAN ID and is not attached to a different network.
				linkInfo, err := ip.LinkByName(entry)
				if err != nil {
					return fmt.Errorf("Failed to get link info for external interface %q", entry)
				}

				if linkInfo.Kind != "vlan" || linkInfo.Parent != ifParent || linkInfo.VlanID != vlanID || (linkInfo.Master != "" && linkInfo.Master != n.name) {
					return fmt.Errorf("External interface %q already in use", entry)
				}
			}

			unused := true
			addrs, err := iface.Addrs()
			if err == nil {
				for _, addr := range addrs {
					ipAddr, _, err := net.ParseCIDR(addr.String())
					if ipAddr != nil && err == nil && ipAddr.IsGlobalUnicast() {
						unused = false
						break
					}
				}
			}

			if !unused {
				return errors.New("Only unconfigured network interfaces can be bridged")
			}

			lspName := networkOVN.OVNSwitchPort(fmt.Sprintf("%s-external-n%d-%s", n.getNetworkPrefix(), n.state.DB.Cluster.GetNodeID(), entry))
			err = n.ovnnb.CreateLogicalSwitchPort(context.TODO(), n.getIntSwitchName(), lspName, &networkOVN.OVNSwitchPortOpts{
				IPV4:        "none",
				IPV6:        "none",
				Promiscuous: true,
			}, false)
			if err != nil {
				return fmt.Errorf("Failed to create logical switch port for %s: %w", entry, err)
			}

			reverter.Add(func() {
				_ = n.ovnnb.DeleteLogicalSwitchPort(context.TODO(), n.getIntSwitchName(), lspName)
			})

			// Attach host side veth interface to bridge.
			integrationBridge := n.state.GlobalConfig.NetworkOVNIntegrationBridge()

			vswitch, err := n.state.OVS()
			if err != nil {
				return fmt.Errorf("Failed to connect to OVS: %w", err)
			}

			err = vswitch.CreateBridgePort(context.TODO(), integrationBridge, entry, true)
			if err != nil {
				return err
			}

			reverter.Add(func() { _ = vswitch.DeleteBridgePort(context.TODO(), integrationBridge, entry) })

			// Link OVS port to OVN logical port.
			err = vswitch.AssociateInterfaceOVNSwitchPort(context.TODO(), entry, string(lspName))
			if err != nil {
				return err
			}

			// Make sure the port is up.
			link := &ip.Link{Name: entry}
			err = link.SetUp()
			if err != nil {
				return fmt.Errorf("Failed to bring up the host interface %s: %w", entry, err)
			}
		}
	}

	// Setup IP allocation config on logical switch.
	err = n.ovnnb.UpdateLogicalSwitchIPAllocation(context.TODO(), n.getIntSwitchName(), &networkOVN.OVNIPAllocationOpts{
		PrefixIPv4:  routerIntPortIPv4Net,
		PrefixIPv6:  routerIntPortIPv6Net,
		ExcludeIPv4: dhcpReserveIPv4s,
	})
	if err != nil {
		return fmt.Errorf("Failed setting IP allocation settings on internal switch: %w", err)
	}

	// Create internal switch address sets and add subnets to address set.
	if update {
		err = n.ovnnb.UpdateAddressSetAdd(context.TODO(), acl.OVNIntSwitchPortGroupAddressSetPrefix(n.ID()), intSubnets...)
		if err != nil {
			return fmt.Errorf("Failed adding internal subnet address set entries: %w", err)
		}
	} else {
		err = n.ovnnb.CreateAddressSet(context.TODO(), acl.OVNIntSwitchPortGroupAddressSetPrefix(n.ID()), intSubnets...)
		if err != nil {
			return fmt.Errorf("Failed creating internal subnet address set entries: %w", err)
		}

		reverter.Add(func() {
			_ = n.ovnnb.DeleteAddressSet(context.TODO(), acl.OVNIntSwitchPortGroupAddressSetPrefix(n.ID()))
		})
	}

	if routerIntPortIPv4 != nil || routerIntPortIPv6 != nil {
		// Apply router security policy.
		err = n.logicalRouterPolicySetup(n.ovnnb)
		if err != nil {
			return fmt.Errorf("Failed applying router security policy: %w", err)
		}

		// Create internal router port.
		err = n.ovnnb.CreateLogicalRouterPort(context.TODO(), n.getRouterName(), n.getRouterIntPortName(), routerMAC, bridgeMTU, intRouterIPs, "", update)
		if err != nil {
			return fmt.Errorf("Failed adding internal router port: %w", err)
		}

		if !update {
			reverter.Add(func() {
				_ = n.ovnnb.DeleteLogicalRouterPort(context.TODO(), n.getRouterName(), n.getRouterIntPortName())
			})
		}
	} else {
		err := n.ovnnb.DeleteLogicalRouterPort(context.TODO(), n.getRouterName(), n.getRouterIntPortName())
		if err != nil && !errors.Is(err, ovs.ErrNotFound) {
			return fmt.Errorf("Failed deleting logical router port: %w", err)
		}
	}

	// Configure DHCP option sets.
	var dhcpv4UUID, dhcpv6UUID networkOVN.OVNDHCPOptionsUUID
	dhcpV4Subnet := n.DHCPv4Subnet()
	dhcpV6Subnet := n.DHCPv6Subnet()

	if update {
		dhcpv4UUID, dhcpv6UUID, err = n.getDhcpOptionUUIDs()
		if err != nil {
			return err
		}

		var deleteDHCPRecords []networkOVN.OVNDHCPOptionsUUID
		if dhcpV4Subnet == nil && dhcpv4UUID != "" {
			deleteDHCPRecords = append(deleteDHCPRecords, dhcpv4UUID)
		}

		if dhcpV6Subnet == nil && dhcpv6UUID != "" {
			deleteDHCPRecords = append(deleteDHCPRecords, dhcpv6UUID)
		}

		if len(deleteDHCPRecords) > 0 {
			err = n.ovnnb.DeleteLogicalSwitchDHCPOption(context.TODO(), n.getIntSwitchName(), deleteDHCPRecords...)
			if err != nil {
				return fmt.Errorf("Failed deleting existing DHCP settings for internal switch: %w", err)
			}
		}
	}

	var dnsIPv4 []net.IP
	var dnsIPv6 []net.IP

	if n.config["dns.nameservers"] != "" {
		for _, s := range util.SplitNTrimSpace(n.config["dns.nameservers"], ",", -1, false) {
			nsIP := net.ParseIP(s)
			if nsIP.To4() != nil {
				dnsIPv4 = append(dnsIPv4, nsIP)
			} else {
				dnsIPv6 = append(dnsIPv6, nsIP)
			}
		}
	} else {
		if uplinkNet != nil {
			dnsIPv4 = uplinkNet.dnsIPv4
			dnsIPv6 = uplinkNet.dnsIPv6
		}

		if len(dnsIPv4) == 0 {
			dnsIPv4 = []net.IP{routerIntPortIPv4}
		}

		if len(dnsIPv6) == 0 {
			dnsIPv6 = []net.IP{routerIntPortIPv6}
		}
	}

	var dhcpv4Created, dhcpv6Created bool

	// Create DHCPv4 options for internal switch.
	if dhcpV4Subnet != nil {
		// In l3only mode we configure the DHCPv4 server to request the instances use a /32 subnet mask.
		var dhcpV4Netmask string
		if util.IsTrue(n.config["ipv4.l3only"]) {
			dhcpV4Netmask = "255.255.255.255"
		}

		leaseTime := time.Hour * 1
		if n.config["ipv4.dhcp.expiry"] != "" {
			duration, err := time.ParseDuration(n.config["ipv4.dhcp.expiry"])
			if err != nil {
				return fmt.Errorf("Failed to parse expiry: %w", err)
			}

			leaseTime = duration
		}

		opts := &networkOVN.OVNDHCPv4Opts{
			ServerID:           routerIntPortIPv4,
			ServerMAC:          routerMAC,
			Router:             routerIntPortIPv4,
			DomainName:         n.getDomainName(),
			LeaseTime:          leaseTime,
			MTU:                bridgeMTU,
			Netmask:            dhcpV4Netmask,
			DNSSearchList:      n.getDNSSearchList(),
			StaticRoutes:       n.config["ipv4.dhcp.routes"],
			RecursiveDNSServer: dnsIPv4,
		}

		err = n.ovnnb.UpdateLogicalSwitchDHCPv4Options(context.TODO(), n.getIntSwitchName(), dhcpv4UUID, dhcpV4Subnet, opts)
		if err != nil {
			return fmt.Errorf("Failed adding DHCPv4 settings for internal switch: %w", err)
		}

		if dhcpv4UUID == "" {
			dhcpv4Created = true
		}
	}

	// Create DHCPv6 options for internal switch.
	if dhcpV6Subnet != nil {
		opts := &networkOVN.OVNDHCPv6Opts{
			ServerID:           routerMAC,
			DNSSearchList:      n.getDNSSearchList(),
			RecursiveDNSServer: dnsIPv6,
			DHCPv6Stateless:    util.IsFalseOrEmpty(n.config["ipv6.dhcp.stateful"]),
		}

		err = n.ovnnb.UpdateLogicalSwitchDHCPv6Options(context.TODO(), n.getIntSwitchName(), dhcpv6UUID, dhcpV6Subnet, opts)
		if err != nil {
			return fmt.Errorf("Failed adding DHCPv6 settings for internal switch: %w", err)
		}

		if dhcpv6UUID == "" {
			dhcpv6Created = true
		}
	}

	if update && (dhcpv4Created || dhcpv6Created) {
		dhcpv4UUID, dhcpv6UUID, err = n.getDhcpOptionUUIDs()
		if err != nil {
			return err
		}

		ports, err := n.ovnnb.GetLogicalSwitchPorts(context.TODO(), n.getIntSwitchName())
		if err != nil {
			return err
		}

		for portName := range ports {
			err := n.ovnnb.UpdateLogicalSwitchPortDHCP(context.TODO(), portName, dhcpv4UUID, dhcpv6UUID)
			if err != nil {
				return err
			}
		}
	}

	// Set IPv6 router advertisement settings.
	if routerIntPortIPv6Net != nil {
		adressMode := networkOVN.OVNIPv6AddressModeSLAAC
		if dhcpV6Subnet != nil {
			adressMode = networkOVN.OVNIPv6AddressModeDHCPStateless
			if util.IsTrue(n.config["ipv6.dhcp.stateful"]) {
				adressMode = networkOVN.OVNIPv6AddressModeDHCPStateful
			}
		}

		var recursiveDNSServer net.IP

		if len(dnsIPv6) > 0 {
			recursiveDNSServer = dnsIPv6[0] // OVN only supports 1 RA DNS server.
		}

		err = n.ovnnb.UpdateLogicalRouterPort(context.TODO(), n.getRouterIntPortName(), &networkOVN.OVNIPv6RAOpts{
			AddressMode:        adressMode,
			SendPeriodic:       true,
			DNSSearchList:      n.getDNSSearchList(),
			RecursiveDNSServer: recursiveDNSServer,
			MTU:                bridgeMTU,

			// Keep these low until we support DNS search domains via DHCPv4, as otherwise RA DNSSL
			// won't take effect until advert after DHCPv4 has run on instance.
			MinInterval: time.Duration(time.Second * 30),
			MaxInterval: time.Duration(time.Minute * 1),
		})
		if err != nil {
			return fmt.Errorf("Failed setting internal router port IPv6 advertisement settings: %w", err)
		}
	} else {
		err = n.ovnnb.UpdateLogicalRouterPort(context.TODO(), n.getRouterIntPortName(), &networkOVN.OVNIPv6RAOpts{})
		if err != nil && !errors.Is(err, networkOVN.ErrNotFound) {
			return fmt.Errorf("Failed removing internal router port IPv6 advertisement settings: %w", err)
		}
	}

	// Create internal switch port and link to router port.
	if routerIntPortIPv4Net != nil || routerIntPortIPv6Net != nil {
		err = n.ovnnb.CreateLogicalSwitchPort(context.TODO(), n.getIntSwitchName(), n.getIntSwitchRouterPortName(), nil, update)
		if err != nil {
			return fmt.Errorf("Failed adding internal switch router port: %w", err)
		}

		if !update {
			reverter.Add(func() {
				_ = n.ovnnb.DeleteLogicalSwitchPort(context.TODO(), n.getIntSwitchName(), n.getIntSwitchRouterPortName())
			})
		}

		err = n.ovnnb.UpdateLogicalSwitchPortLinkRouter(context.TODO(), n.getIntSwitchRouterPortName(), n.getRouterIntPortName())
		if err != nil {
			return fmt.Errorf("Failed linking internal router port to internal switch port: %w", err)
		}
	} else {
		err := n.ovnnb.DeleteLogicalSwitchPort(context.TODO(), n.getIntSwitchName(), n.getIntSwitchRouterPortName())
		if err != nil && !errors.Is(err, networkOVN.ErrNotFound) {
			return fmt.Errorf("Failed removing logical switch port: %w", err)
		}
	}

	// Apply baseline ACL rules to internal logical switch.
	dnsServers := []net.IP{}
	if uplinkNet != nil {
		dnsServers = append(dnsServers, dnsIPv4...)
		dnsServers = append(dnsServers, dnsIPv6...)
	}

	err = acl.OVNApplyNetworkBaselineRules(n.ovnnb, n.getIntSwitchName(), n.getIntSwitchRouterPortName(), intRouterIPs, dnsServers)
	if err != nil {
		return fmt.Errorf("Failed applying baseline ACL rules to internal switch: %w", err)
	}

	// Create network port group if needed.
	err = n.ensureNetworkPortGroup(projectID)
	if err != nil {
		return fmt.Errorf("Failed to setup network port group: %w", err)
	}

	// Ensure any network assigned security ACL port groups are created ready for instance NICs to use.
	securityACLS := util.SplitNTrimSpace(n.config["security.acls"], ",", -1, true)
	if len(securityACLS) > 0 {
		var aclNameIDs map[string]int64

		err := n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
			var err error

			// Get map of ACL names to DB IDs (used for generating OVN port group names).
			acls, err := dbCluster.GetNetworkACLs(ctx, tx.Tx(), dbCluster.NetworkACLFilter{Project: &n.project})
			if err != nil {
				return err
			}

			aclNameIDs = make(map[string]int64, len(acls))
			for _, acl := range acls {
				aclNameIDs[acl.Name] = int64(acl.ID)
			}

			return nil
		})
		if err != nil {
			return fmt.Errorf("Failed getting network ACL IDs for security ACL setup: %w", err)
		}

		// Request our network is setup with the specified ACLs.
		aclNets := map[string]acl.NetworkACLUsage{
			n.Name(): {Name: n.Name(), Type: n.Type(), ID: n.ID(), Config: n.Config()},
		}

		cleanup, err := addressset.OVNEnsureAddressSetsViaACLs(n.state, n.logger, n.ovnnb, n.Project(), securityACLS)
		if err != nil {
			return fmt.Errorf("Failed ensuring address sets for added ACLs are configured in OVN for network: %w", err)
		}

		reverter.Add(cleanup)

		cleanup, err = acl.OVNEnsureACLs(n.state, n.logger, n.ovnnb, n.Project(), aclNameIDs, aclNets, securityACLS, false)
		if err != nil {
			return fmt.Errorf("Failed ensuring security ACLs are configured in OVN for network: %w", err)
		}

		reverter.Add(cleanup)
	}

	reverter.Success()
	return nil
}

func (n *ovn) getDhcpOptionUUIDs() (v4Uuid networkOVN.OVNDHCPOptionsUUID, v6Uuid networkOVN.OVNDHCPOptionsUUID, err error) {
	// Find first existing DHCP options set for IPv4 and IPv6 and update them instead of adding sets.
	existingOpts, err := n.ovnnb.GetLogicalSwitchDHCPOptions(context.TODO(), n.getIntSwitchName())
	if err != nil {
		return "", "", fmt.Errorf("Failed getting existing DHCP settings for internal switch: %w", err)
	}

	for _, existingOpt := range existingOpts {
		if existingOpt.CIDR.IP.To4() == nil {
			if v6Uuid != "" {
				return "", "", fmt.Errorf("Multiple matching DHCPv6 option sets found for switch %q", n.getIntSwitchName())
			}

			v6Uuid = existingOpt.UUID
		} else {
			if v4Uuid != "" {
				return "", "", fmt.Errorf("Multiple matching DHCPv4 option sets found for switch %q", n.getIntSwitchName())
			}

			v4Uuid = existingOpt.UUID
		}
	}

	return v4Uuid, v6Uuid, nil
}

// logicalRouterPolicySetup applies the security policy to the logical router (clearing any existing policies).
// Optionally excludePeers takes a list of peer network IDs to exclude from the router policy. This is useful
// when removing a peer connection as it allows the security policy to be removed from OVN for that peer before the
// peer connection has been removed from the database.
func (n *ovn) logicalRouterPolicySetup(ovnnb *networkOVN.NB, excludePeers ...int64) error {
	extRouterPort := n.getRouterExtPortName()
	intRouterPort := n.getRouterIntPortName()
	addrSetPrefix := acl.OVNIntSwitchPortGroupAddressSetPrefix(n.ID())

	policies := []networkOVN.OVNRouterPolicy{
		{
			// Allow IPv6 packets arriving from internal router port with valid source address.
			Priority: ovnRouterPolicyPeerAllowPriority,
			Match:    fmt.Sprintf(`(inport == "%s" && ip6 && ip6.src == $%s_ip6)`, intRouterPort, addrSetPrefix),
			Action:   "allow",
		},
		{
			// Allow IPv4 packets arriving from internal router port with valid source address.
			Priority: ovnRouterPolicyPeerAllowPriority,
			Match:    fmt.Sprintf(`(inport == "%s" && ip4 && ip4.src == $%s_ip4)`, intRouterPort, addrSetPrefix),
			Action:   "allow",
		},
		{
			// Drop all other traffic arriving from internal router port.
			// This prevents packets with a source address that is not valid to be dropped, and ensures
			// that we can use the internal address set in ACL rules and trust that this represents all
			// possible routed traffic from the internal network.
			Priority: ovnRouterPolicyPeerDropPriority,
			Match:    fmt.Sprintf(`(inport == "%s")`, intRouterPort),
			Action:   "drop",
		},
	}

	// Add rules to drop inbound traffic arriving on external uplink port from peer connection addresses.
	// This prevents source address spoofing of peer connection routes from the external network, which in
	// turn allows us to use the peer connection's address set for referencing traffic from the peer in ACL.
	err := n.forPeers(func(targetOVNNet *ovn) error {
		if slices.Contains(excludePeers, targetOVNNet.ID()) {
			return nil // Don't setup rules for this peer network connection.
		}

		targetAddrSetPrefix := acl.OVNIntSwitchPortGroupAddressSetPrefix(targetOVNNet.ID())

		// Associate the rules with the local peering port so we can identify them later if needed.
		comment := n.getLogicalRouterPeerPortName(targetOVNNet.ID())
		policies = append(policies, networkOVN.OVNRouterPolicy{
			Priority: ovnRouterPolicyPeerDropPriority,
			Match:    fmt.Sprintf(`(inport == "%s" && ip6 && ip6.src == $%s_ip6) // %s`, extRouterPort, targetAddrSetPrefix, comment),
			Action:   "drop",
		}, networkOVN.OVNRouterPolicy{
			Priority: ovnRouterPolicyPeerDropPriority,
			Match:    fmt.Sprintf(`(inport == "%s" && ip4 && ip4.src == $%s_ip4) // %s`, extRouterPort, targetAddrSetPrefix, comment),
			Action:   "drop",
		})

		return nil
	})
	if err != nil {
		return err
	}

	return n.ovnnb.UpdateLogicalRouterPolicy(context.TODO(), n.getRouterName(), policies...)
}

// ensureNetworkPortGroup ensures that the network level port group (used for classifying NICs connected to this
// network as internal) exists.
func (n *ovn) ensureNetworkPortGroup(projectID int64) error {
	// Create port group (if needed) for NICs to classify as internal.
	intPortGroupName := acl.OVNIntSwitchPortGroupName(n.ID())
	intPortGroupUUID, _, err := n.ovnnb.GetPortGroupInfo(context.TODO(), intPortGroupName)
	if err != nil {
		return fmt.Errorf("Failed getting port group UUID for network %q setup: %w", n.Name(), err)
	}

	if intPortGroupUUID == "" {
		// Create internal port group and associate it with the logical switch, so that it will be
		// removed when the logical switch is removed.
		err = n.ovnnb.CreatePortGroup(context.TODO(), projectID, intPortGroupName, "", n.getIntSwitchName())
		if err != nil {
			return fmt.Errorf("Failed creating port group %q for network %q setup: %w", intPortGroupName, n.Name(), err)
		}
	}

	return nil
}

// addChassisGroupEntry adds an entry for the local OVS chassis to the OVN logical network's chassis group.
// The chassis priority value is a stable-random value derived from chassis group name and node ID. This is so we
// don't end up using the same chassis for the primary uplink chassis for all OVN networks in a cluster.
func (n *ovn) addChassisGroupEntry() error {
	// Get local chassis ID for chassis group.
	vswitch, err := n.state.OVS()
	if err != nil {
		return fmt.Errorf("Failed to connect to OVS: %w", err)
	}

	chassisID, err := vswitch.GetChassisID(context.TODO())
	if err != nil {
		return fmt.Errorf("Failed getting OVS Chassis ID: %w", err)
	}

	// Seed the stable random number generator with the chassis group name.
	// This way each OVN network will have its own random seed, so that we don't end up using the same chassis
	// for the primary uplink chassis for all OVN networks in a cluster.
	chassisGroupName := n.getChassisGroupName()
	r, err := localUtil.GetStableRandomGenerator(string(chassisGroupName))
	if err != nil {
		return fmt.Errorf("Failed generating stable random chassis group priority: %w", err)
	}

	// Get all members in cluster.
	ourMemberID := int(n.state.DB.Cluster.GetNodeID())
	var memberIDs []int
	err = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		members, err := tx.GetNodes(ctx)
		if err != nil {
			return fmt.Errorf("Failed getting cluster members for adding chassis group entry: %w", err)
		}

		for _, member := range members {
			memberIDs = append(memberIDs, int(member.ID))
		}

		return nil
	})
	if err != nil {
		return err
	}

	// Sort the nodes based on ID for stable priority generation.
	sort.Ints(memberIDs)

	// Generate a random priority from the seed for each node until we find a match for our node ID.
	// In this way the chassis priority for this node will be set to a per-node stable random value.
	var priority int
	for _, memberID := range memberIDs {
		priority = r.Intn(ovnChassisPriorityMax + 1)
		if memberID == ourMemberID {
			break
		}
	}

	err = n.ovnnb.SetChassisGroupPriority(context.TODO(), chassisGroupName, chassisID, priority)
	if err != nil {
		return fmt.Errorf("Failed adding OVS chassis %q with priority %d to chassis group %q: %w", chassisID, priority, chassisGroupName, err)
	}

	n.logger.Debug("Chassis group entry added", logger.Ctx{"chassisGroup": chassisGroupName, "memberID": ourMemberID, "priority": priority})

	return nil
}

// deleteChassisGroupEntry deletes an entry for the local OVS chassis from the OVN logical network's chassis group.
func (n *ovn) deleteChassisGroupEntry() error {
	// Remove local chassis from chassis group.
	vswitch, err := n.state.OVS()
	if err != nil {
		return fmt.Errorf("Failed to connect to OVS: %w", err)
	}

	chassisID, err := vswitch.GetChassisID(context.TODO())
	if err != nil {
		return fmt.Errorf("Failed getting OVS Chassis ID: %w", err)
	}

	err = n.ovnnb.SetChassisGroupPriority(context.TODO(), n.getChassisGroupName(), chassisID, -1)
	if err != nil && !errors.Is(err, ovs.ErrNotFound) {
		return fmt.Errorf("Failed deleting OVS chassis %q from chassis group %q: %w", chassisID, n.getChassisGroupName(), err)
	}

	return nil
}

// Delete deletes a network.
func (n *ovn) Delete(clientType request.ClientType) error {
	n.logger.Debug("Delete", logger.Ctx{"clientType": clientType})

	err := n.Stop()
	if err != nil {
		return err
	}

	if clientType == request.ClientTypeNormal {
		// Delete the router and anything tied to it (router ports, static routes, policies, nat, ...).
		err = n.ovnnb.DeleteLogicalRouter(context.TODO(), n.getRouterName())
		if err != nil && !errors.Is(err, networkOVN.ErrNotFound) {
			return err
		}

		// Delete the external logical switch and anything tied to it (ports, ...).
		err = n.ovnnb.DeleteLogicalSwitch(context.TODO(), n.getExtSwitchName())
		if err != nil && !errors.Is(err, networkOVN.ErrNotFound) {
			return err
		}

		// Delete the internal logical switch and anything tied to it (ports, ...).
		err = n.ovnnb.DeleteLogicalSwitch(context.TODO(), n.getIntSwitchName())
		if err != nil && !errors.Is(err, networkOVN.ErrNotFound) {
			return err
		}

		// Delete any related address sets.
		err = n.ovnnb.DeleteAddressSet(context.TODO(), acl.OVNIntSwitchPortGroupAddressSetPrefix(n.ID()))
		if err != nil && !errors.Is(err, networkOVN.ErrNotFound) {
			return err
		}

		// Delete address sets used in ACLs.
		securityACLS := util.SplitNTrimSpace(n.config["security.acls"], ",", -1, true)

		// Load address sets referenced by ACLs.
		err = addressset.OVNDeleteAddressSetsViaACLs(n.state, n.logger, n.ovnnb, n.Project(), securityACLS)
		if err != nil {
			return fmt.Errorf("Failed deleting address sets for security ACLs in OVN for network: %w", err)
		}

		// Delete the chassis group for the network.
		err = n.ovnnb.DeleteChassisGroup(context.TODO(), n.getChassisGroupName())
		if err != nil && !errors.Is(err, networkOVN.ErrNotFound) {
			return err
		}

		// Clean up any now unused port group.
		securityACLs := util.SplitNTrimSpace(n.config["security.acls"], ",", -1, true)
		if len(securityACLs) > 0 {
			err = acl.OVNPortGroupDeleteIfUnused(n.state, n.logger, n.ovnnb, n.project, &api.Network{Name: n.name}, "")
			if err != nil {
				return fmt.Errorf("Failed removing unused OVN port groups: %w", err)
			}
		}

		// Delete any network forwards and load balancers.
		forwardListenAddresses := map[int64]string{}
		loadBalancerListenAddresses := map[int64]string{}

		err = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
			networkID := n.ID()

			// Get the forward addresses.
			dbForwards, err := dbCluster.GetNetworkForwards(ctx, tx.Tx(), dbCluster.NetworkForwardFilter{
				NetworkID: &networkID,
			})
			if err != nil {
				return fmt.Errorf("Failed loading network forwards: %w", err)
			}

			// Get the load balancers.
			dbLoadBalancers, err := dbCluster.GetNetworkLoadBalancers(ctx, tx.Tx(), dbCluster.NetworkLoadBalancerFilter{
				NetworkID: &networkID,
			})
			if err != nil {
				return fmt.Errorf("Failed loading network load balancers: %w", err)
			}

			for _, fwd := range dbForwards {
				forwardListenAddresses[fwd.ID] = fwd.ListenAddress
			}

			for _, lb := range dbLoadBalancers {
				loadBalancerListenAddresses[lb.ID] = lb.ListenAddress
			}

			return nil
		})
		if err != nil {
			return err
		}

		loadBalancers := make([]networkOVN.OVNLoadBalancer, 0, len(forwardListenAddresses)+len(loadBalancerListenAddresses))
		for _, listenAddress := range forwardListenAddresses {
			loadBalancers = append(loadBalancers, n.getLoadBalancerName(listenAddress))
		}

		for _, listenAddress := range loadBalancerListenAddresses {
			loadBalancers = append(loadBalancers, n.getLoadBalancerName(listenAddress))
		}

		err = n.ovnnb.DeleteLoadBalancer(context.TODO(), loadBalancers...)
		if err != nil {
			return fmt.Errorf("Failed deleting network forwards and load balancers: %w", err)
		}
	}

	return n.common.delete(clientType)
}

// Rename renames a network.
func (n *ovn) Rename(newName string) error {
	n.logger.Debug("Rename", logger.Ctx{"newName": newName})

	// Rename common steps.
	err := n.common.rename(newName)
	if err != nil {
		return err
	}

	return nil
}

// chassisEnabled checks the cluster config to see if this particular
// member should act as an OVN chassis.
func (n *ovn) chassisEnabled(ctx context.Context, tx *db.ClusterTx) (bool, error) {
	if n.config["network"] == "none" {
		return false, nil
	}
	
	uplinkNet, err := LoadByName(n.state, api.ProjectDefaultName, n.config["network"])
	if err != nil {
		return false, fmt.Errorf("Failed loading uplink network %q: %w", n.config["network"], err)
	}

	if n.config["parent"] == "none" && uplinkNet.Type() == "physical"{
		return false, nil
	}

	// Get the member info.
	memberID := tx.GetNodeID()
	members, err := tx.GetNodes(ctx)
	if err != nil {
		return false, fmt.Errorf("Failed getting cluster members: %w", err)
	}

	// Determine whether to add ourselves as a chassis.
	// If no server has the role, enable the chassis, otherwise only
	// enable if the local server has the role.
	enableChassis := -1

	for _, member := range members {
		hasRole := slices.Contains(member.Roles, db.ClusterRoleOVNChassis)

		if hasRole {
			if member.ID == memberID {
				// Local node has the OVN chassis role, enable chassis.
				enableChassis = 1
				break
			}

			// Some other node has the OVN chassis role, don't enable.
			enableChassis = 0
		}
	}

	return enableChassis != 0, nil
}

// Start starts adds the local OVS chassis ID to the OVN chass group and starts the local OVS uplink port.
func (n *ovn) Start() error {
	n.logger.Debug("Start")

	reverter := revert.New()
	defer reverter.Fail()

	var err error

	reverter.Add(func() { n.setUnavailable() })

	// Check that uplink network is available.
	if n.config["network"] != "" && n.config["network"] != "none" && !IsAvailable(api.ProjectDefaultName, n.config["network"]) {
		return fmt.Errorf("Uplink network %q is unavailable", n.config["network"])
	}

	var projectID int64
	var chassisEnabled bool
	err = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		// Get the project ID.
		projectID, err = dbCluster.GetProjectID(context.Background(), tx.Tx(), n.project)
		if err != nil {
			return err
		}

		// Check if we should enable the chassis.
		chassisEnabled, err = n.chassisEnabled(ctx, tx)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("Failed getting project ID for project %q: %w", n.project, err)
	}

	// Ensure network level port group exists.
	err = n.ensureNetworkPortGroup(projectID)
	if err != nil {
		return err
	}

	// Handle chassis groups.
	if chassisEnabled {
		// Add local member's OVS chassis ID to logical chassis group.
		err = n.addChassisGroupEntry()
		if err != nil {
			return err
		}
	} else {
		// Make sure we don't have a group entry.
		err = n.deleteChassisGroupEntry()
		if err != nil {
			return err
		}
	}

	err = n.startUplinkPort()
	if err != nil {
		return err
	}

	// Setup BGP.
	err = n.bgpSetup(nil)
	if err != nil {
		return err
	}

	err = n.loadBalancerBGPSetupPrefixes()
	if err != nil {
		return fmt.Errorf("Failed applying BGP prefixes for load balancers: %w", err)
	}

	// Setup event handler for monitored services.
	handler := networkOVN.EventHandler{
		Tables: []string{"Service_Monitor"},
		Hook: func(action string, table string, oldObject ovsdbModel.Model, newObject ovsdbModel.Model) {
			// Skip invalid notifications.
			if oldObject == nil && newObject == nil {
				return
			}

			// Get the object.
			dbObject := newObject
			if dbObject == nil {
				dbObject = oldObject
			}

			srvStatus, ok := dbObject.(*ovnSB.ServiceMonitor)
			if !ok {
				return
			}

			// Check if this is our network.
			if !strings.HasPrefix(srvStatus.LogicalPort, fmt.Sprintf("incus-net%d-instance-", n.id)) {
				return
			}

			// Locate affected load-balancers.
			lbs, err := n.ovnnb.GetLoadBalancersByStatusUpdate(context.TODO(), *srvStatus)
			if err != nil {
				return
			}

			for _, lb := range lbs {
				// Check for status of all backends on this load-balancer.
				online, err := n.ovnsb.CheckLoadBalancerOnline(context.TODO(), lb)
				if err != nil {
					return
				}

				// Parse the name.
				fields := strings.Split(lb.Name, "-")
				listenAddr := net.ParseIP(fields[3])
				if listenAddr == nil {
					return
				}

				// Check if we have a matching UDP load-balancer.
				fields[4] = "udp"
				lbUDP, _ := n.ovnnb.GetLoadBalancer(context.TODO(), networkOVN.OVNLoadBalancer(strings.Join(fields, "-")))
				if lbUDP != nil {
					// UDP backends can't be checked, so have to assume online.
					online = true
				}

				// Prepare advertisement.
				ipVersion := uint(4)
				if listenAddr.To4() == nil {
					ipVersion = 6
				}

				bgpOwner := fmt.Sprintf("network_%d_load_balancer", n.id)
				nextHopAddr := n.bgpNextHopAddress(ipVersion)
				natEnabled := util.IsTrue(n.config[fmt.Sprintf("ipv%d.nat", ipVersion)])
				_, netSubnet, _ := net.ParseCIDR(n.config[fmt.Sprintf("ipv%d.address", ipVersion)])

				routeSubnetSize := 128
				if ipVersion == 4 {
					routeSubnetSize = 32
				}

				// Don't export internal address forwards (those inside the NAT enabled network's subnet).
				if natEnabled && netSubnet != nil && netSubnet.Contains(listenAddr) {
					return
				}

				_, ipRouteSubnet, err := net.ParseCIDR(fmt.Sprintf("%s/%d", listenAddr.String(), routeSubnetSize))
				if err != nil {
					return
				}

				// Update the BGP state.
				if online {
					err = n.state.BGP.AddPrefix(*ipRouteSubnet, nextHopAddr, bgpOwner)
					if err != nil {
						return
					}
				} else {
					err = n.state.BGP.RemovePrefix(*ipRouteSubnet, nextHopAddr)
					if err != nil {
						return
					}
				}
			}
		},
	}

	err = networkOVN.AddOVNSBHandler(fmt.Sprintf("network_%d", n.id), handler)
	if err != nil {
		return err
	}

	reverter.Success()

	// Ensure network is marked as available now its started.
	n.setAvailable()

	return nil
}

// Stop deletes the local OVS uplink port (if unused) and deletes the local OVS chassis ID from the
// OVN chassis group.
func (n *ovn) Stop() error {
	n.logger.Debug("Stop")

	// Delete local OVS chassis ID from logical OVN HA chassis group.
	err := n.deleteChassisGroupEntry()
	if err != nil {
		return err
	}

	// Delete local uplink port if not used by other OVN networks.
	err = n.deleteUplinkPort()
	if err != nil {
		return err
	}

	// Clear BGP.
	err = n.bgpClear(n.config)
	if err != nil {
		return err
	}

	// Clear event handler for monitored services.
	err = networkOVN.RemoveOVNSBHandler(fmt.Sprintf("network_%d", n.id))
	if err != nil {
		return err
	}

	return nil
}

// instanceNICGetRoutes returns list of routes defined in nicConfig.
func (n *ovn) instanceNICGetRoutes(nicConfig map[string]string) []net.IPNet {
	var routes []net.IPNet

	routeKeys := []string{"ipv4.routes", "ipv4.routes.external", "ipv6.routes", "ipv6.routes.external"}

	for _, key := range routeKeys {
		for _, routeStr := range util.SplitNTrimSpace(nicConfig[key], ",", -1, true) {
			_, route, err := net.ParseCIDR(routeStr)
			if err != nil {
				continue // Skip invalid routes (should never happen).
			}

			routes = append(routes, *route)
		}
	}

	return routes
}

// Update updates the network. Accepts notification boolean indicating if this update request is coming from a
// cluster notification, in which case do not update the database, just apply local changes needed.
func (n *ovn) Update(newNetwork api.NetworkPut, targetNode string, clientType request.ClientType) error {
	n.logger.Debug("Update", logger.Ctx{"clientType": clientType, "newNetwork": newNetwork})

	err := n.populateAutoConfig(newNetwork.Config)
	if err != nil {
		return fmt.Errorf("Failed generating auto config: %w", err)
	}

	if clientType == request.ClientTypeNotifier {
		// Reload BGP on notifications.
		err = n.bgpSetup(nil)
		if err != nil {
			return err
		}

		err = n.loadBalancerBGPSetupPrefixes()
		if err != nil {
			return fmt.Errorf("Failed applying BGP prefixes for load balancers: %w", err)
		}

		return nil
	}

	dbUpdateNeeded, changedKeys, oldNetwork, err := n.common.configChanged(newNetwork)
	if err != nil {
		return err
	}

	if !dbUpdateNeeded {
		return nil // Nothing changed.
	}

	// If the network as a whole has not had any previous creation attempts, or the node itself is still
	// pending, then don't apply the new settings to the node, just to the database record (ready for the
	// actual global create request to be initiated).
	if n.Status() == api.NetworkStatusPending || n.LocalStatus() == api.NetworkStatusPending {
		return n.common.update(newNetwork, targetNode, clientType)
	}

	reverter := revert.New()
	defer reverter.Fail()

	// Define a function which reverts everything.
	reverter.Add(func() {
		// Reset changes to all nodes and database.
		_ = n.common.update(oldNetwork, targetNode, clientType)

		// Reset any change that was made to logical network.
		if clientType == request.ClientTypeNormal {
			_ = n.setup(true)
		}

		_ = n.Start()
	})

	// Stop network before new config applied if uplink network is changing.
	if slices.Contains(changedKeys, "network") {
		err = n.Stop()
		if err != nil {
			return err
		}

		// Remove volatile keys associated with old network in new config.
		delete(newNetwork.Config, ovnVolatileUplinkIPv4)
		delete(newNetwork.Config, ovnVolatileUplinkIPv6)
	}

	// Apply changes to all nodes and database.
	err = n.common.update(newNetwork, targetNode, clientType)
	if err != nil {
		return err
	}

	// Re-setup the logical network after config applied if needed.
	if len(changedKeys) > 0 && clientType == request.ClientTypeNormal {
		err = n.setup(true)
		if err != nil {
			return err
		}

		// Work out which ACLs have been added and removed.
		oldACLs := util.SplitNTrimSpace(oldNetwork.Config["security.acls"], ",", -1, true)
		newACLs := util.SplitNTrimSpace(newNetwork.Config["security.acls"], ",", -1, true)
		removedACLs := []string{}
		for _, oldACL := range oldACLs {
			if !slices.Contains(newACLs, oldACL) {
				removedACLs = append(removedACLs, oldACL)
			}
		}

		addedACLs := []string{}
		for _, newACL := range newACLs {
			if !slices.Contains(oldACLs, newACL) {
				addedACLs = append(addedACLs, newACL)
			}
		}

		// Detect if network default rule config has changed.
		defaultRuleKeys := []string{"security.acls.default.ingress.action", "security.acls.default.egress.action", "security.acls.default.ingress.logged", "security.acls.default.egress.logged"}
		changedDefaultRuleKeys := []string{}
		for _, k := range defaultRuleKeys {
			if slices.Contains(changedKeys, k) {
				changedDefaultRuleKeys = append(changedDefaultRuleKeys, k)
			}
		}

		var aclNameIDs map[string]int64

		err = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
			// Get map of ACL names to DB IDs (used for generating OVN port group names).
			acls, err := dbCluster.GetNetworkACLs(ctx, tx.Tx(), dbCluster.NetworkACLFilter{Project: &n.project})
			if err != nil {
				return err
			}

			aclNameIDs = make(map[string]int64, len(acls))
			for _, acl := range acls {
				aclNameIDs[acl.Name] = int64(acl.ID)
			}

			return nil
		})
		if err != nil {
			return fmt.Errorf("Failed getting network ACL IDs for security ACL update: %w", err)
		}

		addChangeSet := map[networkOVN.OVNPortGroup][]networkOVN.OVNSwitchPortUUID{}
		removeChangeSet := map[networkOVN.OVNPortGroup][]networkOVN.OVNSwitchPortUUID{}

		// Get list of active switch ports (avoids repeated querying of OVN NB).
		activePorts, err := n.ovnnb.GetLogicalSwitchPorts(context.TODO(), n.getIntSwitchName())
		if err != nil {
			return fmt.Errorf("Failed getting active ports: %w", err)
		}

		aclConfigChanged := len(addedACLs) > 0 || len(removedACLs) > 0 || len(changedDefaultRuleKeys) > 0

		var localNICRoutes []net.IPNet

		// Apply ACL changes to running instance NICs that use this network.
		err = UsedByInstanceDevices(n.state, n.Project(), n.Name(), n.Type(), func(inst db.InstanceArgs, nicName string, nicConfig map[string]string) error {
			nicACLs := util.SplitNTrimSpace(nicConfig["security.acls"], ",", -1, true)

			// Get logical port UUID and name.
			instancePortName := n.getInstanceDevicePortName(inst.Config["volatile.uuid"], nicName)

			portUUID, found := activePorts[instancePortName]
			if !found {
				return nil // No need to update a port that isn't started yet.
			}

			// Apply security ACL and default rule changes.
			if aclConfigChanged {
				// Update relevant address sets and Remove from removedACL.
				if len(addedACLs) > 0 {
					cleanup, err := addressset.OVNEnsureAddressSetsViaACLs(n.state, n.logger, n.ovnnb, n.Project(), addedACLs)
					if err != nil {
						return fmt.Errorf("Failed ensuring address sets for added ACLs are configured in OVN for network: %w", err)
					}

					reverter.Add(cleanup)
				}

				if len(removedACLs) > 0 {
					err = addressset.OVNDeleteAddressSetsViaACLs(n.state, n.logger, n.ovnnb, n.Project(), removedACLs)
					if err != nil {
						return fmt.Errorf("Failed to delete address set for removed ACLs are configured in OVN for network: %w", err)
					}
				}

				// Check whether we need to add any of the new ACLs to the NIC.
				for _, addedACL := range addedACLs {
					if slices.Contains(nicACLs, addedACL) {
						continue // NIC already has this ACL applied directly, so no need to add.
					}

					aclID, found := aclNameIDs[addedACL]
					if !found {
						return fmt.Errorf("Cannot find security ACL ID for %q", addedACL)
					}

					// Add NIC port to ACL port group.
					portGroupName := acl.OVNACLPortGroupName(aclID)
					acl.OVNPortGroupInstanceNICSchedule(portUUID, addChangeSet, portGroupName)
					n.logger.Debug("Scheduled logical port for ACL port group addition", logger.Ctx{"networkACL": addedACL, "portGroup": portGroupName, "port": instancePortName})
				}

				// Check whether we need to remove any of the removed ACLs from the NIC.
				for _, removedACL := range removedACLs {
					if slices.Contains(nicACLs, removedACL) {
						continue // NIC still has this ACL applied directly, so don't remove.
					}

					aclID, found := aclNameIDs[removedACL]
					if !found {
						return fmt.Errorf("Cannot find security ACL ID for %q", removedACL)
					}

					// Remove NIC port from ACL port group.
					portGroupName := acl.OVNACLPortGroupName(aclID)
					acl.OVNPortGroupInstanceNICSchedule(portUUID, removeChangeSet, portGroupName)
					n.logger.Debug("Scheduled logical port for ACL port group removal", logger.Ctx{"networkACL": removedACL, "portGroup": portGroupName, "port": instancePortName})
				}

				// If there are no ACLs being applied to the NIC (either from network or NIC) then
				// we should remove the default rule from the NIC.
				if len(newACLs) <= 0 && len(nicACLs) <= 0 {
					err = n.ovnnb.ClearPortGroupPortACLRules(context.TODO(), acl.OVNIntSwitchPortGroupName(n.ID()), instancePortName)
					if err != nil {
						return fmt.Errorf("Failed clearing OVN default ACL rules for instance NIC: %w", err)
					}

					n.logger.Debug("Cleared NIC default rules", logger.Ctx{"port": instancePortName})
				} else {
					defaultRuleChange := false

					// If there are ACLs being applied, then decide if the default rule config
					// has changed materially for the NIC and update it if needed.
					for _, k := range changedDefaultRuleKeys {
						_, found := nicConfig[k]
						if found {
							continue // Skip if changed key is overridden in NIC.
						}

						defaultRuleChange = true
						break
					}

					// If the default rule config has changed materially for this NIC or the
					// network previously didn't have any ACLs applied and now does, then add
					// the default rule to the NIC.
					if defaultRuleChange || len(oldACLs) <= 0 {
						// Set the automatic default ACL rule for the port.
						ingressAction, ingressLogged := n.instanceDeviceACLDefaults(nicConfig, "ingress")
						egressAction, egressLogged := n.instanceDeviceACLDefaults(nicConfig, "egress")

						logPrefix := fmt.Sprintf("%s-%s", inst.Config["volatile.uuid"], nicName)
						err = acl.OVNApplyInstanceNICDefaultRules(n.ovnnb, acl.OVNIntSwitchPortGroupName(n.ID()), logPrefix, instancePortName, ingressAction, ingressLogged, egressAction, egressLogged)
						if err != nil {
							return fmt.Errorf("Failed applying OVN default ACL rules for instance NIC: %w", err)
						}

						n.logger.Debug("Set NIC default rule", logger.Ctx{"port": instancePortName, "ingressAction": ingressAction, "ingressLogged": ingressLogged, "egressAction": egressAction, "egressLogged": egressLogged})
					}
				}
			}

			// Add NIC routes to list.
			localNICRoutes = append(localNICRoutes, n.instanceNICGetRoutes(nicConfig)...)

			return nil
		})
		if err != nil {
			return err
		}

		// Apply add/remove changesets.
		if len(addChangeSet) > 0 || len(removeChangeSet) > 0 {
			n.logger.Debug("Applying ACL port group member change sets")
			err = n.ovnnb.UpdatePortGroupMembers(context.TODO(), addChangeSet, removeChangeSet)
			if err != nil {
				return fmt.Errorf("Failed applying OVN port group member change sets for instance NIC: %w", err)
			}
		}

		// Check if any of the removed ACLs should have any unused port groups deleted.
		if len(removedACLs) > 0 {
			err = acl.OVNPortGroupDeleteIfUnused(n.state, n.logger, n.ovnnb, n.project, &api.Network{Name: n.name}, "", newACLs...)
			if err != nil {
				return fmt.Errorf("Failed removing unused OVN port groups: %w", err)
			}
		}

		// Ensure all active NIC routes are present in internal switch's address set.
		err = n.ovnnb.UpdateAddressSetAdd(context.TODO(), acl.OVNIntSwitchPortGroupAddressSetPrefix(n.ID()), localNICRoutes...)
		if err != nil {
			return fmt.Errorf("Failed adding active NIC routes to switch address set: %w", err)
		}

		// Remove any old unused subnet addresses from the internal switch's address set.
		rebuildPeers := false
		for _, key := range []string{"ipv4.address", "ipv6.address"} {
			if slices.Contains(changedKeys, key) {
				rebuildPeers = true
				_, oldRouterIntPortIPNet, _ := net.ParseCIDR(oldNetwork.Config[key])
				if oldRouterIntPortIPNet != nil {
					err = n.ovnnb.UpdateAddressSetRemove(context.TODO(), acl.OVNIntSwitchPortGroupAddressSetPrefix(n.ID()), *oldRouterIntPortIPNet)
					if err != nil {
						return fmt.Errorf("Failed removing old network subnet %q from switch address set: %w", oldRouterIntPortIPNet.String(), err)
					}
				}
			}
		}

		if rebuildPeers {
			// Rebuild peering config.
			opts, err := n.peerGetLocalOpts(localNICRoutes)
			if err != nil {
				return err
			}

			err = n.forPeers(func(targetOVNNet *ovn) error {
				err = n.peerSetup(n.ovnnb, targetOVNNet, *opts)
				if err != nil {
					return err
				}

				return nil
			})
			if err != nil {
				return err
			}
		}
	}

	// If uplink network is changing, start network after config applied.
	if slices.Contains(changedKeys, "network") {
		err = n.Start()
		if err != nil {
			return err
		}

		// Notify all other members to refresh their BGP prefixes.
		notifier, err := cluster.NewNotifier(n.state, n.state.Endpoints.NetworkCert(), n.state.ServerCert(), cluster.NotifyAll)
		if err != nil {
			return err
		}

		err = notifier(func(client incus.InstanceServer) error {
			return client.UseProject(n.project).UpdateNetwork(n.name, newNetwork, "")
		})
		if err != nil {
			return err
		}
	} else {
		// Setup BGP.
		err = n.bgpSetup(oldNetwork.Config)
		if err != nil {
			return err
		}
	}

	err = n.loadBalancerBGPSetupPrefixes()
	if err != nil {
		return fmt.Errorf("Failed applying BGP prefixes for load balancers: %w", err)
	}

	// Delete any address set that is unused
	err = addressset.OVNAddressSetsDeleteIfUnused(n.state, n.logger, n.ovnnb, n.Project())
	if err != nil {
		return fmt.Errorf("Failed removing unused OVN address sets: %w", err)
	}

	reverter.Success()
	return nil
}

// getInstanceDevicePortName returns the switch port name to use for an instance device.
func (n *ovn) getInstanceDevicePortName(instanceUUID string, deviceName string) networkOVN.OVNSwitchPort {
	return networkOVN.OVNSwitchPort(fmt.Sprintf("%s-%s-%s", n.getIntSwitchInstancePortPrefix(), instanceUUID, deviceName))
}

// instanceDevicePortRoutesParse parses the instance NIC device config for internal routes and external routes.
func (n *ovn) instanceDevicePortRoutesParse(deviceConfig map[string]string) ([]*net.IPNet, []*net.IPNet, error) {
	var err error

	internalRoutes := []*net.IPNet{}
	for _, key := range []string{"ipv4.routes", "ipv6.routes"} {
		if deviceConfig[key] == "" {
			continue
		}

		internalRoutes, err = SubnetParseAppend(internalRoutes, util.SplitNTrimSpace(deviceConfig[key], ",", -1, false)...)
		if err != nil {
			return nil, nil, fmt.Errorf("Invalid %q value: %w", key, err)
		}
	}

	externalRoutes := []*net.IPNet{}
	for _, key := range []string{"ipv4.routes.external", "ipv6.routes.external"} {
		if deviceConfig[key] == "" {
			continue
		}

		externalRoutes, err = SubnetParseAppend(externalRoutes, util.SplitNTrimSpace(deviceConfig[key], ",", -1, false)...)
		if err != nil {
			return nil, nil, fmt.Errorf("Invalid %q value: %w", key, err)
		}
	}

	return internalRoutes, externalRoutes, nil
}

// InstanceDevicePortValidateExternalRoutes validates the external routes for an OVN instance port.
func (n *ovn) InstanceDevicePortValidateExternalRoutes(deviceInstance instance.Instance, deviceName string, portExternalRoutes []*net.IPNet) error {
	if n.config["network"] == "none" {
		return nil
	}

	var p *api.Project
	var uplink *api.Network

	err := n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		var err error

		// Get uplink routes.
		_, uplink, _, err = tx.GetNetworkInAnyState(ctx, api.ProjectDefaultName, n.config["network"])

		return err
	})
	if err != nil {
		return fmt.Errorf("Failed to load uplink network %q: %w", n.config["network"], err)
	}

	// Check port's external routes are sufficiently small when using l2proxy ingress mode on uplink.
	if slices.Contains([]string{"l2proxy", ""}, uplink.Config["ovn.ingress_mode"]) {
		for _, portExternalRoute := range portExternalRoutes {
			rOnes, rBits := portExternalRoute.Mask.Size()
			if rBits > 32 && rOnes < 122 {
				return fmt.Errorf("External route %q is too large. Maximum size for IPv6 external route is /122", portExternalRoute.String())
			} else if rOnes < 26 {
				return fmt.Errorf("External route %q is too large. Maximum size for IPv4 external route is /26", portExternalRoute.String())
			}
		}
	}

	// Load the project to get uplink network restrictions.
	err = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		project, err := dbCluster.GetProject(ctx, tx.Tx(), n.project)
		if err != nil {
			return err
		}

		p, err = project.ToAPI(ctx, tx.Tx())

		return err
	})
	if err != nil {
		return fmt.Errorf("Failed to load network restrictions from project %q: %w", n.project, err)
	}

	externalSubnetsInUse, err := n.getExternalSubnetInUse(n.config["network"])
	if err != nil {
		return err
	}

	// Get project restricted routes.
	projectRestrictedSubnets, err := n.projectRestrictedSubnets(p, n.config["network"])
	if err != nil {
		return err
	}

	// Check if uplink has routed ingress anycast mode enabled, as this relaxes the overlap checks.
	ipv4UplinkAnycast := n.uplinkHasIngressRoutedAnycastIPv4(uplink)
	ipv6UplinkAnycast := n.uplinkHasIngressRoutedAnycastIPv6(uplink)

	for _, portExternalRoute := range portExternalRoutes {
		// Check the external port route is allowed within both the uplink's external routes and any
		// project restricted subnets.
		err = n.validateExternalSubnet(uplink, projectRestrictedSubnets, portExternalRoute)
		if err != nil {
			return err
		}

		// Skip overlap checks if the external route's protocol has anycast mode enabled on the uplink.
		if portExternalRoute.IP.To4() == nil {
			if ipv6UplinkAnycast {
				continue
			}
		} else if ipv4UplinkAnycast {
			continue
		}

		// Check the external port route doesn't fall within any existing OVN network external subnets.
		for _, externalSubnetUser := range externalSubnetsInUse {
			// Skip our own network's SNAT address (as it can be used for NICs in the network).
			if externalSubnetUser.usageType == subnetUsageNetworkSNAT && externalSubnetUser.networkProject == n.project && externalSubnetUser.networkName == n.name {
				continue
			}

			if deviceInstance == nil {
				// Skip checking instance devices during profile validation, only do this when
				// an instance is supplied.
				if externalSubnetUser.instanceDevice != "" {
					continue
				}
			} else {
				// Skip our own NIC device.
				if externalSubnetUser.instanceProject == deviceInstance.Project().Name && externalSubnetUser.instanceName == deviceInstance.Name() && externalSubnetUser.instanceDevice == deviceName {
					continue
				}
			}

			if SubnetContains(&externalSubnetUser.subnet, portExternalRoute) || SubnetContains(portExternalRoute, &externalSubnetUser.subnet) {
				// This error is purposefully vague so that it doesn't reveal any names of
				// resources potentially outside of the network's project.
				return fmt.Errorf("External subnet %q overlaps with another network or NIC", portExternalRoute.String())
			}
		}
	}

	return nil
}

// InstanceDevicePortAdd adds empty DNS record (to indicate port has been added) and any DHCP reservations for
// instance device port.
func (n *ovn) InstanceDevicePortAdd(instanceUUID string, deviceName string, deviceConfig deviceConfig.Device) error {
	instancePortName := n.getInstanceDevicePortName(instanceUUID, deviceName)

	reverter := revert.New()
	defer reverter.Fail()

	dnsUUID, err := n.ovnnb.UpdateLogicalSwitchPortDNS(context.TODO(), n.getIntSwitchName(), instancePortName, "", nil)
	if err != nil {
		return fmt.Errorf("Failed adding DNS record: %w", err)
	}

	reverter.Add(func() {
		_ = n.ovnnb.DeleteLogicalSwitchPortDNS(context.TODO(), n.getIntSwitchName(), dnsUUID, true)
	})

	// If NIC has static IPv4 address then create a DHCPv4 reservation.
	if deviceConfig["ipv4.address"] != "" {
		ip := net.ParseIP(deviceConfig["ipv4.address"])
		if ip != nil {
			dhcpReservations, err := n.ovnnb.GetLogicalSwitchDHCPv4Revervations(context.TODO(), n.getIntSwitchName())
			if err != nil {
				return fmt.Errorf("Failed getting DHCPv4 reservations: %w", err)
			}

			if !n.hasDHCPv4Reservation(dhcpReservations, ip) {
				dhcpReservations = append(dhcpReservations, iprange.Range{Start: ip})
				err = n.ovnnb.UpdateLogicalSwitchDHCPv4Revervations(context.TODO(), n.getIntSwitchName(), dhcpReservations)
				if err != nil {
					return fmt.Errorf("Failed adding DHCPv4 reservation for %q: %w", ip.String(), err)
				}
			}
		}
	}

	reverter.Success()
	return nil
}

// hasDHCPv4Reservation returns whether IP is in the supplied reservation list.
func (n *ovn) hasDHCPv4Reservation(dhcpReservations []iprange.Range, ip net.IP) bool {
	for _, dhcpReservation := range dhcpReservations {
		if dhcpReservation.Start.Equal(ip) && dhcpReservation.End == nil {
			return true
		}
	}

	return false
}

// InstanceDevicePortStart sets up an instance device port to the internal logical switch.
// Accepts a list of ACLs being removed from the NIC device (if called as part of a NIC update).
// Returns the logical switch port name and a list of IPs that were allocated to the port for DNS.
func (n *ovn) InstanceDevicePortStart(opts *OVNInstanceNICSetupOpts, securityACLsRemove []string) (networkOVN.OVNSwitchPort, []net.IP, error) {
	if opts.InstanceUUID == "" {
		return "", nil, errors.New("Instance UUID is required")
	}

	mac, err := net.ParseMAC(opts.DeviceConfig["hwaddr"])
	if err != nil {
		return "", nil, err
	}

	ipv4 := opts.DeviceConfig["ipv4.address"]
	ipv6 := opts.DeviceConfig["ipv6.address"]

	internalRoutes, externalRoutes, err := n.instanceDevicePortRoutesParse(opts.DeviceConfig)
	if err != nil {
		return "", nil, fmt.Errorf("Failed parsing NIC device routes: %w", err)
	}

	reverter := revert.New()
	defer reverter.Fail()

	// Get existing DHCPv4 static reservations.
	// This is used for both checking sticky DHCPv4 allocation availability and for ensuring static DHCP
	// reservations exist.
	dhcpReservations, err := n.ovnnb.GetLogicalSwitchDHCPv4Revervations(context.TODO(), n.getIntSwitchName())
	if err != nil {
		return "", nil, fmt.Errorf("Failed getting DHCPv4 reservations: %w", err)
	}

	dhcpv4Subnet := n.DHCPv4Subnet()
	dhcpv6Subnet := n.DHCPv6Subnet()
	var dhcpV4UUID, dhcpV6UUID networkOVN.OVNDHCPOptionsUUID

	if dhcpv4Subnet != nil || dhcpv6Subnet != nil {
		dhcpV4UUID, dhcpV6UUID, err = n.getDhcpOptionUUIDs()
		if err != nil {
			return "", nil, err
		}
	}
	if dhcpv4Subnet != nil {
		if dhcpV4UUID == "" {
			return "", nil, fmt.Errorf("Could not find DHCPv4 options for instance port for subnet %q", dhcpv4Subnet.String())
		}

		// If using dynamic IPv4, look for previously used sticky IPs from the NIC's last state.
		var dhcpV4StickyIP net.IP
		if opts.DeviceConfig["ipv4.address"] == "" {
			for _, entry := range opts.LastStateIPs {
				if entry.To4() != nil && SubnetContainsIP(dhcpv4Subnet, entry) {
					dhcpV4StickyIP = entry
					break
				}
			}
		}

		// If a previously used IP has been found and its not one of the static IPs, then check if
		// the IP is available for use and if not then we can request this port use it statically.
		if dhcpV4StickyIP != nil && dhcpV4StickyIP.String() != ipv4 {
			// If the sticky IP isn't statically reserved, lets check its not used dynamically
			// on any active port.
			if !n.hasDHCPv4Reservation(dhcpReservations, dhcpV4StickyIP) {
				existingPortIPs, err := n.ovnnb.GetLogicalSwitchIPs(context.TODO(), n.getIntSwitchName())
				if err != nil {
					return "", nil, fmt.Errorf("Failed getting existing switch port IPs: %w", err)
				}

				found := false
				for _, ips := range existingPortIPs {
					if IPInSlice(dhcpV4StickyIP, ips) {
						found = true
						break // IP is in use with another port, so cannot use it.
					}
				}

				// If IP is not in use then request OVN use previously used IP for port.
				if !found {
					ipv4 = dhcpV4StickyIP.String()
				}
			}
		}
	}

	if dhcpv6Subnet != nil {
		if dhcpV6UUID == "" {
			return "", nil, fmt.Errorf("Could not find DHCPv6 options for instance port for subnet %q", dhcpv6Subnet.String())
		}

		// If port isn't going to have fully dynamic IPs allocated by OVN, and instead only static
		// IPv4 addresses have been added, then add an EUI64 static IPv6 address so that the switch
		// port has an IPv6 address that will be used to generate a DNS record. This works around a
		// limitation in OVN that prevents us requesting dynamic IPv6 address allocation when
		// static IPv4 allocation is used.
		if ipv4 != "" && ipv6 == "" {
			eui64IP, err := eui64.ParseMAC(dhcpv6Subnet.IP, mac)
			if err != nil {
				return "", nil, fmt.Errorf("Failed generating EUI64 for instance port %q: %w", mac.String(), err)
			}

			// Add EUI64 as the IPv6 address.
			ipv6 = eui64IP.String()
		}
	}

	instancePortName := n.getInstanceDevicePortName(opts.InstanceUUID, opts.DeviceName)

	var nestedPortParentName networkOVN.OVNSwitchPort
	var nestedPortVLAN uint16
	if opts.DeviceConfig["nested"] != "" {
		nestedPortParentName = n.getInstanceDevicePortName(opts.InstanceUUID, opts.DeviceConfig["nested"])
		nestedPortVLANInt64, err := strconv.ParseUint(opts.DeviceConfig["vlan"], 10, 16)
		if err != nil {
			return "", nil, fmt.Errorf("Invalid VLAN ID %q: %w", opts.DeviceConfig["vlan"], err)
		}

		nestedPortVLAN = uint16(nestedPortVLANInt64)
	}

	// Add port with mayExist set to true, so that if instance port exists, we don't fail and continue below
	// to configure the port as needed. This is required in case the OVN northbound database was unavailable
	// when the instance NIC was stopped and was unable to remove the port on last stop, which would otherwise
	// prevent future NIC starts.
	err = n.ovnnb.CreateLogicalSwitchPort(context.TODO(), n.getIntSwitchName(), instancePortName, &networkOVN.OVNSwitchPortOpts{
		DHCPv4OptsID: dhcpV4UUID,
		DHCPv6OptsID: dhcpV6UUID,
		MAC:          mac,
		IPV4:         ipv4,
		IPV6:         ipv6,
		Parent:       nestedPortParentName,
		VLAN:         nestedPortVLAN,
		Location:     n.state.ServerName,
		Promiscuous:  util.IsTrue(opts.DeviceConfig["security.promiscuous"]),
	}, true)
	if err != nil {
		return "", nil, err
	}

	reverter.Add(func() {
		_ = n.ovnnb.DeleteLogicalSwitchPort(context.TODO(), n.getIntSwitchName(), instancePortName)
	})

	// Add DNS records for port's IPs, and retrieve the IP addresses used.
	var dnsIPv4, dnsIPv6 net.IP
	dnsIPs := make([]net.IP, 0, 2)

	// checkAndStoreIP checks if the supplied IP is valid and can be used for a missing DNS IP.
	// If the found IP is needed, stores into the relevant dnsIPv{X} variable and into dnsIPs slice.
	checkAndStoreIP := func(ip net.IP) {
		if ip != nil {
			isV4 := ip.To4() != nil
			if dnsIPv4 == nil && isV4 {
				dnsIPv4 = ip
			} else if dnsIPv6 == nil && !isV4 {
				dnsIPv6 = ip
			}

			dnsIPs = append(dnsIPs, ip)
		}
	}

	// Populate DNS IP variables with any static IPs first before checking if we need to extract dynamic IPs.
	for _, staticIP := range []string{ipv4, ipv6} {
		if staticIP == "" || staticIP == "none" {
			continue
		}

		checkAndStoreIP(net.ParseIP(staticIP))
	}

	// Apply device specific external address if any.
	for _, keyPrefix := range []string{"ipv4", "ipv6"} {
		// Check if the address is present.
		value := opts.DeviceConfig[fmt.Sprintf("%s.address.external", keyPrefix)]
		if value == "" {
			continue
		}

		// Check if the family is configured.
		if keyPrefix == "ipv4" && ipv4 == "" {
			continue
		}

		if keyPrefix == "ipv6" && ipv6 == "" {
			continue
		}

		// Parse the internal address.
		var intNet *net.IPNet
		if keyPrefix == "ipv4" {
			_, intNet, err = net.ParseCIDR(fmt.Sprintf("%s/32", ipv4))
			if err != nil {
				return "", nil, fmt.Errorf("Invalid internal address %q: %w", ipv4, err)
			}
		} else {
			_, intNet, err = net.ParseCIDR(fmt.Sprintf("%s/128", ipv6))
			if err != nil {
				return "", nil, fmt.Errorf("Invalid internal address %q: %w", ipv6, err)
			}
		}

		// Parse the external address.
		extIP := net.ParseIP(value)
		if extIP == nil {
			return "", nil, fmt.Errorf("Invalid external address %q", value)
		}

		if err := n.ovnnb.CreateLogicalRouterNAT(
			context.TODO(),
			n.getRouterName(),
			"snat",
			intNet,
			extIP,
			nil,
			false,
			true,
		); err != nil {
			return "", nil, fmt.Errorf("Failed to add SNAT %q: %w", value, err)
		}

		reverter.Add(func() {
			_ = n.ovnnb.DeleteLogicalRouterNAT(
				context.TODO(),
				n.getRouterName(),
				"snat",
				false,
				extIP,
			)
		})
	}

	// Get dynamic IPs for switch port if any IPs not assigned statically.
	if (ipv4 != "none" && dnsIPv4 == nil) || (ipv6 != "none" && dnsIPv6 == nil) {
		var dynamicIPs []net.IP

		// Retry a few times in case port has not yet allocated dynamic IPs.
		for range 40 {
			dynamicIPs, err = n.ovnnb.GetLogicalSwitchPortDynamicIPs(context.TODO(), instancePortName)
			if err == nil {
				if len(dynamicIPs) > 0 {
					break
				}
			} else if !errors.Is(err, ovsClient.ErrNotFound) {
				return "", nil, err
			}

			time.Sleep(250 * time.Millisecond)
		}

		for _, dynamicIP := range dynamicIPs {
			// Try and find the first IPv4 and IPv6 addresses from the dynamic address list.
			checkAndStoreIP(dynamicIP)
		}

		// Check, after considering all dynamic IPs, whether we have got the required ones.
		if (dnsIPv4 == nil && dhcpv4Subnet != nil) || (dnsIPv6 == nil && dhcpv6Subnet != nil) {
			return "", nil, errors.New("Insufficient dynamic addresses allocated")
		}
	}

	dnsName := fmt.Sprintf("%s.%s", opts.DNSName, n.getDomainName())
	dnsUUID, err := n.ovnnb.UpdateLogicalSwitchPortDNS(context.TODO(), n.getIntSwitchName(), instancePortName, dnsName, dnsIPs)
	if err != nil {
		return "", nil, fmt.Errorf("Failed setting DNS for %q: %w", dnsName, err)
	}

	reverter.Add(func() {
		_ = n.ovnnb.DeleteLogicalSwitchPortDNS(context.TODO(), n.getIntSwitchName(), dnsUUID, false)
	})

	// If NIC has static IPv4 address then ensure a DHCPv4 reservation exists.
	// Do this at start time as well as add time in case an instance was copied (causing a duplicate address
	// conflict at add time) which is later resolved by deleting the original instance, meaning a reservation needs to
	// be added when the copied instance next starts.
	if opts.DeviceConfig["ipv4.address"] != "" && dnsIPv4 != nil {
		if !n.hasDHCPv4Reservation(dhcpReservations, dnsIPv4) {
			dhcpReservations = append(dhcpReservations, iprange.Range{Start: dnsIPv4})
			err = n.ovnnb.UpdateLogicalSwitchDHCPv4Revervations(context.TODO(), n.getIntSwitchName(), dhcpReservations)
			if err != nil {
				return "", nil, fmt.Errorf("Failed adding DHCPv4 reservation for %q: %w", dnsIPv4.String(), err)
			}
		}
	}

	// Publish NIC's IPs on uplink network if NAT is disabled and using l2proxy ingress mode on uplink.
	if slices.Contains([]string{"l2proxy", ""}, opts.UplinkConfig["ovn.ingress_mode"]) {
		for _, k := range []string{"ipv4.nat", "ipv6.nat"} {
			if util.IsTrue(n.config[k]) {
				continue
			}

			// Select the correct destination IP from the DNS records.
			var ip net.IP
			if k == "ipv4.nat" {
				ip = dnsIPv4
			} else if k == "ipv6.nat" {
				ip = dnsIPv6
			}

			if ip == nil {
				continue // No qualifying target IP from DNS records.
			}

			err = n.ovnnb.CreateLogicalRouterNAT(context.TODO(), n.getRouterName(), "dnat_and_snat", nil, ip, ip, true, true)
			if err != nil {
				return "", nil, err
			}

			reverter.Add(func() {
				_ = n.ovnnb.DeleteLogicalRouterNAT(context.TODO(), n.getRouterName(), "dnat_and_snat", false, ip)
			})
		}
	}

	var routes []networkOVN.OVNRouterRoute

	// In l3only mode we add the instance port's IPs as static routes to the router.
	if util.IsTrue(n.config["ipv4.l3only"]) && dnsIPv4 != nil {
		ipNet := IPToNet(dnsIPv4)
		internalRoutes = append(internalRoutes, &ipNet)
	}

	if util.IsTrue(n.config["ipv6.l3only"]) && dnsIPv6 != nil {
		ipNet := IPToNet(dnsIPv6)
		internalRoutes = append(internalRoutes, &ipNet)
	}

	// Add each internal route (using the IPs set for DNS as target).
	for _, internalRoute := range internalRoutes {
		targetIP := dnsIPv4
		if internalRoute.IP.To4() == nil {
			targetIP = dnsIPv6
		}

		if targetIP == nil {
			return "", nil, fmt.Errorf("Cannot add static route for %q as target IP is not set", internalRoute.String())
		}

		routes = append(routes, networkOVN.OVNRouterRoute{
			Prefix:  *internalRoute,
			NextHop: targetIP,
			Port:    n.getRouterIntPortName(),
		})
	}

	// Add each external route (using the IPs set for DNS as target).
	for _, externalRoute := range externalRoutes {
		targetIP := dnsIPv4
		if externalRoute.IP.To4() == nil {
			targetIP = dnsIPv6
		}

		if targetIP == nil {
			return "", nil, fmt.Errorf("Cannot add static route for %q as target IP is not set", externalRoute.String())
		}

		routes = append(routes, networkOVN.OVNRouterRoute{
			Prefix:  *externalRoute,
			NextHop: targetIP,
			Port:    n.getRouterIntPortName(),
		})

		// When using l2proxy ingress mode on uplink, in order to advertise the external route to the
		// uplink network using proxy ARP/NDP we need to add a stateless dnat_and_snat rule (as to my
		// knowledge this is the only way to get the OVN router to respond to ARP/NDP requests for IPs that
		// it doesn't actually have). However we have to add each IP in the external route individually as
		// DNAT doesn't support whole subnets.
		if slices.Contains([]string{"l2proxy", ""}, opts.UplinkConfig["ovn.ingress_mode"]) {
			err = SubnetIterate(externalRoute, func(ip net.IP) error {
				err = n.ovnnb.CreateLogicalRouterNAT(context.TODO(), n.getRouterName(), "dnat_and_snat", nil, ip, ip, true, true)
				if err != nil {
					return err
				}

				reverter.Add(func() {
					_ = n.ovnnb.DeleteLogicalRouterNAT(context.TODO(), n.getRouterName(), "dnat_and_snat", false, ip)
				})

				return nil
			})
			if err != nil {
				return "", nil, err
			}
		}
	}

	if len(routes) > 0 {
		// Add routes to local router.
		err = n.ovnnb.CreateLogicalRouterRoute(context.TODO(), n.getRouterName(), true, routes...)
		if err != nil {
			return "", nil, err
		}

		routePrefixes := make([]net.IPNet, 0, len(routes))
		for _, route := range routes {
			routePrefixes = append(routePrefixes, route.Prefix)
		}

		reverter.Add(func() {
			_ = n.ovnnb.DeleteLogicalRouterRoute(context.TODO(), n.getRouterName(), routePrefixes...)
		})

		// Add routes to internal switch's address set for ACL usage.
		err = n.ovnnb.UpdateAddressSetAdd(context.TODO(), acl.OVNIntSwitchPortGroupAddressSetPrefix(n.ID()), routePrefixes...)
		if err != nil {
			return "", nil, fmt.Errorf("Failed adding switch address set entries: %w", err)
		}

		reverter.Add(func() {
			_ = n.ovnnb.UpdateAddressSetRemove(context.TODO(), acl.OVNIntSwitchPortGroupAddressSetPrefix(n.ID()), routePrefixes...)
		})

		routerIntPortIPv4, _, err := n.parseRouterIntPortIPv4Net()
		if err != nil {
			return "", nil, fmt.Errorf("Failed parsing local router's peering port IPv4 Net: %w", err)
		}

		routerIntPortIPv6, _, err := n.parseRouterIntPortIPv6Net()
		if err != nil {
			return "", nil, fmt.Errorf("Failed parsing local router's peering port IPv6 Net: %w", err)
		}

		// Add routes to peer routers, and security policies for each peer port on local router.
		err = n.forPeers(func(targetOVNNet *ovn) error {
			targetRouterName := targetOVNNet.getRouterName()
			targetRouterPort := targetOVNNet.getLogicalRouterPeerPortName(n.ID())
			targetRouterRoutes := make([]networkOVN.OVNRouterRoute, 0, len(routes))
			for _, route := range routes {
				nexthop := routerIntPortIPv4
				if route.Prefix.IP.To4() == nil {
					nexthop = routerIntPortIPv6
				}

				if nexthop == nil {
					continue // Skip routes that cannot be supported by local router.
				}

				targetRouterRoutes = append(targetRouterRoutes, networkOVN.OVNRouterRoute{
					Prefix:  route.Prefix,
					NextHop: nexthop,
					Port:    targetRouterPort,
				})
			}

			err = n.ovnnb.CreateLogicalRouterRoute(context.TODO(), targetRouterName, true, targetRouterRoutes...)
			if err != nil {
				return fmt.Errorf("Failed adding static routes to peer network %q in project %q: %w", targetOVNNet.Name(), targetOVNNet.Project(), err)
			}

			reverter.Add(func() { _ = n.ovnnb.DeleteLogicalRouterRoute(context.TODO(), targetRouterName, routePrefixes...) })

			return nil
		})
		if err != nil {
			return "", nil, err
		}
	}

	// Merge network and NIC assigned security ACL lists.
	netACLNames := util.SplitNTrimSpace(n.config["security.acls"], ",", -1, true)
	nicACLNames := util.SplitNTrimSpace(opts.DeviceConfig["security.acls"], ",", -1, true)

	for _, aclName := range netACLNames {
		if !slices.Contains(nicACLNames, aclName) {
			nicACLNames = append(nicACLNames, aclName)
		}
	}

	// Apply Security ACL port group settings.
	addChangeSet := map[networkOVN.OVNPortGroup][]networkOVN.OVNSwitchPortUUID{}
	removeChangeSet := map[networkOVN.OVNPortGroup][]networkOVN.OVNSwitchPortUUID{}

	// Get logical port UUID.
	portUUID, err := n.ovnnb.GetLogicalSwitchPortUUID(context.TODO(), instancePortName)
	if err != nil || portUUID == "" {
		return "", nil, fmt.Errorf("Failed getting logical port UUID for security ACL removal: %w", err)
	}

	// Add NIC port to network port group (this includes the port in the @internal subject for ACL rules).
	acl.OVNPortGroupInstanceNICSchedule(portUUID, addChangeSet, acl.OVNIntSwitchPortGroupName(n.ID()))
	n.logger.Debug("Scheduled logical port for network port group addition", logger.Ctx{"portGroup": acl.OVNIntSwitchPortGroupName(n.ID()), "port": instancePortName})

	if len(nicACLNames) > 0 || len(securityACLsRemove) > 0 {
		var aclNameIDs map[string]int64

		err = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
			// Get map of ACL names to DB IDs (used for generating OVN port group names).
			acls, err := dbCluster.GetNetworkACLs(ctx, tx.Tx(), dbCluster.NetworkACLFilter{Project: &n.project})
			if err != nil {
				return err
			}

			aclNameIDs = make(map[string]int64, len(acls))
			for _, acl := range acls {
				aclNameIDs[acl.Name] = int64(acl.ID)
			}

			return nil
		})
		if err != nil {
			return "", nil, fmt.Errorf("Failed getting network ACL IDs for security ACL setup: %w", err)
		}

		// Add port to ACLs requested.
		if len(nicACLNames) > 0 {
			// Request our network is setup with the specified ACLs.
			aclNets := map[string]acl.NetworkACLUsage{
				n.Name(): {Name: n.Name(), Type: n.Type(), ID: n.ID(), Config: n.Config()},
			}

			cleanup, err := addressset.OVNEnsureAddressSetsViaACLs(n.state, n.logger, n.ovnnb, n.Project(), nicACLNames)
			if err != nil {
				return "", nil, fmt.Errorf("Failed ensuring address sets for nic ACLs are configured in OVN for network: %w", err)
			}

			reverter.Add(cleanup)
			cleanup, err = acl.OVNEnsureACLs(n.state, n.logger, n.ovnnb, n.Project(), aclNameIDs, aclNets, nicACLNames, false)
			if err != nil {
				return "", nil, fmt.Errorf("Failed ensuring security ACLs are configured in OVN for instance: %w", err)
			}

			reverter.Add(cleanup)

			for _, aclName := range nicACLNames {
				aclID, found := aclNameIDs[aclName]
				if !found {
					return "", nil, fmt.Errorf("Cannot find security ACL ID for %q", aclName)
				}

				// Add NIC port to ACL port group.
				portGroupName := acl.OVNACLPortGroupName(aclID)
				acl.OVNPortGroupInstanceNICSchedule(portUUID, addChangeSet, portGroupName)
				n.logger.Debug("Scheduled logical port for ACL port group addition", logger.Ctx{"networkACL": aclName, "portGroup": portGroupName, "port": instancePortName})
			}
		}

		// Remove port from ACLs requested.
		for _, aclName := range securityACLsRemove {
			// Don't remove ACLs that are in the add ACLs list (there are possibly added from
			// the network assigned ACLs).
			if slices.Contains(nicACLNames, aclName) {
				continue
			}

			aclID, found := aclNameIDs[aclName]
			if !found {
				return "", nil, fmt.Errorf("Cannot find security ACL ID for %q", aclName)
			}

			// Remove NIC port from ACL port group.
			portGroupName := acl.OVNACLPortGroupName(aclID)
			acl.OVNPortGroupInstanceNICSchedule(portUUID, removeChangeSet, portGroupName)
			n.logger.Debug("Scheduled logical port for ACL port group removal", logger.Ctx{"networkACL": aclName, "portGroup": portGroupName, "port": instancePortName})
		}
	}

	// Add instance NIC switch port to port groups required. Always run this as the addChangeSet should always
	// be populated even if no ACLs being applied, because the NIC port needs to be added to the network level
	// port group.
	n.logger.Debug("Applying instance NIC port group member change sets")
	err = n.ovnnb.UpdatePortGroupMembers(context.TODO(), addChangeSet, removeChangeSet)
	if err != nil {
		return "", nil, fmt.Errorf("Failed applying OVN port group member change sets for instance NIC: %w", err)
	}

	// Set the automatic default ACL rule for the port.
	if len(nicACLNames) > 0 {
		ingressAction, ingressLogged := n.instanceDeviceACLDefaults(opts.DeviceConfig, "ingress")
		egressAction, egressLogged := n.instanceDeviceACLDefaults(opts.DeviceConfig, "egress")

		logPrefix := fmt.Sprintf("%s-%s", opts.InstanceUUID, opts.DeviceName)
		err = acl.OVNApplyInstanceNICDefaultRules(n.ovnnb, acl.OVNIntSwitchPortGroupName(n.ID()), logPrefix, instancePortName, ingressAction, ingressLogged, egressAction, egressLogged)
		if err != nil {
			return "", nil, fmt.Errorf("Failed applying OVN default ACL rules for instance NIC: %w", err)
		}

		n.logger.Debug("Set NIC default rule", logger.Ctx{"port": instancePortName, "ingressAction": ingressAction, "ingressLogged": ingressLogged, "egressAction": egressAction, "egressLogged": egressLogged})
	} else {
		err = n.ovnnb.ClearPortGroupPortACLRules(context.TODO(), acl.OVNIntSwitchPortGroupName(n.ID()), instancePortName)
		if err != nil {
			return "", nil, fmt.Errorf("Failed clearing OVN default ACL rules for instance NIC: %w", err)
		}

		err := addressset.OVNAddressSetsDeleteIfUnused(n.state, n.logger, n.ovnnb, n.Project())
		if err != nil {
			return "", nil, fmt.Errorf("Failed removing unused OVN address sets: %w", err)
		}

		n.logger.Debug("Cleared NIC default rule", logger.Ctx{"port": instancePortName})
	}

	reverter.Success()
	return instancePortName, dnsIPs, nil
}

// instanceDeviceACLDefaults returns the action and logging mode to use for the specified direction's default rule.
// If the security.acls.default.{in,e}gress.action or security.acls.default.{in,e}gress.logged settings are not
// specified in the NIC device config, then the settings on the network are used, and if not specified there then
// it returns "reject" and false respectively.
func (n *ovn) instanceDeviceACLDefaults(deviceConfig deviceConfig.Device, direction string) (string, bool) {
	defaults := map[string]string{
		fmt.Sprintf("security.acls.default.%s.action", direction): "reject",
		fmt.Sprintf("security.acls.default.%s.logged", direction): "false",
	}

	for k := range defaults {
		if deviceConfig[k] != "" {
			defaults[k] = deviceConfig[k]
		} else if n.config[k] != "" {
			defaults[k] = n.config[k]
		}
	}

	return defaults[fmt.Sprintf("security.acls.default.%s.action", direction)], util.IsTrue(defaults[fmt.Sprintf("security.acls.default.%s.logged", direction)])
}

// InstanceDevicePortIPs returns the allocated IPs for a device port.
func (n *ovn) InstanceDevicePortIPs(instanceUUID string, deviceName string) ([]net.IP, error) {
	if instanceUUID == "" {
		return nil, errors.New("Instance UUID is required")
	}

	instancePortName := n.getInstanceDevicePortName(instanceUUID, deviceName)

	devIPs, err := n.ovnnb.GetLogicalSwitchPortIPs(context.TODO(), instancePortName)
	if err != nil {
		return nil, fmt.Errorf("Failed to get OVN switch port IPs: %w", err)
	}

	return devIPs, nil
}

// InstanceDevicePortStop deletes an instance device port from the internal logical switch.
func (n *ovn) InstanceDevicePortStop(ovsExternalOVNPort networkOVN.OVNSwitchPort, opts *OVNInstanceNICStopOpts) error {
	// Decide whether to use OVS provided OVN port name or internally derived OVN port name.
	instancePortName := ovsExternalOVNPort
	source := "OVS"
	if ovsExternalOVNPort == "" {
		if opts.InstanceUUID == "" {
			return errors.New("Instance UUID is required")
		}

		instancePortName = n.getInstanceDevicePortName(opts.InstanceUUID, opts.DeviceName)
		source = "internal"
	}

	portLocation, err := n.ovnnb.GetLogicalSwitchPortLocation(context.TODO(), instancePortName)
	if err != nil {
		return fmt.Errorf("Failed getting instance switch port options: %w", err)
	}

	// Don't delete logical switch port if already active on another chassis (i.e during live cluster move).
	if portLocation != "" && portLocation != n.state.ServerName {
		return nil
	}

	n.logger.Debug("Deleting instance port", logger.Ctx{"port": instancePortName, "source": source})

	internalRoutes, externalRoutes, err := n.instanceDevicePortRoutesParse(opts.DeviceConfig)
	if err != nil {
		return fmt.Errorf("Failed parsing NIC device routes: %w", err)
	}

	var uplink *api.Network

	err = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		// Load uplink network config.
		_, uplink, _, err = tx.GetNetworkInAnyState(ctx, api.ProjectDefaultName, n.config["network"])

		return err
	})
	if err != nil {
		return fmt.Errorf("Failed to load uplink network %q: %w", n.config["network"], err)
	}

	// Get DNS records.
	dnsUUID, _, dnsIPs, err := n.ovnnb.GetLogicalSwitchPortDNS(context.TODO(), instancePortName)
	if err != nil {
		return err
	}

	// Cleanup logical switch port and associated config.
	err = n.ovnnb.CleanupLogicalSwitchPort(context.TODO(), instancePortName, n.getIntSwitchName(), acl.OVNIntSwitchPortGroupName(n.ID()), dnsUUID)
	if err != nil {
		return err
	}

	var removeRoutes []net.IPNet
	var removeNATIPs []net.IP

	if len(dnsIPs) > 0 {
		// When using l3only mode the instance port's IPs are added as static routes to the router.
		// So try and remove these in case l3only is (or was) being used.
		for _, dnsIP := range dnsIPs {
			removeRoutes = append(removeRoutes, IPToNet(dnsIP))
		}

		// Delete any associated external IP DNAT rules for the DNS IPs.
		removeNATIPs = append(removeNATIPs, dnsIPs...)
	}

	// Delete internal routes.
	if len(internalRoutes) > 0 {
		for _, internalRoute := range internalRoutes {
			removeRoutes = append(removeRoutes, *internalRoute)
		}
	}

	// Delete external routes.
	for _, externalRoute := range externalRoutes {
		removeRoutes = append(removeRoutes, *externalRoute)

		// Remove the DNAT rules when using l2proxy ingress mode on uplink.
		if slices.Contains([]string{"l2proxy", ""}, uplink.Config["ovn.ingress_mode"]) {
			err = SubnetIterate(externalRoute, func(ip net.IP) error {
				removeNATIPs = append(removeNATIPs, ip)

				return nil
			})
			if err != nil {
				return err
			}
		}
	}

	if len(removeRoutes) > 0 {
		// Delete routes from local router.
		err = n.ovnnb.DeleteLogicalRouterRoute(context.TODO(), n.getRouterName(), removeRoutes...)
		if err != nil {
			return err
		}

		// Delete routes from switch address set.
		err = n.ovnnb.UpdateAddressSetRemove(context.TODO(), acl.OVNIntSwitchPortGroupAddressSetPrefix(n.ID()), removeRoutes...)
		if err != nil {
			return fmt.Errorf("Failed deleting switch address set entries: %w", err)
		}

		// Delete routes from peer routers.
		err = n.forPeers(func(targetOVNNet *ovn) error {
			targetRouterName := targetOVNNet.getRouterName()
			err = n.ovnnb.DeleteLogicalRouterRoute(context.TODO(), targetRouterName, removeRoutes...)
			if err != nil {
				return fmt.Errorf("Failed deleting static routes from peer network %q in project %q: %w", targetOVNNet.Name(), targetOVNNet.Project(), err)
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	if len(removeNATIPs) > 0 {
		err = n.ovnnb.DeleteLogicalRouterNAT(context.TODO(), n.getRouterName(), "dnat_and_snat", false, removeNATIPs...)
		if err != nil {
			return err
		}
	}

	// Tear down per‑NIC egress SNAT rules (ipv4/ipv6.address.external)
	for _, keyPrefix := range []string{"ipv4", "ipv6"} {
		// Check if the address is present.
		value := opts.DeviceConfig[fmt.Sprintf("%s.address.external", keyPrefix)]
		if value == "" {
			continue
		}

		// Validate the address.
		extIP := net.ParseIP(value)
		if extIP == nil {
			return fmt.Errorf("Invalid external address %q", value)
		}

		// Remove the SNAT entry.
		err := n.ovnnb.DeleteLogicalRouterNAT(context.TODO(), n.getRouterName(), "snat", false, extIP)
		if err != nil && !errors.Is(err, networkOVN.ErrNotFound) {
			return err
		}
	}

	return nil
}

// InstanceDevicePortRemove unregisters the NIC device in the OVN database by removing the DNS entry that should
// have been created during InstanceDevicePortAdd(). If the DNS record exists at remove time then this indicates
// the NIC device was successfully added and this function also clears any DHCP reservations for the NIC's IPs.
func (n *ovn) InstanceDevicePortRemove(instanceUUID string, deviceName string, deviceConfig deviceConfig.Device) error {
	instancePortName := n.getInstanceDevicePortName(instanceUUID, deviceName)

	reverter := revert.New()
	defer reverter.Fail()

	// Get DNS records.
	dnsUUID, _, _, err := n.ovnnb.GetLogicalSwitchPortDNS(context.TODO(), instancePortName)
	if err != nil {
		return err
	}

	// Remove DNS record if exists.
	if dnsUUID != "" {
		// If NIC has static IPv4 address then remove the DHCPv4 reservation.
		if deviceConfig["ipv4.address"] != "" {
			ip := net.ParseIP(deviceConfig["ipv4.address"])
			if ip != nil {
				dhcpReservations, err := n.ovnnb.GetLogicalSwitchDHCPv4Revervations(context.TODO(), n.getIntSwitchName())
				if err != nil {
					return fmt.Errorf("Failed getting DHCPv4 reservations: %w", err)
				}

				dhcpReservations = append(dhcpReservations, iprange.Range{Start: ip})
				dhcpReservationsNew := make([]iprange.Range, 0, len(dhcpReservations))

				found := false
				for _, dhcpReservation := range dhcpReservations {
					if dhcpReservation.Start.Equal(ip) && dhcpReservation.End == nil {
						found = true
						continue
					}

					dhcpReservationsNew = append(dhcpReservationsNew, dhcpReservation)
				}

				if found {
					err = n.ovnnb.UpdateLogicalSwitchDHCPv4Revervations(context.TODO(), n.getIntSwitchName(), dhcpReservationsNew)
					if err != nil {
						return fmt.Errorf("Failed removing DHCPv4 reservation for %q: %w", ip.String(), err)
					}
				}
			}
		}

		err = n.ovnnb.DeleteLogicalSwitchPortDNS(context.TODO(), n.getIntSwitchName(), dnsUUID, true)
		if err != nil {
			return fmt.Errorf("Failed deleting DNS record: %w", err)
		}
	}

	reverter.Success()
	return nil
}

// DHCPv4Subnet returns the DHCPv4 subnet (if DHCP is enabled on network).
func (n *ovn) DHCPv4Subnet() *net.IPNet {
	// DHCP is disabled on this network (an empty ipv4.dhcp setting indicates enabled by default).
	if util.IsFalse(n.config["ipv4.dhcp"]) {
		return nil
	}

	_, subnet, err := n.parseRouterIntPortIPv4Net()
	if err != nil {
		return nil
	}

	return subnet
}

// DHCPv6Subnet returns the DHCPv6 subnet (if DHCP or SLAAC is enabled on network).
func (n *ovn) DHCPv6Subnet() *net.IPNet {
	// DHCP is disabled on this network (an empty ipv6.dhcp setting indicates enabled by default).
	if util.IsFalse(n.config["ipv6.dhcp"]) {
		return nil
	}

	_, subnet, err := n.parseRouterIntPortIPv6Net()
	if err != nil {
		return nil
	}

	if subnet != nil {
		ones, _ := subnet.Mask.Size()
		if ones < 64 {
			return nil // OVN only supports DHCPv6 allocated using EUI64 (which needs at least a /64).
		}
	}

	return subnet
}

// ovnNetworkExternalSubnets returns a list of external subnets used by OVN networks using the same uplink as this
// OVN network. OVN networks are considered to be using external subnets for their ipv4.address and/or ipv6.address
// if they have NAT disabled, and/or if they have external NAT addresses specified.
func (n *ovn) ovnNetworkExternalSubnets(ovnProjectNetworksWithOurUplink map[string][]*api.Network) ([]externalSubnetUsage, error) {
	externalSubnets := make([]externalSubnetUsage, 0)
	for netProject, networks := range ovnProjectNetworksWithOurUplink {
		for _, netInfo := range networks {
			for _, keyPrefix := range []string{"ipv4", "ipv6"} {
				// If NAT is disabled, then network subnet is an external subnet.
				if util.IsFalseOrEmpty(netInfo.Config[fmt.Sprintf("%s.nat", keyPrefix)]) {
					key := fmt.Sprintf("%s.address", keyPrefix)

					_, ipNet, err := net.ParseCIDR(netInfo.Config[key])
					if err != nil {
						continue // Skip invalid/unspecified network addresses.
					}

					externalSubnets = append(externalSubnets, externalSubnetUsage{
						subnet:         *ipNet,
						networkProject: netProject,
						networkName:    netInfo.Name,
						usageType:      subnetUsageNetwork,
					})
				}

				// Find any external subnets used for network SNAT.
				if netInfo.Config[fmt.Sprintf("%s.nat.address", keyPrefix)] != "" {
					key := fmt.Sprintf("%s.nat.address", keyPrefix)

					subnetSize := 128
					if keyPrefix == "ipv4" {
						subnetSize = 32
					}

					_, ipNet, err := net.ParseCIDR(fmt.Sprintf("%s/%d", netInfo.Config[key], subnetSize))
					if err != nil {
						return nil, fmt.Errorf("Failed parsing %q of %q in project %q: %w", key, netInfo.Name, netProject, err)
					}

					externalSubnets = append(externalSubnets, externalSubnetUsage{
						subnet:         *ipNet,
						networkProject: netProject,
						networkName:    netInfo.Name,
						usageType:      subnetUsageNetworkSNAT,
					})
				}
			}
		}
	}

	return externalSubnets, nil
}

// ovnNICExternalRoutes returns a list of external routes currently used by OVN NICs that are connected to OVN
// networks that share the same uplink as this network uses.
func (n *ovn) ovnNICExternalRoutes(ovnProjectNetworksWithOurUplink map[string][]*api.Network) ([]externalSubnetUsage, error) {
	externalRoutes := make([]externalSubnetUsage, 0)

	err := n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		return tx.InstanceList(ctx, func(inst db.InstanceArgs, p api.Project) error {
			// Get the instance's effective network project name.
			instNetworkProject := project.NetworkProjectFromRecord(&p)
			devices := db.ExpandInstanceDevices(inst.Devices.Clone(), inst.Profiles)

			// Iterate through each of the instance's devices, looking for OVN NICs that are linked to networks
			// that use our uplink.
			for devName, devConfig := range devices {
				if devConfig["type"] != "nic" {
					continue
				}

				// Check whether the NIC device references one of the OVN networks supplied.
				if !NICUsesNetwork(devConfig, ovnProjectNetworksWithOurUplink[instNetworkProject]...) {
					continue
				}

				// For OVN NICs that are connected to networks that use the same uplink as we do, check
				// if they have any external routes configured, and if so add them to the list to return.
				for _, key := range []string{"ipv4.routes.external", "ipv6.routes.external"} {
					for _, cidr := range util.SplitNTrimSpace(devConfig[key], ",", -1, true) {
						_, ipNet, _ := net.ParseCIDR(cidr)
						if ipNet == nil {
							// Sip if NIC device doesn't have a valid route.
							continue
						}

						externalRoutes = append(externalRoutes, externalSubnetUsage{
							subnet:          *ipNet,
							networkProject:  instNetworkProject,
							networkName:     devConfig["network"],
							instanceProject: inst.Project,
							instanceName:    inst.Name,
							instanceDevice:  devName,
							usageType:       subnetUsageInstance,
						})
					}
				}
			}

			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	return externalRoutes, nil
}

// ovnProjectNetworksWithUplink accepts a map of all networks in all projects and returns a filtered map of OVN
// networks that use the uplink specified.
func (n *ovn) ovnProjectNetworksWithUplink(uplink string, projectNetworks map[string]map[int64]api.Network) map[string][]*api.Network {
	ovnProjectNetworksWithOurUplink := make(map[string][]*api.Network)
	for netProject, networks := range projectNetworks {
		for _, ni := range networks {
			network := ni // Local var creating pointer to rather than iterator.

			// Skip non-OVN networks or those networks that don't use the uplink specified.
			if network.Type != "ovn" || network.Config["network"] != uplink {
				continue
			}

			if ovnProjectNetworksWithOurUplink[netProject] == nil {
				ovnProjectNetworksWithOurUplink[netProject] = []*api.Network{&network}
			} else {
				ovnProjectNetworksWithOurUplink[netProject] = append(ovnProjectNetworksWithOurUplink[netProject], &network)
			}
		}
	}

	return ovnProjectNetworksWithOurUplink
}

// uplinkHasIngressRoutedAnycastIPv4 returns true if the uplink network has IPv4 routed ingress anycast enabled.
func (n *ovn) uplinkHasIngressRoutedAnycastIPv4(uplink *api.Network) bool {
	return util.IsTrue(uplink.Config["ipv4.routes.anycast"]) && uplink.Config["ovn.ingress_mode"] == "routed"
}

// uplinkHasIngressRoutedAnycastIPv6 returns true if the uplink network has routed IPv6 ingress anycast enabled.
func (n *ovn) uplinkHasIngressRoutedAnycastIPv6(uplink *api.Network) bool {
	return util.IsTrue(uplink.Config["ipv6.routes.anycast"]) && uplink.Config["ovn.ingress_mode"] == "routed"
}

// handleDependencyChange applies changes from uplink network if specific watched keys have changed.
func (n *ovn) handleDependencyChange(uplinkName string, uplinkConfig map[string]string, changedKeys []string) error {
	// Detect changes that need to be applied to the network.
	for _, k := range []string{"dns.nameservers", "ipv4.gateway", "ipv6.gateway", "ipv4.gateway.hwaddr", "ipv6.gateway.hwaddr"} {
		if slices.Contains(changedKeys, k) {
			n.logger.Debug("Applying changes from uplink network", logger.Ctx{"uplink": uplinkName})

			// Re-setup logical network in order to apply uplink changes.
			err := n.setup(true)
			if err != nil {
				return err
			}

			break // Only run setup once per notification (all changes will be applied).
		}
	}

	// Add or remove the instance NIC l2proxy DNAT_AND_SNAT rules if uplink's ovn.ingress_mode has changed.
	if slices.Contains(changedKeys, "ovn.ingress_mode") {
		n.logger.Debug("Applying ingress mode changes from uplink network to instance NICs", logger.Ctx{"uplink": uplinkName})

		if slices.Contains([]string{"l2proxy", ""}, uplinkConfig["ovn.ingress_mode"]) {
			// Get list of active switch ports (avoids repeated querying of OVN NB).
			activePorts, err := n.ovnnb.GetLogicalSwitchPorts(context.TODO(), n.getIntSwitchName())
			if err != nil {
				return fmt.Errorf("Failed getting active ports: %w", err)
			}

			// Find all instance NICs that use this network, and re-add the logical OVN instance port.
			// This will restore the l2proxy DNAT_AND_SNAT rules.
			err = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
				return tx.InstanceList(ctx, func(inst db.InstanceArgs, p api.Project) error {
					// Get the instance's effective network project name.
					instNetworkProject := project.NetworkProjectFromRecord(&p)

					// Skip instances who's effective network project doesn't match this network's
					// project.
					if n.Project() != instNetworkProject {
						return nil
					}

					devices := db.ExpandInstanceDevices(inst.Devices.Clone(), inst.Profiles)

					// Iterate through each of the instance's devices, looking for NICs that are linked
					// this network.
					for devName, devConfig := range devices {
						if devConfig["type"] != "nic" || n.Name() != devConfig["network"] {
							continue
						}

						// Check if instance port exists, if not then we can skip.
						instanceUUID := inst.Config["volatile.uuid"]
						instancePortName := n.getInstanceDevicePortName(instanceUUID, devName)
						_, found := activePorts[instancePortName]
						if !found {
							continue // No need to update a port that isn't started yet.
						}

						if devConfig["hwaddr"] == "" {
							// Load volatile MAC if no static MAC specified.
							devConfig["hwaddr"] = inst.Config[fmt.Sprintf("volatile.%s.hwaddr", devName)]
						}

						// Re-add logical switch port to apply the l2proxy DNAT_AND_SNAT rules.
						n.logger.Debug("Re-adding instance OVN NIC port to apply ingress mode changes", logger.Ctx{"project": inst.Project, "instance": inst.Name, "device": devName})
						_, _, err = n.InstanceDevicePortStart(&OVNInstanceNICSetupOpts{
							InstanceUUID: instanceUUID,
							DNSName:      inst.Name,
							DeviceName:   devName,
							DeviceConfig: devConfig,
							UplinkConfig: uplinkConfig,
						}, nil)
						if err != nil {
							n.logger.Error("Failed re-adding instance OVN NIC port", logger.Ctx{"project": inst.Project, "instance": inst.Name, "err": err})
							continue
						}
					}

					return nil
				})
			})
			if err != nil {
				return fmt.Errorf("Failed adding instance NIC ingress mode l2proxy rules: %w", err)
			}
		} else {
			// Remove all DNAT_AND_SNAT rules if not using l2proxy ingress mode, as currently we only
			// use DNAT_AND_SNAT rules for this feature so it is safe to do.
			err := n.ovnnb.DeleteLogicalRouterNAT(context.TODO(), n.getRouterName(), "dnat_and_snat", true)
			if err != nil {
				return fmt.Errorf("Failed deleting instance NIC ingress mode l2proxy rules: %w", err)
			}
		}
	}

	return nil
}

// forwardFlattenVIPs flattens forwards into format compatible with OVN load balancers.
func (n *ovn) forwardFlattenVIPs(listenAddress net.IP, defaultTargetAddress net.IP, portMaps []*forwardPortMap) []networkOVN.OVNLoadBalancerVIP {
	var vips []networkOVN.OVNLoadBalancerVIP

	if defaultTargetAddress != nil {
		vips = append(vips, networkOVN.OVNLoadBalancerVIP{
			ListenAddress: listenAddress,
			Targets:       []networkOVN.OVNLoadBalancerTarget{{Address: defaultTargetAddress}},
		})
	}

	for _, portMap := range portMaps {
		targetPortsLen := len(portMap.target.ports)

		for i, lp := range portMap.listenPorts {
			targetPort := lp // Default to using same port as listen port for target port.

			if targetPortsLen == 1 {
				// If a single target port is specified, forward all listen ports to it.
				targetPort = portMap.target.ports[0]
			} else if targetPortsLen > 1 {
				// If more than 1 target port specified, use listen port index to get the
				// target port to use.
				targetPort = portMap.target.ports[i]
			}

			vips = append(vips, networkOVN.OVNLoadBalancerVIP{
				ListenAddress: listenAddress,
				Protocol:      portMap.protocol,
				ListenPort:    lp,
				Targets: []networkOVN.OVNLoadBalancerTarget{
					{
						Address: portMap.target.address,
						Port:    targetPort,
					},
				},
			})
		}
	}

	return vips
}

// ForwardCreate creates a network forward.
func (n *ovn) ForwardCreate(forward api.NetworkForwardsPost, clientType request.ClientType) error {
	if n.config["network"] == "none" {
		return errors.New("Isolated OVN network cannot use network forwards")
	}

	reverter := revert.New()
	defer reverter.Fail()

	if clientType == request.ClientTypeNormal {
		memberSpecific := false // OVN doesn't support per-member forwards.

		err := n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
			// Check if there is an existing forward using the same listen address.
			_, err := dbCluster.GetNetworkForward(ctx, tx.Tx(), n.ID(), forward.ListenAddress)

			return err
		})
		if err == nil {
			return api.StatusErrorf(http.StatusConflict, "A forward for that listen address already exists")
		}

		// Convert listen address to subnet so we can check its valid and can be used.
		listenAddressNet, err := ParseIPToNet(forward.ListenAddress)
		if err != nil {
			return fmt.Errorf("Failed parsing %q: %w", forward.ListenAddress, err)
		}

		portMaps, err := n.forwardValidate(listenAddressNet.IP, &forward.NetworkForwardPut)
		if err != nil {
			return err
		}

		// Load the project to get uplink network restrictions.
		var p *api.Project
		var uplink *api.Network

		err = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
			project, err := dbCluster.GetProject(ctx, tx.Tx(), n.project)
			if err != nil {
				return fmt.Errorf("Failed to load network restrictions from project %q: %w", n.project, err)
			}

			p, err = project.ToAPI(ctx, tx.Tx())
			if err != nil {
				return fmt.Errorf("Failed to load network restrictions from project %q: %w", n.project, err)
			}

			// Get uplink routes.
			_, uplink, _, err = tx.GetNetworkInAnyState(ctx, api.ProjectDefaultName, n.config["network"])
			if err != nil {
				return fmt.Errorf("Failed to load uplink network %q: %w", n.config["network"], err)
			}

			return nil
		})
		if err != nil {
			return err
		}

		// Get project restricted routes.
		projectRestrictedSubnets, err := n.projectRestrictedSubnets(p, n.config["network"])
		if err != nil {
			return err
		}

		externalSubnetsInUse, err := n.getExternalSubnetInUse(n.config["network"])
		if err != nil {
			return err
		}

		// Check the listen address subnet is allowed within both the uplink's external routes and any
		// project restricted subnets.
		err = n.validateExternalSubnet(uplink, projectRestrictedSubnets, listenAddressNet)
		if err != nil {
			return err
		}

		// Check the listen address subnet doesn't fall within any existing OVN network external subnets.
		for _, externalSubnetUser := range externalSubnetsInUse {
			// Check if usage is from our own network.
			if externalSubnetUser.networkProject == n.project && externalSubnetUser.networkName == n.name {
				// Skip checking conflict with our own network's subnet or SNAT address.
				// But do not allow other conflict with other usage types within our own network.
				if externalSubnetUser.usageType == subnetUsageNetwork || externalSubnetUser.usageType == subnetUsageNetworkSNAT {
					continue
				}
			}

			if SubnetContains(&externalSubnetUser.subnet, listenAddressNet) || SubnetContains(listenAddressNet, &externalSubnetUser.subnet) {
				// This error is purposefully vague so that it doesn't reveal any names of
				// resources potentially outside of the network's project.
				return fmt.Errorf("Forward listen address %q overlaps with another network or NIC", listenAddressNet.String())
			}
		}

		var forwardID int64

		err = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
			// Create forward DB record.
			nodeID := sql.NullInt64{
				Valid: memberSpecific,
				Int64: tx.GetNodeID(),
			}

			dbRecord := dbCluster.NetworkForward{
				NetworkID:     n.ID(),
				NodeID:        nodeID,
				ListenAddress: forward.ListenAddress,
				Description:   forward.Description,
				Ports:         forward.Ports,
			}

			if forward.Ports == nil {
				dbRecord.Ports = []api.NetworkForwardPort{}
			}

			forwardID, err = dbCluster.CreateNetworkForward(ctx, tx.Tx(), dbRecord)
			if err != nil {
				return err
			}

			err = dbCluster.CreateNetworkForwardConfig(ctx, tx.Tx(), forwardID, forward.Config)
			if err != nil {
				return err
			}

			return nil
		})
		if err != nil {
			return err
		}

		reverter.Add(func() {
			_ = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
				return dbCluster.DeleteNetworkForward(ctx, tx.Tx(), n.ID(), forwardID)
			})

			_ = n.ovnnb.DeleteLoadBalancer(context.TODO(), n.getLoadBalancerName(forward.ListenAddress))
			_ = n.forwardBGPSetupPrefixes()
		})

		vips := n.forwardFlattenVIPs(net.ParseIP(forward.ListenAddress), net.ParseIP(forward.Config["target_address"]), portMaps)

		err = n.ovnnb.CreateLoadBalancer(context.TODO(), n.getLoadBalancerName(forward.ListenAddress), n.getRouterName(), n.getIntSwitchName(), vips...)
		if err != nil {
			return fmt.Errorf("Failed applying OVN load balancer: %w", err)
		}

		// Add internal static route to the network forward (helps with OVN IC).
		var nexthop net.IP
		if listenAddressNet.IP.To4() == nil {
			routerV6, _, err := n.parseRouterIntPortIPv6Net()
			if err == nil {
				nexthop = routerV6
			}
		} else {
			routerV4, _, err := n.parseRouterIntPortIPv4Net()
			if err == nil {
				nexthop = routerV4
			}
		}

		if nexthop != nil {
			err = n.ovnnb.CreateLogicalRouterRoute(context.TODO(), n.getRouterName(), true, networkOVN.OVNRouterRoute{NextHop: nexthop, Prefix: *listenAddressNet})
			if err != nil {
				return err
			}

			reverter.Add(func() {
				_ = n.ovnnb.DeleteLogicalRouterRoute(context.TODO(), n.getRouterName(), *listenAddressNet)
			})
		}

		// Notify all other members to refresh their BGP prefixes.
		notifier, err := cluster.NewNotifier(n.state, n.state.Endpoints.NetworkCert(), n.state.ServerCert(), cluster.NotifyAll)
		if err != nil {
			return err
		}

		err = notifier(func(client incus.InstanceServer) error {
			return client.UseProject(n.project).CreateNetworkForward(n.name, forward)
		})
		if err != nil {
			return err
		}
	}

	// Refresh exported BGP prefixes on local member.
	err := n.forwardBGPSetupPrefixes()
	if err != nil {
		return fmt.Errorf("Failed applying BGP prefixes for address forwards: %w", err)
	}

	reverter.Success()
	return nil
}

// ForwardUpdate updates a network forward.
func (n *ovn) ForwardUpdate(listenAddress string, req api.NetworkForwardPut, clientType request.ClientType) error {
	reverter := revert.New()
	defer reverter.Fail()

	if clientType == request.ClientTypeNormal {
		var curForwardID int64
		var curForward *api.NetworkForward

		err := n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
			var err error

			// No memberSpecific filtering needed because OVN doesn't support per-member-forwards
			dbRecord, err := dbCluster.GetNetworkForward(ctx, tx.Tx(), n.ID(), listenAddress)
			if err != nil {
				return err
			}

			curForwardID = dbRecord.ID
			curForward, err = dbRecord.ToAPI(ctx, tx.Tx())
			if err != nil {
				return err
			}

			return nil
		})
		if err != nil {
			return err
		}

		portMaps, err := n.forwardValidate(net.ParseIP(curForward.ListenAddress), &req)
		if err != nil {
			return err
		}

		curForwardEtagHash, err := localUtil.EtagHash(curForward.Etag())
		if err != nil {
			return err
		}

		newForward := api.NetworkForward{
			ListenAddress:     curForward.ListenAddress,
			NetworkForwardPut: req,
		}

		newForwardEtagHash, err := localUtil.EtagHash(newForward.Etag())
		if err != nil {
			return err
		}

		if curForwardEtagHash == newForwardEtagHash {
			return nil // Nothing has changed.
		}

		vips := n.forwardFlattenVIPs(net.ParseIP(newForward.ListenAddress), net.ParseIP(newForward.Config["target_address"]), portMaps)
		err = n.ovnnb.CreateLoadBalancer(context.TODO(), n.getLoadBalancerName(newForward.ListenAddress), n.getRouterName(), n.getIntSwitchName(), vips...)
		if err != nil {
			return fmt.Errorf("Failed applying OVN load balancer: %w", err)
		}

		reverter.Add(func() {
			// Apply old settings to OVN on failure.
			portMaps, err := n.forwardValidate(net.ParseIP(curForward.ListenAddress), &curForward.NetworkForwardPut)
			if err == nil {
				vips := n.forwardFlattenVIPs(net.ParseIP(curForward.ListenAddress), net.ParseIP(curForward.Config["target_address"]), portMaps)
				_ = n.ovnnb.CreateLoadBalancer(context.TODO(), n.getLoadBalancerName(curForward.ListenAddress), n.getRouterName(), n.getIntSwitchName(), vips...)
				_ = n.forwardBGPSetupPrefixes()
			}
		})

		err = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
			fwd := dbCluster.NetworkForward{
				NetworkID:     n.ID(),
				ListenAddress: listenAddress,
				Description:   newForward.Description,
				Ports:         newForward.Ports,
			}

			err = dbCluster.UpdateNetworkForward(ctx, tx.Tx(), n.ID(), listenAddress, fwd)
			if err != nil {
				return err
			}

			err = dbCluster.UpdateNetworkForwardConfig(ctx, tx.Tx(), curForwardID, newForward.Config)
			if err != nil {
				return err
			}

			return nil
		})
		if err != nil {
			return err
		}

		reverter.Add(func() {
			_ = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
				fwd := dbCluster.NetworkForward{
					NetworkID:     n.ID(),
					ListenAddress: listenAddress,
					Description:   curForward.Description,
					Ports:         curForward.Ports,
				}

				err = dbCluster.UpdateNetworkForward(ctx, tx.Tx(), n.ID(), listenAddress, fwd)
				if err != nil {
					return err
				}

				err = dbCluster.UpdateNetworkForwardConfig(ctx, tx.Tx(), curForwardID, curForward.Config)
				if err != nil {
					return err
				}

				return nil
			})
		})

		// Notify all other members to refresh their BGP prefixes.
		notifier, err := cluster.NewNotifier(n.state, n.state.Endpoints.NetworkCert(), n.state.ServerCert(), cluster.NotifyAll)
		if err != nil {
			return err
		}

		err = notifier(func(client incus.InstanceServer) error {
			return client.UseProject(n.project).UpdateNetworkForward(n.name, curForward.ListenAddress, req, "")
		})
		if err != nil {
			return err
		}
	}

	// Refresh exported BGP prefixes on local member.
	err := n.forwardBGPSetupPrefixes()
	if err != nil {
		return fmt.Errorf("Failed applying BGP prefixes for address forwards: %w", err)
	}

	reverter.Success()
	return nil
}

// ForwardDelete deletes a network forward.
func (n *ovn) ForwardDelete(listenAddress string, clientType request.ClientType) error {
	if clientType == request.ClientTypeNormal {
		var forwardID int64
		var forward *api.NetworkForward

		err := n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
			var err error

			// No memberSpecific filtering needed because OVN doesn't support per-member-forwards
			dbRecord, err := dbCluster.GetNetworkForward(ctx, tx.Tx(), n.ID(), listenAddress)
			if err != nil {
				return err
			}

			forwardID = dbRecord.ID
			forward, err = dbRecord.ToAPI(ctx, tx.Tx())
			if err != nil {
				return err
			}

			return nil
		})
		if err != nil {
			return err
		}

		// Delete the network forward itself.
		err = n.ovnnb.DeleteLoadBalancer(context.TODO(), n.getLoadBalancerName(forward.ListenAddress))
		if err != nil {
			return fmt.Errorf("Failed deleting OVN load balancer: %w", err)
		}

		// Delete static route to network forward if present.
		vip, err := ParseIPToNet(forward.ListenAddress)
		if err != nil {
			return err
		}

		_ = n.ovnnb.DeleteLogicalRouterRoute(context.TODO(), n.getRouterName(), *vip)

		// Delete the database records.
		err = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
			return dbCluster.DeleteNetworkForward(ctx, tx.Tx(), n.ID(), forwardID)
		})
		if err != nil {
			return err
		}

		// Notify all other members to refresh their BGP prefixes.
		notifier, err := cluster.NewNotifier(n.state, n.state.Endpoints.NetworkCert(), n.state.ServerCert(), cluster.NotifyAll)
		if err != nil {
			return err
		}

		err = notifier(func(client incus.InstanceServer) error {
			return client.UseProject(n.project).DeleteNetworkForward(n.name, forward.ListenAddress)
		})
		if err != nil {
			return err
		}
	}

	// Refresh exported BGP prefixes on local member.
	err := n.forwardBGPSetupPrefixes()
	if err != nil {
		return fmt.Errorf("Failed applying BGP prefixes for address forwards: %w", err)
	}

	return nil
}

// loadBalancerFlattenVIPs flattens port maps into format compatible with OVN load balancers.
func (n *ovn) loadBalancerFlattenVIPs(listenAddress net.IP, portMaps []*loadBalancerPortMap) []networkOVN.OVNLoadBalancerVIP {
	var vips []networkOVN.OVNLoadBalancerVIP

	for _, portMap := range portMaps {
		for i, lp := range portMap.listenPorts {
			vip := networkOVN.OVNLoadBalancerVIP{
				ListenAddress: listenAddress,
				Protocol:      portMap.protocol,
				ListenPort:    lp,
			}

			for _, target := range portMap.targets {
				targetPort := lp // Default to using same port as listen port for target port.
				targetPortsLen := len(target.ports)

				if targetPortsLen == 1 {
					// If a single target port is specified, forward all listen ports to it.
					targetPort = target.ports[0]
				} else if targetPortsLen > 1 {
					// If more than 1 target port specified, use listen port index to get the
					// target port to use.
					targetPort = target.ports[i]
				}

				vip.Targets = append(vip.Targets, networkOVN.OVNLoadBalancerTarget{
					Address: target.address,
					Port:    targetPort,
				})
			}

			vips = append(vips, vip)
		}
	}

	return vips
}

// LoadBalancerCreate creates a network load balancer.
func (n *ovn) LoadBalancerCreate(loadBalancer api.NetworkLoadBalancersPost, clientType request.ClientType) error {
	if n.config["network"] == "none" {
		return errors.New("Isolated OVN network cannot use network load balancers")
	}

	reverter := revert.New()
	defer reverter.Fail()

	if clientType == request.ClientTypeNormal {
		err := n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
			// Check if there is an existing load balancer using the same listen address.
			_, err := dbCluster.GetNetworkLoadBalancer(ctx, tx.Tx(), n.ID(), loadBalancer.ListenAddress)
			if err != nil {
				return err
			}

			return nil
		})
		if err == nil {
			return api.StatusErrorf(http.StatusConflict, "A load balancer for that listen address already exists")
		}

		// Convert listen address to subnet so we can check its valid and can be used.
		listenAddressNet, err := ParseIPToNet(loadBalancer.ListenAddress)
		if err != nil {
			return fmt.Errorf("Failed parsing %q: %w", loadBalancer.ListenAddress, err)
		}

		portMaps, err := n.loadBalancerValidate(listenAddressNet.IP, &loadBalancer.NetworkLoadBalancerPut)
		if err != nil {
			return err
		}

		// Load the project to get uplink network restrictions.
		var p *api.Project
		var uplink *api.Network

		err = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
			project, err := dbCluster.GetProject(ctx, tx.Tx(), n.project)
			if err != nil {
				return fmt.Errorf("Failed to load network restrictions from project %q: %w", n.project, err)
			}

			p, err = project.ToAPI(ctx, tx.Tx())
			if err != nil {
				return fmt.Errorf("Failed to load network restrictions from project %q: %w", n.project, err)
			}

			// Get uplink routes.
			_, uplink, _, err = tx.GetNetworkInAnyState(ctx, api.ProjectDefaultName, n.config["network"])
			if err != nil {
				return fmt.Errorf("Failed to load uplink network %q: %w", n.config["network"], err)
			}

			return nil
		})
		if err != nil {
			return err
		}

		// Get project restricted routes.
		projectRestrictedSubnets, err := n.projectRestrictedSubnets(p, n.config["network"])
		if err != nil {
			return err
		}

		externalSubnetsInUse, err := n.getExternalSubnetInUse(n.config["network"])
		if err != nil {
			return err
		}

		// Check the listen address subnet is allowed within both the uplink's external routes and any
		// project restricted subnets.
		err = n.validateExternalSubnet(uplink, projectRestrictedSubnets, listenAddressNet)
		if err != nil {
			return err
		}

		// Check the listen address subnet doesn't fall within any existing OVN network external subnets.
		for _, externalSubnetUser := range externalSubnetsInUse {
			// Check if usage is from our own network.
			if externalSubnetUser.networkProject == n.project && externalSubnetUser.networkName == n.name {
				// Skip checking conflict with our own network's subnet or SNAT address.
				// But do not allow other conflict with other usage types within our own network.
				if externalSubnetUser.usageType == subnetUsageNetwork || externalSubnetUser.usageType == subnetUsageNetworkSNAT {
					continue
				}
			}

			if SubnetContains(&externalSubnetUser.subnet, listenAddressNet) || SubnetContains(listenAddressNet, &externalSubnetUser.subnet) {
				// This error is purposefully vague so that it doesn't reveal any names of
				// resources potentially outside of the network's project.
				return fmt.Errorf("Load balancer listen address %q overlaps with another network or NIC", listenAddressNet.String())
			}
		}

		var loadBalancerID int64

		err = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
			// Create load balancer DB record.
			lb := dbCluster.NetworkLoadBalancer{
				NetworkID:     n.ID(),
				ListenAddress: loadBalancer.ListenAddress,
				Description:   loadBalancer.Description,
				Backends:      loadBalancer.Backends,
				Ports:         loadBalancer.Ports,
			}

			loadBalancerID, err = dbCluster.CreateNetworkLoadBalancer(ctx, tx.Tx(), lb)
			if err != nil {
				return err
			}

			// Save the load balancer configuration.
			err = dbCluster.CreateNetworkLoadBalancerConfig(ctx, tx.Tx(), loadBalancerID, loadBalancer.Config)
			if err != nil {
				return err
			}

			return nil
		})
		if err != nil {
			return err
		}

		reverter.Add(func() {
			_ = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
				return dbCluster.DeleteNetworkLoadBalancer(ctx, tx.Tx(), n.ID(), loadBalancerID)
			})

			_ = n.ovnnb.DeleteLoadBalancer(context.TODO(), n.getLoadBalancerName(loadBalancer.ListenAddress))
			_ = n.loadBalancerBGPSetupPrefixes()
		})

		vips := n.loadBalancerFlattenVIPs(net.ParseIP(loadBalancer.ListenAddress), portMaps)

		// Look at health checking configuration.
		healthCheck, err := n.getHealthCheck(loadBalancer.NetworkLoadBalancerPut)
		if err != nil {
			return err
		}

		if healthCheck != nil {
			for i := range vips {
				vips[i].HealthCheck = healthCheck
			}
		}

		err = n.ovnnb.CreateLoadBalancer(context.TODO(), n.getLoadBalancerName(loadBalancer.ListenAddress), n.getRouterName(), n.getIntSwitchName(), vips...)
		if err != nil {
			return fmt.Errorf("Failed applying OVN load balancer: %w", err)
		}

		// Add internal static route to the load-balancer (helps with OVN IC).
		var nexthop net.IP
		if listenAddressNet.IP.To4() == nil {
			routerV6, _, err := n.parseRouterIntPortIPv6Net()
			if err == nil {
				nexthop = routerV6
			}
		} else {
			routerV4, _, err := n.parseRouterIntPortIPv4Net()
			if err == nil {
				nexthop = routerV4
			}
		}

		if nexthop != nil {
			err = n.ovnnb.CreateLogicalRouterRoute(context.TODO(), n.getRouterName(), true, networkOVN.OVNRouterRoute{NextHop: nexthop, Prefix: *listenAddressNet})
			if err != nil {
				return err
			}

			reverter.Add(func() {
				_ = n.ovnnb.DeleteLogicalRouterRoute(context.TODO(), n.getRouterName(), *listenAddressNet)
			})
		}

		// Notify all other members to refresh their BGP prefixes.
		notifier, err := cluster.NewNotifier(n.state, n.state.Endpoints.NetworkCert(), n.state.ServerCert(), cluster.NotifyAll)
		if err != nil {
			return err
		}

		err = notifier(func(client incus.InstanceServer) error {
			return client.UseProject(n.project).CreateNetworkLoadBalancer(n.name, loadBalancer)
		})
		if err != nil {
			return err
		}
	}

	// Refresh exported BGP prefixes on local member.
	err := n.loadBalancerBGPSetupPrefixes()
	if err != nil {
		return fmt.Errorf("Failed applying BGP prefixes for load balancers: %w", err)
	}

	reverter.Success()
	return nil
}

// LoadBalancerUpdate updates a network load balancer.
func (n *ovn) LoadBalancerUpdate(listenAddress string, req api.NetworkLoadBalancerPut, clientType request.ClientType) error {
	reverter := revert.New()
	defer reverter.Fail()

	if clientType == request.ClientTypeNormal {
		var curLoadBalancer *api.NetworkLoadBalancer
		var curLoadBalancerID int64

		err := n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
			networkID := n.ID()

			// Get the load balancer.
			dbLoadBalancers, err := dbCluster.GetNetworkLoadBalancers(ctx, tx.Tx(), dbCluster.NetworkLoadBalancerFilter{
				NetworkID:     &networkID,
				ListenAddress: &listenAddress,
			})
			if err != nil {
				return err
			}

			if len(dbLoadBalancers) != 1 {
				return api.StatusErrorf(http.StatusNotFound, "Network load balancer not found")
			}

			// Get the API struct.
			curLoadBalancer, err = dbLoadBalancers[0].ToAPI(ctx, tx.Tx())
			if err != nil {
				return err
			}

			curLoadBalancerID = dbLoadBalancers[0].ID

			return nil
		})
		if err != nil {
			return err
		}

		portMaps, err := n.loadBalancerValidate(net.ParseIP(curLoadBalancer.ListenAddress), &req)
		if err != nil {
			return err
		}

		curEtagHash, err := localUtil.EtagHash(curLoadBalancer.Etag())
		if err != nil {
			return err
		}

		newLoadBalancer := api.NetworkLoadBalancer{
			ListenAddress:          curLoadBalancer.ListenAddress,
			NetworkLoadBalancerPut: req,
		}

		newLoadBalancerEtagHash, err := localUtil.EtagHash(newLoadBalancer.Etag())
		if err != nil {
			return err
		}

		if curEtagHash == newLoadBalancerEtagHash {
			return nil // Nothing has changed.
		}

		vips := n.loadBalancerFlattenVIPs(net.ParseIP(newLoadBalancer.ListenAddress), portMaps)

		// Look at health checking configuration.
		healthCheck, err := n.getHealthCheck(newLoadBalancer.NetworkLoadBalancerPut)
		if err != nil {
			return err
		}

		if healthCheck != nil {
			for i := range vips {
				vips[i].HealthCheck = healthCheck
			}
		}

		err = n.ovnnb.CreateLoadBalancer(context.TODO(), n.getLoadBalancerName(newLoadBalancer.ListenAddress), n.getRouterName(), n.getIntSwitchName(), vips...)
		if err != nil {
			return fmt.Errorf("Failed applying OVN load balancer: %w", err)
		}

		reverter.Add(func() {
			// Apply old settings to OVN on failure.
			portMaps, err := n.loadBalancerValidate(net.ParseIP(curLoadBalancer.ListenAddress), &curLoadBalancer.NetworkLoadBalancerPut)
			if err == nil {
				vips := n.loadBalancerFlattenVIPs(net.ParseIP(curLoadBalancer.ListenAddress), portMaps)
				_ = n.ovnnb.CreateLoadBalancer(context.TODO(), n.getLoadBalancerName(curLoadBalancer.ListenAddress), n.getRouterName(), n.getIntSwitchName(), vips...)
				_ = n.forwardBGPSetupPrefixes()
			}
		})

		err = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
			lb := dbCluster.NetworkLoadBalancer{
				NetworkID:     n.ID(),
				ListenAddress: listenAddress,
				Description:   newLoadBalancer.Description,
				Backends:      newLoadBalancer.Backends,
				Ports:         newLoadBalancer.Ports,
			}

			err = dbCluster.UpdateNetworkLoadBalancer(ctx, tx.Tx(), n.ID(), listenAddress, lb)
			if err != nil {
				return err
			}

			err = dbCluster.UpdateNetworkLoadBalancerConfig(ctx, tx.Tx(), curLoadBalancerID, newLoadBalancer.Config)
			if err != nil {
				return err
			}

			return nil
		})
		if err != nil {
			return err
		}

		reverter.Add(func() {
			_ = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
				lb := dbCluster.NetworkLoadBalancer{
					NetworkID:     n.ID(),
					ListenAddress: listenAddress,
					Description:   curLoadBalancer.Description,
					Backends:      curLoadBalancer.Backends,
					Ports:         curLoadBalancer.Ports,
				}

				err = dbCluster.UpdateNetworkLoadBalancer(ctx, tx.Tx(), n.ID(), listenAddress, lb)
				if err != nil {
					return err
				}

				err = dbCluster.UpdateNetworkLoadBalancerConfig(ctx, tx.Tx(), curLoadBalancerID, curLoadBalancer.Config)
				if err != nil {
					return err
				}

				return nil
			})
		})

		// Notify all other members to refresh their BGP prefixes.
		notifier, err := cluster.NewNotifier(n.state, n.state.Endpoints.NetworkCert(), n.state.ServerCert(), cluster.NotifyAll)
		if err != nil {
			return err
		}

		err = notifier(func(client incus.InstanceServer) error {
			return client.UseProject(n.project).UpdateNetworkLoadBalancer(n.name, curLoadBalancer.ListenAddress, req, "")
		})
		if err != nil {
			return err
		}
	}

	// Refresh exported BGP prefixes on local member.
	err := n.loadBalancerBGPSetupPrefixes()
	if err != nil {
		return fmt.Errorf("Failed applying BGP prefixes for load balancers: %w", err)
	}

	reverter.Success()
	return nil
}

// LoadBalancerState returns the current state of the load balancer.
func (n *ovn) LoadBalancerState(lb api.NetworkLoadBalancer) (*api.NetworkLoadBalancerState, error) {
	lbState := &api.NetworkLoadBalancerState{}

	if util.IsTrue(lb.Config["healthcheck"]) {
		lbState.BackendHealth = map[string]api.NetworkLoadBalancerStateBackendHealth{}

		for _, backend := range lb.Backends {
			backendHealth := api.NetworkLoadBalancerStateBackendHealth{}
			backendHealth.Address = backend.TargetAddress
			backendHealth.Ports = []api.NetworkLoadBalancerStateBackendHealthPort{}

			for _, lbPort := range lb.Ports {
				if !slices.Contains(lbPort.TargetBackend, backend.Name) {
					continue
				}

				// Check valid listen port(s) supplied.
				listenPortRanges := util.SplitNTrimSpace(lbPort.ListenPort, ",", -1, true)
				if len(listenPortRanges) <= 0 {
					return nil, fmt.Errorf("Missing listen port in port specification %q", lbPort.ListenPort)
				}

				for _, pr := range listenPortRanges {
					portFirst, portRange, err := ParsePortRange(pr)
					if err != nil {
						return nil, fmt.Errorf("Invalid listen port in port specification %q: %w", lbPort.ListenPort, err)
					}

					for i := range portRange {
						port := portFirst + i

						status, err := n.ovnsb.GetServiceHealth(context.TODO(), backend.TargetAddress, lbPort.Protocol, int(port))
						if err != nil {
							return nil, fmt.Errorf("Failed retrieving OVN load-balancer health: %w", err)
						}

						portHealth := api.NetworkLoadBalancerStateBackendHealthPort{
							Protocol: lbPort.Protocol,
							Port:     int(port),
							Status:   status,
						}

						backendHealth.Ports = append(backendHealth.Ports, portHealth)
					}
				}
			}

			lbState.BackendHealth[backend.Name] = backendHealth
		}
	}

	return lbState, nil
}

// LoadBalancerDelete deletes a network load balancer.
func (n *ovn) LoadBalancerDelete(listenAddress string, clientType request.ClientType) error {
	if clientType == request.ClientTypeNormal {
		var lb *dbCluster.NetworkLoadBalancer

		err := n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
			networkID := n.ID()

			dbLoadBalancers, err := dbCluster.GetNetworkLoadBalancers(ctx, tx.Tx(), dbCluster.NetworkLoadBalancerFilter{
				NetworkID:     &networkID,
				ListenAddress: &listenAddress,
			})
			if err != nil {
				return err
			}

			if len(dbLoadBalancers) != 1 {
				return api.StatusErrorf(http.StatusNotFound, "Network load balancer not found")
			}

			lb = &dbLoadBalancers[0]

			return nil
		})
		if err != nil {
			return err
		}

		// Delete the load balancer itself.
		err = n.ovnnb.DeleteLoadBalancer(context.TODO(), n.getLoadBalancerName(lb.ListenAddress))
		if err != nil {
			return fmt.Errorf("Failed deleting OVN load balancer: %w", err)
		}

		// Delete static route to load-balancer if present.
		vip, err := ParseIPToNet(lb.ListenAddress)
		if err != nil {
			return err
		}

		_ = n.ovnnb.DeleteLogicalRouterRoute(context.TODO(), n.getRouterName(), *vip)

		// Delete the database records.
		err = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
			return dbCluster.DeleteNetworkLoadBalancer(ctx, tx.Tx(), n.ID(), lb.ID)
		})
		if err != nil {
			return err
		}

		// Notify all other members to refresh their BGP prefixes.
		notifier, err := cluster.NewNotifier(n.state, n.state.Endpoints.NetworkCert(), n.state.ServerCert(), cluster.NotifyAll)
		if err != nil {
			return err
		}

		err = notifier(func(client incus.InstanceServer) error {
			return client.UseProject(n.project).DeleteNetworkLoadBalancer(n.name, lb.ListenAddress)
		})
		if err != nil {
			return err
		}
	}

	// Refresh exported BGP prefixes on local member.
	err := n.loadBalancerBGPSetupPrefixes()
	if err != nil {
		return fmt.Errorf("Failed applying BGP prefixes for address forwards: %w", err)
	}

	return nil
}

func (n *ovn) getHealthCheck(loadBalancer api.NetworkLoadBalancerPut) (*networkOVN.OVNLoadBalancerHealthCheck, error) {
	// Check if load-balancer is enabled.
	if !util.IsTrue(loadBalancer.Config["healthcheck"]) {
		return nil, nil
	}

	// Get IPv4 checker.
	var checkerIPV4 net.IP
	_, ipv4Net, err := n.parseRouterIntPortIPv4Net()
	if err == nil && ipv4Net != nil {
		checkerIPV4 = dhcpalloc.GetIP(ipv4Net, -2)
	}

	// Get IPv6 checker.
	var checkerIPV6 net.IP
	_, ipv6Net, err := n.parseRouterIntPortIPv6Net()
	if err == nil && ipv6Net != nil {
		checkerIPV6 = dhcpalloc.GetIP(ipv6Net, -2)
	}

	// Parse the healthcheck options.
	hcInterval, err := strconv.Atoi(loadBalancer.Config["healthcheck.interval"])
	if err != nil && loadBalancer.Config["healthcheck.interval"] != "" {
		return nil, err
	}

	hcTimeout, err := strconv.Atoi(loadBalancer.Config["healthcheck.timeout"])
	if err != nil && loadBalancer.Config["healthcheck.timeout"] != "" {
		return nil, err
	}

	hcFailureCount, err := strconv.Atoi(loadBalancer.Config["healthcheck.failure_count"])
	if err != nil && loadBalancer.Config["healthcheck.failure_count"] != "" {
		return nil, err
	}

	hcSuccessCount, err := strconv.Atoi(loadBalancer.Config["healthcheck.success_count"])
	if err != nil && loadBalancer.Config["healthcheck.success_count"] != "" {
		return nil, err
	}

	// Prepare the load-balancer health check.
	healthCheck := &networkOVN.OVNLoadBalancerHealthCheck{
		CheckerIPV4: checkerIPV4,
		CheckerIPV6: checkerIPV6,

		Interval:     hcInterval,
		Timeout:      hcTimeout,
		FailureCount: hcFailureCount,
		SuccessCount: hcSuccessCount,
	}

	return healthCheck, nil
}

// Leases returns a list of leases for the OVN network. Those are directly extracted from the OVN database.
func (n *ovn) Leases(projectName string, clientType request.ClientType) ([]api.NetworkLease, error) {
	var err error
	leases := []api.NetworkLease{}

	// If requested project matches network's project then include gateway IPs.
	if projectName == n.project {
		// Add our own gateway IPs.
		for _, addr := range []string{n.config["ipv4.address"], n.config["ipv6.address"]} {
			ip, _, _ := net.ParseCIDR(addr)
			if ip != nil {
				leases = append(leases, api.NetworkLease{
					Hostname: fmt.Sprintf("%s.gw", n.Name()),
					Address:  ip.String(),
					Type:     "gateway",
				})
			}
		}
	}

	// Get all the instances in the requested project that are connected to this network.
	filter := dbCluster.InstanceFilter{Project: &projectName}
	err = UsedByInstanceDevices(n.state, n.Project(), n.Name(), n.Type(), func(inst db.InstanceArgs, nicName string, nicConfig map[string]string) error {
		// Get the instance UUID needed for OVN port name generation.
		instanceUUID := inst.Config["volatile.uuid"]
		if instanceUUID == "" {
			return nil
		}

		devIPs, err := n.InstanceDevicePortIPs(instanceUUID, nicName)
		if err != nil {
			return nil // There is likely no active port and so no leases.
		}

		// Fill in the hwaddr from volatile.
		if nicConfig["hwaddr"] == "" {
			nicConfig["hwaddr"] = inst.Config[fmt.Sprintf("volatile.%s.hwaddr", nicName)]
		}

		// Parse the MAC.
		hwAddr, _ := net.ParseMAC(nicConfig["hwaddr"])

		// Add the leases.
		for _, ip := range devIPs {
			leaseType := "dynamic"
			if nicConfig["ipv4.address"] == ip.String() || nicConfig["ipv6.address"] == ip.String() {
				leaseType = "static"
			}

			leases = append(leases, api.NetworkLease{
				Hostname: inst.Name,
				Address:  ip.String(),
				Hwaddr:   hwAddr.String(),
				Type:     leaseType,
				Location: inst.Node,
			})
		}

		return nil
	}, filter)
	if err != nil {
		return nil, err
	}

	return leases, nil
}

// localPeerCreate creates a network peering with another local network.
func (n *ovn) localPeerCreate(peer api.NetworkPeersPost) error {
	ctx := context.TODO()

	reverter := revert.New()
	defer reverter.Fail()

	// Get the peer DB record.
	var peerInfo *api.NetworkPeer
	err := n.state.DB.Cluster.Transaction(ctx, func(ctx context.Context, tx *db.ClusterTx) error {
		var err error

		// Load peering to get mutual peering info.
		dbPeer, err := dbCluster.GetNetworkPeer(ctx, tx.Tx(), n.id, peer.Name)
		if err != nil {
			return fmt.Errorf("Failed getting network peer DB object: %w", err)
		}

		var apiErr error
		peerInfo, apiErr = dbPeer.ToAPI(ctx, tx.Tx())
		if apiErr != nil {
			return fmt.Errorf("Failed converting network peer DB object to API object: %w", apiErr)
		}

		return nil
	})
	if err != nil {
		return err
	}

	// Validate the peer.
	if peerInfo.Status != api.NetworkStatusCreated {
		return fmt.Errorf("Only peerings in %q state can be setup", api.NetworkStatusCreated)
	}

	// Apply router security policies.
	// Should have been done during network setup, but ensure its done here anyway.
	err = n.logicalRouterPolicySetup(n.ovnnb)
	if err != nil {
		return fmt.Errorf("Failed applying local router security policy: %w", err)
	}

	activeLocalNICPorts, err := n.ovnnb.GetLogicalSwitchPorts(context.TODO(), n.getIntSwitchName())
	if err != nil {
		return fmt.Errorf("Failed getting active NIC ports: %w", err)
	}

	var localNICRoutes []net.IPNet

	// Get routes on instance NICs connected to local network to be added as routes to target network.
	err = UsedByInstanceDevices(n.state, n.Project(), n.Name(), n.Type(), func(inst db.InstanceArgs, nicName string, nicConfig map[string]string) error {
		instancePortName := n.getInstanceDevicePortName(inst.Config["volatile.uuid"], nicName)
		_, found := activeLocalNICPorts[instancePortName]
		if !found {
			return nil // Don't add config for instance NICs that aren't started.
		}

		localNICRoutes = append(localNICRoutes, n.instanceNICGetRoutes(nicConfig)...)

		return nil
	})
	if err != nil {
		return fmt.Errorf("Failed getting instance NIC routes on local network: %w", err)
	}

	targetNet, err := LoadByName(n.state, peer.TargetProject, peer.TargetNetwork)
	if err != nil {
		return fmt.Errorf("Failed loading target network: %w", err)
	}

	targetOVNNet, ok := targetNet.(*ovn)
	if !ok {
		return errors.New("Target network is not ovn interface type")
	}

	opts, err := n.peerGetLocalOpts(localNICRoutes)
	if err != nil {
		return err
	}

	// Ensure local subnets and all active NIC routes are present in internal switch's address set.
	err = n.ovnnb.UpdateAddressSetAdd(context.TODO(), acl.OVNIntSwitchPortGroupAddressSetPrefix(n.ID()), opts.TargetRouterRoutes...)
	if err != nil {
		return fmt.Errorf("Failed adding active NIC routes to switch address set: %w", err)
	}

	err = n.peerSetup(n.ovnnb, targetOVNNet, *opts)
	if err != nil {
		return err
	}

	reverter.Success()
	return nil
}

// remotePeerCreate creates a network peering with an OVN-IC.
func (n *ovn) remotePeerCreate(peer api.NetworkPeersPost) error {
	ctx := context.TODO()

	reverter := revert.New()
	defer reverter.Fail()

	// Load the project.
	var p *api.Project
	err := n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		project, err := dbCluster.GetProject(ctx, tx.Tx(), n.project)
		if err != nil {
			return err
		}

		p, err = project.ToAPI(ctx, tx.Tx())

		return err
	})
	if err != nil {
		return fmt.Errorf("Failed to load network restrictions from project %q: %w", n.project, err)
	}

	// Validate restrictions.
	if !project.NetworkIntegrationAllowed(p.Config, peer.TargetIntegration) {
		return api.StatusErrorf(http.StatusForbidden, "Project isn't allowed to use this network integration")
	}

	// Load the integration.
	var integration *api.NetworkIntegration
	err = n.state.DB.Cluster.Transaction(ctx, func(ctx context.Context, tx *db.ClusterTx) error {
		entry, err := dbCluster.GetNetworkIntegration(ctx, tx.Tx(), peer.TargetIntegration)
		if err != nil {
			return err
		}

		integration, err = entry.ToAPI(ctx, tx.Tx())

		return err
	})
	if err != nil {
		return fmt.Errorf("Failed to load network integration %q: %w", peer.TargetIntegration, err)
	}

	// Get ICNB.
	icnb, err := networkOVN.NewICNB(integration.Config["ovn.northbound_connection"], integration.Config["ovn.ca_cert"], integration.Config["ovn.client_cert"], integration.Config["ovn.client_key"])
	if err != nil {
		return err
	}

	// Get ICSB.
	icsb, err := networkOVN.NewICSB(integration.Config["ovn.southbound_connection"], integration.Config["ovn.ca_cert"], integration.Config["ovn.client_cert"], integration.Config["ovn.client_key"])
	if err != nil {
		return err
	}

	// Get the OVN AZ name.
	azName, err := n.ovnnb.GetName(ctx)
	if err != nil {
		return err
	}

	// Get the list of interconnect gateways.
	gateways, err := icsb.GetGateways(ctx, azName)
	if err != nil {
		return err
	}

	if len(gateways) == 0 {
		return errors.New("No chassis gateways available for interconnect")
	}

	// Determine the transit switch name.
	pattern := integration.Config["ovn.transit.pattern"]
	if pattern == "" {
		pattern = "ts-incus-{{ integrationName }}-{{ projectName }}-{{ networkName }}"
	}

	tsNameRendered, err := internalUtil.RenderTemplate(pattern, pongo2.Context{
		"projectName":     n.project,
		"networkName":     n.name,
		"integrationName": integration.Name,
		"peerName":        peer.Name,
	})
	if err != nil {
		return err
	}

	tsName := networkOVN.OVNSwitch(tsNameRendered)

	// Determine the chassis group name.
	cgName := networkOVN.OVNChassisGroup(tsName)

	// Create the chassis group.
	err = n.ovnnb.CreateChassisGroup(ctx, cgName, false)
	if err != nil {
		return err
	}

	reverter.Add(func() { _ = n.ovnnb.DeleteChassisGroup(ctx, cgName) })

	// Seed the stable random number generator with the transit switch name.
	// This should cause a reasonable spread of networks on the available IC gateway chassis.
	r, err := localUtil.GetStableRandomGenerator(tsNameRendered)
	if err != nil {
		return fmt.Errorf("Failed generating stable random chassis group priority: %w", err)
	}

	// Assign some priorities.
	for _, gateway := range gateways {
		err = n.ovnnb.SetChassisGroupPriority(ctx, cgName, gateway, r.Intn(ovnChassisPriorityMax+1))
		if err != nil {
			return err
		}
	}

	// Create the transit switch if it doesn't exist already.
	err = icnb.CreateTransitSwitch(ctx, string(tsName), true)
	if err != nil {
		return err
	}

	// Check that the switch appeared on the local OVN.
	found := false
	for range 10 {
		// Try to get the switch.
		logicalSwitch, err := n.ovnnb.GetLogicalSwitch(ctx, tsName)
		if err != nil && !errors.Is(err, networkOVN.ErrNotFound) {
			return err
		}

		if logicalSwitch != nil {
			found = true
			break
		}

		time.Sleep(time.Second)
	}

	if !found {
		return errors.New("New transit switch didn't appear within 10s")
	}

	// Get router MAC address.
	routerMAC, err := n.getRouterMAC()
	if err != nil {
		return err
	}

	// Get bridge MTU.
	bridgeMTU := int(n.getBridgeMTU())
	if bridgeMTU == 0 {
		bridgeMTU = 1500
	}

	// Get peering addresses.
	ipv4Net, ipv6Net, err := icnb.CreateTransitSwitchAllocation(ctx, string(tsName), azName)
	if err != nil {
		return err
	}

	// Determine logical router port name.
	lrpName := networkOVN.OVNRouterPort(tsName)

	// Create the logical router port.
	err = n.ovnnb.CreateLogicalRouterPort(ctx, n.getRouterName(), lrpName, routerMAC, uint32(bridgeMTU), []*net.IPNet{ipv4Net, ipv6Net}, cgName, false)
	if err != nil {
		return err
	}

	reverter.Add(func() { _ = n.ovnnb.DeleteLogicalRouterPort(ctx, n.getRouterName(), lrpName) })

	// Create the logical switch port.
	lspOpts := &networkOVN.OVNSwitchPortOpts{RouterPort: lrpName}
	err = n.ovnnb.CreateLogicalSwitchPort(ctx, tsName, networkOVN.OVNSwitchPort(fmt.Sprintf("%s-%s", tsName, azName)), lspOpts, false)
	if err != nil {
		return err
	}

	reverter.Add(func() {
		_ = n.ovnnb.DeleteLogicalSwitchPort(ctx, tsName, networkOVN.OVNSwitchPort(fmt.Sprintf("%s-%s", tsName, azName)))
	})

	reverter.Success()
	return nil
}

// PeerCreate creates a network peering.
func (n *ovn) PeerCreate(peer api.NetworkPeersPost) error {
	reverter := revert.New()
	defer reverter.Fail()

	// Default type is local.
	if peer.Type == "" {
		peer.Type = "local"
	}

	// Perform create-time validation.
	if peer.Type == "local" {
		// Default to network's project if target project not specified.
		if peer.TargetProject == "" {
			peer.TargetProject = n.Project()
		}

		// Target network name is required.
		if peer.TargetNetwork == "" {
			return api.StatusErrorf(http.StatusBadRequest, "Target network is required")
		}
	} else if peer.Type == "remote" {
		// Target integration name is required.
		if peer.TargetIntegration == "" {
			return api.StatusErrorf(http.StatusBadRequest, "Target integration is required")
		}
	}

	// Look for an existing entry.
	var peers map[int64]*api.NetworkPeer

	err := n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		var err error

		// Use generated function to get peers.
		netID := n.ID()
		filter := dbCluster.NetworkPeerFilter{NetworkID: &netID}
		dbPeers, err := dbCluster.GetNetworkPeers(ctx, tx.Tx(), filter)
		if err != nil {
			return fmt.Errorf("Failed loading network peer DB objects: %w", err)
		}

		// Convert DB objects to API objects and build the map.
		peers = make(map[int64]*api.NetworkPeer, len(dbPeers))
		for _, dbPeer := range dbPeers {
			peer, err := dbPeer.ToAPI(ctx, tx.Tx())
			if err != nil {
				return fmt.Errorf("Failed converting network peer DB object to API object: %w", err)
			}

			peers[dbPeer.ID] = peer
		}

		return nil
	})
	if err != nil {
		return err
	}

	for _, existingPeer := range peers {
		if peer.Name == existingPeer.Name {
			return api.StatusErrorf(http.StatusConflict, "A peer for that name already exists")
		}

		if peer.Type == "local" && peer.TargetProject == existingPeer.TargetProject && peer.TargetNetwork == existingPeer.TargetNetwork {
			return api.StatusErrorf(http.StatusConflict, "A peer for that target network already exists")
		}
	}

	// Perform general (create and update) validation.
	err = n.peerValidate(peer.Name, &peer.NetworkPeerPut)
	if err != nil {
		return err
	}

	var peerID int64
	var mutualExists bool

	err = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error { // Create peer DB record.
		record := dbCluster.NetworkPeer{
			NetworkID:   n.ID(),
			Name:        peer.Name,
			Description: peer.Description,
			Type:        dbCluster.NetworkPeerTypes[peer.Type],
		}

		switch peer.Type {
		case "remote":
			integrationID, err := dbCluster.GetNetworkIntegrationID(ctx, tx.Tx(), peer.TargetIntegration)
			if err != nil {
				return err
			}

			id := sql.NullInt64{}
			err = id.Scan(integrationID)
			if err != nil {
				return err
			}

			record.TargetNetworkIntegrationID = id

		case "local":
			// Check if target peer already exists.
			peers, err := dbCluster.GetNetworkPeers(ctx, tx.Tx(), dbCluster.NetworkPeerFilter{
				Type:                 &record.Type,
				TargetNetworkProject: &n.project,
				TargetNetworkName:    &n.name,
			})
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}

			if len(peers) == 1 {
				// Update the target peer.
				peer := peers[0]

				empty := sql.NullString{}
				peer.TargetNetworkProject = empty
				peer.TargetNetworkName = empty

				targetID := sql.NullInt64{}
				err = targetID.Scan(n.id)
				if err != nil {
					return err
				}

				peer.TargetNetworkID = targetID

				err = dbCluster.UpdateNetworkPeer(ctx, tx.Tx(), peer.NetworkID, peer.Name, peer)
				if err != nil {
					return err
				}

				// Set our target network ID to match.
				id := sql.NullInt64{}
				err = id.Scan(peer.NetworkID)
				if err != nil {
					return err
				}

				record.TargetNetworkID = id

				mutualExists = true
			} else if len(peers) == 0 {
				networkProjectName := sql.NullString{}
				err = networkProjectName.Scan(peer.TargetProject)
				if err != nil {
					return err
				}

				networkName := sql.NullString{}
				err = networkName.Scan(peer.TargetNetwork)
				if err != nil {
					return err
				}

				record.TargetNetworkProject = networkProjectName
				record.TargetNetworkName = networkName
			} else {
				return errors.New("More than one matching network peer was found")
			}
		}

		peerID, err = dbCluster.CreateNetworkPeer(ctx, tx.Tx(), record)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return err
	}

	reverter.Add(func() {
		_ = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
			err := dbCluster.DeleteNetworkPeer(ctx, tx.Tx(), n.ID(), peerID)
			if errors.Is(err, dbCluster.ErrNotFound) {
				return nil
			}

			return err
		})
	})

	// Apply the OVN configuration.
	if peer.Type == "local" && mutualExists {
		err := n.localPeerCreate(peer)
		if err != nil {
			return err
		}
	} else if peer.Type == "remote" {
		err := n.remotePeerCreate(peer)
		if err != nil {
			return err
		}
	}

	reverter.Success()
	return nil
}

// peerGetLocalOpts returns peering options prefilled with local router and local NIC routes config.
// It can then be modified with the target peering network options.
func (n *ovn) peerGetLocalOpts(localNICRoutes []net.IPNet) (*networkOVN.OVNRouterPeering, error) {
	localRouterPortMAC, err := n.getRouterMAC()
	if err != nil {
		return nil, fmt.Errorf("Failed getting router MAC address: %w", err)
	}

	opts := networkOVN.OVNRouterPeering{
		LocalRouter:        n.getRouterName(),
		LocalRouterPortMAC: localRouterPortMAC,
		TargetRouterRoutes: localNICRoutes, // Pre-fill with local NIC routes.
	}

	routerIntPortIPv4, routerIntPortIPv4Net, err := n.parseRouterIntPortIPv4Net()
	if err != nil {
		return nil, fmt.Errorf("Failed parsing local router's peering port IPv4 net: %w", err)
	}

	if routerIntPortIPv4 != nil && routerIntPortIPv4Net != nil {
		// Add a copy of the CIDR subnet to the target router's routes.
		opts.TargetRouterRoutes = append(opts.TargetRouterRoutes, *routerIntPortIPv4Net)

		// Convert the IPNet to include the specific router IP with a single host subnet.
		routerIntPortIPv4Net.IP = routerIntPortIPv4
		routerIntPortIPv4Net.Mask = net.CIDRMask(32, 32)
		opts.LocalRouterPortIPs = append(opts.LocalRouterPortIPs, *routerIntPortIPv4Net)
	}

	routerIntPortIPv6, routerIntPortIPv6Net, err := n.parseRouterIntPortIPv6Net()
	if err != nil {
		return nil, fmt.Errorf("Failed parsing local router's peering port IPv6 net: %w", err)
	}

	if routerIntPortIPv6 != nil && routerIntPortIPv6Net != nil {
		// Add a copy of the CIDR subnet to the target router's routers.
		opts.TargetRouterRoutes = append(opts.TargetRouterRoutes, *routerIntPortIPv6Net)

		// Convert the IPNet to include the specific router IP with a single host subnet.
		routerIntPortIPv6Net.IP = routerIntPortIPv6
		routerIntPortIPv6Net.Mask = net.CIDRMask(128, 128)
		opts.LocalRouterPortIPs = append(opts.LocalRouterPortIPs, *routerIntPortIPv6Net)
	}

	return &opts, err
}

// peerSetup applies the network peering configuration to both networks.
// Accepts an OVN client, a target OVN network, and a set of OVNRouterPeering options pre-filled with local config.
func (n *ovn) peerSetup(ovnnb *networkOVN.NB, targetOVNNet *ovn, opts networkOVN.OVNRouterPeering) error {
	targetRouterMAC, err := targetOVNNet.getRouterMAC()
	if err != nil {
		return fmt.Errorf("Failed getting target router MAC address: %w", err)
	}

	// Populate config based on target network.
	opts.LocalRouterPort = n.getLogicalRouterPeerPortName(targetOVNNet.ID())
	opts.TargetRouter = targetOVNNet.getRouterName()
	opts.TargetRouterPort = targetOVNNet.getLogicalRouterPeerPortName(n.ID())
	opts.TargetRouterPortMAC = targetRouterMAC

	routerIntPortIPv4, routerIntPortIPv4Net, err := targetOVNNet.parseRouterIntPortIPv4Net()
	if err != nil {
		return fmt.Errorf("Failed parsing target router's peering port IPv4 net: %w", err)
	}

	if routerIntPortIPv4 != nil && routerIntPortIPv4Net != nil {
		// Add a copy of the CIDR subnet to the local router's routers.
		opts.LocalRouterRoutes = append(opts.LocalRouterRoutes, *routerIntPortIPv4Net)

		// Convert the IPNet to include the specific router IP with a single host subnet.
		routerIntPortIPv4Net.IP = routerIntPortIPv4
		routerIntPortIPv4Net.Mask = net.CIDRMask(32, 32)
		opts.TargetRouterPortIPs = append(opts.TargetRouterPortIPs, *routerIntPortIPv4Net)
	}

	routerIntPortIPv6, routerIntPortIPv6Net, err := targetOVNNet.parseRouterIntPortIPv6Net()
	if err != nil {
		return fmt.Errorf("Failed parsing target router's peering port IPv6 net: %w", err)
	}

	if routerIntPortIPv6 != nil && routerIntPortIPv6Net != nil {
		// Add a copy of the CIDR subnet to the local router's routers.
		opts.LocalRouterRoutes = append(opts.LocalRouterRoutes, *routerIntPortIPv6Net)

		// Convert the IPNet to include the specific router IP with a single host subnet.
		routerIntPortIPv6Net.IP = routerIntPortIPv6
		routerIntPortIPv6Net.Mask = net.CIDRMask(128, 128)
		opts.TargetRouterPortIPs = append(opts.TargetRouterPortIPs, *routerIntPortIPv6Net)
	}

	// Get list of active switch ports (avoids repeated querying of OVN NB).
	activeTargetNICPorts, err := n.ovnnb.GetLogicalSwitchPorts(context.TODO(), targetOVNNet.getIntSwitchName())
	if err != nil {
		return fmt.Errorf("Failed getting active NIC ports: %w", err)
	}

	// Get routes on instance NICs connected to target network to be added as routes to local network.
	err = UsedByInstanceDevices(n.state, targetOVNNet.Project(), targetOVNNet.Name(), targetOVNNet.Type(), func(inst db.InstanceArgs, nicName string, nicConfig map[string]string) error {
		instancePortName := targetOVNNet.getInstanceDevicePortName(inst.Config["volatile.uuid"], nicName)
		_, found := activeTargetNICPorts[instancePortName]
		if !found {
			return nil // Don't add config for instance NICs that aren't started.
		}

		opts.LocalRouterRoutes = append(opts.LocalRouterRoutes, n.instanceNICGetRoutes(nicConfig)...)

		return nil
	})
	if err != nil {
		return fmt.Errorf("Failed getting instance NIC routes on target network: %w", err)
	}

	// Ensure routes are added to target switch address sets.
	err = n.ovnnb.UpdateAddressSetAdd(context.TODO(), acl.OVNIntSwitchPortGroupAddressSetPrefix(targetOVNNet.ID()), opts.LocalRouterRoutes...)
	if err != nil {
		return fmt.Errorf("Failed adding target switch subnet address set entries: %w", err)
	}

	err = targetOVNNet.logicalRouterPolicySetup(n.ovnnb)
	if err != nil {
		return fmt.Errorf("Failed applying target router security policy: %w", err)
	}

	err = n.ovnnb.CreateLogicalRouterPeering(context.TODO(), opts)
	if err != nil {
		return fmt.Errorf("Failed applying OVN network peering: %w", err)
	}

	return nil
}

// PeerUpdate updates a network peering.
func (n *ovn) PeerUpdate(peerName string, req api.NetworkPeerPut) error {
	reverter := revert.New()
	defer reverter.Fail()

	var curPeer *api.NetworkPeer
	var dbCurPeer *dbCluster.NetworkPeer

	err := n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		var err error

		dbCurPeer, err = dbCluster.GetNetworkPeer(ctx, tx.Tx(), n.id, peerName)
		if err != nil {
			return fmt.Errorf("Failed getting network peer DB object: %w", err)
		}

		curPeer, err = dbCurPeer.ToAPI(ctx, tx.Tx())
		if err != nil {
			return fmt.Errorf("Failed converting network peer DB object to API object: %w", err)
		}

		return nil
	})
	if err != nil {
		return err
	}

	err = n.peerValidate(peerName, &req)
	if err != nil {
		return err
	}

	curPeerEtagHash, err := localUtil.EtagHash(curPeer.Etag())
	if err != nil {
		return err
	}

	newPeer := api.NetworkPeer{
		Name:           curPeer.Name,
		NetworkPeerPut: req,
	}

	newPeerEtagHash, err := localUtil.EtagHash(newPeer.Etag())
	if err != nil {
		return err
	}

	if curPeerEtagHash == newPeerEtagHash {
		return nil // Nothing has changed.
	}

	err = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		// Update the description field from the input.
		dbCurPeer.Description = newPeer.Description

		// Update the main peer object.
		err = dbCluster.UpdateNetworkPeer(ctx, tx.Tx(), n.id, dbCurPeer.Name, *dbCurPeer)
		if err != nil {
			return fmt.Errorf("Failed to update network peer: %w", err)
		}

		// Update the peer configuration.
		err = dbCluster.UpdateNetworkPeerConfig(ctx, tx.Tx(), dbCurPeer.ID, newPeer.Config)
		if err != nil {
			return fmt.Errorf("Failed to update network peer config: %w", err)
		}

		return nil
	})
	if err != nil {
		return err
	}

	reverter.Success()
	return nil
}

// localPeerDelete deletes a network peering with another local network.
func (n *ovn) localPeerDelete(peer *api.NetworkPeer) error {
	targetNet, err := LoadByName(n.state, peer.TargetProject, peer.TargetNetwork)
	if err != nil {
		return fmt.Errorf("Failed loading target network: %w", err)
	}

	targetOVNNet, ok := targetNet.(*ovn)
	if !ok {
		return errors.New("Target network is not ovn interface type")
	}

	opts := networkOVN.OVNRouterPeering{
		LocalRouter:      n.getRouterName(),
		LocalRouterPort:  n.getLogicalRouterPeerPortName(targetOVNNet.ID()),
		TargetRouter:     targetOVNNet.getRouterName(),
		TargetRouterPort: targetOVNNet.getLogicalRouterPeerPortName(n.ID()),
	}

	err = n.ovnnb.DeleteLogicalRouterPeering(context.TODO(), opts)
	if err != nil {
		return fmt.Errorf("Failed deleting OVN network peering: %w", err)
	}

	err = n.logicalRouterPolicySetup(n.ovnnb, targetOVNNet.ID())
	if err != nil {
		return fmt.Errorf("Failed applying local router security policy: %w", err)
	}

	err = targetOVNNet.logicalRouterPolicySetup(n.ovnnb, n.ID())
	if err != nil {
		return fmt.Errorf("Failed applying target router security policy: %w", err)
	}

	return nil
}

// remotePeerDelete deletes a network peering with an OVN-IC.
func (n *ovn) remotePeerDelete(peer *api.NetworkPeer) error {
	ctx := context.TODO()

	// Load the integration.
	var integration *api.NetworkIntegration
	err := n.state.DB.Cluster.Transaction(ctx, func(ctx context.Context, tx *db.ClusterTx) error {
		entry, err := dbCluster.GetNetworkIntegration(ctx, tx.Tx(), peer.TargetIntegration)
		if err != nil {
			return err
		}

		integration, err = entry.ToAPI(ctx, tx.Tx())

		return err
	})
	if err != nil {
		return fmt.Errorf("Failed to load network integration %q: %w", peer.TargetIntegration, err)
	}

	// Get ICNB.
	icnb, err := networkOVN.NewICNB(integration.Config["ovn.northbound_connection"], integration.Config["ovn.ca_cert"], integration.Config["ovn.client_cert"], integration.Config["ovn.client_key"])
	if err != nil {
		return err
	}

	// Get the OVN AZ name.
	azName, err := n.ovnnb.GetName(ctx)
	if err != nil {
		return err
	}

	// Determine the transit switch name.
	pattern := integration.Config["ovn.transit.pattern"]
	if pattern == "" {
		pattern = "ts-incus-{{ integrationName }}-{{ projectName }}-{{ networkName }}"
	}

	tsNameRendered, err := internalUtil.RenderTemplate(pattern, pongo2.Context{
		"projectName":     n.project,
		"networkName":     n.name,
		"integrationName": integration.Name,
		"peerName":        peer.Name,
	})
	if err != nil {
		return err
	}

	tsName := networkOVN.OVNSwitch(tsNameRendered)

	// Delete logical switch port
	err = n.ovnnb.DeleteLogicalSwitchPort(ctx, tsName, networkOVN.OVNSwitchPort(fmt.Sprintf("%s-%s", tsName, azName)))
	if err != nil {
		return err
	}

	// Determine logical router port name.
	lrpName := networkOVN.OVNRouterPort(tsName)

	// Delete logical router port
	err = n.ovnnb.DeleteLogicalRouterPort(ctx, n.getRouterName(), lrpName)
	if err != nil {
		return err
	}

	// Determine the chassis group name.
	cgName := networkOVN.OVNChassisGroup(tsName)

	// Delete chassis group.
	err = n.ovnnb.DeleteChassisGroup(ctx, cgName)
	if err != nil && !errors.Is(err, networkOVN.ErrNotManaged) {
		return err
	}

	// Delete transit switch if empty
	icSwitch, err := n.ovnnb.GetLogicalSwitch(ctx, tsName)
	if err != nil {
		return err
	}

	if len(icSwitch.Ports) == 0 {
		err = icnb.DeleteTransitSwitch(ctx, string(tsName), false)
		if err != nil && !errors.Is(err, networkOVN.ErrNotManaged) {
			return err
		}
	} else {
		// Get the OVN AZ name.
		azName, err := n.ovnnb.GetName(ctx)
		if err != nil {
			return err
		}

		// Release peering addresses.
		err = icnb.DeleteTransitSwitchAllocation(ctx, string(tsName), azName)
		if err != nil {
			return err
		}
	}

	return nil
}

// PeerDelete deletes a network peering.
func (n *ovn) PeerDelete(peerName string) error {
	var peerID int64
	var peer *api.NetworkPeer

	err := n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		dbPeer, err := dbCluster.GetNetworkPeer(ctx, tx.Tx(), n.id, peerName)
		if err != nil {
			return fmt.Errorf("Failed getting network peer DB object: %w", err)
		}

		peerID = dbPeer.ID
		peer, err = dbPeer.ToAPI(ctx, tx.Tx())
		if err != nil {
			return fmt.Errorf("Failed converting network peer DB object to API object: %w", err)
		}

		return nil
	})
	if err != nil {
		return err
	}

	isUsed, err := n.peerIsUsed(peer.Name)
	if err != nil {
		return err
	}

	if isUsed {
		return errors.New("Cannot delete a peer that is in use")
	}

	if peer.Status == api.NetworkStatusCreated {
		if peer.Type == "local" {
			err := n.localPeerDelete(peer)
			if err != nil {
				return err
			}
		} else if peer.Type == "remote" {
			err := n.remotePeerDelete(peer)
			if err != nil {
				return err
			}
		}
	}

	err = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		// Deactivate any existing peer.
		if peer.Type == "local" {
			peers, err := dbCluster.GetNetworkPeers(ctx, tx.Tx(), dbCluster.NetworkPeerFilter{TargetNetworkID: &n.id})
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}

			for _, peer := range peers {
				peer.TargetNetworkID = sql.NullInt64{}

				err = dbCluster.UpdateNetworkPeer(ctx, tx.Tx(), peer.NetworkID, peer.Name, peer)
				if err != nil {
					return err
				}
			}
		}

		// Delete the peer.
		err := dbCluster.DeleteNetworkPeer(ctx, tx.Tx(), n.id, peerID)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return err
	}

	return nil
}

// forPeers runs f for each target peer network that this network is connected to.
func (n *ovn) forPeers(f func(targetOVNNet *ovn) error) error {
	var peers map[int64]*api.NetworkPeer

	err := n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		var err error

		// Use generated function to get peers.
		netID := n.ID()
		filter := dbCluster.NetworkPeerFilter{NetworkID: &netID}
		dbPeers, err := dbCluster.GetNetworkPeers(ctx, tx.Tx(), filter)
		if err != nil {
			return fmt.Errorf("Failed loading network peer DB objects: %w", err)
		}

		// Convert DB objects to API objects and build the map.
		peers = make(map[int64]*api.NetworkPeer, len(dbPeers))
		for _, dbPeer := range dbPeers {
			peer, err := dbPeer.ToAPI(ctx, tx.Tx())
			if err != nil {
				return fmt.Errorf("Failed converting network peer DB object to API object: %w", err)
			}

			peers[dbPeer.ID] = peer
		}

		return nil
	})
	if err != nil {
		return err
	}

	for _, peer := range peers {
		// Skip partially defined peers.
		if peer.Status != api.NetworkStatusCreated {
			continue
		}

		// Skip remote peers (no local networks to load).
		if peer.Type == "remote" {
			continue
		}

		targetNet, err := LoadByName(n.state, peer.TargetProject, peer.TargetNetwork)
		if err != nil {
			return fmt.Errorf("Failed loading target network: %w", err)
		}

		targetOVNNet, ok := targetNet.(*ovn)
		if !ok {
			return errors.New("Target network is not ovn interface type")
		}

		err = f(targetOVNNet)
		if err != nil {
			return err
		}
	}

	return nil
}

// loadBalancerBGPSetupPrefixes exports external load balancer addresses as prefixes.
func (n *ovn) loadBalancerBGPSetupPrefixes() error {
	listenAddresses := []string{}

	err := n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		networkID := n.ID()

		dbLoadBalancers, err := dbCluster.GetNetworkLoadBalancers(ctx, tx.Tx(), dbCluster.NetworkLoadBalancerFilter{
			NetworkID: &networkID,
		})
		if err != nil {
			return err
		}

		for _, lb := range dbLoadBalancers {
			listenAddresses = append(listenAddresses, lb.ListenAddress)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("Failed loading network forwards: %w", err)
	}

	listenAddressesByFamily := map[uint][]string{
		4: make([]string, 0),
		6: make([]string, 0),
	}

	for _, listenAddress := range listenAddresses {
		if strings.Contains(listenAddress, ":") {
			listenAddressesByFamily[6] = append(listenAddressesByFamily[6], listenAddress)
		} else {
			listenAddressesByFamily[4] = append(listenAddressesByFamily[4], listenAddress)
		}
	}

	// Use load balancer specific owner string (different from the network prefixes) so that these can be
	// reapplied independently of the network's own prefixes.
	bgpOwner := fmt.Sprintf("network_%d_load_balancer", n.id)

	// Clear existing address load balancer prefixes for network.
	err = n.state.BGP.RemovePrefixByOwner(bgpOwner)
	if err != nil {
		return err
	}

	// Add the new prefixes.
	for _, ipVersion := range []uint{4, 6} {
		nextHopAddr := n.bgpNextHopAddress(ipVersion)
		natEnabled := util.IsTrue(n.config[fmt.Sprintf("ipv%d.nat", ipVersion)])
		_, netSubnet, _ := net.ParseCIDR(n.config[fmt.Sprintf("ipv%d.address", ipVersion)])

		routeSubnetSize := 128
		if ipVersion == 4 {
			routeSubnetSize = 32
		}

		// Export external forward listen addresses.
		for _, listenAddress := range listenAddressesByFamily[ipVersion] {
			listenAddr := net.ParseIP(listenAddress)

			// Don't export internal address forwards (those inside the NAT enabled network's subnet).
			if natEnabled && netSubnet != nil && netSubnet.Contains(listenAddr) {
				continue
			}

			// Check health of load-balancer (if enabled).
			online := false
			for _, protocol := range []string{"tcp", "udp"} {
				lb, err := n.ovnnb.GetLoadBalancer(context.TODO(), networkOVN.OVNLoadBalancer(fmt.Sprintf("incus-net%d-lb-%s-%s", n.id, listenAddr.String(), protocol)))
				if err != nil {
					continue
				}

				lbOnline, err := n.ovnsb.CheckLoadBalancerOnline(context.TODO(), *lb)
				if err != nil {
					continue
				}

				if lbOnline {
					online = true
					break
				}
			}

			if !online {
				continue
			}

			_, ipRouteSubnet, err := net.ParseCIDR(fmt.Sprintf("%s/%d", listenAddr.String(), routeSubnetSize))
			if err != nil {
				return err
			}

			err = n.state.BGP.AddPrefix(*ipRouteSubnet, nextHopAddr, bgpOwner)
			if err != nil {
				return err
			}
		}
	}

	return nil
}
