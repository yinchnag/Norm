package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/norm/config"
	"github.com/norm/orm"
)

type PerfUser struct {
	orm.TableSchema[*PerfUser]
	UserID    int64          `orm:"primary,name:user_id,autoInc"`
	NickName  string         `orm:"name:nick_name,length:64,notNull"`
	Level     int            `orm:"name:level"`
	Score     float64        `orm:"name:score,index:idx_score"`
	Online    bool           `orm:"name:online"`
	Inventory map[string]int `orm:"name:inventory,comment:背包"`
}

type stats struct {
	Name   string
	Ops    int
	Total  time.Duration
	AvgUs  float64
	P50Us  float64
	P95Us  float64
	P99Us  float64
	QPS    float64
	ErrCnt int64
}

type runConfig struct {
	ConfigPath   string
	N            int
	Workers      int
	FlushWaitMs  int
	FindLimit    int
	FindRounds   int
	Cleanup      bool
	Rounds       int
	ReportOut    string
	GeneratedAt  time.Time
	TableName    string
	FlushWaitDur time.Duration
}

type roundResult struct {
	RoundID    int
	Items      []stats
	StageError map[string]stageErrorInfo
}

type stageErrorInfo struct {
	Total   int64
	ByType  map[string]int64
	Samples []string
}

type errorCollector struct {
	mu         sync.Mutex
	total      int64
	byType     map[string]int64
	samples    []string
	maxSamples int
}

type reportStage struct {
	Name         string           `json:"name"`
	Ops          int              `json:"ops"`
	ErrCnt       int64            `json:"err_cnt"`
	TotalMs      float64          `json:"total_ms"`
	QPS          float64          `json:"qps"`
	AvgUs        float64          `json:"avg_us"`
	P50Us        float64          `json:"p50_us"`
	P95Us        float64          `json:"p95_us"`
	P99Us        float64          `json:"p99_us"`
	ErrorTypes   map[string]int64 `json:"error_types,omitempty"`
	ErrorSamples []string         `json:"error_samples,omitempty"`
}

type reportRound struct {
	RoundID int           `json:"round_id"`
	Stages  []reportStage `json:"stages"`
}

type reportCompare struct {
	Stage         string           `json:"stage"`
	QPSAvg        float64          `json:"qps_avg"`
	QPSMin        float64          `json:"qps_min"`
	QPSMax        float64          `json:"qps_max"`
	QPSStd        float64          `json:"qps_std"`
	AvgUsAvg      float64          `json:"avg_us_avg"`
	AvgUsMin      float64          `json:"avg_us_min"`
	AvgUsMax      float64          `json:"avg_us_max"`
	AvgUsStd      float64          `json:"avg_us_std"`
	ErrTotal      int64            `json:"err_total"`
	ErrTypeTotals map[string]int64 `json:"err_type_totals,omitempty"`
}

type perfReport struct {
	GeneratedAt   string          `json:"generated_at"`
	ConfigPath    string          `json:"config_path"`
	TableName     string          `json:"table_name"`
	N             int             `json:"n"`
	Workers       int             `json:"workers"`
	Rounds        int             `json:"rounds"`
	FindLimit     int             `json:"find_limit"`
	FindRounds    int             `json:"find_rounds"`
	FlushWaitMs   int             `json:"flush_wait_ms"`
	FlushWaitMode string          `json:"flush_wait_mode"` // "auto" | "fixed"
	Cleanup       bool            `json:"cleanup"`
	RoundData     []reportRound   `json:"round_data"`
	CompareData   []reportCompare `json:"compare_data"`
}

func main() {
	cfgPath := flag.String("config", "example/perf/config/orm.json", "ORM 配置文件路径")
	n := flag.Int("n", 20000, "样本总数")
	workers := flag.Int("workers", runtime.NumCPU()*2, "并发 worker 数")
	flushWaitMs := flag.Int("flush-wait-ms", 0, "Save 后等待异步刷盘时间，0=自动推导")
	findLimit := flag.Int("find-limit", 1000, "FindAll 每轮 limit")
	findRounds := flag.Int("find-rounds", 10, "FindAll 轮次")
	cleanup := flag.Bool("cleanup", true, "压测前是否清空测试数据")
	rounds := flag.Int("rounds", 1, "压测轮次（>1 时启用多轮对比模式）")
	reportOut := flag.String("report-out", "", "报告输出路径（默认自动生成到 example/perf/reports）")
	flag.Parse()

	rc := runConfig{
		ConfigPath:  *cfgPath,
		N:           *n,
		Workers:     *workers,
		FlushWaitMs: *flushWaitMs,
		FindLimit:   *findLimit,
		FindRounds:  *findRounds,
		Cleanup:     *cleanup,
		Rounds:      *rounds,
		ReportOut:   *reportOut,
		GeneratedAt: time.Now(),
	}
	validateConfig(rc)

	if err := orm.InitPool(rc.ConfigPath); err != nil {
		panic(err)
	}
	defer orm.Shutdown()

	seed := &PerfUser{UserID: 1}
	seed.Init()
	rc.TableName = seed.Meta().TableName
	rc.FlushWaitDur = calcFlushWait(rc.ConfigPath, rc.FlushWaitMs)

	results := runAllRounds(seed, &rc)
	printRoundSummary(results)
	printCompareSummary(results)
	path, err := writeJSONReport(rc, results)
	if err != nil {
		fmt.Printf("[perf] write report failed: %v\n", err)
		return
	}
	fmt.Printf("[perf] report generated: %s\n", path)
}

