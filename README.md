# Cluster API Provider MAAS

This is a Cluster API infrastructure provider for MAAS.

## Getting Started

### Prerequisites

- Kubernetes v1.29.0+
- MAAS 3.0+
- Cluster API v1.5.0+

## Development

### Building the provider


## Custom Image Generation
Refer [image-generation/](image-generation/README.md)

## MAAS In-Memory Deployment

MAAS in-memory deployment allows machines to run directly from RAM without writing to disk. This feature is useful for ephemeral workloads, testing environments, or scenarios where you want to preserve disk state.

### MAAS Version Compatibility

To use the in-memory deployment feature, your MAAS installation must be running one of the following versions:

| MAAS Version | Status      |
|--------------|-------------|
| \>= 3.5.10   | ✅ Supported |
| \>= 3.6.3    | ✅ Supported |
| \>= 3.7.1    | ✅ Supported |

### Requirements

- **Minimum RAM**: Machines must have at least **16GB of RAM** for proper functionality when using in-memory deployment.

### Usage Example

To enable in-memory deployment, set `deployInMemory: true` in your `MaasMachineTemplate` spec:

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: MaasMachineTemplate
metadata:
  annotations:
  name: mt-worker-memory
  namespace: test
spec:
  template:
    spec:
      deployInMemory: true
      image: custom/your-image
      minCPU: 4
      minMemory: 16384
      resourcePool: default
      tags:
      - memory
```

## Hello world

- Create kind cluster
```bash
make docker-build
```

### Deploying the provider

- Setup clusterctl configuration `~/.cluster-api/clusterctl.yaml`
```
# MAAS access endpoint and key
MAAS_API_KEY: <maas-api-key>
MAAS_ENDPOINT: http://<maas-endpoint>/MAAS
MAAS_DNS_DOMAIN: maas.domain

# Cluster configuration
KUBERNETES_VERSION: v1.33.5
CONTROL_PLANE_MACHINE_IMAGE: custom/u-2204-0-k-1335-0
CONTROL_PLANE_MACHINE_MINCPU: 4
CONTROL_PLANE_MACHINE_MINMEMORY: 8192
WORKER_MACHINE_IMAGE: custom/u-2204-0-k-1335-0
WORKER_MACHINE_MINCPU: 4
WORKER_MACHINE_MINMEMORY: 8192

# Selecting machine based on resourcepool (optional) and machine tag (optional)
CONTROL_PLANE_MACHINE_RESOURCEPOOL: resorcepool-controller
CONTROL_PLANE_MACHINE_TAG: hello-world
WORKER_MACHINE_RESOURCEPOOL: resourcepool-worker
WORKER_MACHINE_TAG: hello-world
```
- Initialize infrastructure
```bash
clusterctl init --infrastructure maas:v0.9.0
```
  `clusterctl init` substitutes `MAAS_ENDPOINT`/`MAAS_API_KEY` into the
  `capmaas-manager-bootstrap-credentials` secret (in the `capmaas-system` namespace)
  and wires it into the controller — you do **not** need to create the secret by hand.
- Generate and create cluster
```
clusterctl generate cluster t-cluster --infrastructure=maas:v0.9.0 --kubernetes-version v1.33.5 --control-plane-machine-count=1 --worker-machine-count=3 | kubectl apply -f -
```

### API server DNS interface selection (optional)

If you want API server DNS records to use the IP from a specific MAAS interface
(instead of the default PXE-path/IP ordering), set `apiServerInterfaceTag` in
`MaasCluster.spec` to a MAAS tag assigned to the desired interface:

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: MaasCluster
metadata:
  name: cluster1
spec:
  dnsDomain: maas.domain
  apiServerInterfaceTag: control-plane
```

The tag must be unique per machine. If two interfaces on the same machine share
the same tag, reconciliation fails with an explicit error before any DNS change
is made.

Fallback behavior:
- If `apiServerInterfaceTag` is empty/unset, CAPMAAS keeps the previous behavior
  and uses the first `MachineExternalIP` from machine status.

You can verify which tag is currently being used via status:

```bash
kubectl get maascluster <name> -n <namespace> -o jsonpath='{.status.network.publishedInterfaceTag}{"\n"}'
```

