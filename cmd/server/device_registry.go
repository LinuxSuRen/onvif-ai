package main

import (
	"sort"
	"sync"
)

// deviceRegistry 维护按设备地址索引的设备会话集合。当前阶段 registry
// 只承载生命周期记账（连接登记、断开注销），多设备取流入口在后续 PR
// 里基于它开放（#39）。并发安全；Register 同键替换（重连场景）。
type deviceRegistry struct {
	mu       sync.Mutex
	sessions map[string]*deviceSession
}

func newDeviceRegistry() *deviceRegistry {
	return &deviceRegistry{sessions: make(map[string]*deviceSession)}
}

// Register 登记一台设备的会话；同地址重连时替换旧条目（旧会话自身的
// 资源清理由 disconnect 流程负责）。空地址与空会话忽略。
func (r *deviceRegistry) Register(addr string, s *deviceSession) {
	if addr == "" || s == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions[addr] = s
}

// Unregister 移除地址对应的会话；地址未登记时静默返回。
func (r *deviceRegistry) Unregister(addr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, addr)
}

// Get 返回地址对应的活跃会话。
func (r *deviceRegistry) Get(addr string) (*deviceSession, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[addr]
	return s, ok
}

// Count 返回当前登记的设备数。
func (r *deviceRegistry) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sessions)
}

// Addresses 返回已登记设备地址列表，排序保证输出稳定。
func (r *deviceRegistry) Addresses() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.sessions))
	for a := range r.sessions {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}
