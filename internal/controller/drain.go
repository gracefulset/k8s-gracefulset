package controller

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/utils/exec"

	appsv1alpha1 "github.com/gracefulset-io/gracefulset/api/v1alpha1"
)

const checkTimeout = 5 * time.Second

var defaultHTTPClient = &http.Client{
	Timeout: checkTimeout,
	Transport: &http.Transport{
		// Pods are addressed by IP, so certificates can never match the host name.
		// Kubelet HTTPS probes skip verification for the same reason.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
	},
}

func (r *GracefulSetReconciler) httpClient() *http.Client {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return defaultHTTPClient
}

func drainCheckOrDefault(gs *appsv1alpha1.GracefulSet) *appsv1alpha1.DrainCheck {
	if dc := gs.Spec.DrainPolicy.DrainCheck; dc != nil {
		return dc
	}
	return &appsv1alpha1.DrainCheck{Type: appsv1alpha1.DrainCheckHTTP, Path: "/drain-status", Port: 8080, Scheme: "HTTP", PeriodSeconds: 30, JSONField: "inflight"}
}

func podURL(pod *corev1.Pod, scheme string, port int32, path string) string {
	if scheme == "" {
		scheme = "HTTP"
	}
	return fmt.Sprintf("%s://%s%s", strings.ToLower(scheme), net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(int(port))), path)
}

type checkResult struct {
	drained bool
	err     error
}

// checkAll runs fn for every pod in parallel so slow pods do not hold up the
// reconcile worker.
func (r *GracefulSetReconciler) checkAll(ctx context.Context, pods []corev1.Pod, fn func(context.Context, *corev1.Pod) (bool, error)) []checkResult {
	results := make([]checkResult, len(pods))
	var wg sync.WaitGroup
	for i := range pods {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i].drained, results[i].err = fn(ctx, &pods[i])
		}(i)
	}
	wg.Wait()
	return results
}

func (r *GracefulSetReconciler) callDrainHook(ctx context.Context, pod *corev1.Pod, hook *appsv1alpha1.DrainHook) error {
	path, port := hook.Path, hook.Port
	if path == "" {
		path = "/drain"
	}
	if port == 0 {
		port = 8080
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, podURL(pod, hook.Scheme, port, path), nil)
	if err != nil {
		return err
	}
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("drain hook returned status %d", resp.StatusCode)
	}
	return nil
}

// checkPodDrained runs the configured drain check against one pod.
func (r *GracefulSetReconciler) checkPodDrained(ctx context.Context, pod *corev1.Pod, dc *appsv1alpha1.DrainCheck) (bool, error) {
	switch dc.Type {
	case appsv1alpha1.DrainCheckTCP:
		return checkTCP(ctx, pod, dc.Port)
	case appsv1alpha1.DrainCheckExec:
		return r.checkExec(ctx, pod, dc.Exec)
	default:
		v, err := r.httpValue(ctx, pod, dc)
		if err != nil {
			return false, err
		}
		return v <= float64(dc.Threshold), nil
	}
}

// httpValue fetches the drain endpoint and returns the number at jsonField.
// A boolean true counts as 0 (drained) and false as +Inf (not drained).
func (r *GracefulSetReconciler) httpValue(ctx context.Context, pod *corev1.Pod, dc *appsv1alpha1.DrainCheck) (float64, error) {
	if pod.Status.PodIP == "" {
		return 0, fmt.Errorf("pod has no IP yet")
	}
	path, port, field := dc.Path, dc.Port, dc.JSONField
	if path == "" {
		path = "/drain-status"
	}
	if port == 0 {
		port = 8080
	}
	if field == "" {
		field = "inflight"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, podURL(pod, dc.Scheme, port, path), nil)
	if err != nil {
		return 0, err
	}
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("drain endpoint returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, err
	}
	var val interface{}
	if err := json.Unmarshal(body, &val); err != nil {
		return 0, fmt.Errorf("failed to parse drain response: %w", err)
	}
	for _, key := range strings.Split(field, ".") {
		obj, ok := val.(map[string]interface{})
		if !ok {
			return 0, fmt.Errorf("drain response missing field %q", field)
		}
		if val, ok = obj[key]; !ok {
			return 0, fmt.Errorf("drain response missing field %q", field)
		}
	}
	switch v := val.(type) {
	case float64:
		return v, nil
	case bool:
		if v {
			return 0, nil
		}
		return math.Inf(1), nil
	default:
		return 0, fmt.Errorf("unexpected type for field %q", field)
	}
}

// checkTCP reports a pod drained once its port refuses connections. Timeouts
// are treated as errors (retried), not as drained.
func checkTCP(ctx context.Context, pod *corev1.Pod, port int32) (bool, error) {
	if pod.Status.PodIP == "" {
		return false, fmt.Errorf("pod has no IP yet")
	}
	if port == 0 {
		port = 8080
	}
	d := net.Dialer{Timeout: checkTimeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(int(port))))
	if err == nil {
		conn.Close()
		return false, nil
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return false, err
	}
	return true, nil
}

// checkExec runs a command in the pod; exit code 0 means drained.
func (r *GracefulSetReconciler) checkExec(ctx context.Context, pod *corev1.Pod, ex *appsv1alpha1.ExecDrainCheck) (bool, error) {
	if ex == nil || len(ex.Command) == 0 {
		return false, fmt.Errorf("drainCheck.exec.command is required for Exec checks")
	}
	if r.Clientset == nil || r.RestConfig == nil {
		return false, fmt.Errorf("exec checks are not configured in this controller")
	}
	container := ex.Container
	if container == "" && len(pod.Spec.Containers) > 0 {
		container = pod.Spec.Containers[0].Name
	}
	req := r.Clientset.CoreV1().RESTClient().Post().Resource("pods").Namespace(pod.Namespace).Name(pod.Name).
		SubResource("exec").VersionedParams(&corev1.PodExecOptions{
		Container: container, Command: ex.Command, Stdout: true, Stderr: true,
	}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(r.RestConfig, http.MethodPost, req.URL())
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*checkTimeout)
	defer cancel()
	var out bytes.Buffer
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &out, Stderr: &out})
	if err == nil {
		return true, nil
	}
	var exitErr utilexec.ExitError
	if errors.As(err, &exitErr) {
		return false, nil // command ran and said "not yet"
	}
	return false, fmt.Errorf("exec drain check: %w", err)
}

// inflightValues reads the HTTP drain value of each pod so scale-down can drain
// the least busy pods first. Only used with WaitForDrain + HTTP checks; pods
// that cannot be read sort last.
func (r *GracefulSetReconciler) inflightValues(ctx context.Context, gs *appsv1alpha1.GracefulSet, pods []corev1.Pod) map[string]float64 {
	dc := drainCheckOrDefault(gs)
	if gs.Spec.DrainPolicy.Mode != appsv1alpha1.DrainPolicyWaitForDrain || (dc.Type != "" && dc.Type != appsv1alpha1.DrainCheckHTTP) || len(pods) < 2 {
		return nil
	}
	values := make([]float64, len(pods))
	r.checkAll(ctx, pods, func(ctx context.Context, p *corev1.Pod) (bool, error) {
		v, err := r.httpValue(ctx, p, dc)
		if err != nil {
			v = math.Inf(1)
		}
		for i := range pods {
			if &pods[i] == p {
				values[i] = v
			}
		}
		return false, nil
	})
	m := make(map[string]float64, len(pods))
	for i := range pods {
		m[pods[i].Name] = values[i]
	}
	return m
}
