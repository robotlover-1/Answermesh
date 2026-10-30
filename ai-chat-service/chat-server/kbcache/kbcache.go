// Package kbcache 是已发布知识的 PocketKV Cache Aside 客户端。
//
// 只存不可变键 kb:answer:<entry>:v<version>：版本指针始终以 MySQL 为准，
// 因此旧 Worker 迟到写入旧版本也不会覆盖新版本（旧键只保留旧内容，不会被读到）。
package kbcache

import (
	"context"
	"errors"
	"time"

	kredis "ai-chat-service/pkg/db/redis"
	"github.com/redis/go-redis/v9"
)

type Cache struct{}

func New() *Cache { return &Cache{} }

func (c *Cache) Get(ctx context.Context, key string) (string, bool, error) {
	pool := kredis.GetPool()
	if pool == nil {
		return "", false, errors.New("redis pool is not initialized")
	}
	client := pool.Get()
	defer pool.Put(client)
	v, err := client.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// SetImmutable 使用 SETEX（PocketKV 支持 Redis 兼容的 SETEX key seconds value）。
// TTL 只用于回收空间；正确性不依赖缓存内容，读取前总会先查 MySQL 权威版本。
func (c *Cache) SetImmutable(ctx context.Context, key, value string, ttl time.Duration) error {
	pool := kredis.GetPool()
	if pool == nil {
		return errors.New("redis pool is not initialized")
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	client := pool.Get()
	defer pool.Put(client)
	// go-redis v9 没有 SetEX；直接发 PocketKV 兼容的 SETEX key seconds value。
	secs := int(ttl.Seconds())
	if secs < 1 {
		secs = 1
	}
	return client.Do(ctx, "SETEX", key, secs, value).Err()
}
