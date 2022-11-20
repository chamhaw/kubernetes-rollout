package replicaset

import (
	"encoding/json"
	"github.com/chamhaw/kubernetes-rollout-api/v1alpha1"
	"github.com/chamhaw/kubernetes-rollout/utils/defaults"
	log "github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"math"
)

const (
	// EphemeralMetadataAnnotation denotes pod metadata which are ephemerally injected to canary/stable pods
	EphemeralMetadataAnnotation = "rollout-ephemeral-metadata"
)

func AllDesiredAreAvailable(rs *appsv1.ReplicaSet, desired int32) bool {
	if *rs.Spec.Replicas > desired {
		return true
	}
	return rs != nil && desired == *rs.Spec.Replicas && desired == rs.Status.AvailableReplicas
}

func allDesiredAreCreated(rs *appsv1.ReplicaSet, desired int32) bool {
	return rs != nil && desired <= *rs.Spec.Replicas && desired <= rs.Status.Replicas
}

func AtDesiredReplicaCountsForCanary(ro *v1alpha1.Rollout, newRS, stableRS *appsv1.ReplicaSet, otherRSs []*appsv1.ReplicaSet) bool {
	// 计算 副本数
	adjustedDesiredNewRSReplicaCount, desiredRSReplicaCount := CalculateReplicaCountsForBasicCanary(ro, newRS, stableRS, otherRSs)

	// 还没满足step要求
	if !allDesiredAreCreated(newRS, desiredRSReplicaCount) {
		log.Infof("The target replicas is: %d, current is: %d", desiredRSReplicaCount, newRS.Status.Replicas)
		return false
	}

	// 不满足available 要求
	if !AllDesiredAreAvailable(newRS, adjustedDesiredNewRSReplicaCount) {
		log.Infof("The target available replicas is: %d, current is: %d", adjustedDesiredNewRSReplicaCount, newRS.Status.AvailableReplicas)
		return false
	}
	return true
}

func CalculateReplicaCountsForBasicCanary(rollout *v1alpha1.Rollout, newRS *appsv1.ReplicaSet, stableRS *appsv1.ReplicaSet, otherRSs []*appsv1.ReplicaSet) (int32, int32) {
	// rollout 最终期望的 replicas
	rolloutSpecReplica := defaults.GetReplicasOrDefault(rollout.Spec.Replicas)

	// 期望的weight，rollout中记录的当前step往前找，包含当前step
	_, desiredWeight := GetCanaryReplicasOrWeight(rollout)

	// 解析 maxSurge
	maxSurge := MaxSurge(rollout)

	// 根据Surge，并结合了 weight 综合计算，找到新老RS期望调整到的副本数(最近似的数值)。四舍五入
	desiredNewRSReplicaCount := approximateWeightedRolloutStableReplicaCounts(rolloutSpecReplica, desiredWeight, maxSurge)

	// 新RS当前配置的副本数
	newRSReplicaCount := int32(0)
	if newRS != nil {
		newRSReplicaCount = *newRS.Spec.Replicas
	}

	// 是否存在不同于新RS的stableRS， 只比对名称
	existingStable := CheckStableRSExists(newRS, stableRS)
	// 最大允许的pod数 = rollout期望数 + maxSurge
	maxReplicaCountAllowed := rolloutSpecReplica + maxSurge

	// 新老其他RS三者合起来是所有RS
	allRSs := append(otherRSs, newRS)
	olderRSs := otherRSs
	if existingStable {
		allRSs = append(allRSs, stableRS)
		olderRSs = append(otherRSs, stableRS)
	}

	// 拿到当前所有RS的总pod数
	totalCurrentReplicaCount := GetReplicaCountForReplicaSets(allRSs)
	scaleUpCount := maxReplicaCountAllowed - totalCurrentReplicaCount

	// 算出 新RS的新副本数
	if newRS != nil && *newRS.Spec.Replicas < desiredNewRSReplicaCount && scaleUpCount > 0 {
		// This follows the same logic as scaling up the stable except with the newRS and it does not need to
		// set the scaleDownCount again since it's not used again
		if *newRS.Spec.Replicas+scaleUpCount < desiredNewRSReplicaCount {
			newRSReplicaCount = *newRS.Spec.Replicas + scaleUpCount
		} else {
			newRSReplicaCount = desiredNewRSReplicaCount
		}
	}

	if newRSReplicaCount > *rollout.Spec.Replicas {
		log.Infof("Since the newRSReplicaCount %d is greater than rollout spec replicas %d, adjust to spec replicas.", newRSReplicaCount, *rollout.Spec.Replicas)
	}

	// 用最大不可用数计算出最少可用数
	minAvailableReplicaCount := rolloutSpecReplica - MaxUnavailable(rollout)

	// stable = new 的情况，可能是缩扩容
	if (stableRS != nil && stableRS.Name == newRS.Name) || existingStable {
		// 根据 Surge 和 Unavailable 修正，不考虑 weight
		newRSReplicaCount = adjustReplicaWithinLimits(olderRSs, newRS, newRSReplicaCount, maxReplicaCountAllowed, minAvailableReplicaCount)
	}
	return newRSReplicaCount, desiredNewRSReplicaCount
}

