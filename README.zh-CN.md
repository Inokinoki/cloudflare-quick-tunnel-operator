# cloudflare-quick-tunnel-operator

[English](README.md) | **简体中文**

把 Kubernetes 集群里的 Service 通过 **Cloudflare Quick Tunnel**（`trycloudflare.com`）暴露到公网——**不需要 Cloudflare 账号、不需要域名、不需要公网 IP、不需要开任何入站端口**。

- 三种暴露方式：**Ingress**（`ingressClassName: quicktunnel`，按 host 拆隧道）、**Gateway API**（`gatewayClassName: quicktunnel`）、**Service annotation**
- 安装方式：**Helm**（推荐）、原始 manifest、或本地直接运行 operator
- 自愈：自动从 Pod 日志采集 URL、探活、回收僵尸隧道

> 📖 完整教程：**[docs/tutorial.zh-CN.md](docs/tutorial.zh-CN.md)**（另有[英文版](docs/tutorial.md)）——安装细节、日常运维、排障速查、卸载。

## 快速开始

**前置条件**：一个 Kubernetes 集群 + `kubectl` + `helm`。**全程不需要 Cloudflare 账号。**

安装已发布的 chart，再用一条 Ingress 暴露 demo nginx：

```console
# 1. 从 OCI registry 安装（会创建 quicktunnel IngressClass 和 GatewayClass）
$ helm install cloudflare-quick-tunnel-operator \
    oci://ghcr.io/inokinoki/charts/cloudflare-quick-tunnel-operator --version 0.1.0 \
    -n cloudflare-quick-tunnel-operator --create-namespace

# 2. 用一条无 host 规则的 Ingress 暴露 demo nginx
$ kubectl apply -f examples/ingress-hostless.yaml

# 3. 从 Ingress 读公网 URL
$ kubectl get ingress demo-nginx
NAME         CLASS         HOSTS   ADDRESS                                  PORTS   AGE
demo-nginx   quicktunnel   *       quiet-marble-otter.trycloudflare.com     80      30s

$ curl https://quiet-marble-otter.trycloudflare.com
```

镜像是多架构的（linux/amd64 + linux/arm64），可匿名从 GHCR 拉取，远程和云集群同样开箱即用。

## 工作原理

```
   互联网用户
          │ HTTPS
          ▼
   Cloudflare 边缘
          │  quick tunnel（纯出站连接，不开任何入站端口）
          ▼
   cloudflared Pod（每条隧道一个）  ──►  Service ClusterIP  ──►  应用 Pod（任意节点）
          ▲
          │ 创建隧道 / 采集 URL / 探活 / 回收僵尸
          │
   cloudflare-quick-tunnel-operator
          ▲
          │ watch
          │
   Service · Ingress · HTTPRoute（你暴露的对象）
```

1. operator watch 被暴露的对象（打注解的 Service、quicktunnel Ingress、挂 quicktunnel Gateway 的 HTTPRoute），为每条隧道创建一个 `cloudflared tunnel --url ...` Deployment，ownerReference 挂在暴露对象上、随之自动回收。
2. 后台 watchdog 轮询隧道 Pod 日志，提取分配到的 `*.trycloudflare.com` URL，写回 annotation 和 status（`Ingress.status.loadBalancer`、`Gateway.status.addresses`）。
3. watchdog 持续探测 cloudflared 的 `/metrics`（`cloudflared_tunnel_ha_connections`）。Quick Tunnel 有一种常见的"僵尸态"——进程活着但边缘连接全断、不会自愈——探测到后主动删 Pod 重建，并自动回写新 URL。

## 安装

### Helm（推荐）

```console
# 从已发布的 OCI chart 安装——不用 clone、不用构建：
$ helm install cloudflare-quick-tunnel-operator \
    oci://ghcr.io/inokinoki/charts/cloudflare-quick-tunnel-operator --version 0.1.0 \
    -n cloudflare-quick-tunnel-operator --create-namespace

# 或从本地 checkout 安装（比如在改 chart 时）：
$ helm install cloudflare-quick-tunnel-operator charts/cloudflare-quick-tunnel-operator \
    -n cloudflare-quick-tunnel-operator --create-namespace
```

