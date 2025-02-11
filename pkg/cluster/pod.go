package cluster

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	acidv1 "github.com/zalando/postgres-operator/pkg/apis/acid.zalan.do/v1"
	"github.com/zalando/postgres-operator/pkg/spec"
	"github.com/zalando/postgres-operator/pkg/util"
	"github.com/zalando/postgres-operator/pkg/util/patroni"
	"github.com/zalando/postgres-operator/pkg/util/retryutil"
)

func (c *Cluster) listPods() ([]v1.Pod, error) {
	listOptions := metav1.ListOptions{
		LabelSelector: c.labelsSet(false).String(),
	}

	pods, err := c.KubeClient.Pods(c.Namespace).List(context.TODO(), listOptions)
	if err != nil {
		return nil, fmt.Errorf("could not get list of pods: %v", err)
	}

	return pods.Items, nil
}

func (c *Cluster) getRolePods(role PostgresRole) ([]v1.Pod, error) {
	listOptions := metav1.ListOptions{
		LabelSelector: c.roleLabelsSet(false, role).String(),
	}

	pods, err := c.KubeClient.Pods(c.Namespace).List(context.TODO(), listOptions)
	if err != nil {
		return nil, fmt.Errorf("could not get list of pods: %v", err)
	}

	if role == Master && len(pods.Items) > 1 {
		return nil, fmt.Errorf("too many masters")
	}

	return pods.Items, nil
}

// markRollingUpdateFlagForPod sets the indicator for the rolling update requirement
// in the Pod annotation.
func (c *Cluster) markRollingUpdateFlagForPod(pod *v1.Pod, msg string) error {
	// no need to patch pod if annotation is already there
	if c.getRollingUpdateFlagFromPod(pod) {
		return nil
	}

	c.logger.Debugf("mark rolling update annotation for %s: reason %s", pod.Name, msg)
	flag := make(map[string]string)
	flag[rollingUpdatePodAnnotationKey] = strconv.FormatBool(true)

	patchData, err := metaAnnotationsPatch(flag)
	if err != nil {
		return fmt.Errorf("could not form patch for pod's rolling update flag: %v", err)
	}

	err = retryutil.Retry(1*time.Second, 5*time.Second,
		func() (bool, error) {
			_, err2 := c.KubeClient.Pods(pod.Namespace).Patch(
				context.TODO(),
				pod.Name,
				types.MergePatchType,
				[]byte(patchData),
				metav1.PatchOptions{},
				"")
			if err2 != nil {
				return false, err2
			}
			return true, nil
		})
	if err != nil {
		return fmt.Errorf("could not patch pod rolling update flag %q: %v", patchData, err)
	}

	return nil
}

// getRollingUpdateFlagFromPod returns the value of the rollingUpdate flag from the given pod
func (c *Cluster) getRollingUpdateFlagFromPod(pod *v1.Pod) (flag bool) {
	anno := pod.GetAnnotations()
	flag = false

	stringFlag, exists := anno[rollingUpdatePodAnnotationKey]
	if exists {
		var err error
		c.logger.Debugf("found rolling update flag on pod %q", pod.Name)
		if flag, err = strconv.ParseBool(stringFlag); err != nil {
			c.logger.Warnf("error when parsing %q annotation for the pod %q: expected boolean value, got %q\n",
				rollingUpdatePodAnnotationKey,
				types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name},
				stringFlag)
		}
	}

	return flag
}

func (c *Cluster) deletePods() error {
	c.logger.Debugln("deleting pods")
	pods, err := c.listPods()
	if err != nil {
		return err
	}

	for _, obj := range pods {
		podName := util.NameFromMeta(obj.ObjectMeta)

		c.logger.Debugf("deleting pod %q", podName)
		if err := c.deletePod(podName); err != nil {
			c.logger.Errorf("could not delete pod %q: %v", podName, err)
		} else {
			c.logger.Infof("pod %q has been deleted", podName)
		}
	}
	if len(pods) > 0 {
		c.logger.Debugln("pods have been deleted")
	} else {
		c.logger.Debugln("no pods to delete")
	}

	return nil
}

func (c *Cluster) deletePod(podName spec.NamespacedName) error {
	c.setProcessName("deleting pod %q", podName)
	ch := c.registerPodSubscriber(podName)
	defer c.unregisterPodSubscriber(podName)

	if err := c.KubeClient.Pods(podName.Namespace).Delete(context.TODO(), podName.Name, c.deleteOptions); err != nil {
		return err
	}

	return c.waitForPodDeletion(ch)
}

