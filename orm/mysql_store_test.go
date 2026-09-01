package orm

import (
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bytedance/sonic"
)

type flushTestObj struct {
	ID    int64  `orm:"primary,name:id,autoInc"`
	Name  string `orm:"name:name"`
	Score int    `orm:"name:score"`
}

// pushFlushItem 把一个对象的快照按固定 key 压入队列，返回入队的条目。
func pushFlushItem(q *flushQueue, meta *TableMeta, o *flushTestObj) *pendingItem {
	item := &pendingItem{
		key:       "t:1",
		tableName: "t",
		meta:      meta,
		snapshot:  snapshotFields(meta, pointerOf(o)),
	}
	q.push(item)
	return item
}

func TestFlushQueueDedup(t *testing.T) {
	q := newFlushQueue()

	meta := GetTableMeta(reflect.TypeOf(&flushTestObj{}))
	pushFlushItem(q, meta, &flushTestObj{ID: 1, Name: "v1", Score: 10})
	pushFlushItem(q, meta, &flushTestObj{ID: 1, Name: "v2", Score: 20}) // 应覆盖 v1

	items := q.due(time.Now())
	if len(items) != 1 {
		t.Fatalf("expected 1 item after dedup, got %d", len(items))
	}
	// snapshot 应该是 v2 的值
	nameIdx := 1 // Fields[1] = name
	if items[0].snapshot[nameIdx].(string) != "v2" {
		t.Errorf("expected v2, got %v", items[0].snapshot[nameIdx])
	}
}

// TestFlushQueueKeepsItemUntilSettled 刷盘失败的条目必须留在队列里等重试，
// 这是 P0 修复的核心：旧实现 drain() 一次性摘走全部条目，失败即永久丢失。
func TestFlushQueueKeepsItemUntilSettled(t *testing.T) {
	q := newFlushQueue()
	meta := GetTableMeta(reflect.TypeOf(&flushTestObj{}))
	item := pushFlushItem(q, meta, &flushTestObj{ID: 1, Name: "v1"})

	now := time.Now()
	if got := len(q.due(now)); got != 1 {
		t.Fatalf("首次应有 1 条待刷，实际 %d", got)
	}

	// 模拟一次失败：退避到 1 秒后
	q.backoff(item, now.Add(time.Second))
	if got := len(q.due(now)); got != 0 {
		t.Fatalf("退避期内不应重试，实际有 %d 条", got)
	}
	if got := len(q.all()); got != 1 {
		t.Fatalf("条目必须留在队列里，实际 %d", got)
	}

	// 退避到期后重新可刷
	if got := len(q.due(now.Add(2 * time.Second))); got != 1 {
		t.Fatalf("退避到期后应可重试，实际 %d", got)
	}

	// 成功后才移除
	q.settle(item)
	if got := len(q.all()); got != 0 {
		t.Fatalf("成功后队列应为空，实际 %d", got)
	}
}

// TestFlushQueueNewerSnapshotWinsOverRetry 重试期间若有新的 Save 覆盖同一 key，
// 必须以新快照为准；旧条目的成功或失败都不能影响新快照。
func TestFlushQueueNewerSnapshotWinsOverRetry(t *testing.T) {
	meta := GetTableMeta(reflect.TypeOf(&flushTestObj{}))
	nameIdx := 1

	// 情况一：旧条目重试失败，新快照必须原样留下且立即可刷
	q := newFlushQueue()
	old := pushFlushItem(q, meta, &flushTestObj{ID: 1, Name: "old"})
	pushFlushItem(q, meta, &flushTestObj{ID: 1, Name: "new"})
	q.backoff(old, time.Now().Add(time.Hour))

	items := q.due(time.Now())
	if len(items) != 1 || items[0].snapshot[nameIdx].(string) != "new" {
		t.Fatalf("新快照应立即可刷，实际 %+v", items)
	}

	// 情况二：旧条目重试成功，也不能把新快照删掉
	q2 := newFlushQueue()
	old2 := pushFlushItem(q2, meta, &flushTestObj{ID: 1, Name: "old"})
	pushFlushItem(q2, meta, &flushTestObj{ID: 1, Name: "new"})
	q2.settle(old2)

	remain := q2.all()
	if len(remain) != 1 || remain[0].snapshot[nameIdx].(string) != "new" {
		t.Fatalf("旧条目成功不应移除新快照，实际 %+v", remain)
	}
}

