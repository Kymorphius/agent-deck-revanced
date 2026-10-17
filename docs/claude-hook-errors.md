# Claude 启动时报告外部钩子脚本不存在

如果 Claude 显示 `SessionStart:startup hook error`，并且后面的 shell 错误是某个外部脚本 `No such file or directory`，先检查执行 Claude 的那台设备上的有效配置。

远程创建会话时，Agent Deck 会在目标设备执行创建和启动命令。该错误可能来自目标设备原有的用户、命名账户或项目 Claude 配置。Agent Deck 自己安装的钩子调用 `agent-deck hook-handler`，不会创建菜单栏工具等其他应用的外部钩子。

## 检查与修复

1. 在目标设备确认报错里的脚本是否存在；若仍需该集成，先查明应用是否换了安装位置。
2. 检查有效 Claude 配置目录中的 `settings.json`、`settings.local.json`，以及项目 `.claude/` 下的对应文件。只读取相关钩子字段，避免将凭据和其他设置写进诊断记录。
3. 如果脚本已经删除且引用确实失效，修改前创建带 UTC 时间戳的完整备份。保留原文件权限和符号链接关系；不要将包含本机信息或凭据的设置文件提交到项目仓库。
4. 只移除命令确实指向该缺失脚本的 hook。逐个检查全部事件：同一应用通常还会注册 `PreToolUse`、`PostToolUse`、`Stop` 等钩子，单独清理 `SessionStart` 会让后续调用继续报错。保留同一匹配项内的其他 hook，仅在匹配项已无动作时移除它。
5. 写入后重新解析 JSON，对比无关设置和其余钩子均未改变，并确认新会话将读取的配置中已无失效引用。配置读回与实际 Claude 启动界面验证应分别记录。

不要为消除一条外部 hook 错误而清空整个 `hooks` 配置，也不要全局关闭所有钩子。
