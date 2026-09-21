# Diagnostics and profiling

`capmaas-controller-manager` serves metrics, `pprof` and a live log-level endpoint from a
single **diagnostics** endpoint, using the Cluster API standard
`sigs.k8s.io/cluster-api/util/flags` wiring.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--diagnostics-address` | `:8443` | Address the diagnostics endpoint binds to. |
| `--insecure-diagnostics` | `false` | Serve plain HTTP with **no** authn/authz. `pprof` and `/debug/flags/v` are **not** mounted in this mode — only `/metrics`. |
| `--tls-min-version` | `VersionTLS12` | Minimum TLS version for the diagnostics endpoint and the webhook server. |
| `--tls-cipher-suites` | `[]` | Comma-separated cipher suites; empty means the Go defaults. |
| `--metrics-bind-addr` | `""` | **Deprecated and ignored.** Retained so existing deployments that still pass it keep starting; a value is logged once and discarded. Use `--diagnostics-address`. |

In the default (secure) mode the endpoint authorises **every request** with a `TokenReview`
followed by a `SubjectAccessReview` against the requested non-resource URL. Paths served:

```
/metrics
/debug/flags/v
/debug/pprof/          /debug/pprof/cmdline   /debug/pprof/profile
/debug/pprof/symbol    /debug/pprof/trace
```

`/debug/pprof/heap`, `/debug/pprof/allocs` and `/debug/pprof/goroutine` are reached through
the `/debug/pprof/` index handler.

## The certificate is self-signed — every client must skip verification

controller-runtime generates the diagnostics endpoint's serving certificate at startup. It
is **self-signed and valid for `localhost` only**; there is no cert-manager Certificate, no
CA bundle published anywhere, and the certificate changes on every pod restart. Nothing can
verify it, so every client has to be told not to try:

| Client | What it needs |
|---|---|
| `curl` | `-k` (equivalently `--insecure`) |
| Prometheus `ServiceMonitor` / `PodMonitor` | `scheme: https` + `tlsConfig.insecureSkipVerify: true` + `bearerTokenFile` |
| Prometheus scrape config | `scheme: https`, `tls_config.insecure_skip_verify: true`, `authorization.credentials_file` |
| Grafana Agent / OTel receiver | the equivalent `insecure_skip_verify` on the TLS block |

`--tls-min-version` (default `VersionTLS12`) and `--tls-cipher-suites` apply to this
endpoint **and** to the webhook server, which shares the same TLS options. They constrain
the handshake; they do not make the certificate verifiable.

Skipping verification is not a downgrade here: confidentiality still holds against a
passive observer, and **authorisation does not rest on TLS at all** — it rests on the
bearer token, which the endpoint checks with a TokenReview and a SubjectAccessReview on
every single request. Prefer `--insecure-diagnostics` only when you genuinely want no
authentication; it also unmounts pprof.

The repo ships a ready ServiceMonitor with these settings at
`config/prometheus/monitor.yaml` (not wired into `config/default` by default).

## Collector RBAC (not shipped — apply on demand)

The provider deliberately ships **no** ClusterRole granting access to `/debug/pprof/*` and
**no** binding. Grant it only for the duration of a collection, then remove it. The existing
`capmaas-metrics-reader` ClusterRole (shipped) covers `/metrics` only.

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: capmaas-pprof-collector
  namespace: cluster-api-provider-maas-system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: capmaas-pprof-reader
rules:
- nonResourceURLs: ["/debug/pprof/*", "/metrics"]
  verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: capmaas-pprof-reader
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: capmaas-pprof-reader
subjects:
- kind: ServiceAccount
  name: capmaas-pprof-collector
  namespace: cluster-api-provider-maas-system
```

> The `/debug/pprof/*` wildcard in `nonResourceURLs` matches the sub-paths but **not** the
> bare `/debug/pprof/` index. Add `"/debug/pprof/"` to the list if you want the index page
> as well. `/debug/flags/v` is intentionally excluded — it can mutate the running log level.

## Collection runbook

```bash
NS=cluster-api-provider-maas-system

kubectl apply -f collector-rbac.yaml            # the YAML above
TOKEN=$(kubectl -n "$NS" create token capmaas-pprof-collector --duration=30m)

kubectl -n "$NS" port-forward deploy/capmaas-controller-manager 18443:8443 &
PF=$!

# -k is required: the endpoint's certificate is self-signed and localhost-only.
curl -sk -H "Authorization: Bearer $TOKEN" \
  https://127.0.0.1:18443/debug/pprof/heap > heap.pb.gz
curl -sk -H "Authorization: Bearer $TOKEN" \
  https://127.0.0.1:18443/debug/pprof/allocs > allocs.pb.gz
curl -sk -H "Authorization: Bearer $TOKEN" \
  "https://127.0.0.1:18443/debug/pprof/goroutine?debug=1" > goroutine.txt
curl -sk -H "Authorization: Bearer $TOKEN" \
  https://127.0.0.1:18443/metrics > metrics.txt

kill $PF
kubectl delete -f collector-rbac.yaml           # revoke when done

go tool pprof -top heap.pb.gz
go tool pprof -top -sample_index=alloc_space allocs.pb.gz
```

Expected status codes, useful when triaging a failed collection:

| Request | Expect |
|---|---|
| any request without `-k` | TLS verification failure — expected, see above |
| no `Authorization` header | `401` |
| token of a ServiceAccount with no matching ClusterRole | `403` |
| token bound to the collector role above | `200` |
| `/debug/flags/v` with the collector token | `403` (not granted) |

## Support bundle

The RBAC above is safe to hand to a customer as a self-contained snippet: it creates one
ServiceAccount plus a read-only ClusterRole/Binding scoped to two non-resource URL prefixes,
grants no access to any Kubernetes object, and is removed with a single
`kubectl delete -f`. A heap profile is a memory-layout snapshot — it contains type and
function names, not object contents — but it should still be handled as customer
environment data.

For a repeat OOM investigation, collect `heap` and `goroutine` at the start of the window
and again just before the restart, so the two can be diffed with
`go tool pprof -base heap-before.pb.gz heap-after.pb.gz`.
