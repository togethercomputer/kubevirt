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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	k8sv1 "k8s.io/api/core/v1"

	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/kubevirt/pkg/libvmi"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
)

var _ = Describe("OverlayDiskHandler", func() {
	var overlayBaseDir string
	var pvcBaseDir string
	var blockDevBaseDir string
	var targetHostPathDir string
	var handler *overlayDiskHandler

	createBackingImageForPVC := func(volumeName string, isBlock bool) error {
		backingVolumeName := fmt.Sprintf("backing-%s", volumeName)
		if err := os.Mkdir(filepath.Join(pvcBaseDir, backingVolumeName), 0755); err != nil {
			return err
		}
		var backingPath string
		if isBlock {
			backingPath = filepath.Join(blockDevBaseDir, backingVolumeName)
		} else {
			backingPath = filepath.Join(pvcBaseDir, backingVolumeName, "disk.img")
		}
		f, err := os.Create(backingPath)
		if err != nil {
			return err
		}
		defer f.Close()
		return nil
	}

	createBackingImageForHostPath := func(hostPath string) error {
		dir := filepath.Dir(hostPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
		f, err := os.Create(hostPath)
		if err != nil {
			return err
		}
		defer f.Close()
		return nil
	}

	BeforeEach(func() {
		overlayBaseDir = GinkgoT().TempDir()
		pvcBaseDir = GinkgoT().TempDir()
		blockDevBaseDir = GinkgoT().TempDir()
		targetHostPathDir = GinkgoT().TempDir()

		handler = &overlayDiskHandler{
			baseDir:         overlayBaseDir,
			pvcBaseDir:      pvcBaseDir,
			blockDevBaseDir: blockDevBaseDir,
			diskCreateFunc:  fakeCreateOverlayDisk,
		}
	})

	Describe("overlay volume with PVC backing", func() {
		Context("With single overlay volume and default target", func() {
			It("Should create overlay image in default location", func() {
				By("Creating a minimal VMI with overlay volume backed by PVC")
				vmi := libvmi.New()
				vmi.Spec.Volumes = []v1.Volume{
					{
						Name: "overlay-disk",
						VolumeSource: v1.VolumeSource{
							Overlay: &v1.OverlayVolumeSource{
								BackingPVC: &v1.PersistentVolumeClaimVolumeSource{
									PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{
										ClaimName: "backing-pvc",
									},
								},
								BackingFormat: "raw",
								Persistent:    false,
							},
						},
					},
				}

				By("Creating a backing image for the PVC")
				Expect(createBackingImageForPVC("overlay-disk", false)).To(Succeed())

				By("Creating overlay image")
				err := handler.CreateOverlayImages(vmi, &api.Domain{})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying overlay exists in default location")
				expectedPath := filepath.Join(overlayBaseDir, "overlay-disk", "disk.qcow2")
				_, err = os.Stat(expectedPath)
				Expect(err).NotTo(HaveOccurred())
			})
		})

		Context("With overlay volume and TargetPVC", func() {
			It("Should create overlay image in target PVC location", func() {
				By("Creating a minimal VMI with overlay volume and target PVC")
				vmi := libvmi.New()
				vmi.Spec.Volumes = []v1.Volume{
					{
						Name: "overlay-disk",
						VolumeSource: v1.VolumeSource{
							Overlay: &v1.OverlayVolumeSource{
								BackingPVC: &v1.PersistentVolumeClaimVolumeSource{
									PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{
										ClaimName: "backing-pvc",
									},
								},
								TargetPVC: &v1.PersistentVolumeClaimVolumeSource{
									PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{
										ClaimName: "target-pvc",
									},
								},
								BackingFormat: "raw",
								Persistent:    true,
							},
						},
					},
				}

				By("Creating a backing image for the PVC")
				Expect(createBackingImageForPVC("overlay-disk", false)).To(Succeed())

				By("Creating overlay image")
				err := handler.CreateOverlayImages(vmi, &api.Domain{})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying overlay exists in target PVC location")
				expectedPath := filepath.Join(pvcBaseDir, "target-overlay-disk", "overlay.qcow2")
				_, err = os.Stat(expectedPath)
				Expect(err).NotTo(HaveOccurred())
			})
		})

		Context("With block PVC backing", func() {
			It("Should create overlay with block backing source", func() {
				By("Creating a minimal VMI with overlay volume backed by block PVC")
				vmi := libvmi.New()
				vmi.Spec.Volumes = []v1.Volume{
					{
						Name: "overlay-disk",
						VolumeSource: v1.VolumeSource{
							Overlay: &v1.OverlayVolumeSource{
								BackingPVC: &v1.PersistentVolumeClaimVolumeSource{
									PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{
										ClaimName: "backing-pvc",
									},
								},
								BackingFormat: "raw",
							},
						},
					},
				}

				By("Creating a backing block device for the PVC")
				Expect(createBackingImageForPVC("overlay-disk", true)).To(Succeed())

				By("Creating overlay image with block backing")
				err := handler.CreateOverlayImages(vmi, &api.Domain{
					Spec: api.DomainSpec{
						Devices: api.Devices{
							Disks: []api.Disk{
								{
									BackingStore: &api.BackingStore{
										Type: "block",
										Source: &api.DiskSource{
											Dev:  filepath.Join(blockDevBaseDir, "backing-overlay-disk"),
											Name: "backing-overlay-disk",
										},
									},
								},
							},
						},
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying overlay exists")
				expectedPath := filepath.Join(overlayBaseDir, "overlay-disk", "disk.qcow2")
				_, err = os.Stat(expectedPath)
				Expect(err).NotTo(HaveOccurred())
			})
		})
	})

	Describe("overlay volume with HostPath backing", func() {
		Context("With HostPath backing and default target", func() {
			It("Should create overlay image with HostPath backing", func() {
				backingPath := filepath.Join(GinkgoT().TempDir(), "base-image.img")
				By("Creating a backing image at HostPath")
				Expect(createBackingImageForHostPath(backingPath)).To(Succeed())

				By("Creating a minimal VMI with overlay volume backed by HostPath")
				vmi := libvmi.New()
				vmi.Spec.Volumes = []v1.Volume{
					{
						Name: "overlay-disk",
						VolumeSource: v1.VolumeSource{
							Overlay: &v1.OverlayVolumeSource{
								BackingHostPath: &k8sv1.HostPathVolumeSource{
									Path: backingPath,
								},
								BackingFormat: "qcow2",
							},
						},
					},
				}

				By("Creating overlay image")
				err := handler.CreateOverlayImages(vmi, &api.Domain{})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying overlay exists in default location")
				expectedPath := filepath.Join(overlayBaseDir, "overlay-disk", "disk.qcow2")
				_, err = os.Stat(expectedPath)
				Expect(err).NotTo(HaveOccurred())
			})
		})

		Context("With HostPath backing and TargetHostPath", func() {
			It("Should create overlay image at target HostPath", func() {
				backingPath := filepath.Join(GinkgoT().TempDir(), "base-image.img")
				By("Creating a backing image at HostPath")
				Expect(createBackingImageForHostPath(backingPath)).To(Succeed())

				By("Creating a minimal VMI with overlay volume and target HostPath")
				vmi := libvmi.New()
				vmi.Spec.Volumes = []v1.Volume{
					{
						Name: "overlay-disk",
						VolumeSource: v1.VolumeSource{
							Overlay: &v1.OverlayVolumeSource{
								BackingHostPath: &k8sv1.HostPathVolumeSource{
									Path: backingPath,
								},
								TargetHostPath: &k8sv1.HostPathVolumeSource{
									Path: targetHostPathDir,
								},
								BackingFormat: "qcow2",
								Persistent:    true,
							},
						},
					},
				}

				By("Creating overlay image")
				err := handler.CreateOverlayImages(vmi, &api.Domain{})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying overlay exists at target HostPath")
				expectedPath := filepath.Join(targetHostPathDir, "overlay-disk.qcow2")
				_, err = os.Stat(expectedPath)
				Expect(err).NotTo(HaveOccurred())
			})
		})
	})

	Describe("multiple overlay volumes", func() {
		It("Should create multiple overlay images", func() {
			By("Creating a VMI with multiple overlay volumes")
			vmi := libvmi.New()
			vmi.Spec.Volumes = []v1.Volume{
				{
					Name: "overlay-disk1",
					VolumeSource: v1.VolumeSource{
						Overlay: &v1.OverlayVolumeSource{
							BackingPVC: &v1.PersistentVolumeClaimVolumeSource{
								PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{
									ClaimName: "backing-pvc1",
								},
							},
							BackingFormat: "raw",
						},
					},
				},
				{
					Name: "overlay-disk2",
					VolumeSource: v1.VolumeSource{
						Overlay: &v1.OverlayVolumeSource{
							BackingPVC: &v1.PersistentVolumeClaimVolumeSource{
								PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{
									ClaimName: "backing-pvc2",
								},
							},
							BackingFormat: "raw",
						},
					},
				},
			}

			By("Creating backing images for the PVCs")
			Expect(createBackingImageForPVC("overlay-disk1", false)).To(Succeed())
			Expect(createBackingImageForPVC("overlay-disk2", false)).To(Succeed())

			By("Creating overlay images")
			err := handler.CreateOverlayImages(vmi, &api.Domain{})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying all overlays exist")
			_, err = os.Stat(filepath.Join(overlayBaseDir, "overlay-disk1", "disk.qcow2"))
			Expect(err).NotTo(HaveOccurred())
			_, err = os.Stat(filepath.Join(overlayBaseDir, "overlay-disk2", "disk.qcow2"))
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("idempotency", func() {
		It("Should handle creating overlays idempotently", func() {
			By("Creating a VMI with overlay volume")
			vmi := libvmi.New()
			vmi.Spec.Volumes = []v1.Volume{
				{
					Name: "overlay-disk",
					VolumeSource: v1.VolumeSource{
						Overlay: &v1.OverlayVolumeSource{
							BackingPVC: &v1.PersistentVolumeClaimVolumeSource{
								PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{
									ClaimName: "backing-pvc",
								},
							},
							BackingFormat: "raw",
						},
					},
				},
			}

			By("Creating backing image")
			Expect(createBackingImageForPVC("overlay-disk", false)).To(Succeed())

			By("Creating overlay image first time")
			err := handler.CreateOverlayImages(vmi, &api.Domain{})
			Expect(err).NotTo(HaveOccurred())

			By("Creating overlay image second time (should be idempotent)")
			err = handler.CreateOverlayImages(vmi, &api.Domain{})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying overlay still exists")
			expectedPath := filepath.Join(overlayBaseDir, "overlay-disk", "disk.qcow2")
			_, err = os.Stat(expectedPath)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("cleanup non-persistent overlays", func() {
		It("Should cleanup non-persistent overlays", func() {
			By("Creating a VMI with non-persistent overlay")
			vmi := libvmi.New()
			vmi.Spec.Volumes = []v1.Volume{
				{
					Name: "overlay-disk",
					VolumeSource: v1.VolumeSource{
						Overlay: &v1.OverlayVolumeSource{
							BackingPVC: &v1.PersistentVolumeClaimVolumeSource{
								PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{
									ClaimName: "backing-pvc",
								},
							},
							BackingFormat: "raw",
							Persistent:    false,
						},
					},
				},
			}

			By("Creating backing image and overlay")
			Expect(createBackingImageForPVC("overlay-disk", false)).To(Succeed())
			err := handler.CreateOverlayImages(vmi, &api.Domain{})
			Expect(err).NotTo(HaveOccurred())

			overlayPath := filepath.Join(overlayBaseDir, "overlay-disk", "disk.qcow2")
			By("Verifying overlay exists")
			_, err = os.Stat(overlayPath)
			Expect(err).NotTo(HaveOccurred())

			By("Cleaning up non-persistent overlays")
			err = handler.CleanupNonPersistentOverlays(vmi)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying overlay was deleted")
			_, err = os.Stat(overlayPath)
			Expect(errors.Is(err, os.ErrNotExist)).To(BeTrue())
		})

		It("Should not cleanup persistent overlays", func() {
			backingPath := filepath.Join(GinkgoT().TempDir(), "base-image.img")
			By("Creating backing image at HostPath")
			Expect(createBackingImageForHostPath(backingPath)).To(Succeed())

			By("Creating a VMI with persistent overlay")
			vmi := libvmi.New()
			vmi.Spec.Volumes = []v1.Volume{
				{
					Name: "overlay-disk",
					VolumeSource: v1.VolumeSource{
						Overlay: &v1.OverlayVolumeSource{
							BackingHostPath: &k8sv1.HostPathVolumeSource{
								Path: backingPath,
							},
							TargetHostPath: &k8sv1.HostPathVolumeSource{
								Path: targetHostPathDir,
							},
							BackingFormat: "qcow2",
							Persistent:    true,
						},
					},
				},
			}

			By("Creating overlay image")
			err := handler.CreateOverlayImages(vmi, &api.Domain{})
			Expect(err).NotTo(HaveOccurred())

			overlayPath := filepath.Join(targetHostPathDir, "overlay-disk.qcow2")
			By("Verifying overlay exists")
			_, err = os.Stat(overlayPath)
			Expect(err).NotTo(HaveOccurred())

			By("Attempting to cleanup (should skip persistent overlays)")
			err = handler.CleanupNonPersistentOverlays(vmi)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying persistent overlay still exists")
			_, err = os.Stat(overlayPath)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

func fakeCreateOverlayDisk(backingFile string, backingFormat string, imagePath string) ([]byte, error) {
	// Validate backing format
	if backingFormat != "raw" && backingFormat != "qcow2" {
		return nil, fmt.Errorf("invalid backing format: %s", backingFormat)
	}

	// Check if backing file exists
	_, err := os.Stat(backingFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("backing file does not exist: %s", backingFile)
	}

	// Create the overlay image file
	f, err := os.Create(imagePath)
	if err != nil {
		return nil, err
	}
	err = f.Close()
	return nil, err
}
