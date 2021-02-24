package rediscli

import (
	"reflect"
	"testing"
)

func TestNewRedisArray(t *testing.T) {
	// the result of redis-cli config get "cluster-*" command
	rawData := `cluster-require-full-coverage
no
cluster-replica-no-failover
no
cluster-slave-no-failover
no
cluster-enabled
yes
cluster-allow-reads-when-down
no
cluster-announce-ip

cluster-replica-validity-factor
0
cluster-slave-validity-factor
0
cluster-migration-barrier
9999
cluster-announce-bus-port
0
cluster-announce-port
0
cluster-node-timeout
5000`
	mapping := map[string]string{
		"cluster-announce-bus-port":       "0",
		"cluster-announce-port":           "0",
		"cluster-node-timeout":            "5000",
		"cluster-replica-no-failover":     "no",
		"cluster-slave-no-failover":       "no",
		"cluster-enabled":                 "yes",
		"cluster-announce-ip":             "",
		"cluster-slave-validity-factor":   "0",
		"cluster-require-full-coverage":   "no",
		"cluster-allow-reads-when-down":   "no",
		"cluster-replica-validity-factor": "0",
		"cluster-migration-barrier":       "9999",
	}
	redisArray := NewRedisArray(rawData)
	if !reflect.DeepEqual(*redisArray, RedisArray(mapping)) {
		t.Errorf("RedisArray is not correct:\nArray: %v\nvs\nMapping: %v\n", *redisArray, mapping)
	}
}
