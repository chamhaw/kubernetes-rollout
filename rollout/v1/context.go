package v1

import (
	"github.com/chamhaw/kubernetes-rollout/api/v1alpha1"
	log "github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"time"
)

type rolloutContext struct {
	reconcilerBase

	log *log.Entry
	// rollout 是原始的 rollout
	rollout *v1alpha1.Rollout
	// newRS 新 RS，可以指向当前 RS 或期望的 RS
	// newRS pod template spec变化时，newRS 是 nil
	newRS *appsv1.ReplicaSet
	// stableRS  用来回滚， 第一次部署时是 nil， fully promote时，等于 newRS
	stableRS *appsv1.ReplicaSet
	// allRSs = (newRS + olderRSs) = (newRS + stableRS + otherRSs)
	allRSs []*appsv1.ReplicaSet
	// olderRSs = (allRSs - newRS)
	olderRSs []*appsv1.ReplicaSet
	// otherRSs = (allRSs - newRS - stableRS)
	otherRSs []*appsv1.ReplicaSet

	// 用于运行时保存rollout状态，最终存储该status
	newStatus v1alpha1.RolloutStatus

	pauseContext *pauseContext
}

func (c *rolloutContext) reconcile() error {
	// 校验
	err := c.getRolloutValidationErrors()
	if err != nil {
		if vErr, ok := err.(*field.Error); ok {
			// We want to frequently requeue rollouts with InvalidSpec errors, because the error
			// condition might be timing related (e.g. the Rollout was applied before the Service).
			c.enqueueRolloutAfter(c.rollout, 20*time.Second)
			return c.createInvalidRolloutCondition(vErr, c.rollout)
		}
		return err
	}

	// 修改 Rollout PausedCondition， err说明修改失败。 check 不会出错
	err = c.checkPausedConditions()
	if err != nil {
		return err
	}

	// 目前我们只支持滚动分批发布
	return c.canary()
}

// haltProgress returns a reason on whether we should halt all progress with an update
// to ReplicaSet counts (e.g. due to canary steps or blue-green promotion). This is either because
// user explicitly paused the rollout by setting `spec.paused`, or the analysis was inconclusive
func (c *rolloutContext) haltProgress() string {
	if c.rollout.Spec.Paused {
		return "user paused or met an infinity pause step"
	}
	if getPauseCondition(c.rollout, v1alpha1.PauseReasonInconclusiveAnalysis) != nil {
		return "inconclusive analysis"
	}
	return ""
}
