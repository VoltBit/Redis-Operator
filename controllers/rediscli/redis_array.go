package rediscli

import (
	"strings"
)

type RedisArray map[string]string

func NewRedisArray(rawData string) *RedisArray {
	var res RedisArray = make(map[string]string)
	if strings.TrimSpace(rawData) != "" {
		lines := strings.Split(rawData, "\n")
		for i := 0; i < len(lines)-1; i += 2 {
			res[lines[i]] = lines[i+1]
		}
	}
	return &res
}
