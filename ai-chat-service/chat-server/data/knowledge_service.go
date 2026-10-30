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

// KnowledgeReader 由 KnowledgeRepository 实现；测试可注入 fake 以覆盖并发发布时序。
type KnowledgeReader interface {
	GetPublishedVersion(ctx context.Context, tenantID, entryID uint64) (uint32, error)
	GetVersion(ctx context.Context, tenantID, entryID uint64, version uint32) (*KnowledgeVersion, error)
}

type KnowledgeService struct {
	repo     KnowledgeReader
	cache    KnowledgeCache
	cacheTTL time.Duration
	obs      KBCacheObserver
}

func NewKnowledgeService(repo KnowledgeReader, cache KnowledgeCache, cacheTTL time.Duration,
	obs KBCacheObserver) *KnowledgeService {
	if obs == nil {
		obs = nopKBObserver{}
	}
	if cacheTTL <= 0 {
		cacheTTL = 24 * time.Hour
	}
	return &KnowledgeService{repo: repo, cache: cache, cacheTTL: cacheTTL, obs: obs}
}

// PublishedAnswer 返回 (answer, version)，两者始终成对：version 是本次实际读取的
// MySQL 权威版本。cache hit 分支也会在返回前再次确认 published_version 未变化；
// 若读取期间有新版本发布，则按新版本重走一轮，避免出现 “version=v2, answer=v1”。
// 缓存故障只影响延迟，不影响正确性。
func (s *KnowledgeService) PublishedAnswer(ctx context.Context, tenantID, entryID uint64) (string, uint32, error) {
	for attempt := 0; attempt < 3; attempt++ {
		version, err := s.repo.GetPublishedVersion(ctx, tenantID, entryID)
		if err != nil {
			return "", 0, err
		}
		if version == 0 {
			return "", 0, ErrNotPublished
		}
		key := PublishedAnswerKey(entryID, version)
		if s.cache != nil {
			if ans, ok, err := s.cache.Get(ctx, key); err != nil {
				s.obs.KBCacheLookup("error")
			} else if ok {
				s.obs.KBCacheLookup("hit")
				// cache hit 同样要确认权威版本没有在读取期间前进。
				current, err := s.repo.GetPublishedVersion(ctx, tenantID, entryID)
				if err != nil {
					return "", 0, err
				}
				if current == version {
					return ans, version, nil
				}
				continue
			} else {
				s.obs.KBCacheLookup("miss")
			}
		}
		kv, err := s.repo.GetVersion(ctx, tenantID, entryID, version)
		if err != nil {
			return "", 0, err
		}
		if s.cache != nil {
			// 写缓存失败不改变权威结果。
			_ = s.cache.SetImmutable(ctx, key, kv.Answer, s.cacheTTL)
		}
		current, err := s.repo.GetPublishedVersion(ctx, tenantID, entryID)
		if err != nil {
			return "", 0, err
		}
		if current == version {
			return kv.Answer, version, nil
		}
	}
	return "", 0, ErrVersionConflict
}
