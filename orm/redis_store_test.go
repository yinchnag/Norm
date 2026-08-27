package orm

import (
	"reflect"
	"sync/atomic"
	"testing"
	"unsafe"

	"github.com/bytedance/sonic"
)

type pkTestObj struct {
	ID   int64
	Name string
}

func TestPointerOf(t *testing.T) {
	obj := &pkTestObj{ID: 42, Name: "hello"}
	ptr := pointerOf(obj)
	// 通过偏移读取 ID 字段（偏移=0）
	id := *(*int64)(unsafe.Pointer(uintptr(ptr)))
	if id != 42 {
		t.Errorf("expected 42, got %d", id)
	}
}

func TestRedisKey(t *testing.T) {
	key := redisKey("player", int64(1001))
	if key != "player:1001" {
		t.Errorf("unexpected key: %s", key)
	}
}

func TestIsNotFound(t *testing.T) {
	if !IsNotFound(errRedisNil()) {
		t.Error("expected IsNotFound=true for redis.Nil")
	}
}

// errRedisNil 返回 goredis.Nil，避免引入 goredis 包直接依赖测试文件。
func errRedisNil() error {
	store := &RedisStore{}
	_ = store
	return goredisNil
}

type redisHashCodecObj struct {
	TableSchema[*redisHashCodecObj]
	ID    int64            `orm:"primary,name:id"`
	Name  string           `orm:"name:name,length:64"`
	Score float64          `orm:"name:score"`
	Tags  map[string]int64 `orm:"name:tags"`
}

func TestRedisHashFieldCodecRoundTrip(t *testing.T) {
	obj := &redisHashCodecObj{
		ID:    101,
		Name:  "alice",
		Score: 99.5,
		Tags:  map[string]int64{"pve": 7, "pvp": 3},
	}

	meta := GetTableMeta(reflect.TypeOf(obj))
	fields, err := buildRedisHashFieldsFromSnapshot(meta, snapshotFields(meta, pointerOf(obj)))
	if err != nil {
		t.Fatalf("buildRedisHashFieldsFromSnapshot error: %v", err)
	}

	raw := make(map[string]string, len(fields))
	for k, v := range fields {
		raw[k] = v.(string)
	}

	out := &redisHashCodecObj{}
	if err := applyRedisHashFields(meta, pointerOf(out), raw); err != nil {
		t.Fatalf("applyRedisHashFields error: %v", err)
	}

	if out.ID != obj.ID || out.Name != obj.Name || out.Score != obj.Score {
		t.Fatalf("primitive fields mismatch: out=%+v want=%+v", out, obj)
	}
	if len(out.Tags) != len(obj.Tags) || out.Tags["pve"] != obj.Tags["pve"] || out.Tags["pvp"] != obj.Tags["pvp"] {
		t.Fatalf("map field mismatch: out=%v want=%v", out.Tags, obj.Tags)
	}
}

// complexMarshalCount 统计 countingBag.MarshalJSON 的调用次数，
// 用于断言"一次 Save 里复杂字段只被编码一次"。
var complexMarshalCount int64

type countingBag map[string]int64

// MarshalJSON 每被调用一次计数加一；内部转成裸 map 编码，不会递归。
func (that countingBag) MarshalJSON() ([]byte, error) {
	atomic.AddInt64(&complexMarshalCount, 1)
	return sonic.Marshal(map[string]int64(that))
}

type encodeOnceObj struct {
	TableSchema[*encodeOnceObj]
	ID    int64       `orm:"primary,name:id"`
	Name  string      `orm:"name:name,length:64"`
	Level int32       `orm:"name:level"`
	Bag   countingBag `orm:"name:bag"`
}

