package orm

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"
)

var benchmarkRedisHashFieldsSink map[string]interface{}

// BenchmarkRedisPing measures one synchronous round trip on the shared client.
func BenchmarkRedisPing(b *testing.B) {
	ctx := context.Background()
	client := GetPool().Redis
	if err := client.Ping(ctx).Err(); err != nil {
		b.Fatalf("warmup PING: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := client.Ping(ctx).Err(); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "ops/s")
}

// BenchmarkRedisHSet measures a single HSET, including command construction,
// but excludes field encoding. TTL is set once during setup, outside timing.
func BenchmarkRedisHSet(b *testing.B) {
	ctx := context.Background()
	client := GetPool().Redis
	key := fmt.Sprintf("norm:bench:hset:%d", time.Now().UnixNano())
	u := &TestUser{
		UserId: 1234, UserName: "benchmark_user",
		Email: "benchmark@example.com", Age: 25, Score: 99.5, Status: 1,
	}
	meta := GetTableMeta(reflect.TypeOf(u))
	fields, err := buildRedisHashFieldsFromSnapshot(meta, snapshotFields(meta, pointerOf(u)))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.Del(ctx, key).Err(); err != nil {
			b.Errorf("cleanup Redis key: %v", err)
		}
	})
	if err := client.HSet(ctx, key, fields).Err(); err != nil {
		b.Fatalf("warmup HSET: %v", err)
	}
	if err := client.Expire(ctx, key, time.Hour).Err(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := client.HSet(ctx, key, fields).Err(); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "ops/s")
}

// BenchmarkRedisTxPipelineExec measures Exec only: connection acquisition,
// protocol I/O and response parsing for the same HSET + EXPIRE as SaveR.
func BenchmarkRedisTxPipelineExec(b *testing.B) {
	b.StopTimer()
	ctx := context.Background()
	client := GetPool().Redis
	key := fmt.Sprintf("norm:bench:txexec:%d", time.Now().UnixNano())
	u := &TestUser{
		UserId: 1234, UserName: "benchmark_user",
		Email: "benchmark@example.com", Age: 25, Score: 99.5, Status: 1,
	}
	meta := GetTableMeta(reflect.TypeOf(u))
	fields, err := buildRedisHashFieldsFromSnapshot(meta, snapshotFields(meta, pointerOf(u)))
	if err != nil {
		b.Fatal(err)
	}
	ttl := time.Duration(GetPool().Cfg.GetRedisKeyTTLSec()) * time.Second
	b.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.Del(ctx, key).Err(); err != nil {
			b.Errorf("cleanup Redis key: %v", err)
		}
	})
	pipe := client.TxPipeline()
	defer pipe.Close()
	pipe.HSet(ctx, key, fields)
	pipe.Expire(ctx, key, ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		b.Fatalf("warmup Exec: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Exec clears the queue, so enqueue fresh commands outside timing.
		pipe.HSet(ctx, key, fields)
		pipe.Expire(ctx, key, ttl)
		b.StartTimer()
		_, err := pipe.Exec(ctx)
		b.StopTimer()
		if err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "ops/s")
}

// BenchmarkBuildRedisHashFieldsFromSnapshot measures field encoding and Hash
// construction only. Metadata, snapshots and codec warmup are outside timing.
func BenchmarkBuildRedisHashFieldsFromSnapshot(b *testing.B) {
	u := &TestUser{
		UserId: 1234, UserName: "benchmark_user",
		Email: "benchmark@example.com", Age: 25, Score: 99.5, Status: 1,
	}
	meta := GetTableMeta(reflect.TypeOf(u))
	snap := snapshotFields(meta, pointerOf(u))
	fields, err := buildRedisHashFieldsFromSnapshot(meta, snap)
	if err != nil {
		b.Fatal(err)
	}
	if len(fields) != 6 || fields["user_name"] != `"benchmark_user"` {
		b.Fatal("unexpected encoded Hash fields")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fields, err := buildRedisHashFieldsFromSnapshot(meta, snap)
		if err != nil {
			b.Fatal(err)
		}
		benchmarkRedisHashFieldsSink = fields
	}
	b.StopTimer()
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "ops/s")
}
