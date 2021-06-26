package controllers

import (
	"context"
	"fmt"

	rediscli "github.com/PayU/redis-operator/controllers/rediscli"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
)

/*
	The Redis config controller is responsible for monitoring configuration files
	of Redis and loading them on the nodes when changed.
	More features can be added easily here since the config controller is
	separated from the main controller to keep the logic more clean.

	Currently used configuration files:

	- redis.conf: ConfigMap, holds the Redis node main configuration, currently
	it is not actively managed by the controller so any change will have to be
	propagated with a manual rolling restart of the cluster
	https://raw.githubusercontent.com/antirez/redis/6.2.0/redis.conf

	- aclfile: ConfigMap, holds the Redis account information, any change is
	automatically propagated to all cluster nodes.
	https://redis.io/topics/acl
*/

type RedisConfigReconciler struct {
	client.Client
	Log      logr.Logger
	Scheme   *runtime.Scheme
	RedisCLI *rediscli.RedisCLI
}

//+kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=configmaps/status,verbs=get;update;patch

/*
	What needs to happen:
	Check if RDC is in Ready state
	Trigger a volume load on the pods: write an annotation with the hash of the file
	Wait for sync
	Tell Redis to load the file again
	Check if the ACL config was updated

	Reconcile loop on changed configmap:
	Check the status of the RDC
	If 'Ready' state send
	Ping the Redis nodes to load the ACL config file
*/

const redisConfigLabelKey string = "redis-cluster"

func (r *RedisConfigReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	var configMaps corev1.ConfigMapList

	r.Log.Info("Running config reconcile")
	r.Log.Info(fmt.Sprintf("Request: %+v", req))

	// r.Log.Info(fmt.Sprintf("Redis label: %s", req.Name))
	if err := r.List(context.Background(), &configMaps, client.InNamespace(req.Namespace), client.MatchingLabels{redisConfigLabelKey: req.Name}); err != nil {
		r.Log.Error(err, "Failed to fetch configmaps")
	}

	r.Log.Info(fmt.Sprintf("List of maps: %+v", len(configMaps.Items)))
	return ctrl.Result{}, nil
}

func (r *RedisConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ConfigMap{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}
