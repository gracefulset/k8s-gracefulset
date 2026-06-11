package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DrainPolicyMode defines how old pods are drained
type DrainPolicyMode string

const (
	DrainPolicyWaitForCompletion DrainPolicyMode = "WaitForCompletion"
	DrainPolicyWaitForDrain      DrainPolicyMode = "WaitForDrain"
	DrainPolicyTTL               DrainPolicyMode = "TTL"
	DrainPolicyManual            DrainPolicyMode = "Manual"
)

// DrainCheck configures how the controller polls a pod to determine whether it
// has finished serving and is safe to remove. Used by the WaitForDrain mode.
type DrainCheck struct {
	// Path is the HTTP path to poll on the pod (e.g. /drain-status)
	// +kubebuilder:default=/drain-status
	Path string `json:"path,omitempty"`

	// Port is the container port to poll
	// +kubebuilder:default=8080
	Port int32 `json:"port,omitempty"`

	// Scheme is HTTP or HTTPS
	// +kubebuilder:validation:Enum=HTTP;HTTPS
	// +kubebuilder:default=HTTP
	Scheme string `json:"scheme,omitempty"`

	// PeriodSeconds is how often to poll the drain endpoint
	// +kubebuilder:default=30
	PeriodSeconds int32 `json:"periodSeconds,omitempty"`

	// A pod is considered drained when the response body contains this JSON field
	// set to zero. For example, with JSONField "inflight", a response of
	// {"inflight": 0} means the pod is safe to remove.
	// +kubebuilder:default=inflight
	JSONField string `json:"jsonField,omitempty"`
}

// DrainPolicy defines how old version pods should be handled
type DrainPolicy struct {
	// Mode determines the drain behavior
	// +kubebuilder:validation:Enum=WaitForCompletion;WaitForDrain;TTL;Manual
	Mode DrainPolicyMode `json:"mode"`

	// TTL is the duration after which old pods are force-deleted.
	// Used with TTL mode, or as a safety cap with WaitForDrain.
	// +optional
	TTL *metav1.Duration `json:"ttl,omitempty"`

	// DrainCheck configures the HTTP drain probe (only used with WaitForDrain mode)
	// +optional
	DrainCheck *DrainCheck `json:"drainCheck,omitempty"`

	// MaxDrainingPods is the maximum number of old pods allowed to be draining at once
	// +optional
	// +kubebuilder:default=20
	MaxDrainingPods *int32 `json:"maxDrainingPods,omitempty"`
}

// GracefulSetSpec defines the desired state of GracefulSet
type GracefulSetSpec struct {
	// Replicas is the desired number of pods for the current version
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	Replicas *int32 `json:"replicas,omitempty"`

	// Version identifies the current desired version (used to detect changes)
	Version string `json:"version"`

	// DrainPolicy defines how old version pods are handled
	// +optional
	DrainPolicy DrainPolicy `json:"drainPolicy,omitempty"`

	// Selector is the label selector for pods managed by this GracefulSet
	Selector *metav1.LabelSelector `json:"selector"`

	// Template is the pod template for creating new pods
	Template corev1.PodTemplateSpec `json:"template"`
}

// VersionStatus tracks pods for a specific version
type VersionStatus struct {
	// Version identifier
	Version string `json:"version"`

	// Pods is the count of pods running this version
	Pods int32 `json:"pods"`

	// ReadyPods is the count of ready pods for this version
	ReadyPods int32 `json:"readyPods"`

	// OldestPodCreation is when the oldest pod of this version was created
	// +optional
	OldestPodCreation *metav1.Time `json:"oldestPodCreation,omitempty"`
}

// GracefulSetStatus defines the observed state of GracefulSet
type GracefulSetStatus struct {
	// ActiveVersion is the current desired version
	ActiveVersion string `json:"activeVersion,omitempty"`

	// ReadyReplicas is the number of ready pods for the active version
	ReadyReplicas int32 `json:"readyReplicas"`

	// TotalPods is the total number of pods (all versions)
	TotalPods int32 `json:"totalPods"`

	// DrainingPods is the total number of pods from old versions still running
	DrainingPods int32 `json:"drainingPods"`

	// DrainingVersions lists all old versions that still have running pods
	// +optional
	DrainingVersions []VersionStatus `json:"drainingVersions,omitempty"`

	// Conditions represent the latest available observations
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Selector is the label selector in string form, required by the scale subresource
	// so that HPA can identify the pods belonging to this GracefulSet.
	// +optional
	Selector string `json:"selector,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.readyReplicas,selectorpath=.status.selector
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.activeVersion`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Draining",type=integer,JSONPath=`.status.drainingPods`
// +kubebuilder:printcolumn:name="Total",type=integer,JSONPath=`.status.totalPods`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GracefulSet is the Schema for the gracefulsets API
type GracefulSet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GracefulSetSpec   `json:"spec,omitempty"`
	Status GracefulSetStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// GracefulSetList contains a list of GracefulSet
type GracefulSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GracefulSet `json:"items"`
}
