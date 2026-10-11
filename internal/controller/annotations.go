package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

const (
	// Annotations read from Services to opt in / tune the tunnel.
	AnnExpose     = "quicktunnel.dev/expose"
	AnnPort       = "quicktunnel.dev/port"
	AnnScheme     = "quicktunnel.dev/scheme"
	AnnURL        = "quicktunnel.dev/url"
	AnnURLUpdated = "quicktunnel.dev/url-updated-at"

	ManagedByLabel        = "app.kubernetes.io/managed-by"
	ManagedByValue        = "quicktunnel-operator"
	LabelServiceNamespace = "quicktunnel.dev/service-namespace"
	LabelServiceName      = "quicktunnel.dev/service-name"

	DefaultCloudflaredImage = "cloudflare/cloudflared:2026.10.0"

	// TunnelMetricsPort is where cloudflared serves /metrics inside tunnel
	// pods; the operator scrapes it to detect zombie tunnels.
	TunnelMetricsPort = 20000
)

func IsExposed(svc *corev1.Service) bool {
	return svc != nil && svc.Annotations[AnnExpose] == "true"
}

// OriginURL returns the in-cluster URL the quick tunnel forwards traffic to.
// Port defaults to the first declared service port; scheme defaults to http.
func OriginURL(svc *corev1.Service) string {
	scheme := svc.Annotations[AnnScheme]
	if scheme != "https" {
		scheme = "http"
	}
	port := svc.Annotations[AnnPort]
	if port == "" && len(svc.Spec.Ports) > 0 {
		port = strconv.Itoa(int(svc.Spec.Ports[0].Port))
	}
	return fmt.Sprintf("%s://%s.%s.svc.cluster.local:%s",
		scheme, svc.Name, svc.Namespace, port)
}

// DeploymentName derives a stable, DNS-1123-safe name for the tunnel
// Deployment of a Service. Long names are shortened with a hash suffix.
func DeploymentName(namespace, name string) string {
	base := sanitizeDNS("qt-" + namespace + "-" + name)
	if len(base) <= 63 {
		return base
	}
	sum := sha256.Sum256([]byte(namespace + "/" + name))
	return base[:52] + "-" + hex.EncodeToString(sum[:])[:10]
}

func sanitizeDNS(s string) string {
	s = strings.ToLower(s)
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		default:
			return '-'
		}
	}, s)
	return strings.Trim(s, "-")
}
