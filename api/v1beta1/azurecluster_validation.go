/*
Copyright 2021 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1beta1

import (
	"fmt"
	"net"
	"reflect"
	"regexp"

	valid "github.com/asaskevich/govalidator"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/pointer"
)

const (
	// can't use: \/"'[]:|<>+=;,.?*@&, Can't start with underscore. Can't end with period or hyphen.
	// not using . in the name to avoid issues when the name is part of DNS name.
	clusterNameRegex = `^[a-z0-9][a-z0-9-]{0,42}[a-z0-9]$`
	// max length of 44 to allow for cluster name to be used as a prefix for VMs and other resources that
	// have limitations as outlined here https://docs.microsoft.com/en-us/azure/azure-resource-manager/management/resource-name-rules.
	clusterNameMaxLength = 44
	// obtained from https://docs.microsoft.com/en-us/rest/api/resources/resourcegroups/createorupdate#uri-parameters.
	resourceGroupRegex = `^[-\w\._\(\)]+$`
	// described in https://docs.microsoft.com/en-us/azure/azure-resource-manager/management/resource-name-rules.
	subnetRegex       = `^[-\w\._]+$`
	loadBalancerRegex = `^[-\w\._]+$`
	// MaxLoadBalancerOutboundIPs is the maximum number of outbound IPs in a Standard LoadBalancer frontend configuration.
	MaxLoadBalancerOutboundIPs = 16
	// MinLBIdleTimeoutInMinutes is the minimum number of minutes for the LB idle timeout.
	MinLBIdleTimeoutInMinutes = 4
	// MaxLBIdleTimeoutInMinutes is the maximum number of minutes for the LB idle timeout.
	MaxLBIdleTimeoutInMinutes = 30
	// Network security rules should be a number between 100 and 4096.
	// https://docs.microsoft.com/en-us/azure/virtual-network/network-security-groups-overview#security-rules
	minRulePriority = 100
	maxRulePriority = 4096
)

// validateCluster validates a cluster.
func (c *AzureCluster) validateCluster(old *AzureCluster) error {
	var allErrs field.ErrorList
	allErrs = append(allErrs, c.validateClusterName()...)
	allErrs = append(allErrs, c.validateClusterSpec(old)...)
	if len(allErrs) == 0 {
		return nil
	}

	return apierrors.NewInvalid(
		schema.GroupKind{Group: "infrastructure.cluster.x-k8s.io", Kind: "AzureCluster"},
		c.Name, allErrs)
}

// validateClusterSpec validates a ClusterSpec.
func (c *AzureCluster) validateClusterSpec(old *AzureCluster) field.ErrorList {
	var allErrs field.ErrorList
	var oldNetworkSpec NetworkSpec
	if old != nil {
		oldNetworkSpec = old.Spec.NetworkSpec
	}
	allErrs = append(allErrs, validateNetworkSpec(c.Spec.NetworkSpec, oldNetworkSpec, field.NewPath("spec").Child("networkSpec"))...)

	var oldCloudProviderConfigOverrides *CloudProviderConfigOverrides
	if old != nil {
		oldCloudProviderConfigOverrides = old.Spec.CloudProviderConfigOverrides
	}
	allErrs = append(allErrs, validateCloudProviderConfigOverrides(c.Spec.CloudProviderConfigOverrides, oldCloudProviderConfigOverrides,
		field.NewPath("spec").Child("cloudProviderConfigOverrides"))...)

	return allErrs
}

// validateClusterName validates ClusterName.
func (c *AzureCluster) validateClusterName() field.ErrorList {
	var allErrs field.ErrorList
	if len(c.Name) > clusterNameMaxLength {
		allErrs = append(allErrs, field.Invalid(field.NewPath("metadata").Child("Name"), c.Name,
			fmt.Sprintf("Cluster Name longer than allowed length of %d characters", clusterNameMaxLength)))
	}
	if success, _ := regexp.MatchString(clusterNameRegex, c.Name); !success {
		allErrs = append(allErrs, field.Invalid(field.NewPath("metadata").Child("Name"), c.Name,
			fmt.Sprintf("Cluster Name doesn't match regex %s, can contain only lowercase alphanumeric characters and '-', must start/end with an alphanumeric character",
				clusterNameRegex)))
	}
	if len(allErrs) == 0 {
		return nil
	}
	return allErrs
}

// validateNetworkSpec validates a NetworkSpec.
func validateNetworkSpec(networkSpec NetworkSpec, old NetworkSpec, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	// If the user specifies a resourceGroup for vnet, it means
	// that she intends to use a pre-existing vnet. In this case,
	// we need to verify the information she provides
	if networkSpec.Vnet.ResourceGroup != "" {
		if err := validateResourceGroup(networkSpec.Vnet.ResourceGroup,
			fldPath.Child("vnet").Child("resourceGroup")); err != nil {
			allErrs = append(allErrs, err)
		}

		allErrs = append(allErrs, validateVnetCIDR(networkSpec.Vnet.CIDRBlocks, fldPath.Child("cidrBlocks"))...)

		allErrs = append(allErrs, validateSubnets(networkSpec.Subnets, networkSpec.Vnet, fldPath.Child("subnets"))...)

		allErrs = append(allErrs, validateVnetPeerings(networkSpec.Vnet.Peerings, fldPath.Child("peerings"))...)
	}

	var cidrBlocks []string
	controlPlaneSubnet, err := networkSpec.GetControlPlaneSubnet()
	if err != nil {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("subnets"), networkSpec.Subnets, "ControlPlaneSubnet invalid"))
	}

	cidrBlocks = controlPlaneSubnet.CIDRBlocks

	allErrs = append(allErrs, validateAPIServerLB(networkSpec.APIServerLB, old.APIServerLB, cidrBlocks, fldPath.Child("apiServerLB"))...)

	var oneSubnetWithoutNatGateway bool
	for _, subnet := range networkSpec.Subnets {
		if subnet.Role == SubnetNode && !subnet.IsNatGatewayEnabled() {
			oneSubnetWithoutNatGateway = true
			break
		}
	}
	if oneSubnetWithoutNatGateway {
		allErrs = append(allErrs, validateNodeOutboundLB(networkSpec.NodeOutboundLB, old.NodeOutboundLB, networkSpec.APIServerLB, fldPath.Child("nodeOutboundLB"))...)
	}

	allErrs = append(allErrs, validateControlPlaneOutboundLB(networkSpec.ControlPlaneOutboundLB, networkSpec.APIServerLB, fldPath.Child("controlPlaneOutboundLB"))...)

	allErrs = append(allErrs, validatePrivateDNSZoneName(networkSpec, fldPath)...)

	if len(allErrs) == 0 {
		return nil
	}
	return allErrs
}

// validateResourceGroup validates a ResourceGroup.
func validateResourceGroup(resourceGroup string, fldPath *field.Path) *field.Error {
	if success, _ := regexp.MatchString(resourceGroupRegex, resourceGroup); !success {
		return field.Invalid(fldPath, resourceGroup,
			fmt.Sprintf("resourceGroup doesn't match regex %s", resourceGroupRegex))
	}
	return nil
}

// validateSubnets validates a list of Subnets.
func validateSubnets(subnets Subnets, vnet VnetSpec, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	subnetNames := make(map[string]bool, len(subnets))
	requiredSubnetRoles := map[string]bool{
		"control-plane": false,
		"node":          false,
	}

	for i, subnet := range subnets {
		if err := validateSubnetName(subnet.Name, fldPath.Index(i).Child("name")); err != nil {
			allErrs = append(allErrs, err)
		}
		if _, ok := subnetNames[subnet.Name]; ok {
			allErrs = append(allErrs, field.Duplicate(fldPath, subnet.Name))
		}
		subnetNames[subnet.Name] = true
		for role := range requiredSubnetRoles {
			if role == string(subnet.Role) {
				requiredSubnetRoles[role] = true
			}
		}
		for _, rule := range subnet.SecurityGroup.SecurityRules {
			if err := validateSecurityRule(
				rule,
				fldPath.Index(i).Child("securityGroup").Child("securityRules").Index(i),
			); err != nil {
				allErrs = append(allErrs, err)
			}
		}
		allErrs = append(allErrs, validateSubnetCIDR(subnet.CIDRBlocks, vnet.CIDRBlocks, fldPath.Index(i).Child("cidrBlocks"))...)
	}
	for k, v := range requiredSubnetRoles {
		if !v {
			allErrs = append(allErrs, field.Required(fldPath,
				fmt.Sprintf("required role %s not included in provided subnets", k)))
		}
	}
	return allErrs
}

// validateSubnetName validates the Name of a Subnet.
func validateSubnetName(name string, fldPath *field.Path) *field.Error {
	if success, _ := regexp.Match(subnetRegex, []byte(name)); !success {
		return field.Invalid(fldPath, name,
			fmt.Sprintf("name of subnet doesn't match regex %s", subnetRegex))
	}
	return nil
}

// validateSubnetCIDR validates the CIDR blocks of a Subnet.
func validateSubnetCIDR(subnetCidrBlocks []string, vnetCidrBlocks []string, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	var vnetNws []*net.IPNet

	for _, vnetCidr := range vnetCidrBlocks {
		if _, vnetNw, err := net.ParseCIDR(vnetCidr); err == nil {
			vnetNws = append(vnetNws, vnetNw)
		}
	}

	for _, subnetCidr := range subnetCidrBlocks {
		subnetCidrIP, _, err := net.ParseCIDR(subnetCidr)
		if err != nil {
			allErrs = append(allErrs, field.Invalid(fldPath, subnetCidr, "invalid CIDR format"))
		}

		var found bool
		for _, vnetNw := range vnetNws {
			if vnetNw.Contains(subnetCidrIP) {
				found = true
				break
			}
		}

		if !found {
			allErrs = append(allErrs, field.Invalid(fldPath, subnetCidr, fmt.Sprintf("subnet CIDR not in vnet address space: %s", vnetCidrBlocks)))
		}
	}

	return allErrs
}

// validateVnetCIDR validates the CIDR blocks of a Vnet.
func validateVnetCIDR(vnetCIDRBlocks []string, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	for _, vnetCidr := range vnetCIDRBlocks {
		if _, _, err := net.ParseCIDR(vnetCidr); err != nil {
			allErrs = append(allErrs, field.Invalid(fldPath, vnetCidr, "invalid CIDR format"))
		}
	}
	return allErrs
}

// validateVnetPeerings validates a list of virtual network peerings.
func validateVnetPeerings(peerings VnetPeerings, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	vnetIdentifiers := make(map[string]bool, len(peerings))

	for _, peering := range peerings {
		vnetIdentifier := peering.ResourceGroup + "/" + peering.RemoteVnetName
		if _, ok := vnetIdentifiers[vnetIdentifier]; ok {
			allErrs = append(allErrs, field.Duplicate(fldPath, vnetIdentifier))
		}
		vnetIdentifiers[vnetIdentifier] = true
	}
	return allErrs
}

// validateLoadBalancerName validates the Name of a Load Balancer.
func validateLoadBalancerName(name string, fldPath *field.Path) *field.Error {
	if success, _ := regexp.Match(loadBalancerRegex, []byte(name)); !success {
		return field.Invalid(fldPath, name,
			fmt.Sprintf("name of load balancer doesn't match regex %s", loadBalancerRegex))
	}
	return nil
}

// validateInternalLBIPAddress validates a InternalLBIPAddress.
func validateInternalLBIPAddress(address string, cidrs []string, fldPath *field.Path) *field.Error {
	ip := net.ParseIP(address)
	if ip == nil {
		return field.Invalid(fldPath, address,
			"Internal LB IP address isn't a valid IPv4 or IPv6 address")
	}
	for _, cidr := range cidrs {
		_, subnet, _ := net.ParseCIDR(cidr)
		if subnet.Contains(ip) {
			return nil
		}
	}
	return field.Invalid(fldPath, address,
		fmt.Sprintf("Internal LB IP address needs to be in control plane subnet range (%s)", cidrs))
}

// validateSecurityRule validates a SecurityRule.
func validateSecurityRule(rule SecurityRule, fldPath *field.Path) *field.Error {
	if rule.Priority < minRulePriority || rule.Priority > maxRulePriority {
		return field.Invalid(fldPath, rule.Priority, fmt.Sprintf("security rule priorities should be between %d and %d", minRulePriority, maxRulePriority))
	}

	return nil
}

func validateAPIServerLB(lb LoadBalancerSpec, old LoadBalancerSpec, cidrs []string, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	// SKU should be Standard and is immutable.
	if lb.SKU != SKUStandard {
		allErrs = append(allErrs, field.NotSupported(fldPath.Child("sku"), lb.SKU, []string{string(SKUStandard)}))
	}
	if old.SKU != "" && old.SKU != lb.SKU {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("sku"), "API Server load balancer SKU should not be modified after AzureCluster creation."))
	}

	// Type should be Public or Internal.
	if lb.Type != Internal && lb.Type != Public {
		allErrs = append(allErrs, field.NotSupported(fldPath.Child("type"), lb.Type,
			[]string{string(Public), string(Internal)}))
	}
	if old.Type != "" && old.Type != lb.Type {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("type"), "API Server load balancer type should not be modified after AzureCluster creation."))
	}

	// Name should be valid.
	if err := validateLoadBalancerName(lb.Name, fldPath.Child("name")); err != nil {
		allErrs = append(allErrs, err)
	}
	if old.Name != "" && old.Name != lb.Name {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("name"), "API Server load balancer name should not be modified after AzureCluster creation."))
	}

	if old.IdleTimeoutInMinutes != nil && !pointer.Int32Equal(old.IdleTimeoutInMinutes, lb.IdleTimeoutInMinutes) {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("idleTimeoutInMinutes"), "API Server load balancer idle timeout cannot be modified after AzureCluster creation."))
	}

	// There should only be one IP config.
	if len(lb.FrontendIPs) != 1 || pointer.Int32Deref(lb.FrontendIPsCount, 1) != 1 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("frontendIPConfigs"), lb.FrontendIPs,
			"API Server Load balancer should have 1 Frontend IP"))
	} else {
		// if Internal, IP config should not have a public IP.
		if lb.Type == Internal {
			if lb.FrontendIPs[0].PublicIP != nil {
				allErrs = append(allErrs, field.Forbidden(fldPath.Child("frontendIPConfigs").Index(0).Child("publicIP"),
					"Internal Load Balancers cannot have a Public IP"))
			}
			if lb.FrontendIPs[0].PrivateIPAddress != "" {
				if err := validateInternalLBIPAddress(lb.FrontendIPs[0].PrivateIPAddress, cidrs,
					fldPath.Child("frontendIPConfigs").Index(0).Child("privateIP")); err != nil {
					allErrs = append(allErrs, err)
				}
				if len(old.FrontendIPs) != 0 && old.FrontendIPs[0].PrivateIPAddress != lb.FrontendIPs[0].PrivateIPAddress {
					allErrs = append(allErrs, field.Forbidden(fldPath.Child("name"), "API Server load balancer private IP should not be modified after AzureCluster creation."))
				}
			}
		}

		// if Public, IP config should not have a private IP.
		if lb.Type == Public {
			if lb.FrontendIPs[0].PrivateIPAddress != "" {
				allErrs = append(allErrs, field.Forbidden(fldPath.Child("frontendIPConfigs").Index(0).Child("privateIP"),
					"Public Load Balancers cannot have a Private IP"))
			}
		}

		if lb.IdleTimeoutInMinutes != nil && (*lb.IdleTimeoutInMinutes < MinLBIdleTimeoutInMinutes || *lb.IdleTimeoutInMinutes > MaxLBIdleTimeoutInMinutes) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("idleTimeoutInMinutes"), *lb.IdleTimeoutInMinutes,
				fmt.Sprintf("Node outbound idle timeout should be between %d and %d minutes", MinLBIdleTimeoutInMinutes, MaxLoadBalancerOutboundIPs)))
		}
	}

	return allErrs
}

func validateNodeOutboundLB(lb *LoadBalancerSpec, old *LoadBalancerSpec, apiserverLB LoadBalancerSpec, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	// LB can be nil when disabled for private clusters.
	if lb == nil && apiserverLB.Type == Internal {
		return allErrs
	}

	if lb == nil {
		allErrs = append(allErrs, field.Required(fldPath, "Node outbound load balancer cannot be nil for public clusters."))
		return allErrs
	}

	if old != nil && old.ID != lb.ID {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("id"), "Node outbound load balancer ID should not be modified after AzureCluster creation."))
	}

	if old != nil && old.Name != lb.Name {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("name"), "Node outbound load balancer Name should not be modified after AzureCluster creation."))
	}

	if old != nil && old.SKU != lb.SKU {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("sku"), "Node outbound load balancer SKU should not be modified after AzureCluster creation."))
	}

	if old != nil && old.Type != lb.Type {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("type"), "Node outbound load balancer Type cannot be modified after AzureCluster creation."))
	}

	if old != nil && !pointer.Int32Equal(old.IdleTimeoutInMinutes, lb.IdleTimeoutInMinutes) {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("idleTimeoutInMinutes"), "Node outbound load balancer idle timeout cannot be modified after AzureCluster creation."))
	}

	if old != nil && old.FrontendIPsCount == lb.FrontendIPsCount {
		if len(old.FrontendIPs) != len(lb.FrontendIPs) {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("frontendIPs"), "Node outbound load balancer FrontendIPs cannot be modified after AzureCluster creation."))
		}

		if len(old.FrontendIPs) == len(lb.FrontendIPs) {
			for i, frontEndIP := range lb.FrontendIPs {
				oldFrontendIP := old.FrontendIPs[i]
				if oldFrontendIP.Name != frontEndIP.Name || *oldFrontendIP.PublicIP != *frontEndIP.PublicIP {
					allErrs = append(allErrs, field.Forbidden(fldPath.Child("frontendIPs").Index(i),
						"Node outbound load balancer FrontendIPs cannot be modified after AzureCluster creation."))
				}
			}
		}
	}

	if lb.FrontendIPsCount != nil && *lb.FrontendIPsCount > MaxLoadBalancerOutboundIPs {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("frontendIPsCount"), *lb.FrontendIPsCount,
			fmt.Sprintf("Max front end ips allowed is %d", MaxLoadBalancerOutboundIPs)))
	}

	if lb.IdleTimeoutInMinutes != nil && (*lb.IdleTimeoutInMinutes < MinLBIdleTimeoutInMinutes || *lb.IdleTimeoutInMinutes > MaxLBIdleTimeoutInMinutes) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("idleTimeoutInMinutes"), *lb.IdleTimeoutInMinutes,
			fmt.Sprintf("Node outbound idle timeout should be between %d and %d minutes", MinLBIdleTimeoutInMinutes, MaxLoadBalancerOutboundIPs)))
	}

	return allErrs
}

func validateControlPlaneOutboundLB(lb *LoadBalancerSpec, apiserverLB LoadBalancerSpec, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	switch apiserverLB.Type {
	case Public:
		if lb != nil {
			allErrs = append(allErrs, field.Forbidden(fldPath, "Control plane outbound load balancer cannot be set for public clusters."))
		}
	case Internal:
		// Control plane outbound lb can be nil when it's disabled for private clusters.
		if lb == nil {
			return nil
		}

		if lb.FrontendIPsCount != nil && *lb.FrontendIPsCount > MaxLoadBalancerOutboundIPs {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("frontendIPsCount"), *lb.FrontendIPsCount,
				fmt.Sprintf("Max front end ips allowed is %d", MaxLoadBalancerOutboundIPs)))
		}

		if lb.IdleTimeoutInMinutes != nil && (*lb.IdleTimeoutInMinutes < MinLBIdleTimeoutInMinutes || *lb.IdleTimeoutInMinutes > MaxLBIdleTimeoutInMinutes) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("idleTimeoutInMinutes"), *lb.IdleTimeoutInMinutes,
				fmt.Sprintf("Control plane outbound idle timeout should be between %d and %d minutes", MinLBIdleTimeoutInMinutes, MaxLoadBalancerOutboundIPs)))
		}
	}

	return allErrs
}

// validatePrivateDNSZoneName validate the PrivateDNSZoneName.
func validatePrivateDNSZoneName(networkSpec NetworkSpec, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	if len(networkSpec.PrivateDNSZoneName) > 0 {
		if networkSpec.APIServerLB.Type != Internal {
			allErrs = append(allErrs, field.Invalid(fldPath, networkSpec.APIServerLB.Type,
				"PrivateDNSZoneName is available only if APIServerLB.Type is Internal"))
		}
		if !valid.IsDNSName(networkSpec.PrivateDNSZoneName) {
			allErrs = append(allErrs, field.Invalid(fldPath, networkSpec.PrivateDNSZoneName,
				"PrivateDNSZoneName can only contain alphanumeric characters, underscores and dashes, must end with an alphanumeric character",
			))
		}
	}
	if len(allErrs) == 0 {
		return nil
	}

	return allErrs
}

// validateCloudProviderConfigOverrides validates CloudProviderConfigOverrides.
func validateCloudProviderConfigOverrides(old, new *CloudProviderConfigOverrides, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if !reflect.DeepEqual(old, new) {
		allErrs = append(allErrs, field.Invalid(fldPath, new, "cannot change cloudProviderConfigOverrides cluster creation"))
	}
	return allErrs
}
