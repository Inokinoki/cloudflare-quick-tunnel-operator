# cloudflare-quick-tunnel-operator

[English](README.md) | **简体中文**

把 Kubernetes 集群里的 Service 通过 **Cloudflare Quick Tunnel**（`trycloudflare.com`）暴露到公网——**不需要 Cloudflare 账号、不需要域名、不需要公网 IP、不需要开任何入站端口**。

> 📖 完整的安装与使用教程见 **[docs/tutorial.zh-CN.md](docs/tutorial.zh-CN.md)**（三种暴露方式、运维命令、排障速查、卸载）。

支持三种暴露方式：Service annotation（最简）、Ingress（`ingressClassName: quicktunnel`，按 host 拆隧道）、Gateway API（`gatewayClassName: quicktunnel`）。

给 Service 打一个 annotation，几秒钟后 operator 会把公网 URL 写回来：

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

## 工作原理

```
                    ┌─────────────────────────── Kubernetes 集群 ───────────────────────────┐
                    │                                                                        │
  用户浏览器        │  ┌────────────┐  创建   ┌─────────────────┐   转发    ┌────────────┐    │
──────────────┐    │  │  Service    │◄───────│ cloudflared Pod │──────────►│  应用 Pod   │    │
              │    │  │ (annotation │        │ (每 Service 一个)│           └────────────┘    │
              ▼    │  │  + URL 回写) │        └────────┬────────┘                             │
   Cloudflare 边缘 ◄┼───────────────────────────────────┘ 出站隧道（outbound-only）           │
                    │        ▲ 采集日志里的 URL / 探活 / 僵尸重启                               │
                    │  ┌─────┴──────────────┐                                             │
                    │  │ cloudflare-quick-tunnel-operator │                                             │
                    │  └────────────────────┘                                             │
                    └────────────────────────────────────────────────────────────────────────┘
```

1. operator watch 带 `quicktunnel.dev/expose: "true"` 的 Service，为每个 Service 创建一个 `cloudflared tunnel --url ...` 的 Deployment（ownerReference 挂在 Service 上，随 Service 删除自动回收）。
2. 后台 watchdog 轮询 tunnel Pod 的日志，提取分配到的 `*.trycloudflare.com` URL，patch 回 Service 的 `quicktunnel.dev/url` annotation。
3. watchdog 持续探测 cloudflared 的 `/metrics`（`cloudflared_tunnel_ha_connections`）。Quick Tunnel 有一种常见的"僵尸态"：进程活着但边缘连接全断、不会自愈——探测到后主动删 Pod 让 Deployment 重建，新 URL 会自动回写。

## Annotation 参考

| annotation | 说明 | 默认值 |
|---|---|---|
| `quicktunnel.dev/expose` | `"true"` 开启暴露 | — |
| `quicktunnel.dev/port` | 源站端口 | Service 的第一个 port |
| `quicktunnel.dev/scheme` | `http` / `https`（https 会加 `--no-tls-verify`） | `http` |
| `quicktunnel.dev/url` | **operator 写入**：当前公网 URL | — |
| `quicktunnel.dev/url-updated-at` | **operator 写入**：URL 更新时间 | — |

## 快速开始（minikube）

```console
$ minikube start
$ make build
$ make run                     # 本地跑 operator（用当前 kubeconfig，免打镜像）
# 另开一个终端：
$ kubectl apply -f examples/demo.yaml
$ kubectl get svc demo-nginx -o jsonpath='{.metadata.annotations.quicktunnel\.dev/url}'
https://quiet-marble-otter.trycloudflare.com
$ curl https://quiet-marble-otter.trycloudflare.com
```

集群内部署：

```console
$ make minikube-load          # docker build + minikube image load
$ make deploy                 # kubectl apply -f deploy/install.yaml
```

或使用 Helm：

```console
$ helm install cloudflare-quick-tunnel-operator charts/cloudflare-quick-tunnel-operator \
    -n cloudflare-quick-tunnel-operator --create-namespace
```

minikube 用户可以 `KUBECTL="minikube kubectl --" make deploy`。

## Gateway API 支持

