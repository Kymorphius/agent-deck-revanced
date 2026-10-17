# 分组搜索与定位

<!-- matrix/hardened: compact group-only navigation, separate from session search. -->
在会话列表按 `Alt+S`（macOS `Option+S`）打开，只按分组名称和路径搜索。可以输入 `agent deck` 匹配 `Agent-Deck`，也可以输入 `adk` 这样的顺序缩写。

结果每组占一行，按列显示名称、路径和设备：

```text
分组          路径       设备
Multica       —          本机
cswap         Network    本机
Agent-Deck    Tools      本机
Agent-Deck    Tools      srv
```

普通分组的路径列只显示父路径，顶级组显示 `—`，避免重复名称；显示名称与真实目录末级不同的分组显示实际相对路径。远程设备单独列出，路径不再重复 `remotes/<设备>/` 前缀。

列表按窗口高度决定可见条目数，超过屏幕的结果继续滚动，不固定为十项。窄窗口收起路径列，保留分组名和设备。底部显示当前所选分组的设备与完整相对路径，过长时从中间缩略，保留路径尾部。

- `↑/↓`、`Ctrl+K/J` 或滚轮选择结果。
- `Enter` 展开并定位分组；为显示目标，会清除状态、时间等视图过滤并回到普通分组视图，保留显式 `-g` 范围。
- `Esc` 取消，保留打开前的视图。
- 每次打开清空关键词，重新打开可纳入刚加载的本地或远程分组。

可在 `[hotkeys]` 设置 `group_search = "alt+s"`，重绑为其他键或用空字符串禁用。
