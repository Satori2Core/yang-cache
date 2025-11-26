package store

import (
	"sync/atomic"
	"time"
)

// ------ 内部时钟 -------

// 内部时钟，减少 time.Now() 调用造成的 GC 压力
var clock, pre, next = time.Now().UnixNano(), uint16(0), uint16(1)

// 返回 clock 变量的当前值。atomic.LoadInt64 是原子操作，用于保证在多线程/协程环境中安全地读取 clock 变量的值
func Now() int64 {
	return atomic.LoadInt64(&clock)
}

func init() {
	go func() {
		// 每秒校准一次
		atomic.StoreInt64(&clock, time.Now().UnixNano())

		for i := 0; i < 9; i++ {
			time.Sleep(100 * time.Millisecond)
			// 保持 clock 在一个精准的时间范围内，同时避免频繁的系统调用
			atomic.AddInt64(&clock, int64(100*time.Millisecond))
		}

		time.Sleep(100 * time.Millisecond)

	}()
}

// ------ 哈希计算 -------

func hashBKRD(s string) (hash int32) {
	for i := 0; i < len(s); i++ {
		hash = hash*131 + int32(s[i]) // 131 是一个常用的质子种子
	}
	return hash
}

// 计算大于或等于输入容量的最小2的幂次方减一
func maskOfNextPowOf2(cap uint16) uint16 {
	if cap > 0 && cap&(cap-1) == 0 {
		return cap - 1
	}
	cap |= cap >> 1
	cap |= cap >> 2
	cap |= cap >> 4
	return cap | (cap >> 8)
}

// ------ 缓存结构 -------

// dlnk
//
//	( [][] ) 	( [前驱是谁？][后继是谁？] )	( [][] )	( [][] )	( [][] )
//																	p：表示尾部在此
//
// dlnk[0]是哨兵节点，记录链表头尾，dlnk[0][p]存储尾部索引，dlnk[0][n]存储头部索引
type cache struct {
	// 双向链表，存储的是前驱和后继节点的索引
	dlnk [][2]uint16
	// 节点存储数组，预分配内存
	m []node
	// 键到节点索引的映射
	hmap map[string]uint16
	// 记录最后一个节点元素得到索引
	last uint16
}

type node struct {
	k        string
	v        Value
	expireAt int64 // 过期时间戳
}

func Create(cap uint16) *cache {
	return &cache{
		dlnk: make([][2]uint16, cap+1),
		m:    make([]node, cap),
		hmap: make(map[string]uint16, cap),
		last: 0,
	}
}

// 向缓存中添加项，如果是新增返回 1，更新返回 0
func (c *cache) put(key string, val Value, expireAt int64, onEvicted func(string, Value)) int {
	if idx, ok := c.hmap[key]; ok {
		// 通过 hmap 获取节点索引
		// 通过数组索引来更新缓存内容
		c.m[idx-1].v, c.m[idx-1].expireAt = val, expireAt
		// lru 调整（调整到链表头：表示最新使用的）
		c.adjust(idx, pre, next)
		return 0
	}

	if c.last == uint16(cap(c.m)) {
		// 获取到尾部节点（最近最少使用）
		tail := &c.m[c.dlnk[0][pre]-1]

		if onEvicted != nil && (*tail).expireAt > 0 {
			onEvicted((*tail).k, (*tail).v)
		}

		delete(c.hmap, (*tail).k)
		// 存入缓存，在调整
		c.hmap[key], (*tail).k, (*tail).v, (*tail).expireAt = c.dlnk[0][pre], key, val, expireAt
		c.adjust(c.dlnk[0][pre], pre, next)

		return 1
	}

	c.last++
	if len(c.hmap) <= 0 {
		c.dlnk[0][pre] = c.last
	} else {
		c.dlnk[c.dlnk[0][next]][pre] = c.last
	}

	// 初始化新节点并更新链表指针
	c.m[c.last-1].k = key
	c.m[c.last-1].v = val
	c.m[c.last-1].expireAt = expireAt
	c.dlnk[c.last] = [2]uint16{0, c.dlnk[0][next]}
	c.hmap[key] = c.last
	c.dlnk[0][next] = c.last

	return 1
}

// 从缓存中获取键对应的节点和状态
func (c *cache) get(key string) (*node, int) {
	if idx, ok := c.hmap[key]; ok {
		c.adjust(idx, pre, next)
		return &c.m[idx-1], 1
	}
	return nil, 0
}

// 从缓存中删除键对应的项
func (c *cache) del(key string) (*node, int, int64) {
	if idx, ok := c.hmap[key]; ok && c.m[idx-1].expireAt > 0 {
		e := c.m[idx-1].expireAt
		c.m[idx-1].expireAt = 0  // 标记为删除
		c.adjust(idx, next, pre) // 移动到链表尾部
		return &c.m[idx-1], 1, e
	}
	return nil, 0, 0
}

// 遍历缓存中的所有有效项
func (c *cache) walk(walker func(key string, val Value, expireAt int64) bool) {
	for idx := c.dlnk[0][next]; idx != 0; idx = c.dlnk[idx][next] {
		if c.m[idx-1].expireAt > 0 && !walker(c.m[idx-1].k, c.m[idx-1].v, c.m[idx-1].expireAt) {
			return
		}
	}
}

// 调整节点在链表中的位置
// 当 front = 0，tail = 1 时，移动到链表头部；否则移动到链表尾部
// dlnk[0]是哨兵节点，记录链表头尾，dlnk[0][p]存储尾部索引，dlnk[0][n]存储头部索引
func (c *cache) adjust(idx, front, tail uint16) {
	// dlnk[idx][f] 获取索引
	if c.dlnk[idx][front] != 0 { // 前驱节点是 0，即头节点
		// 前驱节点索引
		curPre := c.dlnk[idx][front]
		// 后继节点索引
		curNext := c.dlnk[idx][tail]

		c.dlnk[curNext][front] = c.dlnk[idx][front]
		c.dlnk[curPre][tail] = c.dlnk[idx][tail]
		c.dlnk[idx][front] = 0
		c.dlnk[idx][tail] = c.dlnk[0][tail]
		c.dlnk[c.dlnk[0][tail]][front] = idx
		c.dlnk[0][tail] = idx
	}
}
