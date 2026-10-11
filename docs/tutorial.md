# cloudflare-quick-tunnel-operator 安装与使用教程

本文是手把手教程，所有命令均在 minikube + Kubernetes v1.33 上实际验证过。三种暴露方式可混用：

| 方式 | 适用场景 | 前置 |
|---|---|---|
| Service annotation | 单个 Service，最快 | 无 |
| Ingress | 多个 Service、按 host 分流 | 无 |
| Gateway API | 新标准、未来方向 | 需装 Gateway API CRDs |

---

## 0. 前置条件

- 一个 Kubernetes 集群（minikube / kind / 云集群均可）+ `kubectl`
- 想容器化部署：Docker（构建镜像用）
- 想本地开发模式：Go 1.24+（本仓库 Makefile 会自动探测 `~/go/bin/go`）

集群**不需要**公网 IP、不需要开放任何入站端口——quick tunnel 是纯出站连接。

## 1. 安装

### 方式 A：集群内部署（正式用法）

```console
$ git clone <repo> && cd cloudflare-quick-tunnel-operator

# 构建镜像（或 docker buildx 指定多架构）
$ make docker-build                          # → cloudflare-quick-tunnel-operator:dev

# 把镜像塞进集群：
#   minikube:
$ minikube image load cloudflare-quick-tunnel-operator:dev
#   kind:
$ kind load docker-image cloudflare-quick-tunnel-operator:dev --name <cluster名>

# 一键安装：Namespace + ServiceAccount + ClusterRole + operator Deployment
#           + IngressClass quicktunnel + GatewayClass quicktunnel
$ kubectl apply -f deploy/install.yaml

# 等待就绪
$ kubectl -n cloudflare-quick-tunnel-operator wait deploy/cloudflare-quick-tunnel-operator --for=condition=Available --timeout=90s
```

minikube 用户如果没装独立 kubectl，可以 `KUBECTL="minikube kubectl --" make deploy`。

### 1.1 远程/云集群安装（需要推镜像到 registry）

远程集群没法 side-load 镜像，把构建好的镜像推到集群能拉取的 registry（Docker Hub / GHCR / 私有 registry），然后改一下 `deploy/install.yaml` 里的 `image:` 即可：

```console
$ export IMAGE=ghcr.io/<你的用户名>/cloudflare-quick-tunnel-operator:v0.1.0   # 或 docker.io/<你>/...

# 构建并推送（x86 集群）
$ docker build -t $IMAGE .
$ docker push $IMAGE

# 集群是 ARM（如树莓派 / Oracle ARM 免费机）时构建双架构：
$ docker buildx build --platform linux/amd64,linux/arm64 -t $IMAGE --push .

# 安装并指向你的镜像
$ kubectl apply -f deploy/install.yaml
$ kubectl -n cloudflare-quick-tunnel-operator set image deploy/cloudflare-quick-tunnel-operator manager=$IMAGE

# 验证
$ kubectl -n cloudflare-quick-tunnel-operator rollout status deploy/cloudflare-quick-tunnel-operator
```

私有 registry 需要拉取凭据：

```console
$ kubectl -n cloudflare-quick-tunnel-operator create secret docker-registry regcred \
    --docker-server=<registry地址> --docker-username=<用户> --docker-password=<密码>
$ kubectl -n cloudflare-quick-tunnel-operator patch deploy cloudflare-quick-tunnel-operator \
    -p '{"spec":{"template":{"spec":{"imagePullSecrets":[{"name":"regcred"}]}}}}'
```

> 说明：operator 的隧道探活走 API server 的 `pods/proxy` 子资源（RBAC 已含在 install.yaml），标准发行版集群（kubeadm/EKS/GKE/ACK/TKE 等）都支持。若你的平台限制该子资源，日志会持续出现 `tunnel wedged, recycling tunnel pod`，这是已知边界。

