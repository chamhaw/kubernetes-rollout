package replicaset

import (
	"context"
	"fmt"
	"github.com/chamhaw/kubernetes-rollout/api/v1alpha1"
	"sort"
	"strconv"
	"time"

	log "github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	intstrutil "k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	corev1defaults "k8s.io/kubernetes/pkg/apis/core/v1"
	"k8s.io/kubernetes/pkg/controller"
	"k8s.io/utils/pointer"

	"github.com/chamhaw/kubernetes-rollout/rollout/utils/annotations"
	"github.com/chamhaw/kubernetes-rollout/rollout/utils/conditions"
	"github.com/chamhaw/kubernetes-rollout/rollout/utils/defaults"
	"github.com/chamhaw/kubernetes-rollout/rollout/utils/hash"
	logutil "github.com/chamhaw/kubernetes-rollout/rollout/utils/log"
	timeutil "github.com/chamhaw/kubernetes-rollout/rollout/utils/time"
)

// FindNewReplicaSet 找新 RS 列表， 一般来说，通过 podTemplateHash作为label来匹配
func FindNewReplicaSet(rollout *v1alpha1.Rollout, rsList []*appsv1.ReplicaSet) *appsv1.ReplicaSet {
	var newRSList []*appsv1.ReplicaSet

	// 先排空
	for _, rs := range rsList {
		if rs != nil {
			newRSList = append(newRSList, rs)
		}
	}
	rsList = newRSList
	// 按创建时间正序排序， 没有按revision。 时间早的优先被找到，TODO 是否应该倒序
	sort.Sort(controller.ReplicaSetsByCreationTimestamp(rsList))
	// 优先用json序列化的方式计算 podTemplateHash, 如果用hash能找到，则直接返回
	podHash := hash.ComputePodTemplateHash(&rollout.Spec.Template, rollout.Status.CollisionCount)
	if rs := searchRsByHash(rsList, podHash); rs != nil {
		return rs
	}
	// 用deepHash的方式计算 podTemplateHash
	oldHash := controller.ComputeHash(&rollout.Spec.Template, rollout.Status.CollisionCount)
	if rs := searchRsByHash(rsList, oldHash); rs != nil {
		logCtx := logutil.WithRollout(rollout)
		logCtx.Infof("ComputePodTemplateHash hash changed (new hash: %s, old hash: %s)", podHash, oldHash)
		return rs
	}

	// 兜底筛选，正常来讲是小概率事件
	// 用hash的方式都没找到，需要再次遍历rs列表，用递归比较pod template spec的方式来找新RS
	for _, rs := range rsList {
		//  spec.template.metadata 中的 canary/stable metadata  不参与比较
		rsCopy, _ := SyncReplicaSetEphemeralPodMetadata(rs, nil)
		//  template.Spec.Affinity 不参与比较
		live := &rsCopy.Spec.Template
		live.Spec.Affinity = RemoveInjectedAntiAffinityRule(live.Spec.Affinity, *rollout)

		desired := rollout.Spec.Template.DeepCopy()
		if PodTemplateEqualIgnoreHash(live, desired) {
			logCtx := logutil.WithRollout(rollout)
			logCtx.Infof("ComputePodTemplateHash hash changed (expected: %s, actual: %s)", podHash, rs.Labels[v1alpha1.DefaultRolloutUniqueLabelKey])
			return rs
		}
	}
	// new ReplicaSet does not exist.
	return nil
}

func searchRsByHash(rsList []*appsv1.ReplicaSet, hash string) *appsv1.ReplicaSet {
	for _, rs := range rsList {
		if rs.Labels[v1alpha1.DefaultRolloutUniqueLabelKey] == hash {
			return rs
		}
	}
	return nil
}

func GetRolloutAffinity(rollout v1alpha1.Rollout) *v1alpha1.AntiAffinity {
	var antiAffinityStrategy *v1alpha1.AntiAffinity
	if antiAffinityStrategy != nil {
		if antiAffinityStrategy.PreferredDuringSchedulingIgnoredDuringExecution != nil || antiAffinityStrategy.RequiredDuringSchedulingIgnoredDuringExecution != nil {
			return antiAffinityStrategy
		}
	}
	return nil
}

