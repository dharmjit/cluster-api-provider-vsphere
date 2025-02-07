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

package services

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/utils/integer"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	clusterutilv1 "sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	capvcontext "sigs.k8s.io/cluster-api-provider-vsphere/pkg/context"
	infrautilv1 "sigs.k8s.io/cluster-api-provider-vsphere/pkg/util"
)

type VimMachineService struct{}

func (v *VimMachineService) FetchVSphereMachine(c client.Client, name types.NamespacedName) (capvcontext.MachineContext, error) {
	vsphereMachine := &infrav1.VSphereMachine{}
	err := c.Get(context.Background(), name, vsphereMachine)

	return &capvcontext.VIMMachineContext{VSphereMachine: vsphereMachine}, err
}

func (v *VimMachineService) FetchVSphereCluster(c client.Client, cluster *clusterv1.Cluster, machineContext capvcontext.MachineContext) (capvcontext.MachineContext, error) {
	vimMachineCtx, ok := machineContext.(*capvcontext.VIMMachineContext)
	if !ok {
		return nil, errors.New("received unexpected VIMMachineContext type")
	}
	vsphereCluster := &infrav1.VSphereCluster{}
	vsphereClusterName := client.ObjectKey{
		Namespace: machineContext.GetObjectMeta().Namespace,
		Name:      cluster.Spec.InfrastructureRef.Name,
	}
	err := c.Get(context.Background(), vsphereClusterName, vsphereCluster)

	vimMachineCtx.VSphereCluster = vsphereCluster
	return vimMachineCtx, err
}

func (v *VimMachineService) ReconcileDelete(machineCtx capvcontext.MachineContext) error {
	vimMachineCtx, ok := machineCtx.(*capvcontext.VIMMachineContext)
	if !ok {
		return errors.New("received unexpected VIMMachineContext type")
	}

	vm, err := v.findVSphereVM(vimMachineCtx)
	// Attempt to find the associated VSphereVM resource.
	if err != nil {
		return err
	}

	if vm != nil && vm.GetDeletionTimestamp().IsZero() {
		// If the VSphereVM was found and it's not already enqueued for
		// deletion, go ahead and attempt to delete it.
		if err := vimMachineCtx.Client.Delete(vimMachineCtx, vm); err != nil {
			return err
		}
	}

	// VSphereMachine wraps a VMSphereVM, so we are mirroring status from the underlying VMSphereVM
	// in order to provide evidences about machine deletion.
	conditions.SetMirror(vimMachineCtx.VSphereMachine, infrav1.VMProvisionedCondition, vm)
	return nil
}

func (v *VimMachineService) SyncFailureReason(machineCtx capvcontext.MachineContext) (bool, error) {
	vimMachineCtx, ok := machineCtx.(*capvcontext.VIMMachineContext)
	if !ok {
		return false, errors.New("received unexpected VIMMachineContext type")
	}

	vsphereVM, err := v.findVSphereVM(vimMachineCtx)
	if err != nil {
		return false, err
	}
	if vsphereVM != nil {
		// Reconcile VSphereMachine's failures
		vimMachineCtx.VSphereMachine.Status.FailureReason = vsphereVM.Status.FailureReason
		vimMachineCtx.VSphereMachine.Status.FailureMessage = vsphereVM.Status.FailureMessage
	}

	return vimMachineCtx.VSphereMachine.Status.FailureReason != nil || vimMachineCtx.VSphereMachine.Status.FailureMessage != nil, err
}