func adjustReplicaWithinLimits(olderRS []*appsv1.ReplicaSet, newRS *appsv1.ReplicaSet, newRSDesiredCount int32, maxReplicaCountAllowed, minAvailableReplicaCount int32) int32 {
	// 如果计算出的RS副本数 + 忽略可用性的副本数之和比最大允许的副本数大，则记录下具体大了多少，要缩回来
	olderCount := GetReplicaCountForReplicaSets(olderRS)
	// 如果超了maxSurge，减下来
	overTheLimit := maxValue(0, olderCount+newRSDesiredCount-maxReplicaCountAllowed)
	newRSDesiredCount -= overTheLimit

	// 如果available 不够，要补上来
	lessTheLimitVal := minValue(0, GetAvailableReplicaCountForReplicaSets(olderRS)+newRS.Status.AvailableReplicas-minAvailableReplicaCount)
	newRSDesiredCount -= lessTheLimitVal

	return newRSDesiredCount
}

func minValue(countA int32, countB int32) int32 {
	if countA > countB {
		return countB
	}
	return countA
}

func maxValue(countA int32, countB int32) int32 {
	if countA < countB {
		return countB
	}
	return countA
}

func max(left, right int32) int32 {
	if left > right {
		return left
	}
	return right
}

// CheckStableRSExists checks if the stableRS exists and is different than the newRS
func CheckStableRSExists(newRS, stableRS *appsv1.ReplicaSet) bool {
	if stableRS == nil {
		return false
	}
	if newRS == nil {
		return true
	}

	// newRS == stable RS的情况下，认为stable不存在，这种做法，会导致回滚有问题
	if newRS.Name == stableRS.Name {
		return false
	}
	return true
}

// GetCurrentCanaryStep returns the current canary step. If there are no steps or the rollout
// has already executed the last step, the func returns nil
func GetCurrentCanaryStep(rollout *v1alpha1.Rollout) (*v1alpha1.CanaryStep, *int32) {
	if rollout.Spec.Strategy.Canary == nil || len(rollout.Spec.Strategy.Canary.Steps) == 0 {
		return nil, nil
	}
	currentStepIndex := int32(0)
	// 如果当前 step 为空，则从第一步开始
	if rollout.Status.CurrentStepIndex != nil {
		currentStepIndex = *rollout.Status.CurrentStepIndex
	}
	// 如果当前 step 索引大于或等于 总 step 数，则返回当前 nil
	if len(rollout.Spec.Strategy.Canary.Steps) <= int(currentStepIndex) {
		return nil, &currentStepIndex
	}
	return &rollout.Spec.Strategy.Canary.Steps[currentStepIndex], &currentStepIndex
}

// GetOtherRSs the function goes through a list of ReplicaSets and returns a list of RS that are not the new or stable RS
func GetOtherRSs(rollout *v1alpha1.Rollout, newRS, stableRS *appsv1.ReplicaSet, allRSs []*appsv1.ReplicaSet) []*appsv1.ReplicaSet {
	var otherRSs []*appsv1.ReplicaSet
	for _, rs := range allRSs {
		if rs != nil {
			if stableRS != nil && rs.Name == stableRS.Name {
				continue
			}
			if newRS != nil && rs.Name == newRS.Name {
				continue
			}
			otherRSs = append(otherRSs, rs)
		}
	}
	return otherRSs
}

