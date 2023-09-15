# Bootloading <-> Dracut <-> OSTree
```mermaid
sequenceDiagram
    participant Kernel
    participant Initramfs
    participant RealRootFS
    participant Dracut
    participant OSTree

    Note over Kernel: Linux Kernel Loading

    Initramfs ->> Kernel: Initramfs Loaded
    Kernel ->> Initramfs: Execute /init

    Note over Initramfs: Initramfs Setup and Mount Real Root FS

    Initramfs ->> Dracut: Initramfs Setup
    Dracut ->> Dracut: Execute Dracut Modules
    Dracut ->> Dracut: Generate OSTree Hooks

    Note over Dracut: Dracut Integration with OSTree

    Dracut ->> OSTree: Read OSTree Configuration
    Dracut ->> OSTree: Mount OSTree Root FS
    Dracut ->> OSTree: Run OSTree Hooks

    Note over OSTree: OSTree Module Functions

    OSTree ->> OSTree: Prepare Root (initialize rootfs)


    Note over Dracut: PIVOT ROOT Operation

    RealRootFS -->> Initramfs: Move Initramfs to Subdirectory
    RealRootFS ->> /newroot: Create /newroot Directory
    Initramfs ->> /newroot: Mount Real Root FS

    Note over Initramfs: Init Process Switch to /newroot

    /newroot ->> /newroot: Setup Root FS

    /newroot ->> Init: Execute init Process

    Note over Init: Continue Boot Process on Real Root FS

    Init -->> RealRootFS: Boot Complete
```
# CAPI <-> CAPV sequence diagram
```mermaid
sequenceDiagram
    participant KCP
    participant KCP provider
    participant Machine
        participant CAPI Core Provider
    participant InfraMachine
    participant Infra Provider/External implementation
    KCP-->>KCP provider: Watch
    alt RolloutStrategy=RollingUpdate
        note right of KCP provider: RollingUpdate flow<br/>No Change
    else RolloutStrategy=InplaceUpdate
    rect rgb(191, 223, 255)
        KCP provider->>KCP: Update status.Condition[MachinesSpecUpToDateCondition] with InplaceUpdateInProgress reason
        KCP provider->>KCP: Add "controlplane.cluster.x-k8s.io/inplace-upgrade"<br/> annotation to spec.MachineTemplate.ObjectMeta.Annotations
        KCP provider->>Machine: For the oldest machine update<br/> 1) status.Version = spec.Version,<br/>2) spec.Version = &kcp.Spec.Version
        rect rgb(200, 150, 255)
        loop Requeue while there are machine pending inplace upgrade
        KCP provider->>Machine: Get machines with "controlplane.cluster.x-k8s.io/inplace-upgrade" annotation
        end
        end
    end
    end
    KCP provider->>Machine: SyncMachines(in-place annotation propagation)
    KCP provider->>InfraMachine: SyncMachines(in-place annotation propagation)
    Machine-->>CAPI Core Provider: Watch
    alt has controlplane.cluster.x-k8s.io/inplace-upgrade
    CAPI Core Provider->>Machine: Update status.Phase to Upgrading
    end
    InfraMachine-->>Infra Provider/External implementation: Watch
    note right of Infra Provider/External implementation: Implementation to handle<br/>Inplace Upgrades
    Infra Provider/External implementation->>InfraMachine: Update status.Version
    alt has controlplane.cluster.x-k8s.io/inplace-upgrade
    CAPI Core Provider->>Machine: mirror status.Version from InfraMachine status.Version
    end
    alt RolloutStrategy=RollingUpdate
        note left of KCP provider: RollingUpdate flow<br/>No Change
    else RolloutStrategy=InplaceUpdate
    rect rgb(191, 223, 255)
    KCP provider->>KCP: 1) Update status.Version to the lowest version in machines status.Version
    rect rgb(200, 150, 255)
    alt lowestversion=kcp.Spec.Version
    KCP provider->>KCP: delete "controlplane.cluster.x-k8s.io/inplace-upgrade" annotation<br/> from Spec.MachineTemplate.ObjectMeta.Annotations
    end
    end
    end
    end
  
```
# CAPV <-> Agent sequence diagram
```mermaid
sequenceDiagram
    box rgb(255,223,255) Management Cluster
    participant KCP
    participant Machine
    participant InfraMachine
    participant InfraProvider
    end
    Machine-->>InfraProvider: Watches
    InfraMachine-->>InfraProvider: Watches
    alt machine has controlplane.cluster.x-k8s.io/inplace-upgrade && machine.Spec.Version != Machine.Status.Version
    note right of InfraProvider: ReconcileInplaceUpgrade
    InfraProvider->>InfraMachine: update InplaceUpgradeCondition to false
    note right of InfraMachine: update the Ready condition to false
    InfraMachine-->>Machine: update InfrastructureReadyCondition to false
    Machine-->>KCP: update MachinesReadyCondition to false
    note right of Machine: update the Ready condition to false
    InfraProvider->>K8sNode: Add annotation "infrastructure.cluster.x-k8s.io/inplace-upgrade": "Scheduled"
    end
    box rgb(236, 249, 255) Workload Cluster
    participant K8sNode
    participant UpgradeCapability
    end
    K8sNode-->>UpgradeCapability: watches Node
    note right of UpgradeCapability: Inplace-upgrade K8s/OS and <br/> kubeadm config changes 
    alt succeess
    UpgradeCapability->>K8sNode: Update annotation "controlplane.cluster.x-k8s.io/inplace-upgrade": "Upgraded"
    else failure
    UpgradeCapability->>K8sNode: Update annotation "controlplane.cluster.x-k8s.io/inplace-upgrade": "Failed" <br/> Add annotation "controlplane.cluster.x-k8s.io/failure-reason": "someErr"
    end
    K8sNode->>InfraProvider: Get Node
    alt if annotations["controlplane.cluster.x-k8s.io/inplace-upgrade"] == "Upgraded"
    InfraProvider->>InfraMachine: Update version.Status = &node.Status.NodeInfo.KubeletVersion
    else if annotations["controlplane.cluster.x-k8s.io/inplace-upgrade"] == "Failed"
    else else
    note left of InfraProvider: Requeue
    end
```