func (v *VimMachineService) ReconcileNormal(machineCtx capvcontext.MachineContext) (bool, error) {
	vimMachineCtx, ok := machineCtx.(*capvcontext.VIMMachineContext)
	if !ok {
		return false, errors.New("received unexpected VIMMachineContext type")
	}
	vsphereVM, err := v.findVSphereVM(vimMachineCtx)
	if err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}

	vm, err := v.createOrPatchVSphereVM(vimMachineCtx, vsphereVM)
	if err != nil {
		vimMachineCtx.Logger.Error(err, "error creating or patching VM", "vsphereVM", vsphereVM)
		return false, err
	}

	// Convert the VM resource to unstructured data.
	vmData, err := runtime.DefaultUnstructuredConverter.ToUnstructured(vm)
	if err != nil {
		return false, errors.Wrapf(err,
			"failed to convert %s to unstructured data",
			vm.GetObjectKind().GroupVersionKind().String())
	}
	vmObj := &unstructured.Unstructured{Object: vmData}
	vmObj.SetGroupVersionKind(vm.GetObjectKind().GroupVersionKind())
	vmObj.SetAPIVersion(vm.GetObjectKind().GroupVersionKind().GroupVersion().String())
	vmObj.SetKind(vm.GetObjectKind().GroupVersionKind().Kind)

	// Waits the VM's ready state.
	if ok, err := v.waitReadyState(vimMachineCtx, vmObj); !ok {
		if err != nil {
			return false, errors.Wrapf(err, "unexpected error while reconciling ready state for %s", vimMachineCtx)
		}
		vimMachineCtx.Logger.Info("waiting for ready state")
		// VSphereMachine wraps a VMSphereVM, so we are mirroring status from the underlying VMSphereVM
		// in order to provide evidences about machine provisioning while provisioning is actually happening.
		conditions.SetMirror(vimMachineCtx.VSphereMachine, infrav1.VMProvisionedCondition, conditions.UnstructuredGetter(vmObj))
		return true, nil
	}

	// Reconcile the VSphereMachine's provider ID using the VM's BIOS UUID.
	if ok, err := v.reconcileProviderID(vimMachineCtx, vmObj); !ok {
		if err != nil {
			return false, errors.Wrapf(err, "unexpected error while reconciling provider ID for %s", vimMachineCtx)
		}
		vimMachineCtx.Logger.Info("provider ID is not reconciled")
		return true, nil
	}

	// Reconcile the VSphereMachine's node addresses from the VM's IP addresses.
	if ok, err := v.reconcileNetwork(vimMachineCtx, vmObj); !ok {
		if err != nil {
			return false, errors.Wrapf(err, "unexpected error while reconciling network for %s", vimMachineCtx)
		}
		vimMachineCtx.Logger.Info("network is not reconciled")
		conditions.MarkFalse(vimMachineCtx.VSphereMachine, infrav1.VMProvisionedCondition, infrav1.WaitingForNetworkAddressesReason, clusterv1.ConditionSeverityInfo, "")
		return true, nil
	}

	vimMachineCtx.VSphereMachine.Status.Ready = true
	return false, nil
}

func (v *VimMachineService) GetHostInfo(c capvcontext.MachineContext) (string, error) {
	vimMachineCtx, ok := c.(*capvcontext.VIMMachineContext)
	if !ok {
		return "", errors.New("received unexpected VIMMachineContext type")
	}

	vsphereVM := &infrav1.VSphereVM{}
	if err := vimMachineCtx.Client.Get(vimMachineCtx, client.ObjectKey{
		Namespace: vimMachineCtx.VSphereMachine.Namespace,
		Name:      generateVMObjectName(vimMachineCtx, vimMachineCtx.Machine.Name),
	}, vsphereVM); err != nil {
		return "", err
	}

	if conditions.IsTrue(vsphereVM, infrav1.VMProvisionedCondition) {
		return vsphereVM.Status.Host, nil
	}
	vimMachineCtx.Logger.V(4).Info("VMProvisionedCondition is set to false", "vsphereVM", vsphereVM.Name)
	return "", nil
}

func (v *VimMachineService) findVSphereVM(vimMachineCtx *capvcontext.VIMMachineContext) (*infrav1.VSphereVM, error) {
	// Get ready to find the associated VSphereVM resource.
	vm := &infrav1.VSphereVM{}
	vmKey := types.NamespacedName{
		Namespace: vimMachineCtx.VSphereMachine.Namespace,
		Name:      generateVMObjectName(vimMachineCtx, vimMachineCtx.Machine.Name),
	}
	// Attempt to find the associated VSphereVM resource.
	if err := vimMachineCtx.Client.Get(vimMachineCtx, vmKey, vm); err != nil {
		return nil, err
	}
	return vm, nil
}

func (v *VimMachineService) waitReadyState(vimMachineCtx *capvcontext.VIMMachineContext, vm *unstructured.Unstructured) (bool, error) {
	ready, ok, err := unstructured.NestedBool(vm.Object, "status", "ready")
	if !ok {
		if err != nil {
			return false, errors.Wrapf(err,
				"unexpected error when getting status.ready from %s %s/%s for %s",
				vm.GroupVersionKind(),
				vm.GetNamespace(),
				vm.GetName(),
				vimMachineCtx)
		}
		vimMachineCtx.Logger.Info("status.ready not found",
			"vmGVK", vm.GroupVersionKind().String(),
			"vmNamespace", vm.GetNamespace(),
			"vmName", vm.GetName())
		return false, nil
	}
	if !ready {
		vimMachineCtx.Logger.Info("status.ready is false",
			"vmGVK", vm.GroupVersionKind().String(),
			"vmNamespace", vm.GetNamespace(),
			"vmName", vm.GetName())
		return false, nil
	}

	return true, nil
}

