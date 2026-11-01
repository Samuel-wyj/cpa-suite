# CPA Suite 对上游文件的改动清单

上游：cpa-key-billing（MIT，© 原作者，LICENSE 原样保留）。基线 v1.3.20 @ 741ef9f。
原则：上游文件只加“钩子行”（行尾注释 `// CPA Suite hook`），全部逻辑在 `internal/suite/` 与 `*/suite_*.go`。
`scripts/sync-upstream.sh` 会检查：凡是与上游有差异、又不属于 suite 自有文件的路径，必须在本表列出，否则中止。

## 修改的上游文件

| # | 文件 | 改动 | 原因 | 合并冲突时怎么办 |
|---|---|---|---|---|
| 1 | `internal/plugin/types.go` | `PluginID`/`PluginName` → `cpa-suite`；`Version = suite.Version`；`MenuLabel = "CPA Suite 控制台"`；import suite | 与 cpa-key-billing 区分身份（插件 ID、配置键 `plugins.configs.cpa-suite`、菜单名） | 接受上游新内容后重新改这 4 行；上游新 `Version` 写进 `internal/suite/config.go` 的 `UpstreamVersion` |
| 2 | `internal/plugin/app.go` | `App.suite` 字段 + newApp 初始化；`handleMethod` 顶部 `suiteGate(method, request)`（0.3 起多传 request，客户端限制在计费准入之前判定）；`Shutdown` 里 `a.suite.Close()`；`configure` 中 DecodeConfig 前 `suitePrepareConfig`；`ConfigFields` 末尾 `append(..., suiteConfigFields()...)` | 只读导入、冲突时“仅观察”、模块开关、额外配置项 | 保留上游逻辑，重新插入 5 处钩子行 |
| 3 | `internal/plugin/management.go` | 注册路由末尾追加 `suiteManagementRoutes()`；UI `Body: a.suiteUI(uiHTML)`；`routeManagement` 开头 `suiteManagement` | `/suite/status`、`/suite/modules`、`/suite/clients*`（0.3）与红/蓝/橙横幅 | 同上，3 处钩子行 |
| 4 | `internal/plugin/app_test.go` | `wantRoutes` 加入 suite 路由 | 上游测试枚举全部路由，新增路由属预期 | 重新加 3 行 |
| 6 | `internal/plugin/web/core.js` | 前两行 `PLUGIN_BASE`/`ACCOUNT_BASE` 改为由 `PLUGIN_ID` 拼接；`PLUGIN_ID` 读服务端注入的 `<meta name="cpa-plugin-id">`（`suiteUI` 注入），兜底从页面路径解析 | 上游把 `/v0/management/plugins/cpa-key-billing` 写死，改 ID 后界面所有接口 404（0.1 的 bug）。`TestSuiteWebAssetsHaveNoHardcodedPluginPaths` 会在上游新增写死路径时失败 | 重新改这 3 行；若上游新增别的写死路径，测试会指出文件 |
| 7 | `internal/plugin/web/core.js`（0.3） | `ADMIN_TAB_IDS` 加 `"clients"`；`deleteCredential` 首行 `suiteDeleteCredential`；`saveCredential`/`recalledCredential` 首行：无 WebCrypto 时转 `suiteSaveCredential`/`suiteRecalledCredential` | 客户端标签页；http 下登录保持（https 仍走上游加密保险箱，条件与上游 `openCredentialDB` 一致） | 重新加 4 行 |
| 8 | `internal/plugin/web/theme.js` | `panelStoredValue` 同时识别 CPAMP 1.14 的 `enc::v2::`（密钥 `…secure-storage\|v2\|host`），原来只认 v1 | 嵌入 CPAMP 时借用面板记住的管理密钥，免二次登录 | 重新改 2 行 |
| 9 | `internal/plugin/web/keys.js` | 表头 `...suiteKeyHeaders()`、行 `...suiteKeyCells(view)`（在“操作”列前） | API Key 列表“允许客户端”列 + 编辑 | 重新加 2 行 |
| 10 | `internal/plugin/web/data.js` | `createPages` admin 分支加 `result.clients` / `result.overview` / `result.modules`，0.5 起再加 `result.forecast` / `result.ranking`，0.6 起再加 `result.alerts` / `result.monitor`（共 7 行） | 客户端页、总览、模块页、额度预测、用量排行 | 重新加 5 行 |
| 11 | `internal/plugin/web/app.js` | `setAdminTab` 循环后调用 `suiteAfterAdminTab(selected)`（0.4 起替换原 `tab-clients` 显隐行） | suite 分区显隐、设置页子块拆分、导航同步 | 重新加 1 行 |
| 12 | `internal/plugin/web/ui.html` | 标签按钮 `data-tab="clients"`/`"overview"`/`"modules"`，0.5 起加 `"forecast"`/`"ranking"`、0.6 起加 `"alerts"`/`"monitor"`（被 suite 导航隐藏，仅供上游 activeTab 识别）、`<section id="tab-clients">`、`/*suite.js*/`、`/*suite-layout.js*/`、`/*suite-insights.js*/`（0.5）、`/*suite-monitor.js*/`（0.6）、`/*suite.css*/` | suite 前端与新布局 | 重新加 10 行 |
| 13 | `internal/plugin/web/locales/en.json`、`internal/plugin/web/locales/zh-CN.json` | 末尾追加 `suite.*` 词条（由 `scripts/suite_locales.py` 从 `internal/suite/locales/` 生成，勿手改） | 上游测试要求所有界面文字走词表 | **不要手工合并**：`sync-upstream.sh` 自动取上游版本后重跑 `suite_locales.py` |
| 14 | `internal/plugin/web/core.js` | `ADMIN_TAB_IDS` 增加 `"overview"`、`"modules"`（同一行，已有 clients）；0.5 的 `"forecast"`/`"ranking"` 不改 core.js，由 `suite-layout.js` 首行 `ADMIN_TAB_IDS.splice(...)` 追加 | `#admin/overview` 等路由可用 | 重新改 1 行 |
| 15 | `internal/plugin/web/format.js` | `apiKeyIdentity` 中 `view.label` 为空时回退 `suiteAliasFor(scope)`（仅管理员） | CPAMP Key 别名自动作为显示名（只读，不写入） | 重新改 1 行 |
| 16 | `internal/plugin/auth_files.go`（0.5，xAI 额度修复） | `fetchAuthQuota`：`paidXAI := provider == "xai" && paidXAICredential(credential)` 只设 Plan "Paid"、**不再提前返回**；switch 之后 `if paidXAI && err != nil && len(result.Quota) == 0 { return result, nil }`（保持上游“付费账号无额度不报错”）；`fetchXAIQuota` 中周额度调用前 `suiteFirstRow := len(result.Quota)`、最终 return 前 `suiteFixXAIQuota(result, weeklyConfig, suiteFirstRow, time.Now())`。逻辑在 `suite_xai_quota.go` | 上游对 JWT tier ≥ 1 的账号（SuperGrok）直接返回 Paid、从不调用 billing 接口，界面显示“上游未返回可展示的额度”；周期重置后接口省略 `creditUsagePercent` 时上游不当作 0%；月度 0/0 行无意义 | 4 处钩子行均带 `// CPA Suite hook`；若上游已修（不再提前返回、能识别 0%），删除钩子与 `suite_xai_quota.go`，保留 `TestSuiteRealSuperGrokShapes` 作回归 |
| 17 | `internal/suite/clientguard/intercept.go`（0.5） | 新增只读方法 `(*Guard).DailyCounts()` | 用量排行的“拦截激增”异常 | 移植文件，非上游；同步 client-guard 新版时重新加该方法 |
| 5 | `internal/plugin/i18n_test.go` | 中文检查不再包含 `MenuLabel` | 私有构建菜单名是中文 | 重新改 1 行 |

