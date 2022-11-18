package v1alpha1

import (
	"encoding/json"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:path=rollouts,shortName=ro
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.HPAReplicas,selectorpath=.status.selector
// +kubebuilder:printcolumn:name="Desired",type="integer",JSONPath=".spec.replicas",description="Number of desired pods"
// +kubebuilder:printcolumn:name="Current",type="integer",JSONPath=".status.replicas",description="Total number of non-terminated pods targeted by this rollout"
// +kubebuilder:printcolumn:name="Up-to-date",type="integer",JSONPath=".status.updatedReplicas",description="Total number of non-terminated pods targeted by this rollout that have the desired template spec"
// +kubebuilder:printcolumn:name="Available",type="integer",JSONPath=".status.availableReplicas",description="Total number of available pods (ready for at least minReadySeconds) targeted by this rollout"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description="Time since resource was created"
// +kubebuilder:subresource:status

// Rollout is a specification for a Rollout resource
type Rollout struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec   RolloutSpec   `json:"spec" protobuf:"bytes,2,opt,name=spec"`
	Status RolloutStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

// RolloutSpec is the spec for a Rollout resource
type RolloutSpec struct {
	TemplateResolvedFromRef bool `json:"-"`
	SelectorResolvedFromRef bool `json:"-"`
	// Number of desired pods. This is a pointer to distinguish between explicit
	// zero and not specified. Defaults to 1.
	// +optional
	Replicas *int32 `json:"replicas,omitempty" protobuf:"varint,1,opt,name=replicas"`
	// Label selector for pods. Existing ReplicaSets whose pods are
	// selected by this will be the ones affected by this rollout.
	// It must match the pod template's labels.
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty" protobuf:"bytes,2,opt,name=selector"`
	// Template describes the pods that will be created.
	// +optional
	Template corev1.PodTemplateSpec `json:"template,omitempty" protobuf:"bytes,3,opt,name=template"`
	// Minimum number of seconds for which a newly created pod should be ready
	// without any of its container crashing, for it to be considered available.
	// Defaults to 0 (pod will be considered available as soon as it is ready)
	// +optional
	MinReadySeconds int32 `json:"minReadySeconds,omitempty" protobuf:"varint,4,opt,name=minReadySeconds"`
	// The deployment strategy to use to replace existing pods with new ones.
	// +optional
	Strategy RolloutStrategy `json:"strategy" protobuf:"bytes,5,opt,name=strategy"`
	// The number of old ReplicaSets to retain. If unspecified, will retain 10 old ReplicaSets
	RevisionHistoryLimit *int32 `json:"revisionHistoryLimit,omitempty" protobuf:"varint,6,opt,name=revisionHistoryLimit"`
	// Paused pauses the rollout at its current step.
	Paused bool `json:"paused,omitempty" protobuf:"varint,7,opt,name=paused"`
}

func (s *RolloutSpec) SetResolvedSelector(selector *metav1.LabelSelector) {
	s.SelectorResolvedFromRef = true
	s.Selector = selector
}

func (s *RolloutSpec) EmptyTemplate() bool {
	if len(s.Template.Labels) > 0 {
		return false
	}
	if len(s.Template.Annotations) > 0 {
		return false
	}
	return true
}

func (s *RolloutSpec) MarshalJSON() ([]byte, error) {
	type Alias RolloutSpec

	if s.TemplateResolvedFromRef || s.SelectorResolvedFromRef {
		obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&struct {
			Alias `json:",inline"`
		}{
			Alias: (Alias)(*s),
		})
		if err != nil {
			return nil, err
		}
		if s.TemplateResolvedFromRef {
			unstructured.RemoveNestedField(obj, "template")
		}
		if s.SelectorResolvedFromRef {
			unstructured.RemoveNestedField(obj, "selector")
		}

		return json.Marshal(obj)
	}
	return json.Marshal(&struct{ *Alias }{
		Alias: (*Alias)(s),
	})
}

// ObjectRef holds a references to the Kubernetes object
type ObjectRef struct {
	// API Version of the referent
	APIVersion string `json:"apiVersion,omitempty" protobuf:"bytes,1,opt,name=apiVersion"`
	// Kind of the referent
	Kind string `json:"kind,omitempty" protobuf:"bytes,2,opt,name=kind"`
	// Name of the referent
	Name string `json:"name,omitempty" protobuf:"bytes,3,opt,name=name"`
}