func GenerateReplicaSetAffinity(rollout v1alpha1.Rollout) *corev1.Affinity {
	antiAffinityStrategy := GetRolloutAffinity(rollout)
	currentPodHash := hash.ComputePodTemplateHash(&rollout.Spec.Template, rollout.Status.CollisionCount)
	affinitySpec := rollout.Spec.Template.Spec.Affinity.DeepCopy()
	if antiAffinityStrategy != nil && rollout.Status.StableRS != "" && rollout.Status.StableRS != currentPodHash {
		antiAffinityRule := CreateInjectedAntiAffinityRule(rollout)
		if affinitySpec == nil {
			affinitySpec = &corev1.Affinity{}
		}
		if affinitySpec.PodAntiAffinity == nil {
			affinitySpec.PodAntiAffinity = &corev1.PodAntiAffinity{}
		}
		podAntiAffinitySpec := affinitySpec.PodAntiAffinity
		if antiAffinityStrategy.PreferredDuringSchedulingIgnoredDuringExecution != nil {
			weightedPodAffinityTerm := corev1.WeightedPodAffinityTerm{
				Weight:          antiAffinityStrategy.PreferredDuringSchedulingIgnoredDuringExecution.Weight,
				PodAffinityTerm: antiAffinityRule,
			}
			if podAntiAffinitySpec.PreferredDuringSchedulingIgnoredDuringExecution == nil {
				podAntiAffinitySpec.PreferredDuringSchedulingIgnoredDuringExecution = []corev1.WeightedPodAffinityTerm{weightedPodAffinityTerm}
			} else {
				podAntiAffinitySpec.PreferredDuringSchedulingIgnoredDuringExecution = append(podAntiAffinitySpec.PreferredDuringSchedulingIgnoredDuringExecution, weightedPodAffinityTerm)
			}
		} else {
			if podAntiAffinitySpec.RequiredDuringSchedulingIgnoredDuringExecution == nil {
				podAntiAffinitySpec.RequiredDuringSchedulingIgnoredDuringExecution = []corev1.PodAffinityTerm{antiAffinityRule}
			} else {
				podAntiAffinitySpec.RequiredDuringSchedulingIgnoredDuringExecution = append(podAntiAffinitySpec.RequiredDuringSchedulingIgnoredDuringExecution, antiAffinityRule)
			}
		}
	}
	return affinitySpec
}

func CreateInjectedAntiAffinityRule(rollout v1alpha1.Rollout) corev1.PodAffinityTerm {
	// Create anti-affinity rule for last stable rollout
	antiAffinityRule := corev1.PodAffinityTerm{
		LabelSelector: &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      v1alpha1.DefaultRolloutUniqueLabelKey,
				Operator: metav1.LabelSelectorOpIn,
				// Most recent stable ReplicaSet
				Values: []string{rollout.Status.StableRS},
			}},
		},
		Namespaces:  []string{rollout.Namespace},
		TopologyKey: "kubernetes.io/hostname",
	}
	return antiAffinityRule
}

func HasInjectedAntiAffinityRule(affinity *corev1.Affinity, rollout v1alpha1.Rollout) (int, *corev1.PodAffinityTerm) {
	antiAffinityStrategy := GetRolloutAffinity(rollout)
	if antiAffinityStrategy != nil && affinity != nil && affinity.PodAntiAffinity != nil {
		podAntiAffinitySpec := affinity.PodAntiAffinity
		if antiAffinityStrategy.PreferredDuringSchedulingIgnoredDuringExecution != nil {
			for i := range podAntiAffinitySpec.PreferredDuringSchedulingIgnoredDuringExecution {
				podAffinityTerm := podAntiAffinitySpec.PreferredDuringSchedulingIgnoredDuringExecution[i].PodAffinityTerm
				for _, labelSelectorRequirement := range podAffinityTerm.LabelSelector.MatchExpressions {
					if labelSelectorRequirement.Key == v1alpha1.DefaultRolloutUniqueLabelKey {
						return i, &podAffinityTerm
					}
				}

			}
		} else {
			for i := range podAntiAffinitySpec.RequiredDuringSchedulingIgnoredDuringExecution {
				podAffinityTerm := podAntiAffinitySpec.RequiredDuringSchedulingIgnoredDuringExecution[i]
				for _, labelSelectorRequirement := range podAffinityTerm.LabelSelector.MatchExpressions {
					if labelSelectorRequirement.Key == v1alpha1.DefaultRolloutUniqueLabelKey {
						return i, &podAffinityTerm
					}
				}
			}
		}

	}
	return -1, nil
}

