package orm

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestErrorIsKindAndCause 断言多错误 unwrap 生效：
// 同一条错误既能按框架类别判定，也能下探到驱动层的原始错误。
func TestErrorIsKindAndCause(t *testing.T) {
	err := newError("Load", "player", int64(1001), nil, sql.ErrNoRows)

	if !errors.Is(err, ErrNotFound) {
		t.Error("应能判定为 ErrNotFound")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		t.Error("应能下探到 sql.ErrNoRows")
	}
	if errors.Is(err, ErrBackend) {
		t.Error("不该同时命中 ErrBackend")
	}
}

// TestErrorAsContext 断言上下文可以被完整取出，业务不必再从消息里抠表名和主键。
func TestErrorAsContext(t *testing.T) {
	wrapped := fmt.Errorf("上层再包一层: %w", newError("LoadR", "player", int64(7), ErrNotFound, goredisNil))

	var e *Error
	if !errors.As(wrapped, &e) {
		t.Fatal("errors.As 应能取出 *Error")
	}
	if e.Op != "LoadR" || e.Table != "player" || e.PK != int64(7) {
		t.Fatalf("上下文丢失: %+v", e)
	}
	if !errors.Is(wrapped, ErrNotFound) {
		t.Error("包了一层后仍应能判定类别")
	}
}

// TestIsNotFoundSources 覆盖"数据不存在"的三种来源，
// 这正是改造前 Redis 与 MySQL 判定不一致的地方。
func TestIsNotFoundSources(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"哨兵本身", ErrNotFound, true},
		{"Redis key 不存在", goredisNil, true},
		{"MySQL 查询无行", sql.ErrNoRows, true},
		{"包装后的 Redis 未命中", newError("LoadR", "player", int64(1), nil, goredisNil), true},
		{"包装后的 MySQL 未命中", newError("Load", "player", int64(1), nil, sql.ErrNoRows), true},
		{"后端故障", newError("Load", "player", int64(1), nil, errors.New("connection refused")), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := IsNotFound(c.err); got != c.want {
			t.Errorf("%s: IsNotFound=%v want=%v", c.name, got, c.want)
		}
	}
}

// TestKindOfPreservesAnnotatedKind 断言内层已标注的类别不会在外层被降级成兜底值，
// 这是内层逐步替换 fmt.Errorf 时能保持类别准确的前提。
func TestKindOfPreservesAnnotatedKind(t *testing.T) {
	inner := fmt.Errorf("field=tags unmarshal: %w", ErrCodec)
	if got := KindOf(inner); got != ErrCodec {
		t.Errorf("kindOf=%v want=ErrCodec", got)
	}
	if got := KindOf(errors.New("Error 1146: table doesn't exist")); got != ErrBackend {
		t.Errorf("未标注的错误应归入 ErrBackend，实际 %v", got)
	}
	if got := KindOf(nil); got != nil {
		t.Errorf("KindOf(nil) 应为 nil，实际 %v", got)
	}
}

// TestErrorMessage 断言渲染结果不会出现重复前缀、空方括号或连续冒号。
func TestErrorMessage(t *testing.T) {
	cases := []struct {
		err  *Error
		want string
	}{
		{
			&Error{Op: "Load", Table: "player", PK: int64(1001), Kind: ErrNotFound, Err: sql.ErrNoRows},
			"gameorm: Load [player:1001]: not found: sql: no rows in result set",
		},
		{
			&Error{Op: "Migrate", Table: "player", Kind: ErrSchema, Err: errors.New("Error 1064")},
			"gameorm: Migrate [player]: schema: Error 1064",
		},
		{
			&Error{Op: "Save", Table: "player", Column: "tags", Kind: ErrCodec},
			"gameorm: Save [player.tags]: codec",
		},
		{
			&Error{Op: "Load", Table: "player", PK: int64(1), Kind: ErrNotInitialized},
			"gameorm: Load [player:1]: pool not initialized",
		},
		{
			&Error{},
			"gameorm: unknown error",
		},
	}
	for _, c := range cases {
		if got := c.err.Error(); got != c.want {
			t.Errorf("渲染不符:\n got=%q\nwant=%q", got, c.want)
		}
	}
}

