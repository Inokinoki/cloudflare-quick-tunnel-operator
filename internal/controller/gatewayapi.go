package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// GatewayControllerName is the controllerName this operator claims in
// GatewayClass.spec.controllerName.
const GatewayControllerName = "quicktunnel.dev/gateway"

// quickTunnelClass marks GatewayClasses created by our install manifest.
const quickTunnelClassName = "quicktunnel"

// Mapping notes — a quick tunnel has exactly one random hostname and one
// origin, so the Gateway API mapping is:
//
//   - GatewayClass(quicktunnel) is Accepted by this controller.
//   - Each HTTPRoute attached to a quicktunnel Gateway gets one tunnel.
//     Hostname/path matches cannot be honored (the trycloudflare.com name is
//     random and per-tunnel); the first rule's first Service backendRef is
//     the tunnel origin. The URL surfaces on the route annotation
//     quicktunnel.dev/url and on the parent Gateway status.addresses.
//   - A Gateway aggregates the URLs of its attached routes and becomes
//     Programmed once at least one route has a tunnel URL.

// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses;gateways;httproutes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses/status;gateways/status;httproutes/status,verbs=get;update;patch

// GatewayClassReconciler accepts GatewayClasses whose controllerName is ours.
type GatewayClassReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func (r *GatewayClassReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gwv1.GatewayClass{}).
		Named("quicktunnel-gatewayclass").
		Complete(r)
}

