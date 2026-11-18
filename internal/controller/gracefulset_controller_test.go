package controller

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1alpha1 "github.com/gracefulset-io/gracefulset/api/v1alpha1"
)

const ns = "default"

func ptr[T any](v T) *T { return &v }

func newGS(version string, replicas int32, policy appsv1alpha1.DrainPolicy) *appsv1alpha1.GracefulSet {
	return &appsv1alpha1.GracefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: ns},
		Spec: appsv1alpha1.GracefulSetSpec{
			Replicas:    ptr(replicas),
			Version:     version,
			DrainPolicy: policy,
			Selector:    &metav1.LabelSelector{MatchLabels: map[string]string{"app": "app"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "app"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img:" + version}}},
			},
		},
	}
}

func newPod(name, version string, phase corev1.PodPhase, age time.Duration) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-age)),
			Labels:            map[string]string{"app": "app", ownerLabel: "app", versionLabel: version},
			Annotations:       map[string]string{},
		},
		Spec: corev1.PodSpec{
			Containers:     []corev1.Container{{Name: "c", Image: "img"}},
			ReadinessGates: []corev1.PodReadinessGate{{ConditionType: servingCondition}},
		},
		Status: corev1.PodStatus{Phase: phase, PodIP: "127.0.0.1"},
	}
}

func setup(t *testing.T, objs ...client.Object) (*GracefulSetReconciler, client.Client) {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := appsv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&appsv1alpha1.GracefulSet{}, &corev1.Pod{}).Build()
	return &GracefulSetReconciler{Client: c, APIReader: c, Scheme: s, Recorder: record.NewFakeRecorder(1000)}, c
}

func reconcile(t *testing.T, r *GracefulSetReconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "app"}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

func pods(t *testing.T, c client.Client) map[string]corev1.Pod {
	t.Helper()
	list := &corev1.PodList{}
	if err := c.List(context.Background(), list, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	m := map[string]corev1.Pod{}
	for _, p := range list.Items {
		m[p.Name] = p
	}
	return m
}

func getGS(t *testing.T, c client.Client) *appsv1alpha1.GracefulSet {
	t.Helper()
	gs := &appsv1alpha1.GracefulSet{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "app"}, gs); err != nil {
		t.Fatal(err)
	}
	return gs
}

func serving(p corev1.Pod) corev1.ConditionStatus {
	for _, c := range p.Status.Conditions {
		if c.Type == servingCondition {
			return c.Status
		}
	}
	return ""
}

func TestScaleUpCreatesPodsOnce(t *testing.T) {
	r, c := setup(t, newGS("v1", 3, appsv1alpha1.DrainPolicy{}))
	reconcile(t, r)
	reconcile(t, r) // a second pass must not create more pods

	got := pods(t, c)
	if len(got) != 3 {
		t.Fatalf("want 3 pods, got %d", len(got))
	}
	for _, p := range got {
		if !hasReadinessGate(&p) || serving(p) != corev1.ConditionTrue {
			t.Errorf("pod %s: want serving gate set True, got %q", p.Name, serving(p))
		}
		if p.Labels[versionLabel] != "v1" {
			t.Errorf("pod %s: version label %q", p.Name, p.Labels[versionLabel])
		}
	}
	gs := getGS(t, c)
	if gs.Status.Replicas != 3 {
		t.Errorf("status.replicas = %d, want 3", gs.Status.Replicas)
	}
	if !strings.Contains(gs.Status.Selector, drainingLabel+" notin (true)") {
		t.Errorf("status.selector %q should exclude draining pods", gs.Status.Selector)
	}
	if len(gs.Finalizers) != 1 || gs.Finalizers[0] != finalizerName {
		t.Errorf("finalizers = %v", gs.Finalizers)
	}
}

func TestSelectorMismatchCreatesNothing(t *testing.T) {
	gs := newGS("v1", 3, appsv1alpha1.DrainPolicy{})
	gs.Spec.Selector.MatchLabels = map[string]string{"app": "other"}
	r, c := setup(t, gs)
	reconcile(t, r)
	reconcile(t, r)

	if n := len(pods(t, c)); n != 0 {
		t.Fatalf("want no pods for mismatched selector, got %d", n)
	}
	cond := meta.FindStatusCondition(getGS(t, c).Status.Conditions, "Available")
	if cond == nil || cond.Reason != "InvalidSelector" {
		t.Fatalf("want Available condition with reason InvalidSelector, got %+v", cond)
	}
}

