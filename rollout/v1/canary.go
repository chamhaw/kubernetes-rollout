package v1

import (
	"github.com/chamhaw/kubernetes-rollout/api/v1alpha1"
	"sort"

	"github.com/chamhaw/kubernetes-rollout/rollout/utils/conditions"
	"github.com/chamhaw/kubernetes-rollout/rollout/utils/defaults"
	replicasetutil "github.com/chamhaw/kubernetes-rollout/rollout/utils/replicaset"
	rolloututil "github.com/chamhaw/kubernetes-rollout/rollout/utils/rollout"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (c *rolloutContext) canary() error {
	var err error

	// pod template 或 step 发生变化，意味着是一次新的发布， 直接去维护状态了，不创建，原因？
	if replicasetutil.PodTemplateOrStepsChanged(c.rollout, c.newRS) {
		c.newRS, err = c.getNewReplicaSetAndSyncRevision(false)
		if err != nil {
			return err
		}
		return c.tryToCompleteStepAndSyncRolloutStatusCanary()
	}

	// template 和 steps 没有变化的情况，一般是中间状态，或者回滚
	// 创建新的 RS，并且更新 revision 等操作， 此时 RS replicas 是 0
	c.newRS, err = c.getNewReplicaSetAndSyncRevision(true)
	if err != nil {
		return err
	}

	// 打一些 rollout 带过来的 metadata label 和 annotation到 RS po template 中
	err = c.reconcileEphemeralMetadata()
	if err != nil {
		return err
	}

	// 处理保留 RS 个数的问题，不涉及 pod 本身
	if err = c.reconcileRevisionHistoryLimit(c.otherRSs); err != nil {
		return err
	}

	//先处理暂停，暂停优先
	paused := c.reconcileCanaryPause()
	if paused {
		// 暂停
		c.log.Infof("Not finished reconciling canary Pause")
		return c.tryToCompleteStepAndSyncRolloutStatusCanary()
	}
	// 检测到 pause，
	if c.pauseContext.HasAddPause() {
		c.log.Info("Detected pause, stop recoiling")
		return c.tryToCompleteStepAndSyncRolloutStatusCanary()
	}

	// 处理新老 RS 滚动
	noScalingOccurred, err := c.reconcileCanaryReplicaSets()
	if err != nil {
		return err
	}
	// 没有伸缩发生，结束
	if noScalingOccurred {
		c.log.Info("Not finished reconciling ReplicaSets")
		return c.tryToCompleteStepAndSyncRolloutStatusCanary()
	}

	return c.tryToCompleteStepAndSyncRolloutStatusCanary()
}

// 处理 stable RS 副本数
func (c *rolloutContext) reconcileCanaryStableReplicaSet() (bool, error) {
	// 检测 stable RS 是否存在，不存在或者和新RS一样 则结束
	if !replicasetutil.CheckStableRSExists(c.newRS, c.stableRS) {
		c.log.Info("No StableRS exists to reconcile or matches newRS")
		return false, nil
	}
	var desiredStableRSReplicaCount int32

	// 计算期望的 stable RS 的副本数
	_, desiredStableRSReplicaCount = replicasetutil.CalculateReplicaCountsForBasicCanary(c.rollout, c.newRS, c.stableRS, c.otherRSs)

	// 新老name相同时，表示在回滚
	if c.newRS != nil && c.newRS.Name != c.stableRS.Name && desiredStableRSReplicaCount > *c.stableRS.Spec.Replicas {
		c.log.Infof("stable replicas count cannot be scaled up. RS: %s, current:%d, desired:%d", c.stableRS.Name, *c.stableRS.Spec.Replicas, desiredStableRSReplicaCount)
		return false, nil
	}

	c.log.Infof("Scale stable RS[%s] from %d to %d", c.stableRS.Name, *c.stableRS.Spec.Replicas, desiredStableRSReplicaCount)
	// 伸缩 stable RS
	scaled, _, err := c.scaleReplicaSet(c.stableRS, desiredStableRSReplicaCount)
	return scaled, err
}

func (c *rolloutContext) reconcileCanaryPause() bool {
	c.log.Infof("Starting to reconcile Pause")
	// spec 已经是 paused
	if c.rollout.Spec.Paused {
		return false
	}

	totalSteps := len(c.rollout.Spec.Strategy.Canary.Steps)
	if totalSteps == 0 {
		c.log.Info("Rollout does not have any steps")
		return false
	}
	// 找当前 step
	currentStep, currentStepIndex := replicasetutil.GetCurrentCanaryStep(c.rollout)
	// step 走完了
	if totalSteps <= int(*currentStepIndex) {
		c.log.Info("No Steps remain in the canary steps")
		return false
	}

	c.log.Infof("Current step in pause reconciling is: %s", rolloututil.CanaryStepString(*currentStep))

	// 当前 step 不是 pause，跳过
	if currentStep.Pause == nil {
		return false
	}
	c.log.Infof("Reconciling canary pause step (stepIndex: %d/%d)", *currentStepIndex, totalSteps)

	// 当前 step 需要暂停
	cond := getPauseCondition(c.rollout, v1alpha1.PauseReasonCanaryPauseStep)
	if cond == nil {
		// 如果不是被系统 pause，添加 pause condition
		if !c.rollout.Status.ControllerPause {
			c.pauseContext.AddPauseCondition(v1alpha1.PauseReasonCanaryPauseStep)
		}
		return true
	}
	if currentStep.Pause.Duration == nil {
		return true
	}
	// 马上到期的 pause 丢进延时队列
	c.checkEnqueueRolloutDuringWait(cond.StartTime, currentStep.Pause.DurationSeconds())
	return true
}

// scaleDownOldReplicaSetsForCanary scales down old replica sets when rollout strategy is "CanaryStrategy".
// 杀老 rs
// allRSs - newRS - stableRS
func (c *rolloutContext) scaleDownOldReplicaSetsForCanary(stableRS *appsv1.ReplicaSet, otherRS []*appsv1.ReplicaSet) (int32, error) {
	// 先杀 unhealthy 的 pod，防止影响总副本数的控制和timeout
	otherRS, totalScaledDown, err := c.cleanupUnhealthyReplicas(otherRS)
	if err != nil {
		return totalScaledDown, nil
	}
	// 找到所有 healthy 的 pod 数
	availablePodCount := replicasetutil.GetAvailableReplicaCountForReplicaSets(c.allRSs)
	// 计算得到 最少要保证可用的 pod 数
	minAvailable := defaults.GetReplicasOrDefault(c.rollout.Spec.Replicas) - replicasetutil.MaxUnavailable(c.rollout)
	// 计算最多可以杀几个 pod
	maxScaleDown := availablePodCount - minAvailable
	// 可用 pod 不够的情况，没有可杀的了 结束. 例外情况，没有stable，说明是第一次发，这时只管新的按批次，老的一把杀掉
	if maxScaleDown <= 0 {
		if !replicasetutil.CheckStableRSExists(c.newRS, c.stableRS) {
			// Cannot scale down.
			return 0, nil
		}
		// 如果 available已经不够了，这时停止杀other，留给stable和newRS去扩容，下个loop再尝试杀other
		return 0, nil
	}
	c.log.Infof("Found %d available pods, scaling down old RSes (minAvailable: %d, maxScaleDown: %d)", availablePodCount, minAvailable, maxScaleDown)

	// 这里是按 revision 倒序排序，也就意味着后面发的在前面，优先被杀掉  -- 别的地方的排序是按 createTime
	sort.Sort(sort.Reverse(replicasetutil.ReplicaSetsByRevisionNumber(otherRS)))

	// 确保stableRS在最后
	olderRS := append(otherRS, stableRS)

	// 遍历老的 rs，缩容
	for _, targetRS := range olderRS {
		if maxScaleDown <= 0 {
			break
		}
		if *targetRS.Spec.Replicas == 0 {
			// cannot scale down this ReplicaSet.
			continue
		}
		desiredReplicaCount := int32(0)
		// 如果 预期副本数 大于 可杀的最大副本数，那么新的预期副本数就是两者的差值
		if *targetRS.Spec.Replicas > maxScaleDown {
			desiredReplicaCount = *targetRS.Spec.Replicas - maxScaleDown
		}

		if *targetRS.Spec.Replicas == desiredReplicaCount {
			// already at desired account, nothing to do
			continue
		}
		// Scale down.
		c.log.Infof("Scale down other RS[%s] from %d to %d", targetRS.Name, *targetRS.Spec.Replicas, desiredReplicaCount)
		_, _, err = c.scaleReplicaSet(targetRS, desiredReplicaCount)
		if err != nil {
			return totalScaledDown, err
		}
		// 缩了副本数
		scaleDownCount := *targetRS.Spec.Replicas - desiredReplicaCount
		// 标志位，用于避免额外的遍历
		maxScaleDown -= scaleDownCount
		totalScaledDown += scaleDownCount
	}

	return totalScaledDown, nil
}

// 检查当前 step
func (c *rolloutContext) completedCurrentCanaryStep() bool {
	// 当前rollout spec被配置为了 pause(用户暂停),停留下来
	if c.rollout.Spec.Paused {
		return false
	}

	// 获取当前 step
	currentStep, _ := replicasetutil.GetCurrentCanaryStep(c.rollout)
	// 当前 step 为空，则认为要么是没有 step，或者已经执行完成
	if currentStep == nil {
		return false
	}
	c.log.Infof("current step: %s", rolloututil.CanaryStepString(*currentStep))
	// 找到当前 step， 开始执行
	// step 分为几种
	switch {
	case currentStep.Pause != nil:
		// 完成 pause 类型的 step
		_, desiredNewRSReplicaCount := replicasetutil.CalculateReplicaCountsForBasicCanary(c.rollout, c.newRS, c.stableRS, c.otherRSs)
		// 全部满足rollout要求 或 当前数量大于了期望数量，这时跳过暂停。 注意， 等于条件不成立，因为pause处于等于这个阶段
		if replicasetutil.AllDesiredAreAvailable(c.newRS, *c.rollout.Spec.Replicas) || *c.newRS.Spec.Replicas > desiredNewRSReplicaCount {
			return true
		}
		completePause := c.pauseContext.CompletedCanaryPauseStep(*currentStep.Pause)
		//if !completePause && currentStep.Pause.Duration == nil {
		//	// 遇到无限暂停的 step，则把 spec pause 改掉
		//	c.rollout.Spec.Paused = true
		//}
		return completePause
		// 执行 scale 情况的检查
	case currentStep.SetCanaryScale != nil:
	case currentStep.SetWeight != nil:
		return replicasetutil.AtDesiredReplicaCountsForCanary(c.rollout, c.newRS, c.stableRS, c.otherRSs)
	}
	return false
}

// tryToCompleteStepAndSyncRolloutStatusCanary 更新 rollout 状态, 在这里完成了 complete当前step的判断和索引的维护
func (c *rolloutContext) tryToCompleteStepAndSyncRolloutStatusCanary() error {
	// 基本状态
	newStatus := c.calculateBaseStatus()
	newStatus.AvailableReplicas = replicasetutil.GetAvailableReplicaCountForReplicaSets(c.allRSs)
	// 实际副本数
	newStatus.HPAReplicas = replicasetutil.GetActualReplicaCountForReplicaSets(c.allRSs)

	// k8s Label Selector 变成 string 描述
	newStatus.Selector = metav1.FormatLabelSelector(c.rollout.Spec.Selector)

	currentStep, currentStepIndex := replicasetutil.GetCurrentCanaryStep(c.rollout)
	newStatus.StableRS = c.rollout.Status.StableRS
	newStatus.CurrentStepHash = conditions.ComputeStepHash(c.rollout)
	newStatus.Phase = c.rollout.Status.Phase
	newStatus.Message = c.rollout.Status.Message
	stepCount := int32(len(c.rollout.Spec.Strategy.Canary.Steps))

	// template 或 step 发生变更
	if replicasetutil.PodTemplateOrStepsChanged(c.rollout, c.newRS) {
		// 重置状态
		c.resetRolloutStatus(&newStatus)
		// 老 rs 和新 RS 相等，则认为要回滚到 stable
		if c.newRS != nil && c.rollout.Status.StableRS == replicasetutil.GetPodTemplateHash(c.newRS) {
			if stepCount > 0 {
				// If we get here, we detected that we've moved back to the stable ReplicaSet
				//c.recorder.Eventf(c.rollout, record.EventOptions{EventReason: "SkipSteps"}, "Rollback to stable")
				// 当前 step 拉到最后一步 + 1， 越界，不合法的 step
				newStatus.CurrentStepIndex = &stepCount
			}
		}

		// 计算 condition
		newStatus = c.calculateRolloutConditions(newStatus)

		// 持久化 status  存状态，结束
		return c.persistRolloutStatus(&newStatus)
	}

	// 如果新RS就绪副本数大于等于rollout 副本数或 step走完了
	if reason := c.shouldFullPromote(newStatus); reason != "" {
		// 如果需要 full promote， promote stable
		err := c.promoteStable(&newStatus, reason)
		if err != nil {
			return err
		}
		// 计算condition
		newStatus = c.calculateRolloutConditions(newStatus)
		// 存状态，结束
		return c.persistRolloutStatus(&newStatus)
	}

	// TODO 处理 回滚

	// 当前 step 已完成, 前进一步
	if c.completedCurrentCanaryStep() {
		c.log.Infof("current step finished: %s", rolloututil.CanaryStepString(*currentStep))
		*currentStepIndex++
		c.log.Infof("next step: %d", *currentStepIndex)
		// 如果当前 step 是 pause step，当前 step 完成后，需要移除pause condition
		c.pauseContext.ClearPauseConditions()
	}

	// step 往前一步
	newStatus.CurrentStepIndex = currentStepIndex
	// 计算 rollout condition
	newStatus = c.calculateRolloutConditions(newStatus)

	// 存状态 结束
	return c.persistRolloutStatus(&newStatus)
}

// 协调 新老其他RS的数量
func (c *rolloutContext) reconcileCanaryReplicaSets() (bool, error) {
	// 检测是否暂停
	if haltReason := c.haltProgress(); haltReason != "" {
		c.log.Infof("Skipping canary ReplicaSet reconciliation: %s", haltReason)
		return false, nil
	}

	// 伸缩新 RS
	scaledNewRS, err := c.reconcileNewReplicaSet()
	if err != nil {
		return false, err
	}
	if scaledNewRS {
		c.log.Infof("Not finished reconciling new ReplicaSet '%s'", c.newRS.Name)
		return true, nil
	}

	// 伸缩老的 RS， 包含 (c.allRSs - c.newRS)
	scaledDown, err := c.reconcileOldReplicaSets()
	if err != nil {
		return false, err
	}
	if scaledDown {
		c.log.Info("Not finished reconciling old ReplicaSets")
		return true, nil
	}
	return false, nil
}
