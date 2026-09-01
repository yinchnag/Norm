package orm

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	goredis "github.com/go-redis/redis/v8"
)

// goredisNil 将 goredis.Nil 导出给包内测试使用。
var goredisNil error = goredis.Nil

// errPrefix 是所有框架错误的统一前缀，避免包内出现 "gameorm:"、"[gameorm]"、
// "redisStore." 等多种风格并存。
const errPrefix = "gameorm: "

// 框架错误类别（哨兵错误）。
//
// 它们只用于判定，不携带任何上下文——上下文由 Error 结构体承载。
// 业务侧统一用 errors.Is 判断，不要比较错误字符串：
//
//	if errors.Is(err, orm.ErrNotFound) { ... }
//
// 类别的划分粒度对齐"监控看板上想看到的分桶"，而不是代码里 return err 的处数。
// 新增类别前先问一句：业务或监控会因为它做出不同的动作吗？不会就并入已有类别。
var (
	// ErrNotFound 表示目标数据不存在。
	// 它统一了两种底层表现：Redis 的 key 不存在（goredis.Nil）
	// 与 MySQL 的查询无行（sql.ErrNoRows）。业务层不必再关心命中的是哪一级存储。
	ErrNotFound = errors.New(errPrefix + "not found")

	// ErrNotInitialized 表示连接池尚未初始化，InitPool 没有被调用或调用失败。
	ErrNotInitialized = errors.New(errPrefix + "pool not initialized")

	// ErrStoreStopped 表示存储已关闭，新的写入不会被处理。
	ErrStoreStopped = errors.New(errPrefix + "store stopped")

	// ErrSchema 表示建表/补列/建索引等 DDL 操作失败。
	ErrSchema = errors.New(errPrefix + "schema")

	// ErrCodec 表示序列化、反序列化或 SQL 扫描阶段的数据转换失败。
	ErrCodec = errors.New(errPrefix + "codec")

	// ErrBackend 表示底层 MySQL/Redis 报错（连接、超时、语法等），
	// 是无法归入上述类别时的兜底类别。
	ErrBackend = errors.New(errPrefix + "backend")
)

// allKinds 列出全部类别，供 kindOf 复用已标注的类别。
// 新增哨兵时必须同步登记，否则内层标注的类别会在外层被降级成 ErrBackend。
var allKinds = []error{
	ErrNotFound,
	ErrNotInitialized,
	ErrStoreStopped,
	ErrSchema,
	ErrCodec,
	ErrBackend,
}

// Error 是框架错误的统一载体：类别（Kind）+ 上下文（谁、在哪、干什么）+ 底层错误。
//
// 之所以不为每种失败单独定义类型，是因为它们之间的差异只有 Op/Table/Column
// 三个字符串；真正需要独立类型的是字段结构不同、业务处理方式也不同的场景，
// 目前只有异步存档失败满足，见 ArchiveError。
//
// 取用方式：
//
//	errors.Is(err, orm.ErrNotFound)          // 判类别
//	var e *orm.Error; errors.As(err, &e)     // 取上下文：e.Table / e.PK / e.Op
//	errors.Is(err, sql.ErrNoRows)            // 排障时仍可下探到驱动层
type Error struct {
	Op     string // 操作名，如 "Load" / "LoadR" / "FindAll" / "Migrate"
	Table  string // 表名，无表上下文时为空
	Column string // 列名；索引相关的错误存索引名，无字段上下文时为空
	PK     any    // 主键值，非单行操作为 nil
	Kind   error  // 错误类别，取自上面的哨兵之一
	Err    error  // 底层原始错误，可能为 nil（如 ErrNotInitialized 没有下层错误）
}

// Error 渲染成 "gameorm: <Op> [<表>.<列>:<主键>]: <类别>: <底层错误>"，
// 缺席的部分自动省略，不会留下空的方括号或连续冒号。
func (that *Error) Error() string {
	segs := make([]string, 0, 3)
	if head := strings.TrimSpace(that.Op + " " + that.target()); head != "" {
		segs = append(segs, head)
	}
	if that.Kind != nil {
		segs = append(segs, kindLabel(that.Kind))
	}
	if that.Err != nil {
		segs = append(segs, that.Err.Error())
	}
	if len(segs) == 0 {
		return errPrefix + "unknown error"
	}
	return errPrefix + strings.Join(segs, ": ")
}

