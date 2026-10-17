# Windows 原生开发与验证

<!-- matrix/hardened: develop and test the native command on Windows while preserving live sessions and user credentials. -->

Windows 专属开发使用独立源码 checkout 和与 `go.mod` 匹配的 Go 工具链。
macOS/Linux 可继续做跨平台回归和交叉编译；Windows 上直接构建、运行 ConPTY
及命名管道测试，验证目标机的实际行为。完整原主程序仍有 Unix 依赖，此流程
针对 `cmd/agent-deck-native`，当前版本为 `0.6.0+matrix.lite`。

目标 Windows Server 2025 已在专用 `AgentDeckNativeDev-*` 目录安装 Go 1.25.13
和 MinGit 2.56.0.windows.2，保留独立源码 checkout、工具及测试产物；没有替换
现有生产二进制、全局 Git/Go 安装或运行中的会话。该环境已完成原生 Go 构建和
离线集成验证，并完成了下文记录的一次限定范围的真实 Codex 编码与测试执行。

## 原生构建

在仓库根目录的 PowerShell 中执行：

```powershell
$env:CGO_ENABLED = '0'
go build -trimpath -o "$env:TEMP\agent-deck-native.exe" ./cmd/agent-deck-native
& "$env:TEMP\agent-deck-native.exe" --version
```

工具安装在独立目录时使用 `go.exe` 的绝对路径即可，不需要修改全局 PATH。
交付前核对 checkout 的提交、二进制版本和 SHA256；编译通过后继续使用原生版
安装器进行显式部署，不用开发产物覆盖运行中的版本。

## 隔离测试

<!-- matrix/hardened: Windows uses the PowerShell runner; the existing Python runner retains its Unix process and tmux ownership rules. -->

Windows PowerShell 5.1 或更新版本使用专用入口：

```powershell
powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass `
  -File .\scripts\windows-native-test.ps1

# 工具未加入 PATH 时，显式指定实际 go.exe。
powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass `
  -File .\scripts\windows-native-test.ps1 -GoPath 'C:\Tools\Go\bin\go.exe' `
  -NativeIntegration
```

默认执行原生相关包的离线测试。`-NativeIntegration` 先从当前 checkout 构建私有
CLI，再启用真实 ConPTY、后台进程及 IPC fixture；模型联网验收不会因此开启。
在 PowerShell 内可通过数组选择包：

```powershell
& .\scripts\windows-native-test.ps1 `
  -Packages @('./internal/nativefleet', './internal/nativestatestore')
```

入口在隔离前解析并复用 Go 的模块缓存、构建缓存和工具链设置，随后清空继承的
进程环境，只保留 Windows 执行所需变量和这些 Go 设置。HOME、USERPROFILE、
APPDATA、LOCALAPPDATA、CODEX_HOME、XDG 与临时目录均指向本次 GUID 私有目录；
原用户的凭据、配置、外部候选 CLI 和在线测试开关不会继承。结束后恢复调用者的
进程环境及工作目录，不更改系统或用户的全局环境。

临时根目录仅当前 SID 与 SYSTEM 可访问。成功时验证目录身份并移除本次目录；
测试失败、目录身份变化、重解析点或清理失败时保留现场、返回非零并报告准确路径。
保留目录供诊断，不按进程名批量关闭后台，也不删除其他测试或用户会话。

## 验收边界

<!-- matrix/hardened: model replies, command execution and desktop visibility are independent evidence. -->

原生构建及离线 fixture 验证代码、ConPTY、进程树和 IPC。真实 Codex 验收还需要
明确选择 CLI 版本、项目及隔离的认证副本，独立确认模型回复、文件修改、测试执行、
保活和重连。桌面版附带的 CLI 与 npm CLI 可能版本不同，分别核对 `--version`；
模型目录列出一个模型也不能代替实际请求成功。

