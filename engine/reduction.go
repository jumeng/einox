package engine

// 出站上下文经济（Phase H1②③）：adk middlewares/reduction 接线——工具结果
// 截断（>8192 外置换指针）+ 旧轮清除（超 30% 窗口时外置历史工具参数与结果，
// 保最近 2 个工具轮，清出不足 5% 窗口不动——KV-cache 破坏代价闸，且该闸 >0
// 是 clear 区间深拷贝的触发条件〔reduction 深拷贝保护 session 原文的隐式
// 依赖，禁漂移到 0〕）。外置落会话持久域 sessions/<sid>/spill/（与历史同
// 寿命，随会话删除一并清理——非一轮一清的工作区，跨轮 read_file 取回不
// 失效）；模型面路径 = spill/ 虚拟前缀，fsutil read_file 同前缀路由取回。
// TokenCounter 按整形后出站口径（llm.ShapeMessages 后计数——默认计数器
// 无条件计 ReasoningContent，真源口径高估约一倍致 clear 提前触发白破缓存）。
// 规约 = 设计基线 findings/2026-08-26-einox-harness-multiagent-design.md §2.1。

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/filesystem"
	"github.com/cloudwego/eino/adk/middlewares/reduction"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
)

const (
	// truncMaxLength 单工具结果出站上限（量级同源 run_command 头尾各 8KB）。
	truncMaxLength = 8192
	// clear 窗口占比：超 30% 窗口触发清除；清出不足 5% 窗口不动（缓存代价闸）。
	clearWindowPct  = 30
	clearAtLeastPct = 5
)

// toolSearchExclude 动态工具装载的检索结果禁外置（H7：跨轮恢复动态工具
// 选择靠扫描历史 tool_search 结果，被外置即解析失败）——先占位入名单。
var toolSearchExclude = []string{"tool_search"}

// newReductionMiddleware reduction 中间件构造（window = 模型上下文窗口，
// 0/未知 = 跳过 clear 只截断——阈值无从推导；notify = 外置通知卡开关，
// 子代理装配传 false——子外置通知不混入父事件流，见 spillBackend.note）。
func (m *Manager) newReductionMiddleware(s *session.Session, window int, notify bool) (adk.ChatModelAgentMiddleware, error) {
	var note func(path string, size int)
	if notify {
		// H8-3 外置通知卡：每次 spill 写入发 harness_note（工具卡脚注语义——
		// 「结果已外置 path（N 字符）可展开」；live 流经 session 订阅扇出）
		note = func(path string, size int) {
			title := "工具结果已外置（read_file 可取回全文）"
			if strings.HasPrefix(path, "spill/clear/") {
				title = "历史工具结果已外置（read_file 可取回）"
			}
			s.Record(contract.EvHarnessNote, contract.HarnessNote{
				Kind: "offload", Title: title,
				Detail: fmt.Sprintf("%s（%d 字符）——模型经 read_file %s 取回", path, size, path),
			})
		}
	}
	cfg := &reduction.Config{
		Backend:                   spillBackend{st: m.reg.Store(), owner: s.Owner, sid: m.wsSID(s), note: note},
		MaxLengthForTrunc:         truncMaxLength,
		ReadFileToolName:          "read_file", // 耦合 fs 族（sessionTools）：裁 fs 族后外置指针不可取回（Options.SessionToolsOff 注释与 docs/04 裁剪表已警示）；不联动禁外置——上游截断与外置在同一 handler 内一体，禁外置须复制其逻辑
		TruncExcludeTools:         toolSearchExclude,
		ClearExcludeTools:         toolSearchExclude,
		ClearRetentionSuffixLimit: 2, // 保最近 2 个工具轮（eino 默认 1）
		TokenCounter:              shapedTokenCounter,
		GenTruncOffloadFilePath:   spillPathGen("trunc"),
		GenClearOffloadFilePath:   spillPathGen("clear"),
	}
	if window > 0 {
		cfg.MaxTokensForClear = int64(window * clearWindowPct / 100)
		atLeast := window * clearAtLeastPct / 100
		if atLeast < 1 {
			atLeast = 1 // 禁 0：ClearAtLeastTokens>0 才触发 clear 区间深拷贝（session 保真隐式依赖）
		}
		cfg.ClearAtLeastTokens = int64(atLeast)
	} else {
		cfg.SkipClear = true
	}
	return reduction.New(context.Background(), cfg)
}

