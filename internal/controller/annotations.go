package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	// Annotations read from owners to opt in / tune the tunnel.
	// quicktunnel.dev/expose and quicktunnel.dev/port apply to Services;
	// quicktunnel.dev/scheme applies to both Services and HTTPRoutes.
	AnnExpose     = "quicktunnel.dev/expose"
	AnnPort       = "quicktunnel.dev/port"
	AnnScheme     = "quicktunnel.dev/scheme"
	AnnURL        = "quicktunnel.dev/url"
	AnnURLUpdated = "quicktunnel.dev/url-updated-at"

	ManagedByLabel        = "app.kubernetes.io/managed-by"
	ManagedByValue        = "quicktunnel-operator"
	LabelOwnerKind        = "quicktunnel.dev/owner-kind"
	LabelOwnerNamespace   = "quicktunnel.dev/owner-namespace"
	LabelOwnerName        = "quicktunnel.dev/owner-name"

	DefaultCloudflaredImage = "cloudflare/cloudflared:2026.10.0"

	// TunnelMetricsPort is where cloudflared serves /metrics inside tunnel
	// pods; the operator scrapes it to detect zombie tunnels.
	TunnelMetricsPort = 20000
)

func IsExposed(svc *corev1.Service) bool {
	return svc != nil && svc.Annotations[AnnExpose] == "true"
}

// SchemeFor returns the origin scheme an owner requested, defaulting to http.
func SchemeFor(annotations map[string]string) string {
	if annotations[AnnScheme] == "https" {
		return "https"
	}
	return "http"
}

// OriginURL returns the in-cluster URL the quick tunnel forwards traffic to
// for an exposed Service. Port defaults to the first declared service port.
func OriginURL(svc *corev1.Service) string {
	port := svc.Annotations[AnnPort]
	if port == "" && len(svc.Spec.Ports) > 0 {
		port = strconv.Itoa(int(svc.Spec.Ports[0].Port))
	}
	return fmt.Sprintf("%s://%s.%s.svc.cluster.local:%s",
		SchemeFor(svc.Annotations), svc.Name, svc.Namespace, port)
}

// OwnerRef identifies the object a tunnel belongs to: a Service exposed via
// annotation, or an HTTPRoute attached to a quicktunnel Gateway.
type OwnerRef struct {
	Kind      string // "Service" or "HTTPRoute"
	Namespace string
	Name      string
}

func (o OwnerRef) Key() types.NamespacedName {
	return types.NamespacedName{Namespace: o.Namespace, Name: o.Name}
}

func (o OwnerRef) Labels() map[string]string {
	return map[string]string{
		ManagedByLabel:      ManagedByValue,
		LabelOwnerKind:      o.Kind,
		LabelOwnerNamespace: o.Namespace,
		LabelOwnerName:      o.Name,
	}
}

// DeploymentName derives a stable, DNS-1123-safe name for the tunnel
// Deployment of an owner. Long names are shortened with a hash suffix.
func DeploymentName(owner OwnerRef) string {
	prefix := "qt-"
	if owner.Kind == "HTTPRoute" {
		prefix = "qt-hr-"
	}
	base := sanitizeDNS(prefix + owner.Namespace + "-" + owner.Name)
	if len(base) <= 63 {
		return base
	}
	sum := sha256.Sum256([]byte(owner.Kind + "/" + owner.Namespace + "/" + owner.Name))
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
