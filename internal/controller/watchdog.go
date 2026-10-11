package controller

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/go-logr/logr"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var tunnelURLRe = regexp.MustCompile(`https://[a-zA-Z0-9][a-zA-Z0-9-]*\.trycloudflare\.com`)

// ExtractTunnelURL scans cloudflared log output (plain box or --output json)
// for the assigned trycloudflare.com URL.
func ExtractTunnelURL(output []byte) (string, bool) {
	m := tunnelURLRe.Find(output)
	if m == nil {
		return "", false
	}
	return string(m), true
}

// podLogReader tails the logs of a tunnel pod via the API server.
type podLogReader interface {
	podLogs(ctx context.Context, namespace, name string, tail int64) ([]byte, error)
}

type kubeLogReader struct{ core kubernetes.Interface }

func (r kubeLogReader) podLogs(ctx context.Context, namespace, name string, tail int64) ([]byte, error) {
	stream, err := r.core.CoreV1().Pods(namespace).GetLogs(name, &corev1.PodLogOptions{
		Container:  "tunnel",
		TailLines:  &tail,
		Timestamps: false,
	}).Stream(ctx)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	return io.ReadAll(stream)
}

// metricsReader reads cloudflared's /metrics endpoint from the tunnel pod.
type metricsReader interface {
	haConnections(ctx context.Context, addr string) (int, error)
}

type httpMetricsReader struct{ hc *http.Client }

func (r httpMetricsReader) haConnections(ctx context.Context, addr string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/metrics", nil)
	if err != nil {
		return 0, err
	}
	resp, err := r.hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") || !strings.HasPrefix(line, "cloudflared_tunnel_ha_connections") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		return strconv.Atoi(fields[len(fields)-1])
	}
	return 0, fmt.Errorf("metric cloudflared_tunnel_ha_connections not found")
}

// Tunables exposes the watchdog timings for tests.
type Tunables struct {
	PollInterval    time.Duration // log polling while harvesting the URL
	HarvestTimeout time.Duration // restart the pod if no URL appears by then
	ProbeInterval  time.Duration // health probe cadence
	// A tunnel is recycled after this many consecutive probes reporting zero
	// edge connections (process alive, tunnel dead), or unreachable metrics.
	ZeroStrikes   int
	DownStrikes   int
}

func defaultTunables() Tunables {
	return Tunables{
		PollInterval:    3 * time.Second,
		HarvestTimeout:  2 * time.Minute,
		ProbeInterval:   30 * time.Second,
		ZeroStrikes:     2,
		DownStrikes:     3,
	}
}

// TunnelWatchdog owns one background goroutine per exposed Service. The loop
// harvests the quick tunnel URL from tunnel pod logs, publishes it to the
// Service annotations, and recycles the pod when the tunnel wedges. Pod
// identity is re-resolved on every probe so deployment rollouts are followed.
type TunnelWatchdog struct {
	client  client.Client
	logs    podLogReader
	metrics metricsReader
	t       Tunables

	mu    sync.Mutex
	loops map[types.NamespacedName]*tunnelLoop
}

func NewTunnelWatchdog(c client.Client, core kubernetes.Interface) *TunnelWatchdog {
	return &TunnelWatchdog{
		client:  c,
		logs:    kubeLogReader{core: core},
		metrics: httpMetricsReader{hc: &http.Client{Timeout: 5 * time.Second}},
		t:       defaultTunables(),
		loops:   map[types.NamespacedName]*tunnelLoop{},
	}
}

func (w *TunnelWatchdog) Ensure(ctx context.Context, svc *corev1.Service, originURL string) {
	key := types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}
	w.mu.Lock()
	defer w.mu.Unlock()
	if lp, ok := w.loops[key]; ok {
		if lp.origin == originURL {
			return
		}
		lp.cancel()
		delete(w.loops, key)
	}
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	lp := &tunnelLoop{
		wd:     w,
		svc:    key,
		origin: originURL,
		cancel: cancel,
	}
	w.loops[key] = lp
	go func() {
		lp.run(loopCtx)
		w.forget(key, lp)
	}()
}

func (w *TunnelWatchdog) Stop(key types.NamespacedName) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if lp, ok := w.loops[key]; ok {
		lp.cancel()
		delete(w.loops, key)
	}
}

func (w *TunnelWatchdog) forget(key types.NamespacedName, lp *tunnelLoop) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if cur, ok := w.loops[key]; ok && cur == lp {
		delete(w.loops, key)
	}
}

type tunnelLoop struct {
	wd     *TunnelWatchdog
	svc    types.NamespacedName
	origin string
	cancel context.CancelFunc
}

