package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	appsv1alpha1 "github.com/gracefulset-io/gracefulset/api/v1alpha1"
)

// TestAgainstAPIServer runs the reconciler against a real kube-apiserver with the
// generated CRD, to catch what the fake client cannot: CRD defaulting, status
// schema validation, the scale subresource and strategic-merge pod status patches.
// There is no kubelet or garbage collector, so pods stay Pending.
// Needs KUBEBUILDER_ASSETS (see `make test-envtest`); skipped otherwise.
func TestAgainstAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "charts", "gracefulset-controller", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = env.Stop() }()

	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = appsv1alpha1.AddToScheme(s)
	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := &GracefulSetReconciler{Client: c, APIReader: c, Scheme: s, Recorder: record.NewFakeRecorder(1000)}
	key := types.NamespacedName{Namespace: ns, Name: "app"}
	run := func() {
		t.Helper()
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	list := func() []corev1.Pod {
		t.Helper()
		l := &corev1.PodList{}
		if err := c.List(ctx, l, client.InNamespace(ns)); err != nil {
			t.Fatal(err)
		}
		return l.Items
	}

	// drainPolicy left out entirely: CRD defaults must fill it in.
	gs := newGS("v1", 2, appsv1alpha1.DrainPolicy{})
	if err := c.Create(ctx, gs); err != nil {
		t.Fatal(err)
	}
	if gs.Spec.DrainPolicy.Mode != appsv1alpha1.DrainPolicyWaitForCompletion ||
		gs.Spec.DrainPolicy.RemoveFromEndpoints == nil || !*gs.Spec.DrainPolicy.RemoveFromEndpoints ||
		gs.Spec.DrainPolicy.ProtectFromDisruption == nil || !*gs.Spec.DrainPolicy.ProtectFromDisruption {
		t.Fatalf("CRD defaults not applied: %+v", gs.Spec.DrainPolicy)
	}

	run()
	run()
	got := list()
	if len(got) != 2 {
		t.Fatalf("want 2 pods, got %d", len(got))
	}
	for _, p := range got {
		if serving(p) != corev1.ConditionTrue || !hasReadinessGate(&p) {
			t.Errorf("pod %s: serving gate not set True", p.Name)
		}
		if len(p.OwnerReferences) != 1 {
			t.Errorf("pod %s: owner references %v", p.Name, p.OwnerReferences)
		}
	}

	if err := c.Get(ctx, key, gs); err != nil {
		t.Fatal(err)
	}
	if gs.Status.Replicas != 2 || gs.Status.ObservedGeneration != gs.Generation {
		t.Fatalf("status not written: %+v", gs.Status)
	}
	scale := &autoscalingv1.Scale{}
	if err := c.SubResource("scale").Get(ctx, gs, scale); err != nil {
		t.Fatal(err)
	}
	if scale.Status.Replicas != 2 || scale.Status.Selector == "" {
		t.Errorf("scale subresource = %+v", scale.Status)
	}

	// Upgrade: old pods are kept and marked draining with the gate set False.
	gs.Spec.Version = "v2"
	if err := c.Update(ctx, gs); err != nil {
		t.Fatal(err)
	}
	run()
	if len(list()) != 4 {
		t.Fatalf("want 2 old + 2 new pods, got %d", len(list()))
	}
	markReady(t, c, "v2")
	run()
	got = list()
	if len(got) != 4 {
		t.Fatalf("want 2 draining + 2 new pods, got %d", len(got))
	}
	for _, p := range got {
		if p.Labels[versionLabel] == "v1" && (p.Labels[drainingLabel] != "true" || serving(p) != corev1.ConditionFalse) {
			t.Errorf("old pod %s not draining: labels=%v serving=%q", p.Name, p.Labels, serving(p))
		}
	}

	// Changing the template without changing the version must not wedge the
	// controller on the immutable ControllerRevision data.
	if err := c.Get(ctx, key, gs); err != nil {
		t.Fatal(err)
	}
	gs.Spec.Template.Spec.Containers[0].Image = "img:v2-fixed"
	if err := c.Update(ctx, gs); err != nil {
		t.Fatal(err)
	}
	run()

	// Delete: the finalizer keeps the GracefulSet while pods drain.
	if err := c.Delete(ctx, gs); err != nil {
		t.Fatal(err)
	}
	run()
	if err := c.Get(ctx, key, gs); err != nil {
		t.Fatalf("GracefulSet should still exist while pods drain: %v", err)
	}
	for _, p := range list() {
		if p.Labels[drainingLabel] != "true" {
			t.Errorf("pod %s not draining during deletion", p.Name)
		}
		// Unscheduled pods are removed immediately by the apiserver.
		if err := c.Delete(ctx, &p); err != nil {
			t.Fatal(err)
		}
	}
	run()
	if err := c.Get(ctx, key, gs); err == nil {
		t.Fatal("GracefulSet should be gone after its pods are gone")
	}
}