// Unwrap 同时向上暴露类别与底层错误（Go 1.20 起支持多错误 unwrap）。
// 这让 errors.Is(err, ErrNotFound) 与 errors.Is(err, sql.ErrNoRows) 同时成立：
// 业务层用前者做分支，排障时用后者定位驱动细节，两者互不牺牲。
func (that *Error) Unwrap() []error {
	switch {
	case that.Kind != nil && that.Err != nil:
		return []error{that.Kind, that.Err}
	case that.Kind != nil:
		return []error{that.Kind}
	case that.Err != nil:
		return []error{that.Err}
	default:
		return nil
	}
}

// target 渲染错误的定位信息："[表.列:主键]"，三者按存在与否裁剪。
func (that *Error) target() string {
	name := that.Table
	if that.Column != "" {
		if name == "" {
			name = that.Column
		} else {
			name += "." + that.Column
		}
	}
	switch {
	case name == "" && that.PK == nil:
		return ""
	case that.PK == nil:
		return "[" + name + "]"
	default:
		return fmt.Sprintf("[%s:%v]", name, that.PK)
	}
}

// kindLabel 取类别的短标签：哨兵自带 "gameorm: " 前缀是为了单独打印时可读，
// 拼进 Error() 时要去掉，否则一条消息里会出现两次前缀。
func kindLabel(kind error) string {
	return strings.TrimPrefix(kind.Error(), errPrefix)
}

// newError 在失败现场构造一条带上下文的框架错误。
// kind 传 nil 表示交由 KindOf 从 err 推断；err 传 nil 表示这是一条没有下层原因的错误。
//
// 下层可能已经返回 *Error 时，用 withContext 而不是这个函数。
func newError(op, table string, pk any, kind, err error) *Error {
	if kind == nil {
		kind = KindOf(err)
	}
	return &Error{Op: op, Table: table, PK: pk, Kind: kind, Err: err}
}

// withContext 给来自下层的错误补上只有外层才知道的上下文，而不是再套一层。
//
// 下层已经构造好 *Error 时（通常带着列名和更精确的类别），这里只填空缺的表名与主键，
// 并把外层的操作名接在前面，形成 "LoadR/Unmarshal"、"AutoMigrate/AddColumn"
// 这样的调用路径；只有下层返回裸错误时才新建一条。
//
// 这条规矩是刻意的：整条链路上只存在一个 *Error 实例，
// 消息里就不会出现两遍 "gameorm:" 前缀和两遍表名——
// 改造前 redis_store 包一次、table_schema 再包一次，正是这个毛病。
func withContext(op, table string, pk any, kind, err error) error {
	if err == nil {
		return nil
	}
	e, ok := err.(*Error)
	if !ok {
		return newError(op, table, pk, kind, err)
	}
	switch {
	case e.Op == "":
		e.Op = op
	case op != "":
		e.Op = op + "/" + e.Op
	}
	if e.Table == "" {
		e.Table = table
	}
	if e.PK == nil {
		e.PK = pk
	}
	if e.Kind == nil {
		e.Kind = kind
	}
	return e
}

// KindOf 判定一个错误属于哪个类别，返回上面的哨兵之一；err 为 nil 时返回 nil。
//
// 已经带有框架类别的错误原样沿用——内层标注过的类别不该在外层被覆盖成兜底值；
// 只有裸的驱动错误才在这里按驱动语义归类。
//
// 导出它是为了监控：按类别分桶打点时直接 switch KindOf(err)，
// 不必再对错误消息做字符串匹配。
func KindOf(err error) error {
	if err == nil {
		return nil
	}
	for _, kind := range allKinds {
		if errors.Is(err, kind) {
			return kind
		}
	}
	if errors.Is(err, goredis.Nil) || errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return ErrBackend
}

// IsNotFound 判断错误是否表示"数据不存在"。
//
// 三种来源都会返回 true：框架的 ErrNotFound、Redis 的 key 不存在、MySQL 的查询无行。
// 保留这个函数而不是让业务直接写 errors.Is，是因为它同时兼容改造前
// 直接返回 goredis.Nil 的旧调用点。
func IsNotFound(err error) bool {
	return errors.Is(err, ErrNotFound) ||
		errors.Is(err, goredis.Nil) ||
		errors.Is(err, sql.ErrNoRows)
}