func TestHashKey(t *testing.T) {
	a := hashKey("player:1001")
	b := hashKey("player:1001")
	if a != b {
		t.Error("hash must be deterministic")
	}
	c := hashKey("player:1002")
	if a == c {
		t.Log("hash collision (rare, acceptable)")
	}
}

func TestSnapshotFields(t *testing.T) {
	obj := &flushTestObj{ID: 42, Name: "test", Score: 99}
	meta := GetTableMeta(reflect.TypeOf(obj))
	snap := snapshotFields(meta, pointerOf(obj))
	if snap[0].(int64) != 42 {
		t.Errorf("expected ID=42, got %v", snap[0])
	}
	if snap[1].(string) != "test" {
		t.Errorf("expected name=test, got %v", snap[1])
	}
	if snap[2].(int) != 99 {
		t.Errorf("expected score=99, got %v", snap[2])
	}
}

func TestCloneTableMetaDeepCopy(t *testing.T) {
	fields := []*FieldMeta{
		{GoName: "ID", ColName: "id", IsPrimary: true},
		{GoName: "Name", ColName: "name"},
	}
	orig := &TableMeta{TableName: "flush_test", Fields: fields, PrimaryField: fields[0]}

	cloned := cloneTableMeta(orig)
	if cloned == orig {
		t.Fatal("cloneTableMeta should return a new TableMeta instance")
	}
	if len(cloned.Fields) != len(orig.Fields) {
		t.Fatalf("expected %d fields, got %d", len(orig.Fields), len(cloned.Fields))
	}
	if cloned.Fields[0] == orig.Fields[0] {
		t.Fatal("cloneTableMeta should deep-copy FieldMeta entries")
	}
	if cloned.PrimaryField != cloned.Fields[0] {
		t.Fatal("cloned PrimaryField should point to cloned field entry")
	}

	orig.TableName = "mutated_table"
	orig.Fields[0].ColName = "mutated_id"
	orig.PrimaryField.ColName = "mutated_pk"

	if cloned.TableName != "flush_test" {
		t.Fatalf("cloned table name changed unexpectedly: %s", cloned.TableName)
	}
	if cloned.Fields[0].ColName != "id" {
		t.Fatalf("cloned field changed unexpectedly: %s", cloned.Fields[0].ColName)
	}
}

func TestFreezeTableMetaCachesSingleClone(t *testing.T) {
	fields := []*FieldMeta{
		{GoName: "ID", ColName: "id", IsPrimary: true},
		{GoName: "Name", ColName: "name"},
	}
	orig := &TableMeta{TableName: "freeze_test", Fields: fields, PrimaryField: fields[0]}

	const n = 16
	results := make([]*TableMeta, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = freezeTableMeta(orig)
		}(i)
	}
	wg.Wait()

	first := results[0]
	if first == nil {
		t.Fatal("freezeTableMeta returned nil")
	}
	if first == orig {
		t.Fatal("freezeTableMeta should not return original meta pointer")
	}
	for i := 1; i < n; i++ {
		if results[i] != first {
			t.Fatal("freezeTableMeta should return same cached clone for same meta")
		}
	}
}

type jsonValueObj struct {
	ID   int64            `orm:"primary,name:id"`
	Bag  map[string]int64 `orm:"name:bag"`
	Name string           `orm:"name:name"`
}

