package orm

// mysql配置
type MysqlConfiger interface {
	GetMysqlDSN() string       // 获得 MySQL 数据源名称（DSN），例如 "user:pass@tcp(host:port)/dbname?params"
	MysqlMaxOpenConns() int    // 获得 MySQL 连接池最大打开连接数
	MysqlMaxIdleConns() int    // 获得 MySQL 连接池最大空闲连接数
	MysqlConnMaxLifetime() int // 获得 MySQL 连接最大存活时间（秒）
}

// redis配置
type RedisConfiger interface {
	GetRedisAddr() string      // 获得 Redis 地址，例如 "host:port"
	GetRedisPassword() string  // 获得 Redis 密码，若无密码则返回空字符串
	GetRedisDB() int           // 获得 Redis 数据库编号，默认 0
	GetRedisPoolSize() int     // 获得 Redis 连接池大小
	GetRedisMinIdleConns() int // 获得 Redis 最小空闲连接数
	GetRedisKeyTTLSec() int    // 获得 Redis key 默认 TTL（秒）
}

// 全局配置，包含 MySQL 和 Redis 的 DSN/地址，供 MySQLStore 和 RedisStore 初始化连接池时使用。
type GlobalConfiger interface {
	GlobalMysqlDSN() string  // 获得全局 MySQL 数据源名称（DSN），若未配置则返回空字符串
	GlobalRedisAddr() string // 获得全局 Redis 地址，例如 "host:port"，若未配置则返回空字符串
}

type ORMConfiger interface {
	MysqlConfiger
	RedisConfiger
	GlobalConfiger
	GetFlushIntervalMs() int
	GetWorkerCount() int
}
