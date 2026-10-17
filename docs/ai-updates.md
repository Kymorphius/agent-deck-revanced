# AI 工具更新

<!-- matrix/hardened: explicitly requested package updates stay separate from shared-backend activation. -->

在 TUI 按 `S` 打开设置，移到末尾的 **AI UPDATES**，选择 **Manage AI updates…**。目前提供本机 Codex 共享后台的更新管理。

面板分别显示终端 CLI、已安装的后台包、正在运行的后台版本。打开面板和刷新只读本机状态；只有按检查或安装时才访问官方发布服务。

- `r`：刷新本机版本。
- `c`：检查最新公开稳定版。
- `i`：安装更新，保持当前任务运行。
- `R`：单独请求重启生效，需要按 `y` 确认。
- `Esc`：关闭或取消尚未确认的重启。

安装会下载并校验完整 Codex 包，保留旧 release，然后为共享后台选好新版本。当前后台及其任务继续使用原来加载的版本；面板在安装版本与运行版本不同时显示等待生效。**下次真正启动共享后台时使用新版。再次打开一个 Codex 客户端通常仍会连接现有后台，不等于后台重启。**

为了让生效时间由用户控制，安装时会关闭 Codex 原生定时自动更新，并固定已安装的后台版本。它保留其他 Codex daemon 设置，不安装新的系统定时器。后续更新仍可在此面板手动检查、安装。

`R` 是单独的原生后台重启操作，会影响所有连接到该共享后台的客户端，也可能中断其运行任务。请在共享任务都可以停下时确认。安装过程本身不调用重启，不发送停止信号。当前没有自动空闲重启。

命令行使用同一后端：

```sh
agent-deck ai-update status --json
agent-deck ai-update check --json
agent-deck ai-update install --json

# 在合适的时间明确重启；可能中断全部共享任务
agent-deck ai-update restart --yes --json
```

目标目录沿用当前 profile 的 Codex 配置：`CODEX_HOME` 优先，然后是 profile 的 Codex config_dir、全局 Codex config_dir，最后使用 `~/.codex`。自定义 Codex 命令作为可执行文件调用，不作为 shell 脚本求值。面板始终管理本机，不随列表中选中的远程设备切换。

初版只对已核对机制的 Codex `0.163.0` CLI 与运行后台、独立 daemon 完整包布局提供无中断安装。未知版本、缺失或无法确认的后台状态、旧安装布局、无支持锁的平台会明确拒绝安装，不按普通 npm 更新或强制重启来替代。CLI 本身的独立安装和桌面应用更新仍是不同组件。

实现依据是 Codex `rust-v0.163.0` 的 [managed install](https://github.com/openai/codex/blob/rust-v0.163.0/codex-rs/app-server-daemon/src/managed_install.rs)、[updater](https://github.com/openai/codex/blob/rust-v0.163.0/codex-rs/app-server-daemon/src/update_loop.rs) 和 [生命周期管理](https://github.com/openai/codex/blob/rust-v0.163.0/codex-rs/app-server-daemon/src/lib.rs)。安装同时取得原生生命周期锁与安装器兼容锁，关闭自动更新并移除自动更新选择标记，避免已经下载完、等待重启的原生更新器在安装完成后自行切换。
