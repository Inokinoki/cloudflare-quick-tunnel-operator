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

// TunnelManager covers the background per-Service loop that harvests the
// quick tunnel URL from the tunnel pod's logs and probes tunnel health.
type TunnelManager interface {
	Ensure(ctx context.Context, svc *corev1.Service, originURL string)
	Stop(key types.NamespacedName)
}

// ServiceReconciler watches Services annotated with quicktunnel.dev/expose
// and maintains one cloudflared quick-tunnel Deployment per exposed Service.
type ServiceReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	CloudflaredImage string
	Watchdog         TunnelManager
}

// requeuePeriod re-asserts the watchdog goroutine periodically; Ensure is
// idempotent, so this only costs a few API reads per exposed Service.
const requeuePeriod = 5 * time.Minute

func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}, builder.WithPredicates(exposedPredicate())).
		Owns(&appsv1.Deployment{}).
		Named("quicktunnel").
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
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get

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

	if !IsExposed(svc) {
		r.Watchdog.Stop(req.NamespacedName)
		if err := r.teardown(ctx, svc); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if err := r.ensureDeployment(ctx, svc); err != nil {
		return ctrl.Result{}, err
	}
	origin := OriginURL(svc)
	r.Watchdog.Ensure(ctx, svc, origin)
	logger.Info("quick tunnel ensured", "origin", origin)
	return ctrl.Result{RequeueAfter: requeuePeriod}, nil
}

func (r *ServiceReconciler) ensureDeployment(ctx context.Context, svc *corev1.Service) error {
	desired := r.tunnelDeployment(svc)

	existing := &appsv1.Deployment{}
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	if apierrors.IsNotFound(err) {
		if err := controllerutil.SetControllerReference(svc, desired, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, desired)
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
	return r.Update(ctx, existing)
}

func (r *ServiceReconciler) teardown(ctx context.Context, svc *corev1.Service) error {
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name:      DeploymentName(svc.Namespace, svc.Name),
		Namespace: svc.Namespace,
	}}
	if err := r.Delete(ctx, dep); client.IgnoreNotFound(err) != nil {
		return err
	}
	if _, has := svc.Annotations[AnnURL]; !has {
		return nil
	}
	base := svc.DeepCopy()
	delete(svc.Annotations, AnnURL)
	delete(svc.Annotations, AnnURLUpdated)
	return r.Patch(ctx, svc, client.MergeFrom(base))
}

var one int32 = 1

func (r *ServiceReconciler) tunnelDeployment(svc *corev1.Service) *appsv1.Deployment {
	origin := OriginURL(svc)
	labels := map[string]string{
		ManagedByLabel:        ManagedByValue,
		LabelServiceNamespace: svc.Namespace,
		LabelServiceName:      svc.Name,
	}
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
			Name:      DeploymentName(svc.Namespace, svc.Name),
			Namespace: svc.Namespace,
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
						Image:           r.CloudflaredImage,
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