func validateConfig(rc runConfig) {
	if rc.N <= 0 {
		panic("-n must be > 0")
	}
	if rc.Workers <= 0 {
		panic("-workers must be > 0")
	}
	if rc.FindLimit <= 0 {
		panic("-find-limit must be > 0")
	}
	if rc.FindRounds <= 0 {
		panic("-find-rounds must be > 0")
	}
	if rc.Rounds <= 0 {
		panic("-rounds must be > 0")
	}
}

func runAllRounds(seed *PerfUser, rc *runConfig) []roundResult {
	results := make([]roundResult, 0, rc.Rounds)
	fmt.Printf("[perf] table=%s n=%d workers=%d rounds=%d\n", rc.TableName, rc.N, rc.Workers, rc.Rounds)

	for r := 1; r <= rc.Rounds; r++ {
		fmt.Printf("[perf] ===== Round %d/%d =====\n", r, rc.Rounds)
		if rc.Cleanup {
			truncatePerfTable(rc.TableName)
			if err := purgeRedisByTable(rc.TableName); err != nil {
				fmt.Printf("[perf] purge redis warning: %v\n", err)
			}
		}
		roundStats, stageErr := runOneRound(seed, *rc)
		results = append(results, roundResult{RoundID: r, Items: roundStats, StageError: stageErr})
	}
	return results
}

func runOneRound(seed *PerfUser, rc runConfig) ([]stats, map[string]stageErrorInfo) {
	items := make([]stats, 0, 4)
	stageErr := make(map[string]stageErrorInfo)

	saveStats := runConcurrent("Save", rc.N, rc.Workers, nil, func(id int) error {
		u := newPerfUser(id)
		u.Init()
		u.Save()
		return nil
	})
	printStats(saveStats)
	items = append(items, saveStats)

	if rc.FlushWaitMs > 0 {
		// 显式指定等待时长（用于刻意测试刷盘竞态）
		fmt.Printf("[perf] waiting async flush (fixed): %s\n", rc.FlushWaitDur)
		time.Sleep(rc.FlushWaitDur)
	} else {
		// 自动模式：轮询 MySQL 行数，达到 N 行后继续
		waitForMySQLFlush(rc.TableName, rc.N, 60*time.Second)
	}

	loadRedisStats := runConcurrent("Load(redis-hit)", rc.N, rc.Workers, nil, func(id int) error {
		u := &PerfUser{UserID: int64(id)}
		u.Init()
		return u.Load()
	})
	printStats(loadRedisStats)
	items = append(items, loadRedisStats)

	if err := purgeRedisByTable(rc.TableName); err != nil {
		fmt.Printf("[perf] purge redis warning: %v\n", err)
	}

	loadMissErrCollector := newErrorCollector(12)
	loadMySQLStats := runConcurrent("Load(redis-miss->mysql)", rc.N, rc.Workers, loadMissErrCollector, func(id int) error {
		u := &PerfUser{UserID: int64(id)}
		u.Init()
		return u.Load()
	})
	printStats(loadMySQLStats)
	items = append(items, loadMySQLStats)
	if loadMySQLStats.ErrCnt > 0 {
		errInfo := loadMissErrCollector.snapshot()
		stageErr["Load(redis-miss->mysql)"] = errInfo
		printErrorBreakdown("Load(redis-miss->mysql)", errInfo)
	}

	findStats := runFindAll(seed, rc.FindLimit, rc.FindRounds)
	printStats(findStats)
	items = append(items, findStats)

	return items, stageErr
}

