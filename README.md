# quicktunnel-operator

把 Kubernetes 集群里的 Service 通过 **Cloudflare Quick Tunnel**（`trycloudflare.com`）暴露到公网——**不需要 Cloudflare 账号、不需要域名、不需要公网 IP、不需要开任何入站端口**。

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
                    │  │ quicktunnel-operator │                                             │
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

minikube 用户可以 `KUBECTL="minikube kubectl --" make deploy`。

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

- Go 1.24 + controller-runtime v0.21，无 CRD，纯 annotation 驱动。
- cloudflared 镜像版本在 `internal/controller/annotations.go` 的 `DefaultCloudflaredImage` 固定，可用 `-cloudflared-image` 覆盖。

## Roadmap

- [ ] Ingress 支持（`ingressClassName: quicktunnel`，按 host 拆分多隧道）
- [ ] Gateway API 支持
- [ ] operator 自身 metrics（暴露中的隧道数、URL 更新计数）
- [ ] 事件（Event）上报 URL 变更

## License

MIT
