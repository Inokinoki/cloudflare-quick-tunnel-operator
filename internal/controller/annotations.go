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
	LabelTunnelGroup      = "quicktunnel.dev/tunnel-group"

	// AnnTunnelGroup stores the RAW tunnel group (e.g. the Ingress host) on
	// tunnel deployments. The label value is charset-limited and lossy; this
	// annotation is what teardown uses to recover the watchdog key.
	AnnTunnelGroup = "quicktunnel.dev/tunnel-group"

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
// annotation, an HTTPRoute attached to a quicktunnel Gateway, or an Ingress
// with ingressClassName quicktunnel. Group distinguishes multiple tunnels
// owned by one object — Ingress runs one tunnel per host rule, with ""
// meaning the hostless/default tunnel.
type OwnerRef struct {
	Kind      string // "Service", "HTTPRoute" or "Ingress"
	Namespace string
	Name      string
	Group     string
}

func (o OwnerRef) Key() types.NamespacedName {
	return types.NamespacedName{Namespace: o.Namespace, Name: o.Name}
}

// Labels select every tunnel of the owner, regardless of group.
func (o OwnerRef) Labels() map[string]string {
	return map[string]string{
		ManagedByLabel:      ManagedByValue,
		LabelOwnerKind:      o.Kind,
		LabelOwnerNamespace: o.Namespace,
		LabelOwnerName:      o.Name,
	}
}

// TunnelLabels additionally pin the tunnel group; used on deployments, their
// pods, and for pod selection in the watchdog.
func (o OwnerRef) TunnelLabels() map[string]string {
	labels := o.Labels()
	if o.Group != "" {
		labels[LabelTunnelGroup] = LabelValue(o.Group)
	}
	return labels
}

// DeploymentName derives a stable, DNS-1123-safe name for the tunnel
// Deployment of an owner. Long names are shortened with a hash suffix.
func DeploymentName(owner OwnerRef) string {
	prefix := "qt-"
	switch owner.Kind {
	case "HTTPRoute":
		prefix = "qt-hr-"
	case "Ingress":
		prefix = "qt-ing-"
	}
	base := sanitizeDNS(prefix + owner.Namespace + "-" + owner.Name)
	if owner.Group != "" {
		sum := sha256.Sum256([]byte(owner.Group))
		base += "-" + hex.EncodeToString(sum[:])[:8]
	}
	if len(base) <= 63 {
		return base
	}
	sum := sha256.Sum256([]byte(owner.Kind + "/" + owner.Namespace + "/" + owner.Name + "/" + owner.Group))
	return base[:52] + "-" + hex.EncodeToString(sum[:])[:10]
}

// LabelValue turns an arbitrary string (e.g. a hostname) into a valid label
// value, hash-shortening when it exceeds 63 characters.
func LabelValue(s string) string {
	v := sanitizeDNS(s)
	if len(v) <= 63 {
		return v
	}
	sum := sha256.Sum256([]byte(s))
	return v[:47] + "-" + hex.EncodeToString(sum[:])[:15]
}

// annURLKey returns the annotation a tunnel URL is published under: the bare
// quicktunnel.dev/url for groupless tunnels, quicktunnel.dev/url.<host> for
// a grouped (Ingress host rule) tunnel.
func annURLKey(group string) string {
	if group == "" {
		return AnnURL
	}
	return AnnURL + "." + group
}

func annUpdatedKey(group string) string {
	if group == "" {
		return AnnURLUpdated
	}
	return AnnURLUpdated + "." + group
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