// TestErrorUnwrapNoNilElement 断言 Unwrap 不返回 nil 元素——
// errors.Is 的遍历约定要求这一点。
func TestErrorUnwrapNoNilElement(t *testing.T) {
	for _, e := range []*Error{
		{Kind: ErrNotFound},
		{Err: sql.ErrNoRows},
		{Kind: ErrNotFound, Err: sql.ErrNoRows},
		{},
	} {
		for i, sub := range e.Unwrap() {
			if sub == nil {
				t.Errorf("%+v 的 Unwrap 第 %d 个元素为 nil", e, i)
			}
		}
	}
}

// TestWithContextMergesInsteadOfWrapping 是"只包一层"这条规矩的回归测试：
// 外层补上下文时不应新建 *Error，否则消息里会出现两遍前缀和两遍表名。
func TestWithContextMergesInsteadOfWrapping(t *testing.T) {
	inner := &Error{Op: "Unmarshal", Column: "tags", Kind: ErrCodec, Err: errors.New("invalid char")}
	out := withContext("LoadR", "player", int64(1001), nil, inner)

	if out != error(inner) {
		t.Fatal("应复用下层的 *Error 实例，而不是再套一层")
	}
	if inner.Op != "LoadR/Unmarshal" {
		t.Errorf("操作路径应拼接，实际 %q", inner.Op)
	}
	if inner.Table != "player" || inner.PK != int64(1001) {
		t.Errorf("外层上下文没补上: %+v", inner)
	}
	if inner.Column != "tags" || inner.Kind != ErrCodec {
		t.Errorf("下层信息被覆盖了: %+v", inner)
	}

	msg := out.Error()
	if n := strings.Count(msg, errPrefix); n != 1 {
		t.Errorf("消息里出现 %d 次前缀: %s", n, msg)
	}
	if n := strings.Count(msg, "player"); n != 1 {
		t.Errorf("消息里出现 %d 次表名: %s", n, msg)
	}
	if want := "gameorm: LoadR/Unmarshal [player.tags:1001]: codec: invalid char"; msg != want {
		t.Errorf("渲染不符:\n got=%q\nwant=%q", msg, want)
	}
}

// TestWithContextOnRawError 断言下层返回裸错误时才新建一条，并推断类别。
func TestWithContextOnRawError(t *testing.T) {
	out := withContext("Load", "player", int64(2), nil, sql.ErrNoRows)

	var e *Error
	if !errors.As(out, &e) {
		t.Fatal("裸错误应被包装成 *Error")
	}
	if e.Op != "Load" || e.Table != "player" || e.Kind != ErrNotFound {
		t.Errorf("包装结果不符: %+v", e)
	}
	if withContext("Load", "player", nil, nil, nil) != nil {
		t.Error("nil 错误应原样返回 nil")
	}
}

// TestWithContextFillsMissingKind 断言下层没标类别时用外层给的兜底类别，
// 标了就不覆盖——这是 AutoMigrate 统一归入 ErrSchema 的依据。
func TestWithContextFillsMissingKind(t *testing.T) {
	bare := &Error{Op: "AddColumn", Column: "level", Err: errors.New("Error 1064")}
	if withContext("AutoMigrate", "player", nil, ErrSchema, bare); bare.Kind != ErrSchema {
		t.Errorf("缺失的类别应由外层补上，实际 %v", bare.Kind)
	}

	typed := &Error{Op: "Unmarshal", Kind: ErrCodec, Err: errors.New("boom")}
	if withContext("AutoMigrate", "player", nil, ErrSchema, typed); typed.Kind != ErrCodec {
		t.Errorf("下层已标注的类别不该被覆盖，实际 %v", typed.Kind)
	}
}

// TestShutdownArchiveErrorIsStoreStopped 断言关服未落库的存档可以被业务判定，
// 而不是只能靠回调里比对消息文本。
func TestShutdownArchiveErrorIsStoreStopped(t *testing.T) {
	if !errors.Is(errNotFlushed, ErrStoreStopped) {
		t.Error("关服丢档应归入 ErrStoreStopped")
	}
	if KindOf(errNotFlushed) != ErrStoreStopped {
		t.Errorf("KindOf 分桶不符: %v", KindOf(errNotFlushed))
	}
}
