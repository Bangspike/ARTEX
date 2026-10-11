# REA 逆向分析（第一版）

ARTEX 管理任务、模型、会话和报告；REA 提供静态分析工具。每次分析启动一个独立的 stdio MCP 子进程，完成后关闭。无需 HTTP 中转，也不需要另建任务调度服务。

## 使用方式

1. 把待分析文件放入 ARTEX 工作空间。例如 `samples/app.asar`、`samples/extracted-app/` 或 `samples/program`。也可使用任务已上传材料的路径 `tasks/<任务ID>/uploads/...`，以工作空间实际路径为准。
2. 配置并启用名为 `rea` 的 stdio MCP，保留 `逆向分析` Agent 的 `rea-analysis` Skill、`reverse_analysis_context` 和 `complete_reverse_analysis` 工具绑定，并为 Agent 配置可用的 LLM。
3. 打开任务详情的 **逆向分析** 页签，选择 JavaScript/Electron 或原生文件，输入工作空间相对路径与分析目标。
4. 页面显示进度、报告、局限、后续验证建议和可下载的证据 JSON。报告同时作为带来源的 fact 节点写入该任务探索图，通知任务规划器。静态推断不会自动创建已验证漏洞。

Auto、Planner、Worker、Main Agent 也可调用 `start_reverse_analysis` 提交工作，再用 `get_reverse_analysis` 查询。调用返回分析 ID，不等待耗时反编译完成。正在运行的任务不需要改变其原有目标。

## 锁定 REA 源码

本版按用户指定的 `Bangspike/rea` 提交 `603d04ee3ac6ed496f84478164f832a9b5ce3b91` 接入。不要仅按 `6.3.0` 版本号替换成 npm 发布包：验证发现二者的 MCP schema 不同，npm 包缺少本版使用的 `detail: summary` 参数。

在独立目录构建，使用 REA 支持的 Node（本地验证使用 Node 24.19.0）：

```sh
git clone https://github.com/Bangspike/rea.git rea
cd rea
git checkout 603d04ee3ac6ed496f84478164f832a9b5ce3b91
npm ci
npm run build:cached
```

MCP 设置示例（替换成实际绝对路径，不要使用下载时的临时目录作为长期配置）：

```json
{
  "name": "rea",
  "transport": "stdio",
  "command": "/absolute/path/to/node",
  "args": ["/absolute/path/to/rea/scripts/rea.mjs", "mcp"],
  "env": {
    "REA_ANALYSIS_PROVIDER": "ghidra",
    "GHIDRA_INSTALL_DIR": "/absolute/path/to/ghidra_12.1.4_PUBLIC",
    "JAVA_HOME": "/absolute/path/to/jdk-21"
  },
  "enabled": true
}
```

JS/Electron 静态分析不需要 Java/Ghidra，测试该流程时可以省略 `env` 中的 Ghidra 配置。原生分析需要 Ghidra 12.1.4 和 JDK 21，首次启动耗时较长；REA 默认启动超时为 330 秒，应给 Agent 留出更长的运行预算。确认 Ghidra 平台对应的本地反编译器可用。

macOS ARM 本地验证时，官方 Ghidra 12.1.4 包未附带对应反编译器。本次使用包内 Makefile 编译，并覆盖其原有的 x86 编译目标：

```sh
cd "$GHIDRA_INSTALL_DIR/Ghidra/Features/Decompiler/src/decompile/cpp"
make -j4 ghidra_opt ARCH_TYPE='-arch arm64' ADDITIONAL_FLAGS='-mmacosx-version-min=11.0 -w'
mkdir -p ../../../os/mac_arm_64
install -m 755 ghidra_opt ../../../os/mac_arm_64/decompile
```

此步骤仅适用于 macOS ARM，Linux 服务器应使用对应 Linux 原生组件。

新安装只预置 **禁用** 的 `rea` MCP 配置，不自动下载、启动引擎或覆盖现有 MCP/Agent 设置。修改已有 Agent 的绑定后，启动不会覆盖你的配置。

## 数据与执行边界

- 输入先复制为任务专属快照并计算 SHA-256；目录按相对文件名与各文件 SHA-256 生成清单摘要。限制为 512 MiB / 20,000 文件，不接受绝对路径、越界路径、符号链接或包含输出目录的输入。
- 分析 Agent 默认只使用所需的 REA 工具与报告工具，关闭通用 shell、文件读写、WebFetch 和 WebSearch。只允许打开绑定快照；原生分析强制选择 Ghidra；导出路径固定在本次分析目录。
- 每次 MCP 原始结果先写文件再写证据索引。JS summary 仅缩减模型看到的内容，完整分析保存在 REA session，并通过 `export_evidence_bundle` 导出。ARTEX 下载前验证证据哈希。
- 完整报告要求成功的目标分析和完整证据包；部分完成必须明确局限。回写目标由会话绑定，拒绝外部任务的证据 ID 和重复封存。
- 第一版单个 ARTEX 实例同时运行一个逆向分析。不提供跨实例调度或进程隔离沙箱；解析器与 ARTEX 子进程运行在同一系统用户权限下。
- 停止会取消工具调用、关闭 stdio 并等待退出。删除/归档任务也会等待该会话结束。启动时将遗留的未完成分析标记为已停止，保留证据，不自动重试。
- 报告、证据索引和任务目录随任务冷归档保存并恢复；独立聊天历史不属于归档，恢复的报告不依赖旧会话 ID。

