package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"sort"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1alpha1 "github.com/gracefulset-io/gracefulset/api/v1alpha1"
)

const (
	versionLabel  = "gracefulset.io/version"
	ownerLabel    = "gracefulset.io/name"
	finalizerName = "gracefulset.io/finalizer"
	// drainingLabel marks a pod as draining. Once set, the pod is excluded from
	// the serving set and handled by the drain policy. Used for version
	// upgrades, scale-down and GracefulSet deletion.
	drainingLabel = "gracefulset.io/draining"

	drainingSinceAnnotation = "gracefulset.io/draining-since"
	drainHookAnnotation     = "gracefulset.io/drain-hook-called"
	// rollbackAnnotation on a GracefulSet asks the controller to restore the
	// version and template of a stored revision ("previous" or a version).
	rollbackAnnotation = "gracefulset.io/rollback-to"
	deletionCostAnn    = "controller.kubernetes.io/pod-deletion-cost"

	// servingCondition is the readiness gate the controller adds to pods when
	// removeFromEndpoints is enabled. False takes a draining pod out of Services.
	servingCondition corev1.PodConditionType = "gracefulset.io/serving"

	karpenterDoNotDisrupt = "karpenter.sh/do-not-disrupt"
	caSafeToEvict         = "cluster-autoscaler.kubernetes.io/safe-to-evict"

	hookRetryPeriod = 30 * time.Second
)

// GracefulSetReconciler reconciles a GracefulSet object
type GracefulSetReconciler struct {
	client.Client
	// APIReader lists pods without the informer cache, so pods created by the
	// previous reconcile are always seen and never created twice.
	// ponytail: one uncached LIST per reconcile; switch to ReplicaSet-style
	// expectations if apiserver load from many GracefulSets becomes a problem.
	APIReader               client.Reader
	Scheme                  *runtime.Scheme
	Recorder                record.EventRecorder
	HTTPClient              *http.Client
	MaxConcurrentReconciles int
	// RestConfig and Clientset are only needed for Exec drain checks.
	RestConfig *rest.Config
	Clientset  kubernetes.Interface
}

// +kubebuilder:rbac:groups=apps.gracefulset.io,resources=gracefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.gracefulset.io,resources=gracefulsets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.gracefulset.io,resources=gracefulsets/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete;patch
// +kubebuilder:rbac:groups="",resources=pods/status,verbs=patch
// +kubebuilder:rbac:groups="",resources=pods/exec,verbs=create
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=apps,resources=controllerrevisions,verbs=get;list;watch;create;update;patch;delete