未改动（刻意）：计费、额度、套餐、路由、调度、`schemaVersion`、凭证盐 `cpa-key-billing:credential:v1`、`cycles_json`、`cmd/`；前端只有上表的钩子行，全部逻辑在 `web/suite.js`。

## suite 自有文件（上游不存在，合并不会冲突）

- `internal/suite/*`：配置拆分、模块开关、冲突扫描、只读导入、扩展库、控制器
- `internal/plugin/suite_hooks.go`、`suite_hooks_test.go`、`suite_parity_test.go`、`suite_client_test.go`、`suite_cgparity_test.go`、`suite_ui_test.go`
- `internal/suite/clientguard/`（client-guard 0.1.2 移植，规则/匹配/统计逻辑原样，存储改为可插拔 Backend）、`internal/suite/clientmodule.go`、`internal/suite/locales/`
- `internal/plugin/web/suite.js`（客户端页、Key 列、规则编辑器、http 登录保持）
- `internal/plugin/web/suite-layout.js`、`internal/plugin/web/suite.css`（0.4 新布局层：5 个分组 + 子标签、总览、模块页、CPAMP 别名；只复用上游面板与接口，不改上游逻辑）
- 0.5：`internal/plugin/web/suite-insights.js`（额度预测页、用量排行页、总览预测摘要与异常徽标）；`internal/plugin/suite_forecast.go`、`suite_ranking.go`、`suite_xai_quota.go` 及其测试；`internal/suite/forecast.go`（纯计算）、`forecast_store.go`（扩展库 v2 表、主库只读连接）；`scripts/diag_xai_billing.sh`（xAI 只读诊断，令牌打码）、`scripts/suite_ext_downgrade_v1.py`（回滚到 0.4 前把扩展库降回 v1）、`scripts/seed_suite5.py`（仅测试用种子数据）、`scripts/e2e_suite5.sh`、`scripts/e2e_upgrade4.sh`、`scripts/e2e_upgrade5.sh`、`scripts/uitest/ui_suite5.mjs`
- 0.6：`internal/plugin/web/suite-monitor.js`（降智被动监控页、告警中心、导航徽标）与 `suite.css` 末尾样式；`internal/plugin/suite_monitor.go`、`suite_alerts.go` 及测试；`internal/suite/monitor.go`（纯计算）、`monitor_store.go`、`alerts.go`（扩展库 v3 表）；`suite_hooks.go` 中 usage.handle 观察钩子与 5 条路由；`scripts/suite_ext_downgrade_v2.py`（回滚到 0.5 前把扩展库降回 v2）、`scripts/seed_suite6.py`（仅测试用）、`scripts/e2e_suite6.sh`、`scripts/uitest/ui_suite6.mjs`。不改动任何上游文件之外的新钩子（usage.handle 观察挂在已有的 suiteGate 内）
- 0.6 额度预测引擎：`internal/suite/forecast_cycles.go`（周期检测含 A-B-A 拒绝、里程碑中位数容量 + P25–P75、置信度、覆盖模式、三条消耗线与节奏、token 当量容量）及测试——参考 Autsunset/cpa-quota-estimator（MIT）的思路独立实现，未复制代码，文件头注明；`internal/suite/ledger_store.go`（扩展库 v3 表 `forecast_ledger`、`candy_runs`）；`internal/plugin/suite_forecast.go` 周期台账与按 Asia/Shanghai 的月度汇总
- 0.6 糖果测试：`internal/plugin/suite_candy.go` 及测试、`internal/plugin/candy/`（`codex_context.json` 与提示词取自 haowang02/cpa-plugin-codex-candy-eval，MIT，Copyright (c) 2026 Hao Wang，许可证原文 `LICENSE-cpa-codex-candy-eval` 与 `NOTICE.md`）。不读取 candy-eval 的任何文件或状态。测试请求经 `host.model.execute`（宿主自动跳过本插件的拦截/路由）发出，其 usage 记录在 usage.handle 中被认领并丢弃：不写 request_events、不计费、不进排行/统计/降智监控；额度预测把与测试时段重叠的里程碑区间排除
- 测试：`scripts/dummy_provider.py` 增加糖果题应答（答 21、reasoning_tokens 4321、打印一行日志）；`scripts/e2e_suite6.sh` 用 base_url 指向 dummy 的 Grok 认证文件跑糖果测试（Codex OAuth 认证文件不能改 base_url，Codex 请求路径由带 mock 宿主的 Go 单测覆盖）；`scripts/e2e_upgrade6.sh`
- `scripts/clientguard_parity.sh` + `scripts/cgparity/`（与原版 client-guard 0.1.2 逐请求对比）、`scripts/suite_locales.py`、`scripts/uitest/ui_suite3.mjs`、`scripts/uitest/ui_suite4.mjs`、`scripts/uitest/cpamp-sim.html`
- `internal/billing/suite_clock.go`（测试用时钟注入）、`internal/sqlite/suite_export.go`（导出 schemaVersion）
- `scripts/parity.sh`、`scripts/parity_normalize.py`、`scripts/sync-upstream.sh`、`scripts/resolve_identity_conflict.py`、`scripts/e2e_suite.sh`（用上游真实 CPA 端到端脚本测本构建）、`scripts/e2e_migration.sh`（真实 CPA 迁移/冲突/回滚演练）、`scripts/e2e_ui.sh` + `scripts/uitest/`（真实 CPA + 无头 Chrome 加载界面，管理员与 API Key 两种登录逐个标签页检查所有接口无 404、全部指向本插件 ID）、`docs/`、`README.suite.md`

