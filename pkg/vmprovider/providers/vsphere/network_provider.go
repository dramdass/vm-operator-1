// Copyright (c) 2018-2020 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package vsphere

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	ctrlruntime "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/pkg/errors"
	"github.com/vmware-tanzu/vm-operator-api/api/v1alpha1"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	vimtypes "github.com/vmware/govmomi/vim25/types"

	ncpv1alpha1 "github.com/vmware-tanzu/vm-operator/external/ncp/api/v1alpha1"

	netopv1alpha1 "github.com/vmware-tanzu/vm-operator/external/net-operator/api/v1alpha1"
)

const (
	NsxtNetworkType = "nsx-t"
	VdsNetworkType  = "vsphere-distributed"

	defaultEthernetCardType = "vmxnet3"
	retryInterval           = 100 * time.Millisecond
	retryTimeout            = 15 * time.Second
)

// NetworkProvider sets up network for different type of network
type NetworkProvider interface {
	// CreateVnic creates the VirtualEthernetCard for the network interface
	CreateVnic(ctx context.Context, vm *v1alpha1.VirtualMachine, vif *v1alpha1.VirtualMachineNetworkInterface) (vimtypes.BaseVirtualDevice, error)

	// GetInterfaceGuestCustomization returns the CustomizationAdapterMapping for the network interface
	GetInterfaceGuestCustomization(vm *v1alpha1.VirtualMachine, vif *v1alpha1.VirtualMachineNetworkInterface) (*vimtypes.CustomizationAdapterMapping, error)
}

// GetNetworkProvider returns the network provider based on the vmNif providerRef and network type
func GetNetworkProvider(vif *v1alpha1.VirtualMachineNetworkInterface, k8sClient ctrlruntime.Client,
	vimClient *vim25.Client, finder *find.Finder, cluster *object.ClusterComputeResource, scheme *runtime.Scheme) (NetworkProvider, error) {

	// Until we determine a way to handle NSX-t for Netop and NCP
	// Workaround: if a valid NetOp Type is provided use NetOpNetworkProvider
	if vif.ProviderRef != nil {
		netOpNetworkIf := &netopv1alpha1.NetworkInterface{}

		gvk, err := apiutil.GVKForObject(netOpNetworkIf, scheme)
		if err != nil {
			return nil, errors.Wrapf(err, "cannot get GroupVersionKind for NetworkInterface object")
		}

		if gvk.Group != vif.ProviderRef.APIGroup {
			err := fmt.Errorf("unsupported APIGroup for ProviderRef: %+v Supported: %+v", vif.ProviderRef.APIGroup, gvk.Group)
			return nil, err
		}

		return NetOpNetworkProvider(k8sClient, vimClient, finder, cluster, scheme), nil
	}

	return NetworkProviderByType(vif.NetworkType, k8sClient, vimClient, finder, cluster, scheme)
}

// NetworkProviderByType returns the network provider based on network type
func NetworkProviderByType(networkType string, k8sClient ctrlruntime.Client, vimClient *vim25.Client,
	finder *find.Finder, cluster *object.ClusterComputeResource, scheme *runtime.Scheme) (NetworkProvider, error) {

	// TODO VMSVC-295: Need way to determine to use NetOP for NSXT
	switch networkType {
	case NsxtNetworkType:
		return NsxtNetworkProvider(k8sClient, finder, cluster), nil
	case VdsNetworkType:
		return NetOpNetworkProvider(k8sClient, vimClient, finder, cluster, scheme), nil
	case "":
		return DefaultNetworkProvider(finder), nil
	}
	return nil, fmt.Errorf("failed to create network provider for network type '%s'", networkType)
}

func getEthCardType(vif *v1alpha1.VirtualMachineNetworkInterface) string {
	if vif.EthernetCardType != "" {
		return vif.EthernetCardType
	}
	return defaultEthernetCardType
}

// createVnicOnNamedNetwork creates vnic on named network
func createVnicOnNamedNetwork(ctx context.Context, networkName, ethCardType string, finder *find.Finder) (vimtypes.BaseVirtualDevice, error) {
	networkRef, err := finder.Network(ctx, networkName)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to find network %q", networkName)
	}
	return createCommonVnic(ctx, networkRef, ethCardType)
}