const (
	// DefaultRolloutUniqueLabelKey is the default key of the selector that is added
	// to existing ReplicaSets (and label key that is added to its pods) to prevent the existing ReplicaSets
	// to select new pods (and old pods being select by new ReplicaSet).
	DefaultRolloutUniqueLabelKey string = "rollouts-pod-template-hash"
	// DefaultReplicaSetScaleDownDeadlineAnnotationKey is the default key attached to an old stable ReplicaSet after
	// the rollout transitioned to a new version. It contains the time when the controller can scale down the RS.
	DefaultReplicaSetScaleDownDeadlineAnnotationKey = "scale-down-deadline"
	// LabelKeyControllerInstanceID is the label the controller uses for the rollout, experiment, analysis segregation
	// between controllers. Controllers will only operate on objects with the same instanceID as the controller.
	LabelKeyControllerInstanceID = "argo-rollouts.argoproj.io/controller-instance-id"
)

// RolloutStrategy defines strategy to apply during next rollout
type RolloutStrategy struct {
	// +optional
	Canary *CanaryStrategy `json:"canary,omitempty" protobuf:"bytes,2,opt,name=canary"`
}

// AntiAffinity defines which inter-pod scheduling rule to use for anti-affinity injection
type AntiAffinity struct {
	// +optional
	PreferredDuringSchedulingIgnoredDuringExecution *PreferredDuringSchedulingIgnoredDuringExecution `json:"preferredDuringSchedulingIgnoredDuringExecution,omitempty" protobuf:"bytes,1,opt,name=preferredDuringSchedulingIgnoredDuringExecution"`
	// +optional
	RequiredDuringSchedulingIgnoredDuringExecution *RequiredDuringSchedulingIgnoredDuringExecution `json:"requiredDuringSchedulingIgnoredDuringExecution,omitempty" protobuf:"bytes,2,opt,name=requiredDuringSchedulingIgnoredDuringExecution"`
}

// PreferredDuringSchedulingIgnoredDuringExecution defines the weight of the anti-affinity injection
type PreferredDuringSchedulingIgnoredDuringExecution struct {
	// Weight associated with matching the corresponding podAffinityTerm, in the range 1-100.
	Weight int32 `json:"weight" protobuf:"varint,1,opt,name=weight"`
}

// RequiredDuringSchedulingIgnoredDuringExecution defines inter-pod scheduling rule to be RequiredDuringSchedulingIgnoredDuringExecution
type RequiredDuringSchedulingIgnoredDuringExecution struct{}

// PingPongSpec holds the ping and pong service name.
type PingPongSpec struct {
	// name of the ping service
	PingService string `json:"pingService" protobuf:"bytes,1,opt,name=pingService"`
	// name of the pong service
	PongService string `json:"pongService" protobuf:"bytes,2,opt,name=pongService"`
}

// AnalysisRunStrategy configuration for the analysis runs and experiments to retain
type AnalysisRunStrategy struct {
	// SuccessfulRunHistoryLimit limits the number of old successful analysis runs and experiments to be retained in a history
	SuccessfulRunHistoryLimit *int32 `json:"successfulRunHistoryLimit,omitempty" protobuf:"varint,1,opt,name=successfulRunHistoryLimit"`
	// UnsuccessfulRunHistoryLimit limits the number of old unsuccessful analysis runs and experiments to be retained in a history.
	// Stages for unsuccessful: "Error", "Failed", "Inconclusive"
	UnsuccessfulRunHistoryLimit *int32 `json:"unsuccessfulRunHistoryLimit,omitempty" protobuf:"varint,2,opt,name=unsuccessfulRunHistoryLimit"`
}

type StickinessConfig struct {
	Enabled         bool  `json:"enabled" protobuf:"varint,1,opt,name=enabled"`
	DurationSeconds int64 `json:"durationSeconds" protobuf:"varint,2,opt,name=durationSeconds"`
}

// PodTemplateMetadata extra labels to add to the template
type PodTemplateMetadata struct {
	// Labels Additional labels to add to the experiment
	// +optional
	Labels map[string]string `json:"labels,omitempty" protobuf:"bytes,1,rep,name=labels"`
	// Annotations additional annotations to add to the experiment
	// +optional
	Annotations map[string]string `json:"annotations,omitempty" protobuf:"bytes,2,rep,name=annotations"`
}

// ReplicaSetSpecRef defines which RS that the experiment's template will use.
type ReplicaSetSpecRef string

const (
	// CanarySpecRef indicates the RS template should be pulled from the newRS's template
	CanarySpecRef ReplicaSetSpecRef = "canary"
	// StableSpecRef indicates the RS template should be pulled from the stableRS's template
	StableSpecRef ReplicaSetSpecRef = "stable"
)

