package orm

import (
	"errors"
	"fmt"
	"sync/atomic"
)

// ArchiveError 描述一次异步存档失败。
// 异步刷盘发生在业务调用栈之外，Save/Delete 无法把错误返回给调用方，
// 因此框架通过这个结构把失败暴露出来，由业务决定如何告警或补偿。
type ArchiveError struct {
	Table   string // 表名
	PK      any    // 主键值
	Deleted bool   // true 表示这是一条软删除请求
	Attempt int    // 已尝试的刷盘次数，从 1 开始
	Dropped bool   // true 表示这条存档已被放弃，数据不会再落库
	Err     error  // 底层错误
}

func (that ArchiveError) Error() string {
	action := "upsert"
	if that.Deleted {
		action = "softDelete"
	}
	state := "will retry"
	if that.Dropped {
		state = "DROPPED"
	}
	return fmt.Sprintf("%s [%s:%v] attempt=%d %s: %v",
		action, that.Table, that.PK, that.Attempt, state, that.Err)
}

func (that ArchiveError) Unwrap() error { return that.Err }

// archiveErrorHook 保存业务注册的失败回调。
// 用原子指针而非普通全局变量，避免与刷盘 worker 的读取产生竞争。
var archiveErrorHook atomic.Pointer[func(ArchiveError)]

// SetArchiveErrorHandler 注册存档失败回调，传 nil 恢复默认的标准输出打印。
// 可在任意时刻调用。回调在刷盘 worker 上同步执行，务必保持轻量，
// 不要在其中做阻塞操作，否则会拖慢整条刷盘链路。
func SetArchiveErrorHandler(fn func(ArchiveError)) {
	if fn == nil {
		archiveErrorHook.Store(nil)
		return
	}
	archiveErrorHook.Store(&fn)
}

// errStoreStopped 表示存储已关闭，新的写入请求不会被处理。
var errStoreStopped = errors.New("mysql store already stopped")

// errNotFlushed 表示进程退出时该条存档仍未落库。
var errNotFlushed = errors.New("not flushed before shutdown")

// reportArchiveError 把一次存档失败交给业务回调。
// 回调 panic 不会影响刷盘 worker——存档链路不能因为告警代码出错而中断。
func reportArchiveError(ev ArchiveError) {
	hook := archiveErrorHook.Load()
	if hook == nil {
		fmt.Printf("[gameorm] %s\n", ev.Error())
		return
	}
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("[gameorm] OnArchiveError panic: %v (原始错误: %s)\n", r, ev.Error())
		}
	}()
	(*hook)(ev)
}