// createCommonVnic creates an ethernet card on a given the network reference
func createCommonVnic(ctx context.Context, network object.NetworkReference, ethCardType string) (vimtypes.BaseVirtualDevice, error) {
	backing, err := network.EthernetCardBackingInfo(ctx)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to get ethernet card backing info for network %v", network.Reference())
	}

	dev, err := object.EthernetCardTypes().CreateEthernetCard(ethCardType, backing)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to create ethernet card %q for network %v", ethCardType, network.Reference())
	}

	return dev, nil
}

// DefaultNetworkProvider returns a defaultNetworkProvider instance
func DefaultNetworkProvider(finder *find.Finder) *defaultNetworkProvider {
	return &defaultNetworkProvider{
		finder: finder,
	}
}

type defaultNetworkProvider struct {
	finder *find.Finder
}

func (np *defaultNetworkProvider) CreateVnic(ctx context.Context, vm *v1alpha1.VirtualMachine, vif *v1alpha1.VirtualMachineNetworkInterface) (vimtypes.BaseVirtualDevice, error) {
	return createVnicOnNamedNetwork(ctx, vif.NetworkName, getEthCardType(vif), np.finder)
}

func (np *defaultNetworkProvider) GetInterfaceGuestCustomization(vm *v1alpha1.VirtualMachine, vif *v1alpha1.VirtualMachineNetworkInterface) (*vimtypes.CustomizationAdapterMapping, error) {
	return &vimtypes.CustomizationAdapterMapping{
		Adapter: vimtypes.CustomizationIPSettings{
			Ip: &vimtypes.CustomizationDhcpIpGenerator{},
		},
	}, nil
}

// +kubebuilder:rbac:groups=netoperator.vmware.com,resources=networkinterfaces;vmxnet3networkinterfaces,verbs=get;list;watch;create;update;patch;delete

// NetOpNetworkProvider returns a netOpNetworkProvider instance
func NetOpNetworkProvider(k8sClient ctrlruntime.Client, vimClient *vim25.Client, finder *find.Finder, cluster *object.ClusterComputeResource, scheme *runtime.Scheme) *netOpNetworkProvider {
	return &netOpNetworkProvider{
		k8sClient: k8sClient,
		scheme:    scheme,
		vimClient: vimClient,
		finder:    finder,
		cluster:   cluster,
	}
}

type netOpNetworkProvider struct {
	k8sClient ctrlruntime.Client
	scheme    *runtime.Scheme
	vimClient *vim25.Client
	finder    *find.Finder
	cluster   *object.ClusterComputeResource
}

func netOpEthCardType(_ string) netopv1alpha1.NetworkInterfaceType {
	// Only type available.
	return netopv1alpha1.NetworkInterfaceTypeVMXNet3
}

func (np *netOpNetworkProvider) generateNetIfName(networkName, vmName string) string {
	// BMV: This is similar to what NSX does but isn't really right: we can only have one
	// interface per network. Although if we had multiple interfaces per network, we really
	// don't have a way to identify each NIC so true reconciliation is broken. We might
	// later be able to use the ExternalId to correctly associate interfaces.
	return fmt.Sprintf("%s-%s", networkName, vmName)
}