func (r *GracefulSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	gs := &appsv1alpha1.GracefulSet{}
	if err := r.Get(ctx, req.NamespacedName, gs); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	deleting := !gs.DeletionTimestamp.IsZero()
	if deleting && gs.Spec.DeletionPolicy == appsv1alpha1.DeletionPolicyDelete {
		// Let garbage collection delete the pods right away.
		if controllerutil.RemoveFinalizer(gs, finalizerName) {
			return ctrl.Result{}, r.Update(ctx, gs)
		}
		return ctrl.Result{}, nil
	}
	if !deleting && !controllerutil.ContainsFinalizer(gs, finalizerName) {
		// The finalizer keeps the GracefulSet around while its pods drain, so
		// deleting it does not let garbage collection kill every pod at once.
		controllerutil.AddFinalizer(gs, finalizerName)
		if err := r.Update(ctx, gs); err != nil {
			return ctrl.Result{}, err
		}
	}
	if deleting && !controllerutil.ContainsFinalizer(gs, finalizerName) {
		return ctrl.Result{}, nil
	}
	if target := gs.Annotations[rollbackAnnotation]; target != "" && !deleting {
		return ctrl.Result{}, r.rollback(ctx, gs, target)
	}

	// Pods are found by the owner label, not the user's selector, so a selector
	// that does not match the template cannot hide pods and cause endless creation.
	podList := &corev1.PodList{}
	if err := r.APIReader.List(ctx, podList, client.InNamespace(gs.Namespace), client.MatchingLabels{ownerLabel: gs.Name}); err != nil {
		return ctrl.Result{}, err
	}

	version := desiredVersion(gs)
	// updated: serving pods of the desired version. old: serving pods of an
	// older version, kept until a replacement is available. draining: the rest.
	var updated, old, draining []corev1.Pod
	for i := range podList.Items {
		pod := podList.Items[i]
		if pod.DeletionTimestamp != nil {
			continue // already terminating
		}
		isDraining := pod.Labels[drainingLabel] == "true"
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			// Exited pods are removed. A draining pod that exited has finished its
			// work; a serving pod that exited (or was evicted) gets replaced.
			reason := "completed"
			if !isDraining {
				reason = "replaced"
				r.Recorder.Eventf(gs, corev1.EventTypeWarning, "PodExited", "pod %s exited with phase %s, replacing it", pod.Name, pod.Status.Phase)
			}
			if err := r.deletePod(ctx, gs, &pod, reason); err != nil {
				return ctrl.Result{}, err
			}
			continue
		}
		switch {
		case isDraining:
			draining = append(draining, pod)
		case pod.Labels[versionLabel] == version:
			updated = append(updated, pod)
		default:
			old = append(old, pod)
		}
	}

	desired := int32(1)
	if gs.Spec.Replicas != nil {
		desired = *gs.Spec.Replicas
	}
	selector, selectorErr := validSelector(gs, version)
	now := time.Now()

	var requeueAfter time.Duration
	minRequeue := func(d time.Duration) {
		if d > 0 && (requeueAfter == 0 || d < requeueAfter) {
			requeueAfter = d
		}
	}

	if !deleting {
		if err := r.syncRevisions(ctx, gs, version); err != nil {
			return ctrl.Result{}, err
		}
	}

	switch {
	case deleting:
		draining = append(append(draining, updated...), old...)
		updated, old = nil, nil
	case gs.Spec.Paused:
		// Nothing is created or newly drained while paused.
	default:
		// Scale-up first re-uses pods of this version that are still draining
		// (e.g. after a scale-down), as they are already warm.
		if deficit := int(desired) - len(updated); deficit > 0 && selectorErr == nil {
			var reused []string
			draining, reused = r.reuseDraining(ctx, gs, version, draining, deficit, &updated)
			if len(reused) > 0 {
				logger.Info("re-using draining pods", "pods", reused)
			}
		}

		available, next := countAvailable(updated, gs.Spec.MinReadySeconds, now)
		minRequeue(next)

		// Old pods keep serving until enough new pods are available.
		keepOld := int(desired) - int(available)
		if keepOld < 0 {
			keepOld = 0
		}
		if len(old) > keepOld {
			n := len(old) - keepOld
			ordered := r.drainOrder(ctx, gs, old)
			draining = append(draining, ordered[:n]...)
			old = ordered[n:]
		}
		if len(updated) > int(desired) {
			n := len(updated) - int(desired)
			logger.Info("scaling down", "version", version, "draining", n)
			ordered := r.drainOrder(ctx, gs, updated)
			draining = append(draining, ordered[:n]...)
			updated = ordered[n:]
		}

		if selectorErr == nil {
			surge := resolveSurge(gs.Spec.MaxSurge, desired)
			toCreate := int(desired) - len(updated)
			if room := int(desired) + surge - len(updated) - len(old); room < toCreate {
				toCreate = room
			}
			if toCreate > 0 {
				logger.Info("creating pods", "version", version, "count", toCreate)
			}
			for i := 0; i < toCreate; i++ {
				pod, err := r.createPod(ctx, gs, version)
				if err != nil {
					return ctrl.Result{}, err
				}
				updated = append(updated, *pod)
			}
		}
	}

	if deleting && len(updated) == 0 && len(old) == 0 && len(draining) == 0 {
		controllerutil.RemoveFinalizer(gs, finalizerName)
		return ctrl.Result{}, r.Update(ctx, gs)
	}

	if removeFromEndpoints(gs) {
		for _, set := range [][]corev1.Pod{updated, old} {
			for i := range set {
				if err := r.setServing(ctx, &set[i], corev1.ConditionTrue); err != nil {
					return ctrl.Result{}, err
				}
			}
		}
	}
	for i := range draining {
		retry, err := r.startDrain(ctx, gs, &draining[i])
		if err != nil {
			return ctrl.Result{}, err
		}
		if retry {
			minRequeue(hookRetryPeriod)
		}
	}

	remaining, policyRequeue, err := r.handleDrainingPods(ctx, gs, draining)
	if err != nil {
		return ctrl.Result{}, err
	}
	minRequeue(policyRequeue)

	orig := gs.DeepCopy()
	available, _ := countAvailable(updated, gs.Spec.MinReadySeconds, now)
	statusSelector := labels.Everything()
	if selector != nil {
		notDraining, _ := labels.NewRequirement(drainingLabel, selection.NotIn, []string{"true"})
		statusSelector = selector.Add(*notDraining)
	}
	if gs.Status.ActiveVersion != version || gs.Status.RolloutStartTime == nil {
		gs.Status.RolloutStartTime = &metav1.Time{Time: now}
	}
	gs.Status.ObservedGeneration = gs.Generation
	gs.Status.ActiveVersion = version
	gs.Status.Replicas = int32(len(updated) + len(old))
	gs.Status.UpdatedReplicas = int32(len(updated))
	gs.Status.ReadyReplicas = countReadyPods(updated)
	gs.Status.AvailableReplicas = available
	gs.Status.DrainingPods = int32(len(remaining))
	gs.Status.TotalPods = int32(len(updated) + len(old) + len(remaining))
	gs.Status.DrainingVersions = buildDrainingVersionStatus(remaining)
	gs.Status.Selector = statusSelector.String()
	minRequeue(r.setConditions(gs, orig, desired, len(old), len(remaining), selectorErr, now))
	recordPodMetrics(gs, append(updated, old...), remaining)

	if err := r.Status().Patch(ctx, gs, client.MergeFrom(orig)); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// desiredVersion returns spec.version, or a hash of the pod template when the
