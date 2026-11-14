# Overlay Volumes

## Overview

Overlay volumes provide qcow2 copy-on-write functionality for VirtualMachineInstances, enabling fast VM boot times with shared base images while maintaining VM-specific changes in separate overlay files.

## Architecture

The overlay volume implementation consists of:

1. **API Definition** (`staging/src/kubevirt.io/api/core/v1/schema.go`)
   - New `OverlayVolumeSource` type added to `VolumeSource`

2. **Overlay Disk Handler** (`pkg/ephemeral-disk/overlay-disk-handler.go`)
   - Creates qcow2 overlay images with backing file references
   - Manages overlay lifecycle (creation, cleanup)
   - Supports multiple target locations

3. **Validation** (`pkg/virt-api/webhooks/validating-webhook/admitters/vmi-create-admitter.go`)
   - Validates overlay volume configuration
   - Enforces persistent/target consistency rules

4. **Converter** (`pkg/virt-launcher/virtwrap/converter/converter.go`)
   - Converts overlay volumes to libvirt domain XML
   - Sets up backing store references

5. **Manager Integration** (`pkg/virt-launcher/virtwrap/manager.go`)
   - Calls overlay handler during VM creation

## Key Features

- **Flexible Backing Sources**: PVC or HostPath
- **Flexible Target Locations**: PVC, HostPath, or default ephemeral location
- **Persistent vs Ephemeral**: Control overlay lifecycle with `persistent` flag
- **Format Support**: Both raw and qcow2 backing formats
- **Fast Boot**: qcow2 copy-on-write enables instant VM cloning

## How It Works

When you define an overlay volume:

1. **Backing Source** (read-only base image):
   - Mounted as `backing-<volumeName>` in the launcher pod
   - Can be a PVC or HostPath

2. **Overlay Creation**:
   - A qcow2 file is created at the target location
   - Uses `qemu-img create -f qcow2 -b <backing> -F <format> <overlay>`
   - All writes go to the overlay, backing remains unchanged

3. **Target Location**:
   - **No target specified**: `/var/run/kubevirt-private/overlay-disks/<volumeName>/disk.qcow2`
   - **TargetPVC**: `/var/run/kubevirt-private/vmi-disks/target-<volumeName>/overlay.qcow2`
   - **TargetHostPath**: `<path>/<volumeName>.qcow2`

4. **Cleanup**:
   - **persistent=false**: Overlay deleted when VM stops
   - **persistent=true**: Overlay survives VM restarts

## Validation Rules

| persistent | target | Result |
|-----------|--------|--------|
| false | none | ✅ Valid - ephemeral overlay at default location |
| true | PVC or HostPath | ✅ Valid - persistent overlay at target |
| true | none | ❌ Invalid - can't persist to ephemeral storage |
| false | PVC or HostPath | ❌ Invalid - target implies persistence |

Additional rules:
- Exactly ONE backing source required (BackingPVC or BackingHostPath)
- At most ONE target (TargetPVC or TargetHostPath)
- `backingFormat` must be `raw` or `qcow2` (defaults to `raw`)

## YAML Examples

### Example 1: Basic Ephemeral Overlay (Non-Persistent)

```yaml
apiVersion: kubevirt.io/v1
kind: VirtualMachineInstance
metadata:
  name: test-vm
spec:
  domain:
    devices:
      disks:
      - name: rootdisk
        disk:
          bus: virtio
    resources:
      requests:
        memory: 1Gi
  volumes:
  - name: rootdisk
    overlay:
      backingPVC:
        claimName: ubuntu-base-image  # Shared read-only base
      backingFormat: raw
      persistent: false  # Changes lost when VM stops
```

**Use Case**: Temporary test VMs, CI/CD environments

**Overlay Location**: `/var/run/kubevirt-private/overlay-disks/rootdisk/disk.qcow2`

---

### Example 2: Persistent Overlay with PVC Target

```yaml
apiVersion: kubevirt.io/v1
kind: VirtualMachineInstance
metadata:
  name: dev-vm-1
spec:
  domain:
    devices:
      disks:
      - name: rootdisk
        disk:
          bus: virtio
    resources:
      requests:
        memory: 2Gi
  volumes:
  - name: rootdisk
    overlay:
      backingPVC:
        claimName: fedora-base-image    # Shared base image
      targetPVC:
        claimName: dev-vm-1-overlay     # VM-specific overlay storage
      backingFormat: raw
      persistent: true  # Overlay survives restarts
```