func (v *VimMachineService) reconcileProviderID(vimMachineCtx *capvcontext.VIMMachineContext, vm *unstructured.Unstructured) (bool, error) {
	biosUUID, ok, err := unstructured.NestedString(vm.Object, "spec", "biosUUID")
	if !ok {
		if err != nil {
			return false, errors.Wrapf(err,
				"unexpected error when getting spec.biosUUID from %s %s/%s for %s",
				vm.GroupVersionKind(),
				vm.GetNamespace(),
				vm.GetName(),
				vimMachineCtx)
		}
		vimMachineCtx.Logger.Info("spec.biosUUID not found",
			"vmGVK", vm.GroupVersionKind().String(),
			"vmNamespace", vm.GetNamespace(),
			"vmName", vm.GetName())
		return false, nil
	}
	if biosUUID == "" {
		vimMachineCtx.Logger.Info("spec.biosUUID is empty",
			"vmGVK", vm.GroupVersionKind().String(),
			"vmNamespace", vm.GetNamespace(),
			"vmName", vm.GetName())
		return false, nil
	}

	providerID := infrautilv1.ConvertUUIDToProviderID(biosUUID)
	if providerID == "" {
		return false, errors.Errorf("invalid BIOS UUID %s from %s %s/%s for %s",
			biosUUID,
			vm.GroupVersionKind(),
			vm.GetNamespace(),
			vm.GetName(),
			vimMachineCtx)
	}
	if vimMachineCtx.VSphereMachine.Spec.ProviderID == nil || *vimMachineCtx.VSphereMachine.Spec.ProviderID != providerID {
		vimMachineCtx.VSphereMachine.Spec.ProviderID = &providerID
		vimMachineCtx.Logger.Info("updated provider ID", "provider-id", providerID)
	}

	return true, nil
}

//nolint:nestif
func (v *VimMachineService) reconcileNetwork(vimMachineCtx *capvcontext.VIMMachineContext, vm *unstructured.Unstructured) (bool, error) {
	var errs []error
	if networkStatusListOfIfaces, ok, _ := unstructured.NestedSlice(vm.Object, "status", "network"); ok {
		var networkStatusList []infrav1.NetworkStatus
		for i, networkStatusListMemberIface := range networkStatusListOfIfaces {
			if buf, err := json.Marshal(networkStatusListMemberIface); err != nil {
				vimMachineCtx.Logger.Error(err,
					"unsupported data for member of status.network list",
					"index", i)
				errs = append(errs, err)
			} else {
				var networkStatus infrav1.NetworkStatus
				err := json.Unmarshal(buf, &networkStatus)
				if err == nil && networkStatus.MACAddr == "" {
					err = errors.New("macAddr is required")
					errs = append(errs, err)
				}
				if err != nil {
					vimMachineCtx.Logger.Error(err,
						"unsupported data for member of status.network list",
						"index", i, "data", string(buf))
					errs = append(errs, err)
				} else {
					networkStatusList = append(networkStatusList, networkStatus)
				}
			}
		}
		vimMachineCtx.VSphereMachine.Status.Network = networkStatusList
	}

	if addresses, ok, _ := unstructured.NestedStringSlice(vm.Object, "status", "addresses"); ok {
		var machineAddresses []clusterv1.MachineAddress
		for _, addr := range addresses {
			machineAddresses = append(machineAddresses, clusterv1.MachineAddress{
				Type:    clusterv1.MachineExternalIP,
				Address: addr,
			})
		}
		vimMachineCtx.VSphereMachine.Status.Addresses = machineAddresses
	}

	if len(vimMachineCtx.VSphereMachine.Status.Addresses) == 0 {
		vimMachineCtx.Logger.Info("waiting on IP addresses")
		return false, kerrors.NewAggregate(errs)
	}

	return true, nil
}