// TestReadFieldValueWrapsComplexAsJSONValue 复杂字段必须以 jsonValue 返回，
// 这是下游识别"已编码"的唯一依据；基本类型必须保持原生类型。
func TestReadFieldValueWrapsComplexAsJSONValue(t *testing.T) {
	obj := &jsonValueObj{ID: 1, Bag: map[string]int64{"a": 1}, Name: "x"}
	meta := GetTableMeta(reflect.TypeOf(obj))
	snap := snapshotFields(meta, pointerOf(obj))

	if _, ok := snap[0].(int64); !ok {
		t.Errorf("基本类型不应被包装: %T", snap[0])
	}
	if _, ok := snap[1].(jsonValue); !ok {
		t.Errorf("复杂字段应包装为 jsonValue: %T", snap[1])
	}
	if _, ok := snap[2].(string); !ok {
		t.Errorf("string 字段应保持 string: %T", snap[2])
	}
}

// TestJSONValueIsDriverValuer jsonValue 必须能直接作为 SQL 参数，
// 否则 execUpsert 传参时会被 database/sql 拒绝。
func TestJSONValueIsDriverValuer(t *testing.T) {
	var v any = jsonValue(`{"a":1}`)
	valuer, ok := v.(driver.Valuer)
	if !ok {
		t.Fatal("jsonValue 必须实现 driver.Valuer")
	}
	got, err := valuer.Value()
	if err != nil {
		t.Fatalf("Value() error: %v", err)
	}
	if s, ok := got.(string); !ok || s != `{"a":1}` {
		t.Fatalf("Value() 应返回原始 JSON 文本，实际 %#v", got)
	}
}

// newTestStore 构造一个不连数据库的 MySQLStore，用于验证队列与关闭语义。
func newTestStore(nWorker int) *MySQLStore {
	s := &MySQLStore{
		nWorker:       nWorker,
		flushInterval: 500 * time.Millisecond,
		queues:        make([]*flushQueue, nWorker),
		stopCh:        make(chan struct{}),
	}
	for i := range nWorker {
		s.queues[i] = newFlushQueue()
	}
	return s
}

// captureArchiveErrors 接管失败回调并返回收集到的事件，测试结束自动还原。
func captureArchiveErrors(t *testing.T) *[]ArchiveError {
	t.Helper()
	var mu sync.Mutex
	events := make([]ArchiveError, 0, 4)
	SetArchiveErrorHandler(func(ev ArchiveError) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	})
	t.Cleanup(func() { SetArchiveErrorHandler(nil) })
	return &events
}

// TestStopIsIdempotent 关服路径上多处调用 Shutdown 不应 panic。
func TestStopIsIdempotent(t *testing.T) {
	s := newTestStore(1)
	s.Stop()
	s.Stop() // 旧实现在这里 close 了已关闭的 channel
	s.Stop()
}

// TestStopReportsUnflushedItems 退出时仍未落库的存档必须被报出来，不能静默消失。
func TestStopReportsUnflushedItems(t *testing.T) {
	events := captureArchiveErrors(t)
	meta := GetTableMeta(reflect.TypeOf(&flushTestObj{}))

	s := newTestStore(1)
	pushFlushItem(s.queues[0], meta, &flushTestObj{ID: 1, Name: "未落库"})
	s.Stop()

	if len(*events) != 1 {
		t.Fatalf("应报告 1 条未落库存档，实际 %d", len(*events))
	}
	ev := (*events)[0]
	if !ev.Dropped || !errors.Is(ev.Err, errNotFlushed) {
		t.Fatalf("事件内容不符: %+v", ev)
	}
}

