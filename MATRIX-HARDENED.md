# matrix/hardened：Agent Deck 自维护加固版交接文档

> Revanced 导出说明：本文保留原加固分支的历史开发记录。当前仓库为 `Kymorphius/agent-deck-revanced`，默认分支是 `main`；旧分支、标签、提交及远程配置描述属于导出前的来源记录。构建与隐私说明见 [REVANCED.md](REVANCED.md)。

本仓库是 [asheshgoplani/agent-deck](https://github.com/asheshgoplani/agent-deck) 的**自维护分支**，供维护者自己的 Mac 使用。接手修改前请完整读完本文。

- 分支：`matrix/hardened`（基于上游标签 `v1.16.26`，提交 `34cf369`）
- 远程：`upstream` = 上游仓库；`origin` = [Kymorphius/agent-deck](https://github.com/Kymorphius/agent-deck)（**公开**，推 `matrix/hardened` 和 `main`）。只在用户要求时推送。仓库公开，本机专属内容一律写进不入库的 `MATRIX-LOCAL.md`，推送前检查没有本机路径、网络设置、私人邮箱。
- 安装位置：`~/.local/bin/agent-deck`，版本号 `1.16.26+matrix.92`。由 `scripts/matrix-install.sh` 编译安装。
- **绝不再用 Homebrew 装 agent-deck**（`asheshgoplani/tap`），原因见下文。
- 背景记录：见不入库的 `MATRIX-LOCAL.md`（审计结论与替换过程）。

## 1. 为什么要自维护

2026-10-08 通过 Homebrew 安装原版后做了审计，程序本身可信：与 GitHub 发布逐字节一致，发布来源证明校验通过。但默认行为和本机的隐私要求冲突：

1. 约每 2 分钟请求 `api.github.com/repos/asheshgoplani/agent-deck/releases/latest`，并带上 `Authorization: Bearer <token>`。令牌按 GITHUB_TOKEN → GH_TOKEN → `gh auth token` 的顺序取，实际取到的是 gh 里权限很宽的个人令牌。
2. 自动安装每日更新的 LaunchAgent `com.agentdeck.autoupdate`，从界面、通知守护进程、无人值守更新这几条路径都会重新装回来。
3. 内置 PostHog 遥测（`eu.i.posthog.com`），首次弹出的同意框默认选"分享"。
4. `[claude] dangerous_mode`（跳过权限确认）默认开，新建会话对话框会把它写回配置。
5. 反馈弹窗用 `gh api graphql` 以用户身份把内容公开发到 GitHub Discussion。
6. 在 `~/.claude/settings.json` 装 7 个全局钩子，并会悄悄重装（界面启动、通知守护进程"修复"）；卸载时不清理。

## 2. 加固要求（R1–R8）：修改时不得破坏

| 编号 | 要求 |
|---|---|
| R1 | 默认不做任何更新检查或发布轮询；即使被重新打开，任何更新、发布请求也**永不附带 GitHub 令牌或 Authorization 头**。手动 `agent-deck update` 可以用，但不带认证，并且拒绝用原版发布包覆盖本程序。 |
| R2 | 任何路径都不安装、不重装、不"确保"自动更新的 LaunchAgent 或 systemd 定时器。只保留显式移除（`update --uninstall-timer`）。 |
| R3 | 遥测完全移除：不编译进 PostHog 密钥，发送函数是空操作、不建立网络连接，不显示同意框，忽略已存的同意记录。 |
| R4 | `dangerous_mode` 默认 false。新建会话对话框不会悄悄把 true 存进配置。用户显式设置的值照常生效。 |
| R5 | 反馈弹窗不显示，也绝不通过 gh 或 GitHub 发送任何内容。 |
| R6 | Claude 钩子只在用户显式执行 `agent-deck hooks install`（或本次明确同意）时安装；界面和通知守护进程不再"修复"或重装钩子。`agent-deck uninstall` 和 `uninstall.sh` 只删除真正运行 `agent-deck hook-handler` 的钩子（允许前面带 `VAR=值` 前缀），其它钩子保留，删除前先做带时间戳的备份；改写时保持软链接和文件权限不变。 |
| R7 | 版本号能看出是我们的构建：`<上游版本>+matrix.N`。用 `+` 而不用 `-`，因为 `-` 在版本号规则里表示预发布，会让上游正式版显得更新。 |
| R8 | 改动最小，每处都有 `matrix/hardened:` 注释说明原因；不做无关重构；只修改那些断言了被移除行为的测试。 |

所有改动都能用 `git grep -n 'matrix/hardened'` 找到。

## 3. 改动地图（v1.16.26 → matrix/hardened）

查看完整差异：`git diff v1.16.26..matrix/hardened`。核心文件如下：

**更新与令牌（R1）**
- `internal/update/update.go`：`resolveGitHubToken` 不再使用；任何请求都不附带 Authorization（约 229–244 行）。`guardHardenedSelfReplace` / `ErrHardenedSelfReplace`（约 721–800 行）拒绝用上游发布包覆盖加固版。
- `internal/session/userconfig.go`（约 1505–1625 行）：`check_enabled`、`auto_install`、`auto_update`、`auto_update_remotes`、`manage_timer` 未设置时一律视为 false。
- `internal/session/config_keys.go`：`matrixUpdatesOptIn`。第一次显式 `config set` 打开其中任一键时，先把 `check_enabled` 和 `auto_install` 都置为 false，再应用用户请求的那个键。这样原版写在磁盘上的 true 不会被当成同意，打开一个也不会顺带打开另一个。
- `internal/session/userconfig_merge.go`、`internal/ui/settings_panel.go`：设置面板的默认值与上述一致，并会正确携带 opt-in 标记。
- `internal/ui/update_auto.go`：界面不发起发布轮询（启动时、定时、手动刷新都不发）。
- `internal/ui/remote_version.go`、`cmd/agent-deck/remote_cmd.go`：远程更新时不要求远端安装或修复定时器；`remote update --install-timer` 直接报错。

**自动更新定时器（R2）**
- `internal/session/update_timer.go`：`hardenedNoAutoTimer` 总开关，`AutoEnsureUpdateTimer` 总是跳过。
- `cmd/agent-deck/update_cli.go`、`cmd/agent-deck/main.go`（约 4019–4087 行）：`--install-timer` 和 `--ensure-timer` 被拒绝或变成空操作；无人值守运行时若 `check_enabled` 关闭就直接跳过。
- `cmd/agent-deck/notify_daemon_cmd.go`：通知守护进程不再修复定时器，也不再修复钩子（原 `healClaudeHooksAtDaemonStart` 已删除）。

**遥测（R3）**
- `internal/telemetry/config.go`：`hardenedNoTelemetry`，不接受编译进来的密钥。`internal/telemetry/env.go`：`ReasonHardened`，测试之外始终关闭。`internal/telemetry/send.go`：`post()` 直接返回错误，不联网（`DefaultEndpoint` 常量只剩帮助文字里还在用）。
- `.goreleaser.yml`、`.github/workflows/release.yml`、`Makefile`：不注入 PostHog 密钥；Makefile 固定版本号。
- `cmd/agent-deck/telemetry_cmd.go`：删除卸载时的遥测（`maybeSendUninstallTelemetry`）。
- `internal/ui/home.go`：`hardenedNoPrompts`，从不弹同意框，启动时和每小时都不尝试上传。

**dangerous_mode 与反馈（R4、R5）**
- `internal/session/userconfig.go`（GetDangerousMode 默认 false）、`internal/ui/home.go`（新建会话对话框不悄悄写回）。
- `internal/feedback/sender.go`、`internal/ui/feedback_dialog.go`、`cmd/agent-deck/feedback_cmd.go`：`hardenedNoFeedback`，不调用 gh；`agent-deck feedback` 只打印"已禁用"。界面不自动弹出反馈框。

**钩子（R6）**
- `internal/ui/home.go`：`hardenedNoSilentHooks`，界面启动时不重装钩子；也不写 `~/.cursor/hooks.json`，不调用 `HealUsageFeeds`。
- `internal/session/claude_hooks.go`：`isAgentDeckHookCommand` 的判断规则：先去掉开头的 `VAR=值`，再要求第二个词是 `hook-handler`、程序名的文件名部分是 `agent-deck`。`RemoveClaudeHooks` 会扫描全部事件，并做 UTC 时间戳备份（约 387–414 行）。
- `cmd/agent-deck/main.go`（约 4845、5070 行）：`agent-deck uninstall` 会移除这些钩子。`uninstall.sh` 第 253 行附近的 jq 过滤用的是同一套规则（正则），写回时用 `cat >` 保持软链接和权限。

**版本（R7）**：`cmd/agent-deck/main.go:48` 和 `Makefile` 的 `VERSION ?= 1.16.26+matrix.81`。

**测试**：以 `_hardened_test.go` 结尾的文件、`update_check_disable_test.go`、`update_timer_wiring_test.go` 等是新增或调整过的测试；`testdata/goldens/*` 里的快照已经按加固后的输出更新。

## 4. 编译、测试、安装

```sh
cd /path/to/agent-deck
# 编译安装（版本号取最近的上游标签加 +matrix.N；产物里如果有 PostHog 密钥会拒绝安装）
scripts/matrix-install.sh 1
```

**跑测试时必须隔离**，否则会碰到真实的 `~/.claude`、`~/.config/agent-deck` 等：

```sh
scripts/run-isolated.py go test ./internal/update/... ./internal/telemetry/... ./internal/feedback/... ./internal/session/... ./cmd/agent-deck/...
```

- **matrix/hardened: 统一使用 `scripts/run-isolated.py`**（需要 Python 3）。入口在隔离配置前解析并复用 GOPATH、GOMODCACHE、GOCACHE 和工具链设置；每次运行创建临时 HOME 和短的私有 `TMUX_TMPDIR`（`/tmp/ad-it-*`，兼容 macOS 的 Unix socket 路径长度限制），裸 `tmux` 和命名 socket 都落在本次私有目录。正常结束、失败及 INT/TERM/HUP 后，先核对创建时记录的根目录设备号和 inode，通过私有目录内实际 socket 的绝对路径执行 `tmux -S ... kill-server`，确认 socket 无法连接后再回收该目录。目录被替换、socket 有多个硬链接、关停非零/超时或仍可连接时，保留目录/socket 并输出准确路径及身份信息供审核；不扫描 PID/argv 找回或强杀。清理失败时原来成功的命令返回 1，原来非零或信号退出码照常保留。不要再手写 `HOME=$(mktemp -d)` 或裸 `T=$(mktemp -d)` 测试命令。SIGKILL、断电无法触发清理；旧遗留目录需另行审核，入口不会自动删除它们。此隔离不覆盖 launchd 或显式指向私有目录之外的 `tmux -S`。

- 本机 Go 是 1.25.5，`go.mod` 要求 1.25.13，用 `GOTOOLCHAIN=go1.25.13`（会自动下载）。
- 代码规范检查：golangci-lint v2.14.0（Makefile 里固定的版本），要用 `GOTOOLCHAIN=go1.26.0` 运行；当前结果是 0 个问题。
- **已知的、与我们无关的测试失败**（上游 v1.16.26 原版同样失败）：`internal/ui` 的 TestMCPDialogFitsScreenAndNamesItsKey；`cmd/agent-deck` 的 TestCLIGoldens、TestConfigShowEffective、TestRecallHookTrigger、TestRecallSearch_Federated、两个 TestSessionHandoff* 和 TestStartupCreationRollback；`internal/session` 约 80 个，其中个别是受负载影响、时好时坏的测试。判断是不是我们引入的问题，要和上游原版的测试结果对比。
- **matrix/hardened: 隔离 HOME 挡不住 launchd。** 测试可能以当前用户身份注册 `launchctl` 任务（曾经把测试版 notify-daemon 注册成了 `com.agentdeck.transition-notifier`），跑完先做只读检查：
  ```sh
  launchctl list | grep -i agentdeck          # 检查是否出现本次测试注册的任务
  ```
  若发现遗留任务，先核实注册来源与准确 label，只撤销本次测试创建的任务。tmux 由入口按私有 socket 自动收尾；遇到 SIGKILL 等异常中断，只有核实本次 `/tmp/ad-it-*` 目录的归属后，才可对其中仍存在的 socket 使用显式绝对路径 `tmux -S /absolute/private/socket kill-server`，随后删除该私有目录。禁止按进程名、PID 或 argv 扫描结果批量杀 tmux；真实 default/agentdeck socket 和其他运行的测试目录都不属于本次清理范围。

## 5. 合并上游新版本

```sh
git fetch upstream --tags
git merge vX.Y.Z            # 在 matrix/hardened 上
```

- 冲突的高发区：`internal/update/update.go`、`internal/session/userconfig.go`、`internal/ui/home.go`、`internal/ui/update_auto.go`、`internal/telemetry/*`、`cmd/agent-deck/main.go`、`uninstall.sh`。
- 解决冲突时保留所有 `matrix/hardened:` 改动。上游如果新增了联网、遥测、钩子、定时器或自动更新相关的代码，要按 R1–R6 补上同样的处理。
- 合并后检查上游有没有新增这几类东西：发往外部的 HTTP 请求（`git grep -nE 'http\.(Get|Post|NewRequest)|net\.Dial'`）、新的遥测或分析服务、新的会自己装上的 LaunchAgent 或钩子、新的会去读 `gh auth token` 的地方。
- 改 `Makefile` 的 `VERSION`，按第 4 节跑测试，再运行 `scripts/matrix-install.sh N`。

## 6. 安装后的验收清单

```sh
agent-deck --version                                   # vX.Y.Z+matrix.N
strings ~/.local/bin/agent-deck | grep -cE 'phc_[A-Za-z0-9]{20}'   # 0
agent-deck hooks status                                # 钩子指向 ~/.local/bin/agent-deck
launchctl list | grep -i agentdeck                     # 无输出
for k in claude.dangerous_mode updates.check_enabled updates.auto_update updates.auto_install; do agent-deck config get $k; done
```

## 7. 本机环境与用户的选择

本机专属的说明（启动 Claude 的网络出口设置、安装时改过的外部配置及备份位置、用户的个人选择）放在不入库的 `MATRIX-LOCAL.md`（已加入 .gitignore），接手前请一并读完。

- **不要做**：用 Homebrew 装 agent-deck；`agent-deck update` 换成上游发布包；把令牌或 `gh` 交给它。

## 8. World Store 任务看板（我们自己加的功能）

2026-10-09 新增，2026-10-10 改接独立资料服务。任务只存在 World Store（personal-panel 仓库），agent-deck 不存副本，只在打开看板或按 `r` 时读取，用户逐项确认后才写入。

- **连接方式**：运行 personal-panel 的 `scripts/worldstore-ai.cjs`（`node`，stdin 一个 JSON 请求，stdout 一行 `{ok,result|error}`），它连本机的官方独立 Anytype 服务（没在跑时会自动启动），不需要 World Store 桌面应用。不联网、不带令牌、不后台轮询，不影响 R1–R8。旧的桌面维护 socket 和 `multica-task-bridge.cjs` 已经停用，配置指向它会直接报错并提示新路径。
- **配置**（默认关闭，`bridge` 为空即不启用）：
  ```toml
  [worldstore]
  bridge = "/path/to/personal-panel/scripts/worldstore-ai.cjs"
  # node = "node"          # 默认 PATH 中的 node
  # device_id = "..."      # 链接里本机的名字，默认主机名去掉 .local；改了会让旧链接失配
  # execution_category = "AI"   # 看板默认视图显示的「任务｜执行分类」值
  ```
- **只看交给 AI 的任务**：`任务｜分类` 是单值的场景分类（上班、生活），所以另有独立属性 `任务｜执行分类` 表示由谁来做，两者互不影响。看板默认只显示执行分类为 `AI`（取值 AI、我、世界；默认值可配置）或已关联 agent-deck 会话的任务；`a` 切到全部任务，`e` 把选中任务归入/移出（`task.update` 带原值基线）。CLI：`tasks list` 同样默认过滤，`--all` 显示全部，`--done` 显示已完成栏；`tasks mark|unmark <task>`。
- **一次性初始化**：`agent-deck tasks init --yes`（`init-links` 是别名），依次建 `任务｜执行分类` 和 `任务｜agent-deck 会话关联` 两个属性（改 World Store 结构，所以必须显式 `--yes`；每个都取空间独占租约）。没建执行分类时不能归入看板，没建关联属性时不能关联会话。
- **踩过的坑（2026-10-10）**：独立服务的引擎未实现 `ObjectTypeRelationAdd`（协议目录却标可用），搜索索引也滞后于写入。第一次建关联属性时属性建出但没附加，回执未确认，把空间独占租约固定成 uncertain，挡住该空间所有写入约 5 分钟。处理：只读核对后按既有流程把保护记录移到 `~/Library/Application Support/World Store/write-reconciliation/`；personal-panel 改为写任务类型的 `recommendedRelations` 附加、用 `ObjectShow` 读回。以后再遇到 `UNCONFIRMED`，先只读核对（`native.script` 的 inspect 模式），不要重试。
- **读取**：用 `snapshot`（任务、执行记录、卡点）。自 `matrix.80` 起分七列，与 World Store TUI 看板、Multica 一致（见第 68 节）：待规划、待办（含待处理）、进行中（含暂停、周期进行中及有未结束执行的任务，暂停的卡片标 `‖`）、待验收、卡住（由未解除卡点推出）、完成、待核（未知状态，只在有任务时显示）；已取消不显示。日期都是 Unix 秒。
- **状态改动跟随 World Store 流程**（和它自己的终端界面同一套按键）：`s` 开始做（新建一条进行中的执行记录，任务状态本身不变；每次确认生成一个固定的 operationId），`p` 暂停、`c` 继续（作用在该任务的执行记录上），`x` 完成（记完成时间，并结束它的执行记录）；验收相关的 `v` / `x` / `b` 见第 68 节。改回待规划、取消不在看板里做。同一时间只允许一条执行在进行中，看板会先拦下。World Store 要求先核对红线时返回 `REDLINE_REVIEW_REQUIRED`，看板提示到 World Store 里开始；agent-deck 不做红线核对。
- **其它按键**：`alt+t` 打开（打开时光标所在的会话就是“当前会话”），`h/l j/k` 移动，`L/U` 关联/解除当前会话，`n` 在当前会话的目录和分组里新建 claude 会话、把任务作为首条消息并自动关联，`Enter` 跳到已关联会话，`/` 筛选，`r` 重新读取。CLI：`agent-deck tasks [list|show|start|pause|resume|done|link|unlink|launch|init-links] --help`。
- **写入安全**：每次写都带读取时的 owner 和基线（行动带预期状态，关联带该属性读到的原值），不符就不写。`UNCONFIRMED` / `WRITE_UNCERTAIN`（超时、中断、回读不符）表示可能已写入，看板锁住所有写操作直到重新读取，绝不自动重试；进程在写入途中被杀也按“可能已写入”处理。`BUSY` 表示同一记录正在别处保存，什么也没写。导入对象只读。
- **改动地图**：`internal/worldstore/`（客户端、分栏、launch）、`internal/ui/kanban_panel.go`、`internal/ui/kanban_home.go`、`cmd/agent-deck/tasks_cmd.go`、`internal/session/userconfig_worldstore.go`（新文件）；对上游文件只加了钩子行：`internal/session/userconfig.go`（`WorldStore` 字段，必须在结构体里，否则 SaveUserConfig 重写 config.toml 时会丢掉这一节）、`internal/ui/home.go`（字段、SetSize、按键路由、View、hasModalVisible、热键分支、updateInner 开头的消息分发，共 7 处）、`internal/ui/hotkeys.go`（`task_board` = `alt+t`）、`cmd/agent-deck/main.go`（`tasks` 分发和 commandRegistry）。合并上游时这些钩子行都带 `matrix/hardened` 注释。
- **personal-panel 侧**：AI 接口的 `task.links.inspect` / `task.links.initialize` / `task.link` / `task.unlink`（`src/headless/ai-session-links.cjs`、`src/headless/client.cjs`），复用 `src/native-task-execution-links.cjs` 写独立属性，与 Codex 关联互不影响；`task.fields.inspect` / `task.fields.initialize`（`src/native-task-relation-setup.cjs`，只开放 `任务｜执行分类`），`task.update` 允许改执行分类；`src/native-game-snapshot.cjs` 读取这两个属性。协议见该仓库 `docs/ai-interface.md` 的「执行分类」「agent-deck 会话关联」。
- **测试**：`go test ./internal/worldstore/ ./internal/ui/ -run Kanban`；personal-panel 里 `npm test`（含经真实原生适配层的关联端到端测试）。真实空间上只做过读取；真实写入（开始、暂停、完成、关联、新建会话）没有在真实空间上测试过。

## 9. 修过的上游 bug

- **从全局搜索（`G`）接回 Claude 对话后会话立即退出**（`1.16.26+matrix.2`，2026-10-09，上游 main 当时仍未修）。`internal/ui/home.go` 的 `createSessionFromGlobalSearch` 把 `exec claude --resume <id> <权限参数>` 整条存进 `inst.Command`；启动时 `buildClaudeResumeCommand` 把不是 `claude` 的 Command 当作自定义程序，再包一层，结果是 `exec exec claude --resume <id> … --resume <id> …`，不到 1 秒就退出（`spawn_died_fast`）。修法：和 CLI 的 `add --resume-session` 一样，只设置 `ClaudeSessionID`，Command 保持 `claude`，resume、`CLAUDE_CONFIG_DIR` 和权限参数都交给启动路径拼。测试：`internal/session/matrix_global_search_resume_test.go`、`internal/ui/matrix_global_search_resume_test.go`。修复前已经接回、命令被写坏的会话，用 `agent-deck session set <会话> command claude` 修正。
- **从搜索接回 Claude 对话时在错误目录新开了一段空对话**（`1.16.26+matrix.3`，2026-10-09）。全局搜索（`G`）和 `recall open` 用 recall 索引里的 `cwd` 作为恢复目录，而这个 `cwd` 是对话**最后**所在的目录（Claude 会跟随 Bash 工具里的 `cd`；`internal/recall/ingest/ingest.go` 的 `Session` 后写覆盖前写）。`claude --resume <id>` 只能在对话**启动**目录下找到它（`<config>/projects/` 下的项目目录按启动目录命名），找不到时启动路径退回 `--session-id <id>`，结果是在无关目录用同一个 ID 开了一段空对话，还在那个目录的项目目录里留下同 ID 的小文件。修法：新增 `internal/session/matrix_resume_dir.go` 的 `LocateClaudeResumeDir`，在所有已知 Claude 配置目录里找 `<id>.jsonl`，取最大的那份，返回其中编码后等于所在项目目录名的那个 `cwd`（不一定是第一个：从别的目录续上的对话保留着旧目录的记录），找不到这样的 `cwd` 的副本不能恢复，跳过；找不到或启动目录已不存在就报错，不再猜目录。`G`（`createSessionFromGlobalSearch`）和 `recall open`（`recallOpenPlan`）都改用它；对话文件 2 分钟内被写过（`ClaudeConversationLiveWindow`，多半还开在别的终端里）时拒绝接回，避免两个 Claude 进程写同一段对话。测试：`internal/session/matrix_resume_dir_test.go`、`internal/ui/matrix_global_search_resume_test.go`、`cmd/agent-deck/recall_cmd_test.go` 的 `TestRecall_OpenPlansResumeOrStart`（夹具改成真实的项目目录命名和结束于子目录的对话）。修复前误开的会话和留下的小文件要手动清理：`find ~/.claude/projects -name '<id>.jsonl'`，只保留在启动目录对应项目目录里的那份，然后 `agent-deck recall rebuild`。

- **新会话发送首条消息时 tmux 不带 `-u`**（2026-10-09）。`buildClaudeCommandWithMessage` 给新 Claude 会话发首条消息（`launch --message`、任务看板 `n`）的后台片段在窗格里直接跑 `tmux display-message / capture-pane / send-keys -l`，是全仓唯一没带 `-u` 的 tmux 调用（上游 #1867 要求每个 tmux 调用都带）。窗格环境来自 tmux 服务器，agent-deck 从 launchd 或 web 服务启动时没有 UTF-8 区域设置，中文首条消息就可能被当成非 UTF-8。修法：三处都改成 `tmux -u`。测试：`internal/session/matrix_first_message_utf8_test.go`。同一轮也把接管（`session adopt`）的裸 `tmux` 调用改走 `tmux.ExecContext`，见第 12 节。

## 10. 从搜索接回 Codex 线程（我们自己加的功能）

2026-10-09 新增。上游只能从 recall 索引接回 Claude 对话，Codex 线程只能搜、不能接（"searchable but not resumable yet"），除非已有 agent-deck 会话拥有它。现在 `recall open <会话>` 和 TUI 全局搜索（`G`）回车都能接回 Codex 线程：

- **做法**：新建一个 codex 会话，设 `CodexSessionID`，启动时由 `buildCodexCommand` 拼出 `codex resume <线程ID>`。`add` 的 `--resume-session` 也接受 `-c codex`（线程 ID 必须是裸 UUID），并写入 harness 为 `codex` 的权威 recall 关联，之后 `recall open` 变成启动这个会话。TUI 里命中的线程已被某会话占用时，直接跳到那个会话。
- **Codex 主目录必须对上**：`codex resume` 按 ID 在 `CODEX_HOME` 下找线程，与启动目录无关；但 `buildCodexCommand` 在启动用的主目录里找不到 rollout 时，会**悄悄丢掉 ID、开一段新对话**（#756）。所以 `session.LocateCodexResume`（`internal/session/matrix_codex_resume.go`）只在新会话将使用的主目录里找 rollout：索引行的 `profile` 就是 rollout 所在主目录对应的账号（`[profiles.<名>.codex].config_dir`，默认主目录为空），新会话带上同一个 `--account`。找不到就报错，不猜。`add` 本地会话同样先检查。
- **启动目录**：rollout 里最后一条 `turn_context`（或 `session_meta`）的 `cwd`，即线程最后的工作目录，避免 codex 恢复时询问换目录。目录已不存在就报错。
- **和 Codex App 同时开着（无缝接入）**：本机的 codex 客户端（App 和命令行都算）都经 app-server 后台服务（`codex app-server --managed-daemon`）运行，后台服务是对话记录唯一的写入方，并持有 `~/.codex/thread-writer-locks/<ID>.lock`；加载过的对话即使没人连着也一直占着这把锁。所以多个客户端可以同时挂在同一段对话上，消息实时互相可见，**前提是接入时不改任何配置**：只要 `codex resume` 带了覆盖参数（`-c …`、`--model`、`--yolo` 等），codex 就要求独占，显示 "This conversation is open in another app. Close it there and press R"（2026-10-09 实测，codex 0.161 客户端连 0.162 后台服务）。agent-deck 平常每次都会加身份注入 `-c developer_instructions=…`，因此接回的会话在 `CodexOptions` 里打上 `shared_thread` 标记（`session.MarkCodexSharedThread`），`buildCodexCommand` 遇到这个标记只拼 `codex resume <ID>`，不加 yolo、模型、推理强度和身份注入。代价是这类会话没有 agent-deck 的身份说明。`recall open`、`G` 和 `add -c codex --resume-session` 新建的会话都打这个标记。不再像 Claude 那样拒绝"2 分钟内写过"的对话：最初的版本这样做，结果正开在 App 里的对话一律接不上。
- **拒绝的情况**：子代理线程（`thread_source=subagent`，codex 不接受在上面输入，报错里给出父线程 ID）；rollout 文件已删除。
- **改动地图**：`internal/session/matrix_codex_resume.go`（新文件）；`cmd/agent-deck/recall_cmd.go`（`recallOpenCodexPlan`、帮助文字）；`cmd/agent-deck/main.go`（`add --resume-session` 的校验、rollout 检查、关联 harness、flag 说明）；`internal/ui/home.go`（`handleGlobalSearchSelection` 的 Codex 分支、`createCodexSessionFromGlobalSearch`）；`internal/session/tooloptions.go`（`CodexOptions.SharedThread`）；`internal/session/instance.go`（`buildCodexCommand` 的共享对话分支）；goldens `help_add.stderr.golden`、`help_recall.golden`。
- **测试**：`internal/session/matrix_codex_resume_test.go`、`cmd/agent-deck/recall_cmd_test.go` 的 `TestRecall_OpenResumesCodexThread`、`internal/ui/matrix_global_search_resume_test.go` 的 `TestMatrixGlobalSearch_CodexHit`，以及 `TestMatrixCodexSharedThreadResumesWithoutOverrides`（共享对话的启动命令不带覆盖参数）。真机验证（2026-10-09，手动）：用一个 `--remote unix://` 客户端占住一段测试对话、模拟 App 开着，再用单独的 agent-deck 配置 `add -c codex --resume-session` 加 `session start`。agent-deck 启动的是 `codex resume <ID>`，没有锁屏；用 `session send` 发的消息在两边实时出现，只写进同一个记录文件。

## 11. 列表的 vim 操作、Alt+Q 断开（我们自己加的功能）

2026-10-09 新增。会话列表里数字只当 vim 计数（另见下面的行号和 `:N`）：`3j` / `3k`（也可用 ↑/↓、`ctrl+n/p`）一次移动 3 行，计数可多位（`12j`、`10k`，上限 999），没有超时，按其它键作废。

- **跳根分组改成 `'` 加数字**（`'3` 跳到第 3 个根分组）。原来单按 `1`–`9` 跳分组，和计数冲突，用户选了纯 vim。`'` 后面不是 1–9 时，`'` 作废，那个键照常生效。分组行左侧原来的 `N·` 已去掉（它标的是一个已经不跳分组的键，而且和行号、展开箭头挤成 `5 2·▾`），改为在分组名和数量后面暗色显示真正的按键 `'N`（如 `▾ Tools (4) '2`），选中的分组也显示。
- **`0`**：有计数时追加一位（`10j`）；没有计数时照旧清除状态筛选。`!@#$` 等筛选键不受影响。
- **行号和 `:N`**：列表每一行左边显示行号（vim 的 `:set number`），分组标题也编号，视图模式的分隔线不编号。按 `:` 在底部帮助栏位置打开命令行，`:12` 回车跳到第 12 行（行号超出范围时报错、光标不动），`:set nonu` / `:set nu` 隐藏或显示行号（只在本次运行有效，不写配置），`Esc` 或空命令行上的退格取消。行号只在普通列表里画；内嵌侧栏（embedded layout）的卡片高度随宽度变，为了不影响鼠标点击和滚动，那里不画行号，但 `:N` 照样能用。各行渲染时宽度减去行号列，再在前面补行号。
- **`''` 跳回**：回到上一次 `'N` 或 `:N` 跳转之前所在的行，再按一次又跳回去（和 vim 一样来回切）。只有这两种跳转会记位置，`j/k`、翻页不记；跳转没让光标动（比如 `'9` 而没有第 9 组）也不记。记的是那一行的会话或分组，列表折叠、刷新后照样找得到；窗口行和远程行按位置找。和上游的 `` ` ``（直接进入上一个会话）不同，`''` 只移光标。
- **Alt+Q 断开会话**：进入会话后，除了 Ctrl+Q（或配置的 detach 键），Alt+Q 也能回到列表，本地会话和远程（ssh/mosh）会话都行。终端发 Alt+Q 的方式不同：Option 设为 Esc+ 时是 `ESC q`，增强键盘协议下是 `ESC[27;3;113~` / `ESC[113;3u`，iTerm2 默认（Option 为 Normal）时是字符 `œ`，这几种都认。只有这一次读取正好是这个按键时才断开：先按 Esc 再按 q 是两次读取，粘贴含 `œ` 的文字是括号粘贴、按 32 字节分块读，都不会误断开。代价是在会话里打不出单个 `œ`。内嵌侧栏模式（embedded layout）走 Bubble Tea 的按键，那里没加。代码：`internal/tmux/matrix_alt_detach.go`（`MatrixIsAltQDetach`），挂在 `internal/tmux/pty.go` 的 `resolveAttachInterrupt` 开头和 `internal/session/ssh.go` 的 `sshAttachInput.forward` 开头。
- **macOS Option 键当 Alt**：iTerm2 / Terminal.app 默认把 Option 设为 Normal，这时 Option+W 到达程序的是字符 `∑` 而不是 alt+w，列表里所有 alt+键（`alt+w` web 服务面板、`alt+t` 任务看板、`alt+a`、`alt+j/k`、`alt+1-9` 等）都按不出来（2026-10-09 在日志里确认：按 Option+W 收到的是 `∑`）。`normalizeMainKey` 现在先把 US/ABC 布局下 Option+字母/数字 产生的字符映射回 alt+键（`internal/ui/matrix_mac_option.go`），只影响会话列表；搜索框、对话框和 `:` 命令行照样收到原字符。Option+E/I/N/U/` 是死键，单按不发送任何东西，所以 `alt+u`（接管面板）这样按不出来；要用它，把终端的 Option 键设成 Esc+。测试：`internal/ui/matrix_mac_option_test.go`。
- **等待中的提示**：按了 `'` 或数字（计数）后，底部帮助栏换成一行提示（`'` → 按 1–9 跳到该根分组；`3` → j/k 移动 3 行），完成或按别的键后恢复。原来单按 `'` 屏幕上什么都不变，看起来像没反应（2026-10-10 用户反馈）。
- **中文输入法**：Rime（薄荷拼音）中文模式下 `'` 打出 `「`/`」`、`:` 打出 `：`，别的输入法打出 `‘` `’`。列表里把这几个字符当成 `'` 和 `:`（`matrixIMEKey`，和 Option 字符映射在同一处），中文模式下 `'2`、`:12` 也能用。
- **改动地图**：`internal/ui/matrix_nav_count.go`（新文件，`matrixHandleNavPrefix`）、`internal/ui/matrix_line_numbers.go`（新文件，行号列和 `:` 命令行）；`internal/ui/home.go`（`navCount` 字段、`Update` 里紧跟 jump mode 的 `:` 命令行路由、`handleMainKey` 在 switch 前调用前缀处理、`up/k` 与 `down/j` 按计数循环、`renderSessionList` 的行号列、`View` 里帮助栏替换；原 `1`–`9` 分支保留但已不可达，注释说明）；`internal/ui/help.go`（帮助里 `'1-'9`、`3j / 3k`、`:12`、`''`、`Alt+Q`）。合并上游时注意上游若改了数字键、`'`、`:` 键的绑定或 `renderSessionList` 的循环。
- **测试**：`internal/ui/matrix_nav_count_test.go`、`internal/tmux/matrix_alt_detach_test.go`、`internal/session/matrix_alt_detach_test.go`；`group_nav_test.go` 的 `TestGroupNav_Regression_Plain1_JumpsToFirstRootGroup` 改为按 `'1`。列表快照（`testdata/funccheck/`、`testdata/viewers/`、`tree-*` 帧）已用 `UPDATE_GOLDEN=1` 重新生成，去掉行号列后与原快照逐行一致。

## 12. 接管已在运行的 Claude（我们自己加的功能）

2026-10-09 新增。用户自己在 tmux 里开的 Claude（不是 agent-deck 建的会话）可以不中断地交给 agent-deck 管：`agent-deck session adopt <tmux会话>`，`--list` 列出候选；远程用 `agent-deck remote <名> session adopt …`；TUI 里 `alt+u` 打开列表（光标在远程分组或会话上时列远程的），回车后按 `y` 确认才接管。

- **怎么找**：Claude Code 每个进程在 `<配置目录>/sessions/<pid>.json` 写一个状态文件（`sessionId`、`cwd`、`kind`、`status`）。扫描默认 tmux 服务器和 `[tmux].socket_name` 的服务器（`--socket` 可指定），用 `ps` 的父子关系把 Claude 进程对到窗格。登记的目录用 `LocateClaudeResumeDir` 找到的恢复目录，找不到才用状态文件里的 `cwd`。
- **怎么接**：登记一个 claude 会话（绑定对话 ID、`TmuxSocketName` 指向原服务器），再把 tmux 会话改名成新记录要的名字；进程不动。先改名、后保存，保存失败就改回原名。和 `add` 一样持有注册锁。
- **拒绝的情况**：已被 agent-deck 管（同名同服务器的 tmux 会话，或同一对话 ID 已登记）；tmux 会话里不止这一个窗格（agent-deck 管整个 tmux 会话）；同一对话还有别的活着的 Claude 进程（两个进程写一段对话会写乱）；目录不存在；目标名字在该服务器上已被占用。
- **注意**：接管后在 agent-deck 里删除这个会话会连同 tmux 会话和 Claude 进程一起杀掉；原来的 tmux 会话名没了（`tmux new -A -s 原名` 会新建一个空的）。tmux 列表输出的分隔符必须是可打印字符：没有已连接客户端时（所有经 SSH 的命令）tmux 会把控制字符改成 `_`。
- **tmux 调用**：所有 tmux 命令经 `tmux.ExecContext`（`adoptTmux`），显式 `-L <socket>`（用户自己的服务器是 `default`）。最初版本直接执行 `tmux`，`TestNoRawTmuxExec_OutsideAllowlist` 报错；改用工厂后多了 `-u`（非 UTF-8 环境下中文等非 ASCII 会话名不会被印成 `_`，否则按名字匹配会失败）和测试时不碰真实默认服务器的保护。
- **改动地图**：`internal/session/matrix_adopt.go`、`cmd/agent-deck/matrix_adopt_cmd.go`、`internal/ui/matrix_adopt_panel.go`、`internal/ui/matrix_adopt_home.go`（新文件）；`cmd/agent-deck/session_cmd.go`（`adopt` 分发和帮助）、`cmd/agent-deck/remote_exec.go`（允许转发 `session adopt`）、`internal/ui/hotkeys.go`（`adopt_panel` = `alt+u`）、`internal/ui/help.go`、`internal/ui/home.go`（字段、消息分发、SetSize、按键路由、hasModalVisible、热键分支、View，共 7 处）。
- **测试**：`cmd/agent-deck/matrix_adopt_cmd_test.go`（真 tmux、隔离 socket、`sleep` 代替 Claude、手写状态文件；验证接管后窗格 pid 不变、同一对话重复打开时拒绝、多窗格拒绝）、`internal/ui/matrix_adopt_panel_test.go`。

## 13. 用 `M` 移动分组（我们自己加的功能）

2026-10-09 新增。上游 TUI 里 `M` 只能移动会话，光标在分组行上时按 `M` 没有反应；`K`/`J` 只能和同一层的分组交换顺序。把分组挪到别的父分组下只能用命令行 `agent-deck group change`。现在光标在本地分组行上按 `M`，会打开同一个移动对话框（标题 "Move Group"），选择新的父分组，或者 `(root)` 移到最顶层，回车确认。子分组和组内会话随它一起移动，分组自己的展开/折叠状态保持不变；目标父分组如果是折叠的会被展开，让光标能停在移动后的分组上。

- **引擎**：`GroupTree.MoveGroupTo`，和 `group change` 用的是同一个。
- **候选列表**：`(root)` 排第一，其余按树的顺序排列；不包括分组自己、它的子孙，以及它当前所在的父分组。目标位置已有同名分组时不列出（`MoveGroupTo` 也会拒绝）。有分组范围（scope）时不提供 `(root)`：移到最顶层就出了范围。
- **拒绝的情况**：默认分组（`my-sessions`）；没有可去的地方；远程分组（给出提示，让用户在远端运行 `agent-deck group change`）。
- **重载竞态**：和改名一样登记在 `pendingGroupOps`（`groupOpMoveGroup`），保存失败后重载时补做；源分组不在了，或已经在目标下面，就不做。
- **改动地图**：`internal/ui/matrix_group_move.go`（新文件）；`internal/ui/group_dialog.go`（`moveGroupSource`、`ShowMoveGroup`、`SelectedMoveGroupParent`、标题和 `(root)` 显示）；`internal/ui/home.go`（`groupOpMoveGroup` 常量和补做分支、`M` 的分组和远程分组分支、对话框回车的分组移动分支、分组行底栏的 `M Move` 提示）；`internal/ui/help.go`（GROUPS 里的 `M`）。
- **测试**：`internal/ui/matrix_group_move_test.go`。

## 14. 查看和控制 `agent-deck web` 服务（我们自己加的功能）

2026-10-09 新增。手机用的 `agent-deck web --no-tui` 是用户自己写的 LaunchAgent / systemd 单元，agent-deck 不安装它。`agent-deck webservice [status|start|stop|restart] [--json]` 读取它在服务管理器里的状态、探测 `<url>/healthz`，并让服务管理器启动、停止或重启它。`remote_exec.go` 放行了 `webservice`，所以也能对远程主机执行。

- **配置**（默认关闭，`manager` 和 `service` 都填了才启用）：
  ```toml
  [webservice]
  manager = "launchd"              # launchd | systemd | systemd-user
  service = "<launchd label 或 systemd 单元名>"
  url = "http://<地址>:8420"       # 可选：探测 /healthz
  # plist = "..."                  # launchd：默认 ~/Library/LaunchAgents/<service>.plist
  # sudo = true                    # systemd：用 `sudo -n systemctl` 控制
  ```
- **行为**：launchd 的 `stop` 是 `bootout`（卸载，避免 KeepAlive 立刻拉起），`start` 在未加载时用 plist `bootstrap`，`restart` 是 `kickstart -k`；操作后最多等 10 秒，直到状态反映出来。健康探测是一次不带凭据的 GET，不走环境里的代理、不跟随重定向（R1/R3 不受影响）。
- **TUI**：`alt+w` 打开「WEB SERVICE」面板，显示服务名、状态、pid、healthz 应答和延迟；`s` 启动，`x` 停止、`R` 重启（这两个要按 `y` 确认），`r` 重新读取，`Esc` 关闭。光标在远程分组或会话上时管的是远程主机的服务（经 SSH 运行远程的 `agent-deck webservice`，所以远程也要装上带这个命令的版本、配好 `[webservice]`）。面板只是调用 CLI，两边说法不会不一致。
- **顶栏标记**：本机配了 `[webservice]` 时，顶栏多一个 `web ●`（绿：在跑且 healthz 应答）、`web ◐ no reply`（黄：进程在但不应答）、`web ○ stopped`（红）、`web ? unknown`（读不到状态）。后台每 30 秒读一次（在进程内直接调用，不起子进程跑 agent-deck），面板里的操作结果会立即更新它；顶栏太窄时它最先被省略。改了配置要重启 TUI 才会出现。
- **测试**：`internal/session/matrix_webservice_test.go`（假的 launchctl/systemctl，覆盖解析、bootstrap/bootout/kickstart、sudo、失败信息、探测不带 Authorization）、`internal/ui/matrix_webservice_panel_test.go`（面板确认流程、远程目标、未配置、顶栏标记）。
- **改动地图**：`internal/session/matrix_webservice.go`、`cmd/agent-deck/matrix_webservice_cmd.go`、`internal/ui/matrix_webservice_panel.go`、`internal/ui/matrix_webservice_home.go`（新文件）；`cmd/agent-deck/main.go`（分发和 commandRegistry）、`cmd/agent-deck/remote_exec.go`、`internal/session/userconfig.go`（`WebService` 字段，必须在结构体里，否则 SaveUserConfig 会丢掉这一节）、`internal/ui/hotkeys.go`（`web_service` = `alt+w`）、`internal/ui/help.go`、`internal/ui/home.go`（字段、消息分发、SetSize、按键路由、hasModalVisible、热键分支、View、Init 启动和退出时停止后台读取、顶栏 optionalSegments，共 10 处）。

## 15. 异常中断的分类（我们自己加的功能）

2026-10-09 新增，2026-10-10 扩展到 Codex 并加了查看入口（`matrix.22`）。回合没做完就停下的会话，原来看上去和正常做完一模一样：灯变 waiting，看不出活只干了一半、需要说"继续"。

- **Claude**：回合因 API 错误中途结束时（`API Error: Connection lost mid-response`、`Connection dropped (ECONNRESET)`、`Can't reach the API server (ENOTFOUND)`、过载、账号被暂停等），Claude Code 不触发 `Stop`，只重画一个空输入框。窗格上那行横幅认不出来：它是普通的 `⏺` 回复行，没有 #1400 要求的 ` · ` 分隔符，措辞也和普通文字重合。改读对话记录，和上游判断用量超限（#1802，`usagelimit.go`）是同一个来源：主对话（不含子代理）里最新一条用户或助手记录，如果是 `isApiErrorMessage: true` 的助手消息，就是中断；之后只要出现任何用户记录（手打的"继续"、用量重置后自动续跑的提示），就算已经过去了。2026-10-09 用本机全部对话记录核对过：83 条 API 错误记录，每条后面紧跟的都是用户记录；每个错误点都能识别。
- **Codex**：窗格显示 `■ Conversation interrupted - use /feedback if something went wrong`，rollout 里记的是 `event_msg` 的 `turn_aborted`（`reason: "interrupted"`）。当前 Codex 也会在 `task_complete.error` 记录模型容量错误（见第 51 节）；它不属于普通主动中断。判断：最新一个回合事件（`task_started`/`turn_started`、`task_complete`/`turn_complete`、`turn_aborted`、`user_message`）是 `turn_aborted`。插话（steer）打断后会马上开下一轮，回"继续"会写 `user_message` 和 `task_started`，两种都会清掉标记。rollout 不记是谁打断的（按 Esc、共享对话的另一个客户端、app server），所以详情只写 Codex 给的原因。子代理线程跳过：父代理打断它们是正常调度。rollout 用 `CodexRolloutPathForInstance` 找（按会话的 `CodexSessionID` 和 Codex 主目录）。2026-10-11 核对本机 2634 个 rollout：22 个主线程停在被打断上，201 个子代理线程按设计跳过。
- **子状态** `turn-interrupted`（`matrix.18` 时叫 `api-interrupted`，只管 Claude），粗状态（running/waiting/idle…）不变。`substate_detail` 是原因：Claude 是错误种类加信息，例如 `server_error: Connection lost mid-response.`、`rate_limit: You've hit your session limit · resets 11:30pm (Asia/Shanghai)`；错误种类就是记录里的 `error` 字段（Claude Code 2.1.295 的枚举：authentication_failed、oauth_org_not_allowed、account_on_hold、verification_required、billing_error、rate_limit、overloaded、invalid_request、model_not_found、server_error、unknown、max_output_tokens、cloud_credential_error）。Codex 是 `turn_aborted: interrupted`。
- **优先级**：只在窗格没有更具体结论时套用（窗格是 none、`idle-at-empty-prompt` 或 `hook-lag`）。窗格显示正在运行、打开的菜单、后台任务、401 或模型不可用横幅、Codex 的用量或登录横幅时，保留窗格的结论。实时的 `Substate()` 里上游的 `usage-limit`（5 小时内的 429）仍然优先。
- **统一异常入口（2026-10-13，matrix.58）**：TUI 的 `$`（兼容 `&`）从只筛 error 扩展为 **Abnormal**：包含 error、waiting/idle 下的中断，以及登录、模型、额度诊断。顶部固定显示 `✕ Abnormal N`，零时也保留；每个会话只计入一个主展示分类，异常行不再同时增加 waiting/idle 计数。`~` 视图里的 `↯ N` 是异常中的中断子集，不能与主计数相加；普通视图只显示统一主入口。`~` 保留只看中断，error + turn-interrupted 也包含在内，粗状态变化不再使它掉出两个入口。running 的旧诊断不算异常；停止、归档保留生命周期语义。远程筛选/计数使用同一判定，离线缓存不增加当前计数。Open 默认隐藏异常，沿用 `active_filter_excludes` 的 error 配置。
  - 明确的 auth/model/usage 原因优先显示，保留原来的诊断；只在实际 substate 为 turn-interrupted 时显示 `↯`。这是展示与筛选统一，未修改原始 Status、JSON 协议或 runtime 记录结构；中断结果和错误原因仍未拆成两个独立持久字段。
  - 改动地图：`internal/ui/matrix_turn_interrupt_filter.go`（共享展示分类、本地/远程筛选与中断子集计数）、`connection_status.go`（诊断图标）、`home.go`（主计数、固定入口、空列表和远程过滤）、`help.go`；测试 `matrix_abnormal_filter_test.go`（重叠去重、error 转换、恢复运行、归档、Open 排除、远程与空入口）；更新 `state_visibility_test.go` 和筛选栏的 25 个界面快照。
  - 验证：统一异常、中断、图标、计数、窄屏可见性、置顶视图、Active 和相关快照测试通过。UI 整包在 `TestBackgroundStatusPassOwnershipLinear` 超时，未修改的 matrix.57 同样复现；MCP 布局和 Claude 创建限流测试也在该基线失败。整包通过不作为本次验收声明。
- **在哪里看**：
  - TUI 列表里 waiting/idle/error 的中断行显示红色 `↯`。running 行不显示：判断结果每几秒才刷新一次，新回合可能已经开始。
  - 顶部筛选栏固定有 `✕ Abnormal N` 入口，按 `$` 看全部异常，按 `~` 只列出被中断的会话（此时显示 `↯ N` 子集计数），再按一次 `~` 或按 `0` 回到全部。没有时显示 "No interrupted turns"。帮助页（SEARCH & FILTER）和筛选栏的按键提示（`!@#$~ filter`）里都有 `~`。归档的会话不算。
  - `agent-deck status -v` 显示 `[turn interrupted: <原因>]`；`list --json`、`session show --json`、web 接口带 `substate`/`substateDetail`；web 指挥中心面板显示 `turn interrupted`；内嵌侧栏显示子状态名。
- **开销**：`updateStatus` 释放锁之后（defer）刷新，每个会话最多 5 秒读一次，从文件末尾往回读，碰到第一条起决定作用的记录就停（最多回读 8 MiB，超过 1 MiB 的行不读，直接当作"无法判断"）。TUI 渲染路径（`CachedSubstate`）、筛选和计数只读内存里的结果。只对本地 Claude 和 Codex 会话做；SSH 会话和其它工具跳过。和上游的状态筛选一样，筛选开着时列表不会因为后台状态变化自动重排，计数每 0.5 秒更新。
- **已知局限**：Claude 中断后执行本地斜杠命令（`/cost`、`/model`）也会写一条用户记录，会把标记清掉，尽管任务并没有继续。Codex 的"被打断"包括你自己按 Esc。没有改通知：父会话收到的"子会话完成"事件里带着 `substate`，但投递逻辑没有因此改变。`tools/visualcheck` 的快照没有重新生成（我们分支一直没维护它）。
- **改动地图**：`internal/session/matrix_turn_interrupt.go`（新文件：Claude/Codex 扫描、缓存、`SubstateTurnInterrupted`）；`internal/session/instance.go`（`matrixInterrupt` 字段、`updateStatus` 的 defer、`Substate`/`CachedSubstate` 套用、`SubstateDetail`，共 4 处）；`internal/ui/matrix_turn_interrupt_filter.go`（新文件：`~`、`↯ N`、空列表提示、`matrixShowsTurnInterrupted`）；`internal/ui/connection_status.go`（`↯`）；`internal/ui/home.go`（渲染快照的 `matrixArchived`、三处筛选改调 `matchesSessionFilter`、`~` 键、筛选栏计数和按键提示、`statusFilterLabel`、空列表，共 9 处）；`internal/ui/help.go`；`cmd/agent-deck/cli_utils.go`（`SubstateLabel`）；`cmd/agent-deck/main.go`（`status -v` 附原因）；`internal/web/static/app/panes/CommandCenterPane.js`（标签）。
- **测试**：`internal/session/matrix_turn_interrupt_test.go`（Claude 和 Codex 的真实记录形状：错误后跟 `turn_duration`、之后有用户记录、子代理错误、超长行、跨多个读取块、改名后缓存失效、插话、Codex 子代理线程、经 `CODEX_HOME` 的完整刷新）、`internal/ui/matrix_turn_interrupt_glyph_test.go`、`internal/ui/matrix_turn_interrupt_filter_test.go`（计数不含 running 和归档、`~` 来回切换、空列表）。筛选栏多了 `↯ 0` 和 `~`，`internal/ui/testdata` 下的 active_filter、update_banner、runtime_health_warning、devanagari 快照已用 `UPDATE_GOLDEN=1` 重新生成，差别只有这两处。

## 16. 网页终端的图片按钮（我们自己加的功能）

`1.16.26+matrix.5`（提交 2d7128dc）新增。手机没法把截图放进会话。终端顶栏（手机上是会话标题行）有 Image 按钮：浏览器把图片原样 POST 到 `/api/sessions/{id}/image`，服务端存到 `<profile 目录>/uploads/<会话 id>/`（0600）并返回绝对路径，前端用 `terminal.paste()` 把路径贴进窗格，Claude Code 把它变成 `[Image #n]` 附件。

- **限制**：只收 PNG/JPEG/GIF/WebP（按内容判断类型，不信 Content-Type），最大 20 MB；上传时顺手清掉该会话 7 天前的图片。门槛和终端输入一样：令牌、CSRF、只读模式、web mutations、限速。只支持本地会话。
- **改动地图**：`internal/web/handlers_matrix_image.go`（新文件，路由 `POST /api/sessions/{id}/image`）、`internal/web/static/app/terminalImage.js`（新文件）、`api.js`（`uploadSessionImage`）、`TerminalPanel.js`、`icons.js`（image）、`app.css`（`.term-strip-btn`）。
- **测试**：`internal/web/handlers_matrix_image_test.go`、`tests/web/unit/terminalImage.test.js`。

## 17. 手机和折叠屏展开屏的网页布局（我们自己加的功能）

`1.16.26+matrix.17`（提交 a9f997ba）新增。

- **触屏平板模式**（coarse pointer，宽 721–1100px）：折叠屏展开是 724px，刚超过 720px 的手机断点，原来拿到的是固定 300px 侧栏的桌面布局，终端只有约 45 列。现在侧栏变成抽屉（顶栏 ☰ 按钮，选会话或换标签后自动收起），右栏隐藏，顶栏标签可横向滑动，底栏只占一行且不显示键盘提示。
- **手机和触屏平板**：隐藏终端小横条，会话短 id 和 Image 按钮放到会话标题行；触屏上取消残留的 `:hover` 高亮，改用 `:active`。
- **触摸滚动**（`terminalTouchScroll.js`）：开着鼠标上报时，xterm 把小于 50px 的滚轮量乘 0.3，手机上内容只跟手指三分之一的速度移动，而且松手即停。现在按行高换算手指位移，每行发一个 line 模式的滚轮事件，松手后有惯性。
- **tmux latest**：`Resize` 额外给 attach 客户端发 SIGWINCH（同 `tmux.HandLatestToViewer`）；页面切回前台或点终端时发一次同尺寸 resize（2 秒节流）。手机切回来不再看到按桌面宽度截断的窗格。
- **改动地图**：`internal/web/static/app/app.css`（TOUCH TABLET、COMPACT TERMINAL HEAD、NO STICKY HOVER 三段）、`uiState.js`（`sessionDrawerOpenSignal`）、`Topbar.js`（`.sdrawer-btn`、`.rail-toggle`）、`AppShell.js`（遮罩、选中即收起、标题行 `.sid` 和 `.wh-image`）、`terminalImage.js`（picker 注册、`imageUploadingSignal`）、`terminalTouchScroll.js`（新文件）、`TerminalPanel.js`（触摸滚动、抢 latest）、`icons.js`（menu）、`internal/web/terminal_bridge.go`（Resize 发 SIGWINCH）。
- **测试**：`internal/web/terminal_bridge_matrix_claim_test.go`、`tests/web/unit/terminalTouchScroll.test.js`。

## 20. Codex API 等价费用

`1.16.26+matrix.23` 新增：TUI 和 headless web 每 30 秒读取受管理 Codex 会话的原生 `token_usage_record`，使用会话自身的账号、CODEX_HOME 和精确 thread id 查找日志；不猜最近的文件，不读取远程或容器路径。`costs sync` 支持相同来源的历史回填。

- 输入拆成普通输入、缓存读取及缓存写入，推理 token 已包含在输出，不能再加一次。
- 响应 ID 跨重复回填、recall 和 fork 重放去重；唯一键插入处理多实例竞争。
- 日志读取按完整行增量推进，保留当前模型；首次全读后只读新增内容。旧日志只有累计 `token_count` 时不估算。
- GPT-6.1 Sol / GPT-6 Sol / Astra / Luna、GPT-5.6 Sol/Luna 和 GPT-5.3 Codex 使用官方标准 API 单价；适用模型超过 272K 输入按长上下文费率。自定义价格按原配置计算。Fast、Ultrafast、地域附加费不计入这项标准 API 等价估价。
- `$` 页面注明这是 API 等价估价，不是订阅账单或额度。
- 验证：`internal/costs/codex_matrix_test.go`、`cmd/agent-deck/costs_codex_matrix_test.go`，覆盖缓存拆分、回填和 fork 去重、模型切换、缓存输入请求、不完整行、长上下文以及本地/远程精确来源隔离。

## 21. 新建界面路径候选布局

`1.16.26+matrix.24` 修复新建分组的路径候选遮挡输入，以及新建会话历史路径和 Tab 补全因长路径撑宽菜单的问题。

- 候选菜单从实际显示的输入行下方展开，宽度限制在表单内；高度随输入下方的剩余终端行数调整，选中项保持可见。
- 长路径按完整字符簇缩略前半段，保留末尾目录名；接受候选仍使用完整原始路径。
- 长父分组和远程名称先按表单宽度换行，避免挤压路径输入。多仓库模式的视口跟随正在编辑的条目。
- 验证：`internal/ui/matrix_path_dropdown_test.go`，覆盖 80×24、100×30、120×40、长中文和 emoji 路径、历史候选与真实目录 Tab 补全、多仓库编辑、受限高度下的选中项和完整路径接受。原有新建会话、分组、视口和布局快照测试保持通过。

## 22. 费用页模型列表与滚动

`1.16.26+matrix.25` 将模型费用列表移到会话排行上方，按模型名称固定排序，避免 Go map 在每次重绘时改变行顺序。全部模型保留；终端放不下时通过滚动查看。

- `↑/↓` 或 `j/k` 滚动，鼠标滚轮也可使用；`PgUp/PgDn` 或 `Ctrl+B/F` 翻页，`Ctrl+U/D` 半页，`Home/End` 跳到首尾。
- 费用页随终端尺寸调整；窄窗口先换行再计算滚动范围，返回提示固定在底部。
- 验证：`TestMatrixCostDashboardStableOrderAndScroll` 覆盖 40 个模型的稳定顺序、末项可达、翻页及窄窗口尺寸。

## 23. Recall 的管理状态标记

`1.16.26+matrix.26` 修复 Recall 搜索结果没有显示管理状态的问题。原来的圆点标记未接入真实界面，且缺少 Codex 原生线程 ID 匹配；选中行还会覆盖圆点。

- 每条结果在标题下显示 `[已管理]` 或 `[未管理]`，选中后仍保留。右侧预览明确显示当前列表的管理状态，已管理结果还显示对应管理会话 ID。
- 标记范围是当前 profile 的会话列表；Recall 跨 profile 的历史绑定不等于当前列表仍有管理会话。每次渲染重新核对当前列表，加入或移除会话后不会保留旧标签。
- 显示与 Enter 共用匹配逻辑：先匹配明确的管理会话 ID，再按 harness 匹配 Claude 对话 ID 或 Codex 线程 ID；空原生 ID 不匹配任何未设置的字段。
- 验证：`TestMatrixRecallManagement_*` 覆盖真实 Home 渲染、Codex 匹配、跨 harness、空 ID、陈旧绑定、移除后的重算以及显示和 Enter 目标一致；Recall 和对话框快照验证终端宽度与结果滚动。

## 24. 工具身份保持启动配置

`1.16.26+matrix.27` 修复 Codex 启动后的状态刷新把会话类型写成 Claude 的问题。默认身份注入会在完整启动命令前加 `export`，并在说明参数中包含 Claude 的 CLI 示例；原识别器认不出前导命令时扫描全文，优先匹配 `claude`，随后状态保存把误判写入工具配置。

- **matrix/hardened:** 命令识别只接受实际程序位置和明确支持的 npm 启动包；参数、提示词、路径中的工具名不参与全文匹配。无法确认的复杂 shell 包装不据此猜测工具。
- **matrix/hardened:** 后台状态刷新只允许空身份或 `shell` 初次识别为支持的工具，已经配置的内建和自定义工具保持不变。用户显式修改工具、命令和跨工具切换仍使用原来的保存路径。
- 回归覆盖默认 Codex 注入命令形状、shell 包装、说明文字中的其他工具、已识别运行时保持，以及状态刷新后数据库回读与显式工具编辑。
- 部署需核对 TUI 和 Web 实际加载的构建；只替换磁盘文件不能保证旧进程立即使用修复。管理界面支持原地重载，tmux 中的 AI 进程无需重启。

## 25. 仅在已管理会话中 Recall

`1.16.26+matrix.28` 新增 `Alt+R`，直接打开当前 profile 已管理会话范围内的内容搜索；macOS 默认 Option 模式下的 `Option+R` 同样有效。`G` 仍搜索全部历史，`/` 仍是标题和路径等信息的快速筛选。

- **matrix/hardened:** `[hotkeys].recall_managed` 可重绑或禁用。界面标题与输入提示明确显示已管理范围，Enter 只定位已有会话；搜索后移除的会话不会被重新导入。
- 范围在打开界面时从已登记会话中取得，匹配管理会话 ID 或 Claude/Codex 原生对话 ID；空范围返回零结果。新增会话后重新打开此界面即可纳入搜索。
- SQL 在正文候选上限与最终结果排序截断之前限定范围，高排名的未管理历史不会挤掉已管理结果。ID 集合用绑定 JSON 数组查询，避免大量会话耗尽参数数量。
- 每次打开和编辑查询都更新异步请求代次，同一关键词在全量与已管理范围间切换时，不接受旧范围的结果。
- 验证：`TestMatrixManagedScope_*` 覆盖候选上限、结果上限、空范围、跨 harness、标题匹配与大 ID 集合；`TestMatrixManagedRecall_*` 覆盖默认键、重绑禁用、Mac Option 字符、真实索引、范围切换与移除后纯定位；既有 Recall、快捷键、帮助、分组导航和宽度测试保持通过。

## 26. 新建会话默认参数与分组路径候选

`1.16.26+matrix.29` 将 Codex 放在 Claude 前面；新 profile 没有配置或历史选择时优先 Codex，显式 `default_tool` 仍优先。

- **matrix/hardened:** 本地新建会话提供 `[ Save as default (^D) ]`，可用 Tab 聚焦后 Enter 保存，或按 Ctrl+D。只保存参数，不创建会话；保存成功显示 `Defaults saved (Path excluded)`，失败显示实际错误。
- 默认参数保存在 `config.toml` 的 `[new_session_defaults]`，并同步 `default_tool`。保存当前工具、模型、推理强度、YOLO、worktree、Docker 和多仓库开关，以及适用工具的启动选项；保留其它配置。
- 不保存项目路径、仓库列表、会话名、具体分支、父会话或 resume ID。每次打开重新确定路径，多仓库开关开启时也只以本次路径初始化；worktree 分支根据新会话名生成。远程目标继续使用远端配置，不接收本机默认参数。
- 分组路径候选改为表单内展开，输入和选中项一起参与高度适配，操作提示保留在候选框下方；窄宽度提示按完整项目取舍，避免浮层盖住底部及按键提示截断。
- 验证：`matrix_new_session_defaults_test.go` 的冷打开、完整路径排除、其它配置保留、按钮与 Ctrl+D、Claude 参数、远程隔离及 64×28 / 80×24 / 60×15 / 100×30 底部提示；既有路径候选、新建表单键盘流程和布局快照测试。

## 27. Shift 数字键与中文标点筛选

`1.16.26+matrix.30` 修复 Shift+1 偶尔无法切到运行中列表的问题。

- **matrix/hardened:** CSI u 编码可能报告数字键与 Shift 修饰，原来仅转换字母，导致 Shift+1 被当作普通 `1` 的移动计数。现在补上 US/ABC 数字与标点的 Shift 映射；含 Ctrl 时不套用此映射。xterm modifyOtherKeys 使用已经生成的字符码点，保持其数字和标点原样，避免破坏其它键盘布局的输入。
- **matrix/hardened:** 中文输入法输出的全角 `！＠＃％＾＆～` 在列表快捷键入口映射到对应筛选键；`＄` / `￥` 查看异常（matrix.66 起），费用页改为 `Alt+$`，`＊` 对应时间筛选。此映射只应用于列表命令。
- 验证：`matrix_shifted_keyboard_test.go` 覆盖两个协议的解析及 reader、数字与标点、未 Shift 数字、Alt、Shift+Enter 和控制键；`matrix_status_filter_keyboard_test.go` 经 `Home.Update` 验证运行中列表、再次按键取消筛选、待处理的移动计数与分组前缀、Ctrl+Shift 不触发 Shift 筛选，以及全角快捷键和原始文本保持。

## 28. 创建分组可选目标设备

`1.16.26+matrix.31` 的创建分组界面显示 Target，并提供 Local 与已配置远程设备选择；光标位于远程行时仍默认该设备，但不会将创建目的地锁在远程。

- **matrix/hardened:** Ctrl+T 循环切换目标，也可 Shift+Tab 从 Name 聚焦 Target，再用左右键、Space 或 Enter 切换。Enter 在 Target 行只切换设备，不创建分组；Tab 返回 Name。
- 切换设备清空 Path 与父分组，保留输入的名称；回到原设备可恢复 Root/Subgroup 上下文，默认仍为 Root。Local 始终存在，即使列表里只有远程分组。
- 小终端对新增目标行使用布局裁剪，保留当前输入和底部操作提示。
- 回归验证覆盖仅远程行切到 Local 后实际创建本机分组、多目标键盘选择、原设备上下文恢复、重新打开状态重置，以及既有远程创建和路径候选布局。

## 29. 减少历史日志查找与空闲推送开销

`1.16.26+matrix.32` 优化多个看板与 headless Web 并行运行时的后台 CPU 开销。

- **matrix/hardened:** `codexRolloutPathInHome` 缓存已经找到的日志路径，键包含 Codex 主目录与线程 ID。缓存命中只检查文件是否存在，不再遍历整个 `sessions/YYYY/MM/DD` 历史树；日志删除或移动后重新查找。尚未写出的日志不缓存缺失结果，首次写出后仍能立即找到。
- **matrix/hardened:** 推送服务在零订阅时跳过每 3 秒的全量会话快照刷新，并清空状态基线。最后一个订阅移除后立即重置基线，重新出现首个订阅者后先建立新基线，随后正常发送状态变化；订阅读取失败保留原基线，订阅保存成功立即请求一次同步。在途快照和旧状态变化不能覆盖重订阅后的基线，网络发送在锁外执行。
- 保持既有会话监测频率和中断判断。`claim_polling` 仍为可选配置：共享状态行尚不包含完整子状态与详情，不将它作为本轮优化的前提。
- 回归：`matrix_codex_rollout_cache_test.go` 覆盖跨主目录隔离、首次写出、删除与移动、并发以及稳定路径的 100 次查询仅遍历一次；`matrix_push_idle_test.go` 覆盖空订阅、重新订阅、计数或写入失败、并发移除边界和真实文件存储。使用第 4 节的隔离入口运行，另验证既有 Codex 中断与子代理判断、完整 Web 包和新回归的 race 检查。
- `BenchmarkMatrixCodexRolloutPathLookup` 在 120 个合成日期目录、12,001 个日志文件上对比原始遍历与缓存命中；这是路径查找基准，不能直接当作整机或完整看板 CPU 降幅。
- 部署后重启 Web 服务；已打开的 TUI 在会话列表按 `Ctrl+T` 原地重载。核对实际运行版本后再做 CPU 对比，安装磁盘文件不等于旧看板已加载优化。
- **matrix/hardened:** 安装脚本的钩子状态摘要使用 `sed` 读完完整输出后显示前三行，避免 `head` 提前退出使成功安装在 `pipefail` 下报 SIGPIPE；真实钩子状态错误仍会传播。

## 30. 所有尺寸移除网页终端的重复窗口头

`1.16.26+matrix.35` 将终端内的三色圆点、完整会话 ID 和重复 Image 横条从组件中移除。此前 `matrix.17` 仅在手机和触屏平板隐藏横条，桌面仍然显示。

- **matrix/hardened:** 所有尺寸统一使用已有会话操作栏的 Image 按钮；文件选择器仍由 `TerminalPanel` 持有并注册，上传、粘贴及只读门槛沿用现有路径。
- **matrix/hardened:** 移除横条的样式，桌面 Image 按钮不再受手机媒体条件限制。终端获得原横条占用的高度。
- 验证桌面、平板和手机的真实文件选择器事件，切换到 Costs 后按钮隐藏，返回 Terminal 后仍可打开；只读操作栏继续隐藏。真实页面验收需加载新构建后再核对。

## 31. 网页打开时减少重复解析与进程查询

`1.16.26+matrix.36` 优化 headless Web 定期重建会话对象时的重复工作，保留原有状态刷新节奏。

- `matrix_turn_interrupt_cache.go`：按工具和实际日志路径共享文件解析结果，包含 Codex 线程来源与中断判断。文件身份、尺寸、修改时间和支持平台的变更时间一致才复用；追加、截断、替换后重新读取。正常没有中断的结果也复用，IO 失败与未完成记录继续重试。读前后签名不一致不发布缓存；保留会话重绑的 generation 守卫。缓存有容量上限，同一路径合并并发读取；不缓存缺失的 rollout 路径。
- `StatusUpdatePass` 的进程快照：同一轮内共享主机的 PID、父 PID 和完整参数，减少每个 Codex 会话分别运行全表 `ps` 及逐 PID 查询。仍分别读取会话的 tmux pane 和进程文件；快照不跨刷新轮次，失败或缺失进程保留原有回退与不完整信号。
- `system_stats_cache.go`：每个 Web Server 按需缓存系统统计两秒，多个页面的相邻请求共用一次采集；从采集完成时刻计时，不启动额外定时器，不跨服务器共享。不可用指标仍省略，权限与方法检查先于采集。
- 验证包含日志变更/替换/首次 flush、账号目录隔离、并发读取、进程表失败与候选识别，以及系统统计过期/权限/并发。通过后使用隔离脚本完成相关测试，再安装并重启 Web；以相同网页打开状态采样实际服务 CPU。

## 32. 多网页共用一次采集与广播

`1.16.26+matrix.37` 引入按需工作的 Web 菜单快照发布器。REST、网页 WebSocket、兼容 SSE、Command Center、Push 和其它活跃会话读取共用同一份采集结果。

- **matrix/hardened:** 发布器统一执行菜单读取、hook 状态覆盖和注释合并；各连接只负责传输和心跳。菜单指纹与编码按发布计算，增加网页连接不会增加采集循环。
- **matrix/hardened:** `matrix.38` 的内部传输订阅直接复用不可变编码，不为每个连接解码菜单 DTO；需要 DTO 的 REST 和普通订阅继续获得私有数据，纯菜单输出复用编码。Command Center 基础构建只读发布器拥有的数据。
- **matrix/hardened:** 网页使用一条只推送状态的 `/ws/dashboard` 连接，复用菜单和 Command Center 事件，避免每页两条 SSE 占用普通 HTTP 连接池。握手复用既有鉴权与 Origin 检查；应用层输入被拒绝，终端输入仍走原有会话连接。
- **matrix/hardened:** Service Worker shell cache 升至 v23，让 PWA 缓存随新连接实现刷新；页面重载后读取新入口，断线时保留有退避的自动恢复。
- **matrix/hardened:** Command Center 基础状态按每次成功发布懒构建一次，多个连接和 REST 共用文件读取、合成与编码；各连接的完成通知独立合并。新发布会重新读取基础状态，即使菜单内容未变，conductor 文件变化也能显示。
- **matrix/hardened:** 首个订阅者启动唯一循环，最后一个订阅者离开后停止；没有订阅时 REST 按需读取并合并并发请求。Push 没有有效订阅时不维持采集需求。
- **matrix/hardened:** 每份发布拥有自己的数据，消费者不能改动其它窗口的快照。采集失败返回错误，失效期间旧 generation 不能提交；会话操作会唤醒一次共享刷新。
- **matrix/hardened:** 慢连接使用有界待发送状态，采集不等待网络写入。完成通知另做有界观察与积累，保留已观察的回合变化；超出积累上限明确要求重新同步或记录失败。
- **matrix/hardened:** 受既有鉴权保护的 `GET /api/diagnostics/menu-publisher` 提供采集次数、订阅数、错误与运行状态计数，供多窗口验收；不在产品界面展示实现细节，不返回会话或凭证。
- 验证 1/4/16 个订阅者每次发布共用的采集与合成次数、失效与重订阅、慢连接完成事件、错误恢复、只推送 WebSocket 的权限/输入边界和关闭清理；本机安装后核对真实映射构建、AI pane 身份和 1/4 网页连接的稳定 CPU/采集计数。

## 33. 终端、Web 与通知共享本机采集

`1.16.26+matrix.39` 将生产 TUI、Web 和通知守护进程接入同一用户的本机采集服务。多个窗口保留自己的输入、几何和选中预览，完整会话状态由服务统一采集。协议与边界见 `internal/monitor/README.md`。

- **matrix/hardened:** 使用独立 Unix socket、独占 owner lock、0700 目录、0600 socket 与相同 OS UID 的对端校验；不复用执行命令的 core daemon，不新增 LaunchAgent、更新任务、网络端口或钩子安装。
- **matrix/hardened:** 首次读取按实际可执行文件启动服务；仅缺失或拒绝连接时自动启动，协议、权限和数据错误保持可见。`agent-deck monitor serve|status --json|stop` 管理这一个采集服务；停止它不会停止 AI 会话。
- **matrix/hardened:** 按 profile 保留长期 Instance、观察者和原生身份；定向写入只使用所属数据库。没有消费需求时暂停采样，状态与事件按有界历史共享；增加消费者不会增加采集器。
- **matrix/hardened:** 协议包含子状态及详情、完整 hook 回合证据、后台工作、认证 hold、活动与等待时间、确认状态、模型、原生绑定、fork 能力和逐行采样时间。未知或失败有明确标记，不用零值假装已观察。
- **matrix/hardened:** 本地操作时间与持久注册变更时间拒绝旧采样；状态、原生绑定、确认以及相关 sidecar 写回受数据库 generation 校验。慢探针保留唯一在途调用，不能反复生成 goroutine；身份变化后旧结果不发布。
- **matrix/hardened:** 已配置消费者不会在服务故障时各自重启全量采样。TUI 保留上次状态并显示重试提示；短暂断线保留历史游标，明确缺口重新建基线。历史错误发布作为未知边界推进，恢复不会卡在同一错误记录。
- **matrix/hardened:** 终端输出只唤醒本窗口读取已有观察；实际操作才请求新采样。窗口心跳、通知交互与预览保持，既有 idle timeout 和 conductor compact 动作由 profile 的 primary TUI 执行。
- **matrix/hardened:** 安装后需分别核对 Web、monitor 以及 TUI 的实际运行版本。旧 TUI 仍运行旧构建，列表上的 `Ctrl+T` 原地重载后才接入共享服务；AI tmux pane 无需重启。
- 验证包含多个 Unix 消费者共用一次采集、暂停与唤醒、有界历史和大小边界、失败恢复、同 ID 的 profile 隔离、慢调用与代次、确认与新回合、完整状态及消费者零全量探针；使用第 4 节隔离入口。真实验收另核对构建映射、socket 权限、采集计数、稳定 CPU 与 AI pane 身份。

## 34. 本地会话永久删除

`1.16.26+matrix.40` 增加普通删除之外的永久删除入口。主界面选中会话按 `d`，在确认框按 `p` 打开界面内的预览，再输入 `DELETE` 并回车确认。CLI 为 `agent-deck remove --purge <id>`；`--dry-run` 只预览，`--yes` 显式确认，JSON 模式要求这两者之一。

- **matrix/hardened:** `matrix.45` 将预览和确认留在 Bubble Tea 弹窗内，异步读取时显示进度；Esc/Ctrl+C 取消，长列表用方向键/PgUp/PgDn/滚轮查看。不再切换终端或启动交互式子进程，避免按 `p` 后黑屏。过期预览结果不重新打开弹窗，删除中的按键不会泄漏到主列表；错误需关闭后重新预览。
- **matrix/hardened:** UI 和 CLI 共享基于具体 tmux pane 和真实 socket 的终端保护；实际删除前拒绝杀掉承载自身的会话，另一个服务器上同名会话仍可删除。
- 支持本地、非容器的 Claude/Codex 会话；先按当前及历史原生 ID 定位，再检查各 profile 的共享引用。远程、混合 harness 历史或共享对话拒绝执行。
- 停止目标进程后清理原生日志、Claude 会话快照/子代理文件、原生历史 JSONL 中匹配的行及 Codex 线程数据库行；保留项目/worktree 文件。
- 删除 recall 的目标及其子代理索引、FTS、费用、最近配置项、提示/标签、通知与钩子记录；仅保留不含正文的 deck ID 删除标记，阻止旧进程恢复会话/费用。
- 历史 JSONL 匹配行原位置空，保持 inode、文件长度和其他进程的追加写入。跨文件和数据库无法形成单一事务；任何步骤失败均报告未完成，不能当作成功。
- 不清除其他 fork 中已复制的正文、外部备份、云端数据或物理存储残留，也不提供安全擦除保证。
- 验证：`TestPermanentDelete*` 使用隔离环境里的合成日志和数据库，覆盖预览、明确确认、共享引用/计划变化/外部符号链接拒绝、邻居数据及追加历史保留、索引清理和费用防复活。

## 35. 多窗口共享 Codex 成本采集

`1.16.26+matrix.41` 为实际 profile 数据目录中的 Codex 成本采集建立进程生命周期排他锁。所有窗口继续读取费用数据库，只有持锁进程解析原生日志；新增窗口不再各自从头解析完整历史。

- **matrix/hardened:** 锁目录为 0700，稳定锁文件为 0600；校验目录和文件身份、所有者、链接数及权限，拒绝不安全路径。锁中 PID 仅供诊断，排他性由内核 flock 保证，关闭时不删除锁文件。
- **matrix/hardened:** 非持锁窗口每 30 秒重试接管；持锁进程退出后内核释放锁，等待窗口可继续采集。读取与数据库关闭完成后才释放锁，取消和重复停止保持安全。
- **matrix/hardened:** 持锁进程保留完整首次读取和增量 tail，沿用请求 ID 去重，不通过跳至日志末尾丢弃历史费用。只读会话存储使用已经解析的 profile，不增加状态采样或会话写回。
- **matrix/hardened:** `matrix.42` 的成本读取先使用标准 JSON 解码识别记录类型，只将模型和用量记录送入原解析逻辑；不解码、拼接或分类聊天正文和工具输出。Recall 的完整对话解析与日志扫描边界保持原样；费用事件与原解析器做差分核对。
- **matrix/hardened:** 普通 CLI 的认证状态写删也要先匹配数据库中的当前启动身份，再在相同数据水位事务内执行；旧 CLI 不能借用替换会话的新水位修改其认证 hold。
- 验证：隔离 race 覆盖 1/4/16 消费者只进行一次首次读取、关闭前拒绝接管、真实子进程退出接管、profile 隔离、取消、稳定 inode 及不安全路径；并运行既有费用完整性、增量读取和去重回归。真实验收分别记录启动与稳定 CPU，核对运行构建、唯一持锁进程及 AI pane 身份。

## 36. 在原目录 fork 对话

`1.16.26+matrix.46` 增加 `Alt+F`，直接在选中会话的目录 fork 原生对话，不创建 worktree 或分支。`f` 保留现有快捷分叉默认值，`Shift+F` 保留可调整选项的对话框。

- **matrix/hardened:** `[hotkeys].fork_in_place` 默认 `alt+f`，可以重绑或设为空禁用；macOS Option+F 的 `ƒ` 同样可用。帮助及会话预览显示当前绑定。
- **matrix/hardened:** 原目录入口覆盖 `[fork]` 的 worktree、with_state、with_ignored 和 inherit_from_parent 所产生的目录创建选项，保留既有启动参数与 Docker 规则；新会话共享实际文件，使用独立对话 ID。快捷入口共用创建中与 fork 中的守卫。
- **matrix/hardened:** 远程入口使用目标主机已有的 `session fork`，不传 `-w`。CLI 默认 fork 和 Web Fork 本来就在原目录运行。
- **matrix/hardened:** 从已登记 worktree fork 时，五种原生工具的底层 builder 继承共享目录元数据；显式新 worktree 或不同目录继续使用指定目标。普通目录不被标记为 worktree。
- **matrix/hardened:** 多仓库 `Alt+F` 复用原聚合目录与各仓库路径，原生启动命令也在该聚合目录执行；保留已有启动选项或全局默认值。删除时逐一保护共享 worktree 及目录，最后一个使用者先清理 worktree 再移除聚合目录。普通多仓库 `f` 的链接直接指向实际仓库，避免父目录链接被删除后失效。
- **matrix/hardened:** CLI 普通删除、带 prune 的删除、worktree finish 和 Web finish 保留仍由其他会话使用的 worktree 与分支；只解除当前会话登记。最后一个使用者按既有流程清理，finish 的 `branch_deleted` 反映实际结果。
- 验证使用第 4 节的隔离入口：快捷键默认值、重绑与禁用、原生 fork 参数、独立会话、新实例的原生会话目录、实际 tmux 工作目录、源文件与 Git 暂存区/分支/worktree 清单不变，以及真实 Git 上共享目录的删除顺序。tmux 启动验证使用不联网的 Pi 工具替身；真实工具的对话内容由其原生 fork 能力继承。
- 安装后，已打开的 TUI 在会话列表按 `Ctrl+T` 重载新版；AI pane 不需要重启。

## 37. 复用稳定终端帧与限额日志事实

`1.16.26+matrix.47` 减少共享采集服务的持续解析开销，保留既有采样节奏、状态分类、确认和事件时间逻辑。

- **matrix/hardened:** 每个 tmux Session 有界保存一份终端文本准备结果及一份规范化结果；相同内容和检测配置复用 ANSI 清理、背景任务/roster 处理与动态字段规范化。工具、启动状态、配置、内容及生命周期变化会失效；Regexp 通过公开 Copy 快照比对，支持相同表达式的 POSIX/Longest 模式变化。
- **matrix/hardened:** 缓存只保存文本与结构事实。新的成功 capture 即使字节相同，仍执行分类、确认/历史状态和完成样本记录；捕获失败不会提供旧 busy 证据。
- **matrix/hardened:** Claude 限额日志按实际 canonical 路径及文件身份、大小、mtime、mode 和可用 ctime 复用，最多 1024 项，同文件并发合并。打开的文件及读前后签名都匹配才缓存；未知、失败或不完整记录继续重试。每次按当前时间重新判断 5 小时有效期，保留原有 5 秒节流与会话代际防护。
- 验证包含 14 项日志缓存与 7 项终端文本缓存回归及既有状态测试，使用第 4 节隔离入口；真实验收另外记录相同窗口配置下的稳定 CPU、采集次数、加载构建、WebSocket 连接及 AI pane 身份。组合测试中的既有后台任务 fixture 和短时 mutation 计时边界需与同一未修改基线对照，不把单例复测冒称整个组合通过。

## 38. 本地 tmux 查询计数

`1.16.26+matrix.48` 为共享采集服务提供实际 tmux 子进程的本地累计指标，用于核对批量查询前后的开销。

- **matrix/hardened:** 既有命令执行入口只统计实际启动，以固定命令类别汇总启动、完成、错误、等待壁钟时间及已等待客户端的 CPU 时间。构造命令、缓存命中和重复执行同一 Cmd 不重复计启动；异步 Start 的完成耗时不在同步指标覆盖内。
- **matrix/hardened:** 指标仅由同用户 Unix socket 的 `monitor status` 返回，不保存参数、会话、socket 名、路径或终端内容；不新增定时器、磁盘历史、外部遥测或网络接口。
- 等待壁钟总和可能包含并行重叠，不能当成进程 CPU。客户端 CPU 不含父进程 fork 或常驻 tmux server 的开销；这些仍需分别采样。实际查询分类和 CPU 接受结论由运行计数验证，不能把采样栈命中当作调用次数。

## 39. 共享采集服务批量获取终端截图

`1.16.26+matrix.49` 将同 socket 的少量只读截图查询合并到一次 tmux 命令队列，减少子进程启动，保留原状态采样节奏。

- **matrix/hardened:** 每组最多四个已保留 worker 的探针，同 socket 共享一次原生调用；跨 socket 独立运行，等待 worker 前先启动已保留的小组，不能形成自锁。没有新的 attach 客户端、窗口尺寸更改或控制连接事件流。
- **matrix/hardened:** 截图沿用原 `capture-pane -t Session.Name -p -e` 目标，包含当前窗口/窗格语义。独立 argv 的只读命令用随机边界分帧，ANSI 和空行逐字节保留；非零、超时、额外/缺失/重复边界、随机源失败或超过 8 MiB 均拒绝整批并回退原逐会话捕获。
- **matrix/hardened:** 仅预热现有 raw capture cache，500ms 有效期从批次开始计算；过期不种入。普通捕获、Fresh 和失效的 acquisition/completion generation 拒绝旧批次覆盖新观察，已有独立采集的会话不参加批次。Fresh 继续独立子进程、绕过缓存；每次分类、确认、历史及完成时间动作沿用原探针。
- 验证使用隔离入口：完整 monitor race、11 项 batch 和既有 capture/Fresh/发送验证/ACK/substate 回归；真实私有 tmux 核对跨 socket 同名、选中非首窗口、原始字节、子进程计数及 pane PID/尺寸保持。生产另做含计数的稳定采样，比较每轮查询数和 CPU，保留动态负载与错误回退的限制。

## 40. 并发饱和时合并相邻查询

`1.16.26+matrix.50` 在实际采集的并发槽位饱和时，给未凑齐的截图小组一次至多 3ms 的合并机会，减少每次只空出一个槽位就发出独立查询的情况。

- **matrix/hardened:** 合并窗口按小组累计，不为每个新槽位重置。小组占满全部 worker、采集预算耗尽、取消、队列结束或超时时立即启动；保留原 worker 上限、逐 ID 在途与退休代次、观察时间及跨 socket 隔离。没有空闲后台定时器，也不改变采集周期或截图缓存有效期。
- **matrix/hardened:** 私有 `monitor status` 提供固定大小的逻辑小组和实际 socket 分组大小、启动原因及等待计数，不记录目标和内容。分组计数不等于原生进程数，仍与既有 tmux 执行指标一起验证。
- 验证包含 1/2/3/10 worker 的自锁保护、相邻完成合并、超时/取消/预算、快慢 socket、普通不需截图探针以及既有 monitor 代次与公平性回归；真实采样再核对分组分布、每轮启动数、CPU 和快会话延迟。

## 41. 共享采集器的常驻只读 tmux 查询通道

`1.16.26+matrix.51` 仅在用户级 monitor 所有者启用专用查询通道。各 TUI、Web 和普通 CLI 消费共享观察，保留各自原生回退；不复用终端观看连接。

- **matrix/hardened:** 有需求的 profile 按稳定注册顺序提供已有会话作为 anchor，每个有效 socket 至多一个自有 Control Mode 客户端。首次适合的读查询才连接；需求过期、anchor 替换或 collector 关闭时只回收自有客户端。profile、socket 和等待队列分别限制为 16、32 和每 socket 32；没有周期重连定时器。
- **matrix/hardened:** 连接使用 `attach-session -E -f ignore-size,no-output,read-only`，不创建会话、不发送终端输入、不运行观看策略、不调整尺寸、不更新会话环境。它仍是实际附着客户端，会改变 anchor 的 `session_attached` 和客户端列表，也可能触发用户配置的附着/脱离钩子；生产验收需核查这些钩子和窗格 PID/尺寸。
- **matrix/hardened:** 严格命令与参数格式仅接受捕获及有限元数据读取；未知选项、递归或客户端相对格式、控制字符、无效 UTF-8、shell job 格式均沿用原生路径。响应逐命令校验身份，并用随机完成确认拒绝迟到、重复、截断及伪造边界。每次尝试至多 350ms，失败关闭自有连接，5 秒退避仅由后续查询触发；有界原始输出至多 8 MiB。
- **matrix/hardened:** 原生回退保留退出错误、超时和会话消失语义；完整错误帧可保留健康连接。Fresh 保持独立原生查询，批次继续遵守原随机标记、500ms 截图缓存、代次隔离和原观察动作。私有 status 仅输出有界计数及自有客户端 PID，不能把存活客户端 CPU 误计为已等待子进程 CPU。
- 验证使用隔离入口和真实私有 tmux，涵盖跨 socket 同名、当前窗口、ANSI/空行、错误回退、Fresh、尺寸与 PID、连接替换、需求过期/关闭、迟到响应、并发 Close 及容量边界。生产按相同多窗口负载分别测量 collector、存活查询客户端、原生回退客户端和管理界面；本地 CPU 结果不代表整机瓦数。

## 42. Web 目录复用与菜单增量传输

`1.16.26+matrix.52` 将 monitor 配置下的 Web 会话目录与运行观察分离。静态目录由服务拥有的长寿命只读 reader 保留；每次仍读取新的共享运行观察，目录未变化时复用 registry-only 基础菜单。

- **matrix/hardened:** pinned SQLite observer 的 data_version 与连接 epoch、operator touch、配置文件签名和数据库 SameFile 身份共同失效。WAL 提交不依赖主文件 mtime；两个数据库 reader 打开前后及生成结果后均复核。最多三次一致性尝试，错误保持可见，不把旧 overlay 冒充成功。旧 standalone 与注入 factory 保留原加载路径。
- **matrix/hardened:** 目录只保存 registry 基础数据和运行身份索引，每次生成私有嵌套 DTO。缺失、失败、过旧、身份不符的运行行恢复 registry 基础状态，不能留下上一轮 running、后台任务或 fork 能力。观察开始时间及停止/修改水位保持原判断；原生绑定字段仍由 registry 决定。
- **matrix/hardened:** reader 在 Web 退出与 Handler-only Shutdown 时关闭，永久关闭门拒绝迟到 source 重新打开或发布。source 请求在 reader 锁外；资源取消与有界 SQL 查询保持关闭责任。已有鉴权诊断仅增加固定目录 loads/opens/reloads/hits/errors 等计数，不包含路径、会话或内容。
- **matrix/hardened:** 新浏览器握手 opt-in `menu_protocol=delta-v1`，首次连接全量同步；同一 profile、行数、唯一身份及顺序下，仅发送变化的完整菜单行。新增/删除/移动/排序、版本缺口、源错误恢复和增量不比全量小时发送全量。旧客户端沿用原完整协议，鉴权/origin/server-push 输入边界不变。
- **matrix/hardened:** 每个共享 publication 只构建一次版本化完整数据及最多一份前一内容版本的增量；没有每客户端历史或 diff。客户端先完整验证增量再一次性替换数组，保留未变化行对象；字段删除通过整行替换传播。非法或缺口增量触发既有重连恢复，旧 socket 事件失效；Command Center 完成事件与慢客户端背压保持原行为。Service Worker shell 缓存版本同步更新。
- 验证使用隔离 SQLite/race 和浏览器 reducer/transport 回归，覆盖外部 WAL 写入、目录与原生绑定变更、停止/归档/缺失观察、数据库打开与替换竞态、配置变化、并发关闭、私有 DTO，以及新旧协议、整行字段删除、队列跳代、错误恢复和完成事件。生产另核对目录复用指标、多窗口、WebSocket、加载构建及 AI pane 身份，再做相同负载 CPU 对照。

## 43. 区分目录变化与其他数据库提交

`1.16.26+matrix.53` 在 database version 变化时，先读取一致事务中的 instances/groups 原始目录内容，与上一份有界基础内容比较。心跳、费用或注释等其他表写入不会再无条件重新 hydrate 所有 Instance 与重建菜单。

- **matrix/hardened:** data_version 仍用于发现提交，连接 epoch、打开与最终文件身份、配置签名及最终版本检查保留。目录内容相同才复用基础菜单，原生绑定、状态、目录结构和配置真正改变仍重新构建。operator touch 每次重新读取并用于拒绝早开始的运行观察，即使目录相同也不能忽略停止/确认的水位。
- **matrix/hardened:** 私有诊断增加原始 registryReads，区分数据库读取、目录内容复用与实际重建；不承诺数据库零读取。回归包含无关 metadata 连续提交下目录复用和 touch-only 提交仍拒绝旧观察，生产另核对命中/重建分布与 CPU。

## 44. 同轮共享 Codex 费用来源目录枚举

`1.16.26+matrix.54` 为每一轮费用来源解析创建独立 resolver；同一 Codex home 只枚举一次 rollout 日期目录，再按每个原始会话 ID 精确匹配文件名。它也适用于手动历史同步，原来的费用 tail、增量 offset、导入和计价不变。

- **matrix/hardened:** resolver 不跨轮缓存；下一轮重新扫描以发现新建、删除、移动和重复来源。账号 home、SSH/local eligibility、字面 ID 验证以及最终 regular-file/symlink/唯一性检查保留，不按标题、最新文件或任意候选猜测来源。每轮目录枚举形成一份短暂快照，匹配文件在使用前仍检查当前文件状态。
- **matrix/hardened:** 验证包含每 home 一次枚举、与原 resolver 的正常及失败结果一致、后缀身份碰撞、空 wildcard、原始 ID、不同账号和轮间文件变化；生产继续区分目录复用、Web 单进程 CPU 与其他后台工作，不能仅凭命中数宣称整机功耗下降。

## 45. 费用默认手动采集，自动模式显式可选

**matrix/hardened:** `[costs] auto_collect = false` 为省略配置时的默认值。历史费用展示和预算查询始终可用；Web Costs 的“同步费用”以及 `agent-deck costs sync` 才主动读取用量。CLI `agent-deck config set costs.auto_collect true` 可启用自动模式，改动后重载管理窗口与 Web 服务。

- 自动模式统一控制 Codex 费用 owner、Claude 费用事件生产与消费、Recall 索引的费用 handoff 和价格/保留期维护。关闭时不启动费用扫描或 owner 重试，也不为费用读取 hook transcript、创建 spool 或删除历史记录；Recall 原始对话与 usage 索引仍保留。
- 手动 Claude 来源按持久会话的实际账号和 EffectiveWorkingDir 解析精确原生 ID；不借用其它账号同名记录，拒绝非本地及软链接来源。
- 手动 Web 同步要求鉴权及现有写入/origin 边界，一次只处理一个请求，只返回计数。请求取消停止后续来源读取；已经导入的事件保留，重复同步通过既有 native usage 身份去重。查看页面、汇总 GET、主题变化不会启动同步。
- 验证默认与 opt-in、hook/Recall 无隐式费用写入、取消、Web 写入边界、重复请求及前端显式按钮。生产必须重载所有旧管理进程，避免旧版等待者接管费用锁继续扫描，并保留原 AI pane PID 与原费用记录。

## 46. 复用元数据查询通道与按顶栏模板查询费用

`1.16.26+matrix.55` 把 session/window、pane 目录与 session environment 的只读查询接入已有 socket-local query transport。这四类新接入调用的原生回退沿用原调用已经建立的 context 和时限，保留原参数、解析、cache TTL、失败与所有权判断；不为各个客户端新增 query connection。

- **matrix/hardened:** 仅额外允许 `list-panes -s -t <明确会话> -F <既有安全字段>`，以读取该会话所有窗口的 pane PID。隐式 anchor、额外/重复 flags、依赖 client 的 format 和命令注入仍拒绝；不改变 Fresh 重查、不将 query failure 当确定 absence；该 PID probe 继续使用既有 OutputBounded 时限，未把它改成新的三秒总预算。framed command error 仍原生回退，错误计数要与 framing/timeout 分开判断。
- **matrix/hardened:** TUI 顶栏按当前费用模板真正引用的七种已知变量调度查询，去重并沿用 renderer 的首个右括号语义。默认只查今日金额；空模板、纯文本或未知变量不查费用汇总。保留原 Store 日期/rolling 数学、被引用变量的十秒刷新、完整费用看板及预算查询。已加载的 Home 模板依赖变化在下个既有 tick 重取，TOML 配置仍按原有加载方式生效，本轮不新增热加载。
- 验证包括 broker/native 输出与失败语义、socket/session/all-window 边界、原 context 预算、环境正负缓存、query transport 原回归、模板解析与查询次数及切换/刷新。生产另核对加载版本、query 命令计数/child CPU、管理进程 CPU、源错误和原 pane 身份；本机采样不能外推为整机瓦数或固定节能比例。

## 47. Claude 转 Codex 的原生验证与列表刷新

`1.16.26+matrix.56` 修复已启动 Codex 却因自定义 notify 收不到就绪事件而停在 pending 的交接。入口仍为选中 Claude 会话，`Shift+P`，Harness 选 Codex，Enter 后确认。

- **matrix/hardened:** Codex 保留现有 notify 路径，同时允许通过目标 pane 的 Codex 进程持有的唯一原生 thread 验证。只读精确 ID 的 rollout，核对账号 home、cwd、新鲜原生身份、目标 developer 身份与回合事件；来源缺失、不完整、身份冲突或多线程歧义时继续 pending。不按同目录或最新文件猜测，不改全局 notify 与电脑操作组件配置。
- **matrix/hardened:** 待验证目标仍暂存在归档列表，原 Claude 保持未归档；成功后在既有事务中归档原会话、激活新 Codex。两个条目保留同一标题，不能凭标题判断哪一个被归档。原生对话、项目文件与原进程保持原样。
- **matrix/hardened:** TUI 成功结果从存储读取当前源和目标行，刷新已经被 watcher 加载的待验证目标，保留后续编辑与无关会话，选中可见目标。CLI preview 的说明同步为现有可执行流程，避免误报“尚未实现”；恢复已有目标的成功回执保持 `status=success`，`target_created=false` 只表示本次复用了已登记目标。
- 验证使用第 4 节的隔离入口，覆盖精确 thread、账号/cwd/身份/时间/回合与不完整记录边界，以及已有目标先被 watcher 加载后的成功刷新、取消和失败保护。生产交接按原 operation 恢复，核对同一 Codex 原生 ID、源与目标归档状态和 pane PID，不能以创建 tmux 成功代替就绪证明。

## 48. 独立查看置顶会话

`1.16.26+matrix.57` 新增 TUI 的 `Alt+P`（macOS `Option+P`）入口，切换到 `Pinned only`，再次按键回到普通列表。`t` 的视图循环也加入此模式；`,` 继续在不固定、固定到顶部、固定到底部之间切换。

- **matrix/hardened:** 置顶视图集中显示本地会话的两种固定位置，读取完整分组成员，折叠分组或父会话不会隐藏置顶条目。各条目独立显示，退出后保持原分组的折叠状态。
- **matrix/hardened:** 归档、状态、时间和分组范围继续组合；零匹配保留空视图并显示返回入口。未固定会话、创建中的占位条目及尚无固定位置元数据的远程行不进入此列表。`matrix.60` 接入已报告固定位置的远程会话，见第 50 节。
- **matrix/hardened:** `[hotkeys].toggle_pinned_view` 可重绑或禁用，`matrix.64` 起默认 `ctrl+p`（此前为 `alt+p`）；现有 `group_view_mode` 存储恢复所选视图。帮助页和筛选栏显示入口及当前模式。
- 验证使用第 4 节的隔离入口，覆盖折叠分组、两种固定位置、独立子会话、筛选组合、空视图、取消最后一个固定位置、快捷键与持久化。安装后在新开或已重载的 TUI 使用此入口，原 AI pane 不需重启。

## 49. 置顶视图减少无用重建

`1.16.26+matrix.59` 去掉置顶视图先构建完整树再丢弃、为全部会话分配两份行数组、计算无用分组活跃状态的开销。直接读取分组成员，只为固定会话生成独立行；分组与会话顺序保持。内嵌平铺侧栏也直接构建所选呈现。

- **matrix/hardened:** 保留既有两秒 UI 刷新，用于状态筛选和异步缓存的窗口行变化；不增加采集器、后台线程或计时器。普通 TUI 的 Web snapshot 发布器未注册时直接返回，组合 TUI+Web 模式仍保留原快照刷新。
- **matrix/hardened:** `BenchmarkMatrixPinnedRebuild` 使用无 tmux handle、无 storage/watcher/Web menu 的合成本地会话，5% 固定，含嵌套分组和子会话；准备工作在计时之外。它衡量同步列表重建，不包含终端绘制、数据库、进程采集、网络或整机 CPU。
- 三次短基准的中位数：分组全展开时，80/1,000/10,000 会话每次重建分配分别由 109,928/1,312,955/16,797,620 字节降至 4,864/46,080/384,768 字节。分组全折叠也减少分配；普通和活跃置顶视图的分配量保持。并发负载令耗时有明显波动，不将耗时百分比称为稳定加速比或整机功耗变化。
- 验证使用第 4 节隔离入口：置顶、视图循环、状态/时间/归档组合及内嵌侧栏回归；性能用 `go test ./internal/ui -run '^$' -bench '^BenchmarkMatrixPinned' -benchmem -benchtime=150ms -count=3`，在改动前后各运行一次。安装后旧 TUI 仍须在会话列表 `Ctrl+T` 重载。

## 50. 远程会话固定位置与混合置顶视图

`1.16.26+matrix.60` 支持选中远程会话后用 `,` 循环 Off/Top/Bottom，或在 `Shift+P` 的 Pin position 中单独保存。`Alt+P` 的 `Pinned only` 同时集中显示本地及远程的固定会话；远程标题带主机名，折叠主机或分组不会隐藏条目。

- **matrix/hardened:** 两个 `list --json` 输出路径始终携带 `pin`，空字符串明确表示未固定；远程 DTO 的空指针表示旧版未报告，操作会提示更新远端。旧版客户端忽略新增 JSON 字段，协议本身不变。
- **matrix/hardened:** 固定状态保存到所属远端，复用 `session set`，再用完整会话 ID 的 `session show` 回读。只有 ID、字段存在和请求值都吻合才显示成功；未确认状态重新读取，不自动重发写操作。
- **matrix/hardened:** 操作在分派时绑定显示条目的主机配置；同 ID 在不同主机上的进行中操作互不影响。确认后的代次与短时推送保护拒绝旧列表覆盖；主机重指向后的回执不能改动新主机列表。
- **matrix/hardened:** 普通远程分组按固定位置优先于本机手动顺序排列，置顶视图的状态/归档/时间/完整分组路径继续组合，条目单独呈现但不改变持久父会话和折叠状态。标题按终端单元宽度处理，窄侧栏也保留合法 UTF-8。
- 保持现有远程同步频率，只在明确固定操作时增加修改与回读，不增加采集线程或计时器；置顶过滤只为保留的远程条目分配空间。
- 验证通过第 4 节隔离入口覆盖线格式、未知旧字段、完整固定循环、失败与不完整回读、主机身份、迟到列表、混合与折叠视图、筛选、排序及窄宽度。部署需更新远端二进制并重连旧的列表通道，以实际远端持久状态与原生窗口验收。


## 51. Codex 模型容量错误的有限自动重试

`1.16.26+matrix.62` 新增。当前 Codex 将 `Selected model is at capacity. Please try a different model.` 记录为原生 `task_complete.error`，错误类型 `server_overloaded`。只识别此结构和容量文字，不匹配对话里引用的句子；归入现有 `turn-interrupted` 异常并保留具体原因。

- **matrix/hardened: 开关**：`codex.capacity_retry` 默认 false；显式执行 `agent-deck config set codex.capacity_retry true` 开启，设为 false 关闭。配置热重载；只在新鲜容量候选上读取配置，不增加正常会话的文件轮询。
- **matrix/hardened: 行为**：保持当前模型，在观察到容量失败后分别等待 30、60、120 秒，发送简短的“继续”消息重新开启回合（发送器不改变模型，短消息适用于窄窗格）。每个原生线程每小时最多领取 3 次重试，计入整个连续失败链，重启不重置计数。单个原生失败 turn 只能领取一次；5 分钟以前、缺少时间或 turn ID 的错误仍可展示，但不自动继续。耗尽后显示手动继续提示。不切模型、账号，不重启进程，不处理普通 Esc 中断、额度或登录错误。
- **matrix/hardened: 保护**：子代理线程、SSH 镜像、只读 runtime 消费者及没有观察进程代际栅栏的调用者、正在运行、启动、排队、停止及归档会话均不发送。发送前读取最新原生记录并核对当前会话绑定，确认输入框可见且为空，拒绝正在工作、草稿、菜单及审批提示。领取重试后再次核对；最后发送阶段以 Instance 读锁及既有 SQLite registry 代际事务约束本进程和跨进程的删除/停止/重绑定，并重复验证原生回合、开关和输入框。抓屏和发送固定到受管理主窗格 `:0.0` 及确定的 socket，切到辅助窗口不会重定向输入；SQLite busy 重试也不会重复发送。粘贴之后、按 Enter 之前再次确认输入框只有本次“继续”、开关仍开启且失败回合未变化；混入其它输入时保留草稿并不按 Enter。不会清空用户草稿、按 Ctrl+C 或审批。
- **matrix/hardened: 去重与失败边界**：数据目录的 `capacity-retries/` 以规范化原生 rollout 路径散列命名，目录 0700、状态和锁文件 0600。使用进程内锁与跨进程 flock，先原子写入并同步领取记录，再发键盘输入。发送失败或结果不确定也消耗该次领取，绝不自动重发同一 turn。损坏/未知版本的状态拒绝重试。重试是一次新的“继续”回合，并非 Codex 内部的同一请求重放；键盘提交成功不等于模型已恢复。
- **matrix/hardened: 开销及运行范围**：复用现有状态观察节奏，不加定时器。无容量候选时只是内存判断；有候选时每 5 秒最多检查一次，只有首次安排或实际领取时落盘。详情缓存到已有子状态详情，TUI/Web 渲染不读侧文件。需要观察进程持续运行；TUI/Web 无人访问而观察进程不再采样时，延后到下次采样，不承诺无人值守常驻调度。
- **改动地图**：`internal/session/matrix_codex_capacity_retry.go`（计划、耐久去重、发送保护，新文件）、`matrix_turn_interrupt.go`（原生容量错误及刷新接入）、`instance.go`（缓存重试详情）、`userconfig.go` 和 `config_keys.go`（开关）。
- **验证**：`matrix_codex_capacity_retry_test.go` 覆盖真实记录结构、继续/成功清除、子代理、30/60/120 退避、每小时上限、跨进程与并发领取、发送不确定、旧回合重放、草稿/审批/工作状态、只读消费者、损坏状态及配置往返。使用隔离 runner，含 race 检查；不会为了测试向真实会话发消息。另用真实 tmux 与无网络的 Python 模拟终端验证单次消息送达及草稿保留。没有人为制造真实模型容量失败或承诺重试一定成功。

## 52. 用 o 打开会话

`1.16.26+matrix.65` 将 `o` 作为列表打开会话的主快捷键，原发送提示入口移到 `Alt+O`。

- **matrix/hardened:** `[hotkeys].open_session` 默认 `o`，可重绑或置空；Enter 保留兼容入口。使用同一打开流程，覆盖本地、远程、窗口行及内嵌终端，停止的本地会话沿用自动恢复。分组行沿用展开与折叠。
- **matrix/hardened:** 底栏、帮助及本地/远程预览显示当前打开键；禁用可配置键后提示 Enter。对话框中的文本输入保持原有输入方式。
- 验证使用第 4 节隔离入口，覆盖实际主界面按键路由、重绑与禁用、Enter 兼容、提示入口、帮助及界面快照。已打开的 TUI 在会话列表按 `Ctrl+T` 重载后生效。

## 53. Ctrl+P 置顶视图与移除 Ctrl+N/P 导航

`1.16.26+matrix.64` 将置顶视图默认入口改为 `Ctrl+P`；再次按键回到普通列表，`t` 的视图循环保持。帮助、筛选栏和空视图提示读取实际绑定。

- **matrix/hardened:** `[hotkeys].toggle_pinned_view` 默认 `ctrl+p`，仍可重绑或置空；`matrix.67` 起同时提供 `Alt+P`（macOS `Option+P`）默认别名。主列表不再将 `Ctrl+P`、`Ctrl+N` 作为上下移动或数字计数导航；重绑或禁用置顶入口会释放两个默认入口，也不会恢复旧导航别名。
- **matrix/hardened:** 技能、Agents、Watcher、Dead letters、分支、目录、分组、新建和路径弹窗消费这两个旧导航键，防止子输入组件继续移动选择或焦点。原方向键、`j/k`、Tab 流程保持；路径编辑建议用上下方向键。Notes 和未启用的 Feedback 文本编辑器也移除组件默认的 `Ctrl+N/P` 行移动。
- **matrix/hardened:** 已连接的本地/远程终端以及 insert mode 继续接收原始控制按键。按键映射沿用 `config.toml` 的 `[hotkeys]`；`?` 展示当前映射。
- 验证使用第 4 节隔离入口，覆盖置顶切换与选择保持、重绑和禁用、旧入口不再触发、主列表无移动、弹窗输入及选择不变、方向键和文本编辑保持可用；安装后以原生窗口和原会话 pane 身份验收。

## 54. 用 $ 查看异常

`1.16.26+matrix.66` 的列表快捷键 `! @ # $` 依次查看运行、等待、空闲、异常。`$` / Shift+4 固定切换 Abnormal，再按一次或按 `0` 返回全部；保留 `&` / Shift+7 兼容入口。异常分类沿用第 15 节，包括 error、中断和登录、模型、额度诊断。

- **matrix/hardened:** 费用面板移到 `Alt+$`（Alt+Shift+4）；用 `q`、Esc 或 `Alt+$` 返回。帮助页与费用面板底栏同步显示新绑定，普通 `$` 不再打开费用页。中文输入法的 `＄` / `￥` 同样筛选异常。
- 改动地图：`internal/ui/home.go`（绑定与关闭）、`cost_dashboard.go`（返回提示）；帮助和顶栏使用共享常量，自动跟随新绑定；相关筛选栏快照同步更新。
- 验证：`matrix_status_filter_keyboard_test.go` 经 Home.Update 覆盖原始字符、两种终端协议、全角符号、导航前缀及再次取消；`hotkeys_test.go` 验证费用入口独立、缺少费用存储的提示，以及实际费用面板开关。

## 55. 置顶视图的双入口

`1.16.26+matrix.67` 保留 `Ctrl+P` 主入口，同时恢复 `Alt+P` / macOS `Option+P` 的默认备用入口。两者切换同一置顶视图，Mac 普通 Option 字符 `π` 也沿用主列表的字符转换。

- **matrix/hardened:** 快捷键先解析用户显式绑定，再为未重绑的动作加入未被占用的默认别名，最后解析布局字符别名。其它动作显式绑定 `alt+p` 时优先；重绑或禁用 `toggle_pinned_view` 会释放两个默认入口。帮助页缓存按键归属，仅显示实际可用的别名；紧凑筛选栏继续显示主入口。
- 不改变已关闭的 `Ctrl+N/P` 导航、弹窗输入或已连接终端的按键透传，不增加后台任务或渲染时映射重建。验证覆盖真实按键派发、选中条目保持、Mac Option 字符、重绑、禁用及显式冲突。


## 56. 数字跳转后直接激活条目

`1.16.26+matrix.68` 支持输入 `12` 或 `:12` 后按 Enter / `o`，跳到当前可见列表的第 12 行并打开会话或展开、折叠分组。

- **matrix/hardened:** 数字配合 `j/k` 或方向键仍是相对移动计数；配合打开键才是绝对行号。行号跳过视图分隔线，并沿用已有跳回标记。普通 `0` 仍清除筛选，`:0` 和负数不是有效行号。
- **matrix/hardened:** 使用 `[hotkeys].open_session` 的实际绑定；重绑或禁用后不会让原 `o` 继续激活。`:set nu` / `:set nonu` 仍作为文字命令输入。无效、越界、溢出及被截断的命令不会激活当前停留的条目。
- **matrix/hardened:** 新入口和普通打开键共用本地、远程、窗口行、内嵌终端和停止会话恢复流程。帮助页及待输入提示同步说明数字激活方式。
- 验证使用第 4 节隔离入口，覆盖数字确认、分组折叠与展开、分隔线、真实激活目标、重绑与禁用、长行号、无效输入及现有计数、行号显示、跳回和文本输入回归。已打开的 TUI 仍在列表按 `Ctrl+T` 重载。

## 57. F9 返回会话列表、数字配合 l 只定位

`1.16.26+matrix.69` 支持按未修饰的 `F9` 从 Agent Deck 内的本地全屏、SSH/mosh 和内嵌会话返回列表。返回只断开当前查看入口，会话本身继续运行。

- **matrix/hardened:** 本地原有可配置的 Ctrl 返回键、远程和内嵌的 Ctrl+Q 继续生效，全屏会话的 Alt+Q 也保留。帮助、会话状态栏和内嵌输入提示显示 F9。Mac 顶排发送媒体键时，仍需用 Fn+F9 或由用户选择标准功能键设置；程序不修改系统键盘配置。
- **matrix/hardened:** F9 支持传统及增强终端键盘编码，并处理分片输入；粘贴内容、带修饰的 F9 和其他终端序列继续传给会话。内嵌模式沿用原有输入代际取消、尾部丢弃和列表焦点恢复流程。
- **matrix/hardened:** Shift+Enter 新开的独立终端直接执行 tmux 或远端命令，未经过当前 TUI 的输入路由；其中返回键以该终端、远端的实际支持为准。
- **matrix/hardened:** `12l` 或 `:12l` 只把光标移到当前可见列表第 12 行，不打开会话或切换分组。`12` / `:12` 后按 Enter 或配置的打开键仍定位并激活，`3j` / `3k` 仍按相对移动计数，单独 `l` 仍执行原有右移、展开操作。无效、越界或溢出的行号不会操作当前条目。
- 验证使用第 4 节隔离入口，覆盖实际输入转发、分片与粘贴、按键兼容、数字只定位，以及真实 PTY 的打开与返回。已打开的 TUI 在列表按 `Ctrl+T` 重载新版本。

## 58. 用 p 切换置顶视图

`1.16.26+matrix.70` 将列表里的 `p` 作为置顶视图主入口，再按一次返回普通列表；保留 `Ctrl+P`、`Alt+P` / macOS `Option+P` 兼容入口，`Ctrl+N/P` 上下导航继续关闭。

- **matrix/hardened:** `[hotkeys].toggle_pinned_view` 默认 `p`；帮助展示实际可用的三个入口，筛选栏和空列表显示主入口。重绑或禁用会释放默认入口，显式配置的备用键仍优先，布局字符继续沿用统一映射。
- **matrix/hardened:** 原 `p` 的多仓库路径编辑移至 `Ctrl+L`，仍由 `[hotkeys].edit_paths` 重绑或禁用；所有路径提示同步读取映射。普通文本输入、模态窗口及已连接终端继续使用各自的输入路由。
- 验证覆盖主界面实际派发、三入口及 Mac/俄语字符、选中会话保持、重绑和禁用、冲突归属、多仓库编辑仍可达、普通导航与界面提示。安装后旧 TUI 在列表按 `Ctrl+T` 重载。

## 59. 数字配合 h 收起指定分组

`1.16.26+matrix.71` 支持 `12h` 和 `:12h`：定位到当前可见列表第 12 行，再执行原有 `h` / 左方向键的收起逻辑。分组行收起该分组；会话行先收起已展开的多窗口，否则收起父分组。本地和远程列表使用同一入口。

- **matrix/hardened:** 无效、越界或溢出的行号不操作当前选中条目；单独 `0` 仍清除筛选，`:0h` 则按无效行号处理。置顶视图中的独立会话不改变普通列表的隐藏分组折叠状态，文本输入与已连接终端仍沿用各自输入路由。
- **matrix/hardened:** `12l` / `:12l` 仍只定位，数字配合打开键仍定位并激活，`3j` / `3k` 仍是相对移动计数；帮助页和待输入提示同步展示新入口。
- 验证覆盖数字和冒号入口、本地与远程分组、会话到父组、无效地址、置顶视图及输入模式隔离。安装后旧 TUI 在列表按 `Ctrl+T` 重载。

## 60. 排队会话保持生命周期状态

`1.16.26+matrix.72` 修复分组达到并发上限时，`session start` 已返回 Queued，后续状态刷新却将尚未创建窗格的会话改成 starting 或 error/unknown-exit 的问题。

- **matrix/hardened:** 在启动宽限与退出分类之前保留 queued；当前进程及从存储恢复的会话使用同一规则。准确查询确认 tmux 会话存在后，仍执行正常状态判断，避免过时的 queued 掩盖实际运行或退出；查询失败时保留排队状态。
- **matrix/hardened:** 查询在 Instance 锁外执行，回到锁内核对会话、停止代次与排队状态。排队后的 Start 继续进入正常启动流程，启动后失去窗格仍显示实际退出分类。并发上限和队列释放规则保持原样。
- 验证使用隔离入口，覆盖新旧会话、存储恢复、空与缺失 tmux 句柄、排队后启动、已有窗格及启动后外部终止；同时运行状态分类与停止保护的 race 回归、core 启动排队与释放队列测试。安装后需重载 monitor；已打开的 TUI 在列表按 `Ctrl+T` 重载。

## 61. macOS 导航与输入法适配

`1.16.26+matrix.73` 增加显式开启的 `[ui].auto_input_method`。

- **matrix/hardened:** 以 agent-deck 的列表、表单字段和会话焦点切换原生输入源：导航临时使用 ASCII，文字输入和 attach 恢复此前输入法；不推断 AI 会话内部模式。
- **matrix/hardened:** 原生切换核对所属终端的前台应用及终端标签页焦点；SSH、后台 Web 和不支持的平台不取得桌面控制权。手动选择的输入源优先，失败保留恢复机会，正常/错误/信号退出共用清理入口。
- 改动集中在 `internal/inputmethod/`、`internal/ui/matrix_input_method.go`；Home 更新后同步模式，CLI 仅在交互启动时配置控制器；配置入口支持 `config get/set/schema`。
- 验证：隔离的模式/焦点和输入源归属测试；真实候选窗口体验需按 [输入法适配说明](docs/input-method.md) 的前台交互步骤验收。

## 62. 真实终端中的输入法归属与启动焦点

`1.16.26+matrix.74` 修复列表仍触发鼠须管的两个原因。

- **matrix/hardened:** 使用 `sysctl(KERN_PROC_PID)` 读取父进程身份，保留终端应用路径和前台应用核对；不再因普通终端链中的 root `login` 拒绝 `PROC_PIDTBSDINFO` 就放弃识别。
- **matrix/hardened:** 终端可能只报告焦点变化而不报告初始状态；交互启动先按已聚焦初始化，由原生前台应用检查约束实际切换，随后失焦报告仍撤销输入法控制。
- 验证：以 `ps` 对照完整真实父进程链，覆盖其他用户所有的祖先进程；配置启动无首次 FocusMsg 的回归测试，以及既有模式/失焦/恢复 race 测试。实际列表需核对重载后为 ABC、搜索恢复鼠须管、Esc 回列表再切 ABC。

## 63. Path 直接编辑与逐级目录补全

`1.16.26+matrix.75` 将新建会话的 Path 焦点改为直接编辑，保留预填路径，左右移动光标和 Backspace 不再进入候选浏览或整段删除。

- **matrix/hardened:** Tab 仅补全当前层目录并留在 Path，历史项目不会被普通 Tab 隐式接受；唯一目录补全加 `/`，下一次 Tab 才进入下一层。Enter 在可用路径上进入下一项，Shift+Tab 返回上一项。
- 远程 Path 的 Tab 异步请求目标设备 `__complete-path` 只读 JSON 目录列表，使用现有 SSHRunner 的参数引用、连接和超时；不读取控制端文件系统。目标端也需安装此构建；旧版本或断连显示补全不可用，仍保留输入。
- 远程结果按对话框实例、打开代次、输入修订、设备与当前焦点核对，防止慢响应覆盖后续输入。
- 验证覆盖项目历史不抢占目录前缀、逐级补全、无匹配仍留在编辑器、直接编辑与重新聚焦、远程成功/失败/过期响应和既有创建流程。
- **matrix/hardened:** 安装脚本支持交叉构建到隔离暂存目录，跳过在构建机执行异平台程序，版本及目录接口由目标机器读回验证。

## 64. 本地搜索支持分组与模糊名称

`1.16.26+matrix.76` 修复 `/` 搜不到分组名、`agent deck` 无法匹配 `agent-deck` 的问题。`Alt+/` 和 Recall 不可用时的本地搜索回退共用同一匹配逻辑。

- **matrix/hardened:** 标题、分组路径、项目路径、工具各自独立匹配；空分组按默认 `My Sessions` 处理。Unicode 空白与标点归一化，支持空格、连字符、下划线的等价写法，以及 `adk` 这样的顺序缩写。
- **matrix/hardened:** 字面连续匹配优先，其次为归一化连续匹配，再按模糊分数排序；同质量结果保留输入顺序，不修改原列表。模糊路径只匹配项目末级名称及各分组段，避免在长目录链中凑出缩写。
- 保留 `waiting/running/idle/error/stopped` 的精确状态过滤及打开时清空设置。纯标点查询仍按字面处理；全空白查询返回原列表。
- 输入提示和帮助说明可搜字段；30 列窗口将外框纳入宽度限制。相关本地搜索快照只更新提示文字。
- 验证使用第 4 节隔离入口：`TestMatrixSessionSearch*` 覆盖分组、默认组、Unicode、缩写、符号、排序与误匹配；`TestMatrixLocalSearchMatching*` 经真实 `/` 按键路由验证输入、刷新、上下选择、Enter 定位与 30/60/80/120 列布局。安装后列表按 `Ctrl+T` 重载，AI 会话无需重启。

## 65. 已连接远程通道时的 Path 补全

`1.16.26+matrix.77` 修复新建远程会话按 Tab 报 `Directory completion unavailable`。

- **matrix/hardened:** 隐藏的 `__complete-path` 不在 remote-agent 命令注册表中。TUI 的持久通道会拒绝它，而未启用通道的独立 CLI 请求可成功。此只读目录请求明确绕过持久通道，经现有 SSH exec 执行，兼容已运行的远程 agent。
- 错误提示保留经过控制字符清理、长度限制的实际原因，完整错误进入 UI 日志；超时单独标明。
- 验证覆盖已连接模拟通道时列表仍走通道、补全独立执行；Pro 与 srv 的真实已连接通道复现原错误并验证修复，相关 session/UI race 测试通过。

## 66. Codex 6.1 Sol 模型候选

`1.16.26+matrix.78` 将 `gpt-6.1-sol` 加入 Codex 内置模型目录，避免 CLI 模型探测不可用时，新建界面的回退列表遗漏它。探测列表仍优先，内置目录继续参与合并去重；模型字段允许直接输入完整 ID。

## 67. 精简 SSH PATH 下的 Codex 模型探测

`1.16.26+matrix.79` 修复远端模型探测在 SSH PATH 不含用户 bin 目录时静默回退的问题。

- **matrix/hardened:** 默认裸命令 `codex` 仅在 PATH 返回 `exec.ErrNotFound` 时，尝试可执行的 `~/.local/bin/codex`。PATH 中的版本、显式路径和自定义 wrapper 保持优先；`exec.ErrDot` 等拒绝不被掩盖，不修改进程 PATH 或用户启动配置。
- 回退命中后继续使用原有真实可执行文件缓存键，精简 PATH 的请求能够复用此前成功的模型探测缓存。二进制仍缺失时记录具体原因。
- 验证覆盖精简 PATH 下实际探测、跨 PATH 复用缓存、PATH/自定义命令优先、缺失/不可执行/目录/断链回退拒绝以及 ErrDot；并在目标端保留原 SSH PATH、使用隔离空缓存验证真实自动探测。

## 68. World Store 看板七列与验收

`1.16.26+matrix.80` 把 World Store 任务看板（第 8 节）改成三方统一的七列，并加上验收动作。设计见 personal-panel 仓库的 `docs/specs/2026-10-16-three-way-board.md`（P2）。

- **matrix/hardened:** 列：待规划、待办、进行中、待验收、卡住、完成、待核。原来的「暂停」栏并入进行中，卡片前标 `‖`。卡住由快照里未解除的卡点（`卡点｜卡住对象`）推出，不改任务状态；快照没给出卡点时标题提示“blockers not read (卡住 unknown)”，不把任务当成没卡住。打开看板时光标优先落在进行中，其次待验收。
- **`v` 交付验收**：有未结束的执行时先 `finish` 结束本轮，再用 `task.update` 把状态改为 `待验收`（带原状态基线）。两次写入不是事务，第二步失败时本轮已结束，重新读取即可看到。
- **`x` 验收通过**：待验收卡片上的 `x` 即完成任务，确认文字为 “Accept …”。
- **`b` 退回**：只对待验收且没有未结束执行的任务。确认行可以输入原因（此时 `q` 是文字，不是取消；Esc 取消），确认后状态改回 `待办`；原因非空且任务在本机有关联会话时，用 `agent-deck session send --queue --message-file -` 排队发进这些会话（正文走 stdin，不进 argv）。状态已改但原因没排上队时报错并说明，不重试。
- **CLI**：`tasks review|accept|reject <task>`；`accept` 等同 `done`；CLI 的 `reject` 只改状态，不发原因。
- **World Store 侧**：`task.update` 可直接写 `任务｜状态`，但只接受 `待规划`、`待办`、`待验收`、`已取消`；`worldstore.Client.SetStatus` 在本地先拒绝其他值、已关闭任务和有未结束执行的任务。
- 验证：`internal/worldstore` 与 `internal/ui -run Kanban` 全部通过；`internal/ui`、`cmd/agent-deck` 全量里的失败均与基线相同或单独复跑通过（满负载偶发）；真实空间只读 `tasks list --all` 的七列分布与 World Store TUI 看板一致。真实空间未做写入。

## 69. 独立的分组搜索定位

`1.16.26+matrix.81` 新增 `Alt+S`（Mac `Option+S`，兼容普通 Option 字符 `ß`），只搜索分组名称与路径。`[hotkeys].group_search` 可重绑或禁用，帮助页同步显示。

- **matrix/hardened:** 本地读取完整分组树的真实名称与路径，包含空组、折叠子组及显示名称别名；远程读取已有缓存，补齐祖先和宿主根，不增加网络请求。不同宿主或路径的重名组独立展示。
- **matrix/hardened:** 支持大小写、Unicode 空白/标点归一化与顺序缩写，连续匹配优先。组名 `running/error` 按名称匹配；它不接受会话标题、项目路径或工具字段。
- Enter 再核对当前分组身份，展开父组并定位，只进行导航；清除状态/时间过滤并恢复普通分组视图，保证空组可见。保留显式 `-g` 范围，远程列表追加也按同一范围过滤。Esc 和无结果 Enter 不改变列表视图，已移除的分组不会跳到邻近条目。
- 模态输入接入文字输入法、光标闪烁、鼠标滚轮与尺寸更新；组名输入不触发列表快捷键。每次打开清空查询，重新打开可纳入新加载的分组。
- 验证使用第 4 节隔离入口：组件覆盖匹配、排序、重名、刷新、长列表和 30/60/80/120 列；Home 覆盖真实默认/Mac/重绑/禁用、文字焦点、本地/远程定位、空组、折叠父组、视图恢复、取消、范围与陈旧选择。旧 TUI 在列表按 `Ctrl+T` 加载新构建。

## 70. 看板显示 Multica 派发进度

`1.16.26+matrix.82` 读取 World Store 的 `任务｜Multica 关联`（personal-panel `docs/multica-board-sync.md`，三方看板 P3）。

- **matrix/hardened:** 只读，agent-deck 不写这个字段。卡片末尾显示 `·PP-12 进行中`，拆分派发显示 `·PP-12 2/3 完成 · 1 待验收`，详情行加上同样的摘要。issue 处于 in_progress 而任务仍是待规划/待办时，卡片显示在进行中列，任务状态不变。已派给 Multica 的任务进入默认视图。格式不认识的关联值一律忽略。
- 在 agent-deck 里验收（`x`）或退回（`b`）只改 World Store；World Store 的同步脚本（launchd，每 2 分钟）随后把结果带到 Multica。
- 验证：`internal/worldstore`、`internal/ui -run Kanban` 通过；真实环境用测试任务走通派发 → Multica 待验收 → `tasks accept` → Multica 完成。

## 71. launch 的 `--env-file`

`1.16.26+matrix.83` 给 `agent-deck launch` 加 `--env-file /绝对路径`，供 Multica 的 agent-deck 执行器（personal-panel 三方看板 P4）把任务令牌交给会话，而不进 argv 或持久化的命令。

- **matrix/hardened:** 文件在所有已配置的环境来源（全局 env_files、init 脚本、工具 env_file、group/conductor/tool inline env、restart env）之后以 `{ [ ! -f ] || source; }` 读入，所以它的值优先；文件已被所有者删除时这一段仍返回成功，不会打断用 `&&` 串起来的启动命令。只接受绝对路径、普通文件、权限不含 group/other（`chmod 600`），路径里不能有引号、`$`、反引号、反斜杠或换行。
- `matrix.84` 修正：`launch` 先保存会话，Claude 的启动命令由 `detachedClaudeLaunchInstance` 的副本构造，`matrix.83` 里这个字段既没复制也没持久化，实际不生效（Multica 的 agent-deck 执行器实测时会话拿不到任务令牌）。现在副本带上 `EnvFile`，路径（不是内容）存进 tool_data 扩展区 `env_file`。文件被所有者删除后，restart 读不到也照常启动。
- 验证：`TestBuildEnvSourceCommand_EnvFileSourcedLast`、`TestEnvFilePersistsAndMissingFileStillStarts`（tool_data 往返、文件缺失时 `&&` 链继续、Claude 启动副本保留字段）、`TestValidateLaunchEnvFile`；真实 Claude 会话的启动命令中确认读入 env 文件。`internal/session` 中存储/启动相关测试的失败与改动前基线一致或单独复跑通过。

## 72. 看板打开 Multica issue 与验收后立即同步

`1.16.26+matrix.85`，三方看板 P5（personal-panel `docs/multica-board-sync.md`）。

- **matrix/hardened:** `o` 在浏览器打开选中任务的 Multica issue（地址取自 World Store 关联镜像里的 `root.url`，只接受 http/https；macOS 用 `open`，其他用 `xdg-open`）。未派发或没有记录地址时只提示。
- 对已派发任务做验收（`x`）、交付验收（`v`）、退回（`b`），以及命令行 `tasks done|accept|review|reject` 成功后，后台运行一次 bridge 旁边的 `scripts/multica.cjs sync`（`worldstore.Client.TriggerMulticaSync`），结果马上到 Multica，不必等 World Store 的监听或 2 分钟兜底轮询。脚本自带锁并自己记日志；找不到脚本时什么都不做。
- 验证：`TestTriggerMulticaSyncNeedsTheScript`、`TestKanbanOpensMulticaIssue` 及原有看板测试通过。


## 73. 最近交互的会话列表

`1.16.26+matrix.86` 新增 `Ctrl+E` 的「最近 / Recent」视图，再按一次返回普通分组；`t` 的视图循环也可进入。`[hotkeys].toggle_recent_view` 可重绑或置空。

- **matrix/hardened:** 从完整分组成员构建独立会话行，按持久化的 `LastAccessedAt` 跨分组、跨主机倒序排列；固定位置、状态和手动顺序不改变时间顺序。同一时间按主机与会话 ID 稳定排序，展开的窗口跟随所属会话，普通分组与父会话的折叠状态保留。
- TUI 打开与返回、已确认的列表内手动提示、CLI 交互 attach，以及网页终端成功接收的非空输入更新交互时间；网页输入每连接最多每五秒写一次。后台输出、状态刷新、自动发送和网页后台重连不更新。没有交互记录的会话不以创建时间冒充交互。
- **matrix/hardened:** `matrix.90` 起，每次主动切入「最近」或在其中切换状态/时间筛选，都从最新匹配的第 1 行开始并回到列表顶部；后台刷新和打开会话后的返回继续按会话身份保持选择。
- 状态、归档、分组范围与时间窗口继续组合；「最近」中的时间窗口和行内时间都使用交互时间。没有匹配时保持空视图，显示返回入口。所选模式沿用 `ui_state.group_view_mode`，重开后恢复。
- **matrix/hardened:** `list --json` 增加 `last_accessed_at`，供远程列表合并排序；旧远端未报告时不猜测时间，需要更新目标端后才能纳入「最近」。保持原同步频率，不增加后台扫描或定时器。
- 验证使用第 4 节隔离入口，覆盖混合主机倒序、折叠与子会话、固定和状态干扰、筛选组合、稳定同时间排序、窗口跟随、真实快捷键和持久化、交互写入与背景不写入、宽度与空列表。安装后旧 TUI 在列表按 `Ctrl+T` 重载；AI pane 无需重启。

## 74. Codex 共享后台的延后更新

`1.16.26+matrix.87` 在 `S` 设置末尾新增 **AI UPDATES**，检查、安装与重启分别操作。CLI 为 `agent-deck ai-update status|check|install|restart`，重启要求 `--yes`。用法和初版支持范围见 [AI 工具更新](docs/ai-updates.md)。

- **matrix/hardened:** 安装完整官方包时保留旧 release，不发信号、不停止或重启后台；当前任务继续用旧版，下次后台真正启动时使用新选择。再次打开客户端可能复用旧后台。
- **matrix/hardened:** 显式安装同时关闭原生自动更新并固定选择；持原生生命周期与安装器兼容锁，覆盖文件锁及目录 fallback，移除 eligibility marker，防止已排队的原生更新器随后自行重启。没有默认检查、轮询或新增系统定时器。
- 仅对已核对的 `0.163.0` 运行后台、完整 CLI 与专用 daemon 包支持延后安装；未知后台或更新器拒绝盲选。`Restart` 是另一个可能中断所有共享任务的原生操作，界面必须另按 `y` 确认。
- 验证使用第 4 节隔离入口，覆盖完整包与校验失败、安全解包、设置保留、原生锁互斥、安装不重启、实际版本读回、UI 异步状态与设置页布局；旧 TUI 在列表按 `Ctrl+T` 加载新面板。

## 75. 分组搜索紧凑列表

`1.16.26+matrix.88` 将 `Alt+S` 分组搜索改为单行的分组、路径、设备三列，按窗口高度显示结果，解除原来十项的上限。用法见 [分组搜索与定位](docs/group-search.md)。

- **matrix/hardened:** 普通分组只列父路径，顶级组显示 `—`；显示名称别名保留真实相对路径，远程设备单独列出，避免重复命名空间。窄窗口收起路径列，底部固定显示所选设备和完整相对路径，过长时从中间省略。
- 验证使用第 4 节隔离入口，覆盖宽屏至少二十项、中文与 emoji 对齐、同名组身份、窄屏尾栏和所有结果逐项可达；搜索、快捷键和 Home 定位回归通过。旧 TUI 在列表按 `Ctrl+T` 加载新布局。

## 76. Recall 新内容索引与手动刷新

`1.16.26+matrix.89` 在 Recall 中新增 `Ctrl+R` / `F5`，保留关键词、已管理范围和当前会话，重新扫描索引、查询并加载预览。用法及积压边界见 [Recall](docs/recall.md#the-tui)。

- **matrix/hardened:** 交互扫描在目录发现完成后才开始 150 ms / 32 MB 解析预算，避免历史文件多时每次预算提前耗尽、游标永远不前进。CLI/backfill 保留原有预算语义。
- **matrix/hardened:** 请求可取消，重复刷新隔离旧结果、状态、预览和后台计时器；已提交的新内容无需等全部积压完成即可搜索。预览与状态各自拒收较早快照，索引锁短暂竞争最多重试三次。状态读取改走 reader，避免等待单一 writer。
- 验证使用第 4 节隔离入口，覆盖目录发现超过预算仍推进、追加真实 transcript 后搜索与预览更新、并发回调顺序、取消、范围和选择保留、锁竞争、宽度与界面快照。大积压仍需分批处理；后台续扫保留 busy/load gate。旧 TUI 在列表按 `Ctrl+T` 重载。

## 77. Ctrl+H/L 左右切换状态视图

`1.16.26+matrix.91` 在主列表新增 `Ctrl+H` 上一个、`Ctrl+L` 下一个状态视图，按 `All → Running → Waiting → Idle → Abnormal → Open → Archived → All` 双向循环，保留 `Shift+数字`、`0` 与 `~` 的直接入口。

- **matrix/hardened:** 循环顺序固定，空结果不回退；保留分组/置顶/最近展示模式、时间窗口和显式分组范围。普通列表沿用身份选择保持，Recent 仍从最新匹配行开始。`~` 是异常的中断细分，左右按 Abnormal 的位置导航；Open 的隐藏停止子步继续由 `%` 控制。
- **matrix/hardened:** `[hotkeys].previous_view` / `next_view` 支持重绑和置空；默认路径编辑从旧 `Ctrl+L` 移到 `Alt+L`（更新第 58 节的历史默认）。已有显式 `edit_paths = "ctrl+l"` 等自定义绑定优先，冲突的新增默认入口不启用，帮助也不显示失效动作。文字输入、弹窗、insert mode 和已连接终端沿用各自按键路由。
- 验证使用第 4 节隔离入口：双向回绕、空结果、选择/范围、Recent、直接入口、重绑/禁用/冲突与文本编辑测试，以及相关快捷键、帮助、筛选与键盘协议的 race 回归。安装后旧 TUI 在列表按 `Ctrl+T` 加载新构建，AI pane 无需重启。

## 78. 会话搜索范围与跨视图定位

`1.16.26+matrix.92` 将 `/` 全部本机会话、`Alt+?` 当前视图和 `Alt+/` 当前分组搜索分别标明范围。详情见 [会话搜索范围](docs/session-search.md)。

- **matrix/hardened:** 全部及分组搜索在选择后清除遮挡目标的状态、时间、置顶、最近及分组范围，展开分组并定位；归档目标只切换展示分区。Recall 已管理结果使用同一定位规则。打开和取消搜索不改变视图。
- **matrix/hardened:** 当前视图搜索对完整注册集合组合筛选，包含折叠组与子会话，回车保留视图；后台刷新保持范围和关键词。各入口重新装载候选，避免退出分组搜索后 `/` 沿用子集。定位前重新核对注册表身份与当前范围，失效目标显示错误。
- 新 `[hotkeys].view_search = "alt+?"` 支持重绑或禁用；兼容 Mac 普通 Option 字符 `¿`、`÷`。验证使用第 4 节隔离入口，覆盖范围组合、归档互跳、后台刷新、删除/移动/过期目标、取消、按键和输入法，以及 30/60/80/120 列布局及相关界面快照。旧 TUI 在列表按 `Ctrl+T` 加载新构建。

## Claude 外部钩子缺失的处理

**matrix/hardened:** 远程会话的 `SessionStart` 若报告外部脚本不存在，检查目标设备的有效 Claude 配置；该设备已有的第三方钩子不会因重新安装 Agent Deck 而消失。按 [外部钩子故障处理](docs/claude-hook-errors.md) 先完整备份，再精确清理失效引用，保留其他钩子，并区分配置读回与实际启动验证。此类设置修复不需要修改程序或升级构建版本。