// GetStableRS finds the stable RS using the RS's RolloutUniqueLabelKey and the stored StableRS in the rollout status
func GetStableRS(rollout *v1alpha1.Rollout, newRS *appsv1.ReplicaSet, rslist []*appsv1.ReplicaSet) *appsv1.ReplicaSet {
	if rollout.Status.StableRS == "" {
		return nil
	}
	// 新的和老的一样，则认为新的就是 stableRS
	if newRS != nil && newRS.Labels != nil && newRS.Labels[v1alpha1.DefaultRolloutUniqueLabelKey] == rollout.Status.StableRS {
		return newRS
	}
	for i := range rslist {
		rs := rslist[i]
		if rs != nil {
			if rs.Labels[v1alpha1.DefaultRolloutUniqueLabelKey] == rollout.Status.StableRS {
				return rs
			}
		}
	}
	return nil
}

// ParseExistingPodMetadata returns the existing podMetadata which was injected to the ReplicaSet
// based on examination of rollout.argoproj.io/ephemeral-metadata annotation on  the ReplicaSet.
// Returns nil if there was no metadata, or the metadata was not parseable.
func ParseExistingPodMetadata(rs *appsv1.ReplicaSet) *v1alpha1.PodTemplateMetadata {
	var existingPodMetadata *v1alpha1.PodTemplateMetadata
	if rs.Annotations != nil {
		if existingPodMetadataStr, ok := rs.Annotations[EphemeralMetadataAnnotation]; ok {
			err := json.Unmarshal([]byte(existingPodMetadataStr), &existingPodMetadata)
			if err != nil {
				log.Warnf("Failed to determine existing ephemeral metadata from annotation: %s", existingPodMetadataStr)
				return nil
			}
			return existingPodMetadata
		}
	}
	return nil
}

// SyncEphemeralPodMetadata will inject the desired pod metadata to the ObjectMeta as well as remove
// previously injected pod metadata which is no longer desired. This function is careful to only
// modify metadata that we injected previously, and not affect other metadata which might be
// controlled by other controllers (e.g. istio pod sidecar injector)
func SyncEphemeralPodMetadata(metadata *metav1.ObjectMeta, existingPodMetadata, desiredPodMetadata *v1alpha1.PodTemplateMetadata) (*metav1.ObjectMeta, bool) {
	modified := false
	metadata = metadata.DeepCopy()

	// Inject the desired metadata
	if desiredPodMetadata != nil {
		for k, v := range desiredPodMetadata.Annotations {
			if metadata.Annotations == nil {
				metadata.Annotations = make(map[string]string)
			}
			if prev := metadata.Annotations[k]; prev != v {
				metadata.Annotations[k] = v
				modified = true
			}
		}
		for k, v := range desiredPodMetadata.Labels {
			if metadata.Labels == nil {
				metadata.Labels = make(map[string]string)
			}
			if prev := metadata.Labels[k]; prev != v {
				metadata.Labels[k] = v
				modified = true
			}
		}
	}

	isMetadataStillDesired := func(key string, desired map[string]string) bool {
		_, ok := desired[key]
		return ok
	}
	// Remove existing metadata which is no longer desired
	if existingPodMetadata != nil {
		for k := range existingPodMetadata.Annotations {
			if desiredPodMetadata == nil || !isMetadataStillDesired(k, desiredPodMetadata.Annotations) {
				if metadata.Annotations != nil {
					delete(metadata.Annotations, k)
					modified = true
				}
			}
		}
		for k := range existingPodMetadata.Labels {
			if desiredPodMetadata == nil || !isMetadataStillDesired(k, desiredPodMetadata.Labels) {
				if metadata.Labels != nil {
					delete(metadata.Labels, k)
					modified = true
				}
			}
		}
	}
	return metadata, modified
}

