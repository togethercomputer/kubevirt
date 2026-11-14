# Overlay Volume Configuration States

## Complete State Matrix

This document summarizes all valid and invalid combinations of the `persistent` flag and target location settings for overlay volumes.

## Field Definitions

- **`persistent`**: Boolean flag controlling overlay lifecycle
  - `true` = overlay survives VM restarts
  - `false` = overlay deleted when VM stops
  
- **Target Location**: Where the overlay qcow2 file is stored
  - `targetPVC` = PersistentVolumeClaim (must be pre-created)
  - `targetHostPath` = Directory path on the host node
  - `none` = Default ephemeral location

- **Backing Source**: Where the read-only base image comes from
  - `backingPVC` = PersistentVolumeClaim with base image
  - `backingHostPath` = File path on the host node

---

## Valid Configurations

### Configuration 1: Non-Persistent, Default Location

```yaml
overlay:
  backingPVC:
    claimName: base-image
  backingFormat: raw
  persistent: false
  # No target specified
```

| Field | Value | Result |
|-------|-------|--------|
| persistent | `false` | ✅ |
| targetPVC | `null` | ✅ |
| targetHostPath | `null` | ✅ |

**Overlay Location**: `/var/run/kubevirt-private/overlay-disks/<volumeName>/disk.qcow2`

**Lifecycle**:
- ✅ Created when VM starts
- ✅ Deleted when VM stops
- ❌ Changes NOT preserved across restarts

**Use Cases**:
- Ephemeral test VMs
- CI/CD environments
- Temporary workspaces
- VMs that should start fresh each time

**Storage**: EmptyDir volume (pod-local, deleted with pod)

---

### Configuration 2: Persistent with TargetPVC

```yaml
overlay:
  backingPVC:
    claimName: base-image
  targetPVC:
    claimName: vm-overlay-storage
  backingFormat: raw
  persistent: true
```

| Field | Value | Result |
|-------|-------|--------|
| persistent | `true` | ✅ |
| targetPVC | `vm-overlay-storage` | ✅ |
| targetHostPath | `null` | ✅ |

**Overlay Location**: `/var/run/kubevirt-private/vmi-disks/target-<volumeName>/overlay.qcow2`

**Lifecycle**:
- ✅ Created when VM starts (or reused if exists)
- ✅ Preserved when VM stops
- ✅ Reused when VM restarts
- ✅ Survives pod deletion/recreation

**Use Cases**:
- Development VMs
- User workstations
- Stateful applications
- VMs that need to preserve changes

**Storage**: Dedicated PVC (must be created beforehand)

**Requirements**:
```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: vm-overlay-storage
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 10Gi
```

---

### Configuration 3: Persistent with TargetHostPath

```yaml
overlay:
  backingHostPath:
    path: /var/lib/images/base.img
  targetHostPath:
    path: /var/lib/vms/my-vm
  backingFormat: raw
  persistent: true
```

| Field | Value | Result |
|-------|-------|--------|
| persistent | `true` | ✅ |
| targetPVC | `null` | ✅ |
| targetHostPath | `/var/lib/vms/my-vm` | ✅ |

**Overlay Location**: `/var/lib/vms/my-vm/<volumeName>.qcow2`

**Lifecycle**:
- ✅ Created when VM starts (or reused if exists)
- ✅ Preserved when VM stops
- ✅ Reused when VM restarts
- ✅ Survives pod deletion
- ⚠️ Tied to specific node (if VM migrates, overlay stays on original node)

**Use Cases**:
- Local development clusters
- Single-node deployments
- Testing/debugging
- Direct host storage access

**Storage**: Host filesystem (directory must exist or be creatable)

**Node Affinity**: VM must run on same node to access overlay

---

## Invalid Configurations

### Invalid 1: Persistent without Target

```yaml
overlay:
  backingPVC:
    claimName: base-image
  backingFormat: raw
  persistent: true
  # No target specified - INVALID!
```

| Field | Value | Result |
|-------|-------|--------|
| persistent | `true` | ❌ |
| targetPVC | `null` | ❌ |
| targetHostPath | `null` | ❌ |

**Error**: `persistent=true requires targetPVC or targetHostPath to be specified. Overlays cannot persist to ephemeral storage.`

**Why Invalid**: 
- Default location uses EmptyDir (deleted when pod terminates)
- Cannot persist data in ephemeral storage
- Would mislead users into thinking data is saved

