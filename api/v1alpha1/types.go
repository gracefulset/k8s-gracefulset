package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// DrainPolicyMode defines how old pods are drained
type DrainPolicyMode string

const (
	DrainPolicyWaitForCompletion DrainPolicyMode = "WaitForCompletion"
	DrainPolicyWaitForDrain      DrainPolicyMode = "WaitForDrain"
	DrainPolicyTTL               DrainPolicyMode = "TTL"
	DrainPolicyManual            DrainPolicyMode = "Manual"
)

// DrainCheckType selects how WaitForDrain decides a pod is finished
type DrainCheckType string

const (
	DrainCheckHTTP DrainCheckType = "HTTP"
	DrainCheckTCP  DrainCheckType = "TCP"
	DrainCheckExec DrainCheckType = "Exec"
)

// DeletionPolicy controls what happens to pods when the GracefulSet is deleted
type DeletionPolicy string

const (
	DeletionPolicyDrain  DeletionPolicy = "Drain"
	DeletionPolicyDelete DeletionPolicy = "Delete"
)

// ExecDrainCheck runs a command in the pod; exit code 0 means drained.
type ExecDrainCheck struct {
	// Command to run (not run in a shell)
	// +kubebuilder:validation:MinItems=1
	Command []string `json:"command"`
	// Container to run it in; defaults to the first container
	// +optional
	Container string `json:"container,omitempty"`
}

// DrainCheck configures how the controller decides a draining pod has finished
// its work and is safe to remove. Used by the WaitForDrain mode.
//
//   - HTTP: GET path on port; drained when jsonField (dot-separated path into the
//     JSON body) is <= threshold, or is boolean true.
//   - TCP: drained once the port stops accepting connections.
//   - Exec: drained when the command exits 0.
type DrainCheck struct {
	// Type of check
	// +kubebuilder:validation:Enum=HTTP;TCP;Exec
	// +kubebuilder:default=HTTP
	Type DrainCheckType `json:"type,omitempty"`

	// Path is the HTTP path to poll on the pod (e.g. /drain-status)
	// +kubebuilder:default=/drain-status
	Path string `json:"path,omitempty"`

	// Port is the container port to poll (HTTP and TCP)
	// +kubebuilder:default=8080
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`

	// Scheme is HTTP or HTTPS. HTTPS skips certificate verification, like kubelet probes do.
	// +kubebuilder:validation:Enum=HTTP;HTTPS
	// +kubebuilder:default=HTTP
	Scheme string `json:"scheme,omitempty"`

	// PeriodSeconds is how often to run the check
	// +kubebuilder:default=30
	// +kubebuilder:validation:Minimum=1
	PeriodSeconds int32 `json:"periodSeconds,omitempty"`

	// JSONField is a dot-separated path into the HTTP response body, e.g.
	// "inflight" or "stats.sessions".
	// +kubebuilder:default=inflight
	JSONField string `json:"jsonField,omitempty"`

	// Threshold: the pod is drained when the JSON field is <= this value.
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	Threshold int64 `json:"threshold,omitempty"`

	// Exec configures the Exec check. The controller needs pods/exec permission.
	// +optional
	Exec *ExecDrainCheck `json:"exec,omitempty"`
}

// DrainHook is an HTTP endpoint the controller POSTs to once when a pod starts
// draining, so the application can stop accepting new work.
type DrainHook struct {
	// Path is the HTTP path to POST to (e.g. /drain)
	// +kubebuilder:default=/drain
	Path string `json:"path,omitempty"`
	// Port is the container port to call
	// +kubebuilder:default=8080
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`
	// Scheme is HTTP or HTTPS. HTTPS skips certificate verification, like kubelet probes do.
	// +kubebuilder:validation:Enum=HTTP;HTTPS
	// +kubebuilder:default=HTTP
	Scheme string `json:"scheme,omitempty"`
}

// DrainPolicy defines how old version pods should be handled
type DrainPolicy struct {
	// Mode determines the drain behavior
	// +kubebuilder:validation:Enum=WaitForCompletion;WaitForDrain;TTL;Manual
	// +kubebuilder:default=WaitForCompletion
	Mode DrainPolicyMode `json:"mode,omitempty"`

	// TTL is the duration after which old pods are force-deleted.
	// Used with TTL mode, or as a safety cap with WaitForDrain.
	// +optional
	TTL *metav1.Duration `json:"ttl,omitempty"`

	// DrainCheck configures the drain check (only used with WaitForDrain mode)
	// +optional
	DrainCheck *DrainCheck `json:"drainCheck,omitempty"`

	// DrainHook, if set, is called once when a pod starts draining
	// +optional
	DrainHook *DrainHook `json:"drainHook,omitempty"`

	// MaxDrainingPods caps how many pods may be draining at once. When the cap is
	// exceeded, the pods that have been draining longest are force-deleted.
	// Unset means no cap.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MaxDrainingPods *int32 `json:"maxDrainingPods,omitempty"`

	// RemoveFromEndpoints adds a readiness gate to every pod and sets it to False
	// when the pod starts draining, so Services stop sending it new traffic.
	// Pods only become Ready once the controller has set the gate.
	// +optional
	// +kubebuilder:default=true
	RemoveFromEndpoints *bool `json:"removeFromEndpoints,omitempty"`

	// ProtectFromDisruption annotates draining pods so Karpenter and Cluster
	// Autoscaler do not evict them during node consolidation.
	// +optional
	// +kubebuilder:default=true
	ProtectFromDisruption *bool `json:"protectFromDisruption,omitempty"`
}