func (v *VimMachineService) createOrPatchVSphereVM(vimMachineCtx *capvcontext.VIMMachineContext, vsphereVM *infrav1.VSphereVM) (runtime.Object, error) {
	// Create or update the VSphereVM resource.
	vm := &infrav1.VSphereVM{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: vimMachineCtx.VSphereMachine.Namespace,
			Name:      generateVMObjectName(vimMachineCtx, vimMachineCtx.Machine.Name),
		},
	}
	mutateFn := func() (err error) {
		// Ensure the VSphereMachine is marked as an owner of the VSphereVM.
		vm.SetOwnerReferences(clusterutilv1.EnsureOwnerRef(
			vm.OwnerReferences,
			metav1.OwnerReference{
				APIVersion: vimMachineCtx.VSphereMachine.APIVersion,
				Kind:       vimMachineCtx.VSphereMachine.Kind,
				Name:       vimMachineCtx.VSphereMachine.Name,
				UID:        vimMachineCtx.VSphereMachine.UID,
			}))

		// Instruct the VSphereVM to use the CAPI bootstrap data resource.
		// TODO: BootstrapRef field should be replaced with BootstrapSecret of type string
		vm.Spec.BootstrapRef = &corev1.ObjectReference{
			APIVersion: "v1",
			Kind:       "Secret",
			Name:       *vimMachineCtx.Machine.Spec.Bootstrap.DataSecretName,
			Namespace:  vimMachineCtx.Machine.ObjectMeta.Namespace,
		}

		// Initialize the VSphereVM's labels map if it is nil.
		if vm.Labels == nil {
			vm.Labels = map[string]string{}
		}

		// Ensure the VSphereVM has a label that can be used when searching for
		// resources associated with the target cluster.
		vm.Labels[clusterv1.ClusterNameLabel] = vimMachineCtx.Machine.Labels[clusterv1.ClusterNameLabel]

		// For convenience, add a label that makes it easy to figure out if the
		// VSphereVM resource is part of some control plane.
		if val, ok := vimMachineCtx.Machine.Labels[clusterv1.MachineControlPlaneLabel]; ok {
			vm.Labels[clusterv1.MachineControlPlaneLabel] = val
		}

		// Copy the VSphereMachine's VM clone spec into the VSphereVM's
		// clone spec.
		vimMachineCtx.VSphereMachine.Spec.VirtualMachineCloneSpec.DeepCopyInto(&vm.Spec.VirtualMachineCloneSpec)

		// If Failure Domain is present on CAPI machine, use that to override the vm clone spec.
		if overrideFunc, ok := v.generateOverrideFunc(vimMachineCtx); ok {
			overrideFunc(vm)
		}

		// Several of the VSphereVM's clone spec properties can be derived
		// from multiple places. The order is:
		//
		//   1. From the Machine.Spec.FailureDomain
		//   2. From the VSphereMachine.Spec (the DeepCopyInto above)
		//   3. From the VSphereCluster.Spec
		if vm.Spec.Server == "" {
			vm.Spec.Server = vimMachineCtx.VSphereCluster.Spec.Server
		}
		if vm.Spec.Thumbprint == "" {
			vm.Spec.Thumbprint = vimMachineCtx.VSphereCluster.Spec.Thumbprint
		}
		if vsphereVM != nil {
			vm.Spec.BiosUUID = vsphereVM.Spec.BiosUUID
		}
		vm.Spec.PowerOffMode = vimMachineCtx.VSphereMachine.Spec.PowerOffMode
		vm.Spec.GuestSoftPowerOffTimeout = vimMachineCtx.VSphereMachine.Spec.GuestSoftPowerOffTimeout
		return nil
	}

	vmKey := types.NamespacedName{
		Namespace: vm.Namespace,
		Name:      vm.Name,
	}
	result, err := ctrlutil.CreateOrPatch(vimMachineCtx, vimMachineCtx.Client, vm, mutateFn)
	if err != nil {
		vimMachineCtx.Logger.Error(
			err,
			"failed to CreateOrPatch VSphereVM",
			"namespace",
			vm.Namespace,
			"name",
			vm.Name,
		)
		return nil, err
	}
	switch result {
	case ctrlutil.OperationResultNone:
		vimMachineCtx.Logger.Info(
			"no update required for vm",
			"vm",
			vmKey,
		)
	case ctrlutil.OperationResultCreated:
		vimMachineCtx.Logger.Info(
			"created vm",
			"vm",
			vmKey,
		)
	case ctrlutil.OperationResultUpdated:
		vimMachineCtx.Logger.Info(
			"updated vm",
			"vm",
			vmKey,
		)
	case ctrlutil.OperationResultUpdatedStatus:
		vimMachineCtx.Logger.Info(
			"updated vm and vm status",
			"vm",
			vmKey,
		)
	case ctrlutil.OperationResultUpdatedStatusOnly:
		vimMachineCtx.Logger.Info(
			"updated vm status",
			"vm",
			vmKey,
		)
	}

	return vm, nil
}