// spillDirOf 会话外置域绝对路径（fsutil spill/ 前缀的取回根；辅助对话解析
// 到父目录——共享外置域直读，无复制）。
func (m *Manager) spillDirOf(s *session.Session) string {
	return filepath.Join(m.reg.Store().UserTreeDir(s.Owner), "sessions", m.wsSID(s), "spill")
}

// spillPathGen 模型面外置路径生成（spill/<kind>/<callID>——虚拟前缀，由
// spillBackend 落存储域、fsutil 同前缀取回）。
func spillPathGen(kind string) func(context.Context, *reduction.ToolDetail) (string, error) {
	return func(_ context.Context, td *reduction.ToolDetail) (string, error) {
		id := ""
		if td != nil && td.ToolContext != nil {
			id = td.ToolContext.CallID
		}
		if id == "" {
			id = fmt.Sprintf("anon-%d", time.Now().UnixNano())
		}
		return "spill/" + kind + "/" + id, nil
	}
}

// shapedTokenCounter 整形后出站口径计数（llm.ShapeMessages 与模型包装共用
// 同一规则函数——计数面 = 真实发送面）；系数复用 estimateContext 系
// （estTokens CJK 1/字 + 其余 1/4，角色开销 8，工具 = 名+描述）。
func shapedTokenCounter(_ context.Context, msgs []*schema.Message, tools []*schema.ToolInfo) (int64, error) {
	n := 0
	for _, m := range llm.ShapeMessages(msgs) {
		if m == nil {
			continue
		}
		n += estTokens(msgTextOf(m)) + estTokens(m.ReasoningContent) + 8
		for _, tc := range m.ToolCalls {
			n += estTokens(tc.Function.Name) + estTokens(tc.Function.Arguments)
		}
	}
	for _, t := range tools {
		n += estTokens(t.Name) + estTokens(t.Desc)
	}
	return int64(n), nil
}

// spillBackend reduction 外置后端（会话持久域，经 session Store 写——与
// checkpoint 同链路；reduction 只调 Write，Read 备取回，其余面不支持）。
// note = 外置通知回调（H8-3 harness_note 发源；nil = 不发——子代理等
// 无事件面装配态）。
type spillBackend struct {
	st    session.Store
	owner string
	sid   string
	note  func(path string, size int)
}

// spillRel 模型面虚拟路径 → 存储域 rel（fail-closed：仅 spill/ 前缀）。
func (b spillBackend) spillRel(p string) (string, error) {
	rest, ok := strings.CutPrefix(p, "spill/")
	if !ok || rest == "" {
		return "", fmt.Errorf("外置后端仅接受 spill/ 前缀路径：%s", p)
	}
	return path.Join("sessions", b.sid, "spill", rest), nil
}

func (b spillBackend) Write(_ context.Context, req *filesystem.WriteRequest) error {
	rel, err := b.spillRel(req.FilePath)
	if err != nil {
		return err
	}
	if err := b.st.WriteUserTreeFile(b.owner, rel, []byte(req.Content)); err != nil {
		return err
	}
	if b.note != nil {
		b.note(req.FilePath, len(req.Content)) // 外置通知（H8-3；失败静默——通知非关键面）
	}
	return nil
}

func (b spillBackend) Read(_ context.Context, req *filesystem.ReadRequest) (*filesystem.FileContent, error) {
	rel, err := b.spillRel(req.FilePath)
	if err != nil {
		return nil, err
	}
	data, ok := b.st.ReadUserTreeFile(b.owner, rel)
	if !ok {
		return nil, fmt.Errorf("外置文件不存在：%s", req.FilePath)
	}
	return &filesystem.FileContent{Content: string(data)}, nil
}

// errSpillUnsupported reduction 未使用的 Backend 面（检索/编辑等归工作区件）。
var errSpillUnsupported = errors.New("外置后端只支持读写")

func (b spillBackend) LsInfo(context.Context, *filesystem.LsInfoRequest) ([]filesystem.FileInfo, error) {
	return nil, errSpillUnsupported
}

func (b spillBackend) GrepRaw(context.Context, *filesystem.GrepRequest) ([]filesystem.GrepMatch, error) {
	return nil, errSpillUnsupported
}

func (b spillBackend) GlobInfo(context.Context, *filesystem.GlobInfoRequest) ([]filesystem.FileInfo, error) {
	return nil, errSpillUnsupported
}

func (b spillBackend) Edit(context.Context, *filesystem.EditRequest) error {
	return errSpillUnsupported
}
