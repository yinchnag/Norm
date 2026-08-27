package orm

import (
	"testing"

	"github.com/norm/config"
)

func TestInitPoolWithConfig_BadDSN(t *testing.T) {
	// sql.Open 对 mysql driver 是懒连接，不会立刻报错；但不应 panic
	cfg := config.DefaultORMConfig()
	cfg.MySQL.DSN = "root:@tcp(127.0.0.1:63306)/test"
	cfg.Redis.Addr = "127.0.0.1:63379"
	if err := InitPoolWithConfig(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	p := GetPool()
	if p == nil {
		t.Error("pool should not be nil")
	}
}

// TestInitPoolWithConfig_GlobalUsesGlobalDSN 全局库必须连到 global_mysql 配置的地址。
// 用一个格式非法的全局 DSN 探测：sql.Open 会在解析阶段报错，
// 只有真的把 GlobalMysqlDSN 传下去了才会失败。
// 旧实现复用了区域 DSN，这里会静默成功，声明 Global 的表全部写错库。
func TestInitPoolWithConfig_GlobalUsesGlobalDSN(t *testing.T) {
	cfg := config.DefaultORMConfig()
	cfg.MySQL.DSN = "root:@tcp(127.0.0.1:63306)/test"
	cfg.Redis.Addr = "127.0.0.1:63379"
	cfg.GlobalMySQL = &config.DBConfig{DSN: "这不是一个合法的 DSN"}

	if err := InitPoolWithConfig(cfg); err == nil {
		t.Fatal("全局 DSN 非法时应返回错误，说明它根本没被使用")
	}
}

// TestInitPoolWithConfig_GlobalRedisUsesGlobalAddr 全局 Redis 同理。
func TestInitPoolWithConfig_GlobalRedisUsesGlobalAddr(t *testing.T) {
	cfg := config.DefaultORMConfig()
	cfg.MySQL.DSN = "root:@tcp(127.0.0.1:63306)/test"
	cfg.Redis.Addr = "127.0.0.1:63379"
	cfg.GlobalRedis = &config.RedisConfig{Addr: "127.0.0.1:63380"}

	if err := InitPoolWithConfig(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	p := GetPool()
	if p.GlobalRedis == nil {
		t.Fatal("GlobalRedis 应已初始化")
	}
	if got := p.GlobalRedis.Options().Addr; got != "127.0.0.1:63380" {
		t.Fatalf("全局 Redis 连到了 %s，期望 127.0.0.1:63380", got)
	}
}