## 数据库版本策略

- 主库 `plugins/cpa-suite.db` 完全是上游 schema（当前 v17），由上游代码迁移；suite 不往里加表。
- suite 自己的数据在 `<state_file 去掉 .db>.suite-ext.db`，独立 `user_version`，有自己的迁移列表；遇到更新的版本拒绝启动而不是降级。
  - v1（0.1–4）：`suite_meta`（模块记录、client-guard 规则与统计）。
  - v2（0.5）：加 `quota_samples`（官方额度读数，保留 40 天）、`quota_fetch`（每账号查询缓存/限频）。回滚到 0.4 前须停 CPA 运行 `scripts/suite_ext_downgrade_v1.py`（只删这两张缓存表，`suite_meta` 不动），否则 0.4 报 “is v2, newer than this build (v1)” 并不注册插件。
  - v3（0.6）：加降智监控/告警表与 `forecast_ledger`、`candy_runs`。回滚到 0.5 前运行 `scripts/suite_ext_downgrade_v2.py`（删除 6 张 v3 表）。
- 导入器接受 `user_version` 1…`sqlite.SupportedSchemaVersion` 的旧库，自动跟随上游；更新的版本拒绝。
- 上游升 schema（如 v18）时：合并后 `sync-upstream.sh` 会提示；上游测试覆盖迁移，`TestImportRealUpstreamDatabase` 用新上游写出的库验证导入。

