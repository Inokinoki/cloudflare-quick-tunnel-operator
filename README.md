# cloudflare-quick-tunnel-operator

**English** | [简体中文](README.zh-CN.md)

Expose Kubernetes Services to the public internet via **Cloudflare Quick Tunnels** (`trycloudflare.com`) — **no Cloudflare account, no domain, no public IP, no inbound ports**.

> 📖 Full installation and usage tutorial: **[docs/tutorial.md](docs/tutorial.md)** (all three exposure modes, day-2 commands, troubleshooting, uninstall). Also available in [Chinese](docs/tutorial.zh-CN.md).

Three exposure modes are supported: Service annotation (simplest), Ingress (`ingressClassName: quicktunnel`, one tunnel per host), and Gateway API (`gatewayClassName: quicktunnel`).

Annotate a Service, and a few seconds later the operator writes the public URL back:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: demo-nginx
  annotations:
    quicktunnel.dev/expose: "true"
spec:
  selector:
    app: demo-nginx
  ports:
    - port: 80
```

```console
$ kubectl get svc demo-nginx -o jsonpath='{.metadata.annotations.quicktunnel\.dev/url}'
https://quiet-marble-otter.trycloudflare.com
```

## How it works

```
                    ┌───────────────────────── Kubernetes cluster ─────────────────────────┐
                    │                                                                       │
  user browser      │  ┌────────────┐ creates ┌─────────────────┐   forwards ┌───────────┐  │
──────────────┐    │  │  Service    │◄───────│ cloudflared Pod │───────────►│  app Pod  │  │
              │    │  │ (annotation │        │ (one per tunnel)│            └───────────┘  │
              ▼    │  │  + URL back)│        └────────┬────────┘                           │
 Cloudflare edge ◄──┼──────────────────────────────────┘ outbound-only tunnel               │
                    │        ▲ harvest URL from logs / probe health / recycle zombies      │
                    │  ┌─────┴───────────────────┐                                       │
                    │  │ cloudflare-quick-tunnel-operator │                                 │
                    │  └─────────────────────────┘                                       │
                    └───────────────────────────────────────────────────────────────────────┘
```

1. The operator watches Services annotated with `quicktunnel.dev/expose: "true"` and creates a `cloudflared tunnel --url ...` Deployment per exposed owner (via ownerReference, so it is garbage-collected with the owner).
2. A background watchdog polls the tunnel pod's logs, extracts the assigned `*.trycloudflare.com` URL, and patches it back into the owner's `quicktunnel.dev/url` annotation.
3. The watchdog keeps probing cloudflared's `/metrics` (`cloudflared_tunnel_ha_connections`). Quick Tunnels have a known "zombie" mode — the process stays alive with zero edge connections and never recovers — and the operator recycles the pod when it detects it. The new URL is republished automatically.

## Annotation reference

| Annotation | Description | Default |
|---|---|---|
| `quicktunnel.dev/expose` | `"true"` opts a Service in | — |
| `quicktunnel.dev/port` | Origin port | the Service's first port |
| `quicktunnel.dev/scheme` | `http` / `https` (https adds `--no-tls-verify`) | `http` |
| `quicktunnel.dev/url` | **written by the operator**: current public URL | — |
| `quicktunnel.dev/url-updated-at` | **written by the operator**: last URL update time | — |

## Quick start (minikube)

```console
$ minikube start
$ make build
$ make run                     # run the operator locally against the current kubeconfig
# in another terminal:
$ kubectl apply -f examples/demo.yaml
$ kubectl get svc demo-nginx -o jsonpath='{.metadata.annotations.quicktunnel\.dev/url}'
https://quiet-marble-otter.trycloudflare.com
$ curl https://quiet-marble-otter.trycloudflare.com
```

Deploying in-cluster:

```console
$ make minikube-load          # docker build + minikube image load
$ make deploy                 # kubectl apply -f deploy/install.yaml
```

minikube users without a standalone kubectl can use `KUBECTL="minikube kubectl --" make deploy`.

## Gateway API support

The standard [Gateway API](https://gateway-api.sigs.k8s.io/) is also supported (CRDs required):

```console
$ kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.4.1/standard-install.yaml
```

`deploy/install.yaml` ships a `GatewayClass quicktunnel` (controllerName `quicktunnel.dev/gateway`). The mapping:

- **HTTPRoute = one tunnel**: each HTTPRoute attached to a quicktunnel Gateway gets a tunnel targeting the first Service backendRef of its first rule; the URL surfaces on the route's `quicktunnel.dev/url` annotation and route status (Accepted / ResolvedRefs);
- **Gateway = URL aggregator**: the URLs of all attached routes are collected into `status.addresses`, and the Gateway turns Programmed once at least one URL exists — this is the "official" place to read addresses;
- **hostname / path matches are not honored**: trycloudflare.com names are random and cannot serve custom hostnames; only the first rule applies (a quick tunnel is a single origin).

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
$ kubectl get gateway demo -o jsonpath='{.status.addresses[0].value}'
quiet-marble-otter.trycloudflare.com    # Hostname addresses carry no scheme; prepend https://
```