// TestEnqueueAfterStopIsReported 关闭之后再 Save，旧实现会静默入队然后丢掉。
func TestEnqueueAfterStopIsReported(t *testing.T) {
	events := captureArchiveErrors(t)
	meta := GetTableMeta(reflect.TypeOf(&flushTestObj{}))

	s := newTestStore(1)
	s.Stop()
	*events = (*events)[:0] // 忽略 Stop 自身可能产生的事件

	obj := &flushTestObj{ID: 7, Name: "关服后写入"}
	s.EnqueueSaveSnapshot("t", meta, snapshotFields(meta, pointerOf(obj)))

	if len(*events) != 1 {
		t.Fatalf("关闭后入队应上报 1 条，实际 %d", len(*events))
	}
	ev := (*events)[0]
	if !ev.Dropped || !errors.Is(ev.Err, ErrStoreStopped) {
		t.Fatalf("事件内容不符: %+v", ev)
	}
	// 这条数据不会落库，日志必须带着完整内容
	if ev.Columns["name"] != "关服后写入" {
		t.Fatalf("上报缺少存档内容: %v", ev.Columns)
	}
	for _, q := range s.queues {
		if got := len(q.all()); got != 0 {
			t.Fatalf("关闭后不应再入队，队列里却有 %d 条", got)
		}
	}
}

// TestRetryBackoffGrowsAndCaps 退避应指数增长并封顶。
func TestRetryBackoffGrowsAndCaps(t *testing.T) {
	s := newTestStore(1)
	cases := []struct {
		attempts int
		want     time.Duration
	}{
		{1, 500 * time.Millisecond},
		{2, time.Second},
		{3, 2 * time.Second},
		{100, maxRetryBackoff},
	}
	for _, c := range cases {
		if got := s.retryBackoff(c.attempts); got != c.want {
			t.Errorf("retryBackoff(%d) = %v, want %v", c.attempts, got, c.want)
		}
	}
}

// TestArchiveErrorHandlerPanicDoesNotBreakFlush 业务回调 panic 不能拖垮刷盘 worker。
func TestArchiveErrorHandlerPanicDoesNotBreakFlush(t *testing.T) {
	SetArchiveErrorHandler(func(ArchiveError) { panic("业务回调炸了") })
	t.Cleanup(func() { SetArchiveErrorHandler(nil) })

	meta := GetTableMeta(reflect.TypeOf(&flushTestObj{}))
	s := newTestStore(1)
	pushFlushItem(s.queues[0], meta, &flushTestObj{ID: 1, Name: "x"})
	s.Stop() // 内部会触发回调；不应把测试带崩
}

// payloadTestObj 带一个复杂字段，用于验证上报内容里 JSON 列的形态。
type payloadTestObj struct {
	ID   int64            `orm:"primary,name:id"`
	Name string           `orm:"name:name"`
	Bag  map[string]int64 `orm:"name:bag"`
}

// alwaysFail 是永远失败的 exec 桩。
func alwaysFail(err error) func(*pendingItem) error {
	return func(*pendingItem) error { return err }
}

// TestFlushGivesUpAfterMaxRetries 首次尝试 + maxFlushRetries 次重试后放弃，
// 每次失败都要上报一次，最后一次标记为 Dropped 并把条目移出队列。
func TestFlushGivesUpAfterMaxRetries(t *testing.T) {
	events := captureArchiveErrors(t)
	meta := GetTableMeta(reflect.TypeOf(&flushTestObj{}))

	s := newTestStore(1)
	q := s.queues[0]
	pushFlushItem(q, meta, &flushTestObj{ID: 1, Name: "v1"})

	boom := errors.New("mysql 挂了")
	// all() 忽略退避，等价于把每一轮重试都走一遍
	for i := 0; i < maxFlushRetries+3; i++ {
		s.flushItems(q, q.all(), alwaysFail(boom))
	}

	wantAttempts := maxFlushRetries + 1
	if len(*events) != wantAttempts {
		t.Fatalf("应上报 %d 次（首次 + %d 次重试），实际 %d 次",
			wantAttempts, maxFlushRetries, len(*events))
	}
	for i, ev := range *events {
		if ev.Attempt != i+1 {
			t.Errorf("第 %d 条上报的 Attempt = %d，期望 %d", i, ev.Attempt, i+1)
		}
		wantDropped := i == wantAttempts-1
		if ev.Dropped != wantDropped {
			t.Errorf("第 %d 条上报 Dropped = %v，期望 %v", i, ev.Dropped, wantDropped)
		}
	}
	if got := len(q.all()); got != 0 {
		t.Fatalf("放弃后条目应移出队列，实际还剩 %d 条", got)
	}
}

