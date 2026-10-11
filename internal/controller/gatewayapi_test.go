package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func gatewayScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	if err := gwv1.Install(sch); err != nil {
		t.Fatal(err)
	}
	return sch
}

func testGatewayClass(name string, controllerName gwv1.GatewayController) *gwv1.GatewayClass {
	return &gwv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       gwv1.GatewayClassSpec{ControllerName: controllerName},
	}
}

func testGateway(name, className string) *gwv1.Gateway {
	return &gwv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: gwv1.GatewaySpec{
			GatewayClassName: gwv1.ObjectName(className),
			Listeners:        []gwv1.Listener{{Name: "http", Port: 80, Protocol: gwv1.HTTPProtocolType}},
		},
	}
}

func testHTTPRoute(name, gatewayName, svcName string, port int32) *gwv1.HTTPRoute {
	p := gwv1.PortNumber(port)
	return &gwv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: gwv1.HTTPRouteSpec{
			CommonRouteSpec: gwv1.CommonRouteSpec{
				ParentRefs: []gwv1.ParentReference{{Name: gwv1.ObjectName(gatewayName)}},
			},
			Rules: []gwv1.HTTPRouteRule{{
				BackendRefs: []gwv1.HTTPBackendRef{{
					BackendRef: gwv1.BackendRef{
						BackendObjectReference: gwv1.BackendObjectReference{
							Name: gwv1.ObjectName(svcName),
							Port: &p,
						},
					},
				}},
			}},
		},
	}
}

func backendService(name string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
	}
}

func newGatewayTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(gatewayScheme(t)).
		WithStatusSubresource(&gwv1.GatewayClass{}, &gwv1.Gateway{}, &gwv1.HTTPRoute{}).
		WithObjects(objs...).
		Build()
}

func reconcile(t *testing.T, r interface {
	Reconcile(context.Context, ctrl.Request) (ctrl.Result, error)
}, ns, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ns, Name: name},
	})
	if err != nil {
		t.Fatalf("reconcile %s/%s: %v", ns, name, err)
	}
	return res
}

func TestGatewayClassAccepted(t *testing.T) {
	c := newGatewayTestClient(t, testGatewayClass("quicktunnel", GatewayControllerName))
	r := &GatewayClassReconciler{Client: c, Scheme: gatewayScheme(t)}

	reconcile(t, r, "", "quicktunnel")

	gc := &gwv1.GatewayClass{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "quicktunnel"}, gc); err != nil {
		t.Fatal(err)
	}
	cond := apimeta.FindStatusCondition(gc.Status.Conditions, "Accepted")
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Accepted condition not set: %+v", cond)
	}
}

func TestGatewayClassForeignIgnored(t *testing.T) {
	c := newGatewayTestClient(t, testGatewayClass("foreign", "example.net/other"))
	r := &GatewayClassReconciler{Client: c, Scheme: gatewayScheme(t)}

	reconcile(t, r, "", "foreign")

	gc := &gwv1.GatewayClass{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "foreign"}, gc)
	if apimeta.FindStatusCondition(gc.Status.Conditions, "Accepted") != nil {
		t.Fatal("foreign GatewayClass must not be accepted")
	}
}

func TestHTTPRouteCreatesTunnel(t *testing.T) {
	fm := newFakeManager()
	c := newGatewayTestClient(t,
		testGatewayClass("quicktunnel", GatewayControllerName),
		testGateway("demo", "quicktunnel"),
		testHTTPRoute("demo", "demo", "demo-nginx", 80),
		backendService("demo-nginx"),
	)
	r := &HTTPRouteReconciler{
		Client:           c,
		Scheme:           gatewayScheme(t),
		CloudflaredImage: DefaultCloudflaredImage,
		Watchdog:         fm,
	}

	reconcile(t, r, "default", "demo")

	want := "http://demo-nginx.default.svc.cluster.local:80"
	args := getTunnelArgs(t, c, "default", DeploymentName(OwnerRef{Kind: "HTTPRoute", Namespace: "default", Name: "demo"}))
	assertContains(t, args, want)
	if got := fm.ensured[types.NamespacedName{Namespace: "default", Name: "demo"}]; got != want {
		t.Fatalf("watchdog ensured %q, want %q", got, want)
	}

	route := &gwv1.HTTPRoute{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "demo"}, route); err != nil {
		t.Fatal(err)
	}
	if len(route.Status.Parents) != 1 ||
		string(route.Status.Parents[0].ControllerName) != GatewayControllerName {
		t.Fatalf("unexpected parents status: %+v", route.Status.Parents)
	}
	for _, typ := range []string{"Accepted", "ResolvedRefs"} {
		cond := apimeta.FindStatusCondition(route.Status.Parents[0].Conditions, typ)
		if cond == nil || cond.Status != metav1.ConditionTrue {
			t.Fatalf("%s condition not true: %+v", typ, cond)
		}
	}
}

