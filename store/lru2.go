package store

import (
	"sync"
	"time"
)

type lru2Store struct {
	// 分片锁，每个分片一个互斥锁，减少锁竞争
	locks []sync.Mutex
	// 二级缓存数组，每个分片有2个缓存实例，[0]为一级缓存，[1]为二级缓存
	caches [][2]*cache
	// 回调函数，当缓存项被淘汰时触发
	onEvicted func(key string, value Value)
	// 定时清理过期缓存的定时器
	cleanupTick *time.Ticker
	// 分片掩码，用于计算键对应的分片索引
	mask int32
}

func newLRU2Cache(opts Options) *lru2Store {
	// 设置默认参数
	if opts.BucketCount == 0 {
		// 默认16个分片
		opts.BucketCount = 16
	}

	if opts.CapPerBucket == 0 {
		// 每个分片的一级缓存默认容量
		opts.CapPerBucket = 1024
	}

	if opts.Level2Cap == 0 {
		// 每个分片的二级缓存默认容量
		opts.Level2Cap = 1024
	}

	if opts.CleanupInterval <= 0 {
		// 默认每分钟清理一次过期缓存
		opts.CleanupInterval = time.Minute
	}

	// 计算大于等于分片数的最小2的幂次方作为掩码，用于高效的取模运算
	mark := maskOfNextPowOf2(opts.BucketCount)

	s := &lru2Store{
		locks:       make([]sync.Mutex, mark+1),
		caches:      make([][2]*cache, mark+1),
		onEvicted:   opts.OnEvicted,
		cleanupTick: time.NewTicker(opts.CleanupInterval),
		mask:        int32(mark),
	}

	// 为每个分片初始化一级和二级缓存
	for i := range s.caches {
		// 一级缓存（较小）
		s.caches[i][0] = Create(opts.CapPerBucket)
		// 二级缓存（较大）
		s.caches[i][1] = Create(opts.Level2Cap)
	}

	// 启动后台清理协程
	if opts.CleanupInterval > 0 {
		go s.cleanupLoop()
	}

	return s
}

func (s *lru2Store) Get(key string) (Value, bool) {
	// 1. 计算键对应的分片索引（使用哈希和掩码）
	idx := hashBKDR(key) & s.mask
	// 锁定分片
	s.locks[idx].Lock()
	defer s.locks[idx].Unlock()

	// 获取当前时间（调用本地实现的内部时钟）
	currentTime := Now()

	// 2. 首先检查一级缓存（L1缓存）
	n1, status1, expireAt := s.caches[idx][0].del(key)
	if status1 > 0 { // 说明该键在L1中
		// 在一级缓存中找到条目
		if expireAt > 0 && currentTime >= expireAt {
			// 项目已过期，执行删除
			s.delete(key, idx)
			return nil, false
		}

		// 条目有效：从L1删除并晋升到L2缓存（LRU策略）
		s.caches[idx][1].put(key, n1.v, expireAt, s.onEvicted)
	}

	// 3. 一级缓存未命中，检查二级缓存
	n2, status2 := s._get(key, idx, 1) // 在二级缓存中查找
	if status2 > 0 && n2 != nil {
		if n2.expireAt > 0 && currentTime >= n2.expireAt {
			// 二级缓存中的项目已过期
			s.delete(key, idx)
			return nil, false
		}
		return n2.v, true // 返回二级缓存中的值
	}

	// 4. 两级缓存都没有
	return nil, false
}

func (s *lru2Store) Set(key string, value Value) error {
	// 设置一个极长的过期时间，模拟"永不过期"
	return s.SetWithExpiration(key, value, 9999999999999999)
}

func (s *lru2Store) SetWithExpiration(key string, value Value, expiration time.Duration) error {
	// 计算过期时间戳（0表示永不过期）
	expireAt := int64(0)
	if expiration > 0 {
		// now() 返回纳秒时间戳，确保 expiration 也是纳秒单位
		expireAt = Now() + int64(expiration.Nanoseconds())
	}

	// 计算分片并加锁
	idx := hashBKDR(key) & s.mask
	s.locks[idx].Lock()
	defer s.locks[idx].Unlock()

	// 新数据总是写入一级缓存（L1缓存）
	s.caches[idx][0].put(key, value, expireAt, s.onEvicted)

	return nil
}

func (s *lru2Store) Delete(key string) bool {
	idx := hashBKDR(key) & s.mask
	s.locks[idx].Lock()
	defer s.locks[idx].Unlock()

	return s.delete(key, idx)
}

func (s *lru2Store) Clear() {
	var keys []string

	for i := range s.caches {
		s.locks[i].Lock()

		s.caches[i][0].walk(func(key string, value Value, expireAt int64) bool {
			keys = append(keys, key)
			return true
		})

		s.caches[i][1].walk(func(key string, val Value, expireAt int64) bool {
			// 检查键是否已经收集（避免重复）
			for _, k := range keys {
				if key == k {
					return true
				}
			}
			keys = append(keys, key)
			return true
		})

		s.locks[i].Unlock()
	}

	for _, key := range keys {
		s.Delete(key)
	}
}

func (s *lru2Store) Len() int {
	count := 0

	for i := range s.caches {
		s.locks[i].Lock()

		s.caches[i][0].walk(func(key string, value Value, expireAt int64) bool {
			count++
			return true
		})
		s.caches[i][1].walk(func(key string, value Value, expireAt int64) bool {
			count++
			return true
		})

		s.locks[i].Unlock()
	}

	return count
}

func (s *lru2Store) Close() {
	if s.cleanupTick != nil {
		// 终止定时器
		s.cleanupTick.Stop()
	}
}

func (s *lru2Store) _get(key string, idx, level int32) (*node, int) {
	if n, st := s.caches[idx][level].get(key); st > 0 && n != nil {
		currentTime := Now()
		if n.expireAt <= 0 || currentTime >= n.expireAt {
			// 过期或已删除
			return nil, 0
		}
		return n, st
	}

	return nil, 0
}

func (s *lru2Store) delete(key string, idx int32) bool {
	n1, s1, _ := s.caches[idx][0].del(key)
	n2, s2, _ := s.caches[idx][1].del(key)
	deleted := s1 > 0 || s2 > 0

	if deleted && s.onEvicted != nil {
		if n1 != nil && n1.v != nil {
			s.onEvicted(key, n1.v)
		} else if n2 != nil && n2.v != nil {
			s.onEvicted(key, n2.v)
		}
	}

	if deleted {
		//s.expirations.Delete(key)
	}

	return deleted
}

func (s *lru2Store) cleanupLoop() {
	for range s.cleanupTick.C { // 定时触发清理
		currentTime := Now()

		for i := range s.caches { // 遍历所有分片
			s.locks[i].Lock()

			// 收集该分片中所有过期的键
			var expiredKeys []string

			// 检查一级缓存中的过期项
			s.caches[i][0].walk(func(key string, value Value, expireAt int64) bool {
				if expireAt > 0 && currentTime >= expireAt {
					expiredKeys = append(expiredKeys, key)
				}
				return true
			})

			// 检查二级缓存中的过期项（避免重复）
			s.caches[i][1].walk(func(key string, value Value, expireAt int64) bool {
				if expireAt > 0 && currentTime >= expireAt {
					for _, k := range expiredKeys {
						if key == k {
							return true
						} // 已存在则跳过
					}
					expiredKeys = append(expiredKeys, key)
				}
				return true
			})

			// 删除所有过期的键
			for _, key := range expiredKeys {
				s.delete(key, int32(i))
			}

			s.locks[i].Unlock()
		}
	}
}
