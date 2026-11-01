# CPA Suite

[English](README.en.md)

CPA Suite 是 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)（CPA）的一体化管理插件：在一个插件里提供 API Key 计费与套餐额度、并发与路由规则、客户端限制、额度预测、用量排行、降智监控与告警中心，界面风格与 CPA 管理面板一致。

## 来源与许可

CPA Suite 基于 [haowang02/cpa-plugin-key-billing](https://github.com/haowang02/cpa-plugin-key-billing)（cpa-key-billing）v1.3.20 修改扩展，原项目以 MIT License 发布，版权归原作者 Hao Wang 所有。本项目同样以 MIT License 发布，完整许可与版权声明见 [LICENSE](LICENSE)。对上游的改动清单见 [docs/UPSTREAM_PATCHES.md](docs/UPSTREAM_PATCHES.md)。感谢原作者。

## 功能

- **计费与套餐**：按 API Key 设置套餐、额度周期与模型价格，记录每次请求用量（继承自 cpa-key-billing）。
- **路由与并发**：按 Key 的路由规则限定可用账号与顺序、模型感知选号、并发槽位管理。
- **客户端限制**：按 User-Agent 等规则观察或拦截客户端（可一次性导入 client-guard 的规则与统计）。
- **额度预测 / 用量排行**：根据官方额度读数预测 Codex、Grok 账号的周期余量，并给出异常提示。
- **降智监控与告警中心**：被动监控上游质量，告警分页、筛选、分组、批量处理与导出，保留天数可在页面设置。
- **自助余量查询**：Key 持有者可用自己的 Key 查看余量（兼容 cc-switch 用量查询格式）。

## 环境要求

- CLIProxyAPI v8.0.44 或更高版本（已在 v8.0.48、v8.0.49 上验证）。
- 平台：linux amd64 / arm64、macOS amd64 / arm64、windows amd64。

## 安装

### 插件商店（推荐）

在 CPA 管理面板（CPAMC 或 CPAMP）的插件商店中搜索 `cpa-suite` 并安装，然后重启 CPA。

### 手动安装

从 [Releases](https://github.com/Samuel-wyj/cpa-suite/releases) 下载对应平台的 `cpa-suite_<版本>_<系统>_<架构>.zip`，用 `checksums.txt` 校验后解压，把 `cpa-suite.so`（macOS 为 `.dylib`，Windows 为 `.dll`）放到 CPA 的 `plugins/` 目录，重启 CPA。

可选配置（`config.yaml`）：

```yaml
plugins:
  configs:
    cpa-suite:
      state_file: "plugins/cpa-suite.db"   # 默认值
      # import_from: "plugins/cpa-key-billing-state-v1.db"  # 从 cpa-key-billing 迁移
      # modules: { client_guard: "observe", forecast: "passive" }
```

管理页面：`http://<CPA 地址>:<端口>/v0/resource/plugins/cpa-suite/ui`。

## 不能与 cpa-key-billing 同时安装

CPA Suite 已包含 cpa-key-billing 的全部计费、额度、并发与路由功能，两者同时运行会重复扣费、争抢账号调度。CPA Suite 检测到 `plugins/` 下存在 cpa-key-billing 时只观察、不执行，并在页面提示冲突。请先移除 cpa-key-billing 再启用 CPA Suite。

## 从 cpa-key-billing 迁移

1. 停止 CPA，备份 `plugins/cpa-key-billing-state-v1.db`（或你配置的 state_file）。
2. 把 cpa-key-billing 的动态库移出 `plugins/` 目录（例如移到 `plugin-backups/`）。
3. 安装 CPA Suite，在 `plugins.configs.cpa-suite` 中设置 `import_from` 指向旧数据库。
4. 启动 CPA。CPA Suite 首次启动时以只读方式把套餐、Key、价格与用量复制到自己的 `plugins/cpa-suite.db`，旧库不会被修改。
5. 要回退时，停止 CPA，移除 cpa-suite 动态库，放回 cpa-key-billing 即可，旧数据库原样可用。

若之前装过 client-guard：把 `client-guard.so` 移出 `plugins/` 后重启，CPA Suite 会一次性只读导入它的规则与统计并接管；仍在加载时 CPA Suite 的客户端限制模块会自动暂停，避免重复拦截。

## 构建

```bash
scripts/build.sh                 # linux/amd64、linux/arm64、windows/amd64（需要 Go 与 zig）
go test ./...
```

推送 `v*` 标签时，GitHub Actions（`.github/workflows/release.yml`）会构建全部 5 个平台并发布 zip 与 `checksums.txt`。

## 许可

MIT，见 [LICENSE](LICENSE)。