func RemoveInjectedAntiAffinityRule(affinity *corev1.Affinity, rollout v1alpha1.Rollout) *corev1.Affinity {
	i, _ := HasInjectedAntiAffinityRule(affinity, rollout)
	affinitySpec := affinity.DeepCopy()
	if i >= 0 {
		antiAffinityStrategy := GetRolloutAffinity(rollout)
		if antiAffinityStrategy.PreferredDuringSchedulingIgnoredDuringExecution != nil {
			antiAffinityTerms := affinitySpec.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution
			affinitySpec.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution = append(antiAffinityTerms[:i], antiAffinityTerms[i+1:]...)
		}
		if antiAffinityStrategy.RequiredDuringSchedulingIgnoredDuringExecution != nil {
			antiAffinityTerms := affinitySpec.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution
			affinitySpec.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution = append(antiAffinityTerms[:i], antiAffinityTerms[i+1:]...)
		}
		if len(affinitySpec.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution) == 0 {
			affinitySpec.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution = nil
		}
		if affinitySpec.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil && affinitySpec.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution == nil {
			affinitySpec.PodAntiAffinity = nil
		}
		if affinitySpec.PodAntiAffinity == nil && affinitySpec.PodAffinity == nil && affinitySpec.NodeAffinity == nil {
			affinitySpec = nil
		}
	}
	return affinitySpec
}

func IfInjectedAntiAffinityRuleNeedsUpdate(affinity *corev1.Affinity, rollout v1alpha1.Rollout) bool {
	_, podAffinityTerm := HasInjectedAntiAffinityRule(affinity, rollout)
	currentPodHash := hash.ComputePodTemplateHash(&rollout.Spec.Template, rollout.Status.CollisionCount)
	if podAffinityTerm != nil && rollout.Status.StableRS != currentPodHash {
		for _, labelSelectorRequirement := range podAffinityTerm.LabelSelector.MatchExpressions {
			if labelSelectorRequirement.Key == v1alpha1.DefaultRolloutUniqueLabelKey && labelSelectorRequirement.Values[0] != rollout.Status.StableRS {
				return true
			}
		}
	}
	return false
}

func NeedsRestart(rollout *v1alpha1.Rollout) bool {
	return false
}

// FindOldReplicaSets returns the old replica sets targeted by the given Rollout, with the given slice of RSes.
// 除了新的，全都是老的，包括 stable 的
func FindOldReplicaSets(rollout *v1alpha1.Rollout, rsList []*appsv1.ReplicaSet, newRS *appsv1.ReplicaSet) []*appsv1.ReplicaSet {
	var allRSs []*appsv1.ReplicaSet
	for _, rs := range rsList {
		// Filter out new replica set
		if newRS != nil && rs.UID == newRS.UID {
			continue
		}
		allRSs = append(allRSs, rs)
	}
	return allRSs
}

func NewRSNewReplicas(rollout *v1alpha1.Rollout, allRSs []*appsv1.ReplicaSet, newRS *appsv1.ReplicaSet) (int32, error) {
	if rollout.Spec.Strategy.Canary != nil {
		stableRS := GetStableRS(rollout, newRS, allRSs)
		otherRSs := GetOtherRSs(rollout, newRS, stableRS, allRSs)

		// 找到所有 healthy 的 pod 数 // 计算得到 最少要保证可用的 pod 数
		// 现在的pod总数
		totalReplicas := GetReplicaCountForReplicaSets(allRSs)
		otherRSReplicas := GetReplicaCountForReplicaSets(otherRSs)
		rolloutReplicas := defaults.GetReplicasOrDefault(rollout.Spec.Replicas)
		// 如果rollout定义的副本数多余 当前所有副本数，则，先扩到 rollout期望的数量
		if stableRS != nil && stableRS.Name != newRS.Name && totalReplicas+MaxUnavailable(rollout) < rolloutReplicas {
			log.Warningf("current total replicas is less than rollout spec, scale up to %d", rolloutReplicas)
			newRSUpTo := rolloutReplicas - (defaults.GetReplicasOrDefault(stableRS.Spec.Replicas) + otherRSReplicas)
			return newRSUpTo, nil
		}

		newRSReplicaCount, _ := CalculateReplicaCountsForBasicCanary(rollout, newRS, stableRS, otherRSs)
		log.Infof("calculated replicas is %d", newRSReplicaCount)
		return newRSReplicaCount, nil
	}
	return 0, fmt.Errorf("no rollout strategy provided")
}