// TestFlushStopsRetryingOnceSucceeded 中途成功就不该再重试，也不该出现 Dropped。
func TestFlushStopsRetryingOnceSucceeded(t *testing.T) {
	events := captureArchiveErrors(t)
	meta := GetTableMeta(reflect.TypeOf(&flushTestObj{}))

	s := newTestStore(1)
	q := s.queues[0]
	pushFlushItem(q, meta, &flushTestObj{ID: 1, Name: "v1"})

	calls := 0
	exec := func(*pendingItem) error {
		calls++
		if calls <= 2 {
			return errors.New("暂时写不进去")
		}
		return nil
	}
	for i := 0; i < 5; i++ {
		s.flushItems(q, q.all(), exec)
	}

	if calls != 3 {
		t.Fatalf("成功后不应再执行，实际执行 %d 次", calls)
	}
	if len(*events) != 2 {
		t.Fatalf("应只为两次失败各上报一次，实际 %d 次", len(*events))
	}
	for _, ev := range *events {
		if ev.Dropped {
			t.Errorf("未用尽重试次数不应标记 Dropped: %+v", ev)
		}
	}
	if got := len(q.all()); got != 0 {
		t.Fatalf("成功后队列应为空，实际 %d 条", got)
	}
}

// TestArchiveErrorCarriesRecoverableData 上报内容必须足以从日志恢复这条存档：
// 列名齐全，值就是当初要绑定给 SQL 的参数（复杂字段是已编码好的 JSON 文本）。
func TestArchiveErrorCarriesRecoverableData(t *testing.T) {
	events := captureArchiveErrors(t)
	meta := GetTableMeta(reflect.TypeOf(&payloadTestObj{}))
	obj := &payloadTestObj{ID: 1001, Name: "阿吉", Bag: map[string]int64{"gold": 99}}

	s := newTestStore(1)
	q := s.queues[0]
	q.push(&pendingItem{
		key: "payload_test_obj:1001", tableName: "payload_test_obj",
		meta: meta, snapshot: snapshotFields(meta, pointerOf(obj)),
	})
	s.flushItems(q, q.all(), alwaysFail(errors.New("boom")))

	if len(*events) != 1 {
		t.Fatalf("应上报 1 次，实际 %d", len(*events))
	}
	ev := (*events)[0]

	if ev.Table != "payload_test_obj" || ev.PK != int64(1001) {
		t.Fatalf("表名/主键不符: %+v", ev)
	}
	if len(ev.Columns) != 3 {
		t.Fatalf("列数不符: %v", ev.Columns)
	}
	if ev.Columns["name"] != "阿吉" {
		t.Errorf("name 列不符: %v", ev.Columns["name"])
	}

	// 日志里的 JSON 必须能解析回来，且每个值可以直接当 SQL 参数用
	var back map[string]any
	if err := sonic.UnmarshalString(ev.PayloadJSON(), &back); err != nil {
		t.Fatalf("PayloadJSON 不是合法 JSON: %v (%s)", err, ev.PayloadJSON())
	}
	bag, ok := back["bag"].(string)
	if !ok {
		t.Fatalf("JSON 列应渲染成字符串形式的 SQL 参数，实际 %T", back["bag"])
	}
	var bagBack map[string]int64
	if err := sonic.UnmarshalString(bag, &bagBack); err != nil || bagBack["gold"] != 99 {
		t.Fatalf("bag 内容无法还原: %q err=%v", bag, err)
	}

	// 默认日志格式里也必须带上数据
	if !strings.Contains(ev.Error(), "data=") || !strings.Contains(ev.Error(), "gold") {
		t.Fatalf("Error() 未包含存档内容: %s", ev.Error())
	}
}
