# Windows 原生精简版

<!-- matrix/hardened: document the isolated experiment without claiming that the complete app is ported. -->

`agent-deck-native` 是独立的 Windows 原生精简版，当前实验版本为
`0.6.0+matrix.lite`。它不依赖 WSL 或 tmux，使用 ConPTY 启动
交互式子进程，通过本机命名管道提供输入、输出、截图、缩放和停止操作。
现已支持独立的中文多会话列表、分组、搜索、SQLite 注册库，以及自动启动的后台；
新增了对原 Agent Deck 数据库格式及会话运行接口的接入；完整 Windows 主程序、
Web 页面或工具状态识别仍未完成。现有生产配置和数据库不会被原型自动打开。
现有 macOS/Linux 程序继续沿用原来的入口。

## 桌面入口与默认流程

<!-- matrix/hardened: an explicit verified per-user install makes daily use discoverable. -->

安装后，双击桌面或开始菜单中的 **Agent Deck 原生版**。入口优先打开 Windows
Terminal，不可用时使用普通控制台。列表默认新建 Codex，名称和目录已填好：
按 `n`，确认目录后按 Enter 即可创建并进入。需要其它项目时直接修改目录字段；
需要 Shell 或 PowerShell 时在工具字段选择，同一窗口会保留最后一次工具选择。

在 Codex 内按 Ctrl+Q 返回列表；按 Enter 可接回同一个仍在运行的会话。关闭列表
或整个终端窗口后，后台和正在运行的会话继续保留。按 `s` 确认停止会结束这次运行，
保留列表记录和项目文件。仍在运行的会话通过 Enter 或 `o` 接回同一进程与对话；
已停止或退出的 Codex 记录通过 Enter 或 `o` 启动新对话。需要恢复历史时，选择
该记录并按 `h`，在 Codex 自己的历史选择界面中选择原项目下的对话。

`h` 仅用于已停止或退出、直接运行原生 Codex 且没有自定义启动参数的记录；
运行中的会话需先显式停止。它临时执行保存的程序加 `resume`，保持已保存的
程序路径、目录、环境覆盖、分组和原始参数不变，不自动选择最近一条历史。
历史恢复启动失败时显示错误，不改为新对话。每次恢复产生新的 RunID；
旧窗口的输入、停止或恢复请求不能作用于新一代进程。

入口使用原来的 StateDB 格式，专用状态目录为
`%LOCALAPPDATA%\AgentDeckNativeLite\LiteState`，默认项目目录为同级 `Workspace`。
它不会打开或导入此前的 prototype、manual-state 或现有生产数据库。Codex 沿用
当前 Windows 用户的正常配置与登录；本安装器不修改这些设置。

部署者将两个安装脚本放在同一目录，核对二进制的 SHA256 后显式安装：

```powershell
powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File .\scripts\windows-native-install.ps1 `
  -BinaryPath .\agent-deck-native.exe -ExpectedSHA256 '<已核对的 Agent Deck SHA256>' -InstallVersion lite-0.6.0 `
  -CodexExecutable 'C:\Tools\Codex\codex.exe' -ExpectedCodexSHA256 '<已核对的 Codex SHA256>' `
  -StateDirectoryName LiteState-0.6 -ShortcutSuffix 0.6
