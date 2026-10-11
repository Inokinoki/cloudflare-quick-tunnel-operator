GO ?= $(shell command -v go 2>/dev/null || echo $(HOME)/go/bin/go)
IMAGE ?= cloudflare-quick-tunnel-operator:dev
KUBECTL ?= kubectl

.PHONY: tidy build test vet run docker-build minikube-load deploy demo clean

tidy:
	$(GO) mod tidy

build:
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/cloudflare-quick-tunnel-operator .

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

# Run outside the cluster against the current kubeconfig (dev mode).
run:
	$(GO) run . -cloudflared-image=$(CLOUDFLARED_IMAGE)

docker-build:
	docker build -t $(IMAGE) .

minikube-load: docker-build
	minikube image load $(IMAGE)

deploy:
	$(KUBECTL) apply -f deploy/install.yaml

demo:
	$(KUBECTL) apply -f examples/demo.yaml

clean:
	rm -rf bin