**Use Case**: Development VMs, persistent user environments

**Required PVCs**:
```yaml
# Base image (shared, created once)
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: fedora-base-image
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 5Gi
---
# Overlay storage (per-VM, created for each VM)
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: dev-vm-1-overlay
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 10Gi
```

---

### Example 3: HostPath Backing and Target

```yaml
apiVersion: kubevirt.io/v1
kind: VirtualMachineInstance
metadata:
  name: local-vm
spec:
  domain:
    devices:
      disks:
      - name: rootdisk
        disk:
          bus: virtio
    resources:
      requests:
        memory: 2Gi
  volumes:
  - name: rootdisk
    overlay:
      backingHostPath:
        path: /var/lib/kubevirt/images/centos-base.qcow2
      targetHostPath:
        path: /var/lib/kubevirt/vms/local-vm
      backingFormat: qcow2  # Base image is qcow2
      persistent: true
```

**Use Case**: Local development, single-node clusters

**Overlay Location**: `/var/lib/kubevirt/vms/local-vm/rootdisk.qcow2`

---

### Example 4: Multiple Overlays in One VM

```yaml
apiVersion: kubevirt.io/v1
kind: VirtualMachineInstance
metadata:
  name: multi-disk-vm
spec:
  domain:
    devices:
      disks:
      - name: os-disk
        disk:
          bus: virtio
      - name: app-disk
        disk:
          bus: virtio
      - name: scratch-disk
        disk:
          bus: virtio
    resources:
      requests:
        memory: 4Gi
  volumes:
  # OS disk - persistent overlay from shared base
  - name: os-disk
    overlay:
      backingPVC:
        claimName: ubuntu-22-04-base
      targetPVC:
        claimName: multi-disk-vm-os
      backingFormat: raw
      persistent: true
  
  # App disk - persistent overlay with app data
  - name: app-disk
    overlay:
      backingPVC:
        claimName: app-base-v1-2-3
      targetPVC:
        claimName: multi-disk-vm-app
      backingFormat: raw
      persistent: true
  
  # Scratch disk - ephemeral, reset on restart
  - name: scratch-disk
    overlay:
      backingPVC:
        claimName: empty-disk-template
      backingFormat: raw
      persistent: false
```

**Use Case**: Multi-tier applications, separate OS/app/data storage

---

## Common Use Cases

### 1. Fast VM Cloning

Clone multiple VMs from a single base image:

```
Base Image PVC (5GB, shared)
    ├── VM-1 Overlay PVC (10GB) → VM-1
    ├── VM-2 Overlay PVC (10GB) → VM-2
    └── VM-3 Overlay PVC (10GB) → VM-3
```

**Benefits**:
- Fast provisioning (no image copy needed)
- Storage efficiency (base image stored once)
- Independent VM changes

### 2. Golden Images

Maintain golden OS images and customize per environment:

```yaml
# Production VM
overlay:
  backingPVC:
    claimName: rhel-9-golden-image
  targetPVC:
    claimName: prod-app-server-1
  persistent: true

# Development VM (same base, different overlay)
overlay:
  backingPVC:
    claimName: rhel-9-golden-image  # Same base
  targetPVC:
    claimName: dev-app-server-1     # Different overlay
  persistent: true
```

### 3. CI/CD Test Environments

Ephemeral test VMs that start fresh each time:

```yaml
# Test VM reset on each pipeline run
overlay:
  backingPVC:
    claimName: test-environment-base
  persistent: false  # Clean slate every run
```

### 4. Tiered Storage

OS on fast storage, data on slower storage:

```yaml
volumes:
- name: os
  overlay:
    backingPVC:
      claimName: os-base
    targetPVC:
      claimName: vm-os-ssd  # Fast SSD storage
    persistent: true

- name: data
  persistentVolumeClaim:
    claimName: vm-data-hdd  # Slower HDD for data
```

## Implementation Details

### Files Modified/Added

