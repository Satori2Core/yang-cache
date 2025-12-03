package singleflight

import "sync"

// 代表正在进行或已结束的请求
type call struct {
	wg  sync.WaitGroup
	val interface{}
	err error
}

// Group manages all kinds of calls
type Group struct {
	// 优化并发性能
	m sync.Map
}

// Do ​对于相同的 key，保证在并发情况下只会执行一次 fn 函数
func (g *Group) Do(key string, fn func() (interface{}, error)) (interface{}, error) {
	// 检查当前key是否已经在查询了
	if existing, ok := g.m.Load(key); ok {
		c := existing.(*call)
		// 等待已存在的请求结束
		c.wg.Wait()
		return c.val, c.err
	}

	// 没有已存在的请求，就新建
	c := &call{}
	c.wg.Add(1)
	g.m.Store(key, c) // 把请求放在map里

	// 执行方法与结果存储
	c.val, c.err = fn()
	// 标记请求完成
	c.wg.Done()

	// 请求完成则清理map
	g.m.Delete(key)
	return c.val, c.err
}

// DoV2
func (g *Group) DoV2(key string, fn func() (interface{}, error)) (interface{}, error) {
	// 使用LoadOrStore解决竞态条件
	c := &call{}
	c.wg.Add(1)

	if actual, loaded := g.m.LoadOrStore(key, c); loaded {
		// 已存在其他请求
		c = actual.(*call)
		c.wg.Wait()
		return c.val, c.err
	}

	// 当前是第一个请求
	defer func() {
		g.m.Delete(key)
		c.wg.Done()
	}()

	c.val, c.err = fn()
	return c.val, c.err
}