模型能回复并不证明 Windows 命令执行器可用。SSH 非交互会话与已登录的桌面会话
可能表现不同；执行器或沙箱初始化失败时保留错误证据，不自动关闭沙箱或改写用户
配置。后台开发迁移需先验证真实文件操作和 Git 保存。
独立验证必须记录实际执行器版本、进程所在的 Windows 会话、项目文件差异及
测试结果，并保持已登录的其他进程运行。

### 2026-10-14 限定编码验证

官方 Codex `0.163.0-alpha.11` 在已登录的 Windows Session 2 中，使用原生
`gpt-6.1-sol` 及正常的 elevated `workspace-write` 沙箱完成了一次真实编码任务：

- 只新增 `cmd/agent-deck-native/codex_runtime_test.go`，父会话预先提供的
  `codex_runtime.go` 保持不变。六个用例覆盖默认空选择、有效绝对普通文件、相对
  路径、错误文件名、不存在文件及同名目录。
- 工作目录内使用经哈希核对的 Go 1.25.13 windows/amd64 和 MinGit
  2.56.0.windows.2 工具副本，原工具目录、全局 PATH 和用户配置保持不变。
- 实际执行 `scripts/windows-native-test.ps1`，`-GoPath` 指向该 Go 副本，
  `-Packages` 仅包含上述两个 Go 文件。标准库文件列表测试退出码为零，输出为
  `ok command-line-arguments 0.496s`；不需要联网或读取用户凭据。
- 实际执行的 `git diff --check` 退出码为零。工作者没有写入 `.git` 或提交代码，
  父会话复核后已在目标机保存 Git 记录；macOS 的三个相关包隔离 race 回归也通过。

这次证据确认真实模型能在 Windows 原生沙箱内修改项目文件并运行有意义的测试，
范围仅为程序选择边界的六个用例。它不代表 0.6 的全模块验证或可见桌面验收已通过。

### 2026-10-14 在线历史恢复验证

v0.6 候选程序与官方 Codex `0.163.0-alpha.11` 在目标 Windows 上完成真实 ConPTY
自动化验收，用时 34.50 秒：三个提示及回复均独立读回为同一 JSONL 中的同一对话。
返回列表并关闭 UI 后，后台与 Codex 继续运行；重新打开 UI 接回相同 PID。
显式停止后按 `h`，在历史选择界面通过 Ctrl+E 核对准确的对话身份及本次专用目录，
选择后获得新 PID 和 RunID，原启动描述保持不变。旧 RunID 的输入和停止请求被
拒绝；第三次模型回复仍属于原对话，最终通过 UI 停止并确认目录所有权释放。

独立证据读取器在持有目录及文件身份的条件下，核对三个唯一的用户消息收据和
API 用户/助手消息的精确字节；只接受本次固定环境模板，没有工具调用。损坏 JSON、
大小写重复键、未知内容或无法解释的外层事件均拒绝计为通过。
这证明限定的在线保活、重连和显式历史恢复流程。

同日，已提交的 `c1c4867b` 在目标机独立源码目录完成原生构建及
`scripts/windows-native-test.ps1 -NativeIntegration` 的完整离线检查：九个测试包
通过，一个适配器包没有测试文件。首次完整检查的无效映像资源计数出现两个句柄
的波动；保持原检查和源码，ConPTY 整包及完整套件复验均通过。这支持本次交付，
没有将首次波动写成已确认的生产泄漏或已修复的问题。

### 2026-10-14 可见桌面输入验证

<!-- matrix/hardened: real desktop IME composition is independent of Unicode injection. -->
在已登录的 Windows 桌面内，用独立状态目录及无模型的输入 fixture 打开真实
Windows Terminal 窗口，并核对窗口、UI、后台、SessionID、RunID 和子进程身份。
以下检查使用目标窗口的桌面键盘事件和截图，不向已有用户会话输入内容：

