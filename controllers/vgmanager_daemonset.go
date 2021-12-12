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

package controllers

import (
	lvmv1alpha1 "github.com/red-hat-storage/lvm-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	hostContainerPropagation = corev1.MountPropagationHostToContainer
	directoryHostPath        = corev1.HostPathDirectory
	hostPathTypefile         = corev1.HostPathFile

	LVMdVolName   = "lvmd-conf"
	UdevVolName   = "run-udev"
	DevDirVolName = "device-dir"

	VGManagerImage = "vg-manager:latest"

	LVMdConfPath = "/mnt/lvmd.conf"
	devDirPath   = "/dev"
	udevPath     = "/run/udev"

	// LVMDConfVolume is the corev1.Volume definition for the lso symlink host directory.
	// "/mnt/local-storage" is the default, but it can be controlled by env vars.
	// SymlinkMount is the corresponding mount
	LVMDConfVolume = corev1.Volume{
		Name: LVMdVolName,
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: LVMdConfPath,
				Type: &hostPathTypefile,
			},
		},
	}
	// LVMDConfVolMount is the corresponding mount for SymlinkHostDirVolume
	LVMDConfVolMount = corev1.VolumeMount{
		Name:             LVMdVolName,
		MountPath:        LVMdConfPath,
		MountPropagation: &hostContainerPropagation,
	}

	// DevHostDirVolume  is the corev1.Volume definition for the "/dev" bind mount used to
	// list block devices.
	// DevMount is the corresponding mount
	DevHostDirVolume = corev1.Volume{
		Name: DevDirVolName,
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: devDirPath,
				Type: &directoryHostPath,
			},
		},
	}
	// DevMount is the corresponding mount for DevHostDirVolume
	DevMount = corev1.VolumeMount{
		Name:             DevDirVolName,
		MountPath:        devDirPath,
		MountPropagation: &hostContainerPropagation,
	}

	// UDevHostDirVolume is the corev1.Volume definition for the
	// "/run/udev" host bind-mount. This helps lsblk give more accurate output.
	// UDevMount is the corresponding mount
	UDevHostDirVolume = corev1.Volume{
		Name: UdevVolName,
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{Path: udevPath},
		},
	}
	// UDevMount is the corresponding mount for UDevHostDirVolume
	UDevMount = corev1.VolumeMount{
		Name:             UdevVolName,
		MountPath:        udevPath,
		MountPropagation: &hostContainerPropagation,
	}
)

// newVGManagerDaemonset returns the desired vgmanager daemonset for a given LVMCluster
func newVGManagerDaemonset(lvmCluster lvmv1alpha1.LVMCluster) appsv1.DaemonSet {
	volumes := []corev1.Volume{LVMDConfVolume, DevHostDirVolume, UDevHostDirVolume}
	privileged := true
	containers := []corev1.Container{
		{
			Name:  VGManagerUnit,
			Image: VGManagerImage,
			SecurityContext: &corev1.SecurityContext{
				Privileged: &privileged,
			},
			Env: []corev1.EnvVar{
				{
					Name: "NODE_NAME",
					ValueFrom: &corev1.EnvVarSource{
						FieldRef: &corev1.ObjectFieldSelector{
							FieldPath: "spec.nodeName",
						},
					},
				},
				{
					Name: "WATCH_NAMESPACE",
					ValueFrom: &corev1.EnvVarSource{
						FieldRef: &corev1.ObjectFieldSelector{
							FieldPath: "metadata.namespace",
						},
					},
				},
				{
					Name: "POD_NAME",
					ValueFrom: &corev1.EnvVarSource{
						FieldRef: &corev1.ObjectFieldSelector{
							FieldPath: "metadata.name",
						},
					},
				},
			},
		},
	}
	labels := map[string]string{
		"app":                   VGManagerUnit,
		"topolvm.io/lvmcluster": lvmCluster.Name,
	}
	ds := appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      VGManagerUnit,
			Namespace: lvmCluster.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					Volumes:    volumes,
					Containers: containers,
					// to read /proc/1/mountinfo
					HostPID: true,
				},
			},
		},
	}

	return ds
}
