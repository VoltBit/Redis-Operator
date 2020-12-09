// +build e2e_redis_op

package framework

import (
	"context"
	"fmt"
	"io/ioutil"
	"time"

	dbv1 "github.com/PayU/Redis-Operator/api/v1"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// GetRedisPods gets the Redis pods of a given type. The type of the pod requested can be
// one of "follower", "leader" or "any".
func (f *Framework) GetRedisPods(podType string, opts ...client.ListOption) (*corev1.PodList, error) {
	matchingLabels := client.MatchingLabels{
		"app": "redis",
	}
	if podType != "any" {
		if podType != "leader" && podType != "follower" {
			fmt.Printf("[E2E][WARN] Using custom Redis role: %s\n", podType)
		}
		matchingLabels["redis-node-role"] = podType
	}
	opts = append(opts, matchingLabels)
	return f.GetPods(opts...)
}

// MakeRedisCluster returns the object for a RedisCluster
func (f *Framework) MakeRedisCluster(filePath string) (*dbv1.RedisCluster, error) {
	redisCluster := &dbv1.RedisCluster{}
	yamlRes, err := ioutil.ReadFile(filePath)
	if err != nil {
		return nil, errors.Wrap(err, "Could not read the Redis cluster YAML resource")
	}
	if err = yaml.Unmarshal(yamlRes, &redisCluster); err != nil {
		return nil, errors.Wrap(err, "Could not unmarshal the Redis cluster YAML resource")
	}
	return redisCluster, nil
}

// CreateRedisCluster creates the Redis cluster inside a K8s cluster
func (f *Framework) CreateRedisCluster(ctx *TestCtx, redisCluster *dbv1.RedisCluster, timeout time.Duration) error {
	if err := f.CreateResource(ctx, redisCluster, timeout); err != nil {
		return errors.Wrap(err, "Could not create the Redis cluster resource")
	}
	return nil
}

func (f *Framework) CreateRedisClusterAndWaitUntilReady(ctx *TestCtx, redisCluster *dbv1.RedisCluster, timeout time.Duration) error {
	if err := f.CreateRedisCluster(ctx, redisCluster, timeout); err != nil {
		return err
	}

	if timeout == 0 {
		return nil
	}

	if err := f.WaitForState(redisCluster, "Ready", timeout); err != nil {
		return errors.Wrap(err, "Creation of Redis cluster timed out")
	}

	return nil
}

func (f *Framework) DeleteRedisCluster(obj runtime.Object, timeout time.Duration) error {
	return f.DeleteResource(obj, timeout)
}

func (f *Framework) UpdateImage(obj runtime.Object, image string) error {
	patch := []byte(fmt.Sprintf(`{"spec":{"image":"%s"}}`, image))
	err := f.RuntimeClient.Patch(context.TODO(), obj, client.RawPatch(types.MergePatchType, patch))
	if err != nil {
		return err
	}
	return nil
}

func (f *Framework) WaitForState(redisCluster *dbv1.RedisCluster, state string, timeout ...time.Duration) error {
	buf := dbv1.RedisCluster{}
	t := 10 * time.Second
	if len(timeout) > 0 {
		t = timeout[0]
	}
	return wait.PollImmediate(2*time.Second, t, func() (bool, error) {
		key, err := client.ObjectKeyFromObject(redisCluster)
		if err != nil {
			return false, err
		}
		if err = f.RuntimeClient.Get(context.Background(), key, &buf); err != nil {
			return false, err
		}
		if buf.Status.ClusterState == state {
			return true, nil
		}
		return false, nil
	})
}

// DropNodeConnection stops traffic for a specified Redis node
// redisNodeNumber: node number label assigned at creation time
// policyType: 			one of 'ingress', 'egress' or 'ingress,egress'
// timeout:					time to wait for NetworkPolicy resource creation
func (f *Framework) DropNodeConnection(ctx *TestCtx, redisNodeNumber string, policyType string, timeout time.Duration) (*networkingv1.NetworkPolicy, error) {
	np := f.MakeNetworkPolicy(&metav1.LabelSelector{MatchLabels: map[string]string{"node-number": redisNodeNumber}}, nil, nil)
	err := f.CreateResource(ctx, &np, timeout)
	if err != nil {
		return nil, err
	}
	return nil, nil
}

// DropNamespaceConnection stops network traffic for all pods in a namespace
// policyType: one of 'ingress', 'egress' or 'ingress,egress'
func (f *Framework) DropNamespaceConnection(namespace string, policyType string) (*networkingv1.NetworkPolicy, error) {
	return nil, nil
}
