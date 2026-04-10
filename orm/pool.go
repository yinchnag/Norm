package orm

import (
	"database/sql"
	"fmt"
	"time"

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
func InitPool(cfg ORMConfiger) error {
	return InitPoolWithConfig(cfg)
}

// InitPoolWithConfig 使用已加载的 *config.ORMConfig 初始化连接池，便于测试注入。
func InitPoolWithConfig(cfg ORMConfiger) error {
	db, err := openMySQL(cfg)
	if err != nil {
		return fmt.Errorf("gameorm: open mysql: %w", err)
	}
	rdb := openRedis(cfg)

	var globalDB *sql.DB
	if cfg.GlobalMysqlDSN() != "" {
		globalDB, err = openMySQL(cfg)
		if err != nil {
			return fmt.Errorf("gameorm: open global mysql: %w", err)
		}
	}

	var globalRedis *goredis.Client
	if cfg.GlobalRedisAddr() != "" {
		globalRedis = openRedis(cfg)
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

func openMySQL(cfg ORMConfiger) (*sql.DB, error) {
	db, err := sql.Open("mysql", cfg.GetMysqlDSN())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(cfg.MysqlMaxOpenConns())
	db.SetMaxIdleConns(cfg.MysqlMaxIdleConns())
	db.SetConnMaxLifetime(time.Duration(cfg.MysqlConnMaxLifetime()) * time.Second)
	return db, nil
}

func openRedis(cfg ORMConfiger) *goredis.Client {
	return goredis.NewClient(&goredis.Options{
		Addr:         cfg.GetRedisAddr(),
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
