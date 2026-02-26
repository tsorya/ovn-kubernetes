## DPU support

With the emergence of [Data Processing Units](https://blogs.nvidia.com/blog/2020/05/20/whats-a-dpu-data-processing-unit/) (DPUs), 
NIC vendors can now offer greater hardware acceleration capability, flexibility and security. 

It is desirable to leverage DPU in OVN-kubernetes to accelerate networking and secure the network control plane.

A DPU consists of:
- Industry-standard, high-performance, software-programmable multi-core CPU
- High-performance network interface
- Flexible and programmable acceleration engines

Similarly to Smart-NICs, a DPU follows the kernel switchdev model.
In this model, every VF/PF net-device on the host has a corresponding representor net-device existing
on the embedded CPU.

Any vendor that manufactures a DPU which supports the above model should work with current design.

Design document can be found [here](https://docs.google.com/document/d/11IoMKiohK7hIyIE36FJmwJv46DEBx52a4fqvrpCBBcg/edit?usp=sharing).

## OVN-Kubernetes in a DPU-Accelerated Environment

The **ovn-kubernetes** deployment will have two parts one on the host and another on the DPU side.


These aforementioned parts are expected to be deployed also on two different Kubernetes clusters, one for the host and another for the DPUs.


### Host Cluster
---

#### OVN-Kubernetes control plane related component
- ovn-cluster-manager

#### OVN-Kubernetes components on a Standard Host (Non-DPU)
- local-nb-ovsdb
- local-sb-ovsdb
- run-ovn-northd
- ovnkube-controller-with-node
- ovn-controller
- ovs-metrics

#### OVN-Kubernetes component on a DPU-Enabled Host
- ovn-node

For detailed configuration of gateway interfaces in DPU host mode, see [DPU Gateway Interface Configuration](dpu-gateway-interface.md).

### Management Port Resilience in DPU Host Mode

In DPU host mode, the management port (`ovn-k8s-mp0`) is backed by an SR-IOV VF
netdevice. The VF is either specified directly by name
(`ovnkube-node-mgmt-port-netdev`) or allocated from a device plugin resource pool
(`ovnkube-node-mgmt-port-dp-resource-name`).

At startup, the VF is looked up by its current interface name — which may be the
original kernel name (e.g., `enp3s0f0v0`) or `ovn-k8s-mp0` if a previous instance
already renamed it. It is then configured as the management port.

A periodic reconciliation loop monitors the management port and re-applies its
configuration (routes, addresses, nftables rules). If reconciliation fails — for
example because the underlying interface has disappeared — the code attempts to
recreate the management port from scratch.

#### DPU reboot recovery

When a DPU reboots while the host and its ovn-kube-node container are still running,
all VFs on the host are destroyed and recreated by the DPU firmware. The recreated
VFs may appear under different interface names, and a DPU firmware settings change
can even cause the same physical port to be re-enumerated under a different PCI
address.

If the management port cannot be recreated, the ovn-kube-node process terminates.
This is intentional: the original VF no longer exists, and a DPU firmware settings
change can cause the same physical port to be re-enumerated under a different PCI
address, so there is no reliable way to determine which of the newly created VFs
should be used for the management port. The only safe recovery is a container
restart, which allows the device plugin to re-allocate the correct VF from its
resource pool.

### DPU Cluster
---

#### OVN-Kubernetes components
- local-nb-ovsdb 
- local-sb-ovsdb
- run-ovn-northd
- ovnkube-controller-with-node
- ovn-controller
- ovs-metrics
