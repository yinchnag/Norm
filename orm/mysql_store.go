package orm

import (
	"context"
	"database/sql/driver"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/bytedance/sonic"
)

// pendingItem 代表一条待刷盘到 MySQL 的写入请求。
// 同一 key 的新请求会覆盖老请求，从而合并高频存盘，减少 MySQL 写操作次数。
type pendingItem struct {
	key       string // "{table}:{pk}"，用于去重
	tableName string
	meta      *TableMeta
	snapshot  []any // 字段值快照，与 meta.Fields 一一对应
	deleted   bool  // 是否标记删除

	// 以下两个字段只由持有该队列的 worker goroutine 读写：
	// push 永远创建新的 pendingItem，不会修改已入队的条目，因此无需加锁。
	attempts   int       // 已尝试刷盘次数，用于退避与告警
	retryAfter time.Time // 早于此刻不再重试；零值表示立即可刷
}

// flushQueue 是单个 worker 的待刷盘队列，使用 map 保证同 key 只保留最新快照。
type flushQueue struct {
	mu    sync.Mutex
	items map[string]*pendingItem
}

func newFlushQueue() *flushQueue {
	return &flushQueue{items: make(map[string]*pendingItem, 64)}
}

func (that *flushQueue) push(item *pendingItem) {
	that.mu.Lock()
	that.items[item.key] = item // 覆盖旧条目——核心去重逻辑
	that.mu.Unlock()
}

// due 返回当前到期、可以尝试刷盘的条目。
// 条目仍然留在队列里——只有 settle 才会移除它，失败的写入因此天然获得重试机会。
func (that *flushQueue) due(now time.Time) []*pendingItem {
	that.mu.Lock()
	defer that.mu.Unlock()
	out := make([]*pendingItem, 0, len(that.items))
	for _, v := range that.items {
		if !v.retryAfter.After(now) {
			out = append(out, v)
		}
	}
	return out
}

// all 返回队列中全部条目，忽略退避时间。
// 用于关闭前的最后一次刷盘，以及退出时报告仍未落盘的存档。
func (that *flushQueue) all() []*pendingItem {
	that.mu.Lock()
	defer that.mu.Unlock()
	out := make([]*pendingItem, 0, len(that.items))
	for _, v := range that.items {
		out = append(out, v)
	}
	return out
}

// settle 在刷盘成功后把条目移出队列。
// 若期间已有新的 Save 覆盖同一 key，队列里就不再是这一条，此时什么都不做——
// 新快照必须保留下来，否则会把更新的存档丢掉。
func (that *flushQueue) settle(item *pendingItem) {
	that.mu.Lock()
	if that.items[item.key] == item {
		delete(that.items, item.key)
	}
	that.mu.Unlock()
}

// backoff 在刷盘失败后把条目留在队列里，并记录下次可重试的时刻。
// 同样只在队列里仍是这一条时才生效：被新快照覆盖的旧条目直接作废，
// 因为新快照包含了它的全部内容。
func (that *flushQueue) backoff(item *pendingItem, retryAt time.Time) {
	that.mu.Lock()
	if that.items[item.key] == item {
		item.retryAfter = retryAt
	}
	that.mu.Unlock()
}

// MySQLStore 管理异步、批量、去重的 MySQL 刷盘。
// 架构：N 个 worker goroutine，每隔 FlushInterval 批量执行 UPSERT/软删除。
// 提交操作：调用 EnqueueSave/EnqueueDelete 仅将快照入队，不阻塞游戏逻辑。
type MySQLStore struct {
	pool          *Pool
	useGlobal     bool
	queues        []*flushQueue
	nWorker       int
	flushInterval time.Duration
	stopCh        chan struct{}
	wg            sync.WaitGroup
	stopOnce      sync.Once   // 保证 Stop 幂等，重复调用不会 close 已关闭的 channel
	stopped       atomic.Bool // 置位后拒收新的写入请求，不再静默入队
}

// maxFlushRetries 是刷盘失败后的最大重试次数，不含首次尝试。
// 即一条存档最多尝试 1 + maxFlushRetries 次；次数用尽后放弃并按 Dropped 上报，
// 此时数据只剩日志里那一份（ArchiveError.Columns 带着完整内容）。
var maxFlushRetries = 3