// generateVMObjectName returns a new VM object name in specific cases, otherwise return the same
// passed in the parameter.
func generateVMObjectName(vimMachineCtx *capvcontext.VIMMachineContext, machineName string) string {
	// Windows VM names must have 15 characters length at max.
	if vimMachineCtx.VSphereMachine.Spec.OS == infrav1.Windows && len(machineName) > 15 {
		return strings.TrimSuffix(machineName[0:9], "-") + "-" + machineName[len(machineName)-5:]
	}
	return machineName
}

// generateOverrideFunc returns a function which can override the values in the VSphereVM Spec
// with the values from the FailureDomain (if any) set on the owner CAPI machine.
//
//nolint:nestif
func (v *VimMachineService) generateOverrideFunc(vimMachineCtx *capvcontext.VIMMachineContext) (func(vm *infrav1.VSphereVM), bool) {
	failureDomainName := vimMachineCtx.Machine.Spec.FailureDomain
	if failureDomainName == nil {
		return nil, false
	}

	// Use the failureDomain name to fetch the vSphereDeploymentZone object
	var vsphereDeploymentZone infrav1.VSphereDeploymentZone
	if err := vimMachineCtx.Client.Get(vimMachineCtx, client.ObjectKey{Name: *failureDomainName}, &vsphereDeploymentZone); err != nil {
		vimMachineCtx.Logger.Error(err, "unable to fetch vsphere deployment zone", "name", *failureDomainName)
		return nil, false
	}

	var vsphereFailureDomain infrav1.VSphereFailureDomain
	if err := vimMachineCtx.Client.Get(vimMachineCtx, client.ObjectKey{Name: vsphereDeploymentZone.Spec.FailureDomain}, &vsphereFailureDomain); err != nil {
		vimMachineCtx.Logger.Error(err, "unable to fetch failure domain", "name", vsphereDeploymentZone.Spec.FailureDomain)
		return nil, false
	}

	overrideWithFailureDomainFunc := func(vm *infrav1.VSphereVM) {
		vm.Spec.Server = vsphereDeploymentZone.Spec.Server
		vm.Spec.Datacenter = vsphereFailureDomain.Spec.Topology.Datacenter
		if vsphereDeploymentZone.Spec.PlacementConstraint.Folder != "" {
			vm.Spec.Folder = vsphereDeploymentZone.Spec.PlacementConstraint.Folder
		}
		if vsphereDeploymentZone.Spec.PlacementConstraint.ResourcePool != "" {
			vm.Spec.ResourcePool = vsphereDeploymentZone.Spec.PlacementConstraint.ResourcePool
		}
		if vsphereFailureDomain.Spec.Topology.Datastore != "" {
			vm.Spec.Datastore = vsphereFailureDomain.Spec.Topology.Datastore
		}
		if len(vsphereFailureDomain.Spec.Topology.Networks) > 0 {
			vm.Spec.Network.Devices = overrideNetworkDeviceSpecs(vm.Spec.Network.Devices, vsphereFailureDomain.Spec.Topology.Networks)
		}
	}
	return overrideWithFailureDomainFunc, true
}

// overrideNetworkDeviceSpecs updates the network devices with the network definitions from the PlacementConstraint.
// The substitution is done based on the order in which the network devices have been defined.
//
// In case there are more network definitions than the number of network devices specified, the definitions are appended to the list.
func overrideNetworkDeviceSpecs(deviceSpecs []infrav1.NetworkDeviceSpec, networks []string) []infrav1.NetworkDeviceSpec {
	index, length := 0, len(networks)

	devices := make([]infrav1.NetworkDeviceSpec, 0, integer.IntMax(length, len(deviceSpecs)))
	// override the networks on the VM spec with placement constraint network definitions
	for i := range deviceSpecs {
		vmNetworkDeviceSpec := deviceSpecs[i]
		if i < length {
			index++
			vmNetworkDeviceSpec.NetworkName = networks[i]
		}
		devices = append(devices, vmNetworkDeviceSpec)
	}
	// append the remaining network definitions to the VM spec
	for ; index < length; index++ {
		devices = append(devices, infrav1.NetworkDeviceSpec{
			NetworkName: networks[index],
		})
	}

	return devices
}