func TestUpgradeDrainsOldPodsAndTTLCountsFromDrainStart(t *testing.T) {
	policy := appsv1alpha1.DrainPolicy{Mode: appsv1alpha1.DrainPolicyTTL, TTL: &metav1.Duration{Duration: time.Hour}}
	r, c := setup(t, newGS("v2", 2, policy),
		newPod("old-a", "v1", corev1.PodRunning, 72*time.Hour),
		newPod("old-b", "v1", corev1.PodRunning, 72*time.Hour))
	reconcile(t, r)

	got := pods(t, c)
	if len(got) != 4 {
		t.Fatalf("want 2 old + 2 new pods, got %d", len(got))
	}
	if got["old-a"].Labels[drainingLabel] == "true" {
		t.Fatal("old pods must keep serving until new pods are available")
	}
	markReady(t, c, "v2")
	reconcile(t, r)
	got = pods(t, c)
	for _, name := range []string{"old-a", "old-b"} {
		p, ok := got[name]
		if !ok {
			t.Fatalf("%s was deleted even though it only just started draining", name)
		}
		if p.Labels[drainingLabel] != "true" || p.Annotations[drainingSinceAnnotation] == "" {
			t.Errorf("%s not marked draining: labels=%v annotations=%v", name, p.Labels, p.Annotations)
		}
		if p.Annotations[karpenterDoNotDisrupt] != "true" || p.Annotations[caSafeToEvict] != "false" {
			t.Errorf("%s missing disruption protection: %v", name, p.Annotations)
		}
		if serving(p) != corev1.ConditionFalse {
			t.Errorf("%s serving gate = %q, want False", name, serving(p))
		}
	}

	// Pretend old-a started draining two hours ago: the TTL has now passed for it only.
	p := got["old-a"]
	p.Annotations[drainingSinceAnnotation] = time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	if err := c.Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r)
	got = pods(t, c)
	if _, ok := got["old-a"]; ok {
		t.Error("old-a should be deleted after its TTL")
	}
	if _, ok := got["old-b"]; !ok {
		t.Error("old-b should still be draining")
	}
	if gs := getGS(t, c); gs.Status.DrainingPods != 1 || len(gs.Status.DrainingVersions) != 1 {
		t.Errorf("status draining = %d versions=%v", gs.Status.DrainingPods, gs.Status.DrainingVersions)
	}
}

// markReady sets the Ready condition on every pod of the given version, as the
// kubelet would.
func markReady(t *testing.T, c client.Client, version string) {
	t.Helper()
	for _, p := range pods(t, c) {
		if p.Labels[versionLabel] != version || isPodReady(&p) {
			continue
		}
		p.Status.Phase = corev1.PodRunning
		p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{
			Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Hour)),
		})
		if err := c.Status().Update(context.Background(), &p); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExitedActivePodIsReplaced(t *testing.T) {
	r, c := setup(t, newGS("v1", 1, appsv1alpha1.DrainPolicy{}), newPod("evicted", "v1", corev1.PodFailed, time.Hour))
	reconcile(t, r)
	got := pods(t, c)
	if _, ok := got["evicted"]; ok {
		t.Error("failed pod should be deleted")
	}
	if len(got) != 1 {
		t.Errorf("want 1 replacement pod, got %d", len(got))
	}
}

func TestScaleDownDrainsOldestFirst(t *testing.T) {
	r, c := setup(t, newGS("v1", 1, appsv1alpha1.DrainPolicy{}),
		newPod("old", "v1", corev1.PodRunning, 2*time.Hour),
		newPod("new", "v1", corev1.PodRunning, time.Hour))
	reconcile(t, r)
	got := pods(t, c)
	if got["old"].Labels[drainingLabel] != "true" || got["new"].Labels[drainingLabel] == "true" {
		t.Errorf("want only the oldest pod draining: old=%v new=%v", got["old"].Labels, got["new"].Labels)
	}
}

