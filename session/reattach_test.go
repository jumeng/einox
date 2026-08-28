package session

// 重启续接回归（2026-08-27 线上排障锚：qfc1b 排队消息跨重启丢失——入队事件
// 在盘而队列未带回，下一轮 Run 无从注入）。排队消息是用户交互内容，入队即
// 落盘，进程重启后 Reattach 必须完整带回。

import (
	"testing"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/internal/tstore"
)

func TestReattachRestoresPendingQueue(t *testing.T) {
	st := tstore.New(t.TempDir())
	reg := NewRegistry(st)
	s := reg.Create("张三", "任务", "manual", contract.UserPrefs{})
	if !s.BeginRun("manual") {
		t.Fatal("空闲会话应可抢占运行")
	}
	if !s.Steer("补充指令", nil, "") {
		t.Fatal("运行中输入应走排队")
	}
	reg.Persist(s) // 入队即时落盘（api 层同款语义）

	// 模拟进程重启：同一 store 上新 Registry，内存清空后从盘续接
	reg2 := NewRegistry(st)
	s2 := reg2.Reattach("张三", s.SID)
	if s2 == nil {
		t.Fatal("Reattach 应续接会话")
	}
	queued := s2.TakePending()
	if len(queued) != 1 || queued[0].Text != "补充指令" {
		t.Fatalf("重启后排队消息应完整带回（下一轮 Run 前置注入的输入源）：%v", queued)
	}
}
