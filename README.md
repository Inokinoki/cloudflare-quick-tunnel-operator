# cloudflare-quick-tunnel-operator

**English** | [简体中文](README.zh-CN.md)

Expose Kubernetes Services to the public internet via **Cloudflare Quick Tunnels** (`trycloudflare.com`) — **no Cloudflare account, no domain, no public IP, no inbound ports**.

- Three exposure modes: **Ingress** (`ingressClassName: quicktunnel`, one tunnel per host), **Gateway API** (`gatewayClassName: quicktunnel`), or plain **Service annotations**
- Install with **Helm**, a raw manifest, or run the operator locally against any kubeconfig
- Self-healing: harvests tunnel URLs from pod logs, probes cloudflared health, recycles zombie tunnels automatically

> 📖 Full tutorial: **[docs/tutorial.md](docs/tutorial.md)** (also in [简体中文](docs/tutorial.zh-CN.md)) — installation details, day-2 operations, troubleshooting, uninstall.

## Quick start

**Prerequisites**: a Kubernetes cluster with `kubectl` and `helm`, plus Docker
to build the operator image (until a public image is published). No Cloudflare
account is needed — ever.

Install with Helm, then expose a demo nginx through an Ingress:

```console
# 1. Build the operator image (no public image is published yet) and load it
$ make docker-build && minikube image load cloudflare-quick-tunnel-operator:dev

# 2. Install via Helm (creates the quicktunnel IngressClass & GatewayClass)
$ helm install cloudflare-quick-tunnel-operator charts/cloudflare-quick-tunnel-operator \
    -n cloudflare-quick-tunnel-operator --create-namespace

# 3. Expose a demo nginx through a hostless Ingress
$ kubectl apply -f examples/ingress-hostless.yaml

# 4. Read the public URL from the Ingress
$ kubectl get ingress demo-nginx
NAME         CLASS         HOSTS   ADDRESS                                  PORTS   AGE
demo-nginx   quicktunnel   *       quiet-marble-otter.trycloudflare.com     80      30s

$ curl https://quiet-marble-otter.trycloudflare.com
```

On a remote/cloud cluster, push the image to a registry and add
`--set image.repository=<registry>/<image> --set image.tag=<tag>` to the
helm install command — see the tutorial for details.

## How it works

```
   users on the internet
          │ HTTPS
          ▼
   Cloudflare edge
          │  quick tunnel (outbound-only, no inbound ports)
          ▼
   cloudflared pod (one per tunnel)  ──►  Service ClusterIP  ──►  app pods (any node)
          ▲
          │ creates tunnel / harvests URL / probes health / recycles zombies
          │
   cloudflare-quick-tunnel-operator
          ▲
          │ watches
          │
   Service · Ingress · HTTPRoute (whatever you expose)
```

1. The operator watches exposed objects (annotated Services, quicktunnel Ingresses, HTTPRoutes on a quicktunnel Gateway) and creates a `cloudflared tunnel --url ...` Deployment per tunnel, ownerReferenced to the exposing object so it is garbage-collected with it.
2. A background watchdog polls the tunnel pod's logs, extracts the assigned `*.trycloudflare.com` URL, and publishes it to annotations and status (`Ingress.status.loadBalancer`, `Gateway.status.addresses`).
3. The watchdog keeps probing cloudflared's `/metrics` (`cloudflared_tunnel_ha_connections`). Quick Tunnels have a known "zombie" mode — the process stays alive with zero edge connections and never recovers — and the operator recycles the pod when it detects it, then republishes the new URL.

## Installation

### Helm (recommended)

```console
$ make docker-build && minikube image load cloudflare-quick-tunnel-operator:dev   # or push to a registry
$ helm install cloudflare-quick-tunnel-operator charts/cloudflare-quick-tunnel-operator \
    -n cloudflare-quick-tunnel-operator --create-namespace

# Using a registry image:
$ helm install cloudflare-quick-tunnel-operator charts/cloudflare-quick-tunnel-operator \
    -n cloudflare-quick-tunnel-operator --create-namespace \
    --set image.repository=ghcr.io/<you>/cloudflare-quick-tunnel-operator \
    --set image.tag=v0.1.0
```