func TestMaxDrainingPodsDeletesLongestDraining(t *testing.T) {
	policy := appsv1alpha1.DrainPolicy{Mode: appsv1alpha1.DrainPolicyManual, MaxDrainingPods: ptr(int32(1))}
	var objs []client.Object
	objs = append(objs, newGS("v2", 0, policy))
	for i, hoursAgo := range []int{3, 2, 1} {
		p := newPod(fmt.Sprintf("d%d", i), "v1", corev1.PodRunning, 5*time.Hour)
		p.Labels[drainingLabel] = "true"
		p.Annotations[drainingSinceAnnotation] = time.Now().Add(-time.Duration(hoursAgo) * time.Hour).UTC().Format(time.RFC3339)
		objs = append(objs, p)
	}
	r, c := setup(t, objs...)
	reconcile(t, r)
	got := pods(t, c)
	if len(got) != 1 {
		t.Fatalf("want 1 pod left, got %d", len(got))
	}
	if _, ok := got["d2"]; !ok {
		t.Errorf("the most recently drained pod should be kept, got %v", got)
	}
}

func TestWaitForDrainPollsAndCallsHookOnce(t *testing.T) {
	var inflight, hookCalls atomic.Int32
	inflight.Store(5)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodPost && req.URL.Path == "/drain":
			hookCalls.Add(1)
		case req.URL.Path == "/drain-status":
			fmt.Fprintf(w, `{"inflight": %d}`, inflight.Load())
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	policy := appsv1alpha1.DrainPolicy{
		Mode:       appsv1alpha1.DrainPolicyWaitForDrain,
		DrainCheck: &appsv1alpha1.DrainCheck{Path: "/drain-status", Port: int32(port), PeriodSeconds: 10},
		DrainHook:  &appsv1alpha1.DrainHook{Path: "/drain", Port: int32(port)},
	}
	r, c := setup(t, newGS("v2", 0, policy), newPod("old", "v1", corev1.PodRunning, time.Hour))

	res := reconcile(t, r)
	if _, ok := pods(t, c)["old"]; !ok {
		t.Fatal("pod with in-flight work must not be deleted")
	}
	if res.RequeueAfter != 10*time.Second {
		t.Errorf("RequeueAfter = %s, want 10s", res.RequeueAfter)
	}
	reconcile(t, r)
	if n := hookCalls.Load(); n != 1 {
		t.Errorf("drain hook called %d times, want 1", n)
	}

	inflight.Store(0)
	reconcile(t, r)
	if _, ok := pods(t, c)["old"]; ok {
		t.Fatal("drained pod should be deleted")
	}
}

func TestDeletionWaitsForPodsToDrain(t *testing.T) {
	gs := newGS("v1", 1, appsv1alpha1.DrainPolicy{})
	gs.Finalizers = []string{finalizerName}
	r, c := setup(t, gs, newPod("p", "v1", corev1.PodRunning, time.Hour))
	if err := c.Delete(context.Background(), getGS(t, c)); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r)

	p, ok := pods(t, c)["p"]
	if !ok || p.Labels[drainingLabel] != "true" {
		t.Fatalf("pod should still exist and be draining while the GracefulSet is deleted: %+v", p.Labels)
	}
	if len(pods(t, c)) != 1 {
		t.Fatal("no new pods should be created while deleting")
	}

	p.Status.Phase = corev1.PodSucceeded
	if err := c.Status().Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r)
	if len(pods(t, c)) != 0 {
		t.Fatal("finished pod should be removed")
	}
	err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "app"}, &appsv1alpha1.GracefulSet{})
	if err == nil {
		t.Fatal("GracefulSet should be gone once the finalizer is removed")
	}
}

func TestEmptyVersionUsesTemplateHash(t *testing.T) {
	gs := newGS("", 1, appsv1alpha1.DrainPolicy{})
	v1 := desiredVersion(gs)
	gs.Spec.Template.Spec.Containers[0].Image = "img:other"
	if v1 == "" || v1 == desiredVersion(gs) {
		t.Fatalf("template change must change the derived version (%q)", v1)
	}
	gs.Spec.Version = "explicit"
	if desiredVersion(gs) != "explicit" {
		t.Fatal("spec.version must win over the hash")
	}
}
