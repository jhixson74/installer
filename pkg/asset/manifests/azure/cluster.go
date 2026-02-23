package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"

	aznetwork "github.com/Azure/azure-sdk-for-go/profiles/2020-09-01/network/mgmt/network"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	capz "sigs.k8s.io/cluster-api-provider-azure/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift/installer/pkg/asset"
	"github.com/openshift/installer/pkg/asset/installconfig"
	azic "github.com/openshift/installer/pkg/asset/installconfig/azure"
	capzash "github.com/openshift/installer/pkg/asset/manifests/azure/stack/v1beta1"
	"github.com/openshift/installer/pkg/asset/manifests/capiutils"
	"github.com/openshift/installer/pkg/asset/manifests/capiutils/cidr"
	"github.com/openshift/installer/pkg/ipnet"
	"github.com/openshift/installer/pkg/types"
	"github.com/openshift/installer/pkg/types/azure"
	"github.com/openshift/installer/pkg/types/dns"
)

// GenerateClusterAssets generates the manifests for the cluster-api.
func GenerateClusterAssets(installConfig *installconfig.InstallConfig, clusterID *installconfig.ClusterID) (*capiutils.GenerateClusterAssetsOutput, error) {
	manifests := []*asset.RuntimeFile{}
	resourceGroup := installConfig.Config.Platform.Azure.ClusterResourceGroupName(clusterID.InfraID)
	controlPlaneSubnet := installConfig.Config.Platform.Azure.ControlPlaneSubnetName(clusterID.InfraID)
	computeSubnet := installConfig.Config.Platform.Azure.ComputeSubnetName(clusterID.InfraID)
	networkSecurityGroup := installConfig.Config.Platform.Azure.NetworkSecurityGroupName(clusterID.InfraID)
	securityGroup := capz.SecurityGroup{Name: networkSecurityGroup}

	cidrs := capiutils.CIDRsFromInstallConfig(installConfig)
	addressFamilySubnets, err := getAddressFamilySubnets(cidrs)
	if err != nil {
		return nil, errors.Wrap(err, "failed to split cidrs into subnets")
	}

	securityRule := capz.SecurityRule{
		Name:             "apiserver_in",
		Protocol:         capz.SecurityGroupProtocolTCP,
		Direction:        capz.SecurityRuleDirectionInbound,
		Priority:         100,
		SourcePorts:      ptr.To("*"),
		DestinationPorts: ptr.To("6443"),
		Source:           ptr.To("*"),
		Destination:      ptr.To("*"),
		Action:           capz.SecurityRuleActionAllow,
	}

	// If we are using Internal publishing, we need a security rule for each CIDR
	var securityRules []capz.SecurityRule
	var securityRulePriority int32 = 100
	if addressFamilySubnets.IPv4Count() > 0 && installConfig.Config.Publish == types.InternalPublishingStrategy {
		for i := 0; i < addressFamilySubnets.IPv4Count(); i++ {
			ipv4SecurityRule := securityRule
			ipv4SecurityRule.Name = fmt.Sprintf("apiserver_in_ipv4_%02d", i)
			ipv4SecurityRule.Source = to.Ptr(addressFamilySubnets.GetIPv4Subnets()[i].String())
			ipv4SecurityRule.Priority = securityRulePriority
			securityRules = append(securityRules, ipv4SecurityRule)
			securityRulePriority += 10
		}
	}
	if addressFamilySubnets.IPv6Count() > 0 && installConfig.Config.Publish == types.InternalPublishingStrategy {
		for i := 0; i < addressFamilySubnets.IPv6Count(); i++ {
			ipv6SecurityRule := securityRule
			ipv6SecurityRule.Name = fmt.Sprintf("apiserver_in_ipv6_%02d", i)
			ipv6SecurityRule.Source = to.Ptr(addressFamilySubnets.GetIPv6Subnets()[i].String())
			ipv6SecurityRule.Priority = securityRulePriority
			securityRules = append(securityRules, ipv6SecurityRule)
			securityRulePriority += 10
		}
	}
	if len(securityRules) == 0 {
		securityRules = append(securityRules, securityRule)
	}
	securityGroup.SecurityGroupClass.SecurityRules = securityRules

	session, err := installConfig.Azure.Session()
	if err != nil {
		return nil, errors.Wrap(err, "failed to create Azure session")
	}

	splitLength := 0
	zones := []string{}
	// This is only relevant to IPv4 - v1 NAT gateways do not support IPv6
	if installConfig.Config.Azure.OutboundType == azure.NATGatewayMultiZoneOutboundType {
		numZones, err := installConfig.Azure.GenerateZonesSubnetMap(installConfig.Config.Azure.Subnets, computeSubnet)
		if err != nil {
			return nil, fmt.Errorf("failed to get availability zones: %w", err)
		}
		for key := range numZones {
			zones = append(zones, key)
		}
		sort.Strings(zones)
		// Add one for control plane.
		splitLength = len(zones) + 1
	}

	err = addressFamilySubnets.SplitComputeSubnet(splitLength)
	if err != nil {
		return nil, errors.Wrap(err, "unable to split compute subnet")
	}
	virtualNetworkAddressPrefixes := getVirtualNetworkAddressPrefixes(cidrs)

	// CAPZ expects the capz-system to be created.
	azureNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "capz-system"}}
	azureNamespace.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Namespace"))
	manifests = append(manifests, &asset.RuntimeFile{
		Object: azureNamespace,
		File:   asset.File{Filename: "00_azure-namespace.yaml"},
	})

	// Setting ID on the Subnet disables natgw creation. See:
	// https://github.com/kubernetes-sigs/cluster-api-provider-azure/blob/21479a9a4c640b43e0bef028487c522c55605d06/api/v1beta1/azurecluster_default.go#L160
	// CAPZ enables NAT Gateways by default, so we are using this hack to disable
	// nat gateways when we prefer to use load balancers for node egress.
	nodeSubnetID := ""
	switch installConfig.Config.Platform.Azure.OutboundType {
	// Because the node subnet does not already exist, we are using an arbitrary value.
	// We could populate this with the proper subnet ID in the case of BYO VNET, but
	// the value currently has no practical effect.
	case azure.LoadbalancerOutboundType:
		fallthrough
	case azure.UserDefinedRoutingOutboundType:
		nodeSubnetID = "UNKNOWN"
	}

	apiServerLBFrontendConfigName := fmt.Sprintf("%s-internal-frontEnd", clusterID.InfraID)
	apiServerBackendPoolName := fmt.Sprintf("%s-internal", clusterID.InfraID)
	apiServerLB := capz.LoadBalancerSpec{
		Name: fmt.Sprintf("%s-internal", clusterID.InfraID),
		BackendPool: capz.BackendPool{
			//Name: fmt.Sprintf("%s-internal", clusterID.InfraID),
			Name: apiServerBackendPoolName,
		},
		LoadBalancerClassSpec: capz.LoadBalancerClassSpec{
			Type: capz.Internal,
		},
	}

	controlPlaneBackendPoolName := fmt.Sprintf("%s-outbound-lb-outboundBackendPool", clusterID.InfraID)
	controlPlaneOutboundLB := &capz.LoadBalancerSpec{
		Name:             clusterID.InfraID,
		FrontendIPsCount: to.Ptr(int32(1)),
		BackendPool: capz.BackendPool{
			Name: controlPlaneBackendPoolName,
		},
	}

	// IPv6 outbound load balancer for worker subnet(s)
	var nodeOutboundLB *capz.LoadBalancerSpec
	if addressFamilySubnets.IsDualStack() {
		lbip6, err := getLBIP(addressFamilySubnets.GetIPv6Subnets(), installConfig)
		if err != nil {
			return nil, err
		}

		nodeOutboundLB = &capz.LoadBalancerSpec{
			Name:             clusterID.InfraID,
			FrontendIPsCount: to.Ptr(int32(1)),
			FrontendIPs: []capz.FrontendIP{{
				FrontendIPClass: capz.FrontendIPClass{
					PrivateIPAddress: lbip6,
				},
			}},
		}
	}

	if installConfig.Config.Platform.Azure.OutboundType == azure.UserDefinedRoutingOutboundType {
		controlPlaneOutboundLB = nil
	}

	virtualNetworkID := ""
	lbip4, err := getLBIP(addressFamilySubnets.GetIPv4Subnets(), installConfig)
	if err != nil {
		return nil, err
	}
	apiServerLB.FrontendIPs = []capz.FrontendIP{{
		Name: apiServerLBFrontendConfigName,
		FrontendIPClass: capz.FrontendIPClass{
			PrivateIPAddress: lbip4,
		},
	}}

	// For dual-stack, add IPv6 frontend IP for internal LB (dynamic allocation)
	if installConfig.Config.Azure.IPFamily.DualStackEnabled() {
		apiServerLB.FrontendIPs = append(apiServerLB.FrontendIPs, capz.FrontendIP{
			Name: fmt.Sprintf("%s-internal-frontEnd-v6", clusterID.InfraID),
			// No PrivateIPAddress specified = dynamic allocation
		})
	}
	vnetResourceGroup := installConfig.Config.Azure.ResourceGroupName
	if installConfig.Config.Azure.VirtualNetwork != "" {
		virtualNetworkAddressPrefixes = make([]string, 0)
		vnetResourceGroup = installConfig.Config.Azure.NetworkResourceGroupName
		client, err := installConfig.Azure.Client()
		if err != nil {
			return nil, fmt.Errorf("failed to get azure client: %w", err)
		}
		ctx := context.TODO()
		virtualNetwork, err := client.GetVirtualNetwork(ctx, installConfig.Config.Azure.NetworkResourceGroupName, installConfig.Config.Azure.VirtualNetwork)
		if err != nil {
			return nil, fmt.Errorf("failed to get azure virtual network: %w", err)
		}
		if virtualNetwork != nil {
			virtualNetworkID = *virtualNetwork.ID
		}
		lbip4, err := getNextAvailableIPForLoadBalancer(ctx, installConfig, lbip4)
		if err != nil {
			return nil, err
		}
		apiServerLB.FrontendIPs[0].Name = apiServerLBFrontendConfigName
		apiServerLB.FrontendIPs[0].FrontendIPClass = capz.FrontendIPClass{
			PrivateIPAddress: lbip4,
		}
		if virtualNetwork.AddressSpace != nil && virtualNetwork.AddressSpace.AddressPrefixes != nil {
			virtualNetworkAddressPrefixes = append(virtualNetworkAddressPrefixes, *virtualNetwork.AddressSpace.AddressPrefixes...)
		}
	} else {
		apiServerLB.FrontendIPs = []capz.FrontendIP{{
			Name: apiServerLBFrontendConfigName,
			FrontendIPClass: capz.FrontendIPClass{
				PrivateIPAddress: lbip4,
			},
		}}
	}

	azEnv := string(installConfig.Azure.CloudName)
	privateDNSZoneMode := capz.PrivateDNSZoneModeSystem
	// When UserProvisionedDNS is enabled, prevent automatic creation of private DNS zone
	// because the cloud DNS will not be used. Instead, an in-cluster DNS will be configured
	// to resolve api, api-int and *apps URLs.
	if installConfig.Config.Azure.UserProvisionedDNS == dns.UserProvisionedDNSEnabled {
		privateDNSZoneMode = capz.PrivateDNSZoneModeNone
	}

	subnetSpec, err := getSubnetSpec(installConfig, controlPlaneSubnet, computeSubnet, securityGroup, &addressFamilySubnets, nodeSubnetID, clusterID.InfraID, zones)
	if err != nil {
		return nil, fmt.Errorf("failed to get subnets: %w", err)
	}
	azureCluster := &capz.AzureCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterID.InfraID,
			Namespace: capiutils.Namespace,
		},
		Spec: capz.AzureClusterSpec{
			ResourceGroup: resourceGroup,
			AzureClusterClassSpec: capz.AzureClusterClassSpec{
				SubscriptionID:   session.Credentials.SubscriptionID,
				Location:         installConfig.Config.Azure.Region,
				AdditionalTags:   installConfig.Config.Platform.Azure.UserTags,
				AzureEnvironment: azEnv,
				IdentityRef: &corev1.ObjectReference{
					APIVersion: capz.GroupVersion.String(),
					Kind:       "AzureClusterIdentity",
					Name:       clusterID.InfraID,
				},
			},
			NetworkSpec: capz.NetworkSpec{
				NetworkClassSpec: capz.NetworkClassSpec{
					PrivateDNSZoneName: installConfig.Config.ClusterDomain(),
				},
				Vnet: capz.VnetSpec{
					ResourceGroup: vnetResourceGroup,
					Name:          installConfig.Config.Azure.VirtualNetwork,
					// The ID is set to virtual network here for existing vnets here. This is to force CAPZ to consider this resource as
					// "not managed" which would prevent the creation of an additional nsg and route table in the network resource group.
					// The ID field is not used for any other purpose in CAPZ except to set the "managed" status.
					// See https://github.com/kubernetes-sigs/cluster-api-provider-azure/blob/main/azure/scope/cluster.go#L585
					// https://github.com/kubernetes-sigs/cluster-api-provider-azure/commit/0f321e4089a3f4dc37f8420bf2ef6762c398c400
					ID: virtualNetworkID,
					VnetClassSpec: capz.VnetClassSpec{
						CIDRBlocks: virtualNetworkAddressPrefixes,
					},
				},
				APIServerLB:            &apiServerLB,
				NodeOutboundLB:         nodeOutboundLB,
				ControlPlaneOutboundLB: controlPlaneOutboundLB,
				Subnets:                subnetSpec,
				PrivateDNSZone:         &privateDNSZoneMode,
			},
		},
	}

	// We are maintaining a fork of CAPZ for azurestack. The only API difference
	// is the ARMEndpoint field, so we can use the CAPZ cluster object, and if
	// running on ASH convert to the fork API, and add the field.
	var cluster client.Object
	if !strings.EqualFold(azEnv, string(azure.StackCloud)) {
		azureCluster.SetGroupVersionKind(capz.GroupVersion.WithKind("AzureCluster"))
		cluster = azureCluster
	} else {
		var ashCluster capzash.AzureCluster
		if err := deepCopy(azureCluster, &ashCluster); err != nil {
			return nil, fmt.Errorf("failed to convert azureCluster to azure-stack cluster: %w", err)
		}
		ashCluster.Spec.ARMEndpoint = session.Environment.ServiceManagementEndpoint
		ashCluster.SetGroupVersionKind(capzash.GroupVersion.WithKind("AzureCluster"))
		cluster = &ashCluster
	}

	manifests = append(manifests, &asset.RuntimeFile{
		Object: cluster,
		File:   asset.File{Filename: "02_azure-cluster.yaml"},
	})

	id := &capz.AzureClusterIdentity{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterID.InfraID,
			Namespace: capiutils.Namespace,
		},
		Spec: capz.AzureClusterIdentitySpec{
			AllowedNamespaces: &capz.AllowedNamespaces{}, // Allow all namespaces.
			ClientID:          session.Credentials.ClientID,
			TenantID:          session.Credentials.TenantID,
		},
	}

	switch session.AuthType {
	case azic.ManagedIdentityAuth:
		id.Spec.Type = capz.UserAssignedMSI
	case azic.ClientSecretAuth:
		id.Spec.Type = capz.ServicePrincipal
		azureClientSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      clusterID.InfraID + "-azure-client-secret",
				Namespace: capiutils.Namespace,
			},
			StringData: map[string]string{
				"clientSecret": session.Credentials.ClientSecret,
			},
		}
		azureClientSecret.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
		manifests = append(manifests, &asset.RuntimeFile{
			Object: azureClientSecret,
			File:   asset.File{Filename: "01_azure-client-secret.yaml"},
		})

		id.Spec.ClientSecret = corev1.SecretReference{
			Name:      azureClientSecret.Name,
			Namespace: azureClientSecret.Namespace,
		}
	case azic.ClientCertificateAuth:
		id.Spec.Type = capz.ServicePrincipalCertificate
		id.Spec.CertPath = session.Credentials.ClientCertificatePath
	}

	id.SetGroupVersionKind(capz.GroupVersion.WithKind("AzureClusterIdentity"))
	manifests = append(manifests, &asset.RuntimeFile{
		Object: id,
		File:   asset.File{Filename: "01_azure-cluster-controller-identity-default.yaml"},
	})

	return &capiutils.GenerateClusterAssetsOutput{
		Manifests: manifests,
		InfrastructureRefs: []*corev1.ObjectReference{
			{
				APIVersion: capz.GroupVersion.String(),
				Kind:       "AzureCluster",
				Name:       azureCluster.Name,
				Namespace:  azureCluster.Namespace,
			},
		},
	}, nil
}