// MaxRevision finds the highest revision in the replica sets
func MaxRevision(allRSs []*appsv1.ReplicaSet) int64 {
	max := int64(0)
	for _, rs := range allRSs {
		if v, err := Revision(rs); err != nil {
			// Skip the replica sets when it failed to parse their revision information
			log.WithError(err).Info("Couldn't parse revision, rollout controller will skip it when reconciling revisions.")
		} else if v > max {
			max = v
		}
	}
	return max
}

// Revision returns the revision number of the input object.
func Revision(obj runtime.Object) (int64, error) {
	acc, err := meta.Accessor(obj)
	if err != nil {
		return 0, err
	}
	v, ok := acc.GetAnnotations()[annotations.RevisionAnnotation]
	if !ok {
		return 0, nil
	}
	return strconv.ParseInt(v, 10, 64)
}

// FindActiveOrLatest returns the only active or the latest replica set in case there is at most one active
// replica set. If there are more active replica sets, then we should proportionally scale them.
func FindActiveOrLatest(newRS *appsv1.ReplicaSet, oldRSs []*appsv1.ReplicaSet) *appsv1.ReplicaSet {
	if newRS == nil && len(oldRSs) == 0 {
		return nil
	}

	sort.Sort(sort.Reverse(controller.ReplicaSetsByCreationTimestamp(oldRSs)))
	allRSs := controller.FilterActiveReplicaSets(append(oldRSs, newRS))

	switch len(allRSs) {
	case 0:
		// If there is no active replica set then we should return the newest.
		if newRS != nil {
			return newRS
		}
		return oldRSs[0]
	case 1:
		return allRSs[0]
	default:
		return nil
	}
}

// GetReplicaCountForReplicaSets returns the sum of Replicas of the given replica sets.
func GetReplicaCountForReplicaSets(replicaSets []*appsv1.ReplicaSet) int32 {
	totalReplicas := int32(0)
	for _, rs := range replicaSets {
		if rs != nil {
			totalReplicas += *(rs.Spec.Replicas)
		}
	}
	return totalReplicas
}

// GetAvailableReplicaCountForReplicaSets returns the number of available pods corresponding to the given replica sets.
func GetAvailableReplicaCountForReplicaSets(replicaSets []*appsv1.ReplicaSet) int32 {
	totalAvailableReplicas := int32(0)
	for _, rs := range replicaSets {
		if rs != nil {
			totalAvailableReplicas += rs.Status.AvailableReplicas
		}
	}
	return totalAvailableReplicas
}

// GetActualReplicaCountForReplicaSets 返回所有 所有RS的实际副本数
func GetActualReplicaCountForReplicaSets(replicaSets []*appsv1.ReplicaSet) int32 {
	totalActualReplicas := int32(0)
	for _, rs := range replicaSets {
		if rs != nil {
			totalActualReplicas += rs.Status.Replicas
		}
	}
	return totalActualReplicas
}

// GetReadyReplicaCountForReplicaSets returns the number of ready pods corresponding to the given replica sets.
func GetReadyReplicaCountForReplicaSets(replicaSets []*appsv1.ReplicaSet) int32 {
	totalReadyReplicas := int32(0)
	for _, rs := range replicaSets {
		if rs != nil {
			totalReadyReplicas += rs.Status.ReadyReplicas
		}
	}
	return totalReadyReplicas
}

// resolveFencePosts resolves both maxSurge and maxUnavailable. This needs to happen in one
// step. For example:
// Surge 是 0， 是先减 old，再加 new
// 2 desired, max unavailable 1%, surge 0% - should scale old(-1), then new(+1), then old(-1), then new(+1)
// 1 desired, max unavailable 1%, surge 0% - should scale old(-1), then new(+1)

