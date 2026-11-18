package controller

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1alpha1 "github.com/gracefulset-io/gracefulset/api/v1alpha1"
)

func readyPod(name, version string, age time.Duration) *corev1.Pod {
	p := newPod(name, version, corev1.PodRunning, age)
	p.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(time.Now().Add(-age))},
		{Type: servingCondition, Status: corev1.ConditionTrue},
	}
	return p
}

func countBy(got map[string]corev1.Pod, version string, draining bool) int {
	n := 0
	for _, p := range got {
		if p.Labels[versionLabel] == version && (p.Labels[drainingLabel] == "true") == draining {
			n++
		}
	}
	return n
}

func TestMaxSurgeRollsOnePodAtATime(t *testing.T) {
	gs := newGS("v2", 3, appsv1alpha1.DrainPolicy{Mode: appsv1alpha1.DrainPolicyManual})
	gs.Spec.MaxSurge = &intstr.IntOrString{Type: intstr.Int, IntVal: 1}
	r, c := setup(t, gs, readyPod("a", "v1", 3*time.Hour), readyPod("b", "v1", 2*time.Hour), readyPod("c", "v1", time.Hour))

	for step := 1; step <= 3; step++ {
		reconcile(t, r)
		got := pods(t, c)
		if n := countBy(got, "v2", false); n != step {
			t.Fatalf("step %d: want %d new pods, got %d", step, step, n)
		}
		if n := countBy(got, "v1", true); n != step-1 {
			t.Fatalf("step %d: want %d old pods draining, got %d", step, step-1, n)
		}
		markReady(t, c, "v2")
	}
	reconcile(t, r)
	got := pods(t, c)
	if countBy(got, "v1", true) != 3 || countBy(got, "v2", false) != 3 {
		t.Fatalf("rollout should finish with 3 new and 3 draining: %v", got)
	}
	if got["a"].Labels[drainingLabel] != "true" {
		t.Error("oldest pod should have been drained")
	}
	cond := meta.FindStatusCondition(getGS(t, c).Status.Conditions, "Progressing")
	if cond == nil || cond.Reason != "RolloutComplete" {
		t.Errorf("Progressing = %+v, want RolloutComplete", cond)
	}
}