func TestGatewayAggregatesRouteURLs(t *testing.T) {
	route := testHTTPRoute("demo", "demo", "demo-nginx", 80)
	route.Annotations = map[string]string{AnnURL: "https://gw-test.trycloudflare.com"}
	c := newGatewayTestClient(t,
		testGatewayClass("quicktunnel", GatewayControllerName),
		testGateway("demo", "quicktunnel"),
		route,
	)
	r := &GatewayReconciler{Client: c, Scheme: gatewayScheme(t)}

	reconcile(t, r, "default", "demo")

	gw := &gwv1.Gateway{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "demo"}, gw); err != nil {
		t.Fatal(err)
	}
	if len(gw.Status.Addresses) != 1 || gw.Status.Addresses[0].Value != "gw-test.trycloudflare.com" {
		t.Fatalf("unexpected addresses: %+v", gw.Status.Addresses)
	}
	prog := apimeta.FindStatusCondition(gw.Status.Conditions, "Programmed")
	if prog == nil || prog.Status != metav1.ConditionTrue {
		t.Fatalf("Programmed not true: %+v", prog)
	}
	if len(gw.Status.Listeners) != 1 ||
		apimeta.FindStatusCondition(gw.Status.Listeners[0].Conditions, "Ready") == nil {
		t.Fatalf("listener status missing: %+v", gw.Status.Listeners)
	}
}

func TestGatewayProgrammedPendingWithoutRoutes(t *testing.T) {
	c := newGatewayTestClient(t,
		testGatewayClass("quicktunnel", GatewayControllerName),
		testGateway("demo", "quicktunnel"),
	)
	r := &GatewayReconciler{Client: c, Scheme: gatewayScheme(t)}

	reconcile(t, r, "default", "demo")

	gw := &gwv1.Gateway{}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "demo"}, gw)
	prog := apimeta.FindStatusCondition(gw.Status.Conditions, "Programmed")
	if prog == nil || prog.Status != metav1.ConditionFalse {
		t.Fatalf("Programmed should be false without routes: %+v", prog)
	}
}

func TestHTTPRouteBackendNotFound(t *testing.T) {
	fm := newFakeManager()
	c := newGatewayTestClient(t,
		testGatewayClass("quicktunnel", GatewayControllerName),
		testGateway("demo", "quicktunnel"),
		testHTTPRoute("demo", "demo", "missing-svc", 80),
	)
	r := &HTTPRouteReconciler{
		Client:           c,
		Scheme:           gatewayScheme(t),
		CloudflaredImage: DefaultCloudflaredImage,
		Watchdog:         fm,
	}

	reconcile(t, r, "default", "demo")

	dep := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: "default", Name: DeploymentName(OwnerRef{Kind: "HTTPRoute", Namespace: "default", Name: "demo"}),
	}, dep); err == nil {
		t.Fatal("tunnel deployment must not exist for missing backend")
	}
	route := &gwv1.HTTPRoute{}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "demo"}, route)
	if len(route.Status.Parents) != 1 {
		t.Fatalf("expected parent status: %+v", route.Status.Parents)
	}
	resolved := apimeta.FindStatusCondition(route.Status.Parents[0].Conditions, "ResolvedRefs")
	if resolved == nil || resolved.Status != metav1.ConditionFalse || resolved.Reason != "BackendNotFound" {
		t.Fatalf("ResolvedRefs not BackendNotFound: %+v", resolved)
	}
}

func TestHTTPRouteForeignGatewayIgnored(t *testing.T) {
	fm := newFakeManager()
	c := newGatewayTestClient(t,
		testGatewayClass("foreign", "example.net/other"),
		testGateway("demo", "foreign"),
		testHTTPRoute("demo", "demo", "demo-nginx", 80),
		testService(map[string]string{}),
	)
	r := &HTTPRouteReconciler{
		Client:           c,
		Scheme:           gatewayScheme(t),
		CloudflaredImage: DefaultCloudflaredImage,
		Watchdog:         fm,
	}

	reconcile(t, r, "default", "demo")

	if len(fm.ensured) != 0 {
		t.Fatal("watchdog must not be ensured for foreign Gateway")
	}
	route := &gwv1.HTTPRoute{}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "demo"}, route)
	if len(route.Status.Parents) != 0 {
		t.Fatal("status must not be written for foreign Gateway")
	}
}
