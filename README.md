# kubernetes-rollout

From https://github.com/argoproj/argo-rollouts

## 基本概念

一个 Rollout 包含三类 RS

1. 新 ReplicaSet， 对应当前 rollout spec
2. Stable ReplicaSet，对应上一次成功发布的版本
3. 其他 ReplicaSet，中间状态，副本数会被优先降为 0

## 原则：

1. 没有 stable 版本时，也会按照批次分步扩容发布
2. stable版本一定不会扩容（回滚到 stable除外，回滚 到 stable时，stable 版本和新 ReplicaSet 版本相同，表现为在发布新ReplicaSet）
3. 新 ReplicaSet 版本一定不会缩容 ~~(这个有问题。如果最新spec.replicas < 当前最新rs的replicas时，是可能缩的)。~~ 除了 ReplicaSet.spec.replicas > Rollout.spec.replicas 这一种情况，见发布规则 1
4. 其他 ReplicaSet 不会被一把杀掉，而是参与到滚动过程中，优先于 stable 被杀掉
5. 扩容时最大副本数不会超过 Rollout.Spec.Replicas + MaxSurge
6. 缩容时，最少可用副本数（所有Ready的Pod数，包含新ReplicaSet、stable、以及其他ReplicaSet）不会少于 Rollout.Spec.Replicas - MaxUnavailable
7. maxSurge 和 maxUnavailable 的计算都是以当前最新的 rollout spec 为基准，所以可能导致大幅扩容或缩容

## 发布规则：

1. 前置判断当前新ReplicaSet的副本数是否比Rollout.Spec.Replicas大（用户发布时缩容）
    1. 如果最新 ReplicaSet.spec.replicas > Rollout.spec.replicas，则设置 ReplicaSet.spec.replicas=Rollout.spec.replicas。循环结束。
2. 检查新 ReplicaSet 是否可以扩容
    1. 先检查所有存活 Pod总数是否少于 Rollout.Spec.Replicas 定义(减去 maxUnavailable)，如果否，跳到下一步，如果少于，则一把补齐缺失的副本数 newRSReplias = Rollout.Spec.Replicas - (stableRS + otherRS)， 当前循环结束
    2. 如果不可扩容 （total + maxSurge >= rollout.spec.replicas），则跳到 step 3，尝试缩容
    3. 否则按照 maxSurge + step定义 计算可以扩容多少个，对 新 ReplicaSet 进行扩容
3. 检查所有老的ReplicaSet 是否可以缩容
    1. 优先清理掉其他ReplicaSet 中 unhealthy 所有 pod
    2. 检查其他 ReplicaSet 是否可以缩容，如果发生缩容，跳到 step 1。
    3. 如果其他ReplicaSet均不可缩容，则检查 stable是否可以缩容

## Steps执行规则

- 严格按照顺序执行，只有继续执行和重置到第一步这两种操作
- 如果new replicaset.spec.replicas > 当前 step的目标replicas，则跳过，遇到pause也全跳，直到遇到第一个不满足的定义的副本数(比例）的step
- 之后的pause step 将按照定义进行暂停

## 测试用例
测试用例请查看 [文档](https://doyard.notion.site/k8s-Rollout-Canary-10dae6e8b6864b10978c01731b1027b7)