// maxRetryBackoff 是刷盘失败重试的退避上限。
// 退避从 FlushInterval 起按 2 的幂增长，封顶于此值。
// 默认配置（FlushInterval=500ms、重试 3 次）下重试窗口约 3.5 秒。
var maxRetryBackoff = 30 * time.Second

var (
	globalMySQLStore           *MySQLStore
	globalRegionMySQLStore     *MySQLStore
	mysqlStoreOnce             sync.Once
	globalRegionMySQLStoreOnce sync.Once
	frozenMetaCache            sync.Map // key: *TableMeta -> *TableMeta（深拷贝后的只读副本）
)

func freezeTableMeta(meta *TableMeta) *TableMeta {
	if v, ok := frozenMetaCache.Load(meta); ok {
		return v.(*TableMeta)
	}
	cloned := cloneTableMeta(meta)
	actual, _ := frozenMetaCache.LoadOrStore(meta, cloned)
	return actual.(*TableMeta)
}

func cloneTableMeta(meta *TableMeta) *TableMeta {
	clonedFields := make([]*FieldMeta, len(meta.Fields))
	pkIdx := -1

	for i, f := range meta.Fields {
		cf := *f
		clonedFields[i] = &cf
		if f.IsPrimary {
			pkIdx = i
		}
	}

	cloned := &TableMeta{
		TableName: meta.TableName,
		Fields:    clonedFields,
	}
	if pkIdx >= 0 {
		cloned.PrimaryField = clonedFields[pkIdx]
	} else if meta.PrimaryField != nil {
		cpk := *meta.PrimaryField
		cloned.PrimaryField = &cpk
	}
	return cloned
}

// getMySQLStore 返回全局 MySQLStore 单例，首次调用时启动 worker。
func getMySQLStore() *MySQLStore {
	return getMySQLStoreForRoute(false)
}

func getMySQLStoreForRoute(useGlobal bool) *MySQLStore {
	if useGlobal {
		p := GetPool()
		if p.GlobalDB == nil {
			fmt.Printf("[gameorm] global mysql not configured, fallback to default mysql store\n")
			return getMySQLStore()
		}
		globalRegionMySQLStoreOnce.Do(func() {
			n := p.Cfg.GetWorkerCount()
			s := &MySQLStore{
				pool:          p,
				useGlobal:     true,
				nWorker:       n,
				flushInterval: flushIntervalOf(p),
				queues:        make([]*flushQueue, n),
				stopCh:        make(chan struct{}),
			}
			for i := range n {
				s.queues[i] = newFlushQueue()
			}
			s.start()
			globalRegionMySQLStore = s
		})
		return globalRegionMySQLStore
	}

	mysqlStoreOnce.Do(func() {
		p := GetPool()
		n := p.Cfg.GetWorkerCount()
		s := &MySQLStore{
			pool:          p,
			useGlobal:     false,
			nWorker:       n,
			flushInterval: flushIntervalOf(p),
			queues:        make([]*flushQueue, n),
			stopCh:        make(chan struct{}),
		}
		for i := range n {
			s.queues[i] = newFlushQueue()
		}
		s.start()
		globalMySQLStore = s
	})
	return globalMySQLStore
}

// flushIntervalOf 从连接池配置读取刷盘间隔，非法值回退到 500ms。
func flushIntervalOf(p *Pool) time.Duration {
	ms := p.Cfg.GetFlushIntervalMs()
	if ms <= 0 {
		ms = 500
	}
	return time.Duration(ms) * time.Millisecond
}

// start 启动所有 worker goroutine。
func (that *MySQLStore) start() {
	for i := range that.nWorker {
		that.wg.Add(1)
		go that.worker(i)
	}
}

// Stop 优雅停止所有 worker：
//  1. 置位 stopped，拒收新的写入请求
//  2. 等待每个 worker 完成最后一次刷盘
//  3. 报告仍未落库的存档，避免退出时静默丢档
//
// 重复调用是安全的。
func (that *MySQLStore) Stop() {
	that.stopOnce.Do(func() {
		that.stopped.Store(true)
		close(that.stopCh)
		that.wg.Wait()
		that.reportUnflushed()
	})
}