// version is left empty, so template changes alone trigger a new generation.
func desiredVersion(gs *appsv1alpha1.GracefulSet) string {
	if gs.Spec.Version != "" {
		return gs.Spec.Version
	}
	b, _ := json.Marshal(gs.Spec.Template)
	h := fnv.New64a()
	h.Write(b)
	return fmt.Sprintf("%016x", h.Sum64())
}

// resolveSurge turns maxSurge into a pod count (rounded up, at least 1 so an
// upgrade can always make progress). Unset means 100%.
func resolveSurge(v *intstr.IntOrString, desired int32) int {
	if v == nil {
		v = &intstr.IntOrString{Type: intstr.String, StrVal: "100%"}
	}
	n, err := intstr.GetScaledValueFromIntOrPercent(v, int(desired), true)
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// countAvailable counts pods Ready for at least minReadySeconds and returns
// how long until the next Ready pod becomes available.
func countAvailable(pods []corev1.Pod, minReadySeconds int32, now time.Time) (int32, time.Duration) {
	var n int32
	var next time.Duration
	minReady := time.Duration(minReadySeconds) * time.Second
	for i := range pods {
		for _, c := range pods[i].Status.Conditions {
			if c.Type != corev1.PodReady || c.Status != corev1.ConditionTrue {
				continue
			}
			if wait := minReady - now.Sub(c.LastTransitionTime.Time); minReady == 0 || wait <= 0 {
				n++
			} else if next == 0 || wait < next {
				next = wait
			}
		}
	}
	return n, next
}

// validSelector parses spec.selector and checks it is non-empty and matches the
// labels the controller puts on new pods. Pods are not created while it fails.
func validSelector(gs *appsv1alpha1.GracefulSet, version string) (labels.Selector, error) {
	selector, err := metav1.LabelSelectorAsSelector(gs.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("invalid selector: %w", err)
	}
	if selector.Empty() {
		return selector, fmt.Errorf("selector must not be empty")
	}
	if !selector.Matches(labels.Set(podLabels(gs, version))) {
		return selector, fmt.Errorf("selector %q does not match the pod template labels", selector.String())
	}
	return selector, nil
}

func podLabels(gs *appsv1alpha1.GracefulSet, version string) map[string]string {
	l := make(map[string]string, len(gs.Spec.Template.Labels)+2)
	for k, v := range gs.Spec.Template.Labels {
		l[k] = v
	}
	l[versionLabel] = version
	l[ownerLabel] = gs.Name
	return l
}

func removeFromEndpoints(gs *appsv1alpha1.GracefulSet) bool {
	v := gs.Spec.DrainPolicy.RemoveFromEndpoints
	return v == nil || *v
}

func protectFromDisruption(gs *appsv1alpha1.GracefulSet) bool {
	v := gs.Spec.DrainPolicy.ProtectFromDisruption
	return v == nil || *v
}

func (r *GracefulSetReconciler) createPod(ctx context.Context, gs *appsv1alpha1.GracefulSet, version string) (*corev1.Pod, error) {
	tmpl := gs.Spec.Template.DeepCopy()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: gs.Name + "-",
			Namespace:    gs.Namespace,
			Labels:       podLabels(gs, version),
			Annotations:  tmpl.Annotations,
		},
		Spec: tmpl.Spec,
	}
	if removeFromEndpoints(gs) && !hasReadinessGate(pod) {
		pod.Spec.ReadinessGates = append(pod.Spec.ReadinessGates, corev1.PodReadinessGate{ConditionType: servingCondition})
	}
	if err := controllerutil.SetControllerReference(gs, pod, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, pod); err != nil {
		return nil, err
	}
	return pod, nil
}