- Microsoft 拼音通过普通字母键生成未提交的组合文字和候选词；名称、目录、分组
  及搜索字段的候选窗跟随当前输入位置。名称与搜索选择“中文”后，分别独立读回
  保存的名称和筛选后的可见列表；候选窗内 Esc 取消不退出表单。
- 长中文与 emoji 名称在窗口缩小后立即重算可见范围，当前编辑位置仍可输入。
  列表、表单及已连接会话分别验证缩放，fixture 实际报告 46×12 与 66×20，
  原 SessionID、RunID 及子进程 PID 保持不变。
- 通过 Windows Terminal 的 Ctrl+V、Ctrl+Shift+V 粘贴中文，表单名称中的 emoji
  精确保存为原字符。会话内中文加 emoji 也独立读回为精确文本；明确选择复制后，
  桌面剪贴板含本次专用中文标记。操作保留原剪贴板格式，并只在序列仍属于本次
  检查时还原。快捷键检查不把终端拦截与输入控件直接读剪贴板混为同一路径证明。
- Ctrl+Q 返回列表后重新打开 fixture，继续收到新的唯一文本，子进程身份不变。
  光标输出包装器不会把 attach 的标准流变成管道，子程序仍使用实际控制台文件。

这次桌面检查修复了列表与表单的 Windows 尺寸更新、输入法硬件光标位置和
拆分 UTF-16 键盘事件导致的 emoji 损坏。尺寸观察仅在前台窗口存在期间运行；
私有光标元数据只与过滤输出成对启用，不送到终端，也不修改系统输入法配置。
隔离的两包 race 回归、真实 Bubble Tea 帧输出检查及 Windows 编译通过。
这些结果限定于本次目标系统、Windows Terminal 和 Microsoft 拼音，不代表所有
终端或输入法的兼容性。模型编码与历史恢复仍使用前两节各自的独立证据。

本次修复提交 `6a3029d1` 在目标 Windows 的独立源码目录运行
`scripts/windows-native-test.ps1 -Packages @('./cmd/agent-deck-native','./internal/nativeui') -NativeIntegration`：
两个包全部通过，分别用时 35.076 秒与 1.556 秒，测试工具已清理自己创建的私有根目录。
发行程序由同一提交的普通私有 clone 使用 Go 1.25.13、`CGO_ENABLED=0`、
`-trimpath -buildvcs=true` 原生构建，独立读回准确提交及 `vcs.modified=false`。
本次 worktree 构建虽然成功，却没有 Go VCS 标识；它没有作为发行产物安装。
发布检查必须核对实际产物元数据，不能以源码 `git status` 干净代替产物身份。

桌面和开始菜单的 0.6 入口已指向新的哈希目录，并在实际桌面加载该程序，继续连接
原 0.6 后台。原快捷方式已保存经哈希验证的备份，旧版入口、已打开的 UI、后台及
原 Codex 进程保持不变。桌面 fixture、其后台和全部临时启动记录已按准确归属清理；
测试窗口的剪贴板及输入语言操作已收尾。

## 并行部署与程序选择

使用原生安装器的 `-StateDirectoryName LiteState-0.6 -ShortcutSuffix 0.6`，可创建
独立的 0.6 列表及入口，保留旧 `LiteState`、旧快捷方式和运行中的后台。发布前
从已提交的源码构建，核对提交身份、`--version`、二进制 SHA256 及快捷方式读回；
不同版本不复用同一状态目录的后台。

如需固定 Codex 程序，安装时同时指定 `-CodexExecutable` 的绝对 `codex.exe` 路径
及 `-ExpectedCodexSHA256`。安装清单和快捷方式保存经核对的哈希，入口启动时
再次验证外部文件；不复制外部程序、登录凭据或修改 PATH。直接运行 UI 时可用
`--codex-executable` 选择程序，但该参数仅检查路径及普通文件身份，不提供安装器
的哈希固定；它只影响随后创建的新 Codex 记录，已有记录继续沿用已保存路径。
完整安装命令见 [Windows 原生精简版](windows-native-prototype.md)。