**Fix**: Add a target location:
```yaml
targetPVC:
  claimName: my-overlay-storage
```

---

### Invalid 2: Non-Persistent with Target

```yaml
overlay:
  backingPVC:
    claimName: base-image
  targetPVC:
    claimName: vm-overlay-storage
  backingFormat: raw
  persistent: false  # INVALID with target!
```

| Field | Value | Result |
|-------|-------|--------|
| persistent | `false` | ❌ |
| targetPVC | `vm-overlay-storage` | ❌ |
| targetHostPath | `null` | ❌ |

**Error**: `targetPVC or targetHostPath specified requires persistent=true. Non-persistent overlays must use the default ephemeral location.`

**Why Invalid**:
- Specifying a PVC/HostPath target implies you want to keep the data
- Contradicts the `persistent: false` setting
- Would waste storage resources

**Fix**: Either:
1. Set `persistent: true` to keep the overlay
2. Remove the target to use ephemeral storage

---

### Invalid 3: Multiple Targets

```yaml
overlay:
  backingPVC:
    claimName: base-image
  targetPVC:
    claimName: pvc-overlay
  targetHostPath:
    path: /var/lib/overlays  # INVALID - can't have both!
  persistent: true
```

| Field | Value | Result |
|-------|-------|--------|
| persistent | `true` | ✅ |
| targetPVC | `pvc-overlay` | ❌ |
| targetHostPath | `/var/lib/overlays` | ❌ |

**Error**: `can have at most one target (targetHostPath or targetPVC)`

**Why Invalid**:
- Overlay can only be stored in one location
- Ambiguous which target to use
- Could lead to data inconsistency

**Fix**: Choose one target:
```yaml
# Either this:
targetPVC:
  claimName: pvc-overlay

# Or this:
targetHostPath:
  path: /var/lib/overlays
```

---

### Invalid 4: Multiple Backing Sources

```yaml
overlay:
  backingPVC:
    claimName: base-pvc
  backingHostPath:
    path: /var/lib/base.img  # INVALID - can't have both!
  persistent: true
  targetPVC:
    claimName: overlay-pvc
```

**Error**: `must have exactly one backing source (backingPVC or backingHostPath)`

**Why Invalid**:
- qemu-img can only have one backing file
- Ambiguous which backing source to use

**Fix**: Choose one backing source

---

### Invalid 5: No Backing Source

```yaml
overlay:
  # No backing source - INVALID!
  targetPVC:
    claimName: overlay-pvc
  persistent: true
```

**Error**: `must have at least one backing source (backingPVC or backingHostPath)`

**Why Invalid**:
- Overlay must have a backing file to create copy-on-write image
- No base image specified

**Fix**: Add a backing source:
```yaml
backingPVC:
  claimName: base-image
```

---

## State Transition Diagram

```
┌─────────────────────────────────────────────────────────────┐
│                    Overlay Volume States                     │
└─────────────────────────────────────────────────────────────┘

                    User Configuration
                            │
                            ▼
              ┌─────────────────────────┐
              │   Validation Check      │
              └─────────────────────────┘
                     │          │
           Valid     │          │    Invalid
                     │          │
                     ▼          ▼
        ┌────────────────┐  ┌──────────────┐
        │ Allowed States │  │ Reject & Err │
        └────────────────┘  └──────────────┘
                 │
         ┌───────┴───────┬──────────────┐
         │               │              │
         ▼               ▼              ▼
   ┌─────────┐    ┌─────────┐   ┌─────────┐
   │ State 1 │    │ State 2 │   │ State 3 │
   │         │    │         │   │         │
   │ persist │    │ persist │   │ persist │
   │ =false  │    │ =true   │   │ =true   │
   │         │    │         │   │         │
   │ target  │    │ target  │   │ target  │
   │ =none   │    │ =PVC    │   │ =HostP  │
   └─────────┘    └─────────┘   └─────────┘
       │              │              │
       ▼              ▼              ▼
   Ephemeral      Persistent     Persistent
   EmptyDir         PVC          HostPath
```

---

## Backing Format States

Independent of persistent/target settings:

### State: backingFormat = "raw" (default)

```yaml
overlay:
  backingFormat: raw  # or omitted
```