func hasReadinessGate(pod *corev1.Pod) bool {
	for _, g := range pod.Spec.ReadinessGates {
		if g.ConditionType == servingCondition {
			return true
		}
	}
	return false
}

// setServing sets the serving readiness-gate condition on pods that carry the gate.
func (r *GracefulSetReconciler) setServing(ctx context.Context, pod *corev1.Pod, status corev1.ConditionStatus) error {
	if !hasReadinessGate(pod) {
		return nil
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == servingCondition && c.Status == status {
			return nil
		}
	}
	patched := pod.DeepCopy()
	cond := corev1.PodCondition{Type: servingCondition, Status: status, LastTransitionTime: metav1.Now()}
	replaced := false
	for i, c := range patched.Status.Conditions {
		if c.Type == servingCondition {
			patched.Status.Conditions[i] = cond
			replaced = true
		}
	}
	if !replaced {
		patched.Status.Conditions = append(patched.Status.Conditions, cond)
	}
	// Strategic merge keeps the kubelet's own conditions (merge key: type).
	if err := r.Status().Patch(ctx, patched, client.StrategicMergeFrom(pod)); err != nil {
		return client.IgnoreNotFound(err)
	}
	*pod = *patched
	return nil
}

// startDrain makes sure a draining pod is labeled, timestamped, protected from
// node consolidation, out of Service endpoints and told to drain. It is
// idempotent. It returns true when the drain hook should be retried later.
func (r *GracefulSetReconciler) startDrain(ctx context.Context, gs *appsv1alpha1.GracefulSet, pod *corev1.Pod) (bool, error) {
	if removeFromEndpoints(gs) {
		if err := r.setServing(ctx, pod, corev1.ConditionFalse); err != nil {
			return false, err
		}
	}

	patched := pod.DeepCopy()
	if patched.Labels == nil {
		patched.Labels = map[string]string{}
	}
	if patched.Annotations == nil {
		patched.Annotations = map[string]string{}
	}
	newlyDraining := patched.Labels[drainingLabel] != "true"
	patched.Labels[drainingLabel] = "true"
	if _, ok := patched.Annotations[drainingSinceAnnotation]; !ok {
		patched.Annotations[drainingSinceAnnotation] = time.Now().UTC().Format(time.RFC3339)
	}
	if protectFromDisruption(gs) {
		patched.Annotations[karpenterDoNotDisrupt] = "true"
		patched.Annotations[caSafeToEvict] = "false"
	}

	hook := gs.Spec.DrainPolicy.DrainHook
	retry := false
	if hook != nil && patched.Annotations[drainHookAnnotation] == "" && pod.Status.PodIP != "" {
		if err := r.callDrainHook(ctx, pod, hook); err != nil {
			log.FromContext(ctx).Info("drain hook failed, will retry", "pod", pod.Name, "error", err.Error())
			r.Recorder.Eventf(gs, corev1.EventTypeWarning, "DrainHookFailed", "pod %s: %v", pod.Name, err)
			retry = true
		} else {
			patched.Annotations[drainHookAnnotation] = time.Now().UTC().Format(time.RFC3339)
		}
	}

	if equalStringMaps(patched.Labels, pod.Labels) && equalStringMaps(patched.Annotations, pod.Annotations) {
		return retry, nil
	}
	if err := r.Patch(ctx, patched, client.MergeFrom(pod)); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if newlyDraining {
		r.Recorder.Eventf(gs, corev1.EventTypeNormal, "Draining", "pod %s (version %s) started draining", pod.Name, pod.Labels[versionLabel])
	}
	*pod = *patched
	return retry, nil
}

