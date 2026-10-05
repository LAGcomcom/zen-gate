<div align="center">

<img src="assets/icon-256.png" width="88" alt="Zen Gate">

# ZEN—GATE

**把 OpenCode Zen 免费模型，装进你所有的 AI Agent。**

一个 Windows 桌面托盘程序：本地起一个 OpenAI / Anthropic 兼容网关，
自动探测并接入你机器上已安装的 AI Agent——模型选择器里直接出现免费模型。

[![release](https://img.shields.io/github/v/release/LAGcomcom/zen-gate?style=flat-square&label=%E7%89%88%E6%9C%AC)](https://github.com/LAGcomcom/zen-gate/releases/latest)
[![downloads](https://img.shields.io/github/downloads/LAGcomcom/zen-gate/total?style=flat-square&label=%E4%B8%8B%E8%BD%BD)](https://github.com/LAGcomcom/zen-gate/releases)
[![go](https://img.shields.io/badge/Go-1.23-00ADD8?style=flat-square&logo=go&logoColor=white)](https://go.dev)
[![platform](https://img.shields.io/badge/Windows-10%2F11-0078D6?style=flat-square&logo=windows11&logoColor=white)](https://github.com/LAGcomcom/zen-gate/releases)
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
  Crush / ChatBox / Aider / Qwen Code / Continue 后，一键注入配置（先备份，关闭即还原），
  重启对应 Agent 就能在模型选择器里看到免费模型；
- **协议完整移植**：会话铸造、指纹门、三种线协议（chat / responses / messages）、
  纯思考断流恢复——全部来自 MIT 协议层参考实现 dsh-our-free-model；
- **单模型体检**：每个模型可单独探测可用性与首字延迟，探测历史持久化、跨重启可查。

## 亮点

| | |
|---|---|
| 🔄 **限流自动切换** | 模型被限流时自动换下一个可用模型接住请求，响应头标注实际模型 |
| 🧩 **自建端点并入** | 把本地 llama.cpp / Ollama 等 OpenAI 兼容端点挂进同一个网关，Agent 一处配置即可用 |
| 📊 **额度测算** | 无官方余额 API 也能估：限额时段追踪 + 恢复时间预估 + 日额度进度条 |
| ⏱ **首字历史** | 每次探测的首字延迟入样本环，重启不丢，模型页直接看平均首字 |
| 🌡 **GitHub 式热力图** | 365 天用量热力图 + 多模型趋势折线 + 每 / 周 / 累计三种视图 |
| 🖥 **托盘常驻** | 关窗即进托盘、开机自启、系统通知、跟随系统代理 |
| 🛡 **只听本机** | 网关仅绑定 127.0.0.1，管理端有同源护栏，配置先备份再改 |

## 自建端点（本地模型）

除了免费车道，zen-gate 还能把**你自己搭的 OpenAI 兼容端点**并进同一个网关 ——
本机的 llama.cpp / Ollama / vLLM，局域网里另一台机器，或任何第三方 OpenAI 兼容接口。
在「上游」页填地址和模型 id 即可，它们会和免费模型一起出现在 Agent 的模型选择器里。

**为什么是直通转发而不是复用免费车道的协议层。** 免费车道要过 opencode.ai 的校验，
必须铸造会话 id 并改写请求体（注入四件套工具、规范化工具名）。自建端点不吃这一套，
强行套用反而会污染请求：注入的工具会让本地服务困惑，它认识的私有字段会被解码器丢掉。
所以这条路径**原样转发、原样返回** —— 流式分片、SSE 注释、厂商扩展字段都原封不动。

| | 免费车道 | 自建端点 |
|---|---|---|
| 鉴权 | 铸造会话 + 指纹门 | 你的 API Key（或留空） |
| 请求体 | 改写（指纹、effort） | **原样转发** |
| 返回体 | 解码 → 重建 | **原样返回** |
| 可用性 | 每轮探测 + 限额追踪 | 不探测（不可达时如实报 502） |

模型会同时出现在 `/v1/models`（OpenAI 兼容客户端）和 `/v1/codex-catalog`（Codex 动态目录）
里，因此**重启 Codex 后仍然可见**，同时免费模型的自动跟随也不受影响。

## 快速开始

1. 从 [Releases](https://github.com/LAGcomcom/zen-gate/releases/latest) 下载 `zen-gate.exe`，双击运行（托盘出现图标）；
2. 到「Agent 适配」页打开你装的 Agent 开关 → 重启该 Agent；
3. 模型选择器里出现免费模型，直接用。

> 想接 ChatBox / Cherry Studio / 任意 SDK？「接入」页有每个客户端的填法和 curl 示例。


## 从源码构建

```bash
go build -trimpath -ldflags "-s -w -H=windowsgui   -X zen-gate/internal/gateway.Version=1.2.1   -X zen-gate/internal/update.Current=1.2.1" -o dist/zen-gate.exe ./cmd/zen-gate
```

发版：推一个 `v*` 标签（GitHub Actions 自动构建发布），或本地
`powershell -File tools
elease.ps1 -Version 1.2.2`。

## 一键更新

应用每 6 小时检查本仓库的 Releases（走系统代理）。发现新版本时总览页出现「一键更新」
按钮——自动下载、替换、重启，全程约 10 秒。
