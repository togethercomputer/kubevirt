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

package vmi

import (
	"context"

	k8sv1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	virtv1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/controller"
	"kubevirt.io/kubevirt/pkg/pointer"
	"kubevirt.io/kubevirt/pkg/virt-controller/services"
)

// ensureWFFCHostReservation reserves the node of vmi's temporary pod as soon
// as the pod is scheduled, so the reservation is in place before the
// temporary pod is deleted. See services.RenderWFFCHostReservationPod.
func (c *Controller) ensureWFFCHostReservation(vmi *virtv1.VirtualMachineInstance, tempPod *k8sv1.Pod) error {
	if tempPod.Spec.NodeName == "" || !services.NeedsWFFCHostReservation(vmi) {
		return nil
	}
	key := controller.NamespacedKey(vmi.Namespace, services.WFFCHostReservationPodName(vmi))
	if _, exists, err := c.podIndexer.GetByKey(key); err != nil || exists {
		return err
	}

	pod := c.templateService.RenderWFFCHostReservationPod(vmi, tempPod)
	_, err := c.clientset.CoreV1().Pods(vmi.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
	switch {
	case err == nil:
		c.recorder.Eventf(vmi, k8sv1.EventTypeNormal, controller.SuccessfulCreatePodReason,
			"Created host reservation pod %s on node %s", pod.Name, pod.Spec.NodeName)
		return nil
	case k8serrors.IsAlreadyExists(err):
		return nil
	case k8serrors.IsForbidden(err) || k8serrors.IsInvalid(err):
		// The reservation only improves placement. A policy that rejects the
		// pod must not hold up the VMI, so carry on without it.
		c.recorder.Eventf(vmi, k8sv1.EventTypeWarning, controller.FailedCreatePodReason,
			"Failed to create host reservation pod %s on node %s: %v", pod.Name, pod.Spec.NodeName, err)
		return nil
	default:
		return err
	}
}

// deleteWFFCHostReservation releases vmi's host reservation, if it has one.
func (c *Controller) deleteWFFCHostReservation(vmi *virtv1.VirtualMachineInstance) error {
	key := controller.NamespacedKey(vmi.Namespace, services.WFFCHostReservationPodName(vmi))
	obj, exists, err := c.podIndexer.GetByKey(key)
	if err != nil || !exists {
		return err
	}
	pod := obj.(*k8sv1.Pod)
	if pod.DeletionTimestamp != nil {
		return nil
	}

	err = c.clientset.CoreV1().Pods(pod.Namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{
		GracePeriodSeconds: pointer.P(int64(0)),
		Preconditions:      &metav1.Preconditions{UID: &pod.UID},
	})
	if k8serrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	c.recorder.Eventf(vmi, k8sv1.EventTypeNormal, controller.SuccessfulDeletePodReason,
		"Deleted host reservation pod %s", pod.Name)
	return nil
}
