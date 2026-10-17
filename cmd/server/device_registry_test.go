package main

import (
	"fmt"
	"sync"
	"testing"
)

func TestDeviceRegistryLifecycle(t *testing.T) {
	r := newDeviceRegistry()
	if r.Count() != 0 {
		t.Fatalf("fresh registry count = %d, want 0", r.Count())
	}

	addr := "http://192.168.1.10/onvif/device_service"
	s1 := &deviceSession{addr: addr}
	r.Register(addr, s1)
	if got, ok := r.Get(addr); !ok || got != s1 {
		t.Fatalf("Get after register = %v, %v", got, ok)
	}
	if r.Count() != 1 {
		t.Fatalf("count = %d, want 1", r.Count())
	}

	// 同地址重连：替换为新会话
	s2 := &deviceSession{addr: addr}
	r.Register(addr, s2)
	if got, _ := r.Get(addr); got != s2 {
		t.Fatal("re-register should replace the session")
	}
	if r.Count() != 1 {
		t.Fatalf("count after re-register = %d, want 1", r.Count())
	}

	// 空地址与空会话忽略，不产生新条目
	r.Register("", &deviceSession{})
	r.Register("http://192.168.1.11", nil)
	if r.Count() != 1 {
		t.Fatalf("count = %d, want 1", r.Count())
	}

	if got := r.Addresses(); len(got) != 1 || got[0] != addr {
		t.Fatalf("addresses = %v", got)
	}

	r.Unregister(addr)
	if _, ok := r.Get(addr); ok {
		t.Fatal("Get after unregister should miss")
	}
	if r.Count() != 0 {
		t.Fatalf("count after unregister = %d, want 0", r.Count())
	}

	// 未登记地址注销：静默
	r.Unregister("http://192.168.1.99")
}

func TestDeviceRegistryConcurrent(t *testing.T) {
	r := newDeviceRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			addr := fmt.Sprintf("http://192.168.1.%d", i)
			r.Register(addr, &deviceSession{addr: addr})
			r.Get(addr)
			r.Addresses()
			r.Count()
			r.Unregister(addr)
		}(i)
	}
	wg.Wait()
	if r.Count() != 0 {
		t.Fatalf("count after concurrent churn = %d, want 0", r.Count())
	}
}

// TestManagerUsesFreshSessionAfterDisconnect 校验断开后晋升字段可安全
// 访问（嵌入指针恒非 nil），且 registry 同步注销。
func TestManagerUsesFreshSessionAfterDisconnect(t *testing.T) {
	cm := newTestCameraManager()
	cm.mu.Lock()
	cm.deviceSession = &deviceSession{addr: "http://192.168.1.10"}
	cm.registry.Register("http://192.168.1.10", cm.deviceSession)
	cm.mu.Unlock()

	cm.disconnect()

	if cm.deviceSession == nil {
		t.Fatal("embedded session must never be nil")
	}
	// 晋升字段访问不 panic 且为零值
	if cm.addr != "" || cm.units != nil || cm.authUser != "" {
		t.Fatalf("fresh session not zeroed: %+v", cm.deviceSession)
	}
	if cm.registry.Count() != 0 {
		t.Fatalf("registry count after disconnect = %d, want 0", cm.registry.Count())
	}
}