// createNetworkInterface creates a netop NetworkInterface for a given VM network interface
func (np *netOpNetworkProvider) createNetworkInterface(ctx context.Context, vm *v1alpha1.VirtualMachine,
	vmIf *v1alpha1.VirtualMachineNetworkInterface) (*netopv1alpha1.NetworkInterface, error) {
	var netIf *netopv1alpha1.NetworkInterface
	var err error

	// Create NetIf when ProviderRef is unset
	if vmIf.ProviderRef == nil {
		networkName := vmIf.NetworkName
		netIf = &netopv1alpha1.NetworkInterface{
			ObjectMeta: metav1.ObjectMeta{
				Name:      np.generateNetIfName(networkName, vm.Name),
				Namespace: vm.Namespace,
				OwnerReferences: []metav1.OwnerReference{
					{
						Name:       vm.Name,
						APIVersion: vm.APIVersion,
						Kind:       vm.Kind,
						UID:        vm.UID,
					},
				},
			},
			Spec: netopv1alpha1.NetworkInterfaceSpec{
				NetworkName: networkName,
				Type:        netOpEthCardType(vmIf.EthernetCardType),
			},
		}

		err := np.k8sClient.Create(ctx, netIf)
		if err != nil {
			if !k8serrors.IsAlreadyExists(err) {
				return nil, err
			}

			instance := &netopv1alpha1.NetworkInterface{}
			err := np.k8sClient.Get(ctx, types.NamespacedName{Name: netIf.Name, Namespace: netIf.Namespace}, instance)
			if err != nil {
				return nil, err
			}

			instance.SetOwnerReferences([]metav1.OwnerReference{
				{
					Name:       vm.Name,
					APIVersion: vm.APIVersion,
					Kind:       vm.Kind,
					UID:        vm.UID,
				},
			})

			if err := np.k8sClient.Update(ctx, instance); err != nil {
				return nil, err
			}
		}
	}

	netIf, err = np.waitForNetworkInterfaceStatus(ctx, vm, vmIf)
	if err != nil {
		return nil, err
	}
	return netIf, nil
}

func (np *netOpNetworkProvider) getNetOpNetIf(ctx context.Context, providerRef *v1alpha1.NetworkInterfaceProviderReference, vmNamespace string) (*netopv1alpha1.NetworkInterface, error) {
	netOpNetworkIf := &netopv1alpha1.NetworkInterface{}

	gvk, err := apiutil.GVKForObject(netOpNetworkIf, np.scheme)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot get GroupVersionKind for NetworkInterface object")
	}

	if gvk.Group != providerRef.APIGroup || gvk.Version != providerRef.APIVersion || gvk.Kind != providerRef.Kind {
		err := fmt.Errorf("unsupported NetworkInterface ProviderRef: %+v Supported: %+v", providerRef, gvk)
		return nil, err
	}

	key := types.NamespacedName{Name: providerRef.Name, Namespace: vmNamespace}
	if err := np.k8sClient.Get(ctx, key, netOpNetworkIf); err != nil {
		return nil, errors.Wrapf(err, "cannot get NetworkInterface %s from ProviderRef", key)
	}

	return netOpNetworkIf, nil
}

func (np *netOpNetworkProvider) networkForPortGroupId(portGroupId string) (object.NetworkReference, error) {
	pgObjRef := vimtypes.ManagedObjectReference{
		Type:  "DistributedVirtualPortgroup",
		Value: portGroupId,
	}

	return object.NewDistributedVirtualPortgroup(np.vimClient, pgObjRef), nil
}

func (np *netOpNetworkProvider) getNetworkRef(ctx context.Context, networkType, networkID string) (object.NetworkReference, error) {
	switch networkType {
	case VdsNetworkType:
		return np.networkForPortGroupId(networkID)
	case NsxtNetworkType:
		return searchNsxtNetworkReference(ctx, np.finder, np.cluster, networkID)
	default:
		return nil, fmt.Errorf("unsupported NetOP network type %s", networkType)
	}
}

func (np *netOpNetworkProvider) CreateVnic(ctx context.Context, vm *v1alpha1.VirtualMachine,
	vif *v1alpha1.VirtualMachineNetworkInterface) (vimtypes.BaseVirtualDevice, error) {

	netIf, err := np.createNetworkInterface(ctx, vm, vif)
	if err != nil {
		return nil, err
	}

	if netIf.Status.NetworkID == "" {
		err = fmt.Errorf("failed to get NetworkID for netIf '%+v'", netIf)
		log.Error(err, "Failed to get NetworkID for netIf")
		return nil, err
	}

	networkRef, err := np.getNetworkRef(ctx, vif.NetworkType, netIf.Status.NetworkID)
	if err != nil {
		return nil, err
	}

	dev, err := createCommonVnic(ctx, networkRef, getEthCardType(vif))
	if err != nil {
		return nil, err
	}

	nic := dev.(vimtypes.BaseVirtualEthernetCard).GetVirtualEthernetCard()
	nic.ExternalId = netIf.Status.ExternalID
	if mac := netIf.Status.MacAddress; mac != "" {
		nic.MacAddress = mac
		nic.AddressType = string(vimtypes.VirtualEthernetCardMacTypeManual)
	} else {
		nic.AddressType = string(vimtypes.VirtualEthernetCardMacTypeGenerated)
	}
	// TODO
	// nic.WakeOnLanEnabled =
	// nic.UptCompatibilityEnabled =

	return dev, nil
}

