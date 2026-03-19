# GameORM 本地性能压测

本工程用于在本机 MySQL + Redis 环境下快速评估 GameORM 的吞吐和延迟。

## 前置条件

- MySQL 已启动，存在数据库 `test`
- Redis 已启动（无密码）
- 本工程默认配置文件：`example/perf/config/orm.json`

## 运行

在仓库根目录执行：

```powershell
go run ./example/perf \
  -config ./example/perf/config/orm.json \
  -n 20000 \
  -workers 32 \
  -rounds 5 \
  -find-limit 1000 \
  -find-rounds 10 \
  -report-out ./example/perf/reports/my_run.json
```

## 主要参数

- `-n`：样本数量（Save/Load 的对象总数）
- `-workers`：并发 worker 数
- `-flush-wait-ms`：Save 后等待异步刷盘的毫秒数（0 表示自动按配置推导）
- `-find-limit`：FindAll 每轮 limit
- `-find-rounds`：FindAll 重复轮次
- `-cleanup`：每轮开始前是否清空测试数据（默认 true）
- `-rounds`：总轮次，`>1` 时自动进入多轮对比模式
- `-report-out`：报告输出路径；不填时自动生成到 `example/perf/reports/`

默认自动报告文件扩展名为 `.json`。

## 输出说明

程序会输出四段指标：

1. Save（Redis 同步 + MySQL 异步入队）
2. Load（Redis 命中）
3. Load（Redis miss，回源 MySQL）
4. FindAll（带条件和排序）

每段均包含：

- 总耗时
- QPS
- 平均耗时
- p50 / p95 / p99

当 `-rounds > 1` 时，额外输出多轮对比汇总：

- QPS：均值 / 最小 / 最大 / 标准差
- 平均耗时（us）：均值 / 最小 / 最大 / 标准差
- 错误总数

同时会自动生成 JSON 报告，包含：

- 测试参数
- 每轮详细结果
- 多轮对比汇总

## HTML 图表查看

仓库内提供了可视化页面：`example/perf/report_viewer.html`

使用方式：

1. 直接在浏览器打开该 HTML。
2. 点击页面中的“选择 JSON 报告”上传压测结果。
3. 页面会展示多个图表：
  - 各阶段多轮 QPS 折线图
  - 各阶段平均耗时折线图
  - 多轮对比汇总柱状图（QPS）
  - 多轮对比汇总柱状图（Avg us）
  - 各阶段错误占比饼图

## 注意事项

- Save 阶段默认只统计调用开销；真正 MySQL 落盘是异步执行。
- 程序会在 Save 后自动等待至少 3 个刷盘周期，再进入后续阶段。
- 为避免污染业务数据，请使用独立数据库或仅在测试库执行。
