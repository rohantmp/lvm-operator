/*
Copyright 2021.

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

package vgmanager

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/go-cmp/cmp"
	lvmv1alpha1 "github.com/red-hat-storage/lvm-operator/api/v1alpha1"
	"github.com/red-hat-storage/lvm-operator/pkg/internal"
	"github.com/topolvm/topolvm"
	"github.com/topolvm/topolvm/lvmd"
	lvmdCMD "github.com/topolvm/topolvm/pkg/lvmd/cmd"
	"gopkg.in/yaml.v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	corev1helper "k8s.io/component-helpers/scheduling/corev1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SetupWithManager sets up the controller with the Manager.
func (r *VGReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.deviceAgeMap = newAgeMap(&wallTime{})
	return ctrl.NewControllerManagedBy(mgr).
		For(&lvmv1alpha1.LVMCluster{}).
		Complete(r)
}

type VGReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	Log           logr.Logger
	ConfigChannel chan lvmdCMD.Config
	// map from KNAME of device to time when the device was first observed since the process started
	deviceAgeMap *ageMap
}

var ConfigFilePath = "/mnt/lvmd.yaml"

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the LVMCluster object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.10.0/pkg/reconcile
func (r *VGReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// logger

	lvmCluster := &lvmv1alpha1.LVMCluster{}
	err := r.Client.Get(ctx, req.NamespacedName, lvmCluster)
	if err != nil {
		// todo(rohan) log all errors
		return ctrl.Result{}, err
	}

	//  list block devices
	blockDevices, badRows, err := internal.ListBlockDevices()
	if err != nil {
		msg := fmt.Sprintf("failed to list block devices: %v", err)
		r.Log.Error(err, msg, "lsblk.BadRows", badRows)
		return ctrl.Result{}, err
	} else if len(badRows) > 0 {
		r.Log.Error(err, "could not parse all the lsblk rows", "lsblk.BadRows", badRows)
	}

	// load lvmd config
	// todo(rohan)
	lvmdConfig := &lvmdCMD.Config{
		SocketName: topolvm.DefaultLVMdSocket,
	}

	lvmdConfMissing := false

	cfgBytes, err := os.ReadFile(ConfigFilePath)
	if os.IsNotExist(err) {
		lvmdConfMissing = true
	} else if err != nil {
		return ctrl.Result{}, err
	}
	if !lvmdConfMissing {
		err = yaml.Unmarshal(cfgBytes, &lvmdConfig)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	oldLVMDConfig := *lvmdConfig

	// avoid having to iterate through device classes multiple times, map from name to config index
	deviceClassMap := make(map[string]int)
	for i, deviceClass := range lvmdConfig.DeviceClasses {
		deviceClassMap[deviceClass.Name] = i
	}

	// filter out block devices
	remainingValidDevices, delayedDevices, err := r.filterAvailableDevices(blockDevices)
	if err != nil {
		_ = err
	}

	for _, deviceClass := range lvmCluster.Spec.DeviceClasses {
		// set logger with classname

		// ignore deviceClasses whose LabelSelector doesn't match this node
		// NodeSelectorTerms.MatchExpressions are ORed
		node := &corev1.Node{}
		selectsNode, err := NodeSelectorMatchesNodeLabels(node, deviceClass.NodeSelector)
		if err != nil {
			r.Log.Error(err, "failed to match nodeSelector to node labels")
			continue
		}
		if !(selectsNode && ToleratesTaints(deviceClass.Tolerations, node.Spec.Taints)) {
			continue
		}
		_, found := deviceClassMap[deviceClass.Name]
		if !found {
			lvmdConfig.DeviceClasses = append(lvmdConfig.DeviceClasses, &lvmd.DeviceClass{
				Name:        deviceClass.Name,
				VolumeGroup: deviceClass.Name,
			})
		}
		var matchingDevices []internal.BlockDevice
		remainingValidDevices, matchingDevices, err = filterMatchingDevices(remainingValidDevices, deviceClass)
		if err != nil {
			r.Log.Error(err, "could not filterMatchingDevices")
			continue
		}

		// create/update VG and update lvmd confif
		err = addMatchingDevicesToVG(matchingDevices, lvmdConfig, "vgname")
		if err != nil {
			r.Log.Error(err, "could not prepareVGs and update lvmdConfig")
			continue
		}
	}

	// apply and save lvmconfig
	// pass config to configChannel only if config has changed
	if !cmp.Equal(oldLVMDConfig, lvmdConfig) {
		r.ConfigChannel <- *lvmdConfig
		out, err := yaml.Marshal(lvmdConfig)
		if err != nil {
			return ctrl.Result{}, err
		}
		err = os.WriteFile(ConfigFilePath, out, 0600)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	// requeue faster if some devices are too recently observed to consume
	requeueAfter := time.Minute * 2
	if len(delayedDevices) > 0 {
		requeueAfter = time.Second * 30
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// returns unmatched and matched blockdevices
func filterMatchingDevices(blockDevices []internal.BlockDevice, lvmCluster lvmv1alpha1.DeviceClass) ([]internal.BlockDevice, []internal.BlockDevice, error) {
	// currently just match all devices
	return []internal.BlockDevice{}, blockDevices, nil
}

func addMatchingDevicesToVG(matchingDevices []internal.BlockDevice, lvmdConfig *lvmdCMD.Config, vgName string) error {
	// todo(rohan)
	return nil
}

func NodeSelectorMatchesNodeLabels(node *corev1.Node, nodeSelector *corev1.NodeSelector) (bool, error) {
	if nodeSelector == nil {
		return true, nil
	}
	if node == nil {
		return false, fmt.Errorf("the node var is nil")
	}

	matches, err := corev1helper.MatchNodeSelectorTerms(node, nodeSelector)
	return matches, err
}

func ToleratesTaints(tolerations []corev1.Toleration, taints []corev1.Taint) bool {
	for _, t := range taints {
		taint := t
		toleratesTaint := corev1helper.TolerationsTolerateTaint(tolerations, &taint)
		if !toleratesTaint {
			return false
		}
	}
	return true
}