func (c *Cluster) unregisterPodSubscriber(podName spec.NamespacedName) {
	c.logger.Debugf("unsubscribing from pod %q events", podName)
	c.podSubscribersMu.Lock()
	defer c.podSubscribersMu.Unlock()

	ch, ok := c.podSubscribers[podName]
	if !ok {
		panic("subscriber for pod '" + podName.String() + "' is not found")
	}

	delete(c.podSubscribers, podName)
	close(ch)
}

func (c *Cluster) registerPodSubscriber(podName spec.NamespacedName) chan PodEvent {
	c.logger.Debugf("subscribing to pod %q", podName)
	c.podSubscribersMu.Lock()
	defer c.podSubscribersMu.Unlock()

	ch := make(chan PodEvent)
	if _, ok := c.podSubscribers[podName]; ok {
		panic("pod '" + podName.String() + "' is already subscribed")
	}
	c.podSubscribers[podName] = ch

	return ch
}

func (c *Cluster) movePodFromEndOfLifeNode(pod *v1.Pod) (*v1.Pod, error) {
	var (
		eol    bool
		err    error
		newPod *v1.Pod
	)
	podName := util.NameFromMeta(pod.ObjectMeta)

	if eol, err = c.podIsEndOfLife(pod); err != nil {
		return nil, fmt.Errorf("could not get node %q: %v", pod.Spec.NodeName, err)
	} else if !eol {
		c.logger.Infof("check failed: pod %q is already on a live node", podName)
		return pod, nil
	}

	c.setProcessName("moving pod %q out of end-of-life node %q", podName, pod.Spec.NodeName)
	c.logger.Infof("moving pod %q out of the end-of-life node %q", podName, pod.Spec.NodeName)

	if newPod, err = c.recreatePod(podName); err != nil {
		return nil, fmt.Errorf("could not move pod: %v", err)
	}

	if newPod.Spec.NodeName == pod.Spec.NodeName {
		return nil, fmt.Errorf("pod %q remained on the same node", podName)
	}

	if eol, err = c.podIsEndOfLife(newPod); err != nil {
		return nil, fmt.Errorf("could not get node %q: %v", pod.Spec.NodeName, err)
	} else if eol {
		c.logger.Warningf("pod %q moved to end-of-life node %q", podName, newPod.Spec.NodeName)
		return newPod, nil
	}

	c.logger.Infof("pod %q moved from node %q to node %q", podName, pod.Spec.NodeName, newPod.Spec.NodeName)

	return newPod, nil
}

// MigrateMasterPod migrates master pod via failover to a replica
func (c *Cluster) MigrateMasterPod(podName spec.NamespacedName) error {
	var (
		masterCandidateName spec.NamespacedName
		err                 error
		eol                 bool
	)

	oldMaster, err := c.KubeClient.Pods(podName.Namespace).Get(context.TODO(), podName.Name, metav1.GetOptions{})

	if err != nil {
		return fmt.Errorf("could not get pod: %v", err)
	}

	c.logger.Infof("starting process to migrate master pod %q", podName)

	if eol, err = c.podIsEndOfLife(oldMaster); err != nil {
		return fmt.Errorf("could not get node %q: %v", oldMaster.Spec.NodeName, err)
	}
	if !eol {
		c.logger.Debugf("no action needed: master pod is already on a live node")
		return nil
	}

	if role := PostgresRole(oldMaster.Labels[c.OpConfig.PodRoleLabel]); role != Master {
		c.logger.Warningf("no action needed: pod %q is not the master (anymore)", podName)
		return nil
	}
	// we must have a statefulset in the cluster for the migration to work
	if c.Statefulset == nil {
		var sset *appsv1.StatefulSet
		if sset, err = c.KubeClient.StatefulSets(c.Namespace).Get(
			context.TODO(),
			c.statefulSetName(),
			metav1.GetOptions{}); err != nil {
			return fmt.Errorf("could not retrieve cluster statefulset: %v", err)
		}
		c.Statefulset = sset
	}
	// We may not have a cached statefulset if the initial cluster sync has aborted, revert to the spec in that case.
	if *c.Statefulset.Spec.Replicas > 1 {
		if masterCandidateName, err = c.getSwitchoverCandidate(oldMaster); err != nil {
			return fmt.Errorf("could not find suitable replica pod as candidate for failover: %v", err)
		}
	} else {
		c.logger.Warningf("migrating single pod cluster %q, this will cause downtime of the Postgres cluster until pod is back", c.clusterName())
	}

	masterCandidatePod, err := c.KubeClient.Pods(masterCandidateName.Namespace).Get(context.TODO(), masterCandidateName.Name, metav1.GetOptions{})

	if err != nil {
		return fmt.Errorf("could not get master candidate pod: %v", err)
	}

	// there are two cases for each postgres cluster that has its master pod on the node to migrate from:
	// - the cluster has some replicas - migrate one of those if necessary and failover to it
	// - there are no replicas - just terminate the master and wait until it respawns
	// in both cases the result is the new master up and running on a new node.

	if masterCandidatePod == nil {
		if _, err = c.movePodFromEndOfLifeNode(oldMaster); err != nil {
			return fmt.Errorf("could not move pod: %v", err)
		}
		return nil
	}

	if masterCandidatePod, err = c.movePodFromEndOfLifeNode(masterCandidatePod); err != nil {
		return fmt.Errorf("could not move pod: %v", err)
	}

	err = retryutil.Retry(1*time.Minute, 5*time.Minute,
		func() (bool, error) {
			err := c.Switchover(oldMaster, masterCandidateName)
			if err != nil {
				c.logger.Errorf("could not failover to pod %q: %v", masterCandidateName, err)
				return false, nil
			}
			return true, nil
		},
	)

	if err != nil {
		return fmt.Errorf("could not migrate master pod: %v", err)
	}

	return nil
}

