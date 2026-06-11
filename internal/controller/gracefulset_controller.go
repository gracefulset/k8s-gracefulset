package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1alpha1 "github.com/gracefulset-io/gracefulset/api/v1alpha1"
)

const (
	versionLabel = "gracefulset.io/version"
	ownerLabel   = "gracefulset.io/name"
	finalizerName = "gracefulset.io/finalizer"
	// drainingLabel marks a pod as draining. Once set, the pod is excluded from
	// the active replica count and handled by the drain policy. Used both for
	// version upgrades and scale-down.
	drainingLabel = "gracefulset.io/draining"
)

// GracefulSetReconciler reconciles a GracefulSet object
type GracefulSetReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=apps.gracefulset.io,resources=gracefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.gracefulset.io,resources=gracefulsets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.gracefulset.io,resources=gracefulsets/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete

func (r *GracefulSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the GracefulSet instance
	gracefulSet := &appsv1alpha1.GracefulSet{}
	if err := r.Get(ctx, req.NamespacedName, gracefulSet); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// List all pods owned by this GracefulSet
	podList := &corev1.PodList{}
	labelSelector, err := metav1.LabelSelectorAsSelector(gracefulSet.Spec.Selector)
	if err != nil {
		return ctrl.Result{}, err
	}
	listOpts := &client.ListOptions{
		LabelSelector: labelSelector,
		Namespace:     gracefulSet.Namespace,
	}
	if err := r.List(ctx, podList, listOpts); err != nil {
		return ctrl.Result{}, err
	}

	// Filter pods owned by this GracefulSet
	ownedPods := filterOwnedPods(podList.Items, gracefulSet)

	// Separate current version pods from draining pods.
	// A pod is "active" only if it matches the current version AND is not
	// explicitly marked draining (e.g. from a previous scale-down).
	currentVersion := gracefulSet.Spec.Version
	var currentPods, drainingPods []corev1.Pod
	for _, pod := range ownedPods {
		isDraining := pod.Labels[drainingLabel] == "true"
		if !isDraining && pod.Labels[versionLabel] == currentVersion {
			currentPods = append(currentPods, pod)
		} else {
			drainingPods = append(drainingPods, pod)
		}
	}

	// Determine desired replicas
	desiredReplicas := int32(1)
	if gracefulSet.Spec.Replicas != nil {
		desiredReplicas = *gracefulSet.Spec.Replicas
	}

	currentReady := countReadyPods(currentPods)

	if int32(len(currentPods)) < desiredReplicas {
		// Scale UP: create new pods for current version
		toCreate := desiredReplicas - int32(len(currentPods))
		log.Info("scaling up current version", "version", currentVersion, "creating", toCreate)
		for i := int32(0); i < toCreate; i++ {
			if err := r.createPod(ctx, gracefulSet, currentVersion); err != nil {
				return ctrl.Result{}, err
			}
		}
	} else if int32(len(currentPods)) > desiredReplicas {
		// Scale DOWN: mark excess current-version pods as draining instead of
		// deleting them. The drain policy lets the application finish and exit
		// with its own exit code; the controller only cleans up after it exits.
		toDrain := int32(len(currentPods)) - desiredReplicas
		log.Info("scaling down current version", "version", currentVersion, "draining", toDrain)
		// Drain the oldest pods first so the newest (warmest) stay serving.
		sortPodsByCreationTimestamp(currentPods)
		for i := int32(0); i < toDrain; i++ {
			if err := r.markPodDraining(ctx, &currentPods[i]); err != nil {
				return ctrl.Result{}, err
			}
			// Move it into the draining set for status/handling this cycle
			drainingPods = append(drainingPods, currentPods[i])
		}
		// Recompute active set after marking
		currentPods = currentPods[toDrain:]
		currentReady = countReadyPods(currentPods)
	}

	// Handle draining pods based on drain policy
	requeueAfter := r.handleDrainingPods(ctx, gracefulSet, drainingPods)

	// Update status
	gracefulSet.Status.ActiveVersion = currentVersion
	gracefulSet.Status.ReadyReplicas = currentReady
	gracefulSet.Status.TotalPods = int32(len(ownedPods))
	gracefulSet.Status.DrainingPods = int32(len(drainingPods))
	gracefulSet.Status.DrainingVersions = buildDrainingVersionStatus(drainingPods)
	gracefulSet.Status.Selector = labelSelector.String()

	// Set conditions
	r.setConditions(gracefulSet, currentReady, desiredReplicas, len(drainingPods))

	if err := r.Status().Update(ctx, gracefulSet); err != nil {
		return ctrl.Result{}, err
	}

	if requeueAfter > 0 {
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}
	return ctrl.Result{}, nil
}