```

安装器在专用的 `AgentDeckNativeLite` 根目录创建版本和哈希命名的私有目录，
并独立读回桌面及开始菜单的快捷方式；启动入口同时固定程序和启动脚本的哈希。
相同内容可重复安装，已有不同二进制、运行配置或快捷方式时拒绝覆盖。安装器
不替换运行中的旧版本、不结束会话，不安装服务、计划任务或登录启动项。

`CodexExecutable` 和 `ExpectedCodexSHA256` 可省略；提供时必须成对，使用核对过
内容的绝对 `codex.exe` 路径。安装器将路径和哈希保存在版本目录内受保护的
`codex-runtime.json`，快捷方式固定该清单的哈希；每次启动入口先验证清单和
外部 Codex 文件，再传给 UI。它不复制或替换外部程序、不调整 PATH、不复制登录
凭据。外部 Codex 更新后哈希不匹配会拒绝启动，需要核对后显式安装新的版本目录。

上例使用独立状态目录 `LiteState-0.6` 和 **Agent Deck 原生版 0.6** 快捷方式，
可与使用 `LiteState` 的旧版并行，保留旧入口及正在运行的后台。省略这两个参数时
仍使用 `LiteState` 和 **Agent Deck 原生版**；同一状态目录的后台版本握手不能
跨版本复用。项目目录仍是同级 `Workspace`，不会复制旧列表或自动迁移会话。

## 复用原主程序的接入阶段

<!-- matrix/hardened: reuse original persistence and business interfaces rather than extend a second UI. -->

当前以现有精简原生版为交付目标，完整原主程序移植暂缓。v0.4 已接通共享数据库、
会话运行接口与原界面的复制和输入动作；v0.5 已在 Windows 实机完成实际 Codex
的创建、两次模型回复、返回、保活、同一对话重连和停止验证。v0.6 增加停止后的
显式历史选择、创建新 Codex 时的程序路径选择和并行安装入口。2026-10-14，v0.6
候选程序与官方 Codex `0.163.0-alpha.11` 已在目标 Windows 完成三次真实提示/回复的
同一历史验证，包括关闭 UI 后保活、相同 PID 重连、停止后 `h` 选择原对话、新运行
拒绝旧 RunID 输入及停止请求，以及最终停止和目录所有权释放。详见
[Windows 原生开发与验证](windows-native-development.md)。这是实际 ConPTY 链路的
自动化证据。随后同日完成独立的可见 Windows Terminal 桌面交互，验证 Microsoft
拼音组合与候选提交、中文和 emoji 剪贴板、列表/表单/会话缩放，以及返回后接回
同一子进程；并修复三处桌面输入问题。具体范围与证据见上述开发文档的桌面验证节。

原生后台可显式使用原来的 `internal/statedb` 和 `instances` / `groups` 表：

```powershell
.\agent-deck-native.exe manager-start --store statedb --json
.\agent-deck-native.exe ui --store statedb --cwd 'C:\Projects\demo'
```

这个接入模式使用独立目录 `%LOCALAPPDATA%\AgentDeckNativeManagerShared`，数据库为
`state.db`。自选目录仍需是专用私有目录；命名管道和后台版本握手包含存储类型，
显式 `--pipe` 也不能让 StateDB 命令复用另一种存储的后台。其它管理命令使用相同
`--store statedb`、`--state-dir` 和 `--pipe`。

原项目的 StateDB reader 可以直接读取这里的会话与分组。启动描述保存在既有
`tool_data.native_backend` 扩展区，不保存继承环境、屏幕历史或易变的运行标识。
新记录继续使用原生 UUID；已有 Agent Deck 的旧格式 ID 保持原样，未做隐式转换。
原生管理器只操作明确声明的原生记录，采用定向事务，保留其它会话和扩展数据。
每个目录继续只有一个原生后台所有者；独立 StateDB 读取者和写入者需在后台取得
目录所有权后打开。Windows 上若仍有其它 SQLite 句柄占用文件，重新取得目录
所有权可能被拒绝，需要先关闭那些读取者。

未指定 `--store` 时保留 v0.3 的注册库，避免改变已有原型记录。两种存储互不
同步，也不会自动把现有生产 profile 或 `registry.sqlite` 导入 `state.db`。
同一目录已有另一种数据库时拒绝打开，切换接入模式需选用另一专用目录。

原来的 `session.Instance` 已有终端后端接口：原生模式下，已有预览和停止入口
调用固定运行代次的后台；原界面的 `V` 复制与 `I` 输入也复用同一接口。加载原生
描述后必须显式绑定经过核对的运行目标；尚未绑定、损坏或未知版本的描述不能
退回 tmux，也不能启动进程。当前只有进程事实时，AI 状态显示 `unknown`，不把
进程在运行解释为模型在工作。

这部分原会话和界面接入通过隔离测试验证，尚未作为完整 Windows TUI 运行。
原主程序仍需适配 Unix 文件锁、进程所有权、PTY、后台 IPC 和 POSIX 启动命令。
原生对象的原主程序启动、重启、fork、辅助 tmux 窗口和嵌入式 attach 暂时明确
拒绝；原生后台的独立 CLI 启动仍可用于验收。输入模式支持文本、常见导航与
控制键，函数键和带修饰键的导航暂时返回不支持。

原生 `PreviewFull` 读取有界保留输出（最多 4 MiB）并清除终端控制序列，不能
当作完整对话历史。以后恢复完整主程序移植时，继续复用原会话模型、列表、搜索
和业务，把平台执行放入后端。精简版继续使用已验证的 ConPTY、命名管道与进程树管理。

## 构建

最低系统要求是支持 ConPTY 的 Windows 10 1809 或更新版本。推荐在现代
Windows Terminal 内操作 `attach`。程序不会自动安装服务、计划任务、hooks、
更新检查或遥测。

在 macOS/Linux 的仓库根目录交叉编译：

```sh
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 GOTOOLCHAIN=go1.25.13 \
  go build -trimpath -o /tmp/agent-deck-native.exe ./cmd/agent-deck-native
