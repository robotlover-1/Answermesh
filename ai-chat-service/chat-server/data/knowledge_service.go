package data

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var ErrNotPublished = errors.New("knowledge entry has no published version")

// KnowledgeCache 是已发布答案的 Cache Aside 接口（PocketKV 实现见 chat-server/kbcache）。
type KnowledgeCache interface {
	Get(ctx context.Context, key string) (string, bool, error)
	SetImmutable(ctx context.Context, key, value string, ttl time.Duration) error
}

type KBCacheObserver interface {
	KBCacheLookup(outcome string)
}

type nopKBObserver struct{}

func (nopKBObserver) KBCacheLookup(string) {}

// PublishedAnswerKey 返回不可变版本键；Worker 与读取路径必须使用同一函数。
func PublishedAnswerKey(entryID uint64, version uint32) string {
	return fmt.Sprintf("kb:answer:%d:v%d", entryID, version)
}

type KnowledgeService struct {
	repo     *KnowledgeRepository
	cache    KnowledgeCache
	cacheTTL time.Duration
	obs      KBCacheObserver
}

func NewKnowledgeService(repo *KnowledgeRepository, cache KnowledgeCache, cacheTTL time.Duration,
	obs KBCacheObserver) *KnowledgeService {
	if obs == nil {
		obs = nopKBObserver{}
	}
	if cacheTTL <= 0 {
		cacheTTL = 24 * time.Hour
	}
	return &KnowledgeService{repo: repo, cache: cache, cacheTTL: cacheTTL, obs: obs}
}

// PublishedAnswer 先取 MySQL 权威 published_version，再读对应不可变缓存；
// MISS 时从 MySQL 载入该版本并写缓存。缓存故障只影响延迟，不影响正确性。
func (s *KnowledgeService) PublishedAnswer(ctx context.Context, tenantID, entryID uint64) (string, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		version, err := s.repo.GetPublishedVersion(ctx, tenantID, entryID)
		if err != nil {
			return "", err
		}
		if version == 0 {
			return "", ErrNotPublished
		}
		key := PublishedAnswerKey(entryID, version)
		if s.cache != nil {
			if ans, ok, err := s.cache.Get(ctx, key); err != nil {
				s.obs.KBCacheLookup("error")
			} else if ok {
				s.obs.KBCacheLookup("hit")
				return ans, nil
			} else {
				s.obs.KBCacheLookup("miss")
			}
		}
		kv, err := s.repo.GetVersion(ctx, tenantID, entryID, version)
		if err != nil {
			return "", err
		}
		if s.cache != nil {
			if err := s.cache.SetImmutable(ctx, key, kv.Answer, s.cacheTTL); err != nil {
				lastErr = err // 写缓存失败不改变权威结果
			}
		}
		// 读取期间若有新版本发布，重新按新权威版本走一轮，避免返回已过期内容。
		current, err := s.repo.GetPublishedVersion(ctx, tenantID, entryID)
		if err != nil {
			return "", err
		}
		if current == version {
			return kv.Answer, nil
		}
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", ErrVersionConflict
}