func newPerfUser(id int) *PerfUser {
	return &PerfUser{
		UserID:   int64(id),
		NickName: fmt.Sprintf("u_%d", id),
		Level:    (id % 100) + 1,
		Score:    float64((id%1000)+1) * 1.25,
		Online:   id%2 == 0,
		Inventory: map[string]int{
			"gold":   id * 3,
			"gem":    id % 20,
			"potion": (id % 10) + 1,
		},
	}
}

func runConcurrent(name string, n, workers int, collector *errorCollector, fn func(id int) error) stats {
	latencyUs := make([]int64, n)
	jobs := make(chan int, workers*2)
	var wg sync.WaitGroup
	var errCnt int64
	start := time.Now()

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				opStart := time.Now()
				if err := fn(id); err != nil {
					atomic.AddInt64(&errCnt, 1)
					if collector != nil {
						collector.add(err)
					}
				}
				latencyUs[id-1] = time.Since(opStart).Microseconds()
			}
		}()
	}

	for i := 1; i <= n; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	total := time.Since(start)

	return buildStats(name, n, total, latencyUs, errCnt)
}

func runFindAll(seed *PerfUser, limit, rounds int) stats {
	latencyUs := make([]int64, rounds)
	var errCnt int64
	start := time.Now()

	for i := 0; i < rounds; i++ {
		opStart := time.Now()
		_, err := seed.FindAll("level > 10", "score DESC", limit)
		if err != nil {
			errCnt++
		}
		latencyUs[i] = time.Since(opStart).Microseconds()
	}

	total := time.Since(start)
	return buildStats("FindAll", rounds, total, latencyUs, errCnt)
}

func buildStats(name string, n int, total time.Duration, latencyUs []int64, errCnt int64) stats {
	sort.Slice(latencyUs, func(i, j int) bool { return latencyUs[i] < latencyUs[j] })
	var sum int64
	for _, us := range latencyUs {
		sum += us
	}
	avgUs := float64(sum) / float64(n)
	qps := float64(n) / total.Seconds()

	return stats{
		Name:   name,
		Ops:    n,
		Total:  total,
		AvgUs:  avgUs,
		P50Us:  percentile(latencyUs, 0.50),
		P95Us:  percentile(latencyUs, 0.95),
		P99Us:  percentile(latencyUs, 0.99),
		QPS:    qps,
		ErrCnt: errCnt,
	}
}

func percentile(sorted []int64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(float64(len(sorted))*p)) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return float64(sorted[idx])
}

func printStats(s stats) {
	fmt.Printf("[perf] %-22s ops=%d err=%d total=%s qps=%.2f avg=%.2fus p50=%.2fus p95=%.2fus p99=%.2fus\n",
		s.Name, s.Ops, s.ErrCnt, s.Total, s.QPS, s.AvgUs, s.P50Us, s.P95Us, s.P99Us)
}

func printErrorBreakdown(stage string, info stageErrorInfo) {
	if info.Total == 0 {
		return
	}
	fmt.Printf("[perf]   %s error breakdown: %v\n", stage, info.ByType)
	for i, s := range info.Samples {
		fmt.Printf("[perf]   %s sample[%d]: %s\n", stage, i+1, s)
	}
}

func printRoundSummary(results []roundResult) {
	for _, rr := range results {
		fmt.Printf("[perf] Round %d summary\n", rr.RoundID)
		for _, s := range rr.Items {
			fmt.Printf("[perf]   %-22s qps=%10.2f avg=%10.2fus err=%d\n", s.Name, s.QPS, s.AvgUs, s.ErrCnt)
		}
	}
}

func printCompareSummary(results []roundResult) {
	if len(results) <= 1 {
		fmt.Printf("[perf] compare summary skipped: rounds=%d\n", len(results))
		return
	}
	group := groupByName(results)
	fmt.Printf("[perf] ===== Multi-round Compare =====\n")
	for _, name := range sortedNames(group) {
		items := group[name]
		qpsVals := pickFloat(items, func(s stats) float64 { return s.QPS })
		avgVals := pickFloat(items, func(s stats) float64 { return s.AvgUs })
		errVals := pickInt64(items, func(s stats) int64 { return s.ErrCnt })
		fmt.Printf("[perf] %-22s qps(avg/min/max/std)=%.2f/%.2f/%.2f/%.2f avg_us(avg/min/max/std)=%.2f/%.2f/%.2f/%.2f err(total)=%d\n",
			name,
			mean(qpsVals), minFloat(qpsVals), maxFloat(qpsVals), stddev(qpsVals),
			mean(avgVals), minFloat(avgVals), maxFloat(avgVals), stddev(avgVals),
			sumInt64(errVals),
		)
	}
}

