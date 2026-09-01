package orm

import (
	"database/sql"
	"time"

	"github.com/norm/config"

	goredis "github.com/go-redis/redis/v8"
	_ "github.com/go-sql-driver/mysql"
)

// Pool 持有 MySQL 和 Redis 连接池的全局单例，由 InitPool 初始化。
type Pool struct {
	DB          *sql.DB
	Redis       *goredis.Client
	GlobalDB    *sql.DB
	GlobalRedis *goredis.Client
	Cfg         ORMConfiger
}

var globalPool *Pool

// InitPool 通过配置文件路径初始化全局连接池；应在进程启动时调用一次。
func InitPool(path string) error {
	cfg, err := config.LoadFromFile(path)
	if err != nil {
		// 初始化的任何一步失败，结论都是"池子没起来"，业务只需判这一个类别
		return newError("LoadConfig", "", nil, ErrNotInitialized, err)
	}
	return InitPoolWithConfig(cfg)
}

// InitPoolWithConfig 使用已加载的 *config.ORMConfig 初始化连接池，便于测试注入。
func InitPoolWithConfig(cfg ORMConfiger) error {
	db, err := openMySQL(cfg)
	if err != nil {
		return newError("OpenMySQL", "", nil, ErrNotInitialized, err)
	}
	rdb := openRedis(cfg)

	// 全局库必须连到 global_* 配置指定的地址：
	// 这里若沿用区域 DSN，声明了 Global 的表会被静默写进区域库。
	var globalDB *sql.DB
	if dsn := cfg.GlobalMysqlDSN(); dsn != "" {
		globalDB, err = openMySQLDSN(dsn, cfg)
		if err != nil {
			return newError("OpenGlobalMySQL", "", nil, ErrNotInitialized, err)
		}
	}

	var globalRedis *goredis.Client
	if addr := cfg.GlobalRedisAddr(); addr != "" {
		globalRedis = openRedisAddr(addr, cfg)
	}

	globalPool = &Pool{
		DB:          db,
		Redis:       rdb,
		GlobalDB:    globalDB,
		GlobalRedis: globalRedis,
		Cfg:         cfg,
	}
	return nil
}

// openMySQL 按区域配置建立 MySQL 连接池。
func openMySQL(cfg ORMConfiger) (*sql.DB, error) {
	return openMySQLDSN(cfg.GetMysqlDSN(), cfg)
}

// openMySQLDSN 用指定 DSN 建立连接池。
// 连接数、生命周期等调优参数沿用同一份配置——ORMConfiger 目前只暴露了
// 全局库的 DSN，其余参数无法单独配置。
func openMySQLDSN(dsn string, cfg ORMConfiger) (*sql.DB, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(cfg.MysqlMaxOpenConns())
	db.SetMaxIdleConns(cfg.MysqlMaxIdleConns())
	db.SetConnMaxLifetime(time.Duration(cfg.MysqlConnMaxLifetime()) * time.Second)
	return db, nil
}

// openRedis 按区域配置建立 Redis 客户端。
func openRedis(cfg ORMConfiger) *goredis.Client {
	return openRedisAddr(cfg.GetRedisAddr(), cfg)
}

// openRedisAddr 用指定地址建立 Redis 客户端。
// 密码、DB 编号、连接池参数同样沿用区域配置，原因同 openMySQLDSN。
func openRedisAddr(addr string, cfg ORMConfiger) *goredis.Client {
	return goredis.NewClient(&goredis.Options{
		Addr:         addr,
		Password:     cfg.GetRedisPassword(),
		DB:           cfg.GetRedisDB(),
		PoolSize:     cfg.GetRedisPoolSize(),
		MinIdleConns: cfg.GetRedisMinIdleConns(),
	})
}

func (that *Pool) SelectMySQL(useGlobal bool) *sql.DB {
	if useGlobal && that.GlobalDB != nil {
		return that.GlobalDB
	}
	return that.DB
}

func (that *Pool) SelectRedis(useGlobal bool) *goredis.Client {
	if useGlobal && that.GlobalRedis != nil {
		return that.GlobalRedis
	}
	return that.Redis
}

func (that *Pool) SelectRedisConfig(useGlobal bool) ORMConfiger {
	if useGlobal && that.Cfg != nil {
		return that.Cfg
	}
	return that.Cfg
}

// GetPool 返回全局连接池，未初始化时 panic（开发期快速失败）。
func GetPool() *Pool {
	if globalPool == nil {
		panic("gameorm: pool not initialized, call InitPool first")
	}
	return globalPool
}