// StringMatch Used to define what type of matching we will use exact, prefix, or regular expression
type StringMatch struct {
	// Exact The string must match exactly
	Exact string `json:"exact,omitempty" protobuf:"bytes,1,opt,name=exact"`
	// Prefix The string will be prefixed matched
	Prefix string `json:"prefix,omitempty" protobuf:"bytes,2,opt,name=prefix"`
	// Regex The string will be regular expression matched
	Regex string `json:"regex,omitempty" protobuf:"bytes,3,opt,name=regex"`
}

// ArgumentValueFrom defines references to fields within resources to grab for the value (i.e. Pod Template Hash)
type ArgumentValueFrom struct {
	// PodTemplateHashValue gets the value from one of the children ReplicaSet's Pod Template Hash
	PodTemplateHashValue *ValueFromPodTemplateHash `json:"podTemplateHashValue,omitempty" protobuf:"bytes,1,opt,name=podTemplateHashValue,casttype=ValueFromPodTemplateHash"`
	//FieldRef
	FieldRef *FieldRef `json:"fieldRef,omitempty" protobuf:"bytes,2,opt,name=fieldRef"`
}

type FieldRef struct {
	// Required: Path of the field to select in the specified API version
	FieldPath string `json:"fieldPath" protobuf:"bytes,1,opt,name=fieldPath"`
}

// ValueFromPodTemplateHash indicates which ReplicaSet pod template pod hash to use
type ValueFromPodTemplateHash string

const (
	// Stable tells the Rollout to get the pod template hash from the stable ReplicaSet
	Stable ValueFromPodTemplateHash = "Stable"
	// Latest tells the Rollout to get the pod template hash from the latest ReplicaSet
	Latest ValueFromPodTemplateHash = "Latest"
)

const (
	// RolloutTypeLabel indicates how the rollout created the analysisRun
	RolloutTypeLabel = "rollout-type"
	// RolloutTypeStepLabel indicates that the analysisRun was created as a canary step
	RolloutTypeStepLabel = "Step"
	// RolloutTypeBackgroundRunLabel indicates that the analysisRun was created in Background to an execution
	RolloutTypeBackgroundRunLabel = "Background"
	// RolloutTypePrePromotionLabel indicates that the analysisRun was created before the active service promotion
	RolloutTypePrePromotionLabel = "PrePromotion"
	// RolloutTypePostPromotionLabel indicates that the analysisRun was created after the active service promotion
	RolloutTypePostPromotionLabel = "PostPromotion"
	// RolloutCanaryStepIndexLabel indicates which step created this analysisRun
	RolloutCanaryStepIndexLabel = "step-index"
)

// RolloutPause defines a pause stage for a rollout
type RolloutPause struct {
	// Duration the amount of time to wait before moving to the next step.
	// +optional
	Duration *intstr.IntOrString `json:"duration,omitempty" protobuf:"bytes,1,opt,name=duration"`
}

// DurationSeconds converts the pause duration to seconds
// If Duration is nil 0 is returned
// if Duration values is string and does not contain a valid unit -1 is returned
func (p RolloutPause) DurationSeconds() int32 {
	if p.Duration != nil {
		if p.Duration.Type == intstr.String {
			s, err := strconv.ParseInt(p.Duration.StrVal, 10, 32)
			if err != nil {
				d, err := time.ParseDuration(p.Duration.StrVal)
				if err != nil {
					return -1
				}
				return int32(d.Seconds())
			}
			// special case where no unit was specified
			return int32(s)
		}
		return p.Duration.IntVal
	}
	return 0
}

// DurationFromInt creates duration in seconds from int value
func DurationFromInt(i int) *intstr.IntOrString {
	d := intstr.FromInt(i)
	return &d
}

// DurationFromString creates duration from string
// value must be a string representation of an int with optional time unit (see time.ParseDuration)
func DurationFromString(s string) *intstr.IntOrString {
	d := intstr.FromString(s)
	return &d
}

// PauseReason reasons that the rollout can pause
type PauseReason string

const (
	// PauseReasonInconclusiveAnalysis pauses rollout when rollout has an inconclusive analysis run
	PauseReasonInconclusiveAnalysis PauseReason = "InconclusiveAnalysisRun"
	// PauseReasonCanaryPauseStep pause rollout for canary pause step
	PauseReasonCanaryPauseStep PauseReason = "CanaryPauseStep"
)

