// Copyright (c) 2018-2020 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package virtualmachineservice

import (
	goctx "context"
	"fmt"
	"reflect"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/go-logr/logr"
	vmoperatorv1alpha1 "github.com/vmware-tanzu/vm-operator-api/api/v1alpha1"

	"github.com/vmware-tanzu/vm-operator/controllers/virtualmachineservice/providers"
	"github.com/vmware-tanzu/vm-operator/controllers/virtualmachineservice/utils"
	"github.com/vmware-tanzu/vm-operator/pkg"
	"github.com/vmware-tanzu/vm-operator/pkg/conditions"
	"github.com/vmware-tanzu/vm-operator/pkg/context"
	"github.com/vmware-tanzu/vm-operator/pkg/record"
)

const (
	ServiceOwnerRefKind    = "VirtualMachineService"
	ServiceOwnerRefVersion = "vmoperator.vmware.com/v1alpha1"

	finalizerName = "virtualmachineservice.vmoperator.vmware.com"

	OpCreate       = "CreateK8sService"
	OpDelete       = "DeleteK8sService"
	OpUpdate       = "UpdateK8sService"
	ControllerName = "virtualmachineservice-controller"
)

func AddToManager(ctx *context.ControllerManagerContext, mgr manager.Manager) error {
	var (
		controlledType     = &vmoperatorv1alpha1.VirtualMachineService{}
		controlledTypeName = reflect.TypeOf(controlledType).Elem().Name()

		controllerNameShort = fmt.Sprintf("%s-controller", strings.ToLower(controlledTypeName))
		controllerNameLong  = fmt.Sprintf("%s/%s/%s", ctx.Namespace, ctx.Name, controllerNameShort)
	)

	lbProvider, err := providers.GetLoadbalancerProviderByType(mgr, providers.LBProvider)
	if err != nil {
		return err
	}

	r := NewReconciler(
		mgr.GetClient(),
		ctrl.Log.WithName("controllers").WithName(controlledTypeName),
		mgr.GetScheme(),
		record.New(mgr.GetEventRecorderFor(controllerNameLong)),
		lbProvider,
	)

	return add(ctx, mgr, r, r)
}

