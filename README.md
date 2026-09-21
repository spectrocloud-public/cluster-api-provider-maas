# Cluster API Provider MAAS

This is a Cluster API infrastructure provider for MAAS.

## Getting Started

### Prerequisites

- Kubernetes v1.29.0+
- MAAS 3.0+
- Cluster API v1.5.0+

## Development

### Building the provider

```bash
make docker-build
```

### Deploying the provider

```bash
make deploy
```

### Testing the provider

```bash
make test
```

### Diagnostics and profiling

The manager serves metrics, `pprof` and a live log-level endpoint from one TLS diagnostics
endpoint (`--diagnostics-address`, default `:8443`), authorised per request via
TokenReview/SubjectAccessReview. `--metrics-bind-addr` is deprecated and ignored.

The serving certificate is self-signed and localhost-only, so every scraper and every
`curl` must skip TLS verification (`-k`, or `insecureSkipVerify: true`).
`--tls-min-version` and `--tls-cipher-suites` apply to this endpoint and to the webhook
server.

See [docs/diagnostics.md](docs/diagnostics.md) for the flag reference, the collector RBAC
(documented, not shipped) and the heap/goroutine collection runbook.

## LXD Integration

This provider supports dynamic LXD VM creation for workload clusters. This involves two main stages:

1. **Control Plane Cluster Creation Flow**: Dynamically convert SpectroCloud-deployed infrastructure Control Plane (CP) nodes into LXD-capable KVM hosts.
2. **Workload Cluster Creation Flow**: Dynamically provision LXD VMs with static IPs for workload cluster control plane nodes.

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

or
```shell
clusterctl generate cluster t-cluster --infrastructure=maas:v0.5.0 --kubernetes-version v1.26.4 --control-plane-machine-count=1 --worker-machine-count=3 > my_cluster.yaml
kubectl apply -f my_cluster.yaml
```

## LXD Integration

This provider supports dynamic LXD VM creation for workload clusters. This involves two main stages:

1. **Control Plane Cluster Creation Flow**: Dynamically convert SpectroCloud-deployed infrastructure Control Plane (CP) nodes into LXD-capable KVM hosts.
2. **Workload Cluster Creation Flow**: Dynamically provision LXD VMs with static IPs for workload cluster control plane nodes.

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