func (np *netOpNetworkProvider) GetInterfaceGuestCustomization(vm *v1alpha1.VirtualMachine,
	vmIf *v1alpha1.VirtualMachineNetworkInterface) (*vimtypes.CustomizationAdapterMapping, error) {

	netIf, err := np.waitForNetworkInterfaceStatus(context.Background(), vm, vmIf)
	if err != nil {
		return nil, err
	}

	var adapter *vimtypes.CustomizationIPSettings
	if len(netIf.Status.IPConfigs) == 0 {
		adapter = &vimtypes.CustomizationIPSettings{
			Ip: &vimtypes.CustomizationDhcpIpGenerator{},
		}
	} else {
		ipConfig := netIf.Status.IPConfigs[0]
		if ipConfig.IPFamily == netopv1alpha1.IPv4Protocol {
			adapter = &vimtypes.CustomizationIPSettings{
				Ip:         &vimtypes.CustomizationFixedIp{IpAddress: ipConfig.IP},
				SubnetMask: ipConfig.SubnetMask,
				Gateway:    []string{ipConfig.Gateway},
			}
		} else if ipConfig.IPFamily == netopv1alpha1.IPv6Protocol {
			subnetMask := net.ParseIP(ipConfig.SubnetMask)
			var ipMask net.IPMask = make([]byte, net.IPv6len)
			copy(ipMask, subnetMask)
			ones, _ := ipMask.Size()

			adapter = &vimtypes.CustomizationIPSettings{
				IpV6Spec: &vimtypes.CustomizationIPSettingsIpV6AddressSpec{
					Ip: []vimtypes.BaseCustomizationIpV6Generator{
						&vimtypes.CustomizationFixedIpV6{
							IpAddress:  ipConfig.IP,
							SubnetMask: int32(ones),
						},
					},
					Gateway: []string{ipConfig.Gateway},
				},
			}
		} else {
			adapter = &vimtypes.CustomizationIPSettings{}
		}
	}

	// Note that not all NetworkTypes populate the MacAddress.
	return &vimtypes.CustomizationAdapterMapping{
		MacAddress: netIf.Status.MacAddress,
		Adapter:    *adapter,
	}, nil
}

func (np *netOpNetworkProvider) netIfIsReady(netIf *netopv1alpha1.NetworkInterface) bool {
	for _, cond := range netIf.Status.Conditions {
		if cond.Type == netopv1alpha1.NetworkInterfaceReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

func (np *netOpNetworkProvider) waitForNetworkInterfaceStatus(ctx context.Context, vm *v1alpha1.VirtualMachine,
	vmIf *v1alpha1.VirtualMachineNetworkInterface) (*netopv1alpha1.NetworkInterface, error) {

	netIfName := np.generateNetIfName(vmIf.NetworkName, vm.Name)

	// Handling user created networkInterfaces out of the box
	if vmIf.ProviderRef != nil {
		netIf, err := np.getNetOpNetIf(ctx, vmIf.ProviderRef, vm.Namespace)
		if err != nil {
			return nil, err
		}
		netIfName = netIf.Name
	}

	netIfKey := types.NamespacedName{Namespace: vm.Namespace, Name: netIfName}

	var netIf *netopv1alpha1.NetworkInterface
	err := wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
		instance := &netopv1alpha1.NetworkInterface{}
		err := np.k8sClient.Get(ctx, netIfKey, instance)
		if err != nil {
			return false, err
		}

		if !np.netIfIsReady(instance) {
			return false, nil
		}

		netIf = instance
		return true, nil
	})

	return netIf, err
}

