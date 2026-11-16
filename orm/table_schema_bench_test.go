package orm

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// Retain the result so the benchmark includes producing an escaping snapshot.
var benchmarkSnapshotSink []any

// BenchmarkSnapshotFields measures only the six scalar fields used by SaveR.
// Metadata preparation is excluded; no Init/AutoMigrate or Redis calls are needed.
func BenchmarkSnapshotFields(b *testing.B) {
	u := &TestUser{
		UserId: 1234, UserName: "benchmark_user",
		Email: "benchmark@example.com", Age: 25, Score: 99.5, Status: 1,
	}
	meta := GetTableMeta(reflect.TypeOf(u))
	base := pointerOf(u)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkSnapshotSink = snapshotFields(meta, base)
	}
	b.StopTimer()
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "ops/s")
}

// setupRedisBenchmark excludes initialization, warmup and cleanup from timing.
// Each invocation owns a separate key; it never deletes existing test data.
func setupRedisBenchmark(b *testing.B) *TestUser {
	b.Helper()
	b.StopTimer()
	u := &TestUser{
		UserId: -time.Now().UnixNano(), UserName: "benchmark_user",
		Email: "benchmark@example.com", Age: 25, Score: 99.5, Status: 1,
	}
	u.Init()
	key := redisKey(u.Meta().TableName, u.UserId)
	b.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := GetPool().Redis.Del(ctx, key).Err(); err != nil {
			b.Errorf("cleanup Redis key: %v", err)
		}
	})
	u.SaveR()
	if err := u.LoadR(); err != nil {
		b.Fatalf("warmup LoadR: %v", err)
	}
	if u.UserName != "benchmark_user" || u.Age != 25 || u.Score != 99.5 {
		b.Fatal("Redis warmup returned unexpected values")
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.StartTimer()
	return u
}

// BenchmarkTableSchema_SaveR measures synchronous Hash writes plus TTL updates.
// SaveR has no error return: inspect its error logs as well as the final readback.
func BenchmarkTableSchema_SaveR(b *testing.B) {
	u := setupRedisBenchmark(b)
	for i := 0; i < b.N; i++ {
		u.Score = float64(i)
		u.SaveR()
	}
	b.StopTimer()
	want := u.Score
	if err := u.LoadR(); err != nil {
		b.Fatal(err)
	}
	if u.Score != want {
		b.Fatalf("saved score = %v, want %v", u.Score, want)
	}
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "ops/s")
}

// BenchmarkTableSchema_LoadR measures warm-key reads including field decoding.
func BenchmarkTableSchema_LoadR(b *testing.B) {
	u := setupRedisBenchmark(b)
	for i := 0; i < b.N; i++ {
		if err := u.LoadR(); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "ops/s")
}