// surge 大于0， 先 加 new 再 减 old
// 2 desired, max unavailable 25%, surge 1% - should scale new(+1), then old(-1), then new(+1), then old(-1)
// 1 desired, max unavailable 25%, surge 1% - should scale new(+1), then old(-1)
// 2 desired, max unavailable 0%, surge 1% - should scale new(+1), then old(-1), then new(+1), then old(-1)
// 1 desired, max unavailable 0%, surge 1% - should scale new(+1), then old(-1)
func resolveFencePosts(maxSurge, maxUnavailable *intstrutil.IntOrString, desired int32) (int32, int32, error) {
	// 字符串数字无论是否包含%符号，都表示百分比，数字表示数字值
	// surge 向上取整
	surge, err := intstrutil.GetValueFromIntOrPercent(intstrutil.ValueOrDefault(maxSurge, intstrutil.FromInt(0)), int(desired), true)
	if err != nil {
		return 0, 0, err
	}
	// unavailable 向下取整
	unavailable, err := intstrutil.GetValueFromIntOrPercent(intstrutil.ValueOrDefault(maxUnavailable, intstrutil.FromInt(0)), int(desired), false)
	if err != nil {
		return 0, 0, err
	}

	if surge == 0 && unavailable == 0 {
		// Validation should never allow the user to explicitly use zero values for both maxSurge
		// maxUnavailable. Due to rounding down maxUnavailable though, it may resolve to zero.
		// If both fenceposts resolve to zero, then we should set maxUnavailable to 1 on the
		// theory that surge might not workf due to quota.
		unavailable = 1
	}

	return int32(surge), int32(unavailable), nil
}

// MaxUnavailable 最大不可用副本数
func MaxUnavailable(rollout *v1alpha1.Rollout) int32 {
	rolloutReplicas := defaults.GetReplicasOrDefault(rollout.Spec.Replicas)
	if rolloutReplicas == 0 {
		return int32(0)
	}

	// Error caught by validation
	var maxUnavailable int32
	if rollout.Spec.Strategy.Canary != nil {
		_, maxUnavailable, _ = resolveFencePosts(defaults.GetMaxSurgeOrDefault(rollout), defaults.GetMaxUnavailableOrDefault(rollout), rolloutReplicas)
	} else {
		unavailable, _ := intstrutil.GetValueFromIntOrPercent(intstrutil.ValueOrDefault(defaults.GetMaxUnavailableOrDefault(rollout), intstrutil.FromInt(0)), int(rolloutReplicas), false)
		maxUnavailable = int32(unavailable)
	}

	// 最大不可用不会超 Rollout 期望副本数
	if maxUnavailable > rolloutReplicas {
		return rolloutReplicas
	}
	return maxUnavailable
}

// MaxSurge 最大(相比Rolling预期副本数)多出来的副本数
func MaxSurge(rollout *v1alpha1.Rollout) int32 {
	rolloutReplicas := defaults.GetReplicasOrDefault(rollout.Spec.Replicas)
	if rollout.Spec.Strategy.Canary == nil {
		return int32(0)
	}
	// 默认 maxSurge 1 maxUnavailable 0
	maxSurge, _, _ := resolveFencePosts(defaults.GetMaxSurgeOrDefault(rollout), defaults.GetMaxUnavailableOrDefault(rollout), rolloutReplicas)
	return maxSurge
}

// checkStepHashChange indicates if the rollout's step for the strategy have changed. This causes the rollout to reset the
// currentStepIndex to zero. If there was no previously recorded step hash to compare to the function defaults to true
func checkStepHashChange(rollout *v1alpha1.Rollout) bool {
	if rollout.Status.CurrentStepHash == "" {
		return true
	}
	// TODO: conditions.ComputeStepHash is not stable and will change
	stepsHash := conditions.ComputeStepHash(rollout)
	if rollout.Status.CurrentStepHash != stepsHash {
		logCtx := logutil.WithRollout(rollout)
		logCtx.Infof("canary steps change detected (new: %s, old: %s)", stepsHash, rollout.Status.CurrentStepHash)
		return true
	}
	return false
}