func getSubnetSpec(installConfig *installconfig.InstallConfig, controlPlaneSubnet, computeSubnet string, securityGroup capz.SecurityGroup, addressFamilySubnets *AddressFamilySubnets, nodeSubnetID string, infraID string, zones []string) ([]capz.SubnetSpec, error) {
	ipv4Subnets := addressFamilySubnets.GetIPv4AdditionalSubnets()
	ipv6Subnets := addressFamilySubnets.GetIPv6AdditionalSubnets()

	ipv4ComputeSubnets := addressFamilySubnets.GetIPv4ComputeSubnets()
	ipv6ComputeSubnets := addressFamilySubnets.GetIPv6ComputeSubnets()

	ipv4ControlPlaneCIDRBlocks := addressFamilySubnets.GetIPv4ControlPlaneCIDRBlocks()
	ipv6ControlPlaneCIDRBlocks := addressFamilySubnets.GetIPv6ControlPlaneCIDRBlocks()
	controlPlaneCIDRBlocks := []string{ipv4ControlPlaneCIDRBlocks}
	if ipv6ControlPlaneCIDRBlocks != "" {
		controlPlaneCIDRBlocks = append(controlPlaneCIDRBlocks, ipv6ControlPlaneCIDRBlocks)
	}

	ipv4ComputeCIDRBlocks := addressFamilySubnets.GetIPv4ComputeCIDRBlocks()
	ipv6ComputeCIDRBlocks := addressFamilySubnets.GetIPv6ComputeCIDRBlocks()
	computeCIDRBlocks := ipv4ComputeCIDRBlocks
	if len(ipv6ComputeCIDRBlocks) > 0 {
		computeCIDRBlocks = append(computeCIDRBlocks, ipv6ComputeCIDRBlocks...)
	}

	// Set default control plane subnets for default installs.
	defaultControlPlaneSubnet := capz.Subnets{
		{
			SubnetClassSpec: capz.SubnetClassSpec{
				Name:       controlPlaneSubnet,
				Role:       capz.SubnetControlPlane,
				CIDRBlocks: controlPlaneCIDRBlocks,
			},
			SecurityGroup: securityGroup,
		},
	}
	defaultComputeSubnetSpec := capz.SubnetSpec{
		ID: nodeSubnetID,
		SubnetClassSpec: capz.SubnetClassSpec{
			Name:       computeSubnet,
			Role:       capz.SubnetNode,
			CIDRBlocks: computeCIDRBlocks,
		},
		SecurityGroup: securityGroup,
	}

	subnetSpec := []capz.SubnetSpec{}
	subnetsCount := max(len(ipv4Subnets), len(ipv6Subnets))
	for i := 0; i < subnetsCount; i++ {
		spec := capz.SubnetSpec{
			SubnetClassSpec: capz.SubnetClassSpec{
				Name: fmt.Sprintf("subnet_%02d", i),
				Role: capz.SubnetNode,
			},
		}
		var cidrBlocks []string
		if i < len(ipv4Subnets) {
			cidrBlocks = append(cidrBlocks, ipv4Subnets[i].String())
		}
		if i < len(ipv6Subnets) {
			cidrBlocks = append(cidrBlocks, ipv6Subnets[i].String())
		}
		spec.CIDRBlocks = cidrBlocks
		subnetSpec = append(subnetSpec, spec)
	}

	hasControlPlaneSubnet := false
	hasComputePlaneSubnet := false
	// Add the user specified subnets to the spec.
	// For single zone, alter the compute subnet to have a NATGateway and add default control plane subnet
	// configuration.
	zoneIndex := 0
	singleZoneNatGateway := false
	allSubnets := installConfig.Config.Azure.Subnets
	sort.Slice(allSubnets, func(i, j int) bool {
		return allSubnets[i].Name < allSubnets[j].Name
	})
	for index, spec := range allSubnets {
		subnet, err := getSubnet(installConfig, spec.Role, spec.Name)
		if err != nil {
			return nil, err
		}
		addresses, err := getSubnetAddressPrefixes(subnet)
		if err != nil {
			return nil, err
		}
		stringAddress := stringifyAddressPrefixes(addresses)
		specGen := capz.SubnetSpec{
			ID: *subnet.ID,
			SubnetClassSpec: capz.SubnetClassSpec{
				Name:       spec.Name,
				Role:       spec.Role,
				CIDRBlocks: stringAddress,
			},
			SecurityGroup: securityGroup,
		}

		if installConfig.Config.Azure.OutboundType == azure.NATGatewayMultiZoneOutboundType && spec.Role == capz.SubnetNode {
			specGen.NatGateway = capz.NatGateway{
				NatGatewayIP: capz.PublicIPSpec{
					Name: fmt.Sprintf("%s-publicip-%d", infraID, index),
				},
				NatGatewayClassSpec: capz.NatGatewayClassSpec{Name: fmt.Sprintf("%s-natgw-%d", infraID, index)},
				Zones:               []string{zones[zoneIndex]},
			}
			zoneIndex++
			if zoneIndex == len(zones) {
				zoneIndex = 0
			}
		} else if installConfig.Config.Azure.OutboundType == azure.NATGatewaySingleZoneOutboundType && spec.Role == capz.SubnetNode && !singleZoneNatGateway {
			specGen.NatGateway = capz.NatGateway{
				NatGatewayIP: capz.PublicIPSpec{
					Name: fmt.Sprintf("%s-publicip-%d", infraID, index),
				},
				NatGatewayClassSpec: capz.NatGatewayClassSpec{Name: fmt.Sprintf("%s-natgw-%d", infraID, index)},
			}
			singleZoneNatGateway = true
		}
		hasControlPlaneSubnet = hasControlPlaneSubnet || spec.Role == capz.SubnetControlPlane
		hasComputePlaneSubnet = hasComputePlaneSubnet || spec.Role == capz.SubnetNode
		subnetSpec = append(subnetSpec, specGen)
	}
	zoneIndex = 0
	// Make sure there's at least one subnet for compute and control plane.
	// Ordinary installs will get the default setup.
	if !hasComputePlaneSubnet {
		// For single zone, add a NAT gateway to the default value.
		if installConfig.Config.Azure.OutboundType == azure.NATGatewayMultiZoneOutboundType {
			for index, subnet := range ipv4ComputeSubnets {
				name := fmt.Sprintf("%s-%d", computeSubnet, index)
				if index == 1 {
					name = computeSubnet
				}
				specSubnet := capz.SubnetSpec{
					SubnetClassSpec: capz.SubnetClassSpec{
						Name: name,
						Role: capz.SubnetNode,
						CIDRBlocks: []string{
							subnet.String(),
						},
					},
					NatGateway: capz.NatGateway{
						NatGatewayClassSpec: capz.NatGatewayClassSpec{Name: fmt.Sprintf("%s-natgw-%d", infraID, index)},
						Zones:               []string{zones[zoneIndex]},
					},
					SecurityGroup: securityGroup,
				}
				if index < len(ipv6ComputeSubnets) {
					specSubnet.SubnetClassSpec.CIDRBlocks = append(specSubnet.SubnetClassSpec.CIDRBlocks, ipv6ComputeSubnets[index].String())
				}
				zoneIndex++
				if zoneIndex == len(zones) {
					zoneIndex = 0
				}
				subnetSpec = append(subnetSpec, specSubnet)
			}
		} else {
			if installConfig.Config.Azure.OutboundType == azure.NATGatewaySingleZoneOutboundType {
				defaultComputeSubnetSpec.NatGateway = capz.NatGateway{
					NatGatewayClassSpec: capz.NatGatewayClassSpec{Name: fmt.Sprintf("%s-natgw", infraID)},
				}
			}
			subnetSpec = append(subnetSpec, defaultComputeSubnetSpec)
		}
	}
	if !hasControlPlaneSubnet {
		subnetSpec = append(subnetSpec, defaultControlPlaneSubnet...)
	}
	return subnetSpec, nil
}