// GracefulSetSpec defines the desired state of GracefulSet
type GracefulSetSpec struct {
	// Replicas is the desired number of pods for the current version
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	Replicas *int32 `json:"replicas,omitempty"`

	// Version identifies the current desired version (used to detect changes).
	// If empty, a hash of the pod template is used.
	// +optional
	Version string `json:"version,omitempty"`

	// MaxSurge is how many new-version pods may exist above replicas during an
	// upgrade, as a number or a percentage of replicas (rounded up, minimum 1).
	// Old pods keep serving until a new pod is available to take their place.
	// +optional
	// +kubebuilder:default="100%"
	// +kubebuilder:validation:XIntOrString
	MaxSurge *intstr.IntOrString `json:"maxSurge,omitempty"`

	// MinReadySeconds is how long a new pod must be Ready before it counts as
	// available and an old pod is moved to draining.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MinReadySeconds int32 `json:"minReadySeconds,omitempty"`

	// ProgressDeadlineSeconds is how long an upgrade may take to make all new
	// pods available before the Progressing condition turns False.
	// +optional
	// +kubebuilder:default=600
	// +kubebuilder:validation:Minimum=1
	ProgressDeadlineSeconds *int32 `json:"progressDeadlineSeconds,omitempty"`

	// Paused stops the controller from creating pods or starting new drains.
	// Pods that are already draining are still handled by the drain policy.
	// +optional
	Paused bool `json:"paused,omitempty"`

	// RevisionHistoryLimit is how many old revisions (ControllerRevisions) to
	// keep for rollback.
	// +optional
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=0
	RevisionHistoryLimit *int32 `json:"revisionHistoryLimit,omitempty"`

	// DeletionPolicy: Drain (default) drains every pod before the GracefulSet is
	// removed; Delete removes pods right away.
	// +optional
	// +kubebuilder:validation:Enum=Drain;Delete
	// +kubebuilder:default=Drain
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`

	// DrainPolicy defines how old version pods are handled
	// +optional
	// +kubebuilder:default={}
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
	// ObservedGeneration is the most recent generation observed by the controller
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ActiveVersion is the current desired version
	ActiveVersion string `json:"activeVersion,omitempty"`

	// RolloutStartTime is when the controller first saw the active version
	// +optional
	RolloutStartTime *metav1.Time `json:"rolloutStartTime,omitempty"`

	// Replicas is the number of serving (non-draining) pods of any version.
	// During an upgrade this includes old pods still waiting for a replacement.
	// +optional
	Replicas int32 `json:"replicas"`

	// UpdatedReplicas is the number of serving pods of the active version
	// +optional
	UpdatedReplicas int32 `json:"updatedReplicas"`

	// ReadyReplicas is the number of ready pods for the active version
	// +optional
	ReadyReplicas int32 `json:"readyReplicas"`

	// AvailableReplicas is the number of active-version pods ready for at least minReadySeconds
	// +optional
	AvailableReplicas int32 `json:"availableReplicas"`

	// TotalPods is the total number of pods (all versions)
	// +optional
	TotalPods int32 `json:"totalPods"`

	// DrainingPods is the number of pods draining
	// +optional
	DrainingPods int32 `json:"drainingPods"`

	// DrainingVersions lists the versions that still have draining pods
	// +optional
	DrainingVersions []VersionStatus `json:"drainingVersions,omitempty"`

	// Conditions represent the latest available observations
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Selector is the label selector in string form, required by the scale subresource
	// so that HPA can identify the pods belonging to this GracefulSet. Draining pods
	// are excluded.
	// +optional
	Selector string `json:"selector,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=gs
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.selector
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.activeVersion`
// +kubebuilder:printcolumn:name="Updated",type=integer,JSONPath=`.status.updatedReplicas`
// +kubebuilder:printcolumn:name="Available",type=integer,JSONPath=`.status.availableReplicas`
// +kubebuilder:printcolumn:name="Draining",type=integer,JSONPath=`.status.drainingPods`
// +kubebuilder:printcolumn:name="Total",type=integer,JSONPath=`.status.totalPods`
// +kubebuilder:printcolumn:name="Paused",type=boolean,JSONPath=`.spec.paused`,priority=1
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
