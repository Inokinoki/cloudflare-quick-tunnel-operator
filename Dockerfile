FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/cloudflare-quick-tunnel-operator .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/cloudflare-quick-tunnel-operator /cloudflare-quick-tunnel-operator
ENTRYPOINT ["/cloudflare-quick-tunnel-operator"]