func getLBIP(subnets []*net.IPNet, installConfig *installconfig.InstallConfig) (string, error) {
	lbip := capz.DefaultInternalLBIPAddress
	lbip = getIPWithinCIDR(subnets, lbip)

	var controlPlaneSub string
	for _, subnet := range installConfig.Config.Azure.Subnets {
		if subnet.Role == capz.SubnetControlPlane {
			controlPlaneSub = subnet.Name
		}
	}

	if controlPlaneSub != "" {
		client, err := installConfig.Azure.Client()
		if err != nil {
			return "", fmt.Errorf("failed to get azure client: %w", err)
		}
		ctx := context.TODO()
		controlPlaneSubnet, err := client.GetControlPlaneSubnet(ctx, installConfig.Config.Azure.NetworkResourceGroupName, installConfig.Config.Azure.VirtualNetwork, controlPlaneSub)
		if err != nil || controlPlaneSubnet == nil {
			return "", fmt.Errorf("failed to get azure control plane subnet: %w", err)
		} else if controlPlaneSubnet.AddressPrefixes == nil && controlPlaneSubnet.AddressPrefix == nil {
			return "", fmt.Errorf("failed to get azure control plane subnet addresses: %w", err)
		}
		subnetList := []*net.IPNet{}
		if controlPlaneSubnet.AddressPrefixes != nil {
			for _, sub := range *controlPlaneSubnet.AddressPrefixes {
				_, ipnet, err := net.ParseCIDR(sub)
				if err != nil {
					return "", fmt.Errorf("failed to get translate azure control plane subnet addresses: %w", err)
				}
				subnetList = append(subnetList, ipnet)
			}
		}

		if controlPlaneSubnet.AddressPrefix != nil {
			_, ipnet, err := net.ParseCIDR(*controlPlaneSubnet.AddressPrefix)
			if err != nil {
				return "", fmt.Errorf("failed to get translate azure control plane subnet address prefix: %w", err)
			}
			subnetList = append(subnetList, ipnet)
		}
		lbip = getIPWithinCIDR(subnetList, lbip)
	}
	return lbip, nil
}