// PauseCondition the reason for a pause and when it started
type PauseCondition struct {
	Reason    PauseReason `json:"reason" protobuf:"bytes,1,opt,name=reason,casttype=PauseReason"`
	StartTime metav1.Time `json:"startTime" protobuf:"bytes,2,opt,name=startTime"`
}

// RolloutPhase are a set of phases that this rollout
type RolloutPhase string

const (
	// RolloutPhaseHealthy indicates a rollout is healthy
	RolloutPhaseHealthy RolloutPhase = "Healthy"
	// RolloutPhaseDegraded indicates a rollout is degraded (e.g. pod unavailability, misconfiguration)
	RolloutPhaseDegraded RolloutPhase = "Degraded"
	// RolloutPhaseProgressing indicates a rollout is not yet healthy but still making progress towards a healthy state
	RolloutPhaseProgressing RolloutPhase = "Progressing"
	// RolloutPhasePaused indicates a rollout is not yet healthy and will not make progress until unpaused
	RolloutPhasePaused RolloutPhase = "Paused"
)

// RolloutStatus is the status for a Rollout resource
type RolloutStatus struct {
	// PauseConditions 表示 Rollout "自动" 暂停的原因 比如 CanaryPauseStep. 自动意味着列表中的元素是系统添加进去的，比如定时或者遇到 Pause step 等等。
	// 如果该列表是空的，但是 controllerPause 是 true，则表示是用户手动恢复了 Rollout
	PauseConditions []PauseCondition `json:"pauseConditions,omitempty" protobuf:"bytes,2,rep,name=pauseConditions"`
	// ControllerPause 表示 Rollout 被系统"自动"暂停时会标记为true，同时会写入 PauseConditions。 当被系统自动暂停的 Rollout 被用户手动恢复时, PauseConditions 会被清空
	// 但 ControllerPause 的值还是 true
	ControllerPause bool `json:"controllerPause,omitempty" protobuf:"varint,3,opt,name=controllerPause"`
	// CurrentPodHash 表示当前 pod template hash
	// +optional
	CurrentPodHash string `json:"currentPodHash,omitempty" protobuf:"bytes,5,opt,name=currentPodHash"`
	// CurrentStepHash 当前 step "列表" 的 hash(不是单个 step)，用于检测 steps 是否发生了变化。
	// +optional
	CurrentStepHash string `json:"currentStepHash,omitempty" protobuf:"bytes,6,opt,name=currentStepHash"`
	// 未终止的副本数总数， 和 label selector相匹配.
	// +optional
	Replicas int32 `json:"replicas,omitempty" protobuf:"varint,7,opt,name=replicas"`
	// 未终止的副本数总数 里面，已经更新到预期的 pod template的副本数.
	// +optional
	UpdatedReplicas int32 `json:"updatedReplicas,omitempty" protobuf:"varint,8,opt,name=updatedReplicas"`
	// Ready的pod总数.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty" protobuf:"varint,9,opt,name=readyReplicas"`
	// healthy 的pod总数，available 和 ready的区别是 pod进入并保持ready状态经过了 minReadySecond 之后，就认为是 available的
	// 正常来讲，最终 AvailableReplicas = ReadyReplicas
	// +optional
	AvailableReplicas int32 `json:"availableReplicas,omitempty" protobuf:"varint,10,opt,name=availableReplicas"`
	// CurrentStepIndex 表示当前 Rollout 在哪个 step, 为空时，表示还没开始.
	// +optional
	CurrentStepIndex *int32 `json:"currentStepIndex,omitempty" protobuf:"varint,11,opt,name=currentStepIndex"`
	// 用于避免hash冲突的参数，为 Rollout 生成新的 RS 的名称时，需要使用Hash算法生成.
	// +optional
	CollisionCount *int32 `json:"collisionCount,omitempty" protobuf:"varint,12,opt,name=collisionCount"`
	// k8s概念，正在被监听的 generation， 一般最终和 metadata.generation 一致，
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty" protobuf:"bytes,13,opt,name=observedGeneration"`
	// Conditions 表示 当前 Rollout 的状态和原因列表.
	// +optional
	Conditions []RolloutCondition `json:"conditions,omitempty" protobuf:"bytes,14,rep,name=conditions"`

	// Canary 发布策略的状态， 暂时没用
	Canary CanaryStatus `json:"canary,omitempty" protobuf:"bytes,15,opt,name=canary"`
	// HPAReplicas 可以接受流量的副本数， 具体使用时，是所有RS的 ReplicaSetStatus 中的 Replicas 数相加
	// +optional
	HPAReplicas int32 `json:"HPAReplicas,omitempty" protobuf:"varint,17,opt,name=HPAReplicas"`
	// Selector 可以筛选出可接受流量的pod的选择器， 具体使用时，就是指 Rollout .Spec.Selector, 暂时还没看到哪里控制了是否可接受流量
	// +optional
	Selector string `json:"selector,omitempty" protobuf:"bytes,18,opt,name=selector"`
	// StableRS stable rs 的 pod template hash， 也是 RS 的名称
	// +optional
	// TODO 需要确认stable RS在status中的写入时机
	StableRS string `json:"stableRS,omitempty" protobuf:"bytes,19,opt,name=stableRS"`
	// RestartedAt 表示该 Rollout 最后一次重启的时间
	// Phase 表示 Rollout 的 Phase. 只有 ObservedGeneration == Metadata.Generation 时才可以拿来用
	Phase RolloutPhase `json:"phase,omitempty" protobuf:"bytes,22,opt,name=phase,casttype=RolloutPhase"`
	// Message 是 Phase 的描述信息
	Message string `json:"message,omitempty" protobuf:"bytes,23,opt,name=message"`
}

