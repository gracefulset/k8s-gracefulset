package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1alpha1 "github.com/infor/gracefulset/api/v1alpha1"
)

const (
	versionLabel = "gracefulset.infor.com/version"
	ownerLabel   = "gracefulset.infor.com/name"
	finalizerName = "gracefulset.infor.com/finalizer"
)

// GracefulSetReconciler reconciles a GracefulSet object
type GracefulSetReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=apps.infor.com,resources=gracefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.infor.com,resources=gracefulsets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.infor.com,resources=gracefulsets/finalizers,verbs=update
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

	// Separate current version pods from draining pods
	currentVersion := gracefulSet.Spec.Version
	var currentPods, drainingPods []corev1.Pod
	for _, pod := range ownedPods {
		if pod.Labels[versionLabel] == currentVersion {
			currentPods = append(currentPods, pod)
		} else {
			drainingPods = append(drainingPods, pod)
		}
	}

	// Scale UP current version to desired replicas
	desiredReplicas := int32(1)
	if gracefulSet.Spec.Replicas != nil {
		desiredReplicas = *gracefulSet.Spec.Replicas
	}

	currentReady := countReadyPods(currentPods)
	if int32(len(currentPods)) < desiredReplicas {
		// Create new pods for current version
		toCreate := desiredReplicas - int32(len(currentPods))
		log.Info("scaling up current version", "version", currentVersion, "creating", toCreate)
		for i := int32(0); i < toCreate; i++ {
			if err := r.createPod(ctx, gracefulSet, currentVersion); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	// Handle draining pods based on drain policy
	requeueAfter := r.handleDrainingPods(ctx, gracefulSet, drainingPods)

	// Update status
	gracefulSet.Status.ActiveVersion = currentVersion
	gracefulSet.Status.ReadyReplicas = currentReady
	gracefulSet.Status.TotalPods = int32(len(ownedPods))
	gracefulSet.Status.DrainingPods = int32(len(drainingPods))
	gracefulSet.Status.DrainingVersions = buildDrainingVersionStatus(drainingPods)

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
