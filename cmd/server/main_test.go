package main

import (
	"context"
	"testing"

	"github.com/onvif-ai/internal/rtsp"
	"github.com/onvif-ai/internal/server"
	"github.com/onvif-ai/internal/ws"
)

func newTestCameraManager() *cameraManager {
	hub := ws.NewHub()
	h := server.NewHandler(hub, nil)
	return &cameraManager{hub: hub, handler: h}
}

// TestBeginTalkbackLifecycle 校验会话受理的先到先得：
// 通道未就绪拒绝 no_backchannel，受理后重复请求拒绝 talkback_in_use，
// endTalkback 后可再次受理。
func TestBeginTalkbackLifecycle(t *testing.T) {
	cm := newTestCameraManager()

	if ok, reason := cm.beginTalkback(); ok || reason != ws.TalkbackRejectNoBackchannel {
		t.Fatalf("expected no_backchannel rejection, got ok=%v reason=%s", ok, reason)
	}

	cm.setTalkbackState(&server.TalkbackState{Available: true})
	if ok, reason := cm.beginTalkback(); !ok || reason != "" {
		t.Fatalf("expected acceptance, got ok=%v reason=%s", ok, reason)
	}

	if ok, reason := cm.beginTalkback(); ok || reason != ws.TalkbackRejectInUse {
		t.Fatalf("expected in-use rejection, got ok=%v reason=%s", ok, reason)
	}

	cm.endTalkback()
	if ok, reason := cm.beginTalkback(); !ok || reason != "" {
		t.Fatalf("expected re-acceptance after stop, got ok=%v reason=%s", ok, reason)
	}
}

// TestBeginTalkbackRejectedWhileTTSSpeaking 校验与 TTS 回传的互斥：
// TTS 播报占用期间对讲请求被拒并返回 busy。
func TestBeginTalkbackRejectedWhileTTSSpeaking(t *testing.T) {
	cm := newTestCameraManager()
	cm.setTalkbackState(&server.TalkbackState{Available: true})

	cm.mu.Lock()
	cm.ttsAudioActive = true
	cm.mu.Unlock()

	if ok, reason := cm.beginTalkback(); ok || reason != ws.TalkbackRejectBusy {
		t.Fatalf("expected busy rejection, got ok=%v reason=%s", ok, reason)
	}
}

// TestSpeakToBackchannelSkipsWhileTalkbackActive 校验反向互斥：
// 对讲会话占用期间 TTS 跳过回传（nil TTS 客户端未被执行即证明提前返回）。
func TestSpeakToBackbackSkipsWhileTalkbackActive(t *testing.T) {
	cm := newTestCameraManager()

	cm.mu.Lock()
	cm.talkbackActive = true
	cm.mu.Unlock()

	// ttsClient 为 nil：若未在对讲检查处提前返回会空指针崩溃
	cm.speakToBackchannel(context.Background(), nil, "你好")
}

// TestSpeakToBackchannelWithoutChannel 校验无回传通道时安全返回。
func TestSpeakToBackchannelWithoutChannel(t *testing.T) {
	cm := newTestCameraManager()
	cm.speakToBackchannel(context.Background(), nil, "你好")
}

// TestWriteTalkbackPCMDroppedWhenInactive 校验会话未激活时迟到分片被丢弃。
func TestWriteTalkbackPCMDroppedWhenInactive(t *testing.T) {
	cm := newTestCameraManager()
	cm.writeTalkbackPCM(make([]byte, 320)) // 不应 panic

	cm.mu.Lock()
	active := cm.talkbackActive
	cm.mu.Unlock()
	if active {
		t.Fatal("no session may be implied by dropped audio")
	}
}

// TestWriteTalkbackPCMFailureEndsSession 校验写入失败（回传通道断开）
// 立即结束会话，把通道还给 TTS。
func TestWriteTalkbackPCMFailureEndsSession(t *testing.T) {
	cm := newTestCameraManager()
	cm.setTalkbackState(&server.TalkbackState{Available: true})
	if ok, reason := cm.beginTalkback(); !ok || reason != "" {
		t.Fatalf("expected acceptance, got ok=%v reason=%s", ok, reason)
	}

	// 未连接的 backchannel：WritePCM 必然失败
	cm.mu.Lock()
	cm.backchannel = rtsp.NewBackchannel("rtsp://127.0.0.1:1/test")
	cm.mu.Unlock()

	cm.writeTalkbackPCM(make([]byte, 320))

	cm.mu.Lock()
	active := cm.talkbackActive
	cm.mu.Unlock()
	if active {
		t.Fatal("session must end after write failure")
	}
}