// MigrateReplicaPod recreates pod on a new node
func (c *Cluster) MigrateReplicaPod(podName spec.NamespacedName, fromNodeName string) error {
	replicaPod, err := c.KubeClient.Pods(podName.Namespace).Get(context.TODO(), podName.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("could not get pod: %v", err)
	}

	c.logger.Infof("migrating replica pod %q to live node", podName)

	if replicaPod.Spec.NodeName != fromNodeName {
		c.logger.Infof("check failed: pod %q has already migrated to node %q", podName, replicaPod.Spec.NodeName)
		return nil
	}

	if role := PostgresRole(replicaPod.Labels[c.OpConfig.PodRoleLabel]); role != Replica {
		return fmt.Errorf("check failed: pod %q is not a replica", podName)
	}

	_, err = c.movePodFromEndOfLifeNode(replicaPod)
	if err != nil {
		return fmt.Errorf("could not move pod: %v", err)
	}

	return nil
}

func (c *Cluster) getPatroniConfig(pod *v1.Pod) (acidv1.Patroni, map[string]string, error) {
	var (
		patroniConfig acidv1.Patroni
		pgParameters  map[string]string
	)
	podName := util.NameFromMeta(pod.ObjectMeta)
	err := retryutil.Retry(c.OpConfig.PatroniAPICheckInterval, c.OpConfig.PatroniAPICheckTimeout,
		func() (bool, error) {
			var err error
			patroniConfig, pgParameters, err = c.patroni.GetConfig(pod)

			if err != nil {
				return false, err
			}
			return true, nil
		},
	)

	if err != nil {
		return acidv1.Patroni{}, nil, fmt.Errorf("could not get Postgres config from pod %s: %v", podName, err)
	}

	return patroniConfig, pgParameters, nil
}

func (c *Cluster) getPatroniMemberData(pod *v1.Pod) (patroni.MemberData, error) {
	var memberData patroni.MemberData
	err := retryutil.Retry(c.OpConfig.PatroniAPICheckInterval, c.OpConfig.PatroniAPICheckTimeout,
		func() (bool, error) {
			var err error
			memberData, err = c.patroni.GetMemberData(pod)

			if err != nil {
				return false, err
			}
			return true, nil
		},
	)
	if err != nil {
		return patroni.MemberData{}, fmt.Errorf("could not get member data: %v", err)
	}
	if memberData.State == "creating replica" {
		return patroni.MemberData{}, fmt.Errorf("replica currently being initialized")
	}

	return memberData, nil
}

func (c *Cluster) recreatePod(podName spec.NamespacedName) (*v1.Pod, error) {
	stopCh := make(chan struct{})
	ch := c.registerPodSubscriber(podName)
	defer c.unregisterPodSubscriber(podName)
	defer close(stopCh)

	err := retryutil.Retry(1*time.Second, 5*time.Second,
		func() (bool, error) {
			err2 := c.KubeClient.Pods(podName.Namespace).Delete(
				context.TODO(),
				podName.Name,
				c.deleteOptions)
			if err2 != nil {
				return false, err2
			}
			return true, nil
		})
	if err != nil {
		return nil, fmt.Errorf("could not delete pod: %v", err)
	}

	if err := c.waitForPodDeletion(ch); err != nil {
		return nil, err
	}
	pod, err := c.waitForPodLabel(ch, stopCh, nil)
	if err != nil {
		return nil, err
	}
	c.logger.Infof("pod %q has been recreated", podName)
	return pod, nil
}