type nsxtNetworkProvider struct {
	k8sClient ctrlruntime.Client
	finder    *find.Finder
	cluster   *object.ClusterComputeResource
}

// NsxtNetworkProvider returns a nsxtNetworkProvider instance
func NsxtNetworkProvider(client ctrlruntime.Client, finder *find.Finder, cluster *object.ClusterComputeResource) *nsxtNetworkProvider {
	return &nsxtNetworkProvider{
		k8sClient: client,
		finder:    finder,
		cluster:   cluster,
	}
}

// GenerateNsxVnetifName generates the vnetif name for the VM
func (np *nsxtNetworkProvider) GenerateNsxVnetifName(networkName, vmName string) string {
	vnetifName := fmt.Sprintf("%s-lsp", vmName)
	if networkName != "" {
		vnetifName = fmt.Sprintf("%s-%s", networkName, vnetifName)
	}
	return vnetifName
}

// setVnetifOwner sets owner reference for vnetif object
func (np *nsxtNetworkProvider) setVnetifOwner(vm *v1alpha1.VirtualMachine, vnetif *ncpv1alpha1.VirtualNetworkInterface) {
	owner := []metav1.OwnerReference{
		{
			Name:       vm.Name,
			APIVersion: vm.APIVersion,
			Kind:       vm.Kind,
			UID:        vm.UID,
		},
	}
	vnetif.SetOwnerReferences(owner)
}

// ownerMatch checks the owner reference for the vnetif object
// owner of VirtualMachine type should be object vm
func (np *nsxtNetworkProvider) ownerMatch(vm *v1alpha1.VirtualMachine, vnetif *ncpv1alpha1.VirtualNetworkInterface) bool {
	match := false
	for _, owner := range vnetif.GetOwnerReferences() {
		if owner.Kind != vm.Kind {
			continue
		}
		// TODO: shouldn't we check that owner.UID/vm.UID are not empty?
		if owner.UID != vm.UID {
			return false
		} else {
			match = true
		}
	}
	return match
}

// createVirtualNetworkInterface creates a NCP vnetif for a given VM network interface
func (np *nsxtNetworkProvider) createVirtualNetworkInterface(ctx context.Context, vm *v1alpha1.VirtualMachine,
	vmIf *v1alpha1.VirtualMachineNetworkInterface) (*ncpv1alpha1.VirtualNetworkInterface, error) {

	// Create vnetif object
	vnetif := &ncpv1alpha1.VirtualNetworkInterface{
		ObjectMeta: metav1.ObjectMeta{
			Name:      np.GenerateNsxVnetifName(vmIf.NetworkName, vm.Name),
			Namespace: vm.Namespace,
		},
		Spec: ncpv1alpha1.VirtualNetworkInterfaceSpec{
			VirtualNetwork: vmIf.NetworkName,
		},
	}
	np.setVnetifOwner(vm, vnetif)

	err := np.k8sClient.Create(ctx, vnetif)
	if err != nil {
		if !k8serrors.IsAlreadyExists(err) {
			return nil, err
		}
		// Update owner reference if vnetif already exist
		currentVnetIf := &ncpv1alpha1.VirtualNetworkInterface{}
		err := np.k8sClient.Get(ctx, types.NamespacedName{Namespace: vm.Namespace, Name: vnetif.Name}, currentVnetIf)
		if err != nil {
			return nil, err
		}
		if !np.ownerMatch(vm, currentVnetIf) {
			copiedVnetif := currentVnetIf.DeepCopy()
			np.setVnetifOwner(vm, copiedVnetif)
			err := np.k8sClient.Update(ctx, copiedVnetif)
			if err != nil {
				return nil, err
			}
		}
	} else {
		log.Info("Successfully created a VirtualNetworkInterface",
			"VirtualMachine", vm.NamespacedName(),
			"VirtualNetworkInterface", types.NamespacedName{Namespace: vm.Namespace, Name: vnetif.Name})
	}

	// Wait until the vnetif status is available
	// TODO: Rather than synchronously block here, place a watch on the VirtualNetworkInterface
	result, err := np.waitForVnetIfStatus(ctx, vm.Namespace, vmIf.NetworkName, vm.Name)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// vnetifIsReady checks the readiness of vnetif object
func (np *nsxtNetworkProvider) vnetifIsReady(vnetif *ncpv1alpha1.VirtualNetworkInterface) bool {
	for _, condition := range vnetif.Status.Conditions {
		if !strings.Contains(condition.Type, "Ready") {
			continue
		}
		return strings.Contains(condition.Status, "True")
	}
	return false
}

// waitForVnetIfStatus will poll until the vnetif object's status becomes ready
func (np *nsxtNetworkProvider) waitForVnetIfStatus(ctx context.Context, namespace, networkName, vmName string) (*ncpv1alpha1.VirtualNetworkInterface, error) {
	vnetifName := np.GenerateNsxVnetifName(networkName, vmName)
	vnetifKey := types.NamespacedName{Namespace: namespace, Name: vnetifName}

	var result *ncpv1alpha1.VirtualNetworkInterface
	err := wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
		vnetif := &ncpv1alpha1.VirtualNetworkInterface{}
		err := np.k8sClient.Get(ctx, vnetifKey, vnetif)
		if err != nil {
			return false, err
		}

		if !np.vnetifIsReady(vnetif) {
			return false, nil
		}

		result = vnetif
		return true, nil
	})

	return result, err
}