不想在远程集群装任何东西也可以：用[本地开发模式](#方式-b本地开发模式免打镜像推荐先体验)把 `KUBECONFIG` 指向远程集群，operator 跑在本机、隧道 Pod 跑在集群里，效果等同（前提是 kubeconfig 身份具备 install.yaml 中 ClusterRole 的权限，云集群的管理员 kubeconfig 天然满足）。

### 方式 B：本地开发模式（免打镜像，推荐先体验）

operator 直接跑在你本机上，通过 kubeconfig 连集群：

```console
$ make build
$ ./bin/cloudflare-quick-tunnel-operator
```

> 注意：本地模式需要 kubeconfig 里的身份有 Service/Ingress/Deployment/Pod 的操作权限（minikube/kind 默认 admin 没问题）；本机 8080 端口被占的话加 `-metrics-bind-address :18080`。

### 验证安装

```console
$ kubectl -n cloudflare-quick-tunnel-operator logs deploy/cloudflare-quick-tunnel-operator | head -5
# 看到 "starting cloudflare-quick-tunnel-operator" 即成功；
# 集群里没装 Gateway API CRDs 时会多一行 "skipping Gateway API support"，属正常，
# Service annotation 和 Ingress 模式不受影响。
```

## 2. 使用方式一：Service annotation（最简）

给任意 Service 打一个 annotation：

```console
$ kubectl annotate svc my-svc quicktunnel.dev/expose=true
```

十几秒后 URL 会出现在 annotation 里：

```console
$ kubectl get svc my-svc -o jsonpath='{.metadata.annotations.quicktunnel\.dev/url}'
https://quiet-marble-otter.trycloudflare.com

$ curl https://quiet-marble-otter.trycloudflare.com
```

可选微调（都支持）：

```yaml
metadata:
  annotations:
    quicktunnel.dev/expose: "true"
    quicktunnel.dev/port: "8080"    # 默认取 Service 第一个 port
    quicktunnel.dev/scheme: "https" # 源站是 https 时（自动加 --no-tls-verify）
```

取消暴露（隧道 Deployment 自动回收、URL 注解自动清理）：

```console
$ kubectl annotate svc my-svc quicktunnel.dev/expose-
```

## 3. 使用方式二：Ingress（按 host 拆隧道）

`install.yaml` 已创建 `IngressClass quicktunnel`。每个 host 规则得到一条独立隧道：

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: demo
spec:
  ingressClassName: quicktunnel
  rules:
    - host: web.demo.local      # → 隧道1，URL 在 quicktunnel.dev/url.web.demo.local
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service: { name: demo-nginx, port: { number: 80 } }
    - host: api.demo.local      # → 隧道2，URL 在 quicktunnel.dev/url.api.demo.local
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service: { name: demo-echo, port: { number: 5678 } }
```

```console
$ kubectl apply -f examples/ingress-demo.yaml

# 所有隧道的公网地址聚合在 status.loadBalancer（external-dns 等工具读这里）：
$ kubectl get ingress demo -o jsonpath='{.status.loadBalancer.ingress[*].hostname}'
api-xxx.trycloudflare.com web-yyy.trycloudflare.com

# 单个 host 的精确地址：
$ kubectl get ingress demo -o jsonpath='{.metadata.annotations.quicktunnel\.dev/url\.web\.demo\.local}'
```

规则里不带 host 的归入默认组（裸 `quicktunnel.dev/url`）；没有规则时 `defaultBackend` 兜底。path 级路由无法支持（每条隧道单 origin），每 host 组取第一个 backend。

## 4. 使用方式三：Gateway API

先装 CRDs（一次性）：

```console
$ kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.4.1/standard-install.yaml
```

`install.yaml` 已创建 `GatewayClass quicktunnel`：

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

# 每个 HTTPRoute 一条隧道；Gateway 汇总其下所有 route 的 URL：
$ kubectl get gateway demo -o jsonpath='{.status.addresses[0].value}'
quiet-marble-otter.trycloudflare.com      # Hostname 类型不带 scheme，访问时加 https://

# route 自身的状态与注解：
$ kubectl get httproute demo -o jsonpath='{.status.parents[0].conditions[*].type}'
Accepted ResolvedRefs
```

## 5. 日常运维

```console
# 列出所有隧道 Pod（所有暴露方式统一打标）
$ kubectl get pods -A -l app.kubernetes.io/managed-by=cloudflare-quick-tunnel-operator

# 看某条隧道的 cloudflared 日志（确认边缘连接数、当前 URL）
$ kubectl logs <tunnel-pod> | grep -E 'trycloudflare|Registered'

# operator 日志：URL 发布、僵尸回收都有记录
$ kubectl -n cloudflare-quick-tunnel-operator logs deploy/cloudflare-quick-tunnel-operator | grep watchdog
```

**URL 生命周期**（重要契约）：URL 与隧道 Pod 生命周期绑定，Pod 重建（节点排水、镜像更新、探活回收）后 URL 必然变化，annotation/status 会在几秒内自动更新。**消费方必须动态读取**，不要固化到配置里。

## 6. 排障速查

| 症状 | 原因与处理 |
|---|---|
| annotation 迟迟没有 URL | 等 10-20s；查隧道 Pod 是否 Running（镜像拉取慢）；`kubectl describe pod` |
| curl 返回 530 | 多半是 curl 了已重建 Pod 的旧 URL——重新读 annotation；或后端 Service 端口不对 |
| 偶发 429 | 单隧道 200 并发在途请求上限（Quick Tunnel 硬限制） |
| SSE / EventSource 失效 | Quick Tunnel 不支持 SSE，属平台限制 |
| 本地模式起不来 `bind: address already in use` | 加 `-metrics-bind-address :18080` |
| 日志出现 `tunnel wedged, recycling tunnel pod` | 正常自愈：僵尸隧道被探测到并重建，URL 随之更新 |
| Ingress/Gateway 用不了 | Gateway API 模式需先装 CRDs（见第 4 节）；Ingress 不需要 |

## 7. 卸载

```console
$ kubectl delete -f deploy/install.yaml        # operator、RBAC、IngressClass、GatewayClass
$ kubectl delete -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.4.1/standard-install.yaml  # 可选
```

各隧道 Deployment 通过 ownerReference 挂在对应 Service/Ingress/HTTPRoute 上，Kubernetes GC 会自动回收，无需手动清理。