See `examples/gateway-demo.yaml` for a complete example. If the Gateway API CRDs are not installed, the operator logs a notice and skips Gateway API support; Service annotation and Ingress modes are unaffected.

## Ingress support

Classic Ingress is supported too (`deploy/install.yaml` ships an `IngressClass quicktunnel`). The mapping:

- **One tunnel per host**: Ingress rules are grouped by host, and each host gets its own tunnel (and URL); hostless rules share one, with `defaultBackend` as fallback;
- each host group uses the **first Service backend of its first rule** (path-level routing cannot be honored — single origin);
- URLs land on the Ingress's `quicktunnel.dev/url.<host>` annotations (bare `quicktunnel.dev/url` for the hostless group) and are aggregated into **`status.loadBalancer.ingress`** — the field standard tooling such as external-dns reads (Hostname carries no scheme);
- the random trycloudflare.com hostname **cannot actually serve the rule's own host** — hosts are purely the grouping/tunnel-splitting dimension.

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: demo
spec:
  ingressClassName: quicktunnel
  rules:
    - host: web.demo.local        # → tunnel 1 + quicktunnel.dev/url.web.demo.local
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: demo-nginx
                port: { number: 80 }
    - host: api.demo.local        # → tunnel 2 + quicktunnel.dev/url.api.demo.local
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
$ kubectl get ingress demo -o jsonpath='{.status.loadBalancer.ingress[*].hostname}'
api-xxx.trycloudflare.com web-yyy.trycloudflare.com
```

Full example: `examples/ingress-demo.yaml`.

### Exposing a single Service via Ingress

A hostless Ingress is the Ingress-mode equivalent of the Service annotation: one tunnel, and the URL becomes readable from the standard Ingress fields (`kubectl get ingress` ADDRESS column, `status.loadBalancer`):

```console
$ kubectl apply -f examples/ingress-hostless.yaml
$ kubectl get ingress demo-nginx
NAME         CLASS         HOSTS   ADDRESS                                  PORTS   AGE
demo-nginx   quicktunnel   *       quiet-marble-otter.trycloudflare.com     80      30s
```

The full `https://` URL also lands on the bare `quicktunnel.dev/url` annotation. Do not combine this with `quicktunnel.dev/expose` on the same Service — you would get two independent tunnels and two URLs.

## Known limitations (inherent to Quick Tunnels)

- **URLs are tied to the pod lifecycle**: the hostname always changes when a tunnel pod restarts. Annotations are updated automatically, but anything that hard-codes a URL into configuration will break.
- **200 concurrent in-flight requests per tunnel** — extra requests get 429. Fine for dev/demo, not for load tests or production.
- **SSE (Server-Sent Events) is not supported.**
- **No SLA**: Cloudflare may drop idle tunnels. The operator mitigates this with probing and automatic rebuilds, but it remains best-effort.
- All traffic traverses Cloudflare's edge — consider privacy and compliance.

For stable hostnames and production traffic, switch to named tunnels (requires an account): official `cloudflared`, or [STRRL/cloudflare-tunnel-ingress-controller](https://github.com/STRRL/cloudflare-tunnel-ingress-controller).

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
- [ ] Operator metrics (tunnels exposed, URL update counts)

## License

MIT