func (lp *tunnelLoop) logger() logr.Logger {
	return log.Log.WithName("watchdog").WithValues("service", lp.svc.String(), "origin", lp.origin)
}

func (lp *tunnelLoop) run(ctx context.Context) {
	logger := lp.logger()
	for ctx.Err() == nil {
		pod := lp.waitForPod(ctx)
		if pod == nil {
			return
		}
		url, ok := lp.harvest(ctx, pod)
		if !ok {
			continue
		}
		lp.publish(ctx, url)
		logger.Info("tunnel URL published", "url", url)
		lp.probeUntilUnhealthy(ctx)
	}
}

func (lp *tunnelLoop) waitForPod(ctx context.Context) *corev1.Pod {
	for {
		pods, err := lp.listPods(ctx)
		if err == nil && len(pods) > 0 {
			return &pods[0]
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}

func (lp *tunnelLoop) listPods(ctx context.Context) ([]corev1.Pod, error) {
	list := &corev1.PodList{}
	err := lp.wd.client.List(ctx, list,
		client.InNamespace(lp.svc.Namespace),
		client.MatchingLabels{
			ManagedByLabel:        ManagedByValue,
			LabelServiceNamespace: lp.svc.Namespace,
			LabelServiceName:      lp.svc.Name,
		})
	if err != nil {
		return nil, err
	}
	var running []corev1.Pod
	for _, p := range list.Items {
		if p.Status.Phase == corev1.PodRunning && p.DeletionTimestamp == nil {
			running = append(running, p)
		}
	}
	return running, nil
}

func (lp *tunnelLoop) harvest(ctx context.Context, pod *corev1.Pod) (string, bool) {
	deadline := time.Now().Add(lp.wd.t.HarvestTimeout)
	for time.Now().Before(deadline) {
		out, err := lp.wd.logs.podLogs(ctx, pod.Namespace, pod.Name, 200)
		if err == nil {
			if url, ok := ExtractTunnelURL(out); ok {
				return url, true
			}
		}
		select {
		case <-ctx.Done():
			return "", false
		case <-time.After(lp.wd.t.PollInterval):
		}
	}
	lp.logger().Info("no tunnel URL in logs, recycling tunnel pod", "pod", pod.Name)
	lp.deletePod(ctx, pod)
	return "", false
}

func (lp *tunnelLoop) publish(ctx context.Context, url string) {
	svc := &corev1.Service{}
	if err := lp.wd.client.Get(ctx, lp.svc, svc); err != nil {
		// Next reconcile re-asserts the loop and republishes.
		lp.logger().Error(err, "cannot read Service for URL publish")
		return
	}
	if svc.Annotations[AnnURL] == url {
		return
	}
	base := svc.DeepCopy()
	if svc.Annotations == nil {
		svc.Annotations = map[string]string{}
	}
	svc.Annotations[AnnURL] = url
	svc.Annotations[AnnURLUpdated] = time.Now().UTC().Format(time.RFC3339)
	if err := lp.wd.client.Patch(ctx, svc, client.MergeFrom(base)); err != nil {
		lp.logger().Error(err, "failed to publish tunnel URL", "url", url)
	}
}

// probeUntilUnhealthy blocks until the context is done or the tunnel is
// considered wedged, in which case the current pod is deleted so the
// Deployment rolls a fresh one (and run() harvests its new URL).
func (lp *tunnelLoop) probeUntilUnhealthy(ctx context.Context) {
	zero, down := 0, 0
	ticker := time.NewTicker(lp.wd.t.ProbeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		pods, err := lp.listPods(ctx)
		if err != nil || len(pods) == 0 {
			down++
			zero = 0
		} else {
			addr := fmt.Sprintf("%s:%d", pods[0].Status.PodIP, TunnelMetricsPort)
			n, err := lp.wd.metrics.haConnections(ctx, addr)
			switch {
			case err != nil:
				down++
				zero = 0
			case n > 0:
				zero, down = 0, 0
			default:
				zero++
				down = 0
			}
		}
		if zero >= lp.wd.t.ZeroStrikes || down >= lp.wd.t.DownStrikes {
			if len(pods) > 0 {
				lp.logger().Info("tunnel wedged, recycling tunnel pod",
					"pod", pods[0].Name, "zeroStrikes", zero, "downStrikes", down)
				lp.deletePod(ctx, &pods[0])
			}
			return
		}
	}
}

func (lp *tunnelLoop) deletePod(ctx context.Context, pod *corev1.Pod) {
	_ = lp.wd.client.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      pod.Name,
		Namespace: pod.Namespace,
	}})
}
