# Norm (GameORM)

面向 Go 游戏服务器的高性能 ORM 框架。

核心设计：

- **不阻塞游戏逻辑** —— MySQL 写走异步 worker 队列，只有 Redis 写是同步的
- **零 reflect 字段读写** —— 字段访问通过 `unsafe.Pointer` + 偏移量完成，不走 `reflect.Value.Set`
- **类型安全的基类** —— CRTP 模式（`TableSchema[T]`），业务结构体嵌入即可获得全部能力
- **全局软删除** —— 不做物理 DELETE，查询自动过滤 `is_deleted=0`
- **可判定的错误** —— 所有对外错误带类别与上下文，见[错误处理](#错误处理)

## 快速开始

### 1. 配置

`config/orm.json`：

```json
{
  "mysql": {
    "dsn": "root:pass@tcp(127.0.0.1:3306)/test?charset=utf8mb4&parseTime=True&loc=Local",
    "max_open_conns": 100,
    "max_idle_conns": 20,
    "conn_max_lifetime": 3600
  },
  "redis": {
    "addr": "127.0.0.1:6379",
    "password": "",
    "db": 0,
    "pool_size": 50,
    "min_idle_conns": 10,
    "key_ttl_sec": 7200
  },
  "flush_interval_ms": 500,
  "worker_count": 4
}
```

`global_mysql` / `global_redis` 为可选项，用法见[全局库](#全局库)。

### 2. 定义结构体

```go
type TestUser struct {
    orm.TableSchema[*TestUser]
    UserId   int64     `orm:"primary,name:user_id,comment:用户ID,autoInc"`
    UserName string    `orm:"name:user_name,comment:用户名,length:100,notNull"`
    Age      int       `orm:"name:age,comment:年龄"`
    Score    float64   `orm:"name:score,index:idx_score"`
    BuyInfo  *ShopInfo `orm:"name:buy_info,comment:商店购买数据"`
}
```

嵌入的类型参数必须是宿主结构体的指针类型（`*TestUser`）。map / slice / struct 等复杂字段自动序列化为 JSON 列。

### 3. 读写

```go
if err := orm.InitPool("config/orm.json"); err != nil {
    log.Fatal(err)
}
defer orm.Shutdown() // 优雅退出：确保异步写全部刷盘

user := &TestUser{UserId: 5001, UserName: "alice", Age: 30}
user.Init()   // 必须最先调用：绑定宿主指针 + 触发 AutoMigrate

user.Save()   // 同步写 Redis + 异步写 MySQL
user.Delete() // 软删除

if err := user.Load(); err != nil {   // Redis 命中则直接返回，未命中降级查 MySQL
    if orm.IsNotFound(err) {
        // 新号，走创建流程
    }
}

users, err := user.FindAll("age > 28", "age DESC", 100) // 自动过滤软删除记录
```

## 数据流

```
Save()    -> Redis Hash（同步） + MySQL 异步队列（EnqueueSave）
Load()    -> Redis 优先 -> 未命中查 MySQL -> 回写 Redis
LoadR()   -> 只查 Redis，不降级
Delete()  -> Redis 删除 + MySQL 异步软删除（is_deleted=1）
FindAll() -> MySQL SELECT，自动追加 is_deleted=0
```

对象按 `hash(pk) % worker_count` 路由到固定 worker，保证同一主键的写入有序。每个 worker 持有去重表 —— 同一个 key 的多次 `Save` 合并成一次 MySQL 写，每 `flush_interval_ms` 用 `INSERT ... ON DUPLICATE KEY UPDATE` 批量落库。

## 结构体 tag

| tag | 说明 |
|---|---|
| `primary` | 主键 |
| `autoInc` | 自增 |
| `notNull` | NOT NULL |
| `name:xxx` | 列名 |
| `comment:xxx` | 列注释 |
| `length:100` | 字符串列长度 |
| `index` | 为该列建索引（名字自动生成） |
| `index:idx_a_b` | 加入指定名字的索引，同名多列组成联合索引 |

### 系统自动列

每张表自动带上 `is_deleted`、`create_time`、`update_time` 三列，以及 `idx_is_deleted`、`idx_update_time` 两个索引，无需在结构体里声明。

`Init()` 会执行 AutoMigrate：`CREATE TABLE IF NOT EXISTS` + 补齐缺失的列和索引。**只加不减** —— 不会删除旧列，也不会修改已有列的类型。

## 错误处理

框架对外返回的错误都是可判定的：**用 `errors.Is` 判类别，用 `errors.As` 取上下文**，不要匹配错误消息文本。

### 错误类别

| 哨兵 | 含义 |
|---|---|
| `orm.ErrNotFound` | 数据不存在（统一了 Redis key 缺失与 MySQL 查询无行两种底层表现） |
| `orm.ErrNotInitialized` | 连接池没起来：`InitPool` 未调用或调用失败 |
| `orm.ErrStoreStopped` | 存储已关闭，写入不再被处理（含关服时未落库的存档） |
| `orm.ErrSchema` | 建表 / 补列 / 建索引失败 |
| `orm.ErrCodec` | 序列化、反序列化或 SQL 扫描阶段的数据转换失败 |
| `orm.ErrBackend` | 底层 MySQL/Redis 报错（连接、超时、语法等），兜底类别 |

类别的划分粒度对齐"监控看板上想看到的分桶"，不是代码里 `return err` 的处数。

### 判定与取用

```go
err := user.Load()

// 判类别
if orm.IsNotFound(err) { ... }              // 等价于 errors.Is(err, orm.ErrNotFound)
if errors.Is(err, orm.ErrBackend) { ... }

// 取上下文：表名、主键、列名、操作路径
var e *orm.Error
if errors.As(err, &e) {
    log.Printf("表=%s 主键=%v 操作=%s", e.Table, e.PK, e.Op)
}

// 排障时仍可下探到驱动层
if errors.Is(err, sql.ErrNoRows) { ... }
```

第三条之所以成立，是因为 `orm.Error` 实现了 Go 1.20 的多错误 `Unwrap() []error`，同时向上暴露类别和底层错误 —— 业务判类别与排障看驱动细节互不牺牲。

监控打点用 `orm.KindOf(err)` 直接拿到分桶：

```go
switch orm.KindOf(err) {
case orm.ErrNotFound: metrics.Inc("not_found")
case orm.ErrBackend:  metrics.Inc("backend")
...
}
```

### 错误消息形态

```
gameorm: Load [player:1001]: not found: sql: no rows in result set
gameorm: LoadR/Unmarshal [player.tags:1001]: codec: invalid char
gameorm: AutoMigrate/AddColumn [player.level]: schema: Error 1064 ...
gameorm: OpenMySQL: pool not initialized: dial tcp 127.0.0.1:3306: connect: refused
```

格式为 `gameorm: <操作路径> [<表>.<列>:<主键>]: <类别>: <底层错误>`，缺席的部分自动省略。

**一条链路上只存在一个 `*Error` 实例**：下层在失败现场标注类别和列名，上层用 `withContext` 补表名、主键，并把操作名接成 `LoadR/Unmarshal` 这样的路径，而不是再包一层。这条规矩是为了避免消息里出现两遍 `gameorm:` 前缀和两遍表名 —— 新增内层错误时请遵守。

### 异步存档失败

`Save` / `Delete` 是异步的，错误无法从调用栈返回。它们的失败通过 `ArchiveError` 回调暴露：

```go
orm.SetArchiveErrorHandler(func(ev orm.ArchiveError) {
    // ev.Dropped 为 true 表示重试已用尽，这份数据在系统里只剩这一条日志
    log.Printf("[存档失败] %s", ev.Error())
    alert.Send(ev.PayloadJSON()) // 完整存档内容，可据此重建 INSERT
})
```

刷盘失败会自动重试，超过上限才标记 `Dropped`。回调在刷盘 worker 上同步执行，务必保持轻量。

### panic 的边界

以下属于**程序员错误**，框架直接 panic 而不返回 error，目的是开发期快速失败：

- `Init()` 时 AutoMigrate 失败（避免无表静默运行）
- 未调用 `Init()` 就执行任何 ORM 操作
- `TableSchema[T]` 的 T 不是结构体指针类型

运行时错误一律返回 error，不 panic。

## 全局库

结构体里声明一个 `Global bool` 字段，该表的读写就路由到 `global_mysql` / `global_redis` 配置的地址（跨区共享数据用）。未配置全局库时自动回落到区域库。

## 开发

```bash
go test ./...                    # 全部测试
go test ./orm/... -run TestXxx   # 单个测试
go build ./example/...           # 构建示例

# 压测（详见 example/perf/README.md）
go run ./example/perf -config ./example/perf/config/orm.json -n 20000 -workers 32 -rounds 5
```

工程规范见 [CLAUDE.md](CLAUDE.md) 与 [.github/copilot-instructions.md](.github/copilot-instructions.md)。

## 技术栈

Go 1.24、`go-sql-driver/mysql`、`go-redis/redis/v8`、`bytedance/sonic`。不依赖 gorm 等第三方 ORM。
