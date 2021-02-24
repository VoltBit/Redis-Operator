package controllers

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/pkg/errors"

	dbv1 "github.com/PayU/Redis-Operator/api/v1"
	rediscli "github.com/PayU/Redis-Operator/controllers/rediscli"
)

var EMPTY struct{}

// Representation of a cluster, each element contains information about a leader
type RedisClusterView []LeaderNode

type LeaderNode struct {
	Pod         *corev1.Pod
	NodeNumber  string
	RedisID     string
	Failed      bool
	Terminating bool
	Followers   []FollowerNode
}

type FollowerNode struct {
	Pod          *corev1.Pod
	NodeNumber   string
	LeaderNumber string
	RedisID      string
	Failed       bool
	Terminating  bool
}

// In-place sort of a cluster view - ascending alphabetical order by node number
func (v *RedisClusterView) Sort() {
	sort.Slice(*v, func(i, j int) bool {
		return (*v)[i].NodeNumber < (*v)[j].NodeNumber
	})
	for _, leader := range *v {
		sort.Slice(leader.Followers, func(i, j int) bool {
			return leader.Followers[i].NodeNumber < leader.Followers[j].NodeNumber
		})
	}
}

func (v *RedisClusterView) String() string {
	result := ""
	for _, leader := range *v {
		leaderStatus := "ok"
		leaderPodStatus := "up"
		if leader.Pod == nil {
			leaderPodStatus = "down"
		} else if leader.Terminating {
			leaderPodStatus = "terminating"
		}
		if leader.Failed {
			leaderStatus = "fail"
		}
		result = result + fmt.Sprintf("Leader: %s(%s,%s)-[", leader.NodeNumber, leaderPodStatus, leaderStatus)
		for _, follower := range leader.Followers {
			status := "ok"
			podStatus := "up"
			if follower.Pod == nil {
				podStatus = "down"
			} else if follower.Terminating {
				podStatus = "terminating"
			}
			if follower.Failed {
				status = "fail"
			}
			result = result + fmt.Sprintf("%s(%s,%s)", follower.NodeNumber, podStatus, status)
		}
		result = result + "]"
	}
	return result
}

func (r *RedisClusterReconciler) NewRedisClusterView(redisCluster *dbv1.RedisCluster) (*RedisClusterView, error) {
	var cv RedisClusterView

	pods, err := r.getRedisClusterPods(redisCluster)
	if err != nil {
		return nil, err
	}

	followerCounter := redisCluster.Spec.LeaderCount
	for i := 0; i < redisCluster.Spec.LeaderCount; i++ {
		leader := LeaderNode{Pod: nil, NodeNumber: strconv.Itoa(i), RedisID: "", Failed: true, Terminating: false, Followers: nil}
		for j := 0; j < redisCluster.Spec.LeaderFollowersCount; j++ {
			follower := FollowerNode{Pod: nil, NodeNumber: strconv.Itoa(followerCounter), LeaderNumber: strconv.Itoa(i), RedisID: "", Failed: true, Terminating: false}
			leader.Followers = append(leader.Followers, follower)
			followerCounter++
		}
		cv = append(cv, leader)
	}

	for i, pod := range pods {
		nn, err := strconv.Atoi(pod.Labels["node-number"])
		if err != nil {
			return nil, errors.Errorf("Failed to parse node-number label: %s (%s)", pod.Labels["node-number"], pod.Name)
		}
		ln, err := strconv.Atoi(pod.Labels["leader-number"])
		if err != nil {
			return nil, errors.Errorf("Failed to parse leader-number label: %s (%s)", pod.Labels["leader-number"], pod.Name)
		}
		if nn == ln {
			cv[ln].Pod = &pods[i]
			cv[ln].NodeNumber = pod.Labels["node-number"]
			if pod.ObjectMeta.DeletionTimestamp != nil {
				cv[ln].Terminating = true
			} else {
				if pod.Status.PodIP != "" {
					clusterInfo, err := r.RedisCLI.ClusterInfo(pod.Status.PodIP)
					if err == nil && clusterInfo != nil && (*clusterInfo)["cluster_state"] == "ok" {
						cv[ln].Failed = false
					}
				}
			}
		} else {
			index := (nn - redisCluster.Spec.LeaderCount) % redisCluster.Spec.LeaderFollowersCount
			cv[ln].Followers[index].Pod = &pods[i]
			cv[ln].Followers[index].NodeNumber = pod.Labels["node-number"]
			cv[ln].Followers[index].LeaderNumber = pod.Labels["leader-number"]
			if pod.ObjectMeta.DeletionTimestamp != nil {
				cv[ln].Followers[index].Terminating = true
			} else {
				if pod.Status.PodIP != "" {
					clusterInfo, err := r.RedisCLI.ClusterInfo(pod.Status.PodIP)
					if err == nil && clusterInfo != nil && (*clusterInfo)["cluster_state"] == "ok" {
						cv[ln].Followers[index].Failed = false
					}
				}
			}
		}
	}
	return &cv, nil
}