// reuseDraining moves up to n draining pods of the current version back into
// service. Pods whose drain hook was already called are skipped, because the
// application has been told to stop and there is no way to un-tell it.
func (r *GracefulSetReconciler) reuseDraining(ctx context.Context, gs *appsv1alpha1.GracefulSet, version string, draining []corev1.Pod, n int, updated *[]corev1.Pod) ([]corev1.Pod, []string) {
	var candidates, rest []corev1.Pod
	for _, p := range draining {
		if p.Labels[versionLabel] == version && p.Status.Phase == corev1.PodRunning && p.Annotations[drainHookAnnotation] == "" {
			candidates = append(candidates, p)
		} else {
			rest = append(rest, p)
		}
	}
	// Most recently drained first: they have lost the least traffic.
	sort.SliceStable(candidates, func(i, j int) bool { return drainingSince(&candidates[i]).After(drainingSince(&candidates[j])) })
	var reused []string
	for i := range candidates {
		pod := candidates[i]
		if len(reused) >= n {
			rest = append(rest, pod)
			continue
		}
		patched := pod.DeepCopy()
		delete(patched.Labels, drainingLabel)
		delete(patched.Annotations, drainingSinceAnnotation)
		for _, k := range []string{karpenterDoNotDisrupt, caSafeToEvict} {
			if _, fromTemplate := gs.Spec.Template.Annotations[k]; !fromTemplate {
				delete(patched.Annotations, k)
			}
		}
		if err := r.Patch(ctx, patched, client.MergeFrom(&pod)); err != nil {
			log.FromContext(ctx).Info("could not re-use draining pod", "pod", pod.Name, "error", err.Error())
			rest = append(rest, pod)
			continue
		}
		r.Recorder.Eventf(gs, corev1.EventTypeNormal, "Reused", "pod %s returned to service", pod.Name)
		*updated = append(*updated, *patched)
		reused = append(reused, pod.Name)
	}
	return rest, reused
}

// drainOrder returns pods sorted so the best candidates to drain come first:
// lowest pod-deletion-cost, then not Ready, then fewest in-flight requests
// (HTTP drain checks only), then oldest.
func (r *GracefulSetReconciler) drainOrder(ctx context.Context, gs *appsv1alpha1.GracefulSet, pods []corev1.Pod) []corev1.Pod {
	out := append([]corev1.Pod(nil), pods...)
	inflight := r.inflightValues(ctx, gs, out)
	cost := func(p *corev1.Pod) int64 {
		v, _ := strconv.ParseInt(p.Annotations[deletionCostAnn], 10, 64)
		return v
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := &out[i], &out[j]
		if ca, cb := cost(a), cost(b); ca != cb {
			return ca < cb
		}
		if ra, rb := isPodReady(a), isPodReady(b); ra != rb {
			return !ra
		}
		if ia, ib := inflight[a.Name], inflight[b.Name]; ia != ib {
			return ia < ib
		}
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	})
	return out
}

func equalStringMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// drainingSince returns when the pod started draining, falling back to its
// creation time for pods that have no (valid) timestamp annotation.
func drainingSince(pod *corev1.Pod) time.Time {
	if t, err := time.Parse(time.RFC3339, pod.Annotations[drainingSinceAnnotation]); err == nil {
		return t
	}
	return pod.CreationTimestamp.Time
}

func (r *GracefulSetReconciler) deletePod(ctx context.Context, gs *appsv1alpha1.GracefulSet, pod *corev1.Pod, reason string) error {
	if err := r.Delete(ctx, pod); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("deleting pod %s: %w", pod.Name, err)
	}
	if pod.Labels[drainingLabel] == "true" {
		drainDuration.WithLabelValues(gs.Namespace, gs.Name, reason).Observe(time.Since(drainingSince(pod)).Seconds())
	}
	return nil
}