// reportUnflushed 报告进程退出时仍未落库的存档。
// 走到这一步说明 MySQL 在关服期间也写不进去，只能把丢失的内容暴露出来。
func (that *MySQLStore) reportUnflushed() {
	for _, q := range that.queues {
		for _, item := range q.all() {
			reportArchiveError(archiveErrorOf(item, errNotFlushed, true))
		}
	}
}

func (that *MySQLStore) worker(idx int) {
	defer that.wg.Done()
	ticker := time.NewTicker(that.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			that.flush(that.queues[idx])
		case <-that.stopCh:
			// 关闭前最后一次刷盘，忽略退避时间，给每条存档最后一次机会
			that.flushItems(that.queues[idx], that.queues[idx].all(), that.execItem)
			return
		}
	}
}

// flush 尝试刷盘队列中所有到期条目。
func (that *MySQLStore) flush(q *flushQueue) {
	that.flushItems(q, q.due(time.Now()), that.execItem)
}

// flushItems 逐条执行 SQL。
// 失败的条目留在队列里等下一轮重试——UPSERT 与软删除都是幂等的，
// 重复执行不会产生副作用；重试次数用尽后放弃并移出队列。
// 每次失败都会上报一次（带完整存档内容），exec 参数便于测试注入失败。
func (that *MySQLStore) flushItems(q *flushQueue, items []*pendingItem, exec func(*pendingItem) error) {
	for _, item := range items {
		item.attempts++
		err := exec(item)
		if err == nil {
			q.settle(item)
			continue
		}

		giveUp := item.attempts > maxFlushRetries
		reportArchiveError(archiveErrorOf(item, err, giveUp))
		if giveUp {
			// 移出队列：数据不会再落库，只剩刚刚打进日志的那一份
			q.settle(item)
			continue
		}
		q.backoff(item, time.Now().Add(that.retryBackoff(item.attempts)))
	}
}

// retryBackoff 按已尝试次数做指数退避：FlushInterval, 2x, 4x ... 封顶 maxRetryBackoff。
func (that *MySQLStore) retryBackoff(attempts int) time.Duration {
	d := that.flushInterval
	if d <= 0 {
		d = 500 * time.Millisecond
	}
	for i := 1; i < attempts && d < maxRetryBackoff; i++ {
		d *= 2
	}
	if d > maxRetryBackoff {
		return maxRetryBackoff
	}
	return d
}

// archiveErrorOf 由待刷盘条目构造一条失败事件。
func archiveErrorOf(item *pendingItem, err error, dropped bool) ArchiveError {
	return ArchiveError{
		Table:   item.tableName,
		PK:      item.snapshot[pkIndex(item.meta)],
		Deleted: item.deleted,
		Attempt: item.attempts,
		Dropped: dropped,
		Columns: columnsOf(item.meta, item.snapshot),
		Err:     err,
	}
}

// columnsOf 把字段快照还原成"列名 -> 值"。
// 值就是当初要绑定给 SQL 的参数，因此日志里这份内容可以直接拿来重建 INSERT。
// 只在存档失败时构造，不在正常刷盘路径上。
func columnsOf(meta *TableMeta, snap []any) map[string]any {
	cols := make(map[string]any, len(meta.Fields))
	for i, f := range meta.Fields {
		if i >= len(snap) {
			break
		}
		cols[f.ColName] = snap[i]
	}
	return cols
}

// execItem 为单条 item 独立分配 5s context 并执行 SQL，
// 单次写操作超时不影响同批次其他条目。
// 返回的错误由调用方决定重试还是上报，这里不做处理。
func (that *MySQLStore) execItem(item *pendingItem) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if item.deleted {
		return that.execSoftDelete(ctx, item)
	}
	return that.execUpsert(ctx, item)
}

