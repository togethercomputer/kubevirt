/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package services

import (
	k8sv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/pointer"
	"kubevirt.io/kubevirt/pkg/util"
)

// A WaitForFirstConsumer volume is bound to the node where the VMI's
// temporary pod lands. The temporary pod then has to go before CDI can
// populate the volume, and its requests go with it. Until the launcher is
// scheduled, the node looks free, so the next VMI's temporary pod can land
// there and bind its volumes to the same node, although only one of the VMIs
// fits. A host reservation pod stands in for the temporary pod until then,
// and temporary pods are kept off nodes that hold another VMI's reservation.
const (
	// WFFCHostReservationLabel marks a host reservation pod. The value is the
	// UID of the VMI the node is reserved for.
	WFFCHostReservationLabel = "kubevirt.io/wffc-host-reservation"

	wffcHostReservationApp       = "wffc-host-reservation"
	wffcHostReservationPodPrefix = "virt-wffc-reservation-"
)

// NeedsWFFCHostReservation reports whether the node that vmi's
// WaitForFirstConsumer volumes are bound to must stay reserved until the
// launcher is scheduled. Only VMIs with GPUs or host devices need it: those
// devices are what the next temporary pod would find free.
func NeedsWFFCHostReservation(vmi *v1.VirtualMachineInstance) bool {
	devices := vmi.Spec.Domain.Devices
	return len(devices.GPUs) > 0 || len(devices.HostDevices) > 0
}

// WFFCHostReservationPodName returns the name of vmi's host reservation pod.
// The name is fixed so that creating the pod twice fails with AlreadyExists.
func WFFCHostReservationPodName(vmi *v1.VirtualMachineInstance) string {
	return wffcHostReservationPodPrefix + string(vmi.UID)
}

// RenderWFFCHostReservationPod renders the pod that reserves tempPod's node
// for vmi.
//
// The pod is bound to the node directly, so it never queues behind other
// pods. It mounts no volumes, so CDI does not count it as a user of the
// claims. It requests nothing: it reserves the node through the
// anti-affinity of other temporary pods, and it is still there when the
// launcher is scheduled, so any request could keep the launcher off a node
// sized to the VM. The VMI owns it but is not its controller, so the VMI
// controller never takes it for a launcher, and the garbage collector
// removes it with the VMI.
func (t *TemplateService) RenderWFFCHostReservationPod(vmi *v1.VirtualMachineInstance, tempPod *k8sv1.Pod) *k8sv1.Pod {
	tempPodSpec := tempPod.Spec.DeepCopy()
	image := t.launcherImage
	for _, container := range tempPodSpec.Containers {
		if container.Name == "compute" {
			// The node already has this image: the temporary pod ran it.
			image = container.Image
		}
	}

	return &k8sv1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      WFFCHostReservationPodName(vmi),
			Namespace: vmi.Namespace,
			Labels: map[string]string{
				v1.AppLabel:              wffcHostReservationApp,
				WFFCHostReservationLabel: string(vmi.UID),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: v1.VirtualMachineInstanceGroupVersionKind.GroupVersion().String(),
				Kind:       v1.VirtualMachineInstanceGroupVersionKind.Kind,
				Name:       vmi.Name,
				UID:        vmi.UID,
			}},
		},
		Spec: k8sv1.PodSpec{
			NodeName:                      tempPodSpec.NodeName,
			Tolerations:                   tempPodSpec.Tolerations,
			PriorityClassName:             tempPodSpec.PriorityClassName,
			ImagePullSecrets:              tempPodSpec.ImagePullSecrets,
			RestartPolicy:                 k8sv1.RestartPolicyAlways,
			TerminationGracePeriodSeconds: pointer.P(int64(0)),
			AutomountServiceAccountToken:  pointer.P(false),
			EnableServiceLinks:            pointer.P(false),
			SecurityContext: &k8sv1.PodSecurityContext{
				RunAsNonRoot:   pointer.P(true),
				RunAsUser:      pointer.P(int64(util.NonRootUID)),
				SeccompProfile: &k8sv1.SeccompProfile{Type: k8sv1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []k8sv1.Container{{
				Name:            "reservation",
				Image:           image,
				ImagePullPolicy: k8sv1.PullIfNotPresent,
				Command:         []string{"/usr/bin/sleep", "infinity"},
				SecurityContext: &k8sv1.SecurityContext{
					AllowPrivilegeEscalation: pointer.P(false),
					Capabilities:             &k8sv1.Capabilities{Drop: []k8sv1.Capability{"ALL"}},
				},
			}},
		},
	}
}

// addWFFCHostReservationAntiAffinity keeps a temporary pod off every node
// that holds another VMI's host reservation, in any namespace. The VMI's own
// reservation does not repel its temporary pod.
func addWFFCHostReservationAntiAffinity(vmi *v1.VirtualMachineInstance, pod *k8sv1.Pod) {
	term := k8sv1.PodAffinityTerm{
		LabelSelector: &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: WFFCHostReservationLabel, Operator: metav1.LabelSelectorOpExists},
				{Key: WFFCHostReservationLabel, Operator: metav1.LabelSelectorOpNotIn, Values: []string{string(vmi.UID)}},
			},
		},
		NamespaceSelector: &metav1.LabelSelector{},
		TopologyKey:       k8sv1.LabelHostname,
	}
	if pod.Spec.Affinity == nil {
		pod.Spec.Affinity = &k8sv1.Affinity{}
	}
	if pod.Spec.Affinity.PodAntiAffinity == nil {
		pod.Spec.Affinity.PodAntiAffinity = &k8sv1.PodAntiAffinity{}
	}
	antiAffinity := pod.Spec.Affinity.PodAntiAffinity
	antiAffinity.RequiredDuringSchedulingIgnoredDuringExecution = append(
		antiAffinity.RequiredDuringSchedulingIgnoredDuringExecution, term)
}
