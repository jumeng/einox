# NOTICE

本仓库自身代码以 Apache License 2.0 授权（见 [LICENSE](LICENSE)）。
以下第三方项目的部分内容被移植/适配进本仓库，其版权归原作者所有；
各文件头的来源注释为具体出处，本文件为汇总声明。

## openai/codex（Apache-2.0）

源：<https://github.com/openai/codex>

- `tools/applypatch`：`*** Begin Patch` 补丁格式与匹配算法移植自 codex-rs/apply-patch
  crate（parser.rs / file_update.rs / seek_sequence.rs），测试用例改编自其测试
- `prompts/coding.md`：行为纪律与补丁格式规范改编自 gpt_5_codex_prompt.md 与
  prompt_with_apply_patch_instructions.md（英文原稿的中文适配）
- `sandbox/seccomp_linux.go`：部分注释取自 codex linux-sandbox/landlock.rs
- `sandbox/backend_linux.go` / `token_windows.go`：Landlock/rlimit/seccomp 与
  Windows restricted token 的档位与 fail-closed 策略参照其对应实现
- `tools/runcommand`：输出截断策略参照 unified_exec/head_tail_buffer.rs
- `tools/currenttime` / `tools/askuser`：行为与交互语义参照其对应模块

以上部分均已按本系统需要修改。Apache-2.0 全文与 LICENSE 同文。

## deepseek-harness（MIT）

源：<https://github.com/deepseek-ai/deepseek-harness>

- `tools/askuser` / `tools/todo` / `tools/fsutil` / `tools/webfetch` / `mid`：
  交互语义与工具 schema 参照其对应包，均已按本系统需要修改

> MIT License
>
> Copyright (c) 2026 DeepSeek
>
> Permission is hereby granted, free of charge, to any person obtaining a copy
> of this software and associated documentation files (the "Software"), to deal
> in the Software without restriction, including without limitation the rights
> to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
> copies of the Software, and to permit persons to whom the Software is
> furnished to do so, subject to the following conditions:
>
> The above copyright notice and this permission notice shall be included in all
> copies or substantial portions of the Software.
>
> THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
> IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
> FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
> AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
> LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
> OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
> SOFTWARE.

## nanobot（MIT）

源：<https://github.com/HKUDS/nanobot>

- `tools/egress`：出网围栏为 nanobot/security/network.py（344 行 Python）的
  Go 直译收敛——地址归一、默认阻断段、命令串 URL 提取、DNS pinning 等语义
  同源，已按本系统需要修改
- `sandbox/sandbox.go`：denialHint 模型友好边界文案的措辞参照其
  WORKSPACE_BOUNDARY_NOTE 三段式

> MIT License
>
> Copyright (c) 2025-present Xubin Ren and the nanobot contributors
>
> Permission is hereby granted, free of charge, to any person obtaining a copy
> of this software and associated documentation files (the "Software"), to deal
> in the Software without restriction, including without limitation the rights
> to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
> copies of the Software, and to permit persons to whom the Software is
> furnished to do so, subject to the following conditions:
>
> The above copyright notice and this permission notice shall be included in all
> copies or substantial portions of the Software.
>
> THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
> IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
> FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
> AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
> LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
> OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
> SOFTWARE.
