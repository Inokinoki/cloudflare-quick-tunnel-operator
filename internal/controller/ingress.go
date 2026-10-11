package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	nwv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// IngressControllerName is the controller field of the IngressClass this
// operator ships (deploy/install.yaml).
const IngressControllerName = "quicktunnel.dev/ingress"

// Legacy class annotation, honored alongside spec.ingressClassName.
const ingressClassAnnotation = "kubernetes.io/ingress.class"

// Mapping notes — Ingress rules are grouped by host (rules without a host
// form the "" group, with spec.defaultBackend as its fallback origin):
//
//   - Each host group gets one quick tunnel pointing at the group's first
//     valid Service backend (path-level routing cannot be honored; a quick
//     tunnel is a single origin).
//   - The URL lands on quicktunnel.dev/url.<host> annotations (bare
//     quicktunnel.dev/url for the hostless group) and all URLs are
//     aggregated into Ingress.status.loadBalancer for standard tooling.
//   - The random trycloudflare.com hostname cannot serve the rule's own
//     hostname; hosts are only the grouping/tunnel-splitting dimension.

// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses/status,verbs=get;update;patch

// IngressReconciler maintains one quick tunnel per host rule of Ingresses
// classified as quicktunnel.
type IngressReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	CloudflaredImage string
	Watchdog         TunnelManager
}

func (r *IngressReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&nwv1.Ingress{}, builder.WithPredicates(quicktunnelClassPredicate())).
		Owns(&appsv1.Deployment{}).
		Named("quicktunnel-ingress").
		Complete(r)
}

func quicktunnelClassPredicate() predicate.Predicate {
	isOurs := func(o client.Object) bool {
		ing, ok := o.(*nwv1.Ingress)
		return ok && isQuicktunnelIngress(ing)
	}
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return isOurs(e.Object) },
		UpdateFunc:  func(e event.UpdateEvent) bool { return isOurs(e.ObjectNew) || isOurs(e.ObjectOld) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return isOurs(e.Object) },
		GenericFunc: func(e event.GenericEvent) bool { return isOurs(e.Object) },
	}
}

func isQuicktunnelIngress(ing *nwv1.Ingress) bool {
	if ing == nil {
		return false
	}
	if ing.Annotations[ingressClassAnnotation] == "quicktunnel" {
		return true
	}
	return ing.Spec.IngressClassName != nil && *ing.Spec.IngressClassName == "quicktunnel"
}

