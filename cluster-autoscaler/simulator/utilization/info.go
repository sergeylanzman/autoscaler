/*
Copyright 2016 The Kubernetes Authors.

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

package utilization

import (
	"fmt"
	"time"

	"k8s.io/autoscaler/cluster-autoscaler/utils/drain"
	"k8s.io/autoscaler/cluster-autoscaler/utils/gpu"
	pod_util "k8s.io/autoscaler/cluster-autoscaler/utils/pod"

	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	schedulerframework "k8s.io/kubernetes/pkg/scheduler/framework"

	klog "k8s.io/klog/v2"
)

// Info contains utilization information for a node.
type Info struct {
	CpuUtil float64
	MemUtil float64
	GpuUtil float64
	// Resource name of highest utilization resource
	ResourceName apiv1.ResourceName
	// Max(CpuUtil, MemUtil) or GpuUtils
	Utilization float64
}

// Calculate calculates utilization of a node, defined as maximum of (cpu,
// memory) or gpu utilization based on if the node has GPU or not. Per resource
// utilization is the sum of requests for it divided by allocatable. It also
// returns the individual cpu, memory and gpu utilization.
func Calculate(nodeInfo *schedulerframework.NodeInfo, skipDaemonSetPods, skipMirrorPods bool, gpuLabel string, currentTime time.Time) (utilInfo Info, err error) {
	if gpu.NodeHasGpu(gpuLabel, nodeInfo.Node()) {
		gpuUtil, err := calculateUtilizationOfResource(nodeInfo, gpu.ResourceNvidiaGPU, skipDaemonSetPods, skipMirrorPods, currentTime)
		if err != nil {
			klog.V(3).Infof("node %s has unready GPU", nodeInfo.Node().Name)
			// Return 0 if GPU is unready. This will guarantee we can still scale down a node with unready GPU.
			return Info{GpuUtil: 0, ResourceName: gpu.ResourceNvidiaGPU, Utilization: 0}, nil
		}

		// Skips cpu and memory utilization calculation for node with GPU.
		return Info{GpuUtil: gpuUtil, ResourceName: gpu.ResourceNvidiaGPU, Utilization: gpuUtil}, nil
	}

	cpu, err := calculateUtilizationOfResource(nodeInfo, apiv1.ResourceCPU, skipDaemonSetPods, skipMirrorPods, currentTime)
	if err != nil {
		return Info{}, err
	}
	mem, err := calculateUtilizationOfResource(nodeInfo, apiv1.ResourceMemory, skipDaemonSetPods, skipMirrorPods, currentTime)
	if err != nil {
		return Info{}, err
	}

	utilization := Info{CpuUtil: cpu, MemUtil: mem}

	if cpu > mem {
		utilization.ResourceName = apiv1.ResourceCPU
		utilization.Utilization = cpu
	} else {
		utilization.ResourceName = apiv1.ResourceMemory
		utilization.Utilization = mem
	}

	return utilization, nil
}

func calculateUtilizationOfResource(nodeInfo *schedulerframework.NodeInfo, resourceName apiv1.ResourceName, skipDaemonSetPods, skipMirrorPods bool, currentTime time.Time) (float64, error) {
	nodeAllocatable, found := nodeInfo.Node().Status.Allocatable[resourceName]
	if !found {
		return 0, fmt.Errorf("failed to get %v from %s", resourceName, nodeInfo.Node().Name)
	}
	if nodeAllocatable.MilliValue() == 0 {
		return 0, fmt.Errorf("%v is 0 at %s", resourceName, nodeInfo.Node().Name)
	}
	podsRequest := resource.MustParse("0")

	// if skipDaemonSetPods = True, DaemonSet pods resourses will be subtracted
	// from the node allocatable and won't be added to pods requests
	// the same with the Mirror pod.
	daemonSetAndMirrorPodsUtilization := resource.MustParse("0")
	for _, podInfo := range nodeInfo.Pods {
		// factor daemonset pods out of the utilization calculations
		if skipDaemonSetPods && pod_util.IsDaemonSetPod(podInfo.Pod) {
			for _, container := range podInfo.Pod.Spec.Containers {
				if resourceValue, found := container.Resources.Requests[resourceName]; found {
					daemonSetAndMirrorPodsUtilization.Add(resourceValue)
				}
			}
			continue
		}
		// factor mirror pods out of the utilization calculations
		if skipMirrorPods && pod_util.IsMirrorPod(podInfo.Pod) {
			for _, container := range podInfo.Pod.Spec.Containers {
				if resourceValue, found := container.Resources.Requests[resourceName]; found {
					daemonSetAndMirrorPodsUtilization.Add(resourceValue)
				}
			}
			continue
		}
		// ignore Pods that should be terminated
		if drain.IsPodLongTerminating(podInfo.Pod, currentTime) {
			continue
		}
		for _, container := range podInfo.Pod.Spec.Containers {
			if resourceValue, found := container.Resources.Requests[resourceName]; found {
				podsRequest.Add(resourceValue)
			}
		}
	}
	return float64(podsRequest.MilliValue()) / float64(nodeAllocatable.MilliValue()-daemonSetAndMirrorPodsUtilization.MilliValue()), nil
}