The chart ships the operator Deployment, RBAC (including leader-election
leases), and the `quicktunnel` IngressClass & GatewayClass — each toggleable
via `charts/cloudflare-quick-tunnel-operator/values.yaml`.

### Raw manifest

```console
$ make minikube-load          # docker build + minikube image load
$ make deploy                 # kubectl apply -f deploy/install.yaml
```

Equivalent to the chart; minikube users without a standalone kubectl can use
`KUBECTL="minikube kubectl --" make deploy`. Migrating from the manifest to
Helm? Delete the previously created IngressClass/GatewayClass first — Helm
refuses to adopt resources it does not own.

### Local dev mode

Run the operator on your machine against the current kubeconfig — no image
needed:

```console
$ make build
$ ./bin/cloudflare-quick-tunnel-operator
```

## Configuration

Operator CLI flags and their Helm value equivalents:

| CLI flag | Helm value | Default | Description |
|---|---|---|---|
| `--cloudflared-image` | `cloudflaredImage` | `cloudflare/cloudflared:2026.10.0` | cloudflared image used for tunnel pods |
| `--metrics-bind-address` | `metrics.bindAddress` | `:8080` | Prometheus metrics endpoint of the operator |
| `--health-probe-bind-address` | `health.bindAddress` | `:8081` | healthz / readyz endpoint |
| `--namespace` | `namespaceScope` | `""` (all namespaces) | restrict reconciliation to one namespace |
| `--leader-elect` | `leaderElection` | `false` (chart/manifest: `true`) | safe multi-replica operation |

Logging flags from controller-runtime's zap package (`--zap-log-level`,
`--zap-encoder`, …) are also available.

## Usage

### Ingress — one tunnel per host

`ingressClassName: quicktunnel` is all it takes. Rules are grouped by host;
each host gets its own tunnel (and its own URL):

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: demo
spec:
  ingressClassName: quicktunnel
  rules:
    - host: web.demo.local        # → tunnel 1 + annotation quicktunnel.dev/url.web.demo.local
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: demo-nginx
                port: { number: 80 }
    - host: api.demo.local        # → tunnel 2 + annotation quicktunnel.dev/url.api.demo.local
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: demo-echo
                port: { number: 5678 }
```

```console
$ kubectl apply -f examples/ingress-demo.yaml

# All public addresses, aggregated for standard tooling (external-dns & co):
$ kubectl get ingress demo -o jsonpath='{.status.loadBalancer.ingress[*].hostname}'
api-xxx.trycloudflare.com web-yyy.trycloudflare.com

# Per-host exact URL:
$ kubectl get ingress demo -o jsonpath='{.metadata.annotations.quicktunnel\.dev/url\.web\.demo\.local}'
```

Mapping notes:

- Hostless rules share one tunnel (bare `quicktunnel.dev/url` annotation), with `defaultBackend` as fallback when there are no rules — see `examples/ingress-hostless.yaml` for the single-Service case;
- Each host group uses the **first Service backend of its first rule** (path-level routing cannot be honored — a quick tunnel is a single origin);
- The random trycloudflare.com hostname **cannot serve the rule's own host** — hosts are purely the grouping/tunnel-splitting dimension.

### Service annotation — the one-liner

```console
$ kubectl annotate svc my-svc quicktunnel.dev/expose=true
$ kubectl get svc my-svc -o jsonpath='{.metadata.annotations.quicktunnel\.dev/url}'
https://quiet-marble-otter.trycloudflare.com
```

| Annotation | Description | Default |
|---|---|---|
| `quicktunnel.dev/expose` | `"true"` opts a Service in | — |
| `quicktunnel.dev/port` | Origin port | the Service's first port |
| `quicktunnel.dev/scheme` | `http` / `https` (https adds `--no-tls-verify`) | `http` |
| `quicktunnel.dev/url` | **written by the operator**: current public URL | — |
| `quicktunnel.dev/url-updated-at` | **written by the operator**: last URL update time | — |

Remove the annotation to stop exposing (tunnel Deployment and URL annotations are cleaned up automatically). Prefer the URL on standard Ingress fields instead? Use a hostless Ingress as shown above.

### Gateway API

Requires the [Gateway API](https://gateway-api.sigs.k8s.io/) CRDs:

```console
$ kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.4.1/standard-install.yaml
```

**HTTPRoute = one tunnel**: each route attached to a quicktunnel Gateway gets a tunnel targeting the first Service backendRef of its first rule; the URL surfaces on the route's `quicktunnel.dev/url` annotation and its status (Accepted / ResolvedRefs). The Gateway aggregates all route URLs into `status.addresses` and turns Programmed once at least one exists:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: demo
spec:
  gatewayClassName: quicktunnel
  listeners:
    - name: http
      port: 80
      protocol: HTTP
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: demo
spec:
  parentRefs:
    - name: demo
  rules:
    - backendRefs:
        - name: demo-nginx
          port: 80
```