func (r *IngressReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	ing := &nwv1.Ingress{}
	if err := r.Get(ctx, req.NamespacedName, ing); err != nil {
		if apierrors.IsNotFound(err) {
			r.Watchdog.StopAll(req.Namespace, req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	base := OwnerRef{Kind: "Ingress", Namespace: ing.Namespace, Name: ing.Name}

	if ing.DeletionTimestamp != nil || !isQuicktunnelIngress(ing) {
		r.Watchdog.StopAll(ing.Namespace, ing.Name)
		if err := r.deleteTunnels(ctx, base, nil); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.stripURLAnnotations(ctx, ing); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	groups, requeue, err := r.resolveGroups(ctx, ing)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Ensure whatever resolved even when other backends are still pending;
	// the requeue below converges the rest.
	for group, origin := range groups {
		owner := base
		owner.Group = group
		if err := ensureTunnelDeployment(ctx, r.Client, r.Scheme, r.CloudflaredImage,
			ing, owner, origin); err != nil {
			return ctrl.Result{}, err
		}
		r.Watchdog.Ensure(ctx, owner, origin)
	}

	desired := make(map[string]bool, len(groups))
	for group := range groups {
		desired[LabelValue(group)] = true
	}
	if err := r.deleteTunnels(ctx, base, func(groupLabel string) bool {
		return !desired[groupLabel]
	}); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.syncIngressStatus(ctx, ing); err != nil {
		return ctrl.Result{}, err
	}
	logger.Info("quick tunnels ensured", "groups", len(groups))
	if requeue {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	return ctrl.Result{RequeueAfter: requeuePeriod}, nil
}

// resolveGroups maps host → tunnel origin. It reports requeue=true when a
// referenced Service is not (yet) visible.
func (r *IngressReconciler) resolveGroups(ctx context.Context, ing *nwv1.Ingress) (map[string]string, bool, error) {
	groups := map[string]string{}
	set := func(host string, svcName string, port int32) (bool, error) {
		if _, done := groups[host]; done {
			return false, nil
		}
		svc := &corev1.Service{}
		key := types.NamespacedName{Namespace: ing.Namespace, Name: svcName}
		if err := r.Get(ctx, key, svc); err != nil {
			return true, fmt.Errorf("backend Service %s for host %q not found: %w", key.String(), host, err)
		}
		if port == 0 && len(svc.Spec.Ports) > 0 {
			port = svc.Spec.Ports[0].Port
		}
		groups[host] = fmt.Sprintf("%s://%s.%s.svc.cluster.local:%d",
			SchemeFor(ing.Annotations), svc.Name, svc.Namespace, port)
		return false, nil
	}

	var (
		requeue bool
		firstErr error
	)
	record := func(host, svcName string, port int32) {
		if requeue {
			return
		}
		rq, err := set(host, svcName, port)
		if err != nil && firstErr == nil {
			firstErr = err
			logger := log.FromContext(ctx)
			logger.Info("backend not resolvable yet", "host", host, "error", err.Error())
		}
		requeue = requeue || rq
	}

	for _, rule := range ing.Spec.Rules {
		if rule.HTTP == nil {
			continue
		}
		for _, path := range rule.HTTP.Paths {
			svc := path.Backend.Service
			if svc == nil {
				continue
			}
			record(rule.Host, svc.Name, svc.Port.Number)
			break // one tunnel per host: first backend wins
		}
	}
	if len(groups) == 0 && ing.Spec.DefaultBackend != nil && ing.Spec.DefaultBackend.Service != nil {
		record("", ing.Spec.DefaultBackend.Service.Name, ing.Spec.DefaultBackend.Service.Port.Number)
	}
	return groups, requeue, nil
}

// deleteTunnels removes the owner's tunnel deployments whose group label is
// dropped by the drop callback (nil = drop all).
func (r *IngressReconciler) deleteTunnels(ctx context.Context, base OwnerRef, drop func(groupLabel string) bool) error {
	list := &appsv1.DeploymentList{}
	if err := r.List(ctx, list,
		client.InNamespace(base.Namespace),
		client.MatchingLabels(base.Labels()),
	); err != nil {
		return err
	}
	for i := range list.Items {
		dep := &list.Items[i]
		// The label is lossy (charset-limited); the annotation keeps the raw
		// group so the watchdog Stop key matches the Ensure key.
		group := dep.Annotations[AnnTunnelGroup]
		if drop != nil && !drop(dep.Labels[LabelTunnelGroup]) {
			continue
		}
		owner := base
		owner.Group = group
		r.Watchdog.Stop(owner)
		if err := client.IgnoreNotFound(r.Delete(ctx, dep)); err != nil {
			return err
		}
	}
	return nil
}

func (r *IngressReconciler) stripURLAnnotations(ctx context.Context, ing *nwv1.Ingress) error {
	var stale []string
	for k := range ing.Annotations {
		if k == AnnURL || k == AnnURLUpdated || strings.HasPrefix(k, AnnURL+".") || strings.HasPrefix(k, AnnURLUpdated+".") {
			stale = append(stale, k)
		}
	}
	if len(stale) == 0 {
		return nil
	}
	base := ing.DeepCopy()
	for _, k := range stale {
		delete(ing.Annotations, k)
	}
	return r.Patch(ctx, ing, client.MergeFrom(base))
}

// syncIngressStatus aggregates every published tunnel hostname into
// status.loadBalancer.ingress — the standard field external tooling reads.
func (r *IngressReconciler) syncIngressStatus(ctx context.Context, ing *nwv1.Ingress) error {
	var hosts []string
	for k, v := range ing.Annotations {
		if k == AnnURL || strings.HasPrefix(k, AnnURL+".") {
			hosts = append(hosts, strings.TrimPrefix(v, "https://"))
		}
	}
	if len(hosts) == 0 {
		return nil
	}
	sort.Strings(hosts)

	base := ing.DeepCopy()
	ing.Status.LoadBalancer.Ingress = nil
	for _, h := range hosts {
		ing.Status.LoadBalancer.Ingress = append(ing.Status.LoadBalancer.Ingress, nwv1.IngressLoadBalancerIngress{
			Hostname: h,
		})
	}
	return r.Status().Patch(ctx, ing, client.MergeFrom(base))
}
