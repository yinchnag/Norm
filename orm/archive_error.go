package orm

import (
	"fmt"
	"sync/atomic"

	"github.com/bytedance/sonic"
)

// ArchiveError 描述一次异步存档失败。
// 异步刷盘发生在业务调用栈之外，Save/Delete 无法把错误返回给调用方，
// 因此框架通过这个结构把失败暴露出来，由业务决定如何告警或补偿。
//
// Columns 带着这条存档的完整内容：重试次数用尽后数据只剩日志里这一份，
// 必须保证光凭日志就能把它重新写回数据库。
type ArchiveError struct {
	Table   string         // 表名
	PK      any            // 主键值
	Deleted bool           // true 表示这是一条软删除请求
	Attempt int            // 已尝试的刷盘次数，从 1 开始；0 表示根本没来得及尝试
	Dropped bool           // true 表示这条存档已被放弃，数据不会再落库
	Columns map[string]any // 列名 -> 值，值即当初要绑定给 SQL 的参数
	Err     error          // 底层错误
}

// PayloadJSON 把存档内容渲染成 JSON 文本。
// 字段值就是当初要绑定给 SQL 的参数：复杂字段是已经编码好的 JSON 字符串，
// 基本类型是原生值。日志里留下这串文本即可直接重建 INSERT 语句，
// 不需要知道对应的 Go 结构体定义。
func (that ArchiveError) PayloadJSON() string {
	if len(that.Columns) == 0 {
		return "{}"
	}
	s, err := sonic.MarshalString(that.Columns)
	if err != nil {
		// 存档内容已经丢了一次，不能因为渲染失败再丢一次
		return fmt.Sprintf("%v", that.Columns)
	}
	return s
}

func (that ArchiveError) Error() string {
	action := "upsert"
	if that.Deleted {
		action = "softDelete"
	}

	var state string
	switch {
	case that.Attempt == 0:
		state = "not attempted"
	case that.Dropped:
		state = fmt.Sprintf("attempt=%d/%d GIVE UP", that.Attempt, maxFlushRetries+1)
	default:
		state = fmt.Sprintf("attempt=%d/%d will retry", that.Attempt, maxFlushRetries+1)
	}

	return fmt.Sprintf("%s [%s:%v] %s: %v | data=%s",
		action, that.Table, that.PK, state, that.Err, that.PayloadJSON())
}

func (that ArchiveError) Unwrap() error { return that.Err }

// archiveErrorHook 保存业务注册的失败回调。
// 用原子指针而非普通全局变量，避免与刷盘 worker 的读取产生竞争。
var archiveErrorHook atomic.Pointer[func(ArchiveError)]

// SetArchiveErrorHandler 注册存档失败回调，传 nil 恢复默认的标准输出打印。
// 可在任意时刻调用。回调在刷盘 worker 上同步执行，务必保持轻量，
// 不要在其中做阻塞操作，否则会拖慢整条刷盘链路。
//
// 自定义回调务必把 ev.Error() 或 ev.PayloadJSON() 写进日志：
// Dropped 为 true 时，这份数据在系统里已经不存在第二份了。
func SetArchiveErrorHandler(fn func(ArchiveError)) {
	if fn == nil {
		archiveErrorHook.Store(nil)
		return
	}
	archiveErrorHook.Store(&fn)
}

// errNotFlushed 表示进程退出时该条存档仍未落库。
// 挂在 ErrStoreStopped 之下，业务回调里可用 errors.Is 把"存储已停止导致的丢档"
// 与"MySQL 拒绝了这次写入"区分开。
var errNotFlushed = fmt.Errorf("%w: not flushed before shutdown", ErrStoreStopped)

// reportArchiveError 把一次存档失败交给业务回调。
// 回调 panic 不会影响刷盘 worker——存档链路不能因为告警代码出错而中断。
func reportArchiveError(ev ArchiveError) {
	hook := archiveErrorHook.Load()
	if hook == nil {
		// 还能重试的记 WARN，已经放弃的记 ERROR
		level := "WARN"
		if ev.Dropped {
			level = "ERROR"
		}
		fmt.Printf("[gameorm][%s] %s\n", level, ev.Error())
		return
	}
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("[gameorm][ERROR] OnArchiveError panic: %v (原始错误: %s)\n", r, ev.Error())
		}
	}()
	(*hook)(ev)
}