## 版本号

版本号采用语义化版本（`MAJOR.MINOR.PATCH`），1.0.0 为首个正式版；1.0.0 之前的内部预发布版本在本文中记作 0.N。

## 0.6 补充

- 客户端预设修复：`internal/suite/clientguard/rules.go`。「Codex 全家」由 `re:^codex[_-]` 改为 `re:^codex[_ -]`，修复 Codex Desktop 带空格的 UA（`Codex Desktop/0.162…`）被 403 的问题；Codex CLI/桌面/IDE 子预设同样接受空格写法；Gemini CLI 加了词边界，Kilo Code 增加 `Kilo-Code/` 写法。正则一律自动加 `(?i)`，不区分大小写（0.1.2 起就是如此）。启动时，保存的规则中与旧预设字符串【完全相同】的条目会被替换并写一条 INFO 日志；自定义写法不会被改动，页面会提示两类问题：旧特征，以及“被识别为 Codex 但仍被拦截”。测试：`TestPresetAudit`（每个预设的真实样本、跨家族误匹配、词边界）、`TestCodexDesktopRealUA`、`TestMigrateLegacyPatterns`。公开的 client-guard v0.1.2 有同样的问题，这里没有改它的文件。
- 模块开关：`PUT /suite/modules {id, mode, confirm}` 运行时立即生效，保存在 suite_meta 的 `module_overrides`，扩展库仍为 v3。优先级：config.yaml `modules:` > 页面开关 > 内置默认值。config.yaml 中写了的模块在页面上显示为“锁定”。开启 candy_test 必须带 `confirm: true`。
- 顶部横幅：正常情况下不再显示。版本号和导入记录移到“设置 → 模块 → 关于 / 导入记录”。只有出现问题时才显示横幅（只观察模式、客户端限制暂停或出错），并且可以关闭。