// Returns a list with all the IPs of the Redis nodes
func (v *RedisClusterView) IPs() []string {
	var ips []string
	for _, leader := range *v {
		if leader.Pod != nil {
			ips = append(ips, leader.Pod.Status.PodIP)
		}
		for _, follower := range leader.Followers {
			if follower.Pod != nil {
				ips = append(ips, follower.Pod.Status.PodIP)
			}
		}
	}
	return ips
}

// Returns a list with all the IPs of the Redis nodes that are healthy
// A nod eis healthy if Redis can be reached and is in cluster mode
func (r *RedisClusterView) HealthyNodeIPs() []string {
	var ips []string
	for _, leader := range *r {
		if leader.Pod != nil && !(leader.Failed || leader.Terminating) {
			ips = append(ips, leader.Pod.Status.PodIP)
		}
		for _, follower := range leader.Followers {
			if follower.Pod != nil && !(follower.Failed || follower.Terminating) {
				ips = append(ips, follower.Pod.Status.PodIP)
			}
		}
	}
	return ips
}

type NodeNumbers [2]string // 0: node number, 1: leader number

func (r *RedisClusterReconciler) getLeaderIP(followerIP string) (string, error) {
	info, err := r.RedisCLI.Info(followerIP)
	if err != nil {
		return "", err
	}
	return info.Replication["master_host"], nil
}

// Returns the node number and leader number from a pod
func (r *RedisClusterReconciler) getRedisNodeNumbersFromIP(namespace string, podIP string) (string, string, error) {
	pod, err := r.getPodByIP(namespace, podIP)
	if err != nil {
		return "", "", err
	}
	return pod.Labels["node-number"], pod.Labels["leader-number"], err
}

// Returns a mapping between node numbers and IPs
func (r *RedisClusterReconciler) getNodeIPs(redisCluster *dbv1.RedisCluster) (map[string]string, error) {
	nodeIPs := make(map[string]string)
	pods, err := r.getRedisClusterPods(redisCluster)
	if err != nil {
		return nil, err
	}
	for _, pod := range pods {
		nodeIPs[pod.Labels["node-number"]] = pod.Status.PodIP
	}
	return nodeIPs, nil
}

func (r *RedisClusterReconciler) createNewRedisCluster(redisCluster *dbv1.RedisCluster) error {
	r.Log.Info("Creating new cluster...")

	if _, err := r.createRedisSettingConfigMap(redisCluster); err != nil {
		return err
	}

	if _, err := r.createRedisService(redisCluster); err != nil {
		return err
	}

	if err := r.initializeCluster(redisCluster); err != nil {
		return err
	}
	r.Log.Info("[OK] Redis cluster initialized successfully")
	return nil
}

func (r *RedisClusterReconciler) initializeFollowers(redisCluster *dbv1.RedisCluster) error {
	r.Log.Info("Initializing followers...")
	leaderPods, err := r.getRedisClusterPods(redisCluster, "leader")
	if err != nil {
		return err
	}

	var nodeNumbers []NodeNumbers
	nodeNumber := redisCluster.Spec.LeaderCount // first node numbers are reserved for leaders
	for _, leaderPod := range leaderPods {
		for i := 0; i < redisCluster.Spec.LeaderFollowersCount; i++ {
			nodeNumbers = append(nodeNumbers, NodeNumbers{strconv.Itoa(nodeNumber), leaderPod.Labels["node-number"]})
			nodeNumber++
		}
	}

	err = r.addFollowers(redisCluster, nodeNumbers...)
	if err != nil {
		return err
	}

	r.Log.Info("[OK] Redis followers initialized successfully")
	return nil
}

func (r *RedisClusterReconciler) initializeCluster(redisCluster *dbv1.RedisCluster) error {
	var leaderNumbers []string
	// leaders are created first to increase the chance they get scheduled on different
	// AZs when using soft affinity rules
	for leaderNumber := 0; leaderNumber < redisCluster.Spec.LeaderCount; leaderNumber++ {
		leaderNumbers = append(leaderNumbers, strconv.Itoa(leaderNumber))
	}

	newLeaderPods, err := r.createRedisLeaderPods(redisCluster, leaderNumbers...)
	if err != nil {
		return err
	}

	var nodeIPs []string
	for _, leaderPod := range newLeaderPods {
		r.RedisCLI.Flushall(leaderPod.Status.PodIP)
		r.RedisCLI.ClusterReset(leaderPod.Status.PodIP)
		nodeIPs = append(nodeIPs, leaderPod.Status.PodIP)
	}

	if _, err := r.waitForPodReady(newLeaderPods...); err != nil {
		return err
	}

	if err := r.waitForRedis(nodeIPs...); err != nil {
		return err
	}

	if _, err = r.RedisCLI.ClusterCreate(nodeIPs); err != nil {
		return err
	}

	return r.waitForClusterCreate(nodeIPs)
}

func (r *RedisClusterReconciler) addFollower(redisCluster *dbv1.RedisCluster, followerIP string, leaderIP string, leaderID string) {

}

