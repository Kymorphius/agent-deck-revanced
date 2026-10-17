# CLAUDE.md（Agent Deck Revanced 自维护加固版）

本仓库是脱敏后独立导出的源码快照，先读 `REVANCED.md`。默认分支为 `main`；`matrix/hardened` 仍是代码中的加固注释标记。历史交接文档中的旧分支、标签及提交引用仅用于说明来源。

**先读 `MATRIX-HARDENED.md`**，里面有为什么加固、R1–R8 要求、改动地图、编译测试方法和验收清单。如果维护环境另有 `MATRIX-LOCAL.md`，只在本机使用并保持不入库。

最重要的规矩：
- 在分支 `main` 上工作；`origin` 是 Kymorphius/agent-deck-revanced，上游项目为 asheshgoplani/agent-deck。只在用户要求时推送；本机专属内容只写进 `MATRIX-LOCAL.md`（不入库），绝不提交到远程仓库。
- 不得破坏 R1–R8：更新请求不带令牌、默认不检查更新；不装自动更新定时器；没有遥测；不发反馈；钩子只在显式安装时写入；`dangerous_mode` 默认 false。每处改动都写 `matrix/hardened:` 注释。
- matrix/hardened: 跑测试统一用 `scripts/run-isolated.py go test ...` 隔离 HOME 和 XDG、复用 Go 缓存并自动回收临时目录（见文档第 4 节）。禁止裸 `HOME=$(mktemp -d)` 测试命令。跑完检查并清理 launchctl 任务和 tmux 会话：隔离 HOME 挡不住它们。
- 安装只用 `scripts/matrix-install.sh`，装到 `~/.local/bin/agent-deck`。绝不用 Homebrew 装 agent-deck。
- 配置 `[claude].command` 时尊重用户自己的启动包装；机器专属的绝对路径只放在本机配置中。