- `staging/src/kubevirt.io/api/core/v1/schema.go` - API type definition
- `pkg/ephemeral-disk/overlay-disk-handler.go` - Core overlay logic (NEW)
- `pkg/ephemeral-disk/overlay-disk-handler_test.go` - Unit tests (NEW)
- `pkg/virt-api/webhooks/validating-webhook/admitters/vmi-create-admitter.go` - Validation
- `pkg/virt-api/webhooks/validating-webhook/admitters/vmi-create-admitter_test.go` - Tests
- `pkg/virt-launcher/virtwrap/converter/converter.go` - Domain conversion
- `pkg/virt-launcher/virtwrap/converter/converter_test.go` - Tests
- `pkg/virt-launcher/virtwrap/manager.go` - Integration
- `pkg/virt-controller/services/rendervolumes.go` - Volume rendering
- `api/openapi-spec/swagger.json` - OpenAPI spec

### qemu-img Command

The overlay is created using:

```bash
qemu-img create \
  -f qcow2 \
  -b <backing-file-path> \
  -F <backing-format> \
  <overlay-path>
```

Example:
```bash
qemu-img create \
  -f qcow2 \
  -b /var/run/kubevirt-private/vmi-disks/backing-rootdisk/disk.img \
  -F raw \
  /var/run/kubevirt-private/overlay-disks/rootdisk/disk.qcow2
```

### Libvirt Domain XML

The overlay volume is converted to libvirt XML with backing store:

```xml
<disk type='file' device='disk'>
  <driver name='qemu' type='qcow2' discard='unmap'/>
  <source file='/var/run/kubevirt-private/overlay-disks/rootdisk/disk.qcow2'/>
  <backingStore type='file'>
    <format type='raw'/>
    <source file='/var/run/kubevirt-private/vmi-disks/backing-rootdisk/disk.img'/>
  </backingStore>
  <target dev='vda' bus='virtio'/>
</disk>
```

## Troubleshooting

### Overlay Creation Fails

**Error**: `failed to create overlay for volume X`

**Check**:
1. Does the backing PVC exist and is it bound?
2. Is the backing file accessible in the pod?
3. Is there enough disk space for the overlay?
4. Does the target directory exist (for HostPath)?

### VM Won't Boot

**Error**: VM starts but disk is not accessible

**Check**:
1. Is `backingFormat` correct? (raw vs qcow2)
2. Does the backing file format match what you specified?
3. Check virt-launcher logs: `kubectl logs virt-launcher-<vm-name>`

### Persistent Overlay Not Working

**Error**: Changes are lost after VM restart

**Check**:
1. Is `persistent: true` set?
2. Is a target (TargetPVC or TargetHostPath) specified?
3. Does the target PVC still exist?
4. Check if the overlay file exists at the target location

### Validation Errors

**Error**: `persistent=true requires targetPVC or targetHostPath`

**Fix**: Add a target when using `persistent: true`:
```yaml
overlay:
  backingPVC:
    claimName: base-image
  targetPVC:
    claimName: my-overlay  # Add this
  persistent: true
```

**Error**: `targetPVC specified requires persistent=true`

**Fix**: Set persistent to true when using a target:
```yaml
overlay:
  backingPVC:
    claimName: base-image
  targetPVC:
    claimName: my-overlay
  persistent: true  # Change from false to true
```

## Performance Considerations

### Storage Backend

- **Fast local storage** (NVMe, SSD) recommended for overlay PVCs
- Base images can be on slower storage (read-only, less I/O)
- Consider local-path-provisioner for single-node setups

### Overlay Size

- Overlay PVC should be sized for expected changes
- Thin provisioning recommended to avoid over-allocation
- Monitor overlay growth in production

### Backing Format

- **raw format**: Simpler, but larger files
- **qcow2 format**: Compressed, but double-overlay (qcow2 on qcow2)
- For production: raw backing + qcow2 overlay is most common

## Future Enhancements

Potential improvements for the overlay volume feature:

- [ ] Automatic PVC creation for target overlays
- [ ] Overlay size management and warnings
- [ ] Snapshot support for overlay volumes
- [ ] Migration support for VMs with overlay volumes
- [ ] Metrics for overlay disk usage
- [ ] Support for overlay merging back to base
- [ ] Checksum validation of backing sources

## References

- qemu-img documentation: https://qemu.readthedocs.io/en/latest/tools/qemu-img.html
- QCOW2 format spec: https://github.com/qemu/qemu/blob/master/docs/interop/qcow2.txt
- KubeVirt volumes: https://kubevirt.io/user-guide/virtual_machines/disks_and_volumes/