// handleDrainingPods applies maxDrainingPods, the TTL cap and the drain mode.
// It returns the pods still draining and when to requeue.
func (r *GracefulSetReconciler) handleDrainingPods(ctx context.Context, gs *appsv1alpha1.GracefulSet, pods []corev1.Pod) ([]corev1.Pod, time.Duration, error) {
	logger := log.FromContext(ctx)
	policy := gs.Spec.DrainPolicy
	mode := policy.Mode
	if mode == "" {
		mode = appsv1alpha1.DrainPolicyWaitForCompletion
	}

	sort.SliceStable(pods, func(i, j int) bool { return drainingSince(&pods[i]).Before(drainingSince(&pods[j])) })

	// Safety cap: force-delete the pods that have drained longest.
	if policy.MaxDrainingPods != nil && len(pods) > int(*policy.MaxDrainingPods) {
		excess := len(pods) - int(*policy.MaxDrainingPods)
		for i := 0; i < excess; i++ {
			logger.Info("maxDrainingPods exceeded, force-deleting pod", "pod", pods[i].Name)
			r.Recorder.Eventf(gs, corev1.EventTypeWarning, "MaxDrainingExceeded", "force-deleted pod %s: more than %d pods draining", pods[i].Name, *policy.MaxDrainingPods)
			if err := r.deletePod(ctx, gs, &pods[i], "max-draining"); err != nil {
				return nil, 0, err
			}
		}
		pods = pods[excess:]
	}

	if mode == appsv1alpha1.DrainPolicyManual {
		return pods, 0, nil
	}

	var requeueAfter time.Duration
	var remaining, toCheck []corev1.Pod
	for i := range pods {
		pod := pods[i]
		if processesGone(&pod) {
			// Nothing is running, so any in-memory work is already lost; waiting
			// for a drain signal (or the TTL) would only hold a crash-looping pod.
			logger.Info("draining pod has no running containers, deleting", "pod", pod.Name)
			r.Recorder.Eventf(gs, corev1.EventTypeWarning, "NotRunning", "deleted draining pod %s: no containers running", pod.Name)
			if err := r.deletePod(ctx, gs, &pod, "not-running"); err != nil {
				return nil, 0, err
			}
			continue
		}
		if policy.TTL != nil {
			elapsed := time.Since(drainingSince(&pod))
			if elapsed >= policy.TTL.Duration {
				logger.Info("drain TTL reached, deleting pod", "pod", pod.Name, "draining", elapsed)
				r.Recorder.Eventf(gs, corev1.EventTypeNormal, "TTLExpired", "deleted pod %s after draining for %s", pod.Name, elapsed.Round(time.Second))
				if err := r.deletePod(ctx, gs, &pod, "ttl"); err != nil {
					return nil, 0, err
				}
				continue
			}
			if left := policy.TTL.Duration - elapsed; requeueAfter == 0 || left < requeueAfter {
				requeueAfter = left
			}
		}
		if mode == appsv1alpha1.DrainPolicyWaitForDrain {
			toCheck = append(toCheck, pod)
		} else {
			remaining = append(remaining, pod)
		}
	}
	if len(toCheck) == 0 {
		return remaining, requeueAfter, nil
	}

	dc := drainCheckOrDefault(gs)
	results := r.checkAll(ctx, toCheck, func(ctx context.Context, p *corev1.Pod) (bool, error) {
		return r.checkPodDrained(ctx, p, dc)
	})
	for i := range toCheck {
		pod := toCheck[i]
		if err := results[i].err; err != nil {
			drainCheckFailures.WithLabelValues(gs.Namespace, gs.Name).Inc()
			logger.Info("drain check failed, will retry", "pod", pod.Name, "error", err.Error())
		} else if results[i].drained {
			logger.Info("pod reports drained, deleting", "pod", pod.Name)
			r.Recorder.Eventf(gs, corev1.EventTypeNormal, "Drained", "pod %s finished its work", pod.Name)
			if err := r.deletePod(ctx, gs, &pod, "drained"); err != nil {
				return nil, 0, err
			}
			continue
		}
		remaining = append(remaining, pod)
	}
	period := time.Duration(dc.PeriodSeconds) * time.Second
	if period <= 0 {
		period = 30 * time.Second
	}
	if len(remaining) > 0 && (requeueAfter == 0 || period < requeueAfter) {
		requeueAfter = period
	}
	return remaining, requeueAfter, nil
}

// processesGone reports whether a started pod has no running containers
// (crash-looping or exited). Pods that have not started yet return false.
func processesGone(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning || len(pod.Status.ContainerStatuses) == 0 {
		return false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Running != nil {
			return false
		}
	}
	return true
}