两种方式默认都使用已发布的 GHCR 镜像（多架构 amd64/arm64，tag 有
`0.1.0` / `0.1` / `latest`）；只有跑自己的构建时才需要
`--set image.repository=… --set image.tag=…`。Chart 包含 operator
Deployment、RBAC（含 leader election 的 lease 权限）、`quicktunnel`
IngressClass 和 GatewayClass，均可在 `charts/cloudflare-quick-tunnel-operator/values.yaml`
里开关调整。

### 原始 manifest

```console
$ kubectl apply -f deploy/install.yaml    # 使用已发布的 GHCR 镜像
```

想跑本地构建的镜像：`make minikube-load`，然后把 Deployment 的镜像指到
`cloudflare-quick-tunnel-operator:dev`。从 manifest 迁移到 Helm？先删掉已创建的
IngressClass/GatewayClass——Helm 拒绝收编不属于它的存量资源。

### 本地开发模式

operator 直接跑在本机、对当前 kubeconfig 生效，免打镜像：

```console
$ make build
$ ./bin/cloudflare-quick-tunnel-operator
```

## 配置

operator 的 CLI flags 及对应的 Helm value：

| CLI flag | Helm value | 默认值 | 说明 |
|---|---|---|---|
| `--cloudflared-image` | `cloudflaredImage` | `cloudflare/cloudflared:2026.10.0` | 隧道 Pod 使用的 cloudflared 镜像 |
| `--metrics-bind-address` | `metrics.bindAddress` | `:8080` | operator 的 Prometheus metrics 端点 |
| `--health-probe-bind-address` | `health.bindAddress` | `:8081` | healthz / readyz 端点 |
| `--namespace` | `namespaceScope` | `""`（全部 namespace） | 将 reconcile 限制在单个 namespace |
| `--leader-elect` | `leaderElection` | `false`（chart/manifest 里为 `true`） | 多副本安全运行 |

controller-runtime zap 包的日志 flags（`--zap-log-level`、`--zap-encoder` 等）同样可用。

## 使用

### Ingress——按 host 拆隧道

`ingressClassName: quicktunnel` 就够了。规则按 host 分组，每个 host 一条独立隧道（URL 各自独立）：

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: demo
spec:
  ingressClassName: quicktunnel
  rules:
    - host: web.demo.local        # → 隧道1 + 注解 quicktunnel.dev/url.web.demo.local
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: demo-nginx
                port: { number: 80 }
    - host: api.demo.local        # → 隧道2 + 注解 quicktunnel.dev/url.api.demo.local
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

# 所有公网地址，聚合给标准工具用（external-dns 等）：
$ kubectl get ingress demo -o jsonpath='{.status.loadBalancer.ingress[*].hostname}'
api-xxx.trycloudflare.com web-yyy.trycloudflare.com