func getSubnet(installConfig *installconfig.InstallConfig, subnetType capz.SubnetRole, subnetName string) (*aznetwork.Subnet, error) {
	var subnet *aznetwork.Subnet

	azClient, err := installConfig.Azure.Client()
	if err != nil {
		return nil, fmt.Errorf("failed to get azure client: %w", err)
	}
	ctx := context.TODO()

	switch subnetType {
	case capz.SubnetControlPlane:
		subnet, err = azClient.GetControlPlaneSubnet(ctx,
			installConfig.Config.Azure.NetworkResourceGroupName,
			installConfig.Config.Azure.VirtualNetwork,
			subnetName,
		)
	case capz.SubnetNode:
		subnet, err = azClient.GetComputeSubnet(ctx,
			installConfig.Config.Azure.NetworkResourceGroupName,
			installConfig.Config.Azure.VirtualNetwork,
			subnetName,
		)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to get subnet: %w", err)
	}
	if subnet == nil {
		return nil, fmt.Errorf("failed to get subnet")
	}
	if subnet.AddressPrefixes == nil && subnet.AddressPrefix == nil {
		return nil, fmt.Errorf("failed to get subnet addresses: %w", err)
	}
	return subnet, nil
}

func getSubnetAddressPrefixes(subnet *aznetwork.Subnet) ([]*net.IPNet, error) {
	subnetList := []*net.IPNet{}
	if subnet.AddressPrefixes != nil {
		for _, sub := range *subnet.AddressPrefixes {
			_, ipnet, err := net.ParseCIDR(sub)
			if err != nil {
				return subnetList, fmt.Errorf("failed to get translate azure subnet addresses: %w", err)
			}
			subnetList = append(subnetList, ipnet)
		}
	}
	if subnet.AddressPrefix != nil {
		_, ipnet, err := net.ParseCIDR(*subnet.AddressPrefix)
		if err != nil {
			return subnetList, fmt.Errorf("failed to get translate azure subnet address prefix: %w", err)
		}
		subnetList = append(subnetList, ipnet)
	}

	return subnetList, nil
}

