package v1

import (
	"context"
	"fmt"
	"github.com/chamhaw/kubernetes-rollout-api/v1alpha1"
	"github.com/chamhaw/kubernetes-rollout/utils/defaults"
	log "github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/kubernetes/pkg/controller"
	"sort"

	replicasetutil "github.com/chamhaw/kubernetes-rollout/utils/replicaset"
)

// 用rollout 筛选 RS
func (c *Controller) getReplicaSetsForRollout(rollout *v1alpha1.Rollout) (rsList []*appsv1.ReplicaSet, err error) {

	rss, err := replicasetutil.GetReplicaSetsOwnedByRollout(context.TODO(), c.kubeclientset, rollout)
	if err != nil {
		return nil, err
	}
	for _, rs := range rss {
		rsList = append(rsList, rs.DeepCopy())
	}
	return
}

func (c *rolloutContext) reconcileNewReplicaSet() (bool, error) {
	if c.newRS == nil {
		return false, nil
	}
	var newReplicasCount int32
	var err error
	// 如果newRS 的副本数比 rollout 还多，需要缩到rollout要求的数量，再进行计算
	rolloutCount := defaults.GetReplicasOrDefault(c.rollout.Spec.Replicas)
	if *c.newRS.Spec.Replicas > rolloutCount {
		log.Warningf("New ReplicaSet size is greater than rollout. scale down to rollout size firstly: %d", rolloutCount)
		newReplicasCount = rolloutCount
	} else if replicasetutil.AllDesiredAreAvailable(c.newRS, rolloutCount) {
		c.log.Infof("NewRS is ready and matched rollout spec replicas %d, stop scale up newRS.", rolloutCount)
		return false, nil
	} else {
		newReplicasCount, err = replicasetutil.NewRSNewReplicas(c.rollout, c.allRSs, c.newRS)
		if err != nil {
			return false, err
		}
		if *c.newRS.Spec.Replicas > newReplicasCount {
			// 理论上走不到这里
			c.log.Warnf("new replicas count cannot be scaled down. RS: %s, current:%d, desired:%d", c.newRS.Name, *c.newRS.Spec.Replicas, newReplicasCount)
			return false, nil
		}
	}
	c.log.Infof("Scale new RS[%s] from %d to %d", c.newRS.Name, *c.newRS.Spec.Replicas, newReplicasCount)
	scaled, _, err := c.scaleReplicaSet(c.newRS, newReplicasCount)
	return scaled, err
}

// 处理老的的 rs
func (c *rolloutContext) reconcileOldReplicaSets() (bool, error) {
	olderRSs := controller.FilterActiveReplicaSets(c.olderRSs)
	// 其他RS的总pod数， 按spec.replicas 相加
	olderPodsCount := replicasetutil.GetReplicaCountForReplicaSets(olderRSs)
	if olderPodsCount == 0 {
		// Can't scale down further
		return false, nil
	}
	c.log.Infof("Reconciling %d old ReplicaSets (total pods: %d)", len(olderRSs), olderPodsCount)

	hasScaled := false
	if c.rollout.Spec.Strategy.Canary != nil {
		// Scale down old replica sets, need check replicasToKeep to ensure we can scale down
		scaledDownCount, err := c.scaleDownOldReplicaSetsForCanary(c.stableRS, c.otherRSs)
		if err != nil {
			return false, nil
		}
		//hasScaled = hasScaled || scaledDownCount > 0
		hasScaled = scaledDownCount > 0
	}

	if hasScaled {
		c.log.Infof("Scaled down old RSes")
	}
	return hasScaled, nil
}

// cleanupUnhealthyReplicas will scale down old replica sets with unhealthy replicas, so that all unhealthy replicas will be deleted.
// 删除所有 unhealthy 的 pod
func (c *rolloutContext) cleanupUnhealthyReplicas(oldRSs []*appsv1.ReplicaSet) ([]*appsv1.ReplicaSet, int32, error) {
	// 按创建时间排序，正序，先创建的在前面
	sort.Sort(controller.ReplicaSetsByCreationTimestamp(oldRSs))
	// Safely scale down all old replica sets with unhealthy replicas. Replica set will sort the pods in the order
	// such that not-ready < ready, unscheduled < scheduled, and pending < running. This ensures that unhealthy replicas will
	// been deleted first and won't increase unavailability.
	// 顺序  not-ready < ready, unscheduled < scheduled, and pending < running， 已经启动中的优先杀，其次是未调度，再其次是 pending 的
	totalScaledDown := int32(0)
	for i, targetRS := range oldRSs {
		// 已经没有 pod 的跳过
		if *(targetRS.Spec.Replicas) == 0 {
			// cannot scale down this replica set.
			continue
		}
		c.log.Infof("Found %d available pods in old RS %s/%s", targetRS.Status.AvailableReplicas, targetRS.Namespace, targetRS.Name)
		// avaliable 的 pod 认为是 healthy 的，如果预期 replicas 和 healthy 的数量相等，则认为没有 unhealthy 的 pod
		if *(targetRS.Spec.Replicas) == targetRS.Status.AvailableReplicas {
			// no unhealthy replicas found, no scaling required.
			continue
		}
		// 预期的 pod 数  - healthy 的 pod 数，等于 unhealthy 的 pod 数
		scaledDownCount := *(targetRS.Spec.Replicas) - targetRS.Status.AvailableReplicas

		// 把该 rs 的预期副本数调整为 当前 healthy 的 pod 数
		newReplicasCount := targetRS.Status.AvailableReplicas
		// 如果healthy 的 pod 数比 预期副本数还多，则报错
		if newReplicasCount > *(targetRS.Spec.Replicas) {
			return nil, 0, fmt.Errorf("when cleaning up unhealthy replicas, got invalid request to scale down %s/%s %d -> %d", targetRS.Namespace, targetRS.Name, *(targetRS.Spec.Replicas), newReplicasCount)
		}

		c.log.Infof("Scale other RS[%s] from %d to %d", targetRS.Name, *targetRS.Spec.Replicas, newReplicasCount)

		// 操作 减少老rs 中的 pod
		_, updatedOldRS, err := c.scaleReplicaSet(targetRS, newReplicasCount)
		if err != nil {
			return nil, totalScaledDown, err
		}
		totalScaledDown += scaledDownCount
		// 一遍操作减少，一般更新 old rs 列表中的 item
		oldRSs[i] = updatedOldRS
	}
	// 得到 杀掉所有 unhealthy pod 的 老 rs 列表
	return oldRSs, totalScaledDown, nil
}
