# cloudflare-quick-tunnel-operator — Installation & Usage Tutorial

**English** | [简体中文](tutorial.zh-CN.md)

A hands-on tutorial; every command has been verified on minikube + Kubernetes v1.33. The three exposure modes can be mixed freely:

| Mode | Best for | Prerequisite |
|---|---|---|
| Service annotation | A single Service, fastest | none |
| Ingress | Multiple Services, split by host | none |
| Gateway API | The new standard, going forward | Gateway API CRDs |

---

## 0. Prerequisites

- A Kubernetes cluster (minikube / kind / any cloud cluster) + `kubectl`
- In-cluster deployment: Docker (to build the image)
- Local dev mode: Go 1.24+ (the Makefile auto-detects `~/go/bin/go`)

The cluster does **not** need a public IP or any open inbound ports — quick tunnels are outbound-only.

## 1. Installation

### Option A: in-cluster deployment (production use)

```console
$ git clone <repo> && cd cloudflare-quick-tunnel-operator

# Build the image (use docker buildx for multi-arch)
$ make docker-build                          # → cloudflare-quick-tunnel-operator:dev

# Load the image into the cluster:
#   minikube:
$ minikube image load cloudflare-quick-tunnel-operator:dev
#   kind:
$ kind load docker-image cloudflare-quick-tunnel-operator:dev --name <cluster>

# One-shot install: Namespace + ServiceAccount + ClusterRole + operator Deployment
#                    + IngressClass quicktunnel + GatewayClass quicktunnel
$ kubectl apply -f deploy/install.yaml

# Wait for readiness
$ kubectl -n cloudflare-quick-tunnel-operator wait deploy/cloudflare-quick-tunnel-operator --for=condition=Available --timeout=90s
```

minikube users without a standalone kubectl can use `KUBECTL="minikube kubectl --" make deploy`.

### 1.1 Remote / cloud clusters (push the image to a registry)

Remote clusters cannot side-load images. Push the built image to a registry the cluster can pull from (Docker Hub / GHCR / private), then point `deploy/install.yaml`'s `image:` at it:

```console
$ export IMAGE=ghcr.io/<your-username>/cloudflare-quick-tunnel-operator:v0.1.0   # or docker.io/<you>/...

# Build and push (x86 clusters)
$ docker build -t $IMAGE .
$ docker push $IMAGE

# ARM clusters (Raspberry Pi / free Oracle ARM boxes) — build both arches:
$ docker buildx build --platform linux/amd64,linux/arm64 -t $IMAGE --push .

# Install and point at your image
$ kubectl apply -f deploy/install.yaml
$ kubectl -n cloudflare-quick-tunnel-operator set image deploy/cloudflare-quick-tunnel-operator manager=$IMAGE

# Verify
$ kubectl -n cloudflare-quick-tunnel-operator rollout status deploy/cloudflare-quick-tunnel-operator
```

Private registries need pull credentials:

```console
$ kubectl -n cloudflare-quick-tunnel-operator create secret docker-registry regcred \
    --docker-server=<registry> --docker-username=<user> --docker-password=<password>
$ kubectl -n cloudflare-quick-tunnel-operator patch deploy cloudflare-quick-tunnel-operator \
    -p '{"spec":{"template":{"spec":{"imagePullSecrets":[{"name":"regcred"}]}}}}'
```

> Note: the operator's tunnel health probe goes through the API server's `pods/proxy` subresource (RBAC included in install.yaml). Standard clusters (kubeadm/EKS/GKE/ACK/TKE/…) support it. If your platform restricts that subresource, you will see repeated `tunnel wedged, recycling tunnel pod` log lines — a known boundary.

If you'd rather install nothing in the remote cluster, use [local dev mode](#option-b-local-dev-mode-no-image-needed-recommended-for-a-first-try) with `KUBECONFIG` pointing at the remote cluster: the operator runs on your machine while the tunnel pods run in the cluster (the kubeconfig identity needs the permissions of the ClusterRole in install.yaml; cloud admin kubeconfigs qualify out of the box).

### Option B: local dev mode (no image needed — recommended for a first try)

The operator runs on your machine and talks to the cluster via kubeconfig:

```console
$ make build
$ ./bin/cloudflare-quick-tunnel-operator
```

> Note: local mode requires the kubeconfig identity to operate Services/Ingresses/Deployments/Pods (the default admin kubeconfig of minikube/kind is fine). If port 8080 is taken on your machine, add `-metrics-bind-address :18080`.

### Verify the installation

```console
$ kubectl -n cloudflare-quick-tunnel-operator logs deploy/cloudflare-quick-tunnel-operator | head -5
# "starting cloudflare-quick-tunnel-operator" means success;
# without the Gateway API CRDs installed you'll also see
# "skipping Gateway API support" — that's normal, Service annotation
# and Ingress modes are unaffected.
```

## 2. Mode 1: Service annotation (simplest)