除了 Service annotation，也支持标准 [Gateway API](https://gateway-api.sigs.k8s.io/)（需要先装 CRDs）：

```console
$ kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.4.1/standard-install.yaml
```

`deploy/install.yaml` 自带 `GatewayClass quicktunnel`（controllerName `quicktunnel.dev/gateway`）。映射关系：

- **HTTPRoute = 一条隧道**：attach 到 quicktunnel Gateway 的每个 HTTPRoute，取其第一条规则的第一个 Service backendRef 建一条 tunnel，URL 写到 route 的 `quicktunnel.dev/url` annotation 和 route status（Accepted / ResolvedRefs）；
- **Gateway = URL 聚合器**：其下所有 route 的 URL 汇总进 `status.addresses`，有 URL 后 Programmed 变 True——这是读地址的"官方"位置；
- **hostnames / path 匹配不生效**：trycloudflare.com 域名是随机分配的，无法承诺自定义主机名；多条 rule 也只取第一条（quick tunnel 单 origin 限制）。

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
quiet-marble-otter.trycloudflare.com    # Hostname 类型不带 scheme，访问时加 https://
```

完整示例见 `examples/gateway-demo.yaml`。若集群未装 Gateway API CRDs，operator 会跳过 Gateway API 支持并打日志提示，Service annotation 模式不受影响。

## Ingress 支持

传统 Ingress 也支持（`deploy/install.yaml` 自带 `IngressClass quicktunnel`）。映射关系：

- **按 host 拆分隧道**：Ingress 规则按 host 分组，每个 host 一条独立隧道（URL 各自独立）；无 host 的规则共用一条，`defaultBackend` 兜底；
- 每个 host 组取**第一条规则第一个 Service backend**（path 级路由无法支持，单 origin 限制）；
- URL 写到 Ingress 的 `quicktunnel.dev/url.<host>` annotation（无 host 组用裸 `quicktunnel.dev/url`），并聚合进 **`status.loadBalancer.ingress`**（external-dns 等标准工具读这里，Hostname 不带 scheme）；
- trycloudflare.com 的随机域名**无法真正 serve 规则里的自定义 host**——host 只作为分组和拆隧道的维度。

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: demo
spec:
  ingressClassName: quicktunnel
  rules:
    - host: web.demo.local        # → 一条隧道 + quicktunnel.dev/url.web.demo.local
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: demo-nginx
                port: { number: 80 }
    - host: api.demo.local        # → 另一条隧道 + quicktunnel.dev/url.api.demo.local
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

完整示例 `examples/ingress-demo.yaml`。

### 用 Ingress 暴露单个 Service

一条无 host 规则的 Ingress 等价于 Service annotation 模式：同样一条隧道，但 URL 出现在 Ingress 的标准位置（`kubectl get ingress` 的 ADDRESS 列、`status.loadBalancer`）：

```console
$ kubectl apply -f examples/ingress-hostless.yaml
$ kubectl get ingress demo-nginx
NAME         CLASS         HOSTS   ADDRESS                                  PORTS   AGE
demo-nginx   quicktunnel   *       quiet-marble-otter.trycloudflare.com     80      30s
```

完整 `https://` URL 同时写在裸 `quicktunnel.dev/url` 注解上。注意不要和同一 Service 上的 `quicktunnel.dev/expose` 叠加使用——那会开出两条独立隧道、两个 URL。

## 已知限制（来自 Quick Tunnel 本身）

- **URL 与 Pod 生命周期绑定**：tunnel Pod 重启后域名必然变化，annotation 会自动更新，但任何把 URL 固化到配置里的用法都会失效。
- **单隧道 200 并发在途请求**，超出返回 429；适合 dev/demo，不适合压测和生产。
- **不支持 SSE**（Server-Sent Events）。
- **无 SLA**：Cloudflare 可能掐掉空闲隧道；本 operator 通过探活 + 自动重建缓解，但这是 best-effort。
- 所有流量经过 Cloudflare 边缘，注意隐私与合规。

要稳定域名和生产流量，请换命名隧道（需要账号）：官方 `cloudflared` 或 [STRRL/cloudflare-tunnel-ingress-controller](https://github.com/STRRL/cloudflare-tunnel-ingress-controller)。

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
- [ ] operator 自身 metrics（暴露中的隧道数、URL 更新计数）

## License

MIT
