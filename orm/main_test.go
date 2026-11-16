// orm/main_test.go
package orm

import (
	"fmt"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// 相对路径以包目录 orm/ 为起点
	if err := InitPool("../example/perf/config/orm.json"); err != nil {
		fmt.Fprintln(os.Stderr, "初始化连接池失败:", err)
		os.Exit(1)
	}

	// InitPool 只创建客户端；Ping 才验证实际连接。
	if err := GetPool().DB.Ping(); err != nil {
		fmt.Fprintln(os.Stderr, "连接 MySQL 失败:", err)
		os.Exit(1)
	}
	if err := GetPool().Redis.Ping(GetPool().Redis.Context()).Err(); err != nil {
		fmt.Fprintln(os.Stderr, "连接 Redis 失败:", err)
		os.Exit(1)
	}

	code := m.Run()

	Shutdown()
	_ = GetPool().Redis.Close()
	_ = GetPool().DB.Close()

	os.Exit(code)
}