// CheckPodSpecChange checkPodSpecChange indicates if the rollout spec has changed indicating that the rollout needs to reset the
// currentStepIndex to zero. If there is no previous pod spec to compare to the function defaults to false
func CheckPodSpecChange(rollout *v1alpha1.Rollout, newRS *appsv1.ReplicaSet) bool {
	if rollout.Status.CurrentPodHash == "" {
		return false
	}
	podHash := hash.ComputePodTemplateHash(&rollout.Spec.Template, rollout.Status.CollisionCount)
	if newRS != nil {
		podHash = GetPodTemplateHash(newRS)
	}
	if rollout.Status.CurrentPodHash != podHash {
		logCtx := logutil.WithRollout(rollout)
		logCtx.Infof("Pod template change detected (new: %s, old: %s)", podHash, rollout.Status.CurrentPodHash)
		return true
	}
	return false
}

// PodTemplateOrStepsChanged detects if there is a change in either the pod template, or canary steps
func PodTemplateOrStepsChanged(rollout *v1alpha1.Rollout, newRS *appsv1.ReplicaSet) bool {
	// step 发生变化
	if checkStepHashChange(rollout) {
		return true
	}
	// pod template 发生变化
	if CheckPodSpecChange(rollout, newRS) {
		return true
	}
	return false
}

// ResetCurrentStepIndex resets the index back to zero unless there are no steps
func ResetCurrentStepIndex(rollout *v1alpha1.Rollout) *int32 {
	if rollout.Spec.Strategy.Canary != nil && len(rollout.Spec.Strategy.Canary.Steps) > 0 {
		return pointer.Int32Ptr(0)
	}
	return nil
}

// PodTemplateEqualIgnoreHash returns true if two given podTemplateSpec are equal, ignoring the diff in value of Labels[pod-template-hash]
// We ignore pod-template-hash because:
//  1. The hash result would be different upon podTemplateSpec API changes
//     (e.g. the addition of a new field will cause the hash code to change)
//  2. The deployment template won't have hash labels
//
// NOTE: This is a modified version of deploymentutil.EqualIgnoreHash, but modified to perform
// defaulting on the desired spec. This is so that defaulted fields by the replicaset controller
// factor into the comparison. The reason this is necessary, is because unlike the deployment
// controller, the rollout controller does not benefit/operate on a completely defaulted
// rollout object.
func PodTemplateEqualIgnoreHash(live, desired *corev1.PodTemplateSpec) bool {
	live = live.DeepCopy()
	desired = desired.DeepCopy()
	// Remove hash labels from template.Labels before comparing
	delete(live.Labels, v1alpha1.DefaultRolloutUniqueLabelKey)
	delete(desired.Labels, v1alpha1.DefaultRolloutUniqueLabelKey)

	podTemplate := corev1.PodTemplate{
		Template: *desired,
	}
	corev1defaults.SetObjectDefaults_PodTemplate(&podTemplate)
	desired = &podTemplate.Template

	// Do not allow the deprecated spec.serviceAccount to factor into the equality check. In live
	// ReplicaSet pod template, this field will be populated, but in the desired pod template
	// it will be missing (even after defaulting), causing us to believe there is a diff
	// (when there really wasn't), and hence causing an unsolicited update to be triggered.
	// See: https://github.com/argoproj/argo-rollouts/issues/1356
	desired.Spec.DeprecatedServiceAccount = ""
	live.Spec.DeprecatedServiceAccount = ""

	return apiequality.Semantic.DeepEqual(live, desired)
}

// GetPodTemplateHash returns the rollouts-pod-template-hash value from a ReplicaSet's labels
func GetPodTemplateHash(rs *appsv1.ReplicaSet) string {
	if rs == nil || rs.Labels == nil {
		return ""
	}
	return rs.Labels[v1alpha1.DefaultRolloutUniqueLabelKey]
}

func GetReplicaSetRevision(ro *v1alpha1.Rollout, rs *appsv1.ReplicaSet) int {
	logCtx := logutil.WithRollout(ro).WithField("ReplicaSet", rs.Name)
	revisionStr, ok := rs.Annotations[annotations.RevisionAnnotation]
	if !ok {
		logCtx.Warn("ReplicaSet has no revision")
		return -1
	}
	revision, err := strconv.Atoi(revisionStr)
	if err != nil {
		logCtx.Warnf("Unable to convert ReplicaSet revision to int: %s", err.Error())
		return -1
	}
	return revision
}

