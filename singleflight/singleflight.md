# 简化版的 SingleFlight 并发控制实现

## 核心作用

这个 Do 方法的主要作用是：​对于相同的 key，保证在并发情况下只会执行一次 fn 函数，其他并发请求会等待第一个请求的结果并直接复用。

---

## 执行流程
1. ​检查现有请求​：通过 g.m.Load(key) 检查是否已有相同 key 的请求正在进行
2. 等待已有请求​：如果存在，则等待该请求完成并直接返回结果
3. ​创建新请求​：如果不存在，创建新的 call 对象并存储到 Map 中
4. 执行函数​：执行实际的 fn 函数并存储结果
5. ​清理资源​：请求完成后从 Map 中删除对应的 key

---

## 与官方实现的对比

### 优点
- **使用 sync.Map**​：简化了并发控制，不需要手动管理互斥锁
- ​代码简洁​：逻辑清晰易懂，核心功能完备

---

### 存在的问题

竞态条件（严重问题）​
- 问题​：多个 goroutine 可能同时通过 Load 检查，都发现 key 不存在，然后都创建新的 call，导致重复执行。
```go
if existing, ok := g.m.Load(key); ok {
    c := existing.(*call)
    c.wg.Wait()  // 等待已存在的请求结束
    return c.val, c.err
}

// 问题：在这两个操作之间，其他goroutine可能也执行了Load发现key不存在
c := &call{}
c.wg.Add(1)
g.m.Store(key, c)  // 把请求放在map里
```

---

结果共享信息缺失

- 缺少 shared 返回值，调用方无法区分是直接结果还是共享结果。

---

## 简单优化操作：修复竞态条件

```go
func (g *Group) Do(key string, fn func() (interface{}, error)) (interface{}, error) {
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
```