func (r *GatewayClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	gc := &gwv1.GatewayClass{}
	if err := r.Get(ctx, req.NamespacedName, gc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if string(gc.Spec.ControllerName) != GatewayControllerName {
		return ctrl.Result{}, nil
	}
	base := gc.DeepCopy()
	apimeta.SetStatusCondition(&gc.Status.Conditions, metav1.Condition{
		Type:               "Accepted",
		Status:             metav1.ConditionTrue,
		Reason:             "Accepted",
		Message:            "managed by cloudflare-quick-tunnel-operator",
		ObservedGeneration: gc.Generation,
	})
	if err := r.Status().Patch(ctx, gc, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// GatewayReconciler reflects the state of attached HTTPRoutes into Gateway
// status (addresses + conditions). The tunnels themselves are owned by the
// HTTPRoute reconciler.
type GatewayReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func (r *GatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gwv1.Gateway{}).
		Named("quicktunnel-gateway").
		Complete(r)
}

func (r *GatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	gw := &gwv1.Gateway{}
	if err := r.Get(ctx, req.NamespacedName, gw); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !r.isOurs(ctx, gw) {
		return ctrl.Result{}, nil
	}
	if err := syncGatewayStatus(ctx, r.Client, gw); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// isOurs reports whether the Gateway's GatewayClass is controlled by us.
func (r *GatewayReconciler) isOurs(ctx context.Context, gw *gwv1.Gateway) bool {
	class := &gwv1.GatewayClass{}
	if err := r.Get(ctx, types.NamespacedName{Name: string(gw.Spec.GatewayClassName)}, class); err != nil {
		return false
	}
	return string(class.Spec.ControllerName) == GatewayControllerName
}

// HTTPRouteReconciler maintains one quick tunnel per HTTPRoute attached to a
// quicktunnel Gateway.
type HTTPRouteReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	CloudflaredImage string
	Watchdog         TunnelManager
}

func (r *HTTPRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gwv1.HTTPRoute{}).
		Named("quicktunnel-httproute").
		Complete(r)
}

func (r *HTTPRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	route := &gwv1.HTTPRoute{}
	if err := r.Get(ctx, req.NamespacedName, route); err != nil {
		if apierrors.IsNotFound(err) {
			r.Watchdog.StopAll(req.Namespace, req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	owner := OwnerRef{Kind: "HTTPRoute", Namespace: route.Namespace, Name: route.Name}
	gw := r.attachedGateway(ctx, route)

	if route.DeletionTimestamp != nil || gw == nil {
		r.Watchdog.Stop(owner)
		if err := teardownTunnelDeployment(ctx, r.Client, owner); err != nil {
			return ctrl.Result{}, err
		}
		if _, has := route.Annotations[AnnURL]; has {
			base := route.DeepCopy()
			delete(route.Annotations, AnnURL)
			delete(route.Annotations, AnnURLUpdated)
			if err := r.Patch(ctx, route, client.MergeFrom(base)); err != nil {
				return ctrl.Result{}, err
			}
		}
		if gw != nil {
			return ctrl.Result{}, syncGatewayStatus(ctx, r.Client, gw)
		}
		return ctrl.Result{}, nil
	}

	origin, svcName, err := r.resolveBackend(ctx, route)
	if err != nil {
		// Most commonly a race: the route reconciles before its backend
		// Service has reached our cache. Tear down any stale tunnel and
		// retry rather than treating this as terminal.
		r.Watchdog.Stop(owner)
		if tdErr := teardownTunnelDeployment(ctx, r.Client, owner); tdErr != nil {
			return ctrl.Result{}, tdErr
		}
		if err := r.setRouteStatus(ctx, route, gw, false, err.Error()); err != nil {
			return ctrl.Result{}, err
		}
		if err := syncGatewayStatus(ctx, r.Client, gw); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if err := ensureTunnelDeployment(ctx, r.Client, r.Scheme, r.CloudflaredImage,
		route, owner, origin); err != nil {
		return ctrl.Result{}, err
	}
	r.Watchdog.Ensure(ctx, owner, origin)
	if err := r.setRouteStatus(ctx, route, gw, true, ""); err != nil {
		return ctrl.Result{}, err
	}
	if err := syncGatewayStatus(ctx, r.Client, gw); err != nil {
		return ctrl.Result{}, err
	}
	logger.Info("quick tunnel ensured", "origin", origin, "service", svcName)
	return ctrl.Result{RequeueAfter: requeuePeriod}, nil
}

// attachedGateway returns the first parent Gateway whose GatewayClass is
// ours, or nil when the route is not attached to a quicktunnel Gateway.
func (r *HTTPRouteReconciler) attachedGateway(ctx context.Context, route *gwv1.HTTPRoute) *gwv1.Gateway {
	for i := range route.Spec.ParentRefs {
		ref := route.Spec.ParentRefs[i]
		if ref.Kind != nil && *ref.Kind != "Gateway" {
			continue
		}
		if ref.Group != nil && *ref.Group != "gateway.networking.k8s.io" {
			continue
		}
		ns := route.Namespace
		if ref.Namespace != nil && *ref.Namespace != "" {
			ns = string(*ref.Namespace)
		}
		gw := &gwv1.Gateway{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: string(ref.Name)}, gw); err != nil {
			continue
		}
		class := &gwv1.GatewayClass{}
		if err := r.Get(ctx, types.NamespacedName{Name: string(gw.Spec.GatewayClassName)}, class); err != nil {
			continue
		}
		if string(class.Spec.ControllerName) == GatewayControllerName {
			return gw
		}
	}
	return nil
}

// resolveBackend derives the tunnel origin from the route: first rule, first
// Service backendRef with a port. The Service must exist.
func (r *HTTPRouteReconciler) resolveBackend(ctx context.Context, route *gwv1.HTTPRoute) (origin, svcName string, err error) {
	for _, rule := range route.Spec.Rules {
		for _, ref := range rule.BackendRefs {
			backend := ref.BackendObjectReference
			if backend.Kind != nil && *backend.Kind != "Service" {
				continue
			}
			if backend.Group != nil && *backend.Group != "" {
				continue
			}
			if backend.Port == nil {
				continue
			}
			ns := route.Namespace
			if backend.Namespace != nil && *backend.Namespace != "" {
				ns = string(*backend.Namespace)
			}
			svc := &corev1.Service{}
			key := types.NamespacedName{Namespace: ns, Name: string(backend.Name)}
			if getErr := r.Get(ctx, key, svc); getErr != nil {
				return "", "", fmt.Errorf("backend Service %s not found: %w", key.String(), getErr)
			}
			u := fmt.Sprintf("%s://%s.%s.svc.cluster.local:%d",
				SchemeFor(route.Annotations), svc.Name, svc.Namespace, *backend.Port)
			return u, svc.Name, nil
		}
	}
	return "", "", fmt.Errorf("no valid Service backendRef found")
}

// setRouteStatus records Accepted/ResolvedRefs on the parent status entry
// owned by this controller.
func (r *HTTPRouteReconciler) setRouteStatus(ctx context.Context, route *gwv1.HTTPRoute, gw *gwv1.Gateway, resolved bool, notResolvedMsg string) error {
	base := route.DeepCopy()
	ctrlName := gwv1.GatewayController(GatewayControllerName)

	var parent *gwv1.RouteParentStatus
	for i := range route.Status.Parents {
		if string(route.Status.Parents[i].ControllerName) == GatewayControllerName {
			parent = &route.Status.Parents[i]
			break
		}
	}
	if parent == nil {
		route.Status.Parents = append(route.Status.Parents, gwv1.RouteParentStatus{
			ControllerName: ctrlName,
			ParentRef: gwv1.ParentReference{
				Group:     groupPtr("gateway.networking.k8s.io"),
				Kind:      kindPtr("Gateway"),
				Name:      gwv1.ObjectName(gw.Name),
				Namespace: (*gwv1.Namespace)(&gw.Namespace),
			},
		})
		parent = &route.Status.Parents[len(route.Status.Parents)-1]
	}

	apimeta.SetStatusCondition(&parent.Conditions, metav1.Condition{
		Type:               "Accepted",
		Status:             metav1.ConditionTrue,
		Reason:             "Accepted",
		Message:            "managed by cloudflare-quick-tunnel-operator",
		ObservedGeneration: route.Generation,
	})
	resolvedCond := metav1.Condition{
		Type:               "ResolvedRefs",
		Status:             metav1.ConditionTrue,
		Reason:             "ResolvedRefs",
		ObservedGeneration: route.Generation,
	}
	if !resolved {
		resolvedCond.Status = metav1.ConditionFalse
		resolvedCond.Reason = "BackendNotFound"
		resolvedCond.Message = notResolvedMsg
	}
	apimeta.SetStatusCondition(&parent.Conditions, resolvedCond)

	return r.Status().Patch(ctx, route, client.MergeFrom(base))
}

// syncGatewayStatus aggregates the URLs of all attached routes into the
// Gateway's status.addresses and maintains its conditions.
func syncGatewayStatus(ctx context.Context, c client.Client, gw *gwv1.Gateway) error {
	routes := &gwv1.HTTPRouteList{}
	if err := c.List(ctx, routes, client.InNamespace(gw.Namespace)); err != nil {
		return err
	}

	seen := map[string]bool{}
	var addresses []gwv1.GatewayStatusAddress
	for i := range routes.Items {
		route := &routes.Items[i]
		url := route.Annotations[AnnURL]
		if url == "" || !routeAttachedTo(route, gw) {
			continue
		}
		// The Hostname address type is a bare hostname; the scheme is
		// implied (Cloudflare always serves HTTPS).
		host := strings.TrimPrefix(url, "https://")
		if !seen[host] {
			seen[host] = true
			addresses = append(addresses, gwv1.GatewayStatusAddress{
				Type:  addrTypePtr(gwv1.HostnameAddressType),
				Value: host,
			})
		}
	}

	base := gw.DeepCopy()
	gw.Status.Addresses = addresses
	apimeta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
		Type:               "Accepted",
		Status:             metav1.ConditionTrue,
		Reason:             "Accepted",
		Message:            "managed by cloudflare-quick-tunnel-operator",
		ObservedGeneration: gw.Generation,
	})
	programmed := metav1.Condition{
		Type:               "Programmed",
		ObservedGeneration: gw.Generation,
	}
	if len(addresses) > 0 {
		programmed.Status = metav1.ConditionTrue
		programmed.Reason = "Programmed"
	} else {
		programmed.Status = metav1.ConditionFalse
		programmed.Reason = "Pending"
		programmed.Message = "no attached HTTPRoute has a tunnel URL yet"
	}
	apimeta.SetStatusCondition(&gw.Status.Conditions, programmed)

	gw.Status.Listeners = nil
	for _, l := range gw.Spec.Listeners {
		conds := []metav1.Condition{}
		apimeta.SetStatusCondition(&conds, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "Ready",
			ObservedGeneration: gw.Generation,
		})
		gw.Status.Listeners = append(gw.Status.Listeners, gwv1.ListenerStatus{
			Name:           l.Name,
			SupportedKinds: []gwv1.RouteGroupKind{{Group: groupPtr("gateway.networking.k8s.io"), Kind: "HTTPRoute"}},
			Conditions:     conds,
		})
	}
	return c.Status().Patch(ctx, gw, client.MergeFrom(base))
}

func routeAttachedTo(route *gwv1.HTTPRoute, gw *gwv1.Gateway) bool {
	for _, ref := range route.Spec.ParentRefs {
		if string(ref.Name) != gw.Name {
			continue
		}
		if ref.Kind != nil && *ref.Kind != "Gateway" {
			continue
		}
		if ref.Namespace == nil || string(*ref.Namespace) == gw.Namespace {
			return true
		}
	}
	return false
}

func groupPtr(s string) *gwv1.Group    { g := gwv1.Group(s); return &g }
func kindPtr(s string) *gwv1.Kind      { k := gwv1.Kind(s); return &k }
func addrTypePtr(t gwv1.AddressType) *gwv1.AddressType { return &t }
