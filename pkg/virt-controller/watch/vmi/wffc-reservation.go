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
	"fmt"

	k8sv1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	virtv1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/log"

	"kubevirt.io/kubevirt/pkg/controller"
	"kubevirt.io/kubevirt/pkg/pointer"
	"kubevirt.io/kubevirt/pkg/virt-controller/services"
	"kubevirt.io/kubevirt/pkg/virt-controller/watch/common"
)

// syncWFFCHostReservation is the only place sync creates or releases host
// reservations. While vmi needs one (see
// services.TemplateService.NeedsWFFCHostReservation, which also checks that
// the namespace has turned host reservations on), it reserves the node of the
// scheduled temporary pod and keeps the reservation until the launcher is
// scheduled. In every other case, including a namespace that has turned them
// off, it releases any reservation vmi still has.
func (c *Controller) syncWFFCHostReservation(vmi *virtv1.VirtualMachineInstance, pod *k8sv1.Pod) common.SyncError {
	needed := vmi.DeletionTimestamp == nil && !vmi.IsFinal() && c.templateService.NeedsWFFCHostReservation(vmi)
	if needed && controller.PodExists(pod) && isTempPod(pod) {
		if err := c.ensureWFFCHostReservation(vmi, pod); err != nil {
			return common.NewSyncError(fmt.Errorf("failed to create host reservation pod: %v", err), controller.FailedCreatePodReason)
		}
		return nil
	}
	launcherScheduled := controller.PodExists(pod) && !isTempPod(pod) && pod.Spec.NodeName != ""
	if needed && !launcherScheduled {
		return nil
	}
	if err := c.deleteWFFCHostReservation(vmi); err != nil {
		return common.NewSyncError(fmt.Errorf("failed to delete host reservation pod: %v", err), controller.FailedDeletePodReason)
	}
	return nil
}

// WatchWFFCHostReservationOptIn re-syncs every VMI in a namespace whose
// services.WFFCHostReservationEnabledLabel changes. Turning reservations off
// then releases the namespace's reservations at once, and turning them on
// reserves the nodes of its scheduled temporary pods, instead of at each
// VMI's next event.
func (c *Controller) WatchWFFCHostReservationOptIn(namespaceInformer cache.SharedIndexInformer) error {
	_, err := namespaceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: c.updateNamespace,
	})
	return err
}

func (c *Controller) updateNamespace(oldObj, newObj interface{}) {
	oldNamespace, oldOK := oldObj.(*k8sv1.Namespace)
	newNamespace, newOK := newObj.(*k8sv1.Namespace)
	if !oldOK || !newOK ||
		oldNamespace.Labels[services.WFFCHostReservationEnabledLabel] == newNamespace.Labels[services.WFFCHostReservationEnabledLabel] {
		return
	}
	objs, err := c.vmiIndexer.ByIndex(cache.NamespaceIndex, newNamespace.Name)
	if err != nil {
		log.Log.Reason(err).Errorf("Failed to list the VMIs of namespace %s", newNamespace.Name)
		return
	}
	for _, obj := range objs {
		if vmi, ok := obj.(*virtv1.VirtualMachineInstance); ok {
			c.enqueueVirtualMachine(vmi)
		}
	}
}

// ensureWFFCHostReservation reserves the node of vmi's temporary pod as soon
// as the pod is scheduled, so the reservation is in place before the
// temporary pod is deleted. See services.RenderWFFCHostReservationPod.
func (c *Controller) ensureWFFCHostReservation(vmi *virtv1.VirtualMachineInstance, tempPod *k8sv1.Pod) error {
	if tempPod.Spec.NodeName == "" {
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

// enqueueWFFCHostReservationOwner wakes the VMI a host reservation pod
// belongs to and reports whether pod is a host reservation. The VMI is not the
// pod's controller, so the usual lookup misses it. Without this, a VMI that
// ended before its reservation reached the cache would keep the reservation
// until something else synced it.
func (c *Controller) enqueueWFFCHostReservationOwner(pod *k8sv1.Pod) bool {
	if _, isReservation := pod.Labels[services.WFFCHostReservationLabel]; !isReservation {
		return false
	}
	for i := range pod.OwnerReferences {
		if vmi := c.resolveControllerRef(pod.Namespace, &pod.OwnerReferences[i]); vmi != nil {
			c.enqueueVirtualMachine(vmi)
		}
	}
	return true
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
