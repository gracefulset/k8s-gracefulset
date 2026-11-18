package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1alpha1 "github.com/gracefulset-io/gracefulset/api/v1alpha1"
)

const revisionVersionAnnotation = "gracefulset.io/version"

// revisionData is what a ControllerRevision stores: enough to roll back.
type revisionData struct {
	Version  string                 `json:"version,omitempty"`
	Template corev1.PodTemplateSpec `json:"template"`
}

func revisionName(gs *appsv1alpha1.GracefulSet, version string) string {
	h := fnv.New32a()
	h.Write([]byte(version))
	return fmt.Sprintf("%s-%08x", gs.Name, h.Sum32())
}

func (r *GracefulSetReconciler) listRevisions(ctx context.Context, gs *appsv1alpha1.GracefulSet) ([]appsv1.ControllerRevision, error) {
	list := &appsv1.ControllerRevisionList{}
	if err := r.APIReader.List(ctx, list, client.InNamespace(gs.Namespace), client.MatchingLabels{ownerLabel: gs.Name}); err != nil {
		return nil, err
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Revision > list.Items[j].Revision })
	return list.Items, nil
}

// syncRevisions records the current version + template as the newest
// ControllerRevision and prunes old ones beyond revisionHistoryLimit.
func (r *GracefulSetReconciler) syncRevisions(ctx context.Context, gs *appsv1alpha1.GracefulSet, version string) error {
	revs, err := r.listRevisions(ctx, gs)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(revisionData{Version: gs.Spec.Version, Template: gs.Spec.Template})
	if err != nil {
		return err
	}
	var maxRev int64
	if len(revs) > 0 {
		maxRev = revs[0].Revision
	}
	name := revisionName(gs, version)

	var current *appsv1.ControllerRevision
	for i := range revs {
		if revs[i].Name == name {
			current = &revs[i]
		}
	}
	if current != nil && !bytes.Equal(current.Data.Raw, raw) {
		// Same version, different template. Revision data is immutable, so
		// replace the revision.
		if err := r.Delete(ctx, current); client.IgnoreNotFound(err) != nil {
			return err
		}
		current = nil
	}
	switch {
	case current == nil:
		cr := &appsv1.ControllerRevision{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Namespace:   gs.Namespace,
				Labels:      map[string]string{ownerLabel: gs.Name},
				Annotations: map[string]string{revisionVersionAnnotation: version},
			},
			Data:     runtime.RawExtension{Raw: raw},
			Revision: maxRev + 1,
		}
		if err := controllerutil.SetControllerReference(gs, cr, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, cr); err != nil {
			return client.IgnoreAlreadyExists(err)
		}
		revs = append([]appsv1.ControllerRevision{*cr}, revs...)
		r.Recorder.Eventf(gs, corev1.EventTypeNormal, "RevisionRecorded", "recorded revision %d for version %s", cr.Revision, version)
	case current.Revision != maxRev:
		// Version became current again (rollback): make it the newest revision.
		patched := current.DeepCopy()
		patched.Revision = maxRev + 1
		if err := r.Patch(ctx, patched, client.MergeFrom(current)); err != nil {
			return client.IgnoreNotFound(err)
		}
	}

	limit := 10
	if gs.Spec.RevisionHistoryLimit != nil {
		limit = int(*gs.Spec.RevisionHistoryLimit)
	}
	var older []appsv1.ControllerRevision
	for _, rev := range revs {
		if rev.Name != name {
			older = append(older, rev)
		}
	}
	sort.Slice(older, func(i, j int) bool { return older[i].Revision > older[j].Revision })
	for i := limit; i < len(older); i++ {
		if err := r.Delete(ctx, &older[i]); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

// rollback restores spec.version and spec.template from a stored revision.
// target is "previous" or a version string. The annotation is always removed.
func (r *GracefulSetReconciler) rollback(ctx context.Context, gs *appsv1alpha1.GracefulSet, target string) error {
	revs, err := r.listRevisions(ctx, gs)
	if err != nil {
		return err
	}
	current := revisionName(gs, desiredVersion(gs))
	var found *appsv1.ControllerRevision
	for i := range revs {
		rev := &revs[i]
		if (target == "previous" && rev.Name != current) || rev.Annotations[revisionVersionAnnotation] == target {
			found = rev
			break // revs are newest first
		}
	}
	delete(gs.Annotations, rollbackAnnotation)
	if found == nil {
		r.Recorder.Eventf(gs, corev1.EventTypeWarning, "RollbackFailed", "no stored revision for %q", target)
		return r.Update(ctx, gs)
	}
	var data revisionData
	if err := json.Unmarshal(found.Data.Raw, &data); err != nil {
		return fmt.Errorf("revision %s: %w", found.Name, err)
	}
	gs.Spec.Version = data.Version
	gs.Spec.Template = data.Template
	r.Recorder.Eventf(gs, corev1.EventTypeNormal, "RolledBack", "rolled back to version %s (revision %d)", found.Annotations[revisionVersionAnnotation], found.Revision)
	return r.Update(ctx, gs)
}