func (np *nsxtNetworkProvider) CreateVnic(ctx context.Context, vm *v1alpha1.VirtualMachine, vif *v1alpha1.VirtualMachineNetworkInterface) (vimtypes.BaseVirtualDevice, error) {
	vnetif, err := np.createVirtualNetworkInterface(ctx, vm, vif)
	if err != nil {
		log.Error(err, "Failed to create vnetif for vif", "vif", vif)
		return nil, err
	}
	if vnetif.Status.ProviderStatus == nil || vnetif.Status.ProviderStatus.NsxLogicalSwitchID == "" {
		err = fmt.Errorf("failed to get for nsx-t opaque network ID for vnetif '%+v'", vnetif)
		log.Error(err, "Failed to get for nsx-t opaque network ID for vnetif")
		return nil, err
	}

	networkRef, err := searchNsxtNetworkReference(ctx, np.finder, np.cluster, vnetif.Status.ProviderStatus.NsxLogicalSwitchID)
	if err != nil {
		log.Error(err, "Failed to search for nsx-t network associated with vnetif", "vnetif", vnetif)
		return nil, err
	}

	// config vnic
	dev, err := createCommonVnic(ctx, networkRef, getEthCardType(vif))
	if err != nil {
		return nil, err
	}

	nic := dev.(vimtypes.BaseVirtualEthernetCard).GetVirtualEthernetCard()
	nic.ExternalId = vnetif.Status.InterfaceID
	nic.MacAddress = vnetif.Status.MacAddress
	nic.AddressType = string(vimtypes.VirtualEthernetCardMacTypeManual)

	return dev, nil
}

func (np *nsxtNetworkProvider) GetInterfaceGuestCustomization(vm *v1alpha1.VirtualMachine, vif *v1alpha1.VirtualMachineNetworkInterface) (*vimtypes.CustomizationAdapterMapping, error) {
	vnetif, err := np.waitForVnetIfStatus(context.Background(), vm.Namespace, vif.NetworkName, vm.Name)
	if err != nil {
		return nil, err
	}

	var adapter *vimtypes.CustomizationIPSettings
	if len(vnetif.Status.IPAddresses) == 0 {
		adapter = &vimtypes.CustomizationIPSettings{
			Ip: &vimtypes.CustomizationDhcpIpGenerator{},
		}
	} else {
		ipAddr := vnetif.Status.IPAddresses[0]
		adapter = &vimtypes.CustomizationIPSettings{
			Ip:         &vimtypes.CustomizationFixedIp{IpAddress: ipAddr.IP},
			SubnetMask: ipAddr.SubnetMask,
			Gateway:    []string{ipAddr.Gateway},
		}
	}

	return &vimtypes.CustomizationAdapterMapping{
		MacAddress: vnetif.Status.MacAddress,
		Adapter:    *adapter,
	}, nil
}