// Make a new Redis node join the cluster as a follower and wait until data sync is complete
func (r *RedisClusterReconciler) replicateLeader(followerIP string, leaderIP string) error {
	r.Log.Info(fmt.Sprintf("Replicating leader: %s->%s", followerIP, leaderIP))
	time.Sleep(9999 * time.Second)
	r.Log.Info("leader ID start")
	leaderID, err := r.RedisCLI.MyClusterID(leaderIP)
	if err != nil {
		return err
	}

	r.Log.Info("follower ID start")
	followerID, err := r.RedisCLI.MyClusterID(followerIP)
	if err != nil {
		return err
	}

	r.Log.Info(fmt.Sprintf("add follower start: %v %v %v", followerIP, leaderIP, leaderID))
	if stdout, err := r.RedisCLI.AddFollower(followerIP, leaderIP, leaderID); err != nil {
		if !strings.Contains(stdout, "All nodes agree about slots configuration") {
			return err
		}
	}

	r.Log.Info("meet start")
	if err = r.waitForRedisMeet(leaderIP, followerIP); err != nil {
		return err
	}

	r.Log.Info(fmt.Sprintf("Replication successful"))

	if err = r.waitForRedisReplication(leaderIP, leaderID, followerID); err != nil {
		return err
	}

	if err = r.waitForRedisSync(followerIP); err != nil {
		return err
	}

	return r.waitForRedisLoad(followerIP)
}

// Triggeres a forced failover on the specified node
func (r *RedisClusterReconciler) doForcedFailover(followerIP string) error {
	r.Log.Info("Running 'cluster failover FORCE' on " + followerIP)
	_, err := r.RedisCLI.ClusterFailover(followerIP, "force")
	if err != nil {
		return err
	}
	if err := r.waitForManualFailover(followerIP); err != nil {
		return err
	}
	return nil
}

// Changes the role of a leader with one of its followers
// Returns the IP of the promoted follower
func (r *RedisClusterReconciler) doFailover(leaderIP string, followerIP ...string) (string, error) {
	var promotedFollowerIP string
	leaderID, err := r.RedisCLI.MyClusterID(leaderIP)
	if err != nil {
		return "", err
	}

	r.Log.Info(fmt.Sprintf("Starting manual failover process [Cluster-Failover, Verify-New-Leader] on leader: %s(%s)", leaderIP, leaderID))

	if len(followerIP) != 0 {
		promotedFollowerIP = followerIP[0]
	} else {
		replicas, err := r.RedisCLI.ClusterReplicas(leaderIP, leaderID)
		if err != nil {
			return "", err
		}
		if len(*replicas) == 0 {
			return "", errors.Errorf("Attempted FAILOVER on a leader (%s) with no followers. This case is not supported yet.", leaderIP)
		}
		promotedFollowerIP = strings.Split((*replicas)[0].Addr, ":")[0]
	}

	r.Log.Info("Running 'cluster failover' on " + promotedFollowerIP)
	_, err = r.RedisCLI.ClusterFailover(promotedFollowerIP)
	if err != nil {
		return "", err
	}
	if err = r.waitForManualFailover(promotedFollowerIP); err != nil {
		return "", err
	}
	r.Log.Info(fmt.Sprintf("[OK] Leader failover successful (%s,%s)", leaderIP, promotedFollowerIP))
	return promotedFollowerIP, nil
}

// Recreates a leader based on a replica that took its place in a failover process;
// the old leader pod must be already deleted
func (r *RedisClusterReconciler) recreateLeader(redisCluster *dbv1.RedisCluster, promotedFollowerIP string) error {
	nodeNumber, oldLeaderNumber, err := r.getRedisNodeNumbersFromIP(redisCluster.Namespace, promotedFollowerIP)
	if err != nil {
		return err
	}
	r.Log.Info(fmt.Sprintf("Recreating leader [%s] using node [%s]", oldLeaderNumber, nodeNumber))

	newLeaderPods, err := r.createRedisLeaderPods(redisCluster, oldLeaderNumber)
	if err != nil {
		return err
	}
	newLeaderIP := newLeaderPods[0].Status.PodIP

	newLeaderPods, err = r.waitForPodReady(newLeaderPods...)
	if err != nil {
		return err
	}

	if err := r.waitForRedis(newLeaderIP); err != nil {
		return err
	}

	if err = r.replicateLeader(newLeaderIP, promotedFollowerIP); err != nil {
		return err
	}

	r.Log.Info("Leader replication successful")

	if _, err = r.doFailover(promotedFollowerIP, newLeaderIP); err != nil {
		return err
	}

	r.Log.Info(fmt.Sprintf("[OK] Leader [%s] recreated successfully; new IP: [%s]", oldLeaderNumber, newLeaderIP))
	return nil
}

// Adds one or more follower pods to the cluster
func (r *RedisClusterReconciler) addFollowers(redisCluster *dbv1.RedisCluster, nodeNumbers ...NodeNumbers) error {
	if len(nodeNumbers) == 0 {
		return errors.Errorf("Failed to add followers - no node numbers: (%s)", nodeNumbers)
	}

	newFollowerPods, err := r.createRedisFollowerPods(redisCluster, nodeNumbers...)
	if err != nil {
		return err
	}

	nodeIPs, err := r.getNodeIPs(redisCluster)
	if err != nil {
		return err
	}

	pods, err := r.waitForPodReady(newFollowerPods...)
	if err != nil {
		return err
	}

	for _, followerPod := range pods {
		if err := r.waitForRedis(followerPod.Status.PodIP); err != nil {
			return err
		}
		r.Log.Info(fmt.Sprintf("Replicating: %s %s", followerPod.Name, "redis-node-"+followerPod.Labels["leader-number"]))
		if err = r.replicateLeader(followerPod.Status.PodIP, nodeIPs[followerPod.Labels["leader-number"]]); err != nil {
			return err
		}
	}
	return nil
}

