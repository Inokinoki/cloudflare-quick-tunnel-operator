package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	nwv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func ingressClassPtr(s string) *string { return &s }

func testIngress(rules ...nwv1.IngressRule) *nwv1.Ingress {
	return &nwv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "default"},
		Spec: nwv1.IngressSpec{
			IngressClassName: ingressClassPtr("quicktunnel"),
			Rules:            rules,
		},
	}
}

func hostRule(host, svcName string, port int32) nwv1.IngressRule {
	return nwv1.IngressRule{
		Host: host,
		IngressRuleValue: nwv1.IngressRuleValue{
			HTTP: &nwv1.HTTPIngressRuleValue{
				Paths: []nwv1.HTTPIngressPath{{
					Path:     "/",
					PathType: pathTypePtr(nwv1.PathTypePrefix),
					Backend: nwv1.IngressBackend{
						Service: &nwv1.IngressServiceBackend{
							Name: svcName,
							Port: nwv1.ServiceBackendPort{Number: port},
						},
					},
				}},
			},
		},
	}
}

func pathTypePtr(t nwv1.PathType) *nwv1.PathType { return &t }

func newIngressReconciler(t *testing.T, fm *fakeManager, objs ...client.Object) *IngressReconciler {
	t.Helper()
	sch := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().
		WithScheme(sch).
		WithStatusSubresource(&nwv1.Ingress{}).
		WithObjects(objs...).
		Build()
	return &IngressReconciler{
		Client:           c,
		Scheme:           sch,
		CloudflaredImage: DefaultCloudflaredImage,
		Watchdog:         fm,
	}
}

func ingressOwner(group string) OwnerRef {
	return OwnerRef{Kind: "Ingress", Namespace: "default", Name: "demo", Group: group}
}

func TestIngressCreatesTunnelPerHost(t *testing.T) {
	fm := newFakeManager()
	r := newIngressReconciler(t, fm,
		testIngress(hostRule("web.demo.local", "demo-nginx", 80), hostRule("api.demo.local", "demo-echo", 5678)),
		backendService("demo-nginx"),
		backendService("demo-echo"),
	)

	reconcile(t, r, "default", "demo")

	want := map[string]string{
		"web.demo.local": "http://demo-nginx.default.svc.cluster.local:80",
		"api.demo.local": "http://demo-echo.default.svc.cluster.local:5678",
	}
	for host, origin := range want {
		args := getTunnelArgs(t, r.Client, "default", DeploymentName(ingressOwner(host)))
		assertContains(t, args, origin)
		if got := fm.ensured[loopKey(ingressOwner(host))]; got != origin {
			t.Fatalf("watchdog for host %q ensured %q, want %q", host, got, origin)
		}
	}
	if len(fm.ensured) != 2 {
		t.Fatalf("expected 2 ensured tunnels, got %v", fm.ensured)
	}
}

func TestIngressSecondBackendPerHostIgnored(t *testing.T) {
	twoBackends := hostRule("web.demo.local", "demo-nginx", 80)
	twoBackends.HTTP.Paths = append(twoBackends.HTTP.Paths, twoBackends.HTTP.Paths[0])
	fm := newFakeManager()
	r := newIngressReconciler(t, fm, testIngress(twoBackends), backendService("demo-nginx"))

	reconcile(t, r, "default", "demo")

	// one tunnel only: first backend wins
	args := getTunnelArgs(t, r.Client, "default", DeploymentName(ingressOwner("web.demo.local")))
	assertContains(t, args, "http://demo-nginx.default.svc.cluster.local:80")
	deps := &appsv1.DeploymentList{}
	_ = r.Client.List(context.Background(), deps, client.InNamespace("default"))
	if len(deps.Items) != 1 {
		t.Fatalf("expected exactly 1 tunnel deployment, got %d", len(deps.Items))
	}
}

func TestIngressDefaultBackendForHostless(t *testing.T) {
	fm := newFakeManager()
	ing := testIngress() // no rules
	ing.Spec.DefaultBackend = &nwv1.IngressBackend{
		Service: &nwv1.IngressServiceBackend{Name: "demo-nginx", Port: nwv1.ServiceBackendPort{Number: 80}},
	}
	r := newIngressReconciler(t, fm, ing, backendService("demo-nginx"))

	reconcile(t, r, "default", "demo")

	args := getTunnelArgs(t, r.Client, "default", DeploymentName(ingressOwner("")))
	assertContains(t, args, "http://demo-nginx.default.svc.cluster.local:80")
}