func stringifyAddressPrefixes(addressPrefixes []*net.IPNet) []string {
	strAddressPrefixes := []string{}
	for _, addressPrefix := range addressPrefixes {
		strAddressPrefixes = append(strAddressPrefixes, addressPrefix.String())
	}
	return strAddressPrefixes
}

// AddressFamilySubnet holds an IPv4 and IPv6 subnet pair.
type AddressFamilySubnet struct {
	IPv4Subnet *net.IPNet
	IPv6Subnet *net.IPNet
	SubnetRole *capz.SubnetRole
}

// AddressFamilySubnets keeps track of IPv4 and IPv6 subnets.
type AddressFamilySubnets struct {
	addressFamilySubnets []AddressFamilySubnet
	length               int
	ipv4Count            int
	ipv6Count            int
	// XXX: leave this for now, fix to use install-config later
	isDualStack bool
}

// Length returns the number of subnets in the list.
func (a AddressFamilySubnets) Length() int {
	return a.length
}

// IPv4Count returns the number of IPv4 subnets in the list.
func (a AddressFamilySubnets) IPv4Count() int {
	return a.ipv4Count
}

// IPv6Count returns the number of IPv6 subnets in the list.
func (a AddressFamilySubnets) IPv6Count() int {
	return a.ipv6Count
}

// IsDualStack determines if we are using single or dual stack networking.
func (a AddressFamilySubnets) IsDualStack() bool {
	return a.isDualStack
}

// GetIPv4Subnets returns all IPv4 subnets in the list.
func (a AddressFamilySubnets) GetIPv4Subnets() []*net.IPNet {
	var ipv4Subnets []*net.IPNet
	for _, ipv4Subnet := range a.addressFamilySubnets {
		if ipv4Subnet.IPv4Subnet != nil {
			ipv4Subnets = append(ipv4Subnets, ipv4Subnet.IPv4Subnet)
		}
	}
	return ipv4Subnets
}

// GetIPv6Subnets returns all IPv6 subnets in the list.
func (a AddressFamilySubnets) GetIPv6Subnets() []*net.IPNet {
	var ipv6Subnets []*net.IPNet
	for _, ipv6Subnet := range a.addressFamilySubnets {
		if ipv6Subnet.IPv6Subnet != nil {
			ipv6Subnets = append(ipv6Subnets, ipv6Subnet.IPv6Subnet)
		}
	}
	return ipv6Subnets
}