// Removes all nodes the cluster node table entries with IDs of nodes not available
// Recives the list of healthy cluster nodes (Redis is reachable and has cluster mode on)
func (r *RedisClusterReconciler) bckforgetLostNodes(redisCluster *dbv1.RedisCluster) error {

	clusterView, err := r.NewRedisClusterView(redisCluster)
	if err != nil {
		return err
	}

	lostNodeIDSet := make(map[string]struct{})
	nodeMap := make(map[string]string)

	r.Log.Info("Forgetting lost nodes...")
	healthyNodeIPs := clusterView.HealthyNodeIPs()

	for _, healthyNodeIP := range healthyNodeIPs {
		healthyNodeID, err := r.RedisCLI.MyClusterID(healthyNodeIP)
		if err != nil {
			r.Log.Error(err, fmt.Sprintf("Could not reach node %s", healthyNodeIP))
			return err
		}
		nodeMap[healthyNodeIP] = healthyNodeID
	}

	for healthyNodeIP := range nodeMap {
		nodeTable, err := r.RedisCLI.ClusterNodes(healthyNodeIP)
		if err != nil || nodeTable == nil || len(*nodeTable) == 0 {
			r.Log.Info(fmt.Sprintf("[WARN] Could not forget lost nodes on node %s", healthyNodeIP))
			continue
		}
		for _, node := range *nodeTable {
			lost := true
			for _, id := range nodeMap {
				if node.ID == id {
					lost = false
					break
				}
			}
			if lost {
				lostNodeIDSet[node.ID] = EMPTY
			}
		}
	}

	for id := range lostNodeIDSet {
		r.forgetNode(healthyNodeIPs, id)
	}
	return nil
}

// Removes all nodes the cluster node table entries with IDs of nodes not available
// Recives the list of healthy cluster nodes (Redis is reachable and has cluster mode on)
func (r *RedisClusterReconciler) cleanClusterNodeTables(redisCluster *dbv1.RedisCluster) error {

	clusterView, err := r.NewRedisClusterView(redisCluster)
	if err != nil {
		return err
	}

	nodeMap := make(map[string][]bool)

	nodeIPs := clusterView.HealthyNodeIPs()
	if len(nodeIPs) < redisCluster.Spec.LeaderCount*(redisCluster.Spec.LeaderFollowersCount+1) {
		return errors.Errorf("Cleanup failed - at least one node is unhealthy")
	}

	clusterNodeTables, err := r.getClusterNodeTables(nodeIPs...)
	if err != nil {
		return err
	}
	r.Log.Info("Cleanning up cluster node tables...")

	for _, nodeIP := range nodeIPs {
		for i := range *clusterNodeTables[nodeIP] {
			flags := nodeMap[(*clusterNodeTables[nodeIP])[i].ID]
			flags = append(flags, (*clusterNodeTables[nodeIP])[i].IsFailing())
			nodeMap[(*clusterNodeTables[nodeIP])[i].ID] = flags
		}
	}

	r.Log.Info("Node map: ", nodeMap)

	for nodeID, flags := range nodeMap {
		forget := true
		for _, failed := range flags {
			if !failed {
				forget = false
				break
			}
		}
		if forget {
			r.forgetNode(nodeIPs, nodeID)
		}
	}

	return nil
}

// Gets the cluster node table from one or more nodes
// Returns a mapping between IPs and cluster nodes tables
func (r *RedisClusterReconciler) getClusterNodeTables(nodeIPs ...string) (map[string]*rediscli.RedisClusterNodes, error) {
	var wg sync.WaitGroup
	errs := make(chan error, len(nodeIPs))
	tables := make(chan struct {
		Table *rediscli.RedisClusterNodes
		IP    string
	}, len(nodeIPs))

	for i := range nodeIPs {
		wg.Add(1)
		go func(ip string, wg *sync.WaitGroup) {
			defer wg.Done()
			table, err := r.RedisCLI.ClusterNodes(ip)
			if err != nil {
				errs <- err
			} else {
				tables <- struct {
					Table *rediscli.RedisClusterNodes
					IP    string
				}{table, ip}
			}
		}(nodeIPs[i], &wg)
	}

	wg.Wait()
	close(errs)
	close(tables)

	for err := range errs {
		if err != nil {
			return nil, err
		}
	}

	tableMap := make(map[string]*rediscli.RedisClusterNodes, len(tables))
	for table := range tables {
		tableMap[table.IP] = table.Table
	}

	return tableMap, nil
}

