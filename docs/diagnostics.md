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

## Under Palette there is no opt-out

Palette replaces the manager's `args` wholesale from its own overlay, so
`--diagnostics-address` and `--insecure-diagnostics` cannot be set on a Palette-managed
cluster, and a hand edit to the Deployment lasts only until Palette next re-applies it.
The secure default — TLS on `:8443`, every request authorised with a TokenReview and a
SubjectAccessReview — is the only mode there. A scraper that read plain HTTP on `:8080`
must move to `https://…:8443/metrics` with a bearer token for a ServiceAccount bound to the
shipped `capmaas-metrics-reader` ClusterRole, and skip certificate verification.

## Collector RBAC (not shipped — apply on demand)

The provider deliberately ships **no** ClusterRole granting access to `/debug/pprof/*` and
**no** binding. Grant it only for the duration of a collection, then remove it.

The shipped `capmaas-metrics-reader` ClusterRole covers `/metrics` only; a metrics-only
scraper binds to that. Do not edit it, and do not reuse its name for the collector role
below: the runbook's `kubectl delete -f` would then delete the shipped role, and Palette
does not put it back until its next re-apply of the provider.

`CAPMAAS_NS` is the namespace the controller runs in: `cluster-<uid>` for a
Palette-managed cluster, `cluster-api-provider-maas-system` for the bundle in
`spectro/generated/`, `capmaas-system` for a `config/default` install.

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: capmaas-pprof-collector
  namespace: CAPMAAS_NS
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
  namespace: CAPMAAS_NS
```

> `/debug/flags/v` is intentionally excluded — it can mutate the running log level.

## Collection runbook

The primary path is a port-forward to the Deployment. Palette 4.10 and later no longer
ship the `capmaas-controller-manager-metrics-service` Service in per-cluster bundles, so
the Service is optional: use it where it exists, but do not rely on it. From a pod inside
the cluster network the pod IP works too (`https://<pod-ip>:8443/…`, same `-k` and token).

```bash
NS=cluster-<uid>    # the controller's namespace; see CAPMAAS_NS above

sed "s/CAPMAAS_NS/$NS/" collector-rbac.yaml | kubectl apply -f -    # the YAML above
TOKEN=$(kubectl -n "$NS" create token capmaas-pprof-collector --duration=30m)

kubectl -n "$NS" port-forward deploy/capmaas-controller-manager 8443:8443 &
PF=$!

# -k is required: the certificate is self-signed for CN=localhost and no CA is published.
curl -sk -H "Authorization: Bearer $TOKEN" \
  https://localhost:8443/debug/pprof/heap > heap.pb.gz
curl -sk -H "Authorization: Bearer $TOKEN" \
  https://localhost:8443/debug/pprof/allocs > allocs.pb.gz
curl -sk -H "Authorization: Bearer $TOKEN" \
  "https://localhost:8443/debug/pprof/goroutine?debug=1" > goroutine.txt
curl -sk -H "Authorization: Bearer $TOKEN" \
  https://localhost:8443/metrics > metrics.txt

kill $PF
sed "s/CAPMAAS_NS/$NS/" collector-rbac.yaml | kubectl delete -f -   # revoke when done

go tool pprof -top heap.pb.gz
go tool pprof -top -sample_index=alloc_space allocs.pb.gz
```

If local port 8443 is taken, forward another local port (`18443:8443`) and use it in the
URLs.

Expected status codes, useful when triaging a failed collection:

| Request | Expect |
|---|---|
| any request without `-k` | TLS verification failure — expected, see above |
| no `Authorization` header | `401` |
| token of a ServiceAccount with no matching ClusterRole | `403` |
| token bound to the collector role above | `200` |
| `/debug/flags/v` with the collector token | `403` (not granted) |

### The webhook-only copy in `capi-webhook-system` is not a collection target

Palette also runs a webhook-only copy of the manager in `capi-webhook-system`. It binds
`:8443` too, but it runs as the namespace's `default` ServiceAccount, which is not granted
`capmaas-proxy-role`, so it cannot create the TokenReview the filter needs: a request
carrying a token gets `500`, not `401` or `403` (a tokenless request still gets `401`).
This is expected and deferred. The fix — grant that ServiceAccount `capmaas-proxy-role`, or
disable the endpoint there with `--diagnostics-address=0` — must ship together with the
image bump that carries this change, because older images do not have the flag. Collect
from the controller in the cluster's own namespace instead.

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