# 单个 host 的精确 URL：
$ kubectl get ingress demo -o jsonpath='{.metadata.annotations.quicktunnel\.dev/url\.web\.demo\.local}'
```

映射规则：

- 无 host 的规则共用一条隧道（裸 `quicktunnel.dev/url` 注解），没有规则时 `defaultBackend` 兜底——单 Service 场景见 `examples/ingress-hostless.yaml`；
- 每个 host 组取**第一条规则第一个 Service backend**（path 级路由无法支持——每条 quick tunnel 单 origin）；
- trycloudflare.com 的随机域名**无法真正 serve 规则里的自定义 host**——host 只作为分组和拆隧道的维度。

### Service annotation——一行命令

```console
$ kubectl annotate svc my-svc quicktunnel.dev/expose=true
$ kubectl get svc my-svc -o jsonpath='{.metadata.annotations.quicktunnel\.dev/url}'
https://quiet-marble-otter.trycloudflare.com
```

| 注解 | 说明 | 默认值 |
|---|---|---|
| `quicktunnel.dev/expose` | `"true"` 开启暴露 | — |
| `quicktunnel.dev/port` | 源站端口 | Service 的第一个 port |
| `quicktunnel.dev/scheme` | `http` / `https`（https 会加 `--no-tls-verify`） | `http` |
| `quicktunnel.dev/url` | **operator 写入**：当前公网 URL | — |
| `quicktunnel.dev/url-updated-at` | **operator 写入**：URL 更新时间 | — |

删掉注解即收回（隧道 Deployment 和 URL 注解自动清理）。想让 URL 出现在 Ingress 的标准位置？改用上面的 hostless Ingress。

### Gateway API

需要先装 [Gateway API](https://gateway-api.sigs.k8s.io/) CRDs：

```console
$ kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.4.1/standard-install.yaml
```

**HTTPRoute = 一条隧道**：attach 到 quicktunnel Gateway 的每个 HTTPRoute，取其第一条规则的第一个 Service backendRef 建隧道；URL 写到 route 的 `quicktunnel.dev/url` 注解和 route status（Accepted / ResolvedRefs）。Gateway 把其下所有 route 的 URL 聚合进 `status.addresses`，有 URL 后 Programmed 变 True：

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
quiet-marble-otter.trycloudflare.com    # Hostname 不带 scheme，访问时加 https://
```

集群没装 Gateway API CRDs 时，operator 会打日志提示并跳过该部分，其余模式不受影响。hostname/path 匹配不生效（同 Ingress 的单 origin 理由）。

## 已知限制（来自 Quick Tunnel 本身）

- **URL 与 Pod 生命周期绑定**：隧道 Pod 重启后域名必然变化，annotation/status 会自动更新，但任何把 URL 固化到配置里的用法都会失效——务必动态读取。
- **单隧道 200 并发在途请求**，超出返回 429；适合 dev/demo，不适合压测和生产。
- **不支持 SSE**（Server-Sent Events）。
- **无 SLA**：Cloudflare 可能掐掉空闲隧道；本 operator 通过探活 + 自动重建缓解，但这是 best-effort。
- 所有流量经过 Cloudflare 边缘，注意隐私与合规。

## 同类项目

本 operator 刻意**不引入任何自有 CRD**——暴露方式全部用内置 API 表达（Service 注解、`ingressClassName`、`gatewayClassName`）。如果你想要不同的取舍：

- **稳定域名 / 生产流量**：[STRRL/cloudflare-tunnel-ingress-controller](https://github.com/STRRL/cloudflare-tunnel-ingress-controller)——基于*命名* Cloudflare Tunnel 的 Ingress controller；需要账号、Cloudflare 域名和 API token，但能给固定域名、隧道支持多副本。官方 `cloudflared` 也可用。
- **CRD 方式管理隧道**：[adyanth/cloudflare-operator](https://github.com/adyanth/cloudflare-operator) 通过 `Tunnel` CRD 管理 Cloudflare Tunnel。
- **私有暴露（仅 tailnet）**：[Tailscale Kubernetes operator](https://tailscale.com/kb/1236/kubernetes-operator/) 把 Service 和 Ingress 暴露给你的 tailnet 而不是公网。

## 开发

```console
$ make test    # 单测：reconcile 语义、URL 提取、僵尸探活
$ make vet
```

- Go 1.24 + controller-runtime v0.22，无 CRD，纯 annotation 驱动。
- cloudflared 镜像版本在 `internal/controller/annotations.go` 的 `DefaultCloudflaredImage` 固定，可用 `-cloudflared-image` 覆盖。

## Roadmap

- [x] Gateway API 支持（`gatewayClassName: quicktunnel`，HTTPRoute = 隧道，Gateway.status.addresses 聚合 URL）
- [x] Ingress 支持（`ingressClassName: quicktunnel`，按 host 拆分多隧道，status.loadBalancer 聚合 URL）
- [x] Helm chart
- [x] 通过 CI 把镜像和 Helm chart 发布到公共 registry（GHCR / OCI）
- [ ] operator 自身 metrics（暴露中的隧道数、URL 更新计数）

## License

MIT
