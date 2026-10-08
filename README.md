<div align="center">

<img src="assets/icon-256.png" width="88" alt="Zen Gate">

# ZEN—GATE

**把 OpenCode Zen 免费模型，装进你所有的 AI Agent。**

一个桌面托盘程序（Windows 10/11、macOS 13+ 与 Linux）：本地起一个 OpenAI / Anthropic
兼容网关，自动探测并接入你机器上已安装的 AI Agent——模型选择器里直接出现免费模型。

[![release](https://img.shields.io/github/v/release/LAGcomcom/zen-gate?style=flat-square&label=%E7%89%88%E6%9C%AC)](https://github.com/LAGcomcom/zen-gate/releases/latest)
[![downloads](https://img.shields.io/github/downloads/LAGcomcom/zen-gate/total?style=flat-square&label=%E4%B8%8B%E8%BD%BD)](https://github.com/LAGcomcom/zen-gate/releases)
[![go](https://img.shields.io/badge/Go-1.26-00ADD8?style=flat-square&logo=go&logoColor=white)](https://go.dev)
[![platform](https://img.shields.io/badge/Windows-10%2F11%20%7C%20macOS%2013%2B%20%7C%20Linux-0078D6?style=flat-square&logo=windows11&logoColor=white)](https://github.com/LAGcomcom/zen-gate/releases)
[![license](https://img.shields.io/github/license/LAGcomcom/zen-gate?style=flat-square)](LICENSE)

[下载最新版](https://github.com/LAGcomcom/zen-gate/releases/latest) · [问题反馈](https://github.com/LAGcomcom/zen-gate/issues)

</div>

---

## 这是什么

OpenCode Zen 提供了一批**免登录、免计费**的模型车道，但它们没有标准的 API Key 体系，
一般客户端接不上。Zen Gate 在本机把这条车道封装成**标准 OpenAI / Anthropic 兼容接口**，
并自动完成所有 Agent 的配置注入：

<div align="center"><img src="docs/screenshot-home.png" width="820" alt="Zen Gate 总览"></div>

- **全自动适配**：检测到 ZCode / OpenCode / Codex / Claude Code / DeepSeek Harness /
  Crush / ChatBox / Aider / Qwen Code / Continue / WorkBuddy / Qoder 后，一键注入配置（先备份，关闭即还原），
  重启对应 Agent 就能在模型选择器里看到免费模型（WorkBuddy 无需重启，保存后自动热加载；Qoder 注入
  `~/.qoder-cn/settings.json` 自定义供应商，IDE 本地直连）；
- **自定义 API 接入**：免费车道之外，可添加任意 OpenAI / Anthropic 兼容上游——
  内置 NVIDIA NIM、Google Gemini、GitHub Models、Groq、Cerebras、Mistral、OpenRouter、
  HuggingFace、硅基流动、魔搭、智谱、LongCat 等 13 个免费商预设一键填充，
  填上自己的 Key 后自动检索模型列表，模型以 `供应商ID/模型名` 出现在所有 Agent 选择器里；
- **协议层实测打磨**：会话铸造、指纹门、三种线协议（chat / responses / messages），
  断流续写、空停续写、截断自动续写三重兜底——全部按真实客户端行为逐项实测；
- **Kilo 免费池**：第二条免凭据免费源直接并入目录，与免费车道共用路由、
  限流切换与探测看板；免费池提示词可能被上游记录，别交机密内容；
- **单模型体检**：每个模型可单独探测可用性与首字延迟，探测历史持久化、跨重启可查。

## 亮点

| | |
|---|---|
| 🧭 **多模态智能路由** | 请求带图 / 音频 / 文件时，限流切换只会落到支持该模态的模型；模型能力三层打标（内置精选表 → 免费车道大模型 AI 标注 → 一键实测），徽章直接标在模型页 |
| 🎯 **路由策略** | 智能路由总开关随时启停；failover 候选链按「目录顺序 / 首字延迟优先」排序；免费车道全部耗尽可单向回落到你的自定义 API（默认关，消耗自己的 Key） |
| 🔄 **限流自动切换** | 模型被限流时自动换下一个可用模型接住请求，响应头标注实际模型 |
| 📢 **公告系统** | 编辑仓库根目录 `announcements.json` 即向所有用户发公告（info/warn/critical 分级、生效时段、已读跟踪），总览页每 6 小时同步 |
| 🔌 **自定义 API** | 免费商预设一键填充 + 自动检索模型列表，自己的 Key 自己填，用量照常入账 |
| 📖 **能力元数据进标准接口** | `GET /v1/models` 每条模型除 OpenAI 字段外附带 `context_window`、`max_output_tokens`、`input_modalities`、`reasoning`、`supported_reasoning_levels` 与 `capability_source`（实测 > 上游声明 > AI标注 > 规则 / 未标注）——一键实测的结论不用打开面板也能被 Agent、SDK 或任意脚本读到；重启后实测结论仍随 `tags.json` 生效 |
| 🌐 **订阅轮询** | 粘贴代理订阅（vless / vmess / ss / trojan / hy2 / tuic），内嵌 sing-box 侧车转成本地 socks 池，**每个请求轮换一个健康出口**——免费车道额度按出口 IP 计，多节点就是多份额度；节点健康探测 + 拨号失败自动跳下一节点 |
| 📊 **额度测算** | 无官方余额 API 也能估：限额时段追踪 + 恢复时间预估 + 日额度进度条 |
| ⏱ **首字历史** | 每次探测的首字延迟入样本环，重启不丢，模型页直接看平均首字 |
| 🌡 **GitHub 式热力图** | 365 天用量热力图 + 多模型趋势折线 + 每 / 周 / 累计三种视图 |
| 🖥 **托盘常驻** | 关窗即进托盘/菜单栏、开机自启（注册表 / launchd）、系统通知、跟随系统代理（WinINET / `scutil --proxy`） |
| 🛡 **只听本机** | 网关默认仅绑定 127.0.0.1；设置里可放开局域网给同网段设备调用（接口仍必须带 API Key，管理端固定只接受本机），另有同源护栏、配置先备份再改 |

## 快速开始

**Windows** — 从 [Releases](https://github.com/LAGcomcom/zen-gate/releases/latest) 下载
`zen-gate.exe`，双击运行（托盘出现图标）。

**macOS** — `tools/build-macos.sh` 产出 `dist/Zen Gate.app`，安装并启动：

```bash
tools/build-macos.sh
cp -R "dist/Zen Gate.app" /Applications/
open "/Applications/Zen Gate.app"
```

**Linux** — `tools/build-linux.sh` 产出 `dist/Zen_Gate-x86_64.AppImage`
（自包含 GTK3 + appindicator，无需安装依赖），下载后 `chmod +x` 双击运行即可：

```bash
tools/build-linux.sh 1.2.1
chmod +x dist/Zen_Gate-x86_64.AppImage
./dist/Zen_Gate-x86_64.AppImage
```

Linux 版没有原生内嵌窗口：管理页在默认浏览器打开，托盘常驻（AppImage 内的
`zen-gate.desktop` 也支持「开机自启」开关，写入 `~/.config/autostart`）。

**Debian / Ubuntu / Deepin 等** — `tools/build-deb.sh` 产出 `dist/zen-gate_*.deb`
（依赖系统 `libgtk-3-0` 与 `libayatana-appindicator3-1`，体积更小），安装：

```bash
tools/build-deb.sh 1.2.1
sudo apt install ./dist/zen-gate_1.2.1-linux_amd64.deb
zen-gate   # 或从应用菜单启动
```

两边都是：到「Agent 适配」页打开你装的 Agent 开关 → 重启该 Agent →
模型选择器里出现免费模型。

> 想接 ChatBox / Cherry Studio / 任意 SDK？「接入」页有每个客户端的填法和 curl 示例。


## 从源码构建

Windows：

```bash
go build -trimpath -ldflags "-H=windowsgui -X zen-gate/internal/gateway.Version=1.2.1 -X zen-gate/internal/update.Current=1.2.1" -o dist/zen-gate.exe ./cmd/zen-gate
```

不要加 `-s -w`（剥符号表）：火绒一分钟内就会把这样产出、刚解压出来的
`zen-gate.exe` 直接删掉，表现成"下载完文件就不见了"。

macOS（需要 cgo 与 Xcode 命令行工具；脚本负责编出 arm64 + x86_64 双架构、
打 .app 包、生成 .icns、做 ad-hoc 签名并压 zip）：

```bash
tools/build-macos.sh 1.2.1
```

Linux（需要 cgo、gcc、`libgtk-3-dev` 与 `libayatana-appindicator3-dev`；
脚本编译后自动用 linuxdeploy + appimagetool 打出自包含 AppImage）：

```bash
tools/build-linux.sh 1.2.1
```

Debian 系 .deb（同一二进制，依赖系统库，体积更小）：

```bash
tools/build-deb.sh 1.2.1
```

发版：推一个 `v*` 标签（GitHub Actions 自动构建发布），或本地
`powershell -File tools\release.ps1 -Version 1.2.2`。macOS 包目前**不**随标签
发布——只有 ad-hoc 签名，缺少 Developer ID 与公证，发出去只会被 Gatekeeper 拦。
Actions 里的 `build-macos` job 负责每次构建验证，产物作为 workflow artifact 留存。

## 一键更新

应用每 6 小时检查本仓库的 Releases（走系统代理）。发现新版本时总览页出现「一键更新」
按钮——自动下载、替换、重启，全程约 10 秒。

一键更新只在 Windows 开启：那里发行物就是一个可以在运行时改名的 `.exe`。
macOS 的发行物是整个 `.app`，覆盖包内二进制会破坏签名，所以总览页只给「发布页 →
」链接，由用户自行下载替换。

## 平台差异

三个平台的实现按文件后缀拆分（`*_windows.go` / `*_darwin.go` / `*_linux.go`），
共用代码不带后缀：

| | Windows | macOS | Linux |
|---|---|---|---|
| 窗口 | WebView2 + Win32 无边框窗口 | WKWebView + NSWindow（全尺寸内容视图，隐藏系统红绿灯） | 无内嵌窗口：管理页在默认浏览器打开（`xdg-open`），托盘常驻 |
| 托盘 | `getlantern/systray`，独占一个锁定的 goroutine | 同上，但与窗口共用主线程——AppKit 只允许主线程建窗口 | 同上，GTK 主循环跑在主 goroutine（libayatana-appindicator） |
| 开机自启 | `HKCU\...\Run` | `~/Library/LaunchAgents/com.lagcomcom.zen-gate.plist` + `launchctl bootstrap` | `~/.config/autostart/zen-gate.desktop`（AppImage 运行时指向 `.AppImage` 本体） |
| 系统代理 | `HKCU\...\Internet Settings`（WinINET） | `scutil --proxy` | `https_proxy` / `http_proxy` 环境变量 |
| 通知 | PowerShell + WinRT toast | `osascript -e 'display notification'` | `notify-send`（libnotify） |
| 数据目录 | `%APPDATA%\zen-gate` | `~/Library/Application Support/zen-gate`（`ZEN_GATE_HOME` 可覆盖） | `$XDG_CONFIG_HOME`（或 `~/.config`）`/zen-gate`（`ZEN_GATE_HOME` 可覆盖） |
| 单实例 | 命名互斥体 | `zen-gate.lock` 上的 `flock`（进程退出即释放，不会留死锁） | 未做进程级互斥：靠网关端口绑定兜底（同一端口被占时第二个实例退出） |
| 一键更新 | 支持 | 不支持，只跳发布页 | 不支持（AppImage 为只读挂载），只跳发布页 |

窗口按钮（最小化 / 最大化 / 关闭 / 拖拽）在两边都通过页面注入的
`window.zengate*` 绑定实现，所以 dashboard 一份代码三处跑。