// execUpsert 执行 INSERT ... ON DUPLICATE KEY UPDATE（自动幂等）。
// 每次更新都会将 is_deleted 复位为 0，并刷新 update_time。
func (that *MySQLStore) execUpsert(ctx context.Context, item *pendingItem) error {
	fields := item.meta.Fields
	cols := make([]string, 0, len(fields)+3)
	placeholders := make([]string, 0, len(fields)+3)
	updates := make([]string, 0, len(fields))
	args := make([]any, 0, len(fields)+1)

	for i, f := range fields {
		cols = append(cols, f.ColName)
		placeholders = append(placeholders, "?")
		args = append(args, item.snapshot[i])
		if !f.IsPrimary {
			updates = append(updates, fmt.Sprintf("`%s`=VALUES(`%s`)", f.ColName, f.ColName))
		}
	}

	// 内置系统列值：插入时固定 is_deleted=0，创建时间/更新时间交给数据库当前时间。
	cols = append(cols, "is_deleted", "create_time", "update_time")
	placeholders = append(placeholders, "?", "NOW()", "NOW()")
	args = append(args, 0)

	// 更新时恢复软删除标记并刷新更新时间。
	updates = append(updates, "`is_deleted`=0", "`update_time`=NOW()")

	sql := fmt.Sprintf(
		"INSERT INTO `%s` (`%s`) VALUES (%s) ON DUPLICATE KEY UPDATE %s",
		item.tableName,
		strings.Join(cols, "`,`"),
		strings.Join(placeholders, ","),
		strings.Join(updates, ","),
	)
	// 游戏服务器不应因存档失败崩溃：把错误交回给刷盘循环，由它安排重试与上报
	_, err := that.pool.SelectMySQL(that.useGlobal).ExecContext(ctx, sql, args...)
	return err
}

// execSoftDelete 通过设置 is_deleted=1 实现软删除，同时刷新 update_time。
func (that *MySQLStore) execSoftDelete(ctx context.Context, item *pendingItem) error {
	pk := item.meta.PrimaryField
	pkVal := item.snapshot[pkIndex(item.meta)]

	sql := fmt.Sprintf(
		"UPDATE `%s` SET `is_deleted`=1, `update_time`=NOW() WHERE `%s`=? AND `is_deleted`=0",
		item.tableName, pk.ColName,
	)
	_, err := that.pool.SelectMySQL(that.useGlobal).ExecContext(ctx, sql, pkVal)
	return err
}

// rejectIfStopped 在存储已关闭时拒收写入请求并上报。
// 关闭后 worker 已经退出，继续入队等于静默丢档，必须让业务能感知到。
func (that *MySQLStore) rejectIfStopped(tableName string, meta *TableMeta, snap []any, deleted bool) bool {
	if !that.stopped.Load() {
		return false
	}
	reportArchiveError(ArchiveError{
		Table:   tableName,
		PK:      snap[pkIndex(meta)],
		Deleted: deleted,
		Attempt: 0,
		Dropped: true,
		Columns: columnsOf(meta, snap),
		Err:     errStoreStopped,
	})
	return true
}

// EnqueueSave 读取对象字段快照后入队，不阻塞调用方。
func (that *MySQLStore) EnqueueSave(tableName string, meta *TableMeta, base unsafe.Pointer) {
	that.EnqueueSaveSnapshot(tableName, meta, snapshotFields(meta, base))
}

// EnqueueSaveSnapshot 用调用方已经算好的字段快照入队，不阻塞调用方。
// Save() 走这条入口：快照在上层只生成一次，Redis 与 MySQL 共用同一份编码结果，
// 复杂字段因此只经过一次 sonic 编码。
// 入队后 snap 不再被任何一方写入，跨 goroutine 共享是安全的。
// workerIdx = hash(pk) % nWorker，保证同一对象始终进同一队列（顺序保证）。
func (that *MySQLStore) EnqueueSaveSnapshot(tableName string, meta *TableMeta, snap []any) {
	frozenMeta := freezeTableMeta(meta)
	pk := snap[pkIndex(frozenMeta)]
	if that.rejectIfStopped(tableName, frozenMeta, snap, false) {
		return
	}
	key := fmt.Sprintf("%s:%v", tableName, pk)
	idx := hashKey(key) % uint64(that.nWorker)
	that.queues[idx].push(&pendingItem{
		key:       key,
		tableName: tableName,
		meta:      frozenMeta,
		snapshot:  snap,
	})
}