// SyncReplicaSetEphemeralPodMetadata injects the desired pod metadata to the ReplicaSet, and
// removes previously injected metadata (based on the rollout.argoproj.io/ephemeral-metadata
// annotation) if it is no longer desired. A podMetadata value of nil indicates all ephemeral
// metadata should be removed completely.
func SyncReplicaSetEphemeralPodMetadata(rs *appsv1.ReplicaSet, podMetadata *v1alpha1.PodTemplateMetadata) (*appsv1.ReplicaSet, bool) {
	existingPodMetadata := ParseExistingPodMetadata(rs)
	newObjectMeta, modified := SyncEphemeralPodMetadata(&rs.Spec.Template.ObjectMeta, existingPodMetadata, podMetadata)
	rs = rs.DeepCopy()
	if !modified {
		return rs, false
	}
	rs.Spec.Template.ObjectMeta = *newObjectMeta
	if podMetadata != nil {
		// remember what we injected by annotating it
		metadataBytes, _ := json.Marshal(podMetadata)
		if rs.Annotations == nil {
			rs.Annotations = make(map[string]string)
		}
		rs.Annotations[EphemeralMetadataAnnotation] = string(metadataBytes)
	} else {
		delete(rs.Annotations, EphemeralMetadataAnnotation)
	}
	return rs, true
}

// 根据weight计算新RS和stableRS的近似副本数， 新老副本数的和不会超过 Rollout 期望副本数 + 1。
// 如果 weight 不是100，并且 Rollout 副本数大于1的话，新老RS副本数都会至少是1。
func approximateWeightedRolloutStableReplicaCounts(specReplicas, desiredWeight, maxSurge int32) int32 {
	if specReplicas == 0 {
		return 0
	}
	// rolloutOption 用于记录可能的返回值
	type rolloutOption struct {
		newRS int32
		total int32
	}
	var options []rolloutOption

	// 向上取整的新RS副本数
	ceilWeightedNewRSCount := int32(math.Ceil(float64(specReplicas*desiredWeight) / 100.0))
	// 向下取整的新RS副本数
	floorWeightedNewRSCount := int32(math.Floor(float64(specReplicas*desiredWeight) / 100.0))

	// 小数部分是否等于 0.5 四舍五入
	tied := floorCeilingTied(desiredWeight, specReplicas)

	// zeroAllowed indicates if are allowed to return the floored value if it is zero. We don't allow
	// the value to be zero if when user has a weight from 1-99, and they run 2+ replicas (surge included)
	zeroAllowed := desiredWeight == 100 || desiredWeight == 0 || (specReplicas == 1 && maxSurge == 0)

	// 如果向上取整的新RS副本数比 Rollout 副本数小(还没滚动完成) 或者 允许副本数为0的情况，新RS副本数取向上取整，总数取 Rollout 定义
	if ceilWeightedNewRSCount < specReplicas || zeroAllowed {
		options = append(options, rolloutOption{ceilWeightedNewRSCount, specReplicas})
	}
	{
		// TODO  这段代码里，有两次判断 副本数 乘以 weight 百分比 的小数部分是否为 0.5，这时有可能多一个向下取整的选项, 该判断暂未搞明白原理

		// 如果小数部分不等于0.5, 并且 向下取整不等于0 或允许副本数为0的情况下，再加一条候选项，新RS副本数向下取整，总数取rollout定义
		if !tied && (floorWeightedNewRSCount != 0 || zeroAllowed) {
			options = append(options, rolloutOption{floorWeightedNewRSCount, specReplicas})
		}

		// 如果允许 Surge，则允许向上取整到 rollout 定义 + 1
		if maxSurge > 0 {
			options = append(options, rolloutOption{ceilWeightedNewRSCount, specReplicas + 1})

			// 如果按 rollout定义 + 1作为总数取算 weight，小数部分是否为0.5
			surgeIsTied := floorCeilingTied(desiredWeight, specReplicas+1)
			// 如果不是0.5并且 向下取整不为零(回滚快完成时，或者刚开始发时)或允许0值的情况，则再加一条候选项，新RS向下取整， 总数按rollout + 1
			if !surgeIsTied && (floorWeightedNewRSCount != 0 || zeroAllowed) {
				options = append(options, rolloutOption{floorWeightedNewRSCount, specReplicas + 1})
			}
		}
	}

	if len(options) == 0 {
		// should not get here
		return 0
	}

	bestOption := options[0]
	// 实际weight -  预期weight，就是要调整的比例。
	// 遍历 options，找最佳选项 （谁当前weight距离预期weight最近，谁就是最佳选项）
	bestDelta := weightDelta(desiredWeight, bestOption.newRS, bestOption.total)
	// 这里找了最大的
	//maxTotal := bestOption.total
	for i := 1; i < len(options); i++ {
		currOption := options[i]
		currDelta := weightDelta(desiredWeight, currOption.newRS, currOption.total)
		//maxTotal = int32(math.Max(float64(maxTotal), float64(currOption.total)))
		if currDelta < bestDelta {
			bestOption = currOption
			bestDelta = currDelta
		}
	}

	return bestOption.newRS
}