### Testing the provider

```bash
make test
```

## LXD Integration

- Set `IMG` to the controller image you control (registry + image + tag). This same image must be used for `docker-build`, `docker-push`, and `dev-manifests` so that the generated `infrastructure-components.yaml` points at the image you actually pushed. Export it once so every step picks it up:
```shell
export IMG=<your-registry>/cluster-api-provider-maas-controller-amd64:v0.9.0
```

- Build and push the controller image:
```shell
make docker-build && make docker-push
```

- Generate dev manifests. `dev-manifests` requires `IMG` to be set — it errors out if it is empty, since an unset `IMG` produces manifests with an empty controller image:
```shell
make dev-manifests IMG=$IMG
```

- Move _build/dev/ directory contents to ~/.cluster-api/overrides v0.9.0 depending on version you are working with

```text
.
├── clusterctl.yaml
├── overrides
│   ├── infrastructure-maas
│       └── v0.9.0
│           ├── cluster-template.yaml
│           ├── infrastructure-components.yaml
│           └── metadata.yaml
└── version.yaml

# Test MAAS API integration
./maas-test-linux --test-vm-host --node-ip=<node-ip> --maas-endpoint=http://<maas-server>:5240/MAAS --maas-api-key="YOUR_MAAS_API_KEY" --zone=default --resource-pool=default
```

- Run
```shell
clusterctl init --infrastructure maas:v0.9.0
```


## Install CRDs
### v1beta2 v0.9.0 release
- Generate cluster using
```shell
clusterctl generate cluster t-cluster  --infrastructure=maas:v0.9.0 | kubectl apply -f -
```
or
```shell
clusterctl generate cluster t-cluster --infrastructure=maas:v0.9.0 --kubernetes-version v1.33.5 > my_cluster.yaml
kubectl apply -f my_cluster.yaml
```
or
```shell
clusterctl generate cluster t-cluster --infrastructure=maas:v0.9.0 --kubernetes-version v1.33.5 --control-plane-machine-count=1 --worker-machine-count=3 > my_cluster.yaml
kubectl apply -f my_cluster.yaml
```

## LXD Integration (HCP & WLC)

This provider supports dynamic LXD VM creation for workload clusters. This involves two main stages:

1. **HCP — Host Control Plane**: convert bare-metal MAAS machines into LXD VM hosts.
2. **WLC — Workload Cluster**: provision LXD VMs on those hosts for workload clusters.

👉 **See [docs/HCP_WLC_GUIDE.md](docs/HCP_WLC_GUIDE.md) for a step-by-step guide**
(prerequisites, templates, and field reference).

### Testing LXD Integration

We've created test utilities to validate the LXD integration:

1. **LXD Initialization Test**: Tests LXD initialization using the official LXD Go client.
2. **VM Host Registration Test**: Tests VM host registration with MAAS using direct API calls.
3. **MAAS API Test**: Tests MAAS API integration for VM host management and VM creation.

To build and transfer these test utilities to a target node:

```bash
cd lxd-test-tmp
./build-and-transfer.sh --host <target-host> [--key <path/to/key.pem>] [--user <username>]
```

Then on the target node:

```bash
# Test LXD initialization
sudo ./lxd-test-linux --storage-backend=zfs --storage-size=50 --network-bridge=br0

# Test VM host registration
./vmhost-test-linux --node-ip=<node-ip> --maas-endpoint=http://<maas-server>:5240/MAAS --maas-api-key="YOUR_MAAS_API_KEY" --zone=default --resource-pool=default

# Test MAAS API integration
./maas-test-linux --test-vm-host --node-ip=<node-ip> --maas-endpoint=http://<maas-server>:5240/MAAS --maas-api-key="YOUR_MAAS_API_KEY" --zone=default --resource-pool=default
```

## Architecture

The provider uses the MAAS API to manage machines and LXD VMs. It does not use direct LXD socket connections or CLI commands, ensuring it works correctly even when the controller is running on a different host than the LXD host.

## Contributing

Please read [CONTRIBUTING.md](CONTRIBUTING.md) for details on our code of conduct, and the process for submitting pull requests to us.
