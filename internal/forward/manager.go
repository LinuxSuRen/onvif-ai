package forward

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/onvif-ai/internal/onvif"
	"github.com/onvif-ai/internal/onvif/discovery"
)

// Status 是一路转发对外暴露的运行状态（API 与前端契约）。
type Status struct {
	// Source 是源 RTSP 地址（供运维定位；摄像头地址本就在局域网内流转）
	Source string `json:"source"`
	// Path 是流媒体服务器上的流路径，如 onvif-ai/192.168.1.21/cam1
	Path string `json:"path"`
	// State 取 running / retrying / error
	State string `json:"state"`
	// LastError 是最近一次失败原因（成功后清空）
	LastError string `json:"last_error,omitempty"`
}

// Source 描述一路可转发的源流（一个 media profile）。
type Source struct {
	// DeviceAddr 是设备 ONVIF 服务地址，用于设备去重与路径生成
	DeviceAddr string
	// Token/Name 是 profile 标识与可读名（路径优先用 Name）
	Token string
	Name  string
	// RTSPURL 是源流地址；已连接画面直接复用（内嵌凭证），
	// 自动发现的设备则经 GetStreamURI 匿名解析
	RTSPURL string
}

// Manager 维护转发集合与自动发现循环，串起两个接入点：
// (a) 摄像头连接/断开（SetDeviceSources / RemoveDevice）；
// (b) AutoDiscover 开启时定期主动 Probe，新设备匿名解析后自动转发。
type Manager struct {
	mu     sync.Mutex
	cfg    Config
	ctx    context.Context
	cancel context.CancelFunc

	// sources 按设备键（normalizeDeviceAddr）保存当前源集合；
	// 配置热更时据此整体重建转发
	sources map[string][]Source
	// fwdMap 按源地址索引在跑的 Forwarder（同一路流只转发一次）
	fwdMap map[string]*fwdEntry
}

// fwdEntry 绑定 Forwarder 与其生命周期句柄；done 在 Run 返回后关闭，
// 让 Manager 能同步等待转发 goroutine 退出（避免向已拆的目标写包）。
type fwdEntry struct {
	fwd    *Forwarder
	cancel context.CancelFunc
	done   chan struct{}
}

// NewManager 以初始配置创建 Manager。发现循环常驻但按需工作：配置里
// AutoDiscover 关闭时它只空转计时，热更开启后无需重建 goroutine（避免
// 「关闭后再开启」时循环已退出、无人重启的竞态）。
func NewManager(cfg Config) *Manager {
	if cfg.DiscoverInterval <= 0 {
		cfg.DiscoverInterval = DefaultDiscoverInterval
	}
	m := &Manager{
		cfg:     cfg,
		sources: make(map[string][]Source),
		fwdMap:  make(map[string]*fwdEntry),
	}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	go m.discoverLoop()
	return m
}

// Close 停止所有转发与发现循环（进程退出时调用）。
func (m *Manager) Close() {
	m.cancel()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.waitForEntriesLocked()
	m.fwdMap = make(map[string]*fwdEntry)
}