// GetCanaryReplicasOrWeight either returns a static set of replicas or a weight percentage
func GetCanaryReplicasOrWeight(rollout *v1alpha1.Rollout) (*int32, int32) {
	if scs := UseSetCanaryScale(rollout); scs != nil {
		if scs.Replicas != nil {
			return scs.Replicas, 0
		} else if scs.Weight != nil {
			return nil, *scs.Weight
		}
	}
	//|| rollout.Status.StableRS == "" || rollout.Status.CurrentPodHash == rollout.Status.StableRS
	return nil, GetCurrentSetWeight(rollout)
}

func UseSetCanaryScale(rollout *v1alpha1.Rollout) *v1alpha1.SetCanaryScale {
	if rollout.Spec.Strategy.Canary == nil {
		// SetCanaryScale only works with TrafficRouting
		return nil
	}

	currentStep, currentStepIndex := GetCurrentCanaryStep(rollout)
	if currentStep == nil {
		// setCanaryScale feature is unused
		return nil
	}
	for i := *currentStepIndex; i >= 0; i-- {
		step := rollout.Spec.Strategy.Canary.Steps[i]
		if step.SetCanaryScale == nil {
			continue
		}

		return step.SetCanaryScale
	}
	return nil
}

// GetCurrentSetWeight grabs the current setWeight used by the rollout by iterating backwards from the current step
// until it finds a setWeight step. The controller defaults to 100 if it iterates through all the steps with no
// setWeight or if there is no current step (i.e. the controller has already stepped through all the steps).
func GetCurrentSetWeight(rollout *v1alpha1.Rollout) int32 {
	currentStep, currentStepIndex := GetCurrentCanaryStep(rollout)
	// 如果走完了，全部拉到 100
	if currentStep == nil {
		return 100
	}

	// 从当前step倒着找，找第一个 SetWeight 不为空的step
	for i := *currentStepIndex; i >= 0; i-- {
		step := rollout.Spec.Strategy.Canary.Steps[i]
		if step.SetWeight != nil {
			return *step.SetWeight
		}
	}
	return 0
}

// floorCeilingTied indicates if the ceiling and floor values are equidistant from the desired weight
// For example: replicas: 3, desiredWeight: 50%
// A newRS count of 1 (33.33%) or 2 (66.66%) are both equidistant from desired weight of 50%.
// When this happens, we will pick the larger newRS count
// 3 * 0.5 = 1.5, 1或2， 取大的（向上取整？）
func floorCeilingTied(desiredWeight, totalReplicas int32) bool {
	// 分别取整数和小数部分，判断小数部分是否等于 0.5
	_, frac := math.Modf(float64(totalReplicas) * (float64(desiredWeight) / 100))
	return frac == 0.5
}

// weightDelta calculates the difference that the new RS replicas will be from the desired weight
// This is used to pick the closest approximation of new RS counts.
func weightDelta(desiredWeight, newRSReplicas, totalReplicas int32) float64 {
	actualWeight := float64(newRSReplicas*100) / float64(totalReplicas)
	return math.Abs(actualWeight - float64(desiredWeight))
}