func (c *Cluster) recreatePods(pods []v1.Pod, switchoverCandidates []spec.NamespacedName) error {
	c.setProcessName("starting to recreate pods")
	c.logger.Infof("there are %d pods in the cluster to recreate", len(pods))

	var (
		masterPod, newMasterPod *v1.Pod
	)
	replicas := switchoverCandidates

	for i, pod := range pods {
		role := PostgresRole(pod.Labels[c.OpConfig.PodRoleLabel])

		if role == Master {
			masterPod = &pods[i]
			continue
		}

		podName := util.NameFromMeta(pods[i].ObjectMeta)
		newPod, err := c.recreatePod(podName)
		if err != nil {
			return fmt.Errorf("could not recreate replica pod %q: %v", util.NameFromMeta(pod.ObjectMeta), err)
		}

		newRole := PostgresRole(newPod.Labels[c.OpConfig.PodRoleLabel])
		if newRole == Replica {
			replicas = append(replicas, util.NameFromMeta(pod.ObjectMeta))
		} else if newRole == Master {
			newMasterPod = newPod
		}
	}

	if masterPod != nil {
		// switchover if
		// 1. we have not observed a new master pod when re-creating former replicas
		// 2. we know possible switchover targets even when no replicas were recreated
		if newMasterPod == nil && len(replicas) > 0 {
			masterCandidate, err := c.getSwitchoverCandidate(masterPod)
			if err != nil {
				// do not recreate master now so it will keep the update flag and switchover will be retried on next sync
				return fmt.Errorf("skipping switchover: %v", err)
			}
			if err := c.Switchover(masterPod, masterCandidate); err != nil {
				return fmt.Errorf("could not perform switch over: %v", err)
			}
		} else if newMasterPod == nil && len(replicas) == 0 {
			c.logger.Warningf("cannot perform switch over before re-creating the pod: no replicas")
		}
		c.logger.Infof("recreating old master pod %q", util.NameFromMeta(masterPod.ObjectMeta))

		if _, err := c.recreatePod(util.NameFromMeta(masterPod.ObjectMeta)); err != nil {
			return fmt.Errorf("could not recreate old master pod %q: %v", util.NameFromMeta(masterPod.ObjectMeta), err)
		}
	}

	return nil
}

func (c *Cluster) getSwitchoverCandidate(master *v1.Pod) (spec.NamespacedName, error) {

	var members []patroni.ClusterMember
	candidates := make([]patroni.ClusterMember, 0)
	syncCandidates := make([]patroni.ClusterMember, 0)

	err := retryutil.Retry(c.OpConfig.PatroniAPICheckInterval, c.OpConfig.PatroniAPICheckTimeout,
		func() (bool, error) {
			var err error
			members, err = c.patroni.GetClusterMembers(master)

			if err != nil {
				return false, err
			}
			return true, nil
		},
	)
	if err != nil {
		return spec.NamespacedName{}, fmt.Errorf("failed to get Patroni cluster members: %s", err)
	}

	for _, member := range members {
		if PostgresRole(member.Role) != Leader && PostgresRole(member.Role) != StandbyLeader && member.State == "running" {
			candidates = append(candidates, member)
			if PostgresRole(member.Role) == SyncStandby {
				syncCandidates = append(syncCandidates, member)
			}
		}
	}

	// pick candidate with lowest lag
	// if sync_standby replicas were found assume synchronous_mode is enabled and ignore other candidates list
	if len(syncCandidates) > 0 {
		sort.Slice(syncCandidates, func(i, j int) bool {
			return syncCandidates[i].Lag < syncCandidates[j].Lag
		})
		return spec.NamespacedName{Namespace: master.Namespace, Name: syncCandidates[0].Name}, nil
	}
	if len(candidates) > 0 {
		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].Lag < candidates[j].Lag
		})
		return spec.NamespacedName{Namespace: master.Namespace, Name: candidates[0].Name}, nil
	}

	return spec.NamespacedName{}, fmt.Errorf("no switchover candidate found")
}

func (c *Cluster) podIsEndOfLife(pod *v1.Pod) (bool, error) {
	node, err := c.KubeClient.Nodes().Get(context.TODO(), pod.Spec.NodeName, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	return node.Spec.Unschedulable || !util.MapContains(node.Labels, c.OpConfig.NodeReadinessLabel), nil

}