func (r *GracefulSetReconciler) createPod(ctx context.Context, gs *appsv1alpha1.GracefulSet, version string) error {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: gs.Name + "-",
			Namespace:    gs.Namespace,
			Labels:       make(map[string]string),
		},
		Spec: gs.Spec.Template.Spec,
	}

	// Copy labels from template
	for k, v := range gs.Spec.Template.Labels {
		pod.Labels[k] = v
	}
	// Add version and owner labels
	pod.Labels[versionLabel] = version
	pod.Labels[ownerLabel] = gs.Name

	// Copy annotations from template
	if gs.Spec.Template.Annotations != nil {
		pod.Annotations = make(map[string]string)
		for k, v := range gs.Spec.Template.Annotations {
			pod.Annotations[k] = v
		}
	}

	// Set owner reference
	if err := controllerutil.SetControllerReference(gs, pod, r.Scheme); err != nil {
		return err
	}

	return r.Create(ctx, pod)
}

// markPodDraining adds the draining label to a pod so it is excluded from the
// active replica count. The pod keeps running; the drain policy decides when
// (or if) it is removed once the application exits.
func (r *GracefulSetReconciler) markPodDraining(ctx context.Context, pod *corev1.Pod) error {
	if pod.Labels[drainingLabel] == "true" {
		return nil // already marked
	}
	patched := pod.DeepCopy()
	if patched.Labels == nil {
		patched.Labels = make(map[string]string)
	}
	patched.Labels[drainingLabel] = "true"
	if patched.Annotations == nil {
		patched.Annotations = make(map[string]string)
	}
	patched.Annotations["gracefulset.io/draining-since"] = metav1.Now().Format(time.RFC3339)
	return r.Patch(ctx, patched, client.MergeFrom(pod))
}

// sortPodsByCreationTimestamp sorts pods oldest-first.
func sortPodsByCreationTimestamp(pods []corev1.Pod) {
	sort.Slice(pods, func(i, j int) bool {
		return pods[i].CreationTimestamp.Before(&pods[j].CreationTimestamp)
	})
}

func defaultDrainCheck() *appsv1alpha1.DrainCheck {
	return &appsv1alpha1.DrainCheck{
		Path:          "/drain-status",
		Port:          8080,
		Scheme:        "HTTP",
		PeriodSeconds: 30,
		JSONField:     "inflight",
	}
}

// checkPodDrained polls the pod's drain endpoint and returns true when the
// configured JSON field reports zero in-flight work. A pod with no IP yet, or
// that is not running, is not considered drained.
func (r *GracefulSetReconciler) checkPodDrained(pod *corev1.Pod, dc *appsv1alpha1.DrainCheck) (bool, error) {
	if pod.Status.PodIP == "" {
		return false, fmt.Errorf("pod has no IP yet")
	}

	scheme := dc.Scheme
	if scheme == "" {
		scheme = "HTTP"
	}
	path := dc.Path
	if path == "" {
		path = "/drain-status"
	}
	port := dc.Port
	if port == 0 {
		port = 8080
	}
	field := dc.JSONField
	if field == "" {
		field = "inflight"
	}

	url := fmt.Sprintf("%s://%s:%d%s", strings.ToLower(scheme), pod.Status.PodIP, port, path)
	httpClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpClient.Get(url)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("drain endpoint returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}

	var data map[string]interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		return false, fmt.Errorf("failed to parse drain response: %w", err)
	}

	val, ok := data[field]
	if !ok {
		return false, fmt.Errorf("drain response missing field %q", field)
	}

	// Treat numeric zero as drained
	switch v := val.(type) {
	case float64:
		return v == 0, nil
	case int:
		return v == 0, nil
	case bool:
		// {"draining": true} style — true means drained/done
		return v, nil
	default:
		return false, fmt.Errorf("unexpected type for field %q", field)
	}
}