func NewReconciler(
	client client.Client,
	logger logr.Logger,
	scheme *runtime.Scheme,
	recorder record.Recorder,
	lbProvider providers.LoadbalancerProvider,
) *ReconcileVirtualMachineService {

	return &ReconcileVirtualMachineService{
		Client:               client,
		log:                  logger,
		scheme:               scheme,
		recorder:             recorder,
		loadbalancerProvider: lbProvider,
	}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(ctx *context.ControllerManagerContext, mgr manager.Manager, r reconcile.Reconciler, rvms *ReconcileVirtualMachineService) error {
	// Create a new controller
	c, err := controller.New(ControllerName, mgr, controller.Options{Reconciler: r, MaxConcurrentReconciles: ctx.MaxConcurrentReconciles})
	if err != nil {
		return err
	}

	// Watch for changes to VirtualMachineService
	err = c.Watch(&source.Kind{Type: &vmoperatorv1alpha1.VirtualMachineService{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// Watch VirtualMachine resources so that VmServices can be updated in response to changes in VM IP status and VM
	// label configuration.
	//
	// TODO: Ensure that we have adequate tests for these IP and label updates.
	err = c.Watch(&source.Kind{Type: &vmoperatorv1alpha1.VirtualMachine{}},
		&handler.EnqueueRequestsFromMapFunc{ToRequests: handler.ToRequestsFunc(rvms.virtualMachineToVirtualMachineServiceMapper)})
	if err != nil {
		return err
	}

	err = c.Watch(&source.Kind{Type: &corev1.Service{}},
		&handler.EnqueueRequestForOwner{OwnerType: &vmoperatorv1alpha1.VirtualMachineService{}, IsController: false})
	if err != nil {
		return err
	}

	err = c.Watch(&source.Kind{Type: &corev1.Endpoints{}},
		&handler.EnqueueRequestForOwner{OwnerType: &vmoperatorv1alpha1.VirtualMachineService{}, IsController: false})
	if err != nil {
		return err
	}

	return nil
}

var _ reconcile.Reconciler = &ReconcileVirtualMachineService{}

// ReconcileVirtualMachineService reconciles a VirtualMachineService object
type ReconcileVirtualMachineService struct {
	client.Client
	log                  logr.Logger
	scheme               *runtime.Scheme
	recorder             record.Recorder
	loadbalancerProvider providers.LoadbalancerProvider
}

// Reconcile reads that state of the cluster for a VirtualMachineService object and makes changes based on the state read
// and what is in the VirtualMachineService.Spec
// +kubebuilder:rbac:groups=vmoperator.vmware.com,resources=virtualmachineservices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=vmoperator.vmware.com,resources=virtualmachineservices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=vmware.com,resources=loadbalancers;loadbalancers/status,verbs=create;get;list;patch;delete;watch;update
// +kubebuilder:rbac:groups=vmware.com,resources=virtualnetworks;virtualnetworks/status,verbs=create;get;list;patch;delete;watch;update
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=endpoints,verbs=get;list;watch;create;update;patch;delete

func (r *ReconcileVirtualMachineService) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	ctx := goctx.Background()

	instance := &vmoperatorv1alpha1.VirtualMachineService{}
	err := r.Get(ctx, request.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	if !instance.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, r.ReconcileDelete(ctx, instance)
	}

	return reconcile.Result{}, r.ReconcileNormal(ctx, instance)
}

func (r *ReconcileVirtualMachineService) ReconcileDelete(ctx goctx.Context, vmService *vmoperatorv1alpha1.VirtualMachineService) error {
	if controllerutil.ContainsFinalizer(vmService, finalizerName) {
		r.log.Info("Delete VirtualMachineService", "service", vmService)
		r.recorder.EmitEvent(vmService, OpDelete, nil, false)

		controllerutil.RemoveFinalizer(vmService, finalizerName)
		if err := r.Update(ctx, vmService); err != nil {
			return err
		}
	}

	return nil
}

func (r *ReconcileVirtualMachineService) ReconcileNormal(ctx goctx.Context, vmService *vmoperatorv1alpha1.VirtualMachineService) error {
	if !controllerutil.ContainsFinalizer(vmService, finalizerName) {
		controllerutil.AddFinalizer(vmService, finalizerName)
		if err := r.Update(ctx, vmService); err != nil {
			return err
		}
	}

	if err := r.reconcileVmService(ctx, vmService); err != nil {
		r.log.Error(err, "Failed to reconcile VirtualMachineService", "service", vmService)
		return err
	}

	return nil
}

func (r *ReconcileVirtualMachineService) reconcileVmService(ctx goctx.Context, vmService *vmoperatorv1alpha1.VirtualMachineService) (err error) {
	r.log.Info("Reconcile VirtualMachineService", "name", vmService.NamespacedName())
	defer r.log.Info("Finished Reconcile VirtualMachineService", "name", vmService.NamespacedName())

	if vmService.Spec.Type == vmoperatorv1alpha1.VirtualMachineServiceTypeLoadBalancer {
		// Get LoadBalancer to attach
		err = r.loadbalancerProvider.EnsureLoadBalancer(ctx, vmService)
		if err != nil {
			r.log.Error(err, "Failed to create or get load balancer for vm service", "name", vmService.Name)
			return err
		}

		// Get the provider specific annotations for Service and add
		// them to the vm service as that's where Service inherits the
		// values
		annotations, err := r.loadbalancerProvider.GetServiceAnnotations(ctx, vmService)
		if err != nil {
			r.log.Error(err, "Failed to get loadbalancer annotations for service", "name", vmService.Name)
			return err
		}

		// Initialize VM Service Annotations when it is nil
		if vmService.Annotations == nil {
			vmService.Annotations = make(map[string]string)
		}

		for k, v := range annotations {
			if oldValue, ok := vmService.Annotations[k]; ok {
				r.log.V(5).Info("Replacing previous annotation value on vm service",
					"vmServiceName", vmService.NamespacedName,
					"key", k,
					"oldValue", oldValue,
					"newValue", v)
			}
			vmService.Annotations[k] = v
		}

		// Get the provider specific labels for Service and add
		// them to the vm service as that's where Service inherits the
		// values
		labels, err := r.loadbalancerProvider.GetServiceLabels(ctx, vmService)
		if err != nil {
			r.log.Error(err, "Failed to get loadbalancer labels for service", "name", vmService.Name)
			return err
		}

		// Initialize VM Service Labels when it is nil
		if vmService.Labels == nil {
			vmService.Labels = make(map[string]string)
		}

		for k, v := range labels {
			if oldValue, ok := vmService.Labels[k]; ok {
				r.log.V(5).Info("Replacing previous label value on vm service",
					"vmServiceName", vmService.NamespacedName,
					"key", k,
					"oldValue", oldValue,
					"newValue", v)
			}
			vmService.Labels[k] = v
		}
	}

	// Reconcile k8s Service
	newService, err := r.CreateOrUpdateService(ctx, vmService)
	if err != nil {
		r.log.Error(err, "Failed to update k8s Service for VirtualMachineService", "vmServiceName", vmService.NamespacedName)
		return err
	}

	// Update VirtualMachineService endpoints
	err = r.UpdateEndpoints(ctx, vmService, newService)
	if err != nil {
		r.log.Error(err, "Failed to update VirtualMachineService endpoints", "name", vmService.NamespacedName())
		return err
	}

	// Update VirtualMachineService resource
	err = r.UpdateVmService(ctx, vmService, newService)
	if err != nil {
		r.log.Error(err, "Failed to update VirtualMachineService Status", "name", vmService.NamespacedName())
		return err
	}

	return nil
}

// For a given VM, determine which vmServices select that VM via label selector and return a set of reconcile requests
// for those selecting VMServices.
func (r *ReconcileVirtualMachineService) virtualMachineToVirtualMachineServiceMapper(o handler.MapObject) []reconcile.Request {
	var reconcileRequests []reconcile.Request

	vm := o.Object.(*vmoperatorv1alpha1.VirtualMachine)
	// Find all vm services that match this vm
	vmServiceList, err := r.getVirtualMachineServicesSelectingVirtualMachine(goctx.Background(), vm)
	if err != nil {
		return reconcileRequests
	}

	for _, vmService := range vmServiceList {
		r.log.V(4).Info("Generating reconcile request for vmService due to event on VMs",
			"VirtualMachineService", types.NamespacedName{Namespace: vmService.Namespace, Name: vmService.Name},
			"VirtualMachine", types.NamespacedName{Namespace: vm.Namespace, Name: vm.Name})
		reconcileRequests = append(reconcileRequests,
			reconcile.Request{NamespacedName: types.NamespacedName{Namespace: vmService.Namespace, Name: vmService.Name}})
	}

	return reconcileRequests
}

// MakeObjectMeta returns an ObjectMeta struct with values filled in from input VirtualMachineService. Also sets the VM operator annotations.
func MakeObjectMeta(vmService *vmoperatorv1alpha1.VirtualMachineService) metav1.ObjectMeta {
	om := metav1.ObjectMeta{
		Namespace:   vmService.Namespace,
		Name:        vmService.Name,
		Labels:      vmService.Labels,
		Annotations: vmService.Annotations,
		OwnerReferences: []metav1.OwnerReference{
			{
				UID:                vmService.UID,
				Name:               vmService.Name,
				Controller:         pointer.BoolPtr(true),
				BlockOwnerDeletion: pointer.BoolPtr(true),
				Kind:               ServiceOwnerRefKind,
				APIVersion:         ServiceOwnerRefVersion,
			},
		},
	}
	pkg.AddAnnotations(&om)

	return om
}

func (r *ReconcileVirtualMachineService) makeEndpoints(vmService *vmoperatorv1alpha1.VirtualMachineService, currentEndpoints *corev1.Endpoints, subsets []corev1.EndpointSubset) *corev1.Endpoints {
	newEndpoints := currentEndpoints.DeepCopy()
	newEndpoints.ObjectMeta = MakeObjectMeta(vmService)
	newEndpoints.Subsets = subsets
	return newEndpoints
}

func (r *ReconcileVirtualMachineService) makeEndpointAddress(vm *vmoperatorv1alpha1.VirtualMachine) *corev1.EndpointAddress {
	return &corev1.EndpointAddress{
		IP: vm.Status.VmIp,
		TargetRef: &corev1.ObjectReference{
			APIVersion: vm.APIVersion,
			Kind:       vm.Kind,
			Namespace:  vm.Namespace,
			Name:       vm.Name,
			UID:        vm.UID,
		}}
}

// vmServiceToService converts a VM Service to k8s Service.
func (r *ReconcileVirtualMachineService) vmServiceToService(vmService *vmoperatorv1alpha1.VirtualMachineService) *corev1.Service {
	servicePorts := make([]corev1.ServicePort, 0, len(vmService.Spec.Ports))
	for _, vmPort := range vmService.Spec.Ports {
		sport := corev1.ServicePort{
			Name:       vmPort.Name,
			Protocol:   corev1.Protocol(vmPort.Protocol),
			Port:       vmPort.Port,
			TargetPort: intstr.FromInt(int(vmPort.TargetPort)),
		}
		servicePorts = append(servicePorts, sport)
	}

	svc := &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Service",
			APIVersion: "core/v1",
		},
		ObjectMeta: MakeObjectMeta(vmService),
		Spec: corev1.ServiceSpec{
			// Don't specify selector to keep endpoints controller from interfering
			Type:                     corev1.ServiceType(vmService.Spec.Type),
			Ports:                    servicePorts,
			ExternalName:             vmService.Spec.ExternalName,
			ClusterIP:                vmService.Spec.ClusterIP,
			LoadBalancerIP:           vmService.Spec.LoadBalancerIP,
			LoadBalancerSourceRanges: vmService.Spec.LoadBalancerSourceRanges,
		},
	}

	// Setting default value so during update we could find the delta by
	// DeepEqual
	if svc.Spec.Type == corev1.ServiceTypeLoadBalancer || svc.Spec.Type == corev1.ServiceTypeNodePort {
		svc.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyTypeCluster
	}

	// When VirtualMachineService is created with annotation
	// labelServiceExternalTrafficPolicyKey, use its value for the
	// Service.Spec.ExternalTrafficPolicy
	if externalTrafficPolicy, ok := svc.Annotations[utils.AnnotationServiceExternalTrafficPolicyKey]; ok {
		switch corev1.ServiceExternalTrafficPolicyType(externalTrafficPolicy) {
		// Only two valid values are accepted
		case corev1.ServiceExternalTrafficPolicyTypeLocal, corev1.ServiceExternalTrafficPolicyTypeCluster:
			{
				svc.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyType(externalTrafficPolicy)
			}
		default:
			r.log.V(5).Info("Unknown externalTrafficPolicy configured in VirtualMachineService label, skip it", "externalTrafficPolicy", externalTrafficPolicy)
		}
	}

	return svc
}

func findPort(vm *vmoperatorv1alpha1.VirtualMachine, portName intstr.IntOrString, portProto corev1.Protocol) (int, error) {
	switch portName.Type {
	case intstr.String:
		name := portName.StrVal
		for _, port := range vm.Spec.Ports {
			if port.Name == name && port.Protocol == portProto {
				return port.Port, nil
			}
		}
	case intstr.Int:
		return portName.IntValue(), nil
	}

	return 0, fmt.Errorf("no suitable port for manifest: %s", vm.UID)
}

func addEndpointSubset(subsets []corev1.EndpointSubset, epa corev1.EndpointAddress, epp *corev1.EndpointPort) []corev1.EndpointSubset {
	var ports []corev1.EndpointPort
	if epp != nil {
		ports = append(ports, *epp)
	}

	subsets = append(subsets,
		corev1.EndpointSubset{
			Addresses: []corev1.EndpointAddress{epa},
			Ports:     ports,
		})

	return subsets
}

// createOrUpdateService reconciles the k8s Service corresponding to the VirtualMachineService by creating a new Service or updating an existing one. Returns the newly created or updated Service.
// nolint:gocyclo
func (r *ReconcileVirtualMachineService) CreateOrUpdateService(ctx goctx.Context, vmService *vmoperatorv1alpha1.VirtualMachineService) (*corev1.Service, error) {
	// We can use vmService's namespace and name since Service and VirtualMachineService live in the same namespace.
	serviceKey := client.ObjectKey{Name: vmService.Name, Namespace: vmService.Namespace}

	r.log.V(5).Info("Reconciling k8s service", "serviceName", serviceKey)
	defer r.log.V(5).Info("Finished reconciling k8s Service", "serviceName", serviceKey)

	// Find the current Service.
	currentService := &corev1.Service{}
	err := r.Get(ctx, serviceKey, currentService)
	if err != nil {
		if !errors.IsNotFound(err) {
			r.log.Error(err, "Failed to get Service", "name", serviceKey)
			return nil, err
		}

		// Service does not exist, we will create it.
		r.log.V(5).Info("Service not found. Will attempt to create it", "name", serviceKey)
		currentService = r.vmServiceToService(vmService)
	}

	// Determine if VirtualMachineService needs any update by comparing current k8s to newService synthesized from
	// VirtualMachineService
	newService := currentService.DeepCopy()
	svcFromVmService := r.vmServiceToService(vmService)

	// Merge labels of the Service with the VirtualMachineService. VirtualMachineService wins in case of conflicts.
	// We can't just clobber the labels since other operators (Net Operator) might rely on them.
	if newService.Labels == nil {
		newService.Labels = vmService.Labels
	} else {
		for k, v := range vmService.Labels {
			if oldValue, ok := newService.Labels[k]; ok {
				r.log.V(5).Info("Replacing previous label value on service",
					"service", newService.Name,
					"namespace", newService.Namespace,
					"key", k,
					"oldValue", oldValue,
					"newValue", v)
			}
			newService.Labels[k] = v
		}
	}

	// Merge annotations of the Service with the VirtualMachineService. VirtualMachineService wins in case of conflicts.
	if newService.Annotations == nil {
		newService.Annotations = vmService.Annotations
	} else {
		for k, v := range vmService.Annotations {
			if oldValue, ok := newService.Annotations[k]; ok {
				r.log.V(5).Info("Replacing previous annotation value on service",
					"service", newService.Name,
					"namespace", newService.Namespace,
					"key", k,
					"oldValue", oldValue,
					"newValue", v)
			}
			newService.Annotations[k] = v
		}
	}

	// Explicitly remove provider specific annotations
	annotationsToBeRemoved, err := r.loadbalancerProvider.GetToBeRemovedServiceAnnotations(ctx, vmService)
	if err != nil {
		r.log.Error(err, "Failed to get to be removed loadbalancer annotations for service", "virtualmachineservice", vmService.Namespace+"/"+vmService.Name)
		return newService, err
	}
	for k := range annotationsToBeRemoved {
		r.log.V(5).Info("Removing annotation from service",
			"service", newService.Name,
			"namespace", newService.Namespace,
			"key", k,
		)
		delete(newService.Annotations, k)
	}

	// Explicitly remove provider specific labels
	labelsToBeRemoved, err := r.loadbalancerProvider.GetToBeRemovedServiceLabels(ctx, vmService)
	if err != nil {
		r.log.Error(err, "Failed to get to be removed loadbalancer labels for service", "virtualmachineservice", vmService.Namespace+"/"+vmService.Name)
		return newService, err
	}
	for k := range labelsToBeRemoved {
		r.log.V(5).Info("Removing label from service",
			"service", newService.Name,
			"namespace", newService.Namespace,
			"key", k,
		)
		delete(newService.Labels, k)
	}

	// Explicitly remove vm service managed annotations if needed
	for _, k := range []string{utils.AnnotationServiceExternalTrafficPolicyKey, utils.AnnotationServiceHealthCheckNodePortKey} {
		if _, exist := svcFromVmService.Annotations[k]; !exist {
			if v, exist := newService.Annotations[k]; exist {
				r.log.V(5).Info("Removing annotation from service",
					"service", newService.Name,
					"namespace", newService.Namespace,
					"key", k,
					"value", v)
			}
			delete(newService.Annotations, k)
		}
	}

	newService.Spec.Type = svcFromVmService.Spec.Type
	newService.Spec.ExternalName = svcFromVmService.Spec.ExternalName
	newService.Spec.Ports = svcFromVmService.Spec.Ports
	newService.Spec.LoadBalancerIP = svcFromVmService.Spec.LoadBalancerIP
	// Use DeepEqual to check so we avoid unnecessary updates and
	// missing value removal
	if !apiequality.Semantic.DeepEqual(newService.Spec.LoadBalancerSourceRanges, svcFromVmService.Spec.LoadBalancerSourceRanges) {
		newService.Spec.LoadBalancerSourceRanges = svcFromVmService.Spec.LoadBalancerSourceRanges
	}
	if !apiequality.Semantic.DeepEqual(newService.Spec.ExternalTrafficPolicy, svcFromVmService.Spec.ExternalTrafficPolicy) {
		newService.Spec.ExternalTrafficPolicy = svcFromVmService.Spec.ExternalTrafficPolicy
	}

	// Maintain the existing mapping of ServicePort -> NodePort
	// as un-setting it will cause the apiserver to allocate a
	// new NodePort on an Update.
	populateNodePorts(currentService, newService)

	// Add VM operator annotations.
	pkg.AddAnnotations(&newService.ObjectMeta)

	// Create or update or don't update Service.
	createService := len(currentService.ResourceVersion) == 0
	if createService {
		r.log.Info("Creating k8s Service", "name", serviceKey, "service", newService)
		err = r.Create(ctx, newService)
		r.recorder.EmitEvent(vmService, OpCreate, err, false)
	} else if !apiequality.Semantic.DeepEqual(currentService, newService) {
		r.log.Info("Updating k8s Service", "name", serviceKey, "service", newService)
		err = r.Update(ctx, newService)
		r.recorder.EmitEvent(vmService, OpUpdate, err, false)
	} else {
		r.log.V(5).Info("No need to update current K8s Service. Skipping Update",
			"vmServiceName", vmService.NamespacedName(), "currentService", currentService, "vmService", vmService)
	}

	return newService, err
}

func (r *ReconcileVirtualMachineService) GetVirtualMachinesSelectedByVmService(ctx goctx.Context, vmService *vmoperatorv1alpha1.VirtualMachineService) (*vmoperatorv1alpha1.VirtualMachineList, error) {
	vmList := &vmoperatorv1alpha1.VirtualMachineList{}
	err := r.List(ctx, vmList, client.InNamespace(vmService.Namespace), client.MatchingLabels(vmService.Spec.Selector))
	return vmList, err
}

// getVMsReferencedByServiceEndpoints gets all VMs that are referenced by service endpoints
// returns a map, key is the VM uid and value is a boolean value indicating whether this VM is included in the endpoint subsets.
func (r *ReconcileVirtualMachineService) getVMsReferencedByServiceEndpoints(ctx goctx.Context, service *corev1.Service) (map[types.UID]bool, error) {
	currentEndpoints := &corev1.Endpoints{}

	if err := r.Get(ctx, types.NamespacedName{Name: service.Name, Namespace: service.Namespace}, currentEndpoints); err != nil {
		r.log.Error(err, "Failed to get endpoints.", "name", service.Name, "namespace", service.Namespace)
		return map[types.UID]bool{}, err
	}

	subsets := currentEndpoints.Subsets
	vmToSubsetsMap := make(map[types.UID]bool)

	for _, subset := range subsets {
		for _, epa := range subset.Addresses {
			if epa.TargetRef != nil {
				vmToSubsetsMap[epa.TargetRef.UID] = true
			}
		}
	}
	return vmToSubsetsMap, nil
}

// TODO: This mapping function has the potential to be a performance and scaling issue.  Consider this as a candidate for profiling
func (r *ReconcileVirtualMachineService) getVirtualMachineServicesSelectingVirtualMachine(ctx goctx.Context, lookupVm *vmoperatorv1alpha1.VirtualMachine) ([]*vmoperatorv1alpha1.VirtualMachineService, error) {
	var matchingVmServices []*vmoperatorv1alpha1.VirtualMachineService

	matchFunc := func(vmService *vmoperatorv1alpha1.VirtualMachineService) error {
		vmList, err := r.GetVirtualMachinesSelectedByVmService(ctx, vmService)
		if err != nil {
			return err
		}

		lookupVmKey := types.NamespacedName{Namespace: lookupVm.Namespace, Name: lookupVm.Name}
		for _, vm := range vmList.Items {
			vmKey := types.NamespacedName{Namespace: vm.Namespace, Name: vm.Name}

			if vmKey == lookupVmKey {
				matchingVmServices = append(matchingVmServices, vmService)
				// Only one match is needed to add vmService, so return now.
				return nil
			}
		}

		return nil
	}

	vmServiceList := &vmoperatorv1alpha1.VirtualMachineServiceList{}
	err := r.List(ctx, vmServiceList, client.InNamespace(lookupVm.Namespace))
	if err != nil {
		return nil, err
	}

	for _, vmService := range vmServiceList.Items {
		vms := vmService
		err := matchFunc(&vms)
		if err != nil {
			return nil, err
		}
	}

	return matchingVmServices, nil
}

// UpdateEndpoints updates the service endpoints for VirtualMachineService. This is done by running the readiness probe against all the VMs and adding the successful VMs to the endpoint.
func (r *ReconcileVirtualMachineService) UpdateEndpoints(ctx goctx.Context, vmService *vmoperatorv1alpha1.VirtualMachineService, service *corev1.Service) error {
	logger := r.log.WithValues("vmServiceName", vmService.NamespacedName())
	logger.V(5).Info("Updating VirtualMachineService endpoints")
	defer logger.V(5).Info("Finished updating VirtualMachineService endpoints")

	subsets, err := r.generateSubsetsForService(ctx, service, vmService, logger)
	if err != nil {
		return err
	}

	// Repack subsets in canonical order so that comparison of two subsets gives consistent results.
	subsets = utils.RepackSubsets(subsets)

	// See if there's actually an update here.
	currentEndpoints := &corev1.Endpoints{}
	err = r.Get(ctx, types.NamespacedName{Name: service.Name, Namespace: service.Namespace}, currentEndpoints)
	if err != nil {
		if !errors.IsNotFound(err) {
			logger.Error(err, "Failed to list endpoints")
			return err
		}

		currentEndpoints = &corev1.Endpoints{
			ObjectMeta: metav1.ObjectMeta{
				Name:   service.Name,
				Labels: service.Labels,
			},
		}
	} else if apiequality.Semantic.DeepEqual(utils.RepackSubsets(currentEndpoints.Subsets), subsets) {
		// Ideally, we dont have to repack both the endpoints (current endpoints and the calculated one) before comparing since after the first update
		// we expect the endpoint subsets to always be in canonical order. However, some controller is re-ordering these endpoint subsets. We sort both
		// sides for a consistent comparison result. PR: 2623292

		logger.V(5).Info("No change, no need to update endpoints", "endpoints", currentEndpoints)
		return nil
	}

	newEndpoints := r.makeEndpoints(vmService, currentEndpoints, subsets)

	createEndpoints := len(currentEndpoints.ResourceVersion) == 0

	logger.V(5).Info("EndpointSubsets have diverged. Will trigger Endpoint Create or Update", "currentEndpoints", currentEndpoints, "newEndpoints", newEndpoints, "isCreate", createEndpoints)

	if createEndpoints {
		logger.Info("Creating service endpoints", "endpoints", newEndpoints)
		err = r.Create(ctx, newEndpoints)
	} else {
		logger.Info("Updating service endpoints", "endpoints", newEndpoints)
		err = r.Update(ctx, newEndpoints)
	}

	if err != nil {
		if createEndpoints && errors.IsForbidden(err) {
			// A request is forbidden primarily for two reasons:
			// 1. namespace is terminating, endpoint creation is not allowed by default.
			// 2. policy is mis-configured, in which case no service would function anywhere.
			// Given the frequency of 1, we logger at a lower level.
			logger.V(5).Info("Forbidden from creating endpoints", "error", err.Error())
		}
		return err
	}
	return nil
}

// generateSubsetsForService generates endpoint subsets for a given service.
func (r *ReconcileVirtualMachineService) generateSubsetsForService(ctx goctx.Context, service *corev1.Service,
	vmService *vmoperatorv1alpha1.VirtualMachineService, logger logr.Logger) ([]corev1.EndpointSubset, error) {
	vmList, err := r.GetVirtualMachinesSelectedByVmService(ctx, vmService)
	if err != nil {
		return []corev1.EndpointSubset{}, err
	}

	var subsets []corev1.EndpointSubset
	var vmInSubsetsMap map[types.UID]bool
	var vmInSubsetsMapSet bool

	for i := range vmList.Items {
		vm := vmList.Items[i]
		logger := logger.WithValues("virtualmachine", vm.NamespacedName())
		logger.V(5).Info("Resolving ports for VirtualMachine")
		// Skip VM's marked for deletions
		if !vm.DeletionTimestamp.IsZero() {
			logger.Info("Skipping VM marked for deletion")
			continue
		}
		// TODO: Handle multiple VM interfaces
		if len(vm.Status.VmIp) == 0 {
			logger.Info("Failed to find an IP for VirtualMachine")
			continue
		}
		// Ignore if all required values aren't present
		if vm.Status.Host == "" {
			logger.Info("Skipping VirtualMachine due to empty host")
			continue
		}

		// Ignore VMs that fail the readiness check (only when probes are specified)
		// If VM has Ready condition in the status and the value is true, we add the VM to the subset.
		// If this condition is missing but the probe spec is set, which may happen right after upgrade when it is first released,
		// we check the service's backing endpoints and determine whether this VM needs to be added to the subset.
		// If this VM isn't in the old endpoint subsets, we skip it.
		// Otherwise, we consider this VM as ready and add it to the subset.
		if vm.Spec.ReadinessProbe != nil {
			if condition := conditions.Get(&vm, vmoperatorv1alpha1.ReadyCondition); condition == nil {
				// Ready condition type is missing in VM status
				// Don't update endpoint subsets for this VM
				if !vmInSubsetsMapSet {
					vmInSubsetsMap, err = r.getVMsReferencedByServiceEndpoints(ctx, service)
					vmInSubsetsMapSet = err == nil
				}

				if _, ok := vmInSubsetsMap[vm.UID]; !ok {
					// This VM was not included in service's backing endpoints. We skip it.
					logger.Info("Skipping VirtualMachine due to missing Ready condition", "name", vm.NamespacedName())
					continue
				}
			} else if condition.Status != corev1.ConditionTrue {
				logger.Info("Skipping VirtualMachine due to failed readiness probe check", "probeError", condition.Message)
				continue
			}
		}

		epa := *r.makeEndpointAddress(&vm)

		// TODO: Headless support
		for _, servicePort := range service.Spec.Ports {
			portName := servicePort.Name
			portProto := servicePort.Protocol

			logger.V(5).Info("EndpointPort for VirtualMachine",
				"port name", portName, "port proto", portProto)

			portNum, err := findPort(&vm, servicePort.TargetPort, portProto)
			if err != nil {
				logger.Info("Failed to find port for service",
					"name", portName, "protocol", portProto, "error", err)
				continue
			}

			epp := &corev1.EndpointPort{Name: portName, Port: int32(portNum), Protocol: portProto}
			subsets = addEndpointSubset(subsets, epa, epp)
		}
	}

	return subsets, nil
}

// UpdateVmServiceStatus updates the VirtualMachineService status, syncs external IP for loadbalancer type of service. Also ensures that VirtualMachineService contains the VM operator annotations.
func (r *ReconcileVirtualMachineService) UpdateVmService(ctx goctx.Context, vmService *vmoperatorv1alpha1.VirtualMachineService, newService *corev1.Service) error {
	r.log.V(5).Info("Updating VirtualMachineService Status", "name", vmService.NamespacedName())
	defer r.log.V(5).Info("Finished updating VirtualMachineService Status", "name", vmService.NamespacedName())

	newVMService := vmService.DeepCopy()

	// Update the load balancer's external IP.
	if newVMService.Spec.Type == vmoperatorv1alpha1.VirtualMachineServiceTypeLoadBalancer {
		//copy service ingress array to vm service ingress array
		newVMService.Status.LoadBalancer.Ingress = make([]vmoperatorv1alpha1.LoadBalancerIngress, len(newService.Status.LoadBalancer.Ingress))
		for idx, ingress := range newService.Status.LoadBalancer.Ingress {
			vmIngress := vmoperatorv1alpha1.LoadBalancerIngress{
				IP:       ingress.IP,
				Hostname: ingress.Hostname,
			}
			newVMService.Status.LoadBalancer.Ingress[idx] = vmIngress
		}
	}

	// We need to compare Status subresource separately since updating the object will not update the subresource itself.
	if !apiequality.Semantic.DeepEqual(vmService.Status, newVMService.Status) {
		r.log.V(5).Info("Updating the vmService status", "vmService", newVMService)
		if err := r.Status().Update(ctx, newVMService); err != nil {
			r.log.Error(err, "Error in updating the vmService status", "vmService", newVMService)
			return err
		}
	}

	pkg.AddAnnotations(&newVMService.ObjectMeta)

	// Update the resource to reflect the Annotations.
	if !apiequality.Semantic.DeepEqual(vmService.ObjectMeta, newVMService.ObjectMeta) {
		r.log.V(5).Info("Updating the vmService resource", "vmService", newVMService)
		if err := r.Update(ctx, newVMService); err != nil {
			r.log.Error(err, "Error in updating vmService resource", "vmService", newVMService)
			return err
		}
	}

	return nil
}

func populateNodePorts(oldService *corev1.Service, targetService *corev1.Service) {
	nodePortMap := map[string]int32{}
	for _, port := range oldService.Spec.Ports {
		nodePortMap[port.Name] = port.NodePort
	}
	for i := 0; i < len(targetService.Spec.Ports); i++ {
		if val, ok := nodePortMap[targetService.Spec.Ports[i].Name]; ok {
			targetService.Spec.Ports[i].NodePort = val
		}
	}
}
