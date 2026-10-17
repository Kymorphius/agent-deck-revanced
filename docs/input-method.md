# macOS 导航与输入模式

<!-- matrix/hardened: opt-in native input-source ownership for the TUI. -->

`1.16.26+matrix.73` 增加本地 macOS 输入法适配：列表和快捷键操作临时使用英文键盘，进入文字输入后恢复之前使用的输入法。

```sh
agent-deck config set ui.auto_input_method true
```

已打开的列表按 `Ctrl+T` 重载程序，或退出后重新运行。关闭功能：

```sh
agent-deck config set ui.auto_input_method false
```

也需要重载列表。开关默认关闭，保存在 `[ui]` 下。

## 行为

- 列表导航、会话切换列表、移动分组、选择按钮和开关时，临时切到系统的 ASCII 键盘。`j/k` 等快捷键直接送给 agent-deck，不经过拼音组词。
- 搜索、重命名、消息输入、备注和表单的文字字段恢复先前的输入法。表单里的按钮和文字字段按实际焦点区分。
- 进入本地或远程 AI 会话前恢复输入法；回到列表后再次启用英文键盘。适配只识别 agent-deck 的焦点，不推断 AI 会话内部 Vim、shell 或其它程序的模式。
- 原来使用英文键盘时，进入输入框仍然使用英文。在文字输入框内选择鼠须管后，下次返回列表、再进入输入框会恢复鼠须管。
- 恢复前核对当前输入源；用户另选的输入源优先。退出时只恢复本程序实际替换过的输入源，后台标签页的退出不会覆盖前台标签页的选择。

这通过 macOS 原生输入源接口实现，无需模拟 Shift、安装键盘监听器或改动鼠须管词库。它恢复鼠须管输入源，保留鼠须管自身的中英文状态、方案和学习数据。

## 支持范围与验证

需要启用 CGO 的 macOS 构建、可以确认父进程所属的原生终端应用，以及终端焦点报告。Teft/iTerm2 这一类原生终端可识别所属应用。启动/重载时按所属终端应用的前台状态初始化，随后按终端焦点报告跟踪；收到失焦报告后，后台刷新和退出不切换输入源。通过 SSH 启动的 TUI、无原生终端祖先进程的 TUI、无界面 Web 服务、Linux 以及禁用 CGO 的构建不会控制 Mac 桌面的输入源。

常规测试使用替身，不改变真实输入源：

```sh
scripts/run-isolated.py go test -race ./internal/inputmethod ./internal/ui -run '^TestInputMethod' -count=1
```

`TestInputMethodNativeAcceptance` 默认跳过。需要单独从本机前台终端显式运行；它短暂选择指定的、已经启用的输入法，检查输入法 → ASCII → 输入法，并恢复开始时的输入源：

```sh
AGENTDECK_IME_ACCEPTANCE=1 \
AGENTDECK_IME_ACCEPTANCE_SOURCE=im.rime.inputmethod.Squirrel.Hans \
scripts/run-isolated.py go test ./internal/inputmethod -run '^TestInputMethodNativeAcceptance$' -v
```

最终交互验收：鼠须管中文状态进入列表，检查 `j/k` 不弹拼音候选；按 `/` 搜索中文；Esc 返回列表，再打开重命名和消息输入；进入 AI 会话后正常输入中文；切换窗口和退出列表时检查输入法状态。

`matrix.74` 修复普通终端经过 root 所有的 `login` 进程时归属识别失败，以及 Teft 重载不发送首次焦点事件时一直等待的问题。升级前仍在运行的旧版可能在保存配置时丢弃新增字段，升级后应读回 `agent-deck config get ui.auto_input_method` 确认开启。
