# Lamp Guide：zen-gate 桌面重构版

Lamp Guide 基于 zen-gate 继续开发，使用 Go、Win32 和 WebView2 将桌面界面与网关整合进一个进程。

- **新项目源码与介绍：** https://github.com/LAGcomcom/Lamp-Guide
- **Windows 安装包：** https://github.com/LAGcomcom/Lamp-Guide/releases/latest
- **迁移指南：** https://github.com/LAGcomcom/Lamp-Guide/blob/main/docs/MIGRATION.md
- **重构版问题反馈：** https://github.com/LAGcomcom/Lamp-Guide/issues

## 两个仓库的关系

本仓库保留 zen-gate 原版、原平台支持及历史发布。Lamp Guide 在新仓库独立维护桌面重构版，保留原项目来源及许可声明。

Lamp Guide 1.2.0 与 zen-gate 1.10.0 使用各自的版本序列，不能按数字大小判断两者的新旧。当前 Lamp Guide 提供 Windows 10 / 11 x64 安装包；macOS、Linux 和无头部署请继续使用原项目。

## 重构版重点

- 全新桌面布局和独立 OpenCode 页面。
- 聚合模型搜索、启用优先排序、底部分页。
- 渠道参与轮换一键切换，按模型批量启用或停用全部渠道。
- 获取上游完整目录，包含积分模型；展开渠道查看积分倍率。
- 改进模型别名合并和已禁用模型的客户端同步。
- 日间 / 夜间模式、切换过渡动画及主题记忆。
- 灯笼 SVG 图标、中文安装向导、快捷方式、卸载及 WebView2 离线运行环境。

## 安装与数据

下载新项目安装包前，请正常退出旧版并备份 `%APPDATA%\zen-gate`（自定义了 `ZEN_GATE_HOME` 时备份实际目录）。Lamp Guide 沿用该数据目录，账号与设置无需从头填写；两种程序共享数据，避免同时运行。

卸载 Lamp Guide 保留用户数据。回退前退出所有实例，必要时恢复迁移前备份。新项目目前通过下载安装包升级；本次介绍发布不会把原版自动更新二进制替换成重构版。