// GetControlPlaneSubnet returns the control plane subnet.
func (a AddressFamilySubnets) GetControlPlaneSubnet() AddressFamilySubnet {
	if a.length > 0 {
		return a.addressFamilySubnets[0]
	}
	return AddressFamilySubnet{}
}

// SplitIPv4ComputeSubnets splits the IPv4 compute subnet into the number of subnets specified.
func (a AddressFamilySubnets) SplitIPv4ComputeSubnets(numSubnets int) ([]*net.IPNet, error) {
	if a.ipv4Count > 1 && numSubnets > 0 {
		return cidr.SplitIntoSubnetsIPv4(a.GetIPv4Subnets()[1].String(), numSubnets)
	}
	return []*net.IPNet{}, nil
}

// SplitIPv6ComputeSubnets splits the IPv6 compute subnet into the number of subnets specified.
func (a AddressFamilySubnets) SplitIPv6ComputeSubnets(numSubnets int) ([]*net.IPNet, error) {
	if a.ipv6Count > 1 && numSubnets > 0 {
		return cidr.SplitIntoSubnetsIPv6(a.GetIPv6Subnets()[1].String(), numSubnets)
	}
	return []*net.IPNet{}, nil
}

// SplitComputeSubnet splits the compute subnet into the number of subnets specified.
func (a *AddressFamilySubnets) SplitComputeSubnet(numSubnets int) error {
	if numSubnets <= 0 {
		return nil
	}

	ipv4ComputeSubnets, err := a.SplitIPv4ComputeSubnets(numSubnets)
	if err != nil {
		return err
	}
	ipv4Count := len(ipv4ComputeSubnets)

	var ipv6ComputeSubnets []*net.IPNet
	if a.isDualStack {
		ipv6ComputeSubnets, err = a.SplitIPv6ComputeSubnets(numSubnets)
		if err != nil {
			return err
		}
	}

	var ipv6Count int
	if a.isDualStack {
		ipv6Count = len(ipv6ComputeSubnets)
	}

	addressFamilySubnets := make([]AddressFamilySubnet, 0)
	for i, ipv4ComputeSubnet := range ipv4ComputeSubnets {
		addressFamilySubnet := AddressFamilySubnet{
			IPv4Subnet: ipv4ComputeSubnet,
			SubnetRole: ptr.To(capz.SubnetNode),
		}
		if i < ipv6Count {
			addressFamilySubnet.IPv6Subnet = ipv6ComputeSubnets[i]
		}
		addressFamilySubnets = append(addressFamilySubnets, addressFamilySubnet)
	}

	if a.length > 2 {
		a.addressFamilySubnets = append(a.addressFamilySubnets[:1], append(addressFamilySubnets, a.addressFamilySubnets[2:]...)...)
	} else {
		a.addressFamilySubnets = append(a.addressFamilySubnets[:1], addressFamilySubnets...)
	}

	a.length += len(ipv4ComputeSubnets)
	a.ipv4Count += ipv4Count
	a.ipv6Count += ipv6Count

	return nil
}

// GetComputeSubnets returns the compute subnets.
func (a AddressFamilySubnets) GetComputeSubnets() []AddressFamilySubnet {
	var computeSubnets []AddressFamilySubnet

	for _, addressFamilySubnet := range a.addressFamilySubnets {
		if addressFamilySubnet.SubnetRole != nil && *addressFamilySubnet.SubnetRole == capz.SubnetNode {
			computeSubnets = append(computeSubnets, addressFamilySubnet)
		}
	}

	return computeSubnets
}

// GetIPv4ComputeSubnets returns the IPv4 compute subnets.
func (a AddressFamilySubnets) GetIPv4ComputeSubnets() []*net.IPNet {
	computeSubnets := a.GetComputeSubnets()

	var ipv4ComputeSubnets []*net.IPNet
	for _, ipv4ComputeSubnet := range computeSubnets {
		if ipv4ComputeSubnet.IPv4Subnet != nil {
			ipv4ComputeSubnets = append(ipv4ComputeSubnets, ipv4ComputeSubnet.IPv4Subnet)
		}
	}

	return ipv4ComputeSubnets
}

// GetIPv6ComputeSubnets returns the IPv6 compute subnets.
func (a AddressFamilySubnets) GetIPv6ComputeSubnets() []*net.IPNet {
	computeSubnets := a.GetComputeSubnets()

	var ipv6ComputeSubnets []*net.IPNet
	for _, ipv6ComputeSubnet := range computeSubnets {
		if ipv6ComputeSubnet.IPv6Subnet != nil {
			ipv6ComputeSubnets = append(ipv6ComputeSubnets, ipv6ComputeSubnet.IPv6Subnet)
		}
	}

	return ipv6ComputeSubnets
}

// GetAdditionalSubnets returns any additional subnets.
func (a AddressFamilySubnets) GetAdditionalSubnets() []AddressFamilySubnet {
	var additionalSubnets []AddressFamilySubnet

	for _, addressFamilySubnet := range a.addressFamilySubnets {
		if addressFamilySubnet.SubnetRole == nil {
			additionalSubnets = append(additionalSubnets, addressFamilySubnet)
		}
	}

	return additionalSubnets
}

// GetIPv4AdditionalSubnets returns any additional IPv4 subnets.
func (a AddressFamilySubnets) GetIPv4AdditionalSubnets() []*net.IPNet {
	nonComputeSubnets := a.GetAdditionalSubnets()

	var ipv4Subnets []*net.IPNet
	for _, ipv4Subnet := range nonComputeSubnets {
		if ipv4Subnet.IPv4Subnet != nil {
			ipv4Subnets = append(ipv4Subnets, ipv4Subnet.IPv4Subnet)
		}
	}

	return ipv4Subnets
}

// GetIPv6AdditionalSubnets returns any additional IPv6 subnets.
func (a AddressFamilySubnets) GetIPv6AdditionalSubnets() []*net.IPNet {
	nonComputeSubnets := a.GetAdditionalSubnets()

	var ipv6Subnets []*net.IPNet
	for _, ipv6Subnet := range nonComputeSubnets {
		if ipv6Subnet.IPv6Subnet != nil {
			ipv6Subnets = append(ipv6Subnets, ipv6Subnet.IPv6Subnet)
		}
	}

	return ipv6Subnets
}