func TestIngressRemovesDroppedHost(t *testing.T) {
	fm := newFakeManager()
	ing := testIngress(hostRule("web.demo.local", "demo-nginx", 80), hostRule("api.demo.local", "demo-echo", 5678))
	r := newIngressReconciler(t, fm, ing, backendService("demo-nginx"), backendService("demo-echo"))
	ctx := context.Background()

	reconcile(t, r, "default", "demo")

	// Re-read: the reconcile may have patched status, bumping the version.
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "demo"}, ing); err != nil {
		t.Fatal(err)
	}
	ing.Spec.Rules = ing.Spec.Rules[:1] // drop the api host
	if err := r.Update(ctx, ing); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, "default", "demo")

	dep := &appsv1.Deployment{}
	if err := r.Client.Get(ctx, types.NamespacedName{
		Namespace: "default", Name: DeploymentName(ingressOwner("web.demo.local")),
	}, dep); err != nil {
		t.Fatal(err)
	}
	if err := r.Client.Get(ctx, types.NamespacedName{
		Namespace: "default", Name: DeploymentName(ingressOwner("api.demo.local")),
	}, dep); err == nil {
		t.Fatal("dropped host tunnel deployment still exists")
	}
	if fm.stopped[loopKey(ingressOwner("api.demo.local"))] == 0 {
		t.Fatal("watchdog for dropped host not stopped")
	}
}

func TestIngressUnclassifiedTeardown(t *testing.T) {
	fm := newFakeManager()
	ing := testIngress(hostRule("web.demo.local", "demo-nginx", 80))
	ing.Annotations = map[string]string{
		annURLKey("web.demo.local"):     "https://old.trycloudflare.com",
		annUpdatedKey("web.demo.local"): "yesterday",
	}
	r := newIngressReconciler(t, fm, ing, backendService("demo-nginx"))
	ctx := context.Background()

	reconcile(t, r, "default", "demo") // ensure tunnels exist first

	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "demo"}, ing); err != nil {
		t.Fatal(err)
	}
	ing.Spec.IngressClassName = ingressClassPtr("nginx")
	if err := r.Update(ctx, ing); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, "default", "demo")

	deps := &appsv1.DeploymentList{}
	_ = r.Client.List(ctx, deps, client.InNamespace("default"))
	if len(deps.Items) != 0 {
		t.Fatalf("expected all tunnel deployments removed, got %d", len(deps.Items))
	}
	fresh := &nwv1.Ingress{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "demo"}, fresh); err != nil {
		t.Fatal(err)
	}
	if _, ok := fresh.Annotations[annURLKey("web.demo.local")]; ok {
		t.Fatal("stale URL annotation not stripped")
	}
	if fm.stoppedAll["default/demo"] == 0 {
		t.Fatal("StopAll not called")
	}
}

func TestIngressStatusAggregatesHosts(t *testing.T) {
	fm := newFakeManager()
	ing := testIngress(hostRule("web.demo.local", "demo-nginx", 80))
	ing.Annotations = map[string]string{
		annURLKey(""):                  "https://bare.trycloudflare.com",
		annURLKey("web.demo.local"):    "https://web.trycloudflare.com",
		annUpdatedKey("web.demo.local"): "now",
	}
	r := newIngressReconciler(t, fm, ing, backendService("demo-nginx"))

	reconcile(t, r, "default", "demo")

	fresh := &nwv1.Ingress{}
	if err := r.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "demo"}, fresh); err != nil {
		t.Fatal(err)
	}
	if len(fresh.Status.LoadBalancer.Ingress) != 2 {
		t.Fatalf("expected 2 loadBalancer entries, got %+v", fresh.Status.LoadBalancer.Ingress)
	}
	if fresh.Status.LoadBalancer.Ingress[0].Hostname != "bare.trycloudflare.com" ||
		fresh.Status.LoadBalancer.Ingress[1].Hostname != "web.trycloudflare.com" {
		t.Fatalf("unexpected loadBalancer entries: %+v", fresh.Status.LoadBalancer.Ingress)
	}
}

func TestIngressForeignClassIgnored(t *testing.T) {
	fm := newFakeManager()
	ing := testIngress(hostRule("web.demo.local", "demo-nginx", 80))
	ing.Spec.IngressClassName = ingressClassPtr("nginx")
	r := newIngressReconciler(t, fm, ing, backendService("demo-nginx"))

	reconcile(t, r, "default", "demo")

	if len(fm.ensured) != 0 {
		t.Fatal("foreign-class Ingress must not create tunnels")
	}
}