func (r *GracefulSetReconciler) handleDrainingPods(ctx context.Context, gs *appsv1alpha1.GracefulSet, drainingPods []corev1.Pod) time.Duration {
	log := log.FromContext(ctx)
	var requeueAfter time.Duration

	for i := range drainingPods {
		pod := &drainingPods[i]

		// If pod has completed (Succeeded or Failed), clean it up
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			log.Info("cleaning up completed draining pod", "pod", pod.Name, "phase", pod.Status.Phase)
			r.Delete(ctx, pod)
			continue
		}

		switch gs.Spec.DrainPolicy.Mode {
		case appsv1alpha1.DrainPolicyTTL:
			if gs.Spec.DrainPolicy.TTL != nil {
				podAge := time.Since(pod.CreationTimestamp.Time)
				ttl := gs.Spec.DrainPolicy.TTL.Duration
				if podAge >= ttl {
					log.Info("TTL expired, deleting draining pod", "pod", pod.Name, "age", podAge)
					r.Delete(ctx, pod)
				} else {
					remaining := ttl - podAge
					if requeueAfter == 0 || remaining < requeueAfter {
						requeueAfter = remaining
					}
				}
			}
		case appsv1alpha1.DrainPolicyWaitForCompletion:
			// Do nothing — just wait for the pod to exit naturally
			// Requeue periodically to update status
			if requeueAfter == 0 {
				requeueAfter = 60 * time.Second
			}
		case appsv1alpha1.DrainPolicyWaitForDrain:
			// Poll the pod's drain endpoint. Remove it only when the app reports
			// zero in-flight work. A TTL (if set) acts as a safety cap.
			dc := gs.Spec.DrainPolicy.DrainCheck
			if dc == nil {
				dc = defaultDrainCheck()
			}

			// Safety cap via TTL
			if gs.Spec.DrainPolicy.TTL != nil {
				podAge := time.Since(pod.CreationTimestamp.Time)
				if podAge >= gs.Spec.DrainPolicy.TTL.Duration {
					log.Info("drain TTL cap reached, force-deleting pod", "pod", pod.Name, "age", podAge)
					r.Delete(ctx, pod)
					continue
				}
			}

			drained, err := r.checkPodDrained(pod, dc)
			if err != nil {
				log.Info("drain check failed, will retry", "pod", pod.Name, "error", err.Error())
			} else if drained {
				log.Info("pod reports drained, deleting", "pod", pod.Name)
				r.Delete(ctx, pod)
				continue
			}
			period := time.Duration(dc.PeriodSeconds) * time.Second
			if period <= 0 {
				period = 30 * time.Second
			}
			if requeueAfter == 0 || period < requeueAfter {
				requeueAfter = period
			}
		case appsv1alpha1.DrainPolicyManual:
			// Do nothing — operator must manually delete pods
		}
	}

	return requeueAfter
}

func (r *GracefulSetReconciler) setConditions(gs *appsv1alpha1.GracefulSet, ready, desired int32, draining int) {
	now := metav1.Now()

	// Available condition
	availableCondition := metav1.Condition{
		Type:               "Available",
		LastTransitionTime: now,
	}
	if ready >= desired {
		availableCondition.Status = metav1.ConditionTrue
		availableCondition.Reason = "ReplicasAvailable"
		availableCondition.Message = fmt.Sprintf("%d/%d replicas ready", ready, desired)
	} else {
		availableCondition.Status = metav1.ConditionFalse
		availableCondition.Reason = "ReplicasUnavailable"
		availableCondition.Message = fmt.Sprintf("%d/%d replicas ready", ready, desired)
	}

	// Draining condition
	drainingCondition := metav1.Condition{
		Type:               "Draining",
		LastTransitionTime: now,
	}
	if draining > 0 {
		drainingCondition.Status = metav1.ConditionTrue
		drainingCondition.Reason = "PodsDraining"
		drainingCondition.Message = fmt.Sprintf("%d pods from old versions still running", draining)
	} else {
		drainingCondition.Status = metav1.ConditionFalse
		drainingCondition.Reason = "NoPodsDraining"
		drainingCondition.Message = "All pods running current version"
	}

	gs.Status.Conditions = []metav1.Condition{availableCondition, drainingCondition}
}

func filterOwnedPods(pods []corev1.Pod, gs *appsv1alpha1.GracefulSet) []corev1.Pod {
	var owned []corev1.Pod
	for _, pod := range pods {
		if pod.Labels[ownerLabel] == gs.Name {
			owned = append(owned, pod)
		}
	}
	return owned
}

func countReadyPods(pods []corev1.Pod) int32 {
	var count int32
	for _, pod := range pods {
		if isPodReady(&pod) {
			count++
		}
	}
	return count
}

func isPodReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func buildDrainingVersionStatus(pods []corev1.Pod) []appsv1alpha1.VersionStatus {
	versionMap := make(map[string]*appsv1alpha1.VersionStatus)
	for _, pod := range pods {
		version := pod.Labels[versionLabel]
		if _, exists := versionMap[version]; !exists {
			versionMap[version] = &appsv1alpha1.VersionStatus{
				Version: version,
			}
		}
		vs := versionMap[version]
		vs.Pods++
		if isPodReady(&pod) {
			vs.ReadyPods++
		}
		creation := pod.CreationTimestamp
		if vs.OldestPodCreation == nil || creation.Before(vs.OldestPodCreation) {
			vs.OldestPodCreation = &creation
		}
	}

	var result []appsv1alpha1.VersionStatus
	for _, vs := range versionMap {
		result = append(result, *vs)
	}
	return result
}

// SetupWithManager sets up the controller with the Manager
func (r *GracefulSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.GracefulSet{}).
		Owns(&corev1.Pod{}).
		Complete(r)
}
