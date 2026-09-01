package orm

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"
	"unsafe"

	"github.com/bytedance/sonic"
	goredis "github.com/go-redis/redis/v8"
)

// RedisStore 提供对象级 Redis 存取操作，序列化使用 sonic（字节级零拷贝 JSON）。
type RedisStore struct {
	pool      *Pool
	useGlobal bool
}

var (
	defaultRedisStore = &RedisStore{}
	globalRedisStore  = &RedisStore{useGlobal: true}
)

func getRedisStoreForRoute(useGlobal bool) *RedisStore {
	if useGlobal {
		globalRedisStore.pool = GetPool()
		return globalRedisStore
	}
	defaultRedisStore.pool = GetPool()
	return defaultRedisStore
}

// redisKey 生成对象的 Redis key：{table}:{pk}
func redisKey(tableName string, pk any) string {
	return fmt.Sprintf("%s:%v", tableName, pk)
}

// Set 将对象按字段写入 Redis Hash，TTL 从全局配置读取。
// 对象级入口：内部先做一次字段快照，再交给 SetSnapshot。
func (that *RedisStore) Set(ctx context.Context, tableName string, pk any, obj any) error {
	meta := GetTableMeta(reflect.TypeOf(obj))
	return that.SetSnapshot(ctx, tableName, pk, meta, snapshotFields(meta, pointerOf(obj)))
}

// SetSnapshot 用调用方已经算好的字段快照写入 Redis Hash。
// 快照中的复杂字段已由 readFieldValue 编码为 jsonValue，这里直接复用那串字节，
// 因此 Save() 一次调用里复杂字段只会被 sonic 编码一次（Redis 与 MySQL 共用）。
func (that *RedisStore) SetSnapshot(ctx context.Context, tableName string, pk any, meta *TableMeta, snap []any) error {
	fields, err := buildRedisHashFieldsFromSnapshot(meta, snap)
	if err != nil {
		// 下层已带好列名与 ErrCodec，这里只补表名和主键
		return withContext("", tableName, pk, nil, err)
	}

	key := redisKey(tableName, pk)
	rcfg := that.pool.SelectRedisConfig(that.useGlobal)
	ttl := time.Duration(rcfg.GetRedisKeyTTLSec()) * time.Second
	client := that.pool.SelectRedis(that.useGlobal)
	pipe := client.TxPipeline()
	pipe.HSet(ctx, key, fields)
	pipe.Expire(ctx, key, ttl)
	if _, err = pipe.Exec(ctx); err != nil {
		return newError("SetSnapshot", tableName, pk, nil, err)
	}
	return nil
}

// Get 从 Redis Hash 读取对象；key 不存在时返回 goredis.Nil。
func (that *RedisStore) Get(ctx context.Context, tableName string, pk any, dest any) error {
	key := redisKey(tableName, pk)
	vals, err := that.pool.SelectRedis(that.useGlobal).HGetAll(ctx, key).Result()
	if err != nil {
		return err // 包含 goredis.Nil
	}
	if len(vals) == 0 {
		return goredis.Nil
	}

	meta := GetTableMeta(reflect.TypeOf(dest))
	base := pointerOf(dest)
	if err := applyRedisHashFields(meta, base, vals); err != nil {
		return withContext("", tableName, pk, nil, err)
	}
	return nil
}

// Del 从 Redis 删除对象缓存。
func (that *RedisStore) Del(ctx context.Context, tableName string, pk any) error {
	return that.pool.SelectRedis(that.useGlobal).Del(ctx, redisKey(tableName, pk)).Err()
}

// buildRedisHashFieldsFromSnapshot 把字段快照转换成 Redis Hash 字段表。
//
// 快照里有两类值：
//   - jsonValue —— 复杂字段，readFieldValue 已经编码好，直接复用（省掉重复的一次 Marshal）
//   - 基本类型原生值 —— 仍编码为 JSON 文本，保持与 applyRedisHashFields 的读侧解码对称
func buildRedisHashFieldsFromSnapshot(meta *TableMeta, snap []any) (map[string]interface{}, error) {
	fields := make(map[string]interface{}, len(meta.Fields))
	for i, f := range meta.Fields {
		if jv, ok := snap[i].(jsonValue); ok {
			fields[f.ColName] = string(jv)
			continue
		}
		if snap[i] == nil {
			// readFieldValue 编码失败时才会返回 nil（已打日志），此处不静默写脏数据
			return nil, &Error{Op: "Marshal", Column: f.ColName, Kind: ErrCodec,
				Err: errors.New("nil snapshot value (marshal failed)")}
		}
		data, err := sonic.Marshal(snap[i])
		if err != nil {
			return nil, &Error{Op: "Marshal", Column: f.ColName, Kind: ErrCodec, Err: err}
		}
		fields[f.ColName] = string(data)
	}
	return fields, nil
}

func applyRedisHashFields(meta *TableMeta, base unsafe.Pointer, values map[string]string) error {
	for _, f := range meta.Fields {
		raw, ok := values[f.ColName]
		if !ok {
			continue
		}
		ptr := FieldPtr(base, f.Offset)
		target := reflect.NewAt(f.GoType, ptr).Interface()
		if err := sonic.Unmarshal([]byte(raw), target); err != nil {
			return &Error{Op: "Unmarshal", Column: f.ColName, Kind: ErrCodec, Err: err}
		}
	}
	return nil
}

// SetRaw 直接写入 JSON bytes，供批量刷盘使用，避免二次序列化。
func (that *RedisStore) SetRaw(ctx context.Context, key string, data []byte, ttl time.Duration) error {
	return that.pool.SelectRedis(that.useGlobal).Set(ctx, key, data, ttl).Err()
}

// pointerOf 将任意指针类型转换为 unsafe.Pointer，用于字段偏移运算。
// 这是整个框架的"魔法入口"：通过 unsafe.Pointer 桥接 reflect 元数据与运行时对象。
func pointerOf(v any) unsafe.Pointer {
	// any 的底层布局：[itab *ptr | data *ptr]
	// 此处利用 Go interface 内存布局直接取 data 指针。
	type iface struct {
		_    uintptr
		data unsafe.Pointer
	}
	return (*iface)(unsafe.Pointer(&v)).data
}