// matchOpaqueNetwork takes the network ID, returns whether the opaque network matches the networkID
func matchOpaqueNetwork(ctx context.Context, network object.NetworkReference, networkID string) bool {
	obj, ok := network.(*object.OpaqueNetwork)
	if !ok {
		return false
	}

	var opaqueNet mo.OpaqueNetwork
	if err := obj.Properties(ctx, obj.Reference(), []string{"summary"}, &opaqueNet); err != nil {
		return false
	}

	summary, _ := opaqueNet.Summary.(*vimtypes.OpaqueNetworkSummary)
	return summary.OpaqueNetworkId == networkID
}

// Get Host MoIDs for a cluster
func getClusterHostMoIDs(ctx context.Context, cluster *object.ClusterComputeResource) ([]vimtypes.ManagedObjectReference, error) {
	var computeResource mo.ComputeResource
	obj := cluster.Reference()

	// Get the list of ESX host moRef objects for this cluster
	err := cluster.Properties(ctx, obj, nil, &computeResource)
	if err != nil {
		log.Error(err, "Failed to get cluster properties")
		return nil, err
	}
	return computeResource.Host, nil
}

// matchDistributedPortGroup takes the network ID, returns whether the distributed port group matches the networkID
func matchDistributedPortGroup(ctx context.Context, cluster *object.ClusterComputeResource, network object.NetworkReference, networkID string) bool {
	obj, ok := network.(*object.DistributedVirtualPortgroup)
	if !ok {
		return false
	}

	hostMoIDs, err := getClusterHostMoIDs(ctx, cluster)
	if err != nil {
		log.Error(err, "Unable to get the list of hosts for cluster")
		return false
	}

	var configInfo []vimtypes.ObjectContent
	err = obj.Properties(ctx, obj.Reference(), []string{"config.logicalSwitchUuid", "host"}, &configInfo)
	if err != nil {
		return false
	}

	if len(configInfo) > 0 {
		// Check "logicalSwitchUuid" property
		lsIDMatch := false
		for _, dynamicProperty := range configInfo[0].PropSet {
			if dynamicProperty.Name == "config.logicalSwitchUuid" && dynamicProperty.Val == networkID {
				lsIDMatch = true
				break
			}
		}

		// logicalSwitchUuid did not match
		if !lsIDMatch {
			return false
		}

		foundAllHosts := false
		for _, dynamicProperty := range configInfo[0].PropSet {
			// In the case of a single NSX Overlay Transport Zone for all the clusters and DVS's,
			// multiple DVPGs(associated with different DVS's) will have the same "logicalSwitchUuid".
			// So matching "logicalSwitchUuid" is necessary condition, but not sufficient.
			// Checking if the DPVG has all the hosts in the cluster, along with the above would be sufficient
			if dynamicProperty.Name == "host" {
				if hosts, ok := dynamicProperty.Val.(vimtypes.ArrayOfManagedObjectReference); ok {
					foundAllHosts = true
					dvsHostSet := make(map[string]bool, len(hosts.ManagedObjectReference))
					for _, dvsHost := range hosts.ManagedObjectReference {
						dvsHostSet[dvsHost.Value] = true
					}

					for _, hostMoRef := range hostMoIDs {
						if _, ok := dvsHostSet[hostMoRef.Value]; !ok {
							foundAllHosts = false
							break
						}
					}
				}
			}
		}

		// Implicit that lsID Matches at this point
		return foundAllHosts
	}
	return false
}

// searchNsxtNetworkReference takes in nsx-t logical switch UUID and returns the reference of the network
func searchNsxtNetworkReference(ctx context.Context, finder *find.Finder, cluster *object.ClusterComputeResource, networkID string) (object.NetworkReference, error) {
	networks, err := finder.NetworkList(ctx, "*")
	if err != nil {
		return nil, err
	}
	for _, network := range networks {
		if matchDistributedPortGroup(ctx, cluster, network, networkID) {
			return network, nil
		}
		if matchOpaqueNetwork(ctx, network, networkID) {
			return network, nil
		}
	}
	return nil, fmt.Errorf("opaque network with ID '%s' not found", networkID)
}