```

在 Windows 已安装匹配版本 Go 的环境中构建：

```powershell
$env:CGO_ENABLED = '0'
go build -trimpath -o .\agent-deck-native.exe .\cmd\agent-deck-native
.\agent-deck-native.exe --version
```

非 Windows 平台可以编译和测试参数解析，但运行命名管道或 ConPTY 会明确报错。

<!-- matrix/hardened: native development has a Windows-specific isolated runner. -->
Windows 原生测试使用 `scripts/windows-native-test.ps1`；默认离线，
`-NativeIntegration` 显式开启私有 ConPTY 和后台 fixture。
工具路径、包选择、隔离与失败现场处理见
[Windows 原生开发与验证](windows-native-development.md)。

## 多会话列表

在 Windows Terminal 中直接运行：

```powershell
.\agent-deck-native.exe ui --cwd 'C:\Projects\demo' --tool codex
# 可选：只为此 UI 后续创建的新 Codex 记录选择程序。
.\agent-deck-native.exe ui --cwd 'C:\Projects\demo' --tool codex --codex-executable 'C:\Tools\Codex\codex.exe'
# 不带参数也会打开同一列表。
.\agent-deck-native.exe
```

默认注册目录为 `%LOCALAPPDATA%\AgentDeckNativeManager`，仅当前用户和 SYSTEM
可以访问。每个专用目录只有一个管理进程持有注册库；默认命名管道根据用户和目录
生成。测试或独立实例可显式指定 `--state-dir` 和 `--pipe`，所有客户端需使用同样的值。
自选目录必须是已有可信父目录下的专用目录。

首次打开列表会启动独立后台，再通过本机命名管道核对版本、运行标识和注册目录。
以后打开的窗口复用通过核对的后台，关闭窗口或其控制台进程树不会结束会话。
若已有旧版本或另一注册目录占用管道，程序明确报错；需要由用户显式关闭原后台。
后台通过 Windows 自带的本机 WMI 进程创建接口启动，不安装任务、服务或登录启动项。
启动参数与继承环境通过固定代理脚本的 stdin 传递，环境不会进入启动命令行或注册库。
[Microsoft Win32_Process.Create](https://learn.microsoft.com/en-us/windows/win32/cimwin32prov/create-method-in-class-win32-process)
说明了 WMI 创建进程与调用者 Job 的关系。

SSH 或脚本环境可以先启动后台，再管理会话：

```powershell
.\agent-deck-native.exe manager-start --json
.\agent-deck-native.exe list --json
```

| 按键 | 操作 |
|---|---|
| 上/下或 j/k | 选择会话 |
| n | 输入名称、目录、分组，选择 Shell、Codex 或 PowerShell 后创建并进入 |
| Enter 或 o | 接回运行中的会话；已停止或退出的会话显式启动，Codex 开始新对话 |
| h | 已停止或退出的原生 Codex 记录打开当前项目的历史选择界面 |
| Ctrl+Q | 从会话终端返回列表，后台进程继续运行 |
| s | 确认后停止所选进程树，保留记录 |
| r | 修改名称 |
| g | 移动所选会话到另一分组，空名称表示默认分组 |
| / | 按名称、目录、工具或分组实时搜索；Enter 保留、Esc 清除 |
| Tab / Shift+Tab | 切换全部、默认及已有分组 |
| d | 确认后停止所选进程树并移除列表记录，保留项目文件 |
| q 或 Ctrl+C | 退出列表，管理进程和会话继续运行 |

分组只是注册记录的组织方式，移动不会改变运行中的 PID、RunID 或启动参数。
搜索时列表快捷键作为文字输入，提示会切换为搜索的 Enter/Esc 操作。

列表显示进程观测状态：正在运行、已退出、已停止或异常；不会把进程运行解释为模型忙碌、
等待授权或已完成任务。列表每秒刷新一次，进入会话终端后暂停列表轮询。带运行标识
的异常实例仍可按 s 精确清理；部分启动后的清理失败不会被报告成已停止或已移除。
Codex 使用真实的原生 `codex.exe`；当前 Windows x64 查找也包含 npm 安装的
`@openai/codex-win32-x64` 路径。其他安装布局可通过 `create` 指定实际程序，或通过
`ui --codex-executable` 为后续创建的新 Codex 记录选取绝对程序路径；已有记录
继续使用自己的启动描述，重连、重新启动和历史恢复不会因此换用另一程序。

也可以用命令管理注册记录：

```powershell
.\agent-deck-native.exe create --name '工作 Shell' --cwd 'C:\Projects\demo' --tool shell --group '研发' --json
.\agent-deck-native.exe create --name '系统控制' --cwd 'C:\Projects\demo' --tool powershell --group '运维' --json
.\agent-deck-native.exe list --json
.\agent-deck-native.exe create --name '指定程序' --tool custom --cwd 'C:\Projects\demo' --no-start -- 'C:\Tools\agent.exe' '--model' 'two words'
.\agent-deck-native.exe start --session '<session UUID>' --json
# 仅恢复已停止/退出的原生 Codex；省略 run-id 时单次命令先读取当前代次。
.\agent-deck-native.exe resume --session '<session UUID>' --run-id '<run UUID>' --json
.\agent-deck-native.exe rename --session '<session UUID>' --name '新名称'
.\agent-deck-native.exe move --session '<session UUID>' --run-id '<run UUID>' --group '另组'
.\agent-deck-native.exe remove --session '<session UUID>'
.\agent-deck-native.exe shutdown
```

`create` 先保存描述再启动。启动失败时仍返回保存的 ID，退出码为失败；列表提示
“会话已创建，启动失败”，选择同一记录重试，不需要再次创建。
注册库只保存名称、分组、工具、目录、独立 argv 和显式 `--env` 覆盖；不保存继承环境、
账户配置或屏幕历史。管理进程重启后记录仍在，所有记录处于已停止状态，
程序不会自动重启 Agent。

v0.3 会在同一事务内把 v0.2 的 schema 1 升级为 schema 2，原有 UUID、启动描述、工具
和时间保留，已有记录进入默认分组。校验失败时拒绝迁移，事务失败时回滚列和版本。
需要回退 v0.2 时，应使用升级前备份的专用注册目录；v0.2 会拒绝读取 schema 2。

管理进程中的 `attach`、`send`、`capture`、`resize` 和 `stop` 使用同一管道，
并增加 `--session UUID`。每次启动分配新的 `--run-id UUID`，旧查看者的输入和
缩放请求无法送到后来启动的进程。交互查看者绑定该标识；脚本可明确指定它，
未指定的单次操作先读取当前标识再提交。

`serve` 仍可显式在前台运行，用于诊断；关闭该前台终端会结束其所属会话。
默认 UI 或 `manager-start` 创建的是独立后台，可跨客户端和 SSH 连接运行。
系统退出或管理进程意外退出仍会结束所属进程树；程序没有自动安装系统服务。
`shutdown` 清理所属进程树并关闭管理进程，保留注册库和项目文件。

## 手动运行一个 Shell 会话

以下命令均在 PowerShell 中执行。每个会话选取不同的管道名，完整路径必须以
`\\.\pipe\agent-deck-native-` 开头。后缀仅接受字母、数字、下划线和短横线；
不接受远程管道地址。

```powershell
$pipe = '\\.\pipe\agent-deck-native-demo'
.\agent-deck-native.exe host --pipe $pipe --cols 120 --rows 30 -- cmd.exe /d /q
```

`host` 在前台持续运行，启动成功后打印一行 JSON，含 `ready`、`host_pid`
和子进程 `pid`。它持有 ConPTY 和子进程，客户端断开不会结束它。
在第二个终端打开同一个目录后操作：

```powershell
$pipe = '\\.\pipe\agent-deck-native-demo'
.\agent-deck-native.exe status --pipe $pipe --json
.\agent-deck-native.exe send --pipe $pipe --text 'echo hello' --enter
.\agent-deck-native.exe capture --pipe $pipe --screen
.\agent-deck-native.exe resize --pipe $pipe --cols 100 --rows 25
.\agent-deck-native.exe attach --pipe $pipe
```

`attach` 要求输入、输出连接到交互式控制台；重定向或普通 SSH 命令可以用
`send`、`capture` 和 `status`。进入后按 Ctrl+Q 返回调用方终端，仅关闭客户端。
再次执行 `attach` 会恢复当前屏幕和光标，并按后续屏幕快照更新。后台的终端
模拟器统一回答设备查询，客户端不把原始查询转发给第二个终端。窗口尺寸每
250ms 检查一次，屏幕每 40ms 读取一次，有变化才重新绘制。

子进程退出后，host 保留最后的屏幕、有限输出和退出码，直到明确停止：

```powershell
.\agent-deck-native.exe stop --pipe $pipe
```

`stop` 在所属进程树和终端输出清理完成后确认请求，然后关闭这个 host。
`host` 自身的 Ctrl+C、关闭其终端、
系统退出或 host 崩溃也会结束会话。因此通过 SSH 启动时，SSH 登出后是否仍运行，
取决于 host 是否由独立的进程持有；前台启动不能承诺登出保活。验收工具可以显式
创建、检查并移除独立计划任务，程序本身不自动设置这些系统配置。

## 启动 Codex 或指定程序

`--` 后的第一个参数是可执行程序，剩余参数保持为独立 argv。程序不隐式拼接
Shell 命令，也不读取或修改现有 Agent Deck、Codex 或 Claude 配置。

```powershell
$pipe = '\\.\pipe\agent-deck-native-codex'
.\agent-deck-native.exe host --pipe $pipe --cwd 'C:\Projects\demo' -- codex.exe
```

上面的程序名需与本机实际安装匹配。`.cmd` 或 `.bat` 包装器需要显式指定
`cmd.exe`；例如用户已选择运行包装器时可以使用
`-- cmd.exe /d /c codex.cmd`。测试阶段优先使用真实的原生 `.exe` 路径，
带空格的路径在 PowerShell 中用引号包住。

未提供 `--env` 时继承 host 的全部环境。重复 `--env NAME=VALUE` 是覆盖，
不会丢弃 PATH；Windows 环境变量名按大小写不敏感处理。

```powershell
.\agent-deck-native.exe host --pipe $pipe --env 'AD_MARK=中文值' -- cmd.exe /d /q
.\agent-deck-native.exe send --pipe $pipe --text 'echo %AD_MARK%' --enter
```

`send --text` 发送 UTF-8 字节，`--enter` 追加 `\r`。也可以只发送 `--enter`。
需要控制字符或任意二进制字节时用 `--base64`，它与 `--text`、`--enter` 互斥：

```powershell
# Ctrl+C 字节，不是停止 host 的命令。
.\agent-deck-native.exe send --pipe $pipe --base64 'Aw=='
```

每次输入最多 64 KiB，终端尺寸为 1–500 列、1–300 行。
输入写入成功仅表示终端收到请求，不表示 Agent 已完成工作。

## 输出与验收边界

host 在内存中保留最近 4 MiB 原始输出，退出 host 后这些记录消失。
`capture --screen` 输出当前可见屏幕的纯文本，保留的原始输出由
`capture --after OFFSET` 读取。`capture --json` 返回一次最多 256 KiB
原始输出（JSON 中为 Base64），并包含以下位置字段：

| 字段 | 含义 |
|---|---|
| `start_offset` | 本次返回数据的起点 |
| `end_offset` | 本次返回数据之后的位置，下次增量读取使用它 |
| `head_offset` | 当前终端屏幕对应的全部已读取输出位置 |
| `truncated` | 请求位置早于内存保留范围，历史已被截断 |

默认的非 JSON `capture` 自动读取到首次请求时的输出位置。
输出历史截断会明确提示；`attach` 此时重新恢复当前屏幕，而不会假装恢复完整历史。

本阶段验收需要分别记录以下证据：

1. 在 Windows 原生系统实际创建 ConPTY，并启动 Shell 和真实 Codex 程序。
2. 输入 ASCII、中文、Enter 和控制字符，独立读取实际输出与当前屏幕。
3. 缩放后程序能读到新尺寸；子进程身份保持一致。
4. 关闭多个客户端后，host 和子进程继续运行；重新连接读到原会话输出。
5. 由显式独立 host 启动器运行时，SSH 登出后再登录确认 host 与子进程 PID。
6. 在可见的 Windows Terminal 内验证中文、光标、实时输入、窗口缩放和
   Ctrl+Q 断开后再接回。
7. 停止后检查该 host、子进程、管道和验收使用的任务确实结束。

交叉编译、命名管道应答或 SSH 下输出读取不能代替第 6 项的可见界面验收。
ConPTY API 存在也不能代替真实创建和交互证据。

它没有崩溃后自动恢复、完整滚动历史、查看者独占、
完整终端模式恢复或多窗口尺寸协调。多个客户端可以同时发送输入，最后一个
尺寸请求决定终端尺寸。颜色、中文、全屏界面和终端设备查询需要真机验证；
host 崩溃后的恢复与现有完整 TUI 集成属于后续阶段。

本机单元验证使用仓库规定的隔离入口：

```sh
scripts/run-isolated.py go test ./cmd/agent-deck-native ./internal/conpty ./internal/winipc ./internal/terminalhost ./internal/nativeregistry ./internal/nativefleet ./internal/nativeui
```

## Windows 自动验收工具

`scripts/windows-native-smoke.ps1` 显式建立一个独立的临时计划任务，在测试结束时
停止并移除该任务。它使用专用临时工作目录，不修改现有 Agent 配置。

```powershell
.\scripts\windows-native-smoke.ps1 -BinaryPath .\agent-deck-native.exe -ReportPath .\native-report.json
# 可选：验证真实 Codex 程序的版本和启动屏幕，使用独立 CODEX_HOME，不提交模型消息。
.\scripts\windows-native-smoke.ps1 -BinaryPath .\agent-deck-native.exe -CodexPath 'C:\Tools\codex.exe'
```

`-StartOnly` 只启动 host，返回精确的 `pipe`、`task_name`、`host_pid` 和 `child_pid`，
供关闭 SSH 后用另一条连接核对同一会话。它保留临时任务，执行时间上限为五分钟。
测试者完成核对后，用返回的管道执行 `stop`，并用返回的任务名精确停止和移除任务。
此模式的 `passed` 仅表示启动成功，不表示完整验收通过。

第二阶段使用独立的多会话验收工具：

```powershell
.\scripts\windows-native-fleet-smoke.ps1 -BinaryPath .\agent-deck-native.exe -ReportPath .\fleet-report.json
```

它创建当前 SID 下无触发器、十分钟时限的临时任务，验证两个 Shell、独立输出、
选择性停止、重启标识、旧输入拒绝、改名与仅删除描述、所属进程清理，以及同一注册库
恢复为已停止记录。测试结束精确移除本次任务，保留独立目录用于诊断。
其 `-StartOnly` 返回管理进程和两个会话的精确标识，供另一条 SSH 连接验证保活；
调用者需按返回的管道执行 `shutdown`，再停止和移除返回的任务。

## 2026-10-13 原生验证记录

目标系统为 Windows Server 2025 Standard，x64，Build 26100。

- Windows ConPTY 全套测试连续三次通过：真实 argv/env/cwd、中文与 emoji、输入、
  实际控制台尺寸、最终输出和退出码、所属子孙清理、无关进程保持、宿主意外退出，
  以及四个启动失败阶段和真实无效 PE 映像的资源回收。资源检查先预热 Go 运行时，
  再要求句柄基线和直接子进程数量准确恢复。
- Windows Named Pipe 测试通过：本机 roundtrip、用户 DACL、重复监听拒绝、连接取消、
  路径别名拒绝，以及目录 ACL 不改动已有子文件、新子文件正确继承的原生回读。
- 真实 CLI 在第二个 ConPTY 控制台中通过中文交互、DA/DSR/光标查询各回答一次、
  Ctrl+Q、原 host/root PID 保持、接回原屏幕、原生 123×35 缩放和停止清理。
- 独立计划任务中的 Shell 通过中文输出、132×42 原生尺寸、多个客户端重新连接。
  真实 Codex 0.154.0 的版本输出和启动画面均通过 ConPTY 捕获，使用独立测试主目录，
  没有提交模型消息。
- 第一条 SSH 连接结束后，第二条连接确认同一 host/root PID 仍运行并执行新命令；
  第三条连接确认停止、所属进程退出和临时任务移除。
- 本机上述四个 Go 包的隔离 `-race`、Windows amd64 `go vet` 和交叉编译通过。

这些是原生系统和控制台链路证据。可见 Windows Terminal 桌面操作尚未验收；
也未证明已登录 Codex 的真实模型回合、Claude、MCP 或完整 Agent Deck TUI 已移植。

## 第二阶段原生验证记录

同一 Windows Server 2025 目标通过以下验证：

- 注册库全套 Windows 测试：SQLite 持久化、真实子进程独占锁、中文名称、UUID、
  损坏与未来 schema 拒绝、路径别名/硬链接拒绝和用户 DACL 原生回读。
- 最新 CLI 在真实 ConPTY 控制台中操作中文列表：键盘输入名称、目录和工具，创建
  两个 Shell，执行编码命令得到中文标记，Ctrl+Q 返回列表，退出 UI 后保留两个 PID，
  新 UI 接回原屏幕和 PID，确认后停止一个而另一个保持运行。
- 重启分配新 RunID，旧 RunID 输入被拒绝；改名与移除保留项目哨兵文件，管理进程
  重启保留描述且不自动启动。失败的自动启动返回已保存的 ID，避免重复创建。
- 首次默认注册目录可在仅有 LOCALAPPDATA 父目录的环境中成功建立。
- 独立临时计划任务的完整多会话验收通过；关闭管理进程后所属根进程退出，任务
  精确移除；重新打开注册库时记录为已停止、PID 为零，原生进程表无子进程。
- 前一条 SSH 连接退出后，另一条连接确认管理进程和两个 Shell 的相同 PID/RunID，
  在原 Shell 执行新编码命令。默认 Codex 查找路径也启动真实原生程序，使用独立
  CODEX_HOME 并读取启动屏幕，不提交模型消息；随后精确停止并移除该测试描述。
- 管理器全套测试在 Windows 实际执行：并发启动一次、旧代请求拒绝、异常输出仍可
  停止、typed-nil 启动失败、部分清理失败保留记录/独占锁、独立慢停止、断开客户端
  保活、关闭等待应答排空，以及 32 会话/64 客户端上限。七个相关包的隔离 race 和
  Windows amd64 vet 通过。

上述 UI 流程是实际 Windows 控制台链路的自动化证据，仍不代表可见桌面的人工验收。

## 2026-10-14 第三阶段原生验证记录

目标仍为 Windows Server 2025 Standard x64，Build 26100。

- 真实 CLI 自动创建 WMI 后台并进入列表；关闭整个外层 ConPTY 的
  kill-on-close Job 后，管理进程和 Shell 保留相同 PID/RunID，另一个窗口接回原屏幕。
  包含中文、引号、美元符号的继承环境通过 stdin 传递，并由实际 Shell 执行验证。
- 两个同时启动的客户端取得同一个后台和独占注册库；另一目录复用同名管道时被拒绝，
  原后台保持运行。缺失父目录的启动在十五秒内明确失败。
- 真实键盘创建第四字段中的分组，操作全部/默认/命名分组筛选，搜索中文名称、目录和
  分组，Esc 清除、Enter 保留，移动分组后同一进程继续运行。独立 PowerShell 使用
  原生程序和 `-NoProfile`，执行编码命令；选择性停止与移除保留另一进程及项目哨兵文件。
- 真实 80×6 控制台的搜索界面保留 Enter/Esc 提示，n/q 作为查询文本，不会创建会话
  或退出窗口；Esc 返回列表后原快捷键恢复。
- Windows 注册库测试覆盖 v1 到 v2 原子迁移、多个工具的启动原始 JSON/UUID/时间保持、
  回滚、只读失败、损坏和未来 schema 拒绝、额外 `sqliteX...` 对象拒绝以及目录身份。
- 把第二阶段实际生成的 v1 QA 注册库复制到独立目录升级，两个原记录的 UUID、名称、
  工具、目录和时间均保留，初始无自动子进程。原 QA 注册库前后 SHA256 相同。
- 一条 SSH 连接结束后，另一条连接确认同一 WMI 后台、Shell 和 PowerShell 的 PID/RunID，
  在两个原会话执行新编码命令。默认原生 Codex 的启动画面也验证通过，使用独立
  CODEX_HOME，不提交模型消息；停止它后另两个进程继续运行，最后精确关闭本次后台。
- 完整 Windows CLI、注册库、管理器和 UI 测试通过；七个相关包的隔离 race、
  Windows amd64 vet 和交叉编译通过。测试后台和所属进程清理完成，未安装任务或服务。

本阶段增加了自动后台、分组、搜索和 PowerShell，仍未验收可见 Windows Terminal 桌面、
真实模型回合、Claude、MCP、Web 或现有完整主程序的所有功能。
