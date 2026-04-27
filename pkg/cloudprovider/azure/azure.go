/*
Copyright The Kubernetes Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	resourceapi "k8s.io/api/resource/v1"
	"sigs.k8s.io/dranet/internal/nlwrap"
	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/cloudprovider"
)

const (
	AzureAttrPrefix = "azure.dra.net"

	AttrAzurePlacementGroupID = AzureAttrPrefix + "/" + "placementGroupId"
	AttrAzureVMSize           = AzureAttrPrefix + "/" + "vmSize"

	// imdsEndpoint is the Azure Instance Metadata Service endpoint.
	imdsEndpoint = "http://169.254.169.254/metadata/instance"
	// imdsAPIVersion is the API version used for IMDS queries.
	imdsAPIVersion = "2021-02-01"
	// imdsPathCompute is the IMDS path for compute metadata.
	imdsPathCompute = "compute"
	// imdsPathNetwork is the IMDS path for network metadata.
	imdsPathNetwork = "network"
)

// imdsComputeMetadata contains the fields we care about from the Azure IMDS
// compute metadata response.
type imdsComputeMetadata struct {
	PlacementGroupID string `json:"placementGroupId"`
	VMSize           string `json:"vmSize"`
}

// imdsResponse represents the top-level IMDS response structure.
type imdsResponse struct {
	Compute imdsComputeMetadata `json:"compute"`
}

// imdsNetworkResponse represents the IMDS network metadata response.
type imdsNetworkResponse struct {
	Interface []networkInterface `json:"interface"`
}

// networkInterface represents a single network interface from IMDS.
type networkInterface struct {
	IPv4       ipv4Config `json:"ipv4"`
	IPv6       ipv6Config `json:"ipv6"`
	MacAddress string     `json:"macAddress"`
}

// ipv4Config represents the IPv4 configuration from IMDS.
type ipv4Config struct {
	IPAddress []ipv4Address `json:"ipAddress"`
	Subnet    []subnet      `json:"subnet"`
}

// ipv6Config represents the IPv6 configuration from IMDS.
type ipv6Config struct {
	IPAddress []ipv6Address `json:"ipAddress"`
}

// ipv4Address represents a single IPv4 address from IMDS.
type ipv4Address struct {
	PrivateIPAddress string `json:"privateIpAddress"`
	PublicIPAddress  string `json:"publicIpAddress"`
}

// ipv6Address represents a single IPv6 address from IMDS.
type ipv6Address struct {
	PrivateIPAddress string `json:"privateIpAddress"`
}

// subnet represents a subnet from IMDS.
type subnet struct {
	Address string `json:"address"`
	Prefix  string `json:"prefix"`
}

var _ cloudprovider.CloudInstance = (*AzureInstance)(nil)

// AzureInstance holds Azure-specific instance data retrieved from IMDS.
type AzureInstance struct {
	PlacementGroupID string
	VMSize           string
	Interfaces       []networkInterface
}

// GetDeviceAttributes returns Azure-specific attributes for a device.
// PlacementGroupID and VMSize are node-level properties that apply to all
// devices on the node.
func (a *AzureInstance) GetDeviceAttributes(id cloudprovider.DeviceIdentifiers) map[resourceapi.QualifiedName]resourceapi.DeviceAttribute {
	attributes := make(map[resourceapi.QualifiedName]resourceapi.DeviceAttribute)

	if a.VMSize != "" {
		attributes[AttrAzureVMSize] = resourceapi.DeviceAttribute{StringValue: &a.VMSize}
	}

	if a.PlacementGroupID != "" {
		attributes[AttrAzurePlacementGroupID] = resourceapi.DeviceAttribute{StringValue: &a.PlacementGroupID}
	}

	return attributes
}

const (
	// routingTableBase is the base routing table ID for policy routing.
	// Each NIC gets its own table: routingTableBase + nicIndex.
	routingTableBase = 100
)

// GetDeviceConfig returns Azure-specific network configuration (rules and routes)
// for a device identified by its MAC address. It reads subnet info from IMDS
// and computes policy routing rules and routes for both IPv4 and IPv6.
func (a *AzureInstance) GetDeviceConfig(id cloudprovider.DeviceIdentifiers) *apis.NetworkConfig {
	if id.MAC == "" {
		return nil
	}

	normalizedMAC := normalizeMAC(id.MAC)

	var iface *networkInterface
	var nicIndex int
	for i := range a.Interfaces {
		if normalizeMAC(a.Interfaces[i].MacAddress) == normalizedMAC {
			iface = &a.Interfaces[i]
			nicIndex = i
			break
		}
	}
	if iface == nil {
		klog.V(4).Infof("No Azure IMDS network interface found for MAC %q", id.MAC)
		return nil
	}

	config := &apis.NetworkConfig{}
	tableID := routingTableBase + nicIndex

	// IPv4 rules and routes
	if len(iface.IPv4.Subnet) > 0 {
		subnet := iface.IPv4.Subnet[0]
		gateway, err := subnetFirstAddress(subnet.Address, subnet.Prefix)
		if err != nil {
			klog.Warningf("Could not compute gateway for subnet %s/%s: %v", subnet.Address, subnet.Prefix, err)
		} else {
			config.Rules = append(config.Rules, apis.RuleConfig{
				Source: subnet.Address + "/" + subnet.Prefix,
				Table:  tableID,
			})
			config.Routes = append(config.Routes, apis.RouteConfig{
				Destination: "0.0.0.0/0",
				Gateway:     gateway,
				Table:       tableID,
			})
		}
	}

	// IPv6 rules and routes (only if non-link-local IPv6 addresses are configured)
	ipv6Gw := getIPv6DefaultGateway(id.Name)
	if ipv6Gw != "" && len(iface.IPv6.IPAddress) > 0 && iface.IPv6.IPAddress[0].PrivateIPAddress != "" &&
		!isIPv6LinkLocal(iface.IPv6.IPAddress[0].PrivateIPAddress) {
		ipv6Addr := iface.IPv6.IPAddress[0].PrivateIPAddress
		config.Rules = append(config.Rules, apis.RuleConfig{
			Source: ipv6Addr + "/128",
			Table:  tableID,
		})
		config.Routes = append(config.Routes, apis.RouteConfig{
			Destination: "::/0",
			Gateway:     ipv6Gw,
			Table:       tableID,
		})
	}

	// Return nil if no rules/routes were generated
	if len(config.Rules) == 0 && len(config.Routes) == 0 {
		return nil
	}

	return config
}

// normalizeMAC converts a MAC address to a consistent lowercase, colon-free format
// for comparison. Azure IMDS returns MACs like "0011AAFFBB22" while Linux uses
// "00:11:aa:ff:bb:22".
func normalizeMAC(mac string) string {
	return strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(mac, ":", ""), "-", ""))
}

// getIPv6DefaultGateway returns the IPv6 default gateway for the given
// interface name by inspecting IPv6 default routes in the main routing table,
// or an empty string if none is found.
func getIPv6DefaultGateway(ifName string) string {
	link, err := nlwrap.LinkByName(ifName)
	if err != nil {
		klog.V(4).Infof("Failed to look up link %s for IPv6 gateway discovery: %v", ifName, err)
		return ""
	}
	filter := &netlink.Route{
		Table:     unix.RT_TABLE_MAIN,
		LinkIndex: link.Attrs().Index,
	}
	routes, err := nlwrap.RouteListFiltered(netlink.FAMILY_V6, filter, netlink.RT_FILTER_TABLE|netlink.RT_FILTER_OIF)
	if err != nil {
		klog.Warningf("Failed to list IPv6 routes for %s: %v", ifName, err)
		return ""
	}
	for _, r := range routes {
		// Default route: Dst is nil or ::/0
		if r.Dst != nil {
			ones, bits := r.Dst.Mask.Size()
			if !r.Dst.IP.IsUnspecified() || ones != 0 || bits != 128 {
				continue
			}
		}
		if r.Gw != nil {
			return r.Gw.String()
		}
	}
	return ""
}

// isIPv6LinkLocal returns true if the given address is an IPv6 link-local
// address (fe80::/10).
func isIPv6LinkLocal(addr string) bool {
	ip := net.ParseIP(addr)
	return ip != nil && ip.IsLinkLocalUnicast()
}

// subnetFirstAddress returns the first usable IP address in a subnet
// (the subnet address + 1). For example, for ("10.9.255.0", "24") it returns "10.9.255.1".
// It validates that the address and prefix form a valid IPv4 CIDR, that the
// address is the actual network base, and that the gateway falls within the subnet.
func subnetFirstAddress(subnetAddr, prefix string) (string, error) {
	ipPrefix, err := netip.ParsePrefix(subnetAddr + "/" + prefix)
	if err != nil {
		return "", fmt.Errorf("invalid CIDR %s/%s: %w", subnetAddr, prefix, err)
	}
	if !ipPrefix.Addr().Is4() {
		return "", fmt.Errorf("%s is not an IPv4 address", subnetAddr)
	}
	maskedPrefix := ipPrefix.Masked()
	// Verify the address is the network base (e.g., reject 10.0.0.5/24)
	if maskedPrefix.Addr().String() != subnetAddr {
		return "", fmt.Errorf("%s is not the network base for /%s (expected %s)", subnetAddr, prefix, maskedPrefix.Addr().String())
	}
	ipFirst := maskedPrefix.Addr().Next()
	if !maskedPrefix.Contains(ipFirst) {
		return "", fmt.Errorf("gateway %s falls outside subnet %s/%s", ipFirst, subnetAddr, prefix)
	}
	return ipFirst.String(), nil
}

// OnAzure returns true if the code is running on an Azure VM by probing the
// IMDS endpoint. It uses a context and active polling to avoid flaky behavior
// in corner cases such as slow network initialization.
func OnAzure(ctx context.Context) bool {
	client := &http.Client{}

	err := wait.PollUntilContextTimeout(ctx, 1*time.Second, 5*time.Second, true, func(ctx context.Context) (done bool, err error) {
		req, err := http.NewRequestWithContext(ctx, "GET", imdsEndpoint+"?api-version="+imdsAPIVersion+"&format=text", nil)
		if err != nil {
			return false, nil
		}
		req.Header.Set("Metadata", "true")
		resp, err := client.Do(req)
		if err != nil {
			return false, nil
		}
		defer resp.Body.Close()
		// IMDS returns 200 on Azure VMs. Any successful response indicates Azure.
		return resp.StatusCode == http.StatusOK, nil
	})

	return err == nil
}

// queryIMDS performs a GET request to the given Azure IMDS URL
// with retry logic and unmarshals the JSON response into result.
func queryIMDS(ctx context.Context, client *http.Client, url string, result interface{}) error {
	return wait.PollUntilContextTimeout(ctx, 1*time.Second, 15*time.Second, true, func(ctx context.Context) (done bool, err error) {
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			klog.Infof("could not create Azure IMDS request for %s ... retrying: %v", url, err)
			return false, nil
		}
		req.Header.Set("Metadata", "true")

		resp, err := client.Do(req)
		if err != nil {
			klog.Infof("could not query Azure IMDS %s ... retrying: %v", url, err)
			return false, nil
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			klog.Infof("Azure IMDS %s returned status %d ... retrying", url, resp.StatusCode)
			return false, nil
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			klog.Infof("could not read Azure IMDS response from %s ... retrying: %v", url, err)
			return false, nil
		}

		if err := json.Unmarshal(body, result); err != nil {
			klog.Infof("could not parse Azure IMDS response from %s ... retrying: %v", url, err)
			return false, nil
		}

		return true, nil
	})
}

// GetInstance retrieves Azure instance properties by querying IMDS.
func GetInstance(ctx context.Context) (cloudprovider.CloudInstance, error) {
	client := &http.Client{Timeout: 5 * time.Second}

	var computeMetadata imdsComputeMetadata
	computeURL := fmt.Sprintf("%s/%s?api-version=%s&format=json", imdsEndpoint, imdsPathCompute, imdsAPIVersion)
	if err := queryIMDS(ctx, client, computeURL, &computeMetadata); err != nil {
		return nil, err
	}

	instance := &AzureInstance{
		PlacementGroupID: computeMetadata.PlacementGroupID,
		VMSize:           computeMetadata.VMSize,
	}
	klog.Infof("Azure IMDS: vmSize=%s, placementGroupId=%s", instance.VMSize, instance.PlacementGroupID)

	// Fetch network interface metadata in a separate call.
	var networkResp imdsNetworkResponse
	networkURL := fmt.Sprintf("%s/%s?api-version=%s&format=json", imdsEndpoint, imdsPathNetwork, imdsAPIVersion)
	if err := queryIMDS(ctx, client, networkURL, &networkResp); err != nil {
		klog.Warningf("Failed to retrieve Azure IMDS network metadata: %v", err)
	} else {
		instance.Interfaces = networkResp.Interface
		klog.Infof("Azure IMDS: retrieved %d network interfaces", len(instance.Interfaces))
	}

	return instance, nil
}
