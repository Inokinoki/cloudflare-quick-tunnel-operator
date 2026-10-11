package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestExtractTunnelURLFromPlainLogs(t *testing.T) {
	out := []byte(`
2026-10-11T10:00:00Z INF +--------------------------------------------------------------------------------------------+
2026-10-11T10:00:00Z INF |  Your quick Tunnel has been created! Visit it at (it may take some time to be reachable):  |
2026-10-11T10:00:00Z INF |  https://quiet-marble-otter.trycloudflare.com                                               |
2026-10-11T10:00:00Z INF +--------------------------------------------------------------------------------------------+
`)
	url, ok := ExtractTunnelURL(out)
	if !ok || url != "https://quiet-marble-otter.trycloudflare.com" {
		t.Fatalf("got %q ok=%v", url, ok)
	}
}

func TestExtractTunnelURLFromJSONLogs(t *testing.T) {
	out := []byte(`{"level":"info","t":"2026-10-11T10:00:00Z","msg":"https://noisy-lavender-lynx.trycloudflare.com"}`)
	url, ok := ExtractTunnelURL(out)
	if !ok || url != "https://noisy-lavender-lynx.trycloudflare.com" {
		t.Fatalf("got %q ok=%v", url, ok)
	}
}

func TestExtractTunnelURLNoMatch(t *testing.T) {
	if _, ok := ExtractTunnelURL([]byte("INF Registered tunnel connection")); ok {
		t.Fatal("unexpected match")
	}
}

func TestDeploymentName(t *testing.T) {
	cases := []struct{ ns, name string }{
		{"default", "demo"},
		{"kube-system", "a-very-long-service-name-that-exceeds-the-sixty-three-character-limit-for-sure"},
		{"ingress-nginx", "demo.web.example.com"},
	}
	for _, c := range cases {
		got := DeploymentName(c.ns, c.name)
		if len(got) > 63 {
			t.Fatalf("%s/%s: %q exceeds 63 chars", c.ns, c.name, got)
		}
		if !strings.HasPrefix(got, "qt-") {
			t.Fatalf("%s/%s: %q lacks qt- prefix", c.ns, c.name, got)
		}
	}
	if DeploymentName("default", "x") != DeploymentName("default", "x") {
		t.Fatal("name not deterministic")
	}
	if DeploymentName("default", "x") == DeploymentName("default", "y") {
		t.Fatal("name collision")
	}
}

type fakeLogReader struct {
	calls  int
	output map[string][]byte // pod name prefix match -> output
}

func (f *fakeLogReader) podLogs(_ context.Context, _, name string, _ int64) ([]byte, error) {
	f.calls++
	for prefix, out := range f.output {
		if strings.Contains(name, prefix) {
			return out, nil
		}
	}
	return nil, nil
}

type fakeMetrics struct {
	ha   int
	err  bool
	calls int
}

func (f *fakeMetrics) haConnections(_ context.Context, _ string) (int, error) {
	f.calls++
	if f.err {
		return 0, context.DeadlineExceeded
	}
	return f.ha, nil
}

func newWatchdogForTest(c client.Client, logs podLogReader, metrics metricsReader) *TunnelWatchdog {
	return &TunnelWatchdog{
		client:  c,
		logs:    logs,
		metrics: metrics,
		loops:   map[types.NamespacedName]*tunnelLoop{},
		t: Tunables{
			PollInterval:    5 * time.Millisecond,
			HarvestTimeout:  500 * time.Millisecond,
			ProbeInterval:   10 * time.Millisecond,
			ZeroStrikes:     2,
			DownStrikes:     3,
		},
	}
}

func newWatchdogClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	sch := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).Build()
}

func tunnelPod(name, ns, svcName string, phase corev1.PodPhase, ip string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				ManagedByLabel:        ManagedByValue,
				LabelServiceNamespace: ns,
				LabelServiceName:      svcName,
			},
		},
		Status: corev1.PodStatus{Phase: phase, PodIP: ip},
	}
}

func TestWatchdogPublishesURLAndRecyclesZombie(t *testing.T) {
	svc := testService(map[string]string{AnnExpose: "true"})
	ctx := context.Background()

	pod1 := tunnelPod("tunnel-a", "default", "demo", corev1.PodRunning, "10.0.0.1")
	c := newWatchdogClient(t, svc, pod1)

	logs := &fakeLogReader{output: map[string][]byte{
		"tunnel-a": []byte("INF https://zombie-test.trycloudflare.com"),
	}}
	// Start healthy, then report a dead tunnel on every probe.
	metrics := &fakeMetrics{ha: 4}
	w := newWatchdogForTest(c, logs, metrics)

	w.Ensure(ctx, svc, "http://demo.default.svc.cluster.local:80")
	defer w.Stop(types.NamespacedName{Namespace: "default", Name: "demo"})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		fresh := &corev1.Service{}
		_ = c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "demo"}, fresh)
		if fresh.Annotations[AnnURL] == "https://zombie-test.trycloudflare.com" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	fresh := &corev1.Service{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "demo"}, fresh); err != nil {
		t.Fatal(err)
	}
	if got := fresh.Annotations[AnnURL]; got != "https://zombie-test.trycloudflare.com" {
		t.Fatalf("URL not published, got %q", got)
	}

	// Tunnel goes zombie: metrics says zero connections from now on.
	metrics.ha = 0
	for time.Now().Before(deadline) {
		podList := &corev1.PodList{}
		_ = c.List(ctx, podList, client.InNamespace("default"))
		if len(podList.Items) == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	podList := &corev1.PodList{}
	_ = c.List(ctx, podList, client.InNamespace("default"))
	if len(podList.Items) != 0 {
		t.Fatalf("zombie tunnel pod not recycled, %d pods remain", len(podList.Items))
	}
}

func TestWatchdogHarvestTimeoutRecyclesPod(t *testing.T) {
	svc := testService(map[string]string{AnnExpose: "true"})
	ctx := context.Background()
	pod := tunnelPod("tunnel-b", "default", "demo", corev1.PodRunning, "10.0.0.2")
	c := newWatchdogClient(t, svc, pod)

	// Log reader never yields a URL.
	logs := &fakeLogReader{}
	metrics := &fakeMetrics{ha: 4}
	w := newWatchdogForTest(c, logs, metrics)
	w.Ensure(ctx, svc, "http://demo.default.svc.cluster.local:80")
	defer w.Stop(types.NamespacedName{Namespace: "default", Name: "demo"})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		podList := &corev1.PodList{}
		_ = c.List(ctx, podList, client.InNamespace("default"))
		if len(podList.Items) == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("tunnel pod not recycled after harvest timeout")
}