// GetIPv4CIDRBlocks returns all IPv4 CIDR blocks.
func (a AddressFamilySubnets) GetIPv4CIDRBlocks() []string {
	var ipv4CIDRBlocks []string

	for _, ipv4Subnet := range a.addressFamilySubnets {
		if ipv4Subnet.IPv4Subnet != nil {
			ipv4CIDRBlocks = append(ipv4CIDRBlocks, (*ipv4Subnet.IPv4Subnet).String())
		}
	}

	return ipv4CIDRBlocks
}

// GetIPv6CIDRBlocks returns all IPv6 CIDR blocks.
func (a AddressFamilySubnets) GetIPv6CIDRBlocks() []string {
	var ipv6CIDRBlocks []string

	for _, ipv6Subnet := range a.addressFamilySubnets {
		if ipv6Subnet.IPv6Subnet != nil {
			ipv6CIDRBlocks = append(ipv6CIDRBlocks, (*ipv6Subnet.IPv6Subnet).String())
		}
	}

	return ipv6CIDRBlocks
}

// GetIPv4ControlPlaneCIDRBlocks returns all IPv4 control plane CIDR blocks.
func (a AddressFamilySubnets) GetIPv4ControlPlaneCIDRBlocks() string {
	ipv4Subnet := a.GetControlPlaneSubnet()
	if ipv4Subnet.IPv4Subnet != nil {
		return (*ipv4Subnet.IPv4Subnet).String()
	}
	return ""
}

// GetIPv6ControlPlaneCIDRBlocks returns all IPv6 control plane CIDR blocks.
func (a AddressFamilySubnets) GetIPv6ControlPlaneCIDRBlocks() string {
	ipv6Subnet := a.GetControlPlaneSubnet()
	if ipv6Subnet.IPv6Subnet != nil {
		return (*ipv6Subnet.IPv6Subnet).String()
	}
	return ""
}

// GetIPv4ComputeCIDRBlocks returns all IPv4 compute CIDR blocks.
func (a AddressFamilySubnets) GetIPv4ComputeCIDRBlocks() []string {
	var ipv4CIDRBlocks []string

	for _, computeSubnet := range a.GetComputeSubnets() {
		if computeSubnet.IPv4Subnet != nil {
			ipv4CIDRBlocks = append(ipv4CIDRBlocks, (*computeSubnet.IPv4Subnet).String())
		}
	}

	return ipv4CIDRBlocks
}

// GetIPv6ComputeCIDRBlocks returns all IPv6 compute CIDR blocks.
func (a AddressFamilySubnets) GetIPv6ComputeCIDRBlocks() []string {
	var ipv6CIDRBlocks []string

	for _, computeSubnet := range a.GetComputeSubnets() {
		if computeSubnet.IPv6Subnet != nil {
			ipv6CIDRBlocks = append(ipv6CIDRBlocks, (*computeSubnet.IPv6Subnet).String())
		}
	}

	return ipv6CIDRBlocks
}

// GetIPv4AdditionalCIDRBlocks returns all additional IPv4 CIDR blocks.
func (a AddressFamilySubnets) GetIPv4AdditionalCIDRBlocks() []string {
	nonComputeSubnets := a.GetAdditionalSubnets()

	var ipv4CIDRBlocks []string
	for _, nonComputeSubnet := range nonComputeSubnets {
		if nonComputeSubnet.IPv4Subnet != nil {
			ipv4CIDRBlocks = append(ipv4CIDRBlocks, (*nonComputeSubnet.IPv4Subnet).String())
		}
	}

	return ipv4CIDRBlocks
}

// GetIPv6AdditionalCIDRBlocks returns all additional IPv6 CIDR blocks.
func (a AddressFamilySubnets) GetIPv6AdditionalCIDRBlocks() []string {
	nonComputeSubnets := a.GetAdditionalSubnets()

	var ipv6CIDRBlocks []string
	for _, nonComputeSubnet := range nonComputeSubnets {
		if nonComputeSubnet.IPv6Subnet != nil {
			ipv6CIDRBlocks = append(ipv6CIDRBlocks, (*nonComputeSubnet.IPv6Subnet).String())
		}
	}

	return ipv6CIDRBlocks
}

func getAddressFamilySubnets(cidrs []ipnet.IPNet) (AddressFamilySubnets, error) {
	var addressFamilySubnets AddressFamilySubnets
	var err error

	// Split cidrs into IPv4 and IPv6 cidrs
	var ipv4Cidrs, ipv6Cidrs []ipnet.IPNet
	for _, cidr := range cidrs {
		switch len(cidr.IP) {
		case net.IPv4len:
			ipv4Cidrs = append(ipv4Cidrs, cidr)
		case net.IPv6len:
			ipv6Cidrs = append(ipv6Cidrs, cidr)
		}
	}

	// Split IPv4 cidrs into IPv4 subnets
	var ipv4Subnets []*net.IPNet
	switch len(ipv4Cidrs) {
	case 1:
		ipv4Subnets, err = cidr.SplitIntoSubnetsIPv4(ipv4Cidrs[0].String(), 2)
		if err != nil {
			return addressFamilySubnets, err
		}
	default:
		for _, ipv4Cidr := range ipv4Cidrs {
			ipv4Subnets = append(ipv4Subnets, &net.IPNet{
				IP:   ipv4Cidr.IP,
				Mask: ipv4Cidr.Mask,
			})
		}
	}

	// Split IPv6 cidrs into IPv6 subnets
	var ipv6Subnets []*net.IPNet
	switch len(ipv6Cidrs) {
	case 1:
		ipv6Subnets, err = cidr.SplitIntoSubnetsIPv6(ipv6Cidrs[0].String(), 2)
		if err != nil {
			return addressFamilySubnets, err
		}
	default:
		for _, ipv6Cidr := range ipv6Cidrs {
			ipv6Subnets = append(ipv6Subnets, &net.IPNet{
				IP:   ipv6Cidr.IP,
				Mask: ipv6Cidr.Mask,
			})
		}
	}

	for i, ipv4Subnet := range ipv4Subnets {
		addressFamilySubnet := AddressFamilySubnet{
			IPv4Subnet: ipv4Subnet,
			SubnetRole: nil,
		}
		switch i {
		case 0:
			addressFamilySubnet.SubnetRole = ptr.To(capz.SubnetControlPlane)
		case 1:
			addressFamilySubnet.SubnetRole = ptr.To(capz.SubnetNode)
		}
		if i < len(ipv6Subnets) {
			addressFamilySubnet.IPv6Subnet = ipv6Subnets[i]
			addressFamilySubnets.isDualStack = true
			addressFamilySubnets.ipv6Count++
		}
		addressFamilySubnets.addressFamilySubnets = append(addressFamilySubnets.addressFamilySubnets, addressFamilySubnet)
		addressFamilySubnets.ipv4Count++
		addressFamilySubnets.length++
	}

	return addressFamilySubnets, err
}

