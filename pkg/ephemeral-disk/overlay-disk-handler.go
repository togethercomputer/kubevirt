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
 * Copyright 2025 Red Hat, Inc.
 *
 */

package ephemeraldisk

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	v1 "kubevirt.io/api/core/v1"

	diskutils "kubevirt.io/kubevirt/pkg/ephemeral-disk-utils"
	"kubevirt.io/kubevirt/pkg/util"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
)

const (
	overlayDiskDir = "/var/run/kubevirt-private/overlay-disks"
)

type OverlayDiskHandlerInterface interface {
	CreateOverlayImages(vmi *v1.VirtualMachineInstance, domain *api.Domain) error
	CleanupNonPersistentOverlays(vmi *v1.VirtualMachineInstance) error
	GetOverlayPath(volumeName string) string
}

type overlayDiskHandler struct {
	baseDir         string
	pvcBaseDir      string
	blockDevBaseDir string
	diskCreateFunc  func(backingFile string, backingFormat string, imagePath string) ([]byte, error)
}

func NewOverlayDiskHandler() *overlayDiskHandler {
	return &overlayDiskHandler{
		baseDir:         overlayDiskDir,
		pvcBaseDir:      ephemeralDiskPVCBaseDir,
		blockDevBaseDir: ephemeralDiskBlockDeviceBaseDir,
		diskCreateFunc:  createBackingDisk,
	}
}

func (h *overlayDiskHandler) GetOverlayPath(volumeName string) string {
	return filepath.Join(h.baseDir, volumeName, "disk.qcow2")
}

func (h *overlayDiskHandler) getTargetPath(overlay *v1.OverlayVolumeSource, volumeName string) string {
	if overlay.TargetPVC != nil {
		// For target PVC, the volume is mounted at /var/run/kubevirt-private/vmi-disks/target-<volumeName>
		targetVolumeName := fmt.Sprintf("target-%s", volumeName)
		return filepath.Join(h.pvcBaseDir, targetVolumeName, "overlay.qcow2")
	}

	if overlay.TargetHostPath != nil {
		// For target HostPath, create the file in the specified directory
		// Assume the path is a directory and append the filename
		return filepath.Join(overlay.TargetHostPath.Path, fmt.Sprintf("%s.qcow2", volumeName))
	}

	// Default: use ephemeral overlay disk location
	return h.GetOverlayPath(volumeName)
}

func (h *overlayDiskHandler) getBackingInfo(overlay *v1.OverlayVolumeSource, volumeName string, isBlockVolumes map[string]bool) (string, string) {
	// Determine backing format (default to raw if not specified)
	backingFormat := "raw"
	if overlay.BackingFormat != "" {
		backingFormat = overlay.BackingFormat
	}

	// Determine backing file path
	var backingFile string
	backingVolumeName := fmt.Sprintf("backing-%s", volumeName)

	if overlay.BackingPVC != nil {
		// For PVC backing, use the standard backing file path
		if isBlockVolumes[backingVolumeName] {
			backingFile = filepath.Join(h.blockDevBaseDir, backingVolumeName)
		} else {
			backingFile = filepath.Join(h.pvcBaseDir, backingVolumeName, "disk.img")
		}
	} else if overlay.BackingHostPath != nil {
		// For HostPath backing, use the direct path
		backingFile = overlay.BackingHostPath.Path
	}

	return backingFile, backingFormat
}

func (h *overlayDiskHandler) createOverlayImage(volumeName string, backingFile string, backingFormat string, targetPath string) error {
	// Create the directory for the overlay file
	overlayDir := filepath.Dir(targetPath)
	if err := util.MkdirAllWithNosec(overlayDir); err != nil {
		return err
	}

	// Check if overlay already exists (for persistent overlays that are being reused)
	if _, err := os.Stat(targetPath); err == nil {
		// Overlay exists - for persistent overlays this is expected on VM restart
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	// Create the overlay using qemu-img
	output, err := h.diskCreateFunc(backingFile, backingFormat, targetPath)
	if err != nil {
		return fmt.Errorf("qemu-img failed with output '%s': %v", string(output), err)
	}

	// Set proper permissions
	if err = os.Chmod(targetPath, 0640); err != nil {
		return fmt.Errorf("failed to change permissions on %s", targetPath)
	}

	// Ensure correct ownership
	err = diskutils.DefaultOwnershipManager.UnsafeSetFileOwnership(targetPath)
	return err
}

func (h *overlayDiskHandler) CreateOverlayImages(vmi *v1.VirtualMachineInstance, domain *api.Domain) error {
	isBlockVolumes := diskutils.GetEphemeralBackingSourceBlockDevices(domain)

	for _, volume := range vmi.Spec.Volumes {
		if volume.VolumeSource.Overlay != nil {
			overlay := volume.VolumeSource.Overlay

			// Get backing file information
			backingFile, backingFormat := h.getBackingInfo(overlay, volume.Name, isBlockVolumes)

			// Determine the target path for the overlay
			targetPath := h.getTargetPath(overlay, volume.Name)

			// Create the overlay image
			if err := h.createOverlayImage(volume.Name, backingFile, backingFormat, targetPath); err != nil {
				return fmt.Errorf("failed to create overlay for volume %s: %v", volume.Name, err)
			}
		}
	}

	return nil
}

func (h *overlayDiskHandler) CleanupNonPersistentOverlays(vmi *v1.VirtualMachineInstance) error {
	// Clean up overlay volumes that are marked as non-persistent
	// This should be called by virt-handler when a VMI is being deleted/stopped
	// Note: Due to validation, non-persistent overlays can only use the default location
	// (persistent=true requires a target, and target requires persistent=true)
	for _, volume := range vmi.Spec.Volumes {
		if volume.VolumeSource.Overlay != nil && !volume.VolumeSource.Overlay.Persistent {
			overlay := volume.VolumeSource.Overlay

			// Get the actual overlay path (should be default location for non-persistent)
			overlayPath := h.getTargetPath(overlay, volume.Name)

			// Check if overlay file exists before trying to delete
			if _, err := os.Stat(overlayPath); err == nil {
				// Remove the overlay file
				if err := os.Remove(overlayPath); err != nil {
					return fmt.Errorf("failed to remove non-persistent overlay %s: %v", overlayPath, err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("failed to stat overlay file %s: %v", overlayPath, err)
			}

			// Try to remove the directory if it's empty
			overlayDir := filepath.Dir(overlayPath)
			os.Remove(overlayDir) // Ignore error - directory might not be empty or already gone
		}
	}
	return nil
}