// setConditions updates Available, Draining and Progressing, and returns when to
// requeue to notice the progress deadline passing.
func (r *GracefulSetReconciler) setConditions(gs, orig *appsv1alpha1.GracefulSet, desired int32, oldServing, draining int, selectorErr error, now time.Time) time.Duration {
	set := func(t string, status metav1.ConditionStatus, reason, msg string) {
		meta.SetStatusCondition(&gs.Status.Conditions, metav1.Condition{
			Type: t, Status: status, Reason: reason, Message: msg, ObservedGeneration: gs.Generation,
		})
	}

	avail := gs.Status.AvailableReplicas
	switch {
	case selectorErr != nil:
		set("Available", metav1.ConditionFalse, "InvalidSelector", selectorErr.Error())
	case avail+int32(oldServing) >= desired:
		set("Available", metav1.ConditionTrue, "ReplicasAvailable", fmt.Sprintf("%d/%d replicas available", avail+int32(oldServing), desired))
	default:
		set("Available", metav1.ConditionFalse, "ReplicasUnavailable", fmt.Sprintf("%d/%d replicas available", avail+int32(oldServing), desired))
	}

	if draining > 0 {
		set("Draining", metav1.ConditionTrue, "PodsDraining", fmt.Sprintf("%d pods draining", draining))
	} else {
		set("Draining", metav1.ConditionFalse, "NoPodsDraining", "No pods draining")
	}

	deadline := 600 * time.Second
	if gs.Spec.ProgressDeadlineSeconds != nil {
		deadline = time.Duration(*gs.Spec.ProgressDeadlineSeconds) * time.Second
	}
	elapsed := now.Sub(gs.Status.RolloutStartTime.Time)
	prev := meta.FindStatusCondition(orig.Status.Conditions, "Progressing")
	// A rollout that completed stays complete: losing a pod later (eviction,
	// node consolidation) is reported by Available, not as a stuck rollout.
	completed := prev != nil && prev.Reason == "RolloutComplete" && orig.Status.ActiveVersion == gs.Status.ActiveVersion
	switch {
	case gs.Spec.Paused:
		set("Progressing", metav1.ConditionUnknown, "Paused", "GracefulSet is paused")
	case completed && oldServing == 0:
		set("Progressing", metav1.ConditionTrue, "RolloutComplete", fmt.Sprintf("version %s is fully available", gs.Status.ActiveVersion))
	case oldServing == 0 && avail >= desired && gs.Status.UpdatedReplicas == desired:
		set("Progressing", metav1.ConditionTrue, "RolloutComplete", fmt.Sprintf("version %s is fully available", gs.Status.ActiveVersion))
	case elapsed >= deadline:
		if c := meta.FindStatusCondition(orig.Status.Conditions, "Progressing"); c == nil || c.Reason != "ProgressDeadlineExceeded" {
			r.Recorder.Eventf(gs, corev1.EventTypeWarning, "ProgressDeadlineExceeded", "version %s not fully available after %s", gs.Status.ActiveVersion, deadline)
		}
		set("Progressing", metav1.ConditionFalse, "ProgressDeadlineExceeded", fmt.Sprintf("%d/%d pods of version %s available after %s", avail, desired, gs.Status.ActiveVersion, deadline))
	default:
		set("Progressing", metav1.ConditionTrue, "RollingOut", fmt.Sprintf("%d/%d pods of version %s available", avail, desired, gs.Status.ActiveVersion))
		return deadline - elapsed
	}
	return 0
}

func countReadyPods(pods []corev1.Pod) int32 {
	var count int32
	for i := range pods {
		if isPodReady(&pods[i]) {
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
	for i := range pods {
		pod := &pods[i]
		version := pod.Labels[versionLabel]
		vs, ok := versionMap[version]
		if !ok {
			vs = &appsv1alpha1.VersionStatus{Version: version}
			versionMap[version] = vs
		}
		vs.Pods++
		if isPodReady(pod) {
			vs.ReadyPods++
		}
		creation := pod.CreationTimestamp
		if vs.OldestPodCreation == nil || creation.Before(vs.OldestPodCreation) {
			vs.OldestPodCreation = &creation
		}
	}
	result := make([]appsv1alpha1.VersionStatus, 0, len(versionMap))
	for _, vs := range versionMap {
		result = append(result, *vs)
	}
	// Stable order so the status does not change on every reconcile.
	sort.Slice(result, func(i, j int) bool { return result[i].Version < result[j].Version })
	return result
}


// SetupWithManager sets up the controller with the Manager
func (r *GracefulSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.GracefulSet{}).
		Owns(&corev1.Pod{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}).
		Complete(r)
}