// EnqueueDelete 将软删除请求入队。
func (that *MySQLStore) EnqueueDelete(tableName string, meta *TableMeta, base unsafe.Pointer) {
	frozenMeta := freezeTableMeta(meta)
	snap := snapshotFields(frozenMeta, base)
	pk := snap[pkIndex(frozenMeta)]
	if that.rejectIfStopped(tableName, frozenMeta, snap, true) {
		return
	}
	key := fmt.Sprintf("%s:%v", tableName, pk)
	idx := hashKey(key) % uint64(that.nWorker)
	that.queues[idx].push(&pendingItem{
		key:       key,
		tableName: tableName,
		meta:      frozenMeta,
		snapshot:  snap,
		deleted:   true,
	})
}

// snapshotFields 将对象当前字段值全量快照为 []any，避免后续对象被修改导致存档错乱。
func snapshotFields(meta *TableMeta, base unsafe.Pointer) []any {
	snap := make([]any, len(meta.Fields))
	for i, f := range meta.Fields {
		ptr := FieldPtr(base, f.Offset)
		snap[i] = readFieldValue(f, ptr)
	}
	return snap
}

// jsonValue 标记"已经完成 JSON 编码"的字段值。
// readFieldValue 对复杂类型编码一次后用它包装返回，下游（Redis Hash 字段表、
// MySQL 语句参数）凭类型断言即可识别并直接复用这串字节，无需再编码一次。
// 用独立类型而不是 string 做标记，是为了让"已编码"由类型系统保证，
// 避免在多处各维护一份"哪些 Kind 算复杂类型"的清单而产生漂移。
type jsonValue string

// Value 实现 driver.Valuer，使 jsonValue 可直接作为 SQL 参数传给 database/sql。
func (that jsonValue) Value() (driver.Value, error) { return string(that), nil }

// readFieldValue 通过 unsafe 指针读取字段值，返回适合 MySQL driver 的类型。
// 基本类型通过指针直接转型（零开销）；map/slice/array/struct 等复杂类型
// 用 sonic 序列化为 JSON 字符串，存入 JSON 列。
func readFieldValue(f *FieldMeta, ptr unsafe.Pointer) any {
	switch f.GoType.Kind() {
	case reflect.Int64:
		return *(*int64)(ptr)
	case reflect.Int32:
		return *(*int32)(ptr)
	case reflect.Int:
		return *(*int)(ptr)
	case reflect.Int8:
		return *(*int8)(ptr)
	case reflect.Int16:
		return *(*int16)(ptr)
	case reflect.Uint64:
		return *(*uint64)(ptr)
	case reflect.Uint32:
		return *(*uint32)(ptr)
	case reflect.Uint:
		return *(*uint)(ptr)
	case reflect.Float32:
		return *(*float32)(ptr)
	case reflect.Float64:
		return *(*float64)(ptr)
	case reflect.String:
		return *(*string)(ptr)
	case reflect.Bool:
		return *(*bool)(ptr)
	default:
		// map / slice / array / struct → JSON 字符串存入 JSON 列。
		// 这是复杂字段在整条存盘链路上唯一一次 JSON 编码，结果以 jsonValue 形式
		// 传出，Redis 写入侧直接复用，不再重复编码。
		v := reflect.NewAt(f.GoType, ptr).Elem().Interface()
		data, err := sonic.Marshal(v)
		if err != nil {
			fmt.Printf("[gameorm] marshal field %s error: %v\n", f.ColName, err)
			return nil
		}
		return jsonValue(data)
	}
}

// pkIndex 返回主键字段在 meta.Fields 中的下标。
func pkIndex(meta *TableMeta) int {
	for i, f := range meta.Fields {
		if f.IsPrimary {
			return i
		}
	}
	return 0
}

// Shutdown 等待所有异步 MySQL 写操作完成后停止 worker，适用于进程优雅退出场景。
// 若 worker 从未启动（未调用过 Save/Delete），此函数为空操作。
// 调用后 MySQLStore 停止，不可再次提交写入请求。
func Shutdown() {
	if globalMySQLStore != nil {
		globalMySQLStore.Stop()
	}
	if globalRegionMySQLStore != nil {
		globalRegionMySQLStore.Stop()
	}
}

// hashKey 使用 FNV-1a 对 key 做轻量哈希，用于分派 worker。
func hashKey(s string) uint64 {
	const offset64 uint64 = 14695981039346656037
	const prime64 uint64 = 1099511628211
	h := offset64
	for i := range len(s) {
		h ^= uint64(s[i])
		h *= prime64
	}
	return h
}