func TestMinReadySecondsDelaysDrainingOldPods(t *testing.T) {
	gs := newGS("v2", 1, appsv1alpha1.DrainPolicy{})
	gs.Spec.MinReadySeconds = 60
	newer := readyPod("new", "v2", 10*time.Second) // Ready for 10s only
	r, c := setup(t, gs, readyPod("old", "v1", time.Hour), newer)
	res := reconcile(t, r)
	if pods(t, c)["old"].Labels[drainingLabel] == "true" {
		t.Fatal("old pod drained before the new one was available for minReadySeconds")
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > 50*time.Second {
		t.Errorf("RequeueAfter = %s, want about 50s", res.RequeueAfter)
	}
}

func TestPausedDoesNothing(t *testing.T) {
	gs := newGS("v2", 2, appsv1alpha1.DrainPolicy{})
	gs.Spec.Paused = true
	r, c := setup(t, gs, readyPod("old", "v1", time.Hour))
	reconcile(t, r)
	got := pods(t, c)
	if len(got) != 1 || got["old"].Labels[drainingLabel] == "true" {
		t.Fatalf("paused GracefulSet must not create or drain pods: %v", got)
	}
	if cond := meta.FindStatusCondition(getGS(t, c).Status.Conditions, "Progressing"); cond == nil || cond.Reason != "Paused" {
		t.Errorf("Progressing = %+v, want Paused", cond)
	}
}

func TestProgressDeadlineExceeded(t *testing.T) {
	gs := newGS("v1", 1, appsv1alpha1.DrainPolicy{})
	gs.Spec.ProgressDeadlineSeconds = ptr(int32(60))
	gs.Status.ActiveVersion = "v1"
	gs.Status.RolloutStartTime = &metav1.Time{Time: time.Now().Add(-time.Hour)}
	r, c := setup(t, gs, newPod("p", "v1", corev1.PodPending, time.Hour))
	reconcile(t, r)
	cond := meta.FindStatusCondition(getGS(t, c).Status.Conditions, "Progressing")
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "ProgressDeadlineExceeded" {
		t.Fatalf("Progressing = %+v, want False/ProgressDeadlineExceeded", cond)
	}
}

func TestScaleDownHonoursDeletionCostAndReadiness(t *testing.T) {
	cheap := readyPod("newest-cheap", "v1", time.Minute)
	cheap.Annotations[deletionCostAnn] = "-10"
	unready := newPod("unready", "v1", corev1.PodRunning, time.Hour)
	r, c := setup(t, newGS("v1", 1, appsv1alpha1.DrainPolicy{}), readyPod("oldest", "v1", 2*time.Hour), cheap, unready)
	reconcile(t, r)
	got := pods(t, c)
	if got["newest-cheap"].Labels[drainingLabel] != "true" || got["unready"].Labels[drainingLabel] != "true" {
		t.Fatalf("lowest-cost and not-Ready pods should drain first: %v", got)
	}
	if got["oldest"].Labels[drainingLabel] == "true" {
		t.Fatal("oldest pod should keep serving")
	}
}

// fakePodTransport answers HTTP drain checks per pod IP without a network.
type fakePodTransport map[string]string

func (f fakePodTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	host, _, _ := net.SplitHostPort(req.URL.Host)
	body, ok := f[host]
	if !ok {
		return nil, fmt.Errorf("unreachable")
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

func TestScaleDownDrainsLeastBusyFirstAndNestedThreshold(t *testing.T) {
	busy, idle := readyPod("busy-old", "v1", 2*time.Hour), readyPod("idle-new", "v1", time.Hour)
	busy.Status.PodIP, idle.Status.PodIP = "10.0.0.1", "10.0.0.2"
	policy := appsv1alpha1.DrainPolicy{
		Mode:       appsv1alpha1.DrainPolicyWaitForDrain,
		DrainCheck: &appsv1alpha1.DrainCheck{JSONField: "stats.sessions", Threshold: 2, PeriodSeconds: 5, Port: 8080},
	}
	r, c := setup(t, newGS("v1", 1, policy), busy, idle)
	transport := fakePodTransport{"10.0.0.1": `{"stats":{"sessions":9}}`, "10.0.0.2": `{"stats":{"sessions":3}}`}
	r.HTTPClient = &http.Client{Transport: transport}

	reconcile(t, r)
	got := pods(t, c)
	if got["idle-new"].Labels[drainingLabel] != "true" || got["busy-old"].Labels[drainingLabel] == "true" {
		t.Fatalf("the pod with fewer sessions should drain, even though it is newer: %v", got)
	}
	transport["10.0.0.2"] = `{"stats":{"sessions":2}}` // at threshold
	reconcile(t, r)
	if _, ok := pods(t, c)["idle-new"]; ok {
		t.Fatal("pod at or below threshold should be removed")
	}
}

func TestTCPDrainCheck(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := int32(ln.Addr().(*net.TCPAddr).Port)
	pod := newPod("p", "v1", corev1.PodRunning, time.Hour)
	if drained, err := checkTCP(context.Background(), pod, port); err != nil || drained {
		t.Fatalf("listening port: drained=%v err=%v, want not drained", drained, err)
	}
	ln.Close()
	if drained, err := checkTCP(context.Background(), pod, port); err != nil || !drained {
		t.Fatalf("closed port: drained=%v err=%v, want drained", drained, err)
	}
}

func TestScaleUpReusesDrainingPods(t *testing.T) {
	warm := readyPod("warm", "v1", time.Hour)
	warm.Labels[drainingLabel] = "true"
	warm.Annotations[drainingSinceAnnotation] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	warm.Annotations[karpenterDoNotDisrupt] = "true"
	hooked := readyPod("hooked", "v1", time.Hour)
	hooked.Labels[drainingLabel] = "true"
	hooked.Annotations[drainHookAnnotation] = "x"
	r, c := setup(t, newGS("v1", 2, appsv1alpha1.DrainPolicy{}), warm, hooked)
	reconcile(t, r)

	got := pods(t, c)
	if got["warm"].Labels[drainingLabel] == "true" || got["warm"].Annotations[karpenterDoNotDisrupt] != "" {
		t.Errorf("warm pod should be back in service: %v %v", got["warm"].Labels, got["warm"].Annotations)
	}
	if serving(got["warm"]) != corev1.ConditionTrue {
		t.Error("reused pod should be serving")
	}
	if got["hooked"].Labels[drainingLabel] != "true" {
		t.Error("a pod whose drain hook was called must not be reused")
	}
	if len(got) != 3 {
		t.Errorf("want warm + hooked + 1 new pod, got %d", len(got))
	}
}

func revisions(t *testing.T, c client.Client) []appsv1.ControllerRevision {
	t.Helper()
	l := &appsv1.ControllerRevisionList{}
	if err := c.List(context.Background(), l, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	return l.Items
}

func TestRevisionsAndRollback(t *testing.T) {
	gs := newGS("v1", 1, appsv1alpha1.DrainPolicy{})
	gs.Spec.RevisionHistoryLimit = ptr(int32(1))
	r, c := setup(t, gs)
	reconcile(t, r)

	for _, v := range []string{"v2", "v3"} {
		cur := getGS(t, c)
		cur.Spec.Version = v
		cur.Spec.Template.Spec.Containers[0].Image = "img:" + v
		if err := c.Update(context.Background(), cur); err != nil {
			t.Fatal(err)
		}
		reconcile(t, r)
	}
	if n := len(revisions(t, c)); n != 2 {
		t.Fatalf("want current + 1 old revision, got %d", n)
	}

	cur := getGS(t, c)
	cur.Annotations = map[string]string{rollbackAnnotation: "previous"}
	if err := c.Update(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r)
	cur = getGS(t, c)
	if cur.Spec.Version != "v2" || cur.Spec.Template.Spec.Containers[0].Image != "img:v2" {
		t.Fatalf("rollback did not restore v2: version=%s image=%s", cur.Spec.Version, cur.Spec.Template.Spec.Containers[0].Image)
	}
	if _, ok := cur.Annotations[rollbackAnnotation]; ok {
		t.Error("rollback annotation should be removed")
	}

	cur.Annotations = map[string]string{rollbackAnnotation: "v9"}
	if err := c.Update(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r)
	if cur = getGS(t, c); cur.Spec.Version != "v2" || cur.Annotations[rollbackAnnotation] != "" {
		t.Fatal("rollback to an unknown version must change nothing but clear the annotation")
	}
}

func TestDeletionPolicyDelete(t *testing.T) {
	gs := newGS("v1", 1, appsv1alpha1.DrainPolicy{})
	gs.Finalizers = []string{finalizerName}
	gs.Spec.DeletionPolicy = appsv1alpha1.DeletionPolicyDelete
	r, c := setup(t, gs, readyPod("p", "v1", time.Hour))
	if err := c.Delete(context.Background(), getGS(t, c)); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "app"}, &appsv1alpha1.GracefulSet{}); err == nil {
		t.Fatal("deletionPolicy Delete should drop the finalizer immediately")
	}
	if pods(t, c)["p"].Labels[drainingLabel] == "true" {
		t.Fatal("pods should not be drained with deletionPolicy Delete")
	}
}

func TestCrashLoopingDrainingPodIsRemoved(t *testing.T) {
	crashing := newPod("crashing", "v1", corev1.PodRunning, time.Hour)
	crashing.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "c", State: corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}
	running := newPod("running", "v1", corev1.PodRunning, time.Hour)
	running.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "c", State: corev1.ContainerState{
		Running: &corev1.ContainerStateRunning{}}}}
	policy := appsv1alpha1.DrainPolicy{Mode: appsv1alpha1.DrainPolicyTTL, TTL: &metav1.Duration{Duration: time.Hour}}
	r, c := setup(t, newGS("v2", 0, policy), crashing, running)
	reconcile(t, r)
	got := pods(t, c)
	if _, ok := got["crashing"]; ok {
		t.Error("draining pod with no running containers should be removed")
	}
	if _, ok := got["running"]; !ok {
		t.Error("draining pod with a running container must wait for its TTL")
	}
}

func TestLostPodAfterRolloutIsNotADeadlineFailure(t *testing.T) {
	gs := newGS("v1", 2, appsv1alpha1.DrainPolicy{})
	gs.Spec.ProgressDeadlineSeconds = ptr(int32(60))
	gs.Status.ActiveVersion = "v1"
	gs.Status.RolloutStartTime = &metav1.Time{Time: time.Now().Add(-time.Hour)}
	gs.Status.Conditions = []metav1.Condition{{Type: "Progressing", Status: metav1.ConditionTrue, Reason: "RolloutComplete", LastTransitionTime: metav1.Now()}}
	// One of the two pods was evicted; its replacement is not Ready yet.
	r, c := setup(t, gs, readyPod("a", "v1", time.Hour))
	reconcile(t, r)
	st := getGS(t, c).Status
	if p := meta.FindStatusCondition(st.Conditions, "Progressing"); p == nil || p.Reason != "RolloutComplete" {
		t.Errorf("Progressing = %+v, want RolloutComplete", p)
	}
	if a := meta.FindStatusCondition(st.Conditions, "Available"); a == nil || a.Status != metav1.ConditionFalse {
		t.Errorf("Available = %+v, want False", a)
	}
}