数据库增加 `reverse_analyses`、`reverse_analysis_evidence`。文件存储在 `tasks/<taskID>/reverse/<analysisID>/`。接口均复用 ARTEX 现有登录认证：

| 接口 | 作用 |
|---|---|
| `POST /api/tasks/{id}/reverse-analyses` | 提交 `{kind,input_path,objective}`，返回 202 |
| `GET /api/tasks/{id}/reverse-analyses` | 列表及状态 |
| `GET /api/tasks/{id}/reverse-analyses/{analysis}` | 报告和证据索引 |
| `POST /api/tasks/{id}/reverse-analyses/{analysis}/stop` | 取消当前分析 |
| `GET /api/tasks/{id}/reverse-analyses/{analysis}/evidence/{evidence}` | 下载经哈希校验的 JSON |

## 本地验证

使用独立的本机 PostgreSQL 测试库；不要复用服务器数据库、运行目录或监听端口。测试库名必须含 `test`，REA 测试拒绝远程数据库。测试会写入并清理测试任务。

```sh
export ARTEX_PG_DSN='postgres://TEST_USER@127.0.0.1:TEST_PORT/artex_rea_test?sslmode=disable'
export ARTEX_REA_COMMAND='/absolute/path/to/node'
export ARTEX_REA_SCRIPT='/absolute/path/to/rea/scripts/rea.mjs'
export ARTEX_REA_GHIDRA_DIR='/absolute/path/to/ghidra_12.1.4_PUBLIC'
export ARTEX_REA_JAVA_HOME='/absolute/path/to/jdk-21'
# 可选：保存实际分析报告与完整证据包到一个独立目录
export ARTEX_REA_TEST_OUTPUT='/absolute/path/to/local-test-output'
go test ./server -run '^TestReverse' -count=1 -v
go test ./mcpstdio ./reanalysis
```

`TestReverseRealREA` 使用真实 REA 引擎；缺少引擎环境变量时明确跳过对应测试。`TestReverseConversationRealREA` 使用真实 MCP、Skill 解锁、HTTP 提交、报告工具和 PostgreSQL，以确定性的模型替身验证编排及停止，不调用外部 LLM。该测试不代表实际模型对任意目标的分析质量已经验收。

原生测试仅编译测试样本供 Ghidra 静态分析，不运行样本。正式接入前仍需用实际业务样本和所选模型验证报告质量、分析耗时与内存预算。

## 本次本地验证记录（2026-10-11）

ARTEX 基于 `e6ec56999175509551a1e43a3ee3b053d0941bd9`，改动位于独立分支 `codex/rea-analysis`。REA 使用上文锁定提交；环境为 macOS ARM、Node 24.19.0、PostgreSQL 18.4、Ghidra 12.1.4、Temurin JDK 21.0.12.1。数据库与运行目录均为本次独立创建的本机测试资源。

| 验证 | 结果 |
|---|---|
| 真实 JS/Electron 解析 | 识别测试应用的 `read-config` IPC，保存完整分析证据包并写回任务 |
| 真实 Ghidra 分析 | 导入本地 Mach-O ARM64 样本，搜索 `_artex_check`，反编译出 `return param_1 == 0x1092;`，检索样本字符串，导出证据并回写任务 |
| ARTEX 完整会话 | HTTP 提交 → Skill 解锁 → 真实 MCP 分析 → 报告工具 → 任务事实节点；模型使用确定性测试替身 |
| 生命周期与证据 | 停止、重复提交、跨任务证据拒绝、重复报告拒绝、过期证据包拒绝、下载哈希校验、重启恢复、任务归档恢复 |
| 并发隔离 | 删除任务仅取消自己的 REA 会话，等待清理，不取消其他任务；新增流程通过竞态检查 |
| 前端 | 新页签通过 TypeScript 检查、Biome 检查与 Next.js 静态构建 |
| 回归 | `server`、`db`、`mcpstdio`、`reanalysis` 测试通过；Go vet 通过 |

完整 `agent` 测试存在一个基线失败：`TestGraphOverviewExpandsAssociatedCompanyScope` 的 `task scope missing`。在未修改的主工作树和独立测试库中也复现，未在本次功能中修改。

本次未部署、连接或重启服务器。真实 LLM 对实际业务样本的报告质量仍需后续验收。
