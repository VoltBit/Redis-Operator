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
			sp := strings.Split(lines[i], ")")
			if len(sp) != 2 {
				continue
			}
			l1 := strings.Trim(strings.TrimSpace(sp[1]), "\"")
			sp = strings.Split(lines[i+1], ")")
			if len(sp) != 2 {
				continue
			}
			l2 := strings.Trim(strings.TrimSpace(sp[1]), "\"")
			res[l1] = l2
		}
	}
	return &res
}
