package session

// 持久会话只读检索面（recall 拉通道的数据底座，findings/2026-08-29-memory-
// three-channel-design.md §1.1）：复用既有落盘真源 users/<op>/sessions/<sid>/
// session.json，零第二存储、零同步。只暴露轻字段 + 续聊历史投影——不含
// Events 原始流（脱敏隔层：检索/深读到的是摘要与消息文本，非事件载荷）。
//
// fs 前提与 Sweeper 相同（UserTreeDir 为用户域绝对路径，仓内既有 os 直接
// 操作先例）；ListPersistedRecent 按 session.json 修改时间排序，绕开「必须
// 全量解析才能拿到 UpdatedAt」的成本（V2 轻字段前置流式解析的升级位见设计
// 文档 §5）。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/cloudwego/eino/schema"
)

// ListPersistedRecent 最近的 n 个持久会话 sid（按 session.json 修改时间降序；
// 无记录文件或不可读的目录跳过）。n ≤0 返回空。
func ListPersistedRecent(st Store, owner string, n int) []string {
	if n <= 0 {
		return nil
	}
	dir := filepath.Join(st.UserTreeDir(owner), "sessions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type ent struct {
		sid string
		mod time.Time
	}
	found := make([]ent, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if fi, err := os.Stat(filepath.Join(dir, e.Name(), "session.json")); err == nil {
			found = append(found, ent{sid: e.Name(), mod: fi.ModTime()})
		}
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].mod.After(found[j].mod) })
	out := make([]string, 0, min(n, len(found)))
	for _, e := range found {
		if len(out) >= n {
			break
		}
		out = append(out, e.sid)
	}
	return out
}

// PersistedDigest 持久会话检索视图（轻字段 + 续聊历史——多模态在落盘时已
// 拍平为文本）。
type PersistedDigest struct {
	SID       string
	Title     string
	Task      string
	Summary   string
	State     string
	UpdatedAt time.Time
	Messages  []*schema.Message
}

// ReadPersisted 读单个持久会话的检索视图（不存在/损坏 = ok=false 不报错——
// 检索面容忍缺页，调用方跳过即可）。
func ReadPersisted(st Store, owner, sid string) (PersistedDigest, bool) {
	data, ok := st.ReadUserTreeFile(owner, filepath.Join("sessions", sid, "session.json"))
	if !ok || len(data) == 0 {
		return PersistedDigest{}, false
	}
	var rec sessionRecord
	if json.Unmarshal(data, &rec) != nil {
		return PersistedDigest{}, false
	}
	return PersistedDigest{
		SID: rec.SID, Title: rec.Title, Task: rec.Task, Summary: rec.Summary,
		State: rec.State, UpdatedAt: rec.UpdatedAt, Messages: rec.Messages,
	}, true
}