func getVirtualNetworkAddressPrefixes(cidrs []ipnet.IPNet) []string {
	virtualNetworkAddressPrefixes := make([]string, 0)
	for _, cidr := range cidrs {
		virtualNetworkAddressPrefixes = append(virtualNetworkAddressPrefixes, cidr.String())
	}

	// XXX: I don't know if this is necessary, but copying it over for now
	// XXX: 10.1.0.0 is node CIDR, where does 10.2.0.0 come from?
	virtualNetworkAddressPrefixes = append(virtualNetworkAddressPrefixes, "10.1.0.0/16")
	virtualNetworkAddressPrefixes = append(virtualNetworkAddressPrefixes, "10.2.0.0/16")

	return virtualNetworkAddressPrefixes
}

func getIPWithinCIDR(subnets []*net.IPNet, ip string) string {
	if subnets == nil || ip == "" {
		return ""
	}
	// Check if default lbip is within control plane network.
	// If not in control plane network, assign the first non-reserved IP in the CIDR to lbip.
	for _, subnet := range subnets {
		if subnet == nil {
			continue
		}
		if subnet.Contains(net.ParseIP(ip)) {
			return ip
		}
	}
	ipSubnets := make(net.IP, len(subnets[0].IP))
	copy(ipSubnets, subnets[0].IP)
	// Since the first 4 IP of the subnets are usually reserved[1], pick the next one that's available in the CIDR.
	// [1] - https://learn.microsoft.com/en-us/azure/virtual-network/ip-services/private-ip-addresses#allocation-method
	ipSubnets[len(ipSubnets)-1] += 4
	return ipSubnets.String()
}

func getNextAvailableIPForLoadBalancer(ctx context.Context, installConfig *installconfig.InstallConfig, lbip string) (string, error) {
	client, err := installConfig.Azure.Client()
	if err != nil {
		return "", fmt.Errorf("failed to get azure client: %w", err)
	}
	networkResourceGroupName := installConfig.Config.Azure.NetworkResourceGroupName
	virtualNetworkName := installConfig.Config.Azure.VirtualNetwork
	machineCidr := installConfig.Config.MachineNetwork
	var cpSubnet string
	for _, subnetSpec := range installConfig.Config.Azure.Subnets {
		if subnetSpec.Role == capz.SubnetControlPlane {
			cpSubnet = subnetSpec.Name
		}
	}
	if cpSubnet != "" {
		controlPlane, err := client.GetControlPlaneSubnet(ctx, networkResourceGroupName, virtualNetworkName, cpSubnet)
		if err != nil {
			return "", fmt.Errorf("failed to get control plane subnet: %w", err)
		}
		if controlPlane.AddressPrefix == nil && controlPlane.AddressPrefixes == nil {
			return "", fmt.Errorf("failed to get control plane subnet addresses: %w", err)
		}
		prefixes := []*ipnet.IPNet{}
		if controlPlane.AddressPrefixes != nil {
			for _, sub := range *controlPlane.AddressPrefixes {
				ipnet, err := ipnet.ParseCIDR(sub)
				if err != nil {
					return "", fmt.Errorf("failed to get translate azure control plane subnet addresses: %w", err)
				}
				prefixes = append(prefixes, ipnet)
			}
		}

		if controlPlane.AddressPrefix != nil {
			ipnet, err := ipnet.ParseCIDR(*controlPlane.AddressPrefix)
			if err != nil {
				return "", fmt.Errorf("failed to get translate azure control plane subnet address prefix: %w", err)
			}
			prefixes = append(prefixes, ipnet)
		}
		cidrRange := []types.MachineNetworkEntry{}
		for _, prefix := range prefixes {
			if prefix != nil {
				cidrRange = append(cidrRange, types.MachineNetworkEntry{CIDR: *prefix})
			}
		}
		machineCidr = cidrRange
	}
	// AzureStack does not support the call to CheckIPAddressAvailability.
	if installConfig.Azure.CloudName == azure.StackCloud {
		cidr := machineCidr[0]
		if cidr.CIDR.Contains(net.IP(lbip)) {
			return lbip, nil
		}
		ipSubnets := cidr.CIDR.IP
		ipSubnets[len(ipSubnets)-1] += 4
		return ipSubnets.String(), nil
	}
	availableIP, err := client.CheckIPAddressAvailability(ctx, networkResourceGroupName, virtualNetworkName, lbip)
	if err != nil {
		return "", fmt.Errorf("failed to get azure ip availability: %w", err)
	}
	if availableIP == nil {
		return "", errors.New("failed to get available IP in given machine network: this error may be caused by lack of necessary permissions")
	}
	ipAvail := *availableIP
	if ipAvail.Available != nil && *ipAvail.Available {
		for _, cidrRange := range machineCidr {
			if cidrRange.CIDR.Contains(net.ParseIP(lbip)) {
				return lbip, nil
			}
		}
	}
	if ipAvail.AvailableIPAddresses == nil || len(*ipAvail.AvailableIPAddresses) == 0 {
		return "", fmt.Errorf("failed to get an available IP in given virtual network for LB: this error may be caused by lack of necessary permissions")
	}
	for _, ip := range *ipAvail.AvailableIPAddresses {
		for _, cidrRange := range machineCidr {
			if cidrRange.CIDR.Contains(net.ParseIP(lbip)) {
				return ip, nil
			}
		}
	}
	return "", fmt.Errorf("failed to get an IP that's available and in the given machine network: this error may be caused by lack of necessary permissions")
}

func deepCopy(src, dst interface{}) error {
	bytes, err := json.Marshal(src)
	if err != nil {
		return err
	}
	return json.Unmarshal(bytes, dst)
}