// UpdateConfig 热更新配置：停掉现有转发，按新配置从已登记的源集合整体
// 重建——目标地址、认证、TLS 任一变更都要求重连，统一整轮重建最简单。
func (m *Manager) UpdateConfig(cfg Config) {
	if cfg.DiscoverInterval <= 0 {
		cfg.DiscoverInterval = DefaultDiscoverInterval
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg = cfg
	m.waitForEntriesLocked()
	m.fwdMap = make(map[string]*fwdEntry)
	if cfg.Enabled {
		for key := range m.sources {
			m.startDeviceLocked(key)
		}
	}
}

// SetDeviceSources 设置一台设备的源集合（摄像头连接时调用）：
// 先停旧再启新，profile 集合变化时不留残余转发；空集合等价于移除设备。
func (m *Manager) SetDeviceSources(deviceAddr string, srcs []Source) {
	key := normalizeDeviceAddr(deviceAddr)
	if key == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopDeviceLocked(key)
	if len(srcs) == 0 {
		delete(m.sources, key)
		return
	}
	m.sources[key] = srcs
	if m.cfg.Enabled {
		m.startDeviceLocked(key)
	}
}

// RemoveDevice 移除一台设备的全部转发（摄像头断开时调用）。
func (m *Manager) RemoveDevice(deviceAddr string) {
	m.SetDeviceSources(deviceAddr, nil)
}

// discoverLoop 常驻循环：每个周期读取最新配置，AutoDiscover 关闭时空转，
// 开启时执行一轮主动发现。对未登记的设备匿名解析流地址后自动转发；
// 解析失败（含 401 要求认证）记日志跳过，下轮发现天然重试。
func (m *Manager) discoverLoop() {
	for {
		m.mu.Lock()
		interval := m.cfg.DiscoverInterval
		work := m.cfg.Enabled && m.cfg.AutoDiscover
		m.mu.Unlock()

		select {
		case <-m.ctx.Done():
			return
		case <-time.After(interval):
			if work {
				m.discoverOnce()
			}
		}
	}
}

// discoverOnce 执行一轮主动发现并接入新设备。
func (m *Manager) discoverOnce() {
	devices, err := discovery.Probe("")
	if err != nil {
		log.Printf("[forward] 自动发现失败: %v", err)
		return
	}
	for _, d := range devices {
		key := normalizeDeviceAddr(d.Address)
		if key == "" {
			continue
		}
		m.mu.Lock()
		_, known := m.sources[key]
		m.mu.Unlock()
		if known {
			continue // 已连接或已转发的设备不重复接入
		}
		srcs, err := resolveAnonymousSources(d.Address)
		if err != nil {
			// 401（设备要求认证）最常见：匿名解析不了，跳过等下轮
			log.Printf("[forward] 设备 %s 解析失败，跳过自动转发: %v", d.Address, err)
			continue
		}
		m.SetDeviceSources(d.Address, srcs)
		log.Printf("[forward] 自动发现设备 %s，转发 %d 路画面", d.Address, len(srcs))
	}
}

// resolveAnonymousSources 匿名访问设备 ONVIF 服务，解析出全部 profile
// 的 RTSP 流地址。不带凭证：被要求认证的设备返回错误，由调用方跳过。
func resolveAnonymousSources(deviceAddr string) ([]Source, error) {
	client := onvif.NewClient(onvif.Config{
		DeviceAddr: deviceAddr,
		Timeout:    5 * time.Second,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	profiles, err := client.GetProfiles(ctx)
	if err != nil {
		return nil, err
	}
	if len(profiles) == 0 {
		return nil, fmt.Errorf("设备未返回任何媒体配置")
	}

	srcs := make([]Source, 0, len(profiles))
	for _, p := range profiles {
		uri, err := client.GetStreamURI(ctx, p.Token)
		if err != nil || uri.URI == "" {
			log.Printf("[forward] GetStreamUri(%s) 失败: %v，跳过该路", p.Token, err)
			continue
		}
		srcs = append(srcs, Source{
			DeviceAddr: deviceAddr,
			Token:      p.Token,
			Name:       p.Name,
			RTSPURL:    uri.URI,
		})
	}
	if len(srcs) == 0 {
		return nil, fmt.Errorf("设备所有画面均未取到流地址")
	}
	return srcs, nil
}

// startDeviceLocked 为设备的每路源各起一个 Forwarder goroutine（调用方持锁）。
func (m *Manager) startDeviceLocked(key string) {
	for _, src := range m.sources[key] {
		if src.RTSPURL == "" {
			continue // 仅快照降级的画面没有取流地址
		}
		if _, exists := m.fwdMap[src.RTSPURL]; exists {
			continue // 同一地址只转发一路（个别设备多 profile 撞同一流地址）
		}
		path := ForwardPath(deviceHostFromAddr(src.DeviceAddr), src.Name, src.Token)
		target, err := BuildTargetURL(m.cfg, path)
		if err != nil {
			log.Printf("[forward] 目标地址配置无效，%s 不转发: %v", src.RTSPURL, err)
			continue
		}
		fwd := NewForwarder(src.RTSPURL, target, path)
		ctx, cancel := context.WithCancel(m.ctx)
		entry := &fwdEntry{fwd: fwd, cancel: cancel, done: make(chan struct{})}
		m.fwdMap[src.RTSPURL] = entry
		go func() {
			defer close(entry.done)
			fwd.Run(ctx)
		}()
	}
}

// stopDeviceLocked 停掉属于某设备的全部转发（调用方持锁）。Forwarder 的
// 退出路径只有 ctx 取消，网络操作也有超时，等待必然有限。
func (m *Manager) stopDeviceLocked(key string) {
	for _, src := range m.sources[key] {
		if entry, ok := m.fwdMap[src.RTSPURL]; ok {
			entry.cancel()
			<-entry.done
			delete(m.fwdMap, src.RTSPURL)
		}
	}
}

// waitForEntriesLocked 停止并等待所有在跑的 Forwarder 退出（调用方持锁）。
func (m *Manager) waitForEntriesLocked() {
	for _, entry := range m.fwdMap {
		entry.cancel()
	}
	for _, entry := range m.fwdMap {
		<-entry.done
	}
}

// Status 汇总每路转发的当前状态。
func (m *Manager) Status() []Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Status, 0, len(m.fwdMap))
	for _, entry := range m.fwdMap {
		out = append(out, entry.fwd.Snapshot())
	}
	return out
}