// TestComplexFieldEncodedOnce 复现 Save() 的编码流程：快照一次 + 构建 Hash 字段表一次。
// 复杂字段的 JSON 编码只应发生在快照阶段，Redis 侧必须复用结果而不是再编一遍。
//
// 断言方式不数"总调用次数"——sonic 一次编码内部调用 MarshalJSON 的次数不固定
// （开启 -race 时会走兼容实现）——而是看构建 Hash 字段表这一步有没有新增调用。
func TestComplexFieldEncodedOnce(t *testing.T) {
	obj := &encodeOnceObj{ID: 7, Name: "bob", Level: 3, Bag: countingBag{"gold": 99}}
	meta := GetTableMeta(reflect.TypeOf(obj))

	atomic.StoreInt64(&complexMarshalCount, 0)

	snap := snapshotFields(meta, pointerOf(obj)) // MySQL 侧用这份
	afterSnapshot := atomic.LoadInt64(&complexMarshalCount)
	if afterSnapshot == 0 {
		t.Fatal("快照阶段应完成复杂字段的编码")
	}

	fields, err := buildRedisHashFieldsFromSnapshot(meta, snap) // Redis 侧复用同一份
	if err != nil {
		t.Fatalf("buildRedisHashFieldsFromSnapshot error: %v", err)
	}
	if n := atomic.LoadInt64(&complexMarshalCount); n != afterSnapshot {
		t.Fatalf("Redis 侧重复编码了复杂字段：新增 %d 次调用", n-afterSnapshot)
	}

	bagIdx := 3
	jv, ok := snap[bagIdx].(jsonValue)
	if !ok {
		t.Fatalf("复杂字段快照应为 jsonValue，实际类型 %T", snap[bagIdx])
	}
	if got := fields["bag"].(string); got != string(jv) {
		t.Fatalf("Redis 与 MySQL 的复杂字段编码结果不一致: %q vs %q", got, string(jv))
	}
}

// legacyBuildRedisHashFields 是改造前的实现，仅供测试比对存储格式用。
func legacyBuildRedisHashFields(meta *TableMeta, base unsafe.Pointer) (map[string]string, error) {
	fields := make(map[string]string, len(meta.Fields))
	for _, f := range meta.Fields {
		ptr := FieldPtr(base, f.Offset)
		v := reflect.NewAt(f.GoType, ptr).Elem().Interface()
		data, err := sonic.Marshal(v)
		if err != nil {
			return nil, err
		}
		fields[f.ColName] = string(data)
	}
	return fields, nil
}

// TestRedisHashWireFormatUnchanged 断言改造只影响编码次数，不改变 Redis 里的存储格式。
//
// 按语义而非字节比对：sonic 编码 map 时不排序 key，同一个 map 两次编码的
// 字节顺序可能不同，逐字节比较会偶发失败。
func TestRedisHashWireFormatUnchanged(t *testing.T) {
	obj := &redisHashCodecObj{
		ID:    101,
		Name:  "alice",
		Score: 99.5,
		Tags:  map[string]int64{"pve": 7, "pvp": 3},
	}
	meta := GetTableMeta(reflect.TypeOf(obj))

	want, err := legacyBuildRedisHashFields(meta, pointerOf(obj))
	if err != nil {
		t.Fatalf("legacyBuildRedisHashFields error: %v", err)
	}
	got, err := buildRedisHashFieldsFromSnapshot(meta, snapshotFields(meta, pointerOf(obj)))
	if err != nil {
		t.Fatalf("buildRedisHashFieldsFromSnapshot error: %v", err)
	}

	if len(got) != len(want) {
		t.Fatalf("字段数不一致: got=%d want=%d", len(got), len(want))
	}
	for col, w := range want {
		g, ok := got[col].(string)
		if !ok {
			t.Errorf("列 %s 应写入 JSON 文本，实际类型 %T", col, got[col])
			continue
		}
		var gv, wv any
		if err := sonic.Unmarshal([]byte(g), &gv); err != nil {
			t.Errorf("列 %s 新值不是合法 JSON: %q", col, g)
			continue
		}
		if err := sonic.Unmarshal([]byte(w), &wv); err != nil {
			t.Errorf("列 %s 旧值不是合法 JSON: %q", col, w)
			continue
		}
		if !reflect.DeepEqual(gv, wv) {
			t.Errorf("列 %s 存储内容变化: got=%q want=%q", col, g, w)
		}
	}
}

// BenchmarkBuildRedisHashFields 度量 Save() 的编码开销：一次快照 + 一次 Hash 字段表构建。
// 当前剩余的大头是基本类型仍逐字段 sonic.Marshal，可作为后续优化的基线。
func BenchmarkBuildRedisHashFields(b *testing.B) {
	obj := &redisHashCodecObj{
		ID:    101,
		Name:  "alice",
		Score: 99.5,
		Tags:  map[string]int64{"pve": 7, "pvp": 3},
	}
	meta := GetTableMeta(reflect.TypeOf(obj))
	base := pointerOf(obj)

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := buildRedisHashFieldsFromSnapshot(meta, snapshotFields(meta, base)); err != nil {
			b.Fatal(err)
		}
	}
}