type PingPongType string

const (
	PPPing PingPongType = "ping"
	PPPong PingPongType = "pong"
)

type WeightDestination struct {
	// Weight is an percentage of traffic being sent to this destination
	Weight int32 `json:"weight" protobuf:"varint,1,opt,name=weight"`
	// ServiceName is the Kubernetes service name traffic is being sent to
	ServiceName string `json:"serviceName,omitempty" protobuf:"bytes,2,opt,name=serviceName"`
	// PodTemplateHash is the pod template hash label for this destination
	PodTemplateHash string `json:"podTemplateHash,omitempty" protobuf:"bytes,3,opt,name=podTemplateHash"`
}

// RolloutConditionType defines the conditions of Rollout
type RolloutConditionType string

// These are valid conditions of a rollout.
const (
	// InvalidSpec 表示 Rollout 的 Spec 不合法，在我们的场景下不会出现，因为Spec校验回同步进行，并直接通过接口返回.
	InvalidSpec RolloutConditionType = "InvalidSpec"
	// RolloutAvailable means the rollout is available, ie. the active service is pointing at a
	// replicaset with the required replicas up and running for at least minReadySeconds.
	RolloutAvailable RolloutConditionType = "Available"
	// RolloutProgressing Rollout 处于中间状态，一般是新RS被创建，新pod正在扩容中，或老pod正在缩容中。
	RolloutProgressing RolloutConditionType = "Progressing"
	// RolloutReplicaFailure ReplicaFailure is added in a deployment when one of its pods
	// fails to be created or deleted.
	RolloutReplicaFailure RolloutConditionType = "ReplicaFailure"
	// RolloutPaused 处于 Pause 状态，该状态一定是中间状态. 该状态下经过的时间，不会计入 Rollout 总时长
	RolloutPaused RolloutConditionType = "Paused"
	// RolloutCompleted 表示 Rollout 完成，达到了预期的 Revision 并且不处于任何中间状态.
	RolloutCompleted RolloutConditionType = "Completed"
	// RolloutHealthy 表示 Rollout 完成，且副本数达到预期 且 所有 pod 可接受流量（不确定有没有判定 minReadySecond）.
	RolloutHealthy RolloutConditionType = "Healthy"
)

// RolloutCondition describes the state of a rollout at a certain point.
type RolloutCondition struct {
	// Type of deployment condition.
	Type RolloutConditionType `json:"type" protobuf:"bytes,1,opt,name=type,casttype=RolloutConditionType"`
	// Phase of the condition, one of True, False, Unknown.
	Status corev1.ConditionStatus `json:"status" protobuf:"bytes,2,opt,name=status,casttype=k8s.io/api/core/v1.ConditionStatus"`
	// The last time this condition was updated.
	LastUpdateTime metav1.Time `json:"lastUpdateTime" protobuf:"bytes,3,opt,name=lastUpdateTime"`
	// Last time the condition transitioned from one status to another.
	LastTransitionTime metav1.Time `json:"lastTransitionTime" protobuf:"bytes,4,opt,name=lastTransitionTime"`
	// The reason for the condition's last transition.
	Reason string `json:"reason" protobuf:"bytes,5,opt,name=reason"`
	// A human readable message indicating details about the transition.
	Message string `json:"message" protobuf:"bytes,6,opt,name=message"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// RolloutList is a list of Rollout resources
type RolloutList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata" protobuf:"bytes,1,opt,name=metadata"`

	Items []Rollout `json:"items" protobuf:"bytes,2,rep,name=items"`
}
