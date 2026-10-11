package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrl "sigs.k8s.io/controller-runtime"
)

type fakeManager struct {
	ensured map[types.NamespacedName]string
	stopped map[types.NamespacedName]int
}

func newFakeManager() *fakeManager {
	return &fakeManager{
		ensured: map[types.NamespacedName]string{},
		stopped: map[types.NamespacedName]int{},
	}
}

func (f *fakeManager) Ensure(_ context.Context, owner OwnerRef, originURL string) {
	f.ensured[owner.Key()] = originURL
}

func (f *fakeManager) Stop(key types.NamespacedName) {
	f.stopped[key]++
}

func testService(annotations map[string]string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "demo",
			Namespace:   "default",
			Annotations: annotations,
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{Port: 80}},
		},
	}
}

func newTestReconciler(t *testing.T, mgr *fakeManager, objs ...client.Object) *ServiceReconciler {
	t.Helper()
	sch := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).Build()
	return &ServiceReconciler{
		Client:           c,
		Scheme:           sch,
		CloudflaredImage: DefaultCloudflaredImage,
		Watchdog:         mgr,
	}
}

func getTunnelArgs(t *testing.T, c client.Client, ns, name string) []string {
	t.Helper()
	dep := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, dep); err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	return dep.Spec.Template.Spec.Containers[0].Args
}

func TestReconcileCreatesDeployment(t *testing.T) {
	fm := newFakeManager()
	svc := testService(map[string]string{AnnExpose: "true"})
	r := newTestReconciler(t, fm, svc)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "demo"},
	}); err != nil {
		t.Fatal(err)
	}

	want := "http://demo.default.svc.cluster.local:80"
	args := getTunnelArgs(t, r.Client, "default", DeploymentName(OwnerRef{Kind: "Service", Namespace: "default", Name: "demo"}))
	found := false
	for i, a := range args {
		if a == "--url" && i+1 < len(args) && args[i+1] == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("--url %q not found in args %v", want, args)
	}
	if fm.ensured[types.NamespacedName{Namespace: "default", Name: "demo"}] != want {
		t.Fatalf("watchdog not ensured with origin %q, got %q", want, fm.ensured[types.NamespacedName{Namespace: "default", Name: "demo"}])
	}
}

func TestReconcileHonorsPortAndSchemeAnnotations(t *testing.T) {
	fm := newFakeManager()
	svc := testService(map[string]string{
		AnnExpose: "true",
		AnnPort:   "8443",
		AnnScheme: "https",
	})
	r := newTestReconciler(t, fm, svc)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "demo"},
	}); err != nil {
		t.Fatal(err)
	}

	args := getTunnelArgs(t, r.Client, "default", DeploymentName(OwnerRef{Kind: "Service", Namespace: "default", Name: "demo"}))
	assertContains(t, args, "https://demo.default.svc.cluster.local:8443")
	assertContains(t, args, "--no-tls-verify")
}

func TestReconcileUpdatesOriginOnAnnotationChange(t *testing.T) {
	fm := newFakeManager()
	svc := testService(map[string]string{AnnExpose: "true"})
	r := newTestReconciler(t, fm, svc)
	ctx := context.Background()
	key := types.NamespacedName{Namespace: "default", Name: "demo"}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}

	svc.Annotations[AnnPort] = "8080"
	if err := r.Update(ctx, svc); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}

	args := getTunnelArgs(t, r.Client, "default", DeploymentName(OwnerRef{Kind: "Service", Namespace: "default", Name: "demo"}))
	assertContains(t, args, "http://demo.default.svc.cluster.local:8080")
}

func TestReconcileTeardownOnOptOut(t *testing.T) {
	fm := newFakeManager()
	svc := testService(map[string]string{AnnExpose: "true", AnnURL: "https://old.trycloudflare.com"})
	r := newTestReconciler(t, fm, svc)
	ctx := context.Background()
	key := types.NamespacedName{Namespace: "default", Name: "demo"}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}

	svc.Annotations[AnnExpose] = "false"
	if err := r.Update(ctx, svc); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}

	dep := &appsv1.Deployment{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: DeploymentName(OwnerRef{Kind: "Service", Namespace: "default", Name: "demo"})}, dep); err == nil {
		t.Fatal("tunnel deployment still exists after opt-out")
	}
	fresh := &corev1.Service{}
	if err := r.Client.Get(ctx, key, fresh); err != nil {
		t.Fatal(err)
	}
	if _, ok := fresh.Annotations[AnnURL]; ok {
		t.Fatal("quicktunnel.dev/url annotation not cleaned up")
	}
	if fm.stopped[key] == 0 {
		t.Fatal("watchdog not stopped")
	}
}

func TestReconcileStopsWatchdogOnServiceDelete(t *testing.T) {
	fm := newFakeManager()
	r := newTestReconciler(t, fm)
	key := types.NamespacedName{Namespace: "gone", Name: "svc"}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if fm.stopped[key] == 0 {
		t.Fatal("watchdog not stopped for deleted Service")
	}
}

func TestDeploymentOwnership(t *testing.T) {
	fm := newFakeManager()
	svc := testService(map[string]string{AnnExpose: "true"})
	r := newTestReconciler(t, fm, svc)
	ctx := context.Background()

	if _, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "demo"},
	}); err != nil {
		t.Fatal(err)
	}

	dep := &appsv1.Deployment{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: DeploymentName(OwnerRef{Kind: "Service", Namespace: "default", Name: "demo"})}, dep); err != nil {
		t.Fatal(err)
	}
	if len(dep.OwnerReferences) != 1 || dep.OwnerReferences[0].Kind != "Service" || !*dep.OwnerReferences[0].Controller {
		t.Fatalf("expected controlled-by Service ownerReference, got %+v", dep.OwnerReferences)
	}
	if dep.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatalf("expected Recreate strategy, got %s", dep.Spec.Strategy.Type)
	}
}

func assertContains(t *testing.T, args []string, want string) {
	t.Helper()
	for _, a := range args {
		if a == want {
			return
		}
	}
	t.Fatalf("%q not found in %v", want, args)
}