```console
$ kubectl apply -f examples/gateway-demo.yaml
$ kubectl get gateway demo -o jsonpath='{.status.addresses[0].value}'
quiet-marble-otter.trycloudflare.com    # Hostname carries no scheme; prepend https://
```

If the Gateway API CRDs are not installed, the operator logs a notice and
skips Gateway API support; the other modes are unaffected. Hostnames and
path matches are not honored (same single-origin reasoning as Ingress).

## Known limitations (inherent to Quick Tunnels)

- **URLs are tied to the pod lifecycle**: the hostname always changes when a tunnel pod restarts. Annotations/status are updated automatically, but anything that hard-codes a URL into configuration will break — always read dynamically.
- **200 concurrent in-flight requests per tunnel** — extra requests get 429. Fine for dev/demo, not for load tests or production.
- **SSE (Server-Sent Events) is not supported.**
- **No SLA**: Cloudflare may drop idle tunnels. The operator mitigates this with probing and automatic rebuilds, but it remains best-effort.
- All traffic traverses Cloudflare's edge — consider privacy and compliance.

## Alternatives

This operator deliberately adds **no CRDs of its own** — exposure is declared
with built-in APIs only (Service annotations, `ingressClassName`,
`gatewayClassName`). If you prefer different trade-offs:

- **Stable hostnames / production traffic**: [STRRL/cloudflare-tunnel-ingress-controller](https://github.com/STRRL/cloudflare-tunnel-ingress-controller) — Ingress controller over *named* Cloudflare Tunnels; needs an account, a Cloudflare domain and an API token, but gives fixed hostnames and multi-replica tunnels. Plain `cloudflared` also works.
- **CRD-based tunnel management**: [adyanth/cloudflare-operator](https://github.com/adyanth/cloudflare-operator) manages Cloudflare Tunnels through a `Tunnel` CRD.
- **Private exposure (tailnet-only)**: the [Tailscale Kubernetes operator](https://tailscale.com/kb/1236/kubernetes-operator/) exposes Services and Ingresses to your tailnet instead of the public internet.

## Development

```console
$ make test    # unit tests: reconcile semantics, URL extraction, zombie probing
$ make vet
```

- Go 1.24 + controller-runtime v0.22, no CRDs — purely annotation- and class-driven.
- The cloudflared image is pinned in `DefaultCloudflaredImage` (`internal/controller/annotations.go`); override with `-cloudflared-image`.

## Roadmap

- [x] Gateway API support (`gatewayClassName: quicktunnel`, HTTPRoute = tunnel, Gateway.status.addresses aggregates URLs)
- [x] Ingress support (`ingressClassName: quicktunnel`, one tunnel per host, status.loadBalancer aggregates URLs)
- [x] Helm chart
- [ ] Operator metrics (tunnels exposed, URL update counts)
- [ ] Publish images and the Helm chart to a public registry (GHCR / OCI) via CI

## License

MIT