Annotate any Service:

```console
$ kubectl annotate svc my-svc quicktunnel.dev/expose=true
```

Within ~15 seconds the URL shows up in an annotation:

```console
$ kubectl get svc my-svc -o jsonpath='{.metadata.annotations.quicktunnel\.dev/url}'
https://quiet-marble-otter.trycloudflare.com

$ curl https://quiet-marble-otter.trycloudflare.com
```

Optional tweaks:

```yaml
metadata:
  annotations:
    quicktunnel.dev/expose: "true"
    quicktunnel.dev/port: "8080"    # defaults to the Service's first port
    quicktunnel.dev/scheme: "https" # for https origins (adds --no-tls-verify)
```

Stop exposing (tunnel Deployment is removed and URL annotations cleaned up automatically):

```console
$ kubectl annotate svc my-svc quicktunnel.dev/expose-
```

## 3. Mode 2: Ingress (one tunnel per host)

`install.yaml` already created the `IngressClass quicktunnel`. Each host rule gets its own tunnel:

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: demo
spec:
  ingressClassName: quicktunnel
  rules:
    - host: web.demo.local      # → tunnel 1, URL in quicktunnel.dev/url.web.demo.local
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service: { name: demo-nginx, port: { number: 80 } }
    - host: api.demo.local      # → tunnel 2, URL in quicktunnel.dev/url.api.demo.local
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service: { name: demo-echo, port: { number: 5678 } }
```

```console
$ kubectl apply -f examples/ingress-demo.yaml

# All public addresses are aggregated in status.loadBalancer
# (this is what external-dns & friends read):
$ kubectl get ingress demo -o jsonpath='{.status.loadBalancer.ingress[*].hostname}'
api-xxx.trycloudflare.com web-yyy.trycloudflare.com

# The exact address for one host:
$ kubectl get ingress demo -o jsonpath='{.metadata.annotations.quicktunnel\.dev/url\.web\.demo\.local}'
```

Hostless rules fall into the default group (bare `quicktunnel.dev/url`); `defaultBackend` is the fallback when there are no rules. Path-level routing cannot be honored (each tunnel is a single origin); the first backend of each host group wins.

## 4. Mode 3: Gateway API

Install the CRDs first (one-time):

```console
$ kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.4.1/standard-install.yaml
```

`install.yaml` already created the `GatewayClass quicktunnel`:

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

# One tunnel per HTTPRoute; the Gateway aggregates all route URLs:
$ kubectl get gateway demo -o jsonpath='{.status.addresses[0].value}'
quiet-marble-otter.trycloudflare.com      # Hostname carries no scheme; prepend https://

# The route's own status:
$ kubectl get httproute demo -o jsonpath='{.status.parents[0].conditions[*].type}'
Accepted ResolvedRefs
```

## 5. Day-2 operations

```console
# List every tunnel pod (all modes share one label)
$ kubectl get pods -A -l app.kubernetes.io/managed-by=cloudflare-quick-tunnel-operator

# Inspect one tunnel's cloudflared logs (edge connections, current URL)
$ kubectl logs <tunnel-pod> | grep -E 'trycloudflare|Registered'

# Operator logs: URL publications and zombie recycles are all recorded
$ kubectl -n cloudflare-quick-tunnel-operator logs deploy/cloudflare-quick-tunnel-operator | grep watchdog
```

**URL lifecycle** (the important contract): a URL is tied to its tunnel pod's lifetime. Whenever the pod is rebuilt (node drain, image update, health-probe recycle) the hostname changes, and annotations/status catch up within seconds. **Consumers must read the URL dynamically** — never bake it into configuration.

## 6. Troubleshooting quick reference

| Symptom | Cause & fix |
|---|---|
| No URL in annotations after a while | Wait 10–20s; check the tunnel pod is Running (slow image pull); `kubectl describe pod` |
| curl returns 530 | Usually a stale URL from a rebuilt pod — re-read the annotation; or the backend Service port is wrong |
| Occasional 429 | 200 concurrent in-flight request limit per tunnel (hard Quick Tunnel limit) |
| SSE / EventSource broken | Quick Tunnels do not support SSE — platform limitation |
| Local mode fails with `bind: address already in use` | Add `-metrics-bind-address :18080` |
| Logs show `tunnel wedged, recycling tunnel pod` | Normal self-healing: a zombie tunnel was detected and rebuilt; the URL follows |
| Ingress/Gateway mode does nothing | Gateway API mode needs the CRDs (section 4); Ingress does not |

## 7. Uninstall

```console
$ kubectl delete -f deploy/install.yaml        # operator, RBAC, IngressClass, GatewayClass
$ kubectl delete -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.4.1/standard-install.yaml  # optional
```

Tunnel Deployments are ownerReferenced to their Service/Ingress/HTTPRoute, so Kubernetes GC removes them automatically — no manual cleanup needed.