- Backing file is raw disk image
- Most common for PVC-based volumes
- Example: ContainerDisk volumes, CDI-imported images

**qemu-img command**:
```bash
qemu-img create -f qcow2 -b <backing> -F raw <overlay>
```

---

### State: backingFormat = "qcow2"

```yaml
overlay:
  backingFormat: qcow2
```

- Backing file is already qcow2
- Common for cloud images, downloaded VMs
- Creates qcow2-on-qcow2 (less common but valid)

**qemu-img command**:
```bash
qemu-img create -f qcow2 -b <backing> -F qcow2 <overlay>
```

---

### State: backingFormat = invalid

```yaml
overlay:
  backingFormat: vmdk  # INVALID
```

**Error**: `invalid backingFormat 'vmdk', allowed values are 'raw' or 'qcow2'`

**Why Invalid**: Only raw and qcow2 formats are supported

---

## Summary Table

| persistent | targetPVC | targetHostPath | Valid | Storage Location | Lifecycle |
|-----------|-----------|----------------|-------|------------------|-----------|
| `false` | `null` | `null` | ✅ | EmptyDir (default) | Deleted on VM stop |
| `true` | PVC name | `null` | ✅ | PVC | Survives restarts |
| `true` | `null` | path | ✅ | Host filesystem | Survives restarts |
| `true` | `null` | `null` | ❌ | - | Error: need target for persistence |
| `false` | PVC name | `null` | ❌ | - | Error: target implies persistence |
| `false` | `null` | path | ❌ | - | Error: target implies persistence |
| `true` | PVC name | path | ❌ | - | Error: multiple targets |
| `false` | PVC name | path | ❌ | - | Error: multiple targets |

---

## Decision Tree

```
Need overlay volume?
│
├─ Do you need to keep changes across VM restarts?
│  │
│  ├─ YES → Set persistent: true
│  │         │
│  │         ├─ Need portable storage?
│  │         │  └─ YES → Use targetPVC
│  │         │           (works on any node)
│  │         │
│  │         └─ Local/single node OK?
│  │            └─ YES → Use targetHostPath
│  │                     (faster, simpler for dev)
│  │
│  └─ NO → Set persistent: false
│            └─ Don't specify target
│               (uses ephemeral EmptyDir)
│
└─ Backing source?
   │
   ├─ Base image in PVC? → Use backingPVC
   │
   └─ Base image on host? → Use backingHostPath
```

---

## Real-World Scenarios

### Scenario 1: CI/CD Test Runner
```yaml
persistent: false
target: none
# Fresh environment for each test run
```

### Scenario 2: Developer Workstation
```yaml
persistent: true
targetPVC: dev-workspace-overlay
# Keep changes, survives cluster operations
```

### Scenario 3: Production VM (Multi-node cluster)
```yaml
persistent: true
targetPVC: prod-vm-overlay
# Portable, can migrate between nodes
```

### Scenario 4: Local Lab/Testing
```yaml
persistent: true
targetHostPath: /lab/vms/test-vm
# Simple, direct host access
```

### Scenario 5: Classroom Environment
```yaml
persistent: false
target: none
# Students get fresh VM each session
```

---

## Migration Between States

### From Non-Persistent to Persistent

**Not Possible**: Ephemeral overlays are deleted when VM stops

**Workaround**:
1. Keep VM running
2. Copy overlay file to persistent location
3. Update VMI spec with new persistent configuration
4. Restart VM with new config

### From Persistent to Non-Persistent

**Possible but Destructive**:
1. Change `persistent: true` → `false`
2. Remove target
3. Restart VM
4. Old overlay remains in PVC/HostPath but is no longer used
5. New ephemeral overlay created

### Changing Target Location

**Requires Manual Copy**:
1. Stop VM
2. Copy overlay from old target to new target
3. Update VMI spec with new target
4. Start VM

---

## Best Practices

1. **Development**: Use `persistent: true` + `targetPVC` for flexibility
2. **Testing**: Use `persistent: false` for clean test runs
3. **Production**: Use `persistent: true` + `targetPVC` for reliability
4. **Local Dev**: Use `persistent: true` + `targetHostPath` for simplicity
5. **Shared Bases**: Always use `backingPVC` for multiple VMs from one base
6. **Storage Planning**: Size target PVCs appropriately for expected changes
7. **Cleanup**: Delete unused target PVCs to reclaim storage