func groupByName(results []roundResult) map[string][]stats {
	out := make(map[string][]stats)
	for _, rr := range results {
		for _, s := range rr.Items {
			out[s.Name] = append(out[s.Name], s)
		}
	}
	return out
}

func sortedNames(group map[string][]stats) []string {
	names := make([]string, 0, len(group))
	for name := range group {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func pickFloat(items []stats, fn func(stats) float64) []float64 {
	vals := make([]float64, 0, len(items))
	for _, s := range items {
		vals = append(vals, fn(s))
	}
	return vals
}

func pickInt64(items []stats, fn func(stats) int64) []int64 {
	vals := make([]int64, 0, len(items))
	for _, s := range items {
		vals = append(vals, fn(s))
	}
	return vals
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	var sum float64
	for _, x := range v {
		sum += x
	}
	return sum / float64(len(v))
}

func stddev(v []float64) float64 {
	if len(v) <= 1 {
		return 0
	}
	m := mean(v)
	var s float64
	for _, x := range v {
		d := x - m
		s += d * d
	}
	return math.Sqrt(s / float64(len(v)))
}

func minFloat(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	m := v[0]
	for _, x := range v[1:] {
		if x < m {
			m = x
		}
	}
	return m
}

func maxFloat(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	m := v[0]
	for _, x := range v[1:] {
		if x > m {
			m = x
		}
	}
	return m
}

func sumInt64(v []int64) int64 {
	var sum int64
	for _, x := range v {
		sum += x
	}
	return sum
}

func writeJSONReport(rc runConfig, results []roundResult) (string, error) {
	path := rc.ReportOut
	if path == "" {
		ts := rc.GeneratedAt.Format("20060102_150405")
		path = filepath.Join("example", "perf", "reports", "perf_report_"+ts+".json")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}

	report := buildJSONReport(rc, results)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func buildJSONReport(rc runConfig, results []roundResult) perfReport {
	flushMode := "auto"
	if rc.FlushWaitMs > 0 {
		flushMode = "fixed"
	}
	report := perfReport{
		GeneratedAt:   rc.GeneratedAt.Format(time.RFC3339),
		ConfigPath:    rc.ConfigPath,
		TableName:     rc.TableName,
		N:             rc.N,
		Workers:       rc.Workers,
		Rounds:        rc.Rounds,
		FindLimit:     rc.FindLimit,
		FindRounds:    rc.FindRounds,
		FlushWaitMs:   rc.FlushWaitMs,
		FlushWaitMode: flushMode,
		Cleanup:       rc.Cleanup,
		RoundData:     make([]reportRound, 0, len(results)),
		CompareData:   make([]reportCompare, 0),
	}

	for _, rr := range results {
		round := reportRound{RoundID: rr.RoundID, Stages: make([]reportStage, 0, len(rr.Items))}
		for _, s := range rr.Items {
			errInfo := rr.StageError[s.Name]
			round.Stages = append(round.Stages, reportStage{
				Name:         s.Name,
				Ops:          s.Ops,
				ErrCnt:       s.ErrCnt,
				TotalMs:      float64(s.Total) / float64(time.Millisecond),
				QPS:          s.QPS,
				AvgUs:        s.AvgUs,
				P50Us:        s.P50Us,
				P95Us:        s.P95Us,
				P99Us:        s.P99Us,
				ErrorTypes:   cloneErrMap(errInfo.ByType),
				ErrorSamples: append([]string{}, errInfo.Samples...),
			})
		}
		report.RoundData = append(report.RoundData, round)
	}

	group := groupByName(results)
	for _, name := range sortedNames(group) {
		items := group[name]
		qpsVals := pickFloat(items, func(s stats) float64 { return s.QPS })
		avgVals := pickFloat(items, func(s stats) float64 { return s.AvgUs })
		errVals := pickInt64(items, func(s stats) int64 { return s.ErrCnt })
		errTypeTotals := mergeStageErrTypes(results, name)
		report.CompareData = append(report.CompareData, reportCompare{
			Stage:         name,
			QPSAvg:        mean(qpsVals),
			QPSMin:        minFloat(qpsVals),
			QPSMax:        maxFloat(qpsVals),
			QPSStd:        stddev(qpsVals),
			AvgUsAvg:      mean(avgVals),
			AvgUsMin:      minFloat(avgVals),
			AvgUsMax:      maxFloat(avgVals),
			AvgUsStd:      stddev(avgVals),
			ErrTotal:      sumInt64(errVals),
			ErrTypeTotals: errTypeTotals,
		})
	}

	return report
}

func truncatePerfTable(table string) {
	db := orm.GetPool().DB
	query := fmt.Sprintf("DELETE FROM `%s`", table)
	if _, err := db.Exec(query); err != nil {
		panic(fmt.Errorf("cleanup table failed: %w", err))
	}
}

func purgeRedisByTable(table string) error {
	ctx := context.Background()
	client := orm.GetPool().Redis
	pattern := table + ":*"
	var cursor uint64

	for {
		keys, next, err := client.Scan(ctx, cursor, pattern, 1000).Result()
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			if err := client.Del(ctx, keys...).Err(); err != nil {
				return err
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return nil
}

func calcFlushWait(cfgPath string, explicitMs int) time.Duration {
	if explicitMs > 0 {
		return time.Duration(explicitMs) * time.Millisecond
	}
	cfg, err := config.LoadFromFile(cfgPath)
	if err != nil {
		return 2 * time.Second
	}
	interval := time.Duration(cfg.FlushIntervalMs) * time.Millisecond
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	return interval * 3
}

// waitForMySQLFlush 轮询 MySQL 行数，直到 count >= expected 或超时。
// 用于替代固定等待，确保异步刷盘真正完成后再开始 redis-miss 阶段。
func waitForMySQLFlush(tableName string, expected int, timeout time.Duration) {
	const pollInterval = 200 * time.Millisecond
	const reportInterval = 2 * time.Second
	const stallThreshold = 5 * time.Second

	db := orm.GetPool().DB
	// 表名来自 ORM 元数据，非用户输入；反引号转义防止保留字冲突
	query := fmt.Sprintf("SELECT COUNT(*) FROM `%s` WHERE is_deleted=0", tableName)
	deadline := time.Now().Add(timeout)
	lastReport := time.Now()
	lastChange := time.Now()
	lastCount := -1
	start := time.Now()

	fmt.Printf("[perf] waiting MySQL flush (auto): expected=%d ...\n", expected)
	for {
		var count int
		if err := db.QueryRow(query).Scan(&count); err != nil {
			fmt.Printf("[perf] waitForMySQLFlush scan error: %v\n", err)
		}
		if count >= expected {
			fmt.Printf("[perf] MySQL flush done: count=%d/%d elapsed=%s\n", count, expected, time.Since(start))
			return
		}
		if count != lastCount {
			lastCount = count
			lastChange = time.Now()
		}
		if time.Since(lastChange) >= stallThreshold {
			fmt.Printf("[perf] WARNING: MySQL flush stalled at count=%d/%d elapsed=%s, proceeding\n", count, expected, time.Since(start))
			return
		}
		if time.Now().After(deadline) {
			fmt.Printf("[perf] WARNING: MySQL flush timeout! count=%d/%d elapsed=%s\n", count, expected, time.Since(start))
			return
		}
		if time.Since(lastReport) >= reportInterval {
			fmt.Printf("[perf] MySQL flush progress: count=%d/%d elapsed=%s\n", count, expected, time.Since(start))
			lastReport = time.Now()
		}
		time.Sleep(pollInterval)
	}
}

func newErrorCollector(maxSamples int) *errorCollector {
	if maxSamples <= 0 {
		maxSamples = 8
	}
	return &errorCollector{
		byType:     make(map[string]int64),
		maxSamples: maxSamples,
	}
}

func (c *errorCollector) add(err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total++
	typ := classifyError(err)
	c.byType[typ]++
	if len(c.samples) < c.maxSamples {
		c.samples = append(c.samples, err.Error())
	}
}

func (c *errorCollector) snapshot() stageErrorInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return stageErrorInfo{
		Total:   c.total,
		ByType:  cloneErrMap(c.byType),
		Samples: append([]string{}, c.samples...),
	}
}

func classifyError(err error) string {
	msg := strings.ToLower(err.Error())
	switch {
	case errors.Is(err, sql.ErrNoRows) || strings.Contains(msg, "no rows"):
		return "mysql_not_found"
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline"):
		return "timeout"
	case strings.Contains(msg, "connection") || strings.Contains(msg, "broken pipe"):
		return "connection_error"
	case strings.Contains(msg, "redis"):
		return "redis_error"
	case strings.Contains(msg, "mysql"):
		return "mysql_error"
	case strings.Contains(msg, "scan") || strings.Contains(msg, "unmarshal"):
		return "decode_scan_error"
	default:
		return "other"
	}
}

func cloneErrMap(src map[string]int64) map[string]int64 {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]int64, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func mergeStageErrTypes(results []roundResult, stageName string) map[string]int64 {
	out := make(map[string]int64)
	for _, rr := range results {
		errInfo, ok := rr.StageError[stageName]
		if !ok {
			continue
		}
		for k, v := range errInfo.ByType {
			out[k] += v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
