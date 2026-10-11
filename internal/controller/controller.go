package controller

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// TunnelManager covers the background per-owner loop that harvests the quick
// tunnel URL from the tunnel pod's logs and probes tunnel health.
type TunnelManager interface {
	Ensure(ctx context.Context, owner OwnerRef, originURL string)
	Stop(key types.NamespacedName)
}

// requeuePeriod re-asserts the watchdog goroutine periodically; Ensure is
// idempotent, so this only costs a few API reads per exposed owner.
const requeuePeriod = 5 * time.Minute

// ensureTunnelDeployment idempotently maintains the cloudflared Deployment
// for an owner (Service or HTTPRoute). ownerObj receives the ownerReference.
func ensureTunnelDeployment(ctx context.Context, c client.Client, scheme *runtime.Scheme,
	image string, ownerObj client.Object, owner OwnerRef, origin string) error {

	desired := buildTunnelDeployment(image, owner, origin)

	existing := &appsv1.Deployment{}
	err := c.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	if apierrors.IsNotFound(err) {
		if err := controllerutil.SetControllerReference(ownerObj, desired, scheme); err != nil {
			return err
		}
		return c.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	// The apiserver defaults pod-template fields, so compare only the parts
	// that vary (image + args encode origin URL and scheme).
	dc := desired.Spec.Template.Spec.Containers[0]
	ec := existing.Spec.Template.Spec.Containers
	if len(ec) == 1 && ec[0].Image == dc.Image && reflect.DeepEqual(ec[0].Args, dc.Args) {
		return nil
	}
	existing.Spec.Template.Spec.Containers = desired.Spec.Template.Spec.Containers
	return c.Update(ctx, existing)
}

// teardownTunnelDeployment removes the owner's tunnel Deployment if present.
func teardownTunnelDeployment(ctx context.Context, c client.Client, owner OwnerRef) error {
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name:      DeploymentName(owner),
		Namespace: owner.Namespace,
	}}
	return client.IgnoreNotFound(c.Delete(ctx, dep))
}

var one int32 = 1

func buildTunnelDeployment(image string, owner OwnerRef, origin string) *appsv1.Deployment {
	labels := owner.Labels()
	args := []string{
		"tunnel",
		"--no-autoupdate",
		"--metrics", fmt.Sprintf("0.0.0.0:%d", TunnelMetricsPort),
		"--url", origin,
	}
	if strings.HasPrefix(origin, "https://") {
		// Cluster-internal origins almost always use self-signed certs.
		args = append(args, "--no-tls-verify")
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      DeploymentName(owner),
			Namespace: owner.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			// Recreate: a second concurrent pod would race for the tunnel
			// and hand out a different URL than the published one.
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: ptr.To(int64(10)),
					Containers: []corev1.Container{{
						Name:            "tunnel",
						Image:           image,
						Args:            args,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Ports: []corev1.ContainerPort{{
							Name:          "metrics",
							ContainerPort: TunnelMetricsPort,
							Protocol:      corev1.ProtocolTCP,
						}},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("10m"),
								corev1.ResourceMemory: resource.MustParse("32Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
						},
					}},
				},
			},
		},
	}
}

// ServiceReconciler watches Services annotated with quicktunnel.dev/expose
// and maintains one cloudflared quick-tunnel Deployment per exposed Service.
type ServiceReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	CloudflaredImage string
	Watchdog         TunnelManager
}

func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}, builder.WithPredicates(exposedPredicate())).
		Owns(&appsv1.Deployment{}).
		Named("quicktunnel-service").
		Complete(r)
}

func exposedPredicate() predicate.Predicate {
	isExposed := func(o client.Object) bool {
		svc, ok := o.(*corev1.Service)
		return ok && IsExposed(svc)
	}
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return isExposed(e.Object) },
		UpdateFunc:  func(e event.UpdateEvent) bool { return isExposed(e.ObjectNew) || isExposed(e.ObjectOld) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return isExposed(e.Object) },
		GenericFunc: func(e event.GenericEvent) bool { return isExposed(e.Object) },
	}
}

// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=pods/log;pods/proxy,verbs=get

func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	svc := &corev1.Service{}
	if err := r.Get(ctx, req.NamespacedName, svc); err != nil {
		if apierrors.IsNotFound(err) {
			r.Watchdog.Stop(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	owner := OwnerRef{Kind: "Service", Namespace: svc.Namespace, Name: svc.Name}

	if !IsExposed(svc) {
		r.Watchdog.Stop(owner.Key())
		if err := teardownTunnelDeployment(ctx, r.Client, owner); err != nil {
			return ctrl.Result{}, err
		}
		if _, has := svc.Annotations[AnnURL]; has {
			base := svc.DeepCopy()
			delete(svc.Annotations, AnnURL)
			delete(svc.Annotations, AnnURLUpdated)
			return ctrl.Result{}, r.Patch(ctx, svc, client.MergeFrom(base))
		}
		return ctrl.Result{}, nil
	}

	if err := ensureTunnelDeployment(ctx, r.Client, r.Scheme, r.CloudflaredImage,
		svc, owner, OriginURL(svc)); err != nil {
		return ctrl.Result{}, err
	}
	r.Watchdog.Ensure(ctx, owner, OriginURL(svc))
	logger.Info("quick tunnel ensured", "origin", OriginURL(svc))
	return ctrl.Result{RequeueAfter: requeuePeriod}, nil
}