// Removes a node from the cluster nodes table of all specified nodes
// nodeIPs: 	list of active node IPs
// removedID: ID of node to be removed
func (r *RedisClusterReconciler) forgetNode(nodeIPs []string, removedID string) error {
	var wg sync.WaitGroup
	errs := make(chan error, len(nodeIPs))

	for _, nodeIP := range nodeIPs {
		wg.Add(1)
		go func(ip string, wg *sync.WaitGroup) {
			defer wg.Done()
			r.Log.Info(fmt.Sprintf("Running cluster FORGET with: %s %s", ip, removedID))
			if _, err := r.RedisCLI.ClusterForget(ip, removedID); err != nil {
				// TODO we should chatch here the error thrown when the ID was already removed
				errs <- err
			}
		}(nodeIP, &wg)
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *RedisClusterReconciler) cleanupNodeList(redisCluster *dbv1.RedisCluster) error {
	var wg sync.WaitGroup

	r.Log.Info("Cleanning up node tables...")

	clusterView, err := r.NewRedisClusterView(redisCluster)
	if err != nil {
		return err
	}

	podIPs := clusterView.HealthyNodeIPs()
	if len(podIPs) < redisCluster.Spec.LeaderCount*(redisCluster.Spec.LeaderFollowersCount+1) {
		return errors.Errorf("Cleanup failed - one or more nodes are not healthy")
	}

	for i := range podIPs {
		wg.Add(1)
		go func(ip string, wg *sync.WaitGroup) {
			defer wg.Done()
			clusterNodes, err := r.RedisCLI.ClusterNodes(ip)
			if err != nil {
				// TODO node is not reachable => nothing to clean; we could consider throwing an error instead
				return
			}
			for _, clusterNode := range *clusterNodes {
				if clusterNode.IsFailing() {
					// TODO opportunity for higher concurrency - spawn a routine for each ClusterForget command
					if _, err := r.RedisCLI.ClusterForget(ip, clusterNode.ID); err != nil {
						r.Log.Info(fmt.Sprintf("[WARN] Could not clean table for ip %s: %v", ip, err))
					}
				}
			}
		}(podIPs[i], &wg)
	}

	wg.Wait()

	return nil
}

func (r *RedisClusterReconciler) handleFailedAutomaticFailover(leader *LeaderNode) string {
	failoverPodIP := ""
	for _, follower := range leader.Followers {
		if follower.Pod != nil && !follower.Failed {
			if _, pingErr := r.RedisCLI.Ping(follower.Pod.Status.PodIP); pingErr == nil {
				if failoverErr := r.doForcedFailover(follower.Pod.Status.PodIP); failoverErr != nil {
					if rediscli.IsFailoverNotOnReplica(failoverErr) {
						r.Log.Info(fmt.Sprintf("Forced failover successful on [%s](%s)", follower.NodeNumber, follower.Pod.Status.PodIP))
						failoverPodIP = follower.Pod.Status.PodIP
						break
					}
					r.Log.Error(failoverErr, fmt.Sprintf("[WARN] Failed attempt to make node [%s](%s) leader", follower.NodeNumber, follower.Pod.Status.PodIP))
				} else {
					r.Log.Info(fmt.Sprintf("Forced failover successful on [%s](%s)", follower.NodeNumber, follower.Pod.Status.PodIP))
					failoverPodIP = follower.Pod.Status.PodIP
					break
				}
			}
		}
	}
	return failoverPodIP
}

func (r *RedisClusterReconciler) getClusterNodeTimeoutSetting(nodeIP string) (int, error) {
	setting, err := r.RedisCLI.ConfigGet(nodeIP, "cluster-node-timeout")
	r.Log.Info(fmt.Sprintf("Result of config get: %v | %v", *setting, err))
	if err == nil {
		timeout, convErr := strconv.Atoi((*setting)["cluster-node-timeout"])
		if convErr != nil {
			time.Sleep(9999 * time.Second)
			r.Log.Info("Fucked up: %v | %v\n", timeout, convErr)
			return -1, convErr
		}
		return timeout, nil
	}
	return -1, err
}

// Waits for NODE_TIMEOUT * FAIL_REPORT_VALIDITY_MULT
// https://redis.io/topics/cluster-spec
func (r *RedisClusterReconciler) waitForRedisFailDetection(ip string) error {
	clusterNodeTimeout, err := r.getClusterNodeTimeoutSetting(ip)
	if err != nil {
		return err
	}
	r.Log.Info(fmt.Sprintf("Setting: %d\n", clusterNodeTimeout))
	waitTime := clusterNodeTimeout*4 + int(clusterNodeTimeout/4)
	r.Log.Info(fmt.Sprintf("Waiting %dms for Redis to detect the failure\n", waitTime))
	time.Sleep(time.Millisecond * time.Duration(waitTime))
	return nil
}

func (r *RedisClusterReconciler) recoverCluster(redisCluster *dbv1.RedisCluster) error {
	clusterView, err := r.NewRedisClusterView(redisCluster)
	if err != nil {
		return err
	}

	// wait for Redis to flag the nodes as failed
	r.Log.Info(clusterView.String())
	for _, leader := range *clusterView {
		if leader.Failed {
			if leader.Terminating {
				if err = r.waitForPodDelete(*leader.Pod); err != nil {
					return errors.Errorf("Failed to wait for leader pod to be deleted %s: %v", leader.NodeNumber, err)
				}
			}

			err := r.waitForRedisFailDetection(clusterView.HealthyNodeIPs()[0])
			if err != nil {
				return err
			}

			failoverPodIP, err := r.waitForFailover(redisCluster, leader)
			if err != nil {
				r.Log.Error(err, fmt.Sprintf("[WARN] Automatic failover failed for leader [%s]", leader.NodeNumber))
				failoverPodIP = r.handleFailedAutomaticFailover(&leader)
				if failoverPodIP == "" {
					return err
				}
			}

			// 			if err := r.bckforgetLostNodes(redisCluster); err != nil {
			// 				return err
			// 			}

			// if err := r.cleanClusterNodeTables(redisCluster); err != nil {
			// 	return err
			// }

			if leader.Pod != nil && !leader.Terminating {
				_, err := r.deletePodsByIP(redisCluster.Namespace, leader.Pod.Status.PodIP)
				if err != nil {
					return err
				}
				if err = r.waitForPodDelete(*leader.Pod); err != nil {
					return err
				}
			}

			if err := r.recreateLeader(redisCluster, failoverPodIP); err != nil {
				return err
			}
		} else {
			var missingFollowers []NodeNumbers
			var failedFollowerIPs []string
			var terminatingFollowerIPs []string
			var terminatingFollowerPods []corev1.Pod

			for _, follower := range leader.Followers {
				if follower.Pod == nil {
					missingFollowers = append(missingFollowers, NodeNumbers{follower.NodeNumber, follower.LeaderNumber})
				} else if follower.Terminating {
					terminatingFollowerPods = append(terminatingFollowerPods, *follower.Pod)
					terminatingFollowerIPs = append(terminatingFollowerIPs, follower.Pod.Status.PodIP)
					missingFollowers = append(missingFollowers, NodeNumbers{follower.NodeNumber, follower.LeaderNumber})
				} else if follower.Failed {
					failedFollowerIPs = append(failedFollowerIPs, follower.Pod.Status.PodIP)
					missingFollowers = append(missingFollowers, NodeNumbers{follower.NodeNumber, follower.LeaderNumber})
				}
			}
			deletedPods, err := r.deletePodsByIP(redisCluster.Namespace, failedFollowerIPs...)
			if err != nil {
				return err
			}
			if err = r.waitForPodDelete(append(terminatingFollowerPods, deletedPods...)...); err != nil {
				return err
			}

			if len(missingFollowers) > 0 {
				err := r.waitForRedisFailDetection(clusterView.HealthyNodeIPs()[0])
				if err != nil {
					return err
				}

				// if err := r.cleanClusterNodeTables(redisCluster); err != nil {
				// 	return err
				// }

				// if err := r.bckforgetLostNodes(redisCluster); err != nil {
				// 	return err
				// }
				if err := r.addFollowers(redisCluster, missingFollowers...); err != nil {
					return err
				}
			}
		}
	}
	complete, err := r.isClusterComplete(redisCluster)
	if err != nil || !complete {
		return errors.Errorf("Cluster recovery not complete")
	}

	if err := r.cleanClusterNodeTables(redisCluster); err != nil {
		return err
	}
	return nil
}

func (r *RedisClusterReconciler) updateFollower(redisCluster *dbv1.RedisCluster, followerIP string) error {
	pod, err := r.getPodByIP(redisCluster.Namespace, followerIP)
	if err != nil {
		return err
	}

	deletedPods, err := r.deletePodsByIP(redisCluster.Namespace, followerIP)
	if err != nil {
		return err
	} else {
		if err := r.waitForPodDelete(deletedPods...); err != nil {
			return err
		}
	}

	// if err := r.forgetLostNodes(redisCluster); err != nil {
	// 	return err
	// }

	r.Log.Info(fmt.Sprintf("Starting to add follower: (%s %s)", pod.Labels["node-number"], pod.Labels["leader-number"]))
	if err := r.addFollowers(redisCluster, NodeNumbers{pod.Labels["node-number"], pod.Labels["leader-number"]}); err != nil {
		return err
	}

	return nil
}

func (r *RedisClusterReconciler) updateLeader(redisCluster *dbv1.RedisCluster, leaderIP string) error {
	// TODO handle the case where a leader has no followers
	promotedFollowerIP, err := r.doFailover(leaderIP)
	if err != nil {
		return err
	}

	if deletedPods, err := r.deletePodsByIP(redisCluster.Namespace, leaderIP); err != nil {
		return err
	} else {
		if err := r.waitForPodDelete(deletedPods...); err != nil {
			return err
		}
	}

	// 	if err := r.forgetLostNodes(redisCluster); err != nil {
	// 		return err
	// 	}

	if err := r.recreateLeader(redisCluster, promotedFollowerIP); err != nil {
		return err
	}
	return nil
}

func (r *RedisClusterReconciler) updateCluster(redisCluster *dbv1.RedisCluster) error {
	clusterView, err := r.NewRedisClusterView(redisCluster)
	if err != nil {
		return err
	}
	r.Log.Info(clusterView.String())
	r.Log.Info("Updating...")
	for _, leader := range *clusterView {
		for _, follower := range leader.Followers {
			podUpToDate, err := r.isPodUpToDate(redisCluster, follower.Pod)
			if err != nil {
				return err
			}
			if !podUpToDate {
				if err = r.updateFollower(redisCluster, follower.Pod.Status.PodIP); err != nil {
					return err
				}
			} else {
				if _, pollErr := r.waitForPodReady(*follower.Pod); pollErr != nil {
					return pollErr
				}
				if pollErr := r.waitForRedis(follower.Pod.Status.PodIP); pollErr != nil {
					return pollErr
				}
			}
		}
		podUpToDate, err := r.isPodUpToDate(redisCluster, leader.Pod)
		if err != nil {
			return err
		}
		if !podUpToDate {
			if err = r.updateLeader(redisCluster, leader.Pod.Status.PodIP); err != nil {
				// >>> TODO the logic of checking if a leader pod (frst N pods) is indeed a Redis leader must be handled separately
				if rediscli.IsNodeIsNotMaster(err) {
					if _, errDel := r.deletePodsByIP(redisCluster.Namespace, leader.Pod.Status.PodIP); errDel != nil {
						return errDel
					}
				}
				return err
			}
		}
	}
	// if err = r.cleanupNodeList(clusterView.HealthyNodeIPs()); err != nil {
	// 	return err
	// }
	if err = r.cleanClusterNodeTables(redisCluster); err != nil {
		return err
	}
	return nil
}

// TODO replace with a readyness probe on the redis container
func (r *RedisClusterReconciler) waitForRedis(nodeIPs ...string) error {
	for _, nodeIP := range nodeIPs {
		r.Log.Info("Waiting for Redis on " + nodeIP)
		if nodeIP == "" {
			return errors.Errorf("Missing IP")
		}
		if pollErr := wait.PollImmediate(genericCheckInterval, genericCheckTimeout, func() (bool, error) {
			reply, err := r.RedisCLI.Ping(nodeIP)
			if err != nil {
				return false, err
			}
			if strings.ToLower(strings.TrimSpace(reply)) != "pong" {
				return false, nil
			}
			return true, nil
		}); pollErr != nil {
			return pollErr
		}
	}
	return nil
}

func (r *RedisClusterReconciler) waitForClusterCreate(leaderIPs []string) error {
	r.Log.Info("Waiting for cluster create execution to complete...")
	return wait.Poll(clusterCreateInterval, clusterCreateTimeout, func() (bool, error) {
		for _, leaderIP := range leaderIPs {
			clusterInfo, err := r.RedisCLI.ClusterInfo(leaderIP)
			if err != nil {
				return false, err
			}
			if clusterInfo.IsClusterFail() {
				return false, nil
			}
			clusterNodes, err := r.RedisCLI.ClusterNodes(leaderIP)
			if err != nil {
				return false, err
			}
			if len(*clusterNodes) != len(leaderIPs) {
				return false, nil
			}
		}
		return true, nil
	})
}

// Safe to be called with both followers and leaders, the call on a leader will be ignored
func (r *RedisClusterReconciler) waitForRedisSync(nodeIP string) error {
	r.Log.Info("Waiting for SYNC on " + nodeIP)
	return wait.PollImmediate(genericCheckInterval, genericCheckTimeout, func() (bool, error) {
		redisInfo, err := r.RedisCLI.Info(nodeIP)
		if err != nil {
			return false, err
		}
		syncStatus := redisInfo.GetSyncStatus()
		if syncStatus != "" {
			r.Log.Info(fmt.Sprintf("node %s SYNC status: %s", nodeIP, syncStatus))
			return false, nil
		}

		r.Log.Info(fmt.Sprintf("node %s is fully synced", nodeIP))
		return true, nil
	})
}

func (r *RedisClusterReconciler) waitForRedisLoad(nodeIP string) error {
	// we verify that loading process has started
	r.Log.Info(fmt.Sprintf("Waiting for node %s to start LOADING", nodeIP))
	if err := wait.PollImmediate(loadTimeInterval, genericCheckTimeout, func() (bool, error) {
		redisInfo, err := r.RedisCLI.Info(nodeIP)
		if err != nil {
			return false, err
		}

		loadStatusETA := redisInfo.GetLoadStatus()
		if loadStatusETA == "" {
			return false, nil
		}

		r.Log.Info(fmt.Sprintf("node %s started to load", nodeIP))
		return true, nil
	}); err != nil {
		if err.Error() != wait.ErrWaitTimeout.Error() {
			return err
		}
		r.Log.Info(fmt.Sprintf("[WARN] timeout waiting for LOADING process to start on node %s", nodeIP))
	}

	// waiting for loading process to finish
	return wait.PollImmediate(genericCheckInterval, genericCheckTimeout, func() (bool, error) {
		redisInfo, err := r.RedisCLI.Info(nodeIP)
		if err != nil {
			return false, err
		}

		loadStatusETA := redisInfo.GetLoadStatus()
		if loadStatusETA != "" {
			r.Log.Info(fmt.Sprintf("node %s LOAD ETA: %s", nodeIP, loadStatusETA))
			return false, nil
		}

		r.Log.Info(fmt.Sprintf("node %s is fully loaded", nodeIP))
		return true, nil
	})
}

func (r *RedisClusterReconciler) waitForRedisReplication(leaderIP string, leaderID string, followerID string) error {
	r.Log.Info(fmt.Sprintf("Waiting for CLUSTER REPLICATION (%s, %s)", leaderIP, followerID))
	return wait.PollImmediate(genericCheckInterval, genericCheckTimeout, func() (bool, error) {
		replicas, err := r.RedisCLI.ClusterReplicas(leaderIP, leaderID)
		if err != nil {
			return false, err
		}
		for _, replica := range *replicas {
			if replica.ID == followerID {
				return true, nil
			}
		}
		return false, nil
	})
}

func (r *RedisClusterReconciler) waitForRedisMeet(nodeIP string, newNodeIP string) error {
	r.Log.Info(fmt.Sprintf("Waiting for CLUSTER MEET (%s, %s)", nodeIP, newNodeIP))
	return wait.PollImmediate(genericCheckInterval, genericCheckTimeout, func() (bool, error) {
		clusterNodes, err := r.RedisCLI.ClusterNodes(nodeIP)
		if err != nil {
			return false, err
		}
		for _, node := range *clusterNodes {
			if strings.Split(node.Addr, ":")[0] == newNodeIP {
				return true, nil
			}
		}
		return false, nil
	})
}

// Waits for a specified pod to be marked as master
func (r *RedisClusterReconciler) waitForManualFailover(podIP string) error {
	r.Log.Info(fmt.Sprintf("Waiting for [%s] to become leader", podIP))
	return wait.PollImmediate(genericCheckInterval, genericCheckTimeout, func() (bool, error) {
		info, err := r.RedisCLI.Info(podIP)
		if err != nil {
			return false, err
		}
		if info.Replication["role"] == "master" {
			return true, nil
		}
		return false, nil
	})
}

// Waits for Redis to pick a new leader
// Returns the IP of the promoted follower
func (r *RedisClusterReconciler) waitForFailover(redisCluster *dbv1.RedisCluster, leader LeaderNode) (string, error) {
	r.Log.Info(fmt.Sprintf("Waiting for leader [%s] failover", leader.NodeNumber))
	failedFollowers := 0
	var promotedFollowerIP string

	for _, follower := range leader.Followers {
		if follower.Failed {
			failedFollowers++
		}
	}

	if failedFollowers == redisCluster.Spec.LeaderFollowersCount {
		return "", errors.Errorf("Failing leader [%s] lost all followers. Recovery unsupported.", leader.NodeNumber)
	}

	return promotedFollowerIP, wait.PollImmediate(genericCheckInterval, genericCheckTimeout, func() (bool, error) {
		for _, follower := range leader.Followers {
			if follower.Failed {
				continue
			}

			info, err := r.RedisCLI.Info(follower.Pod.Status.PodIP)
			if err != nil {
				continue
			}

			if info.Replication["role"] == "master" {
				promotedFollowerIP = follower.Pod.Status.PodIP
				return true, nil
			}
		}
		return false, nil
	})
}

func (r *RedisClusterReconciler) isPodUpToDate(redisCluster *dbv1.RedisCluster, pod *corev1.Pod) (bool, error) {
	for _, container := range pod.Spec.Containers {
		for _, crContainer := range redisCluster.Spec.RedisPodSpec.Containers {
			if crContainer.Name == container.Name {
				if !reflect.DeepEqual(container.Resources, crContainer.Resources) || crContainer.Image != container.Image {
					return false, nil
				}
			}
		}
	}
	return true, nil
}

// Checks if the image declared by the custom resource is the same as the image in the pods
func (r *RedisClusterReconciler) isClusterUpToDate(redisCluster *dbv1.RedisCluster) (bool, error) {
	pods, err := r.getRedisClusterPods(redisCluster)
	if err != nil {
		return false, err
	}
	for _, pod := range pods {
		podUpdated, err := r.isPodUpToDate(redisCluster, &pod)
		if err != nil {
			return false, err
		}
		if !podUpdated {
			return false, nil
		}
	}
	return true, nil
}

func (r *RedisClusterReconciler) isClusterComplete(redisCluster *dbv1.RedisCluster) (bool, error) {
	clusterView, err := r.NewRedisClusterView(redisCluster)
	if err != nil {
		return false, err
	}
	for _, leader := range *clusterView {
		if leader.Terminating {
			r.Log.Info("Found terminating leader: " + leader.NodeNumber)
			return false, nil
		}
		if leader.Failed {
			r.Log.Info("Found failed leader: " + leader.NodeNumber)
			return false, nil
		}
		for _, follower := range leader.Followers {
			if follower.Terminating {
				r.Log.Info("Found terminating follower: " + follower.NodeNumber)
				return false, nil
			}
			if follower.Failed {
				r.Log.Info("Found failed follower: " + follower.NodeNumber)
				return false, nil
			}
		}
	}
	return true, nil
}