// ReplicaSetsByRevisionNumber sorts a list of ReplicaSet by revision timestamp, using their creation timestamp as a tie breaker.
type ReplicaSetsByRevisionNumber []*appsv1.ReplicaSet

func (o ReplicaSetsByRevisionNumber) Len() int      { return len(o) }
func (o ReplicaSetsByRevisionNumber) Swap(i, j int) { o[i], o[j] = o[j], o[i] }
func (o ReplicaSetsByRevisionNumber) Less(i, j int) bool {
	iRevision, iErr := strconv.Atoi(o[i].Annotations[annotations.RevisionAnnotation])
	jRevision, jErr := strconv.Atoi(o[j].Annotations[annotations.RevisionAnnotation])
	// 两个都没 revision， 则按创建时间戳
	if iErr != nil && jErr != nil {
		return o[i].CreationTimestamp.Before(&o[j].CreationTimestamp)
	}

	// TODO 其中一个为空时，比索引，这？？？？什么骚操作。。
	if iErr != nil {
		return i > j
	}
	if jErr != nil {
		return i < j

	}
	// revision 相同时，比时间
	if iRevision == jRevision {
		return o[i].CreationTimestamp.Before(&o[j].CreationTimestamp)
	}
	return iRevision < jRevision
}

// IsStillReferenced returns if the given ReplicaSet is still being referenced by any of
// the current, stable, blue-green active references. Used to determine if the ReplicaSet can
// safely be scaled to zero, or deleted.
func IsStillReferenced(status v1alpha1.RolloutStatus, rs *appsv1.ReplicaSet) bool {
	h := GetPodTemplateHash(rs)
	if h != "" && (h == status.StableRS || h == status.CurrentPodHash) {
		return true
	}
	return false
}

// HasScaleDownDeadline returns whether or not the given ReplicaSet is annotated with a scale-down delay
func HasScaleDownDeadline(rs *appsv1.ReplicaSet) bool {
	if rs == nil || rs.Annotations == nil {
		return false
	}
	return rs.Annotations[v1alpha1.DefaultReplicaSetScaleDownDeadlineAnnotationKey] != ""
}

func GetTimeRemainingBeforeScaleDownDeadline(rs *appsv1.ReplicaSet) (*time.Duration, error) {
	if HasScaleDownDeadline(rs) {
		scaleDownAtStr := rs.Annotations[v1alpha1.DefaultReplicaSetScaleDownDeadlineAnnotationKey]
		scaleDownAtTime, err := time.Parse(time.RFC3339, scaleDownAtStr)
		if err != nil {
			return nil, fmt.Errorf("unable to read scaleDownAt label on rs '%s'", rs.Name)
		}
		now := timeutil.MetaNow()
		scaleDownAt := metav1.NewTime(scaleDownAtTime)
		if scaleDownAt.After(now.Time) {
			remainingTime := scaleDownAt.Sub(now.Time)
			return &remainingTime, nil
		}
	}
	return nil, nil
}

// GetPodsOwnedByReplicaSet returns a list of pods owned by the given replicaset
func GetPodsOwnedByReplicaSet(ctx context.Context, client kubernetes.Interface, rs *appsv1.ReplicaSet) ([]*corev1.Pod, error) {
	pods, err := client.CoreV1().Pods(rs.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: metav1.FormatLabelSelector(rs.Spec.Selector),
	})
	if err != nil {
		return nil, err
	}
	var podOwnedByRS []*corev1.Pod
	for i := range pods.Items {
		pod := pods.Items[i]
		if metav1.IsControlledBy(&pod, rs) {
			podOwnedByRS = append(podOwnedByRS, &pod)
		}
	}
	return podOwnedByRS, nil
}

// IsReplicaSetAvailable returns if a ReplicaSet is scaled up and its ready count is >= desired count
// 当前 RS 是否 ready
func IsReplicaSetAvailable(rs *appsv1.ReplicaSet) bool {
	if rs == nil {
		return false
	}
	replicas := rs.Spec.Replicas
	availableReplicas := rs.Status.AvailableReplicas
	return replicas != nil && *replicas != 0 && availableReplicas != 0 && *replicas <= availableReplicas
}
