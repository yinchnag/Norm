# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Norm (GameORM) is a high-performance ORM framework for Go game servers. Core design: async MySQL writes via worker queues, sync Redis writes, soft delete everywhere, zero-reflection field access using unsafe pointers, and CRTP (`TableSchema[T]`) for type safety.

## Build & Test Commands

```bash
# Run all tests
go test ./...

# Run tests for a specific package
go test ./orm/...
go test ./config/...

# Run a single test
go test ./orm/... -run TestFieldMeta

# Build the example
go build ./example/...

# Run the performance benchmark
go run ./example/perf -config ./example/perf/config/orm.json -n 20000 -workers 32 -rounds 5 -report-out ./reports/run.json
```

No Makefile or custom build tooling -- standard `go build` / `go test`.

## Architecture

### Data Flow

```
Save() -> Redis Hash (sync) + MySQL async queue (EnqueueSave)
Load() -> Redis first -> miss: MySQL query -> write back to Redis
Delete() -> Redis delete + MySQL async soft delete (is_deleted=1)
FindAll() -> MySQL SELECT with auto is_deleted=0 filter
```

### Key Packages

- **`orm/`** -- the entire ORM framework lives here
  - `table_schema.go` -- CRTP base `TableSchema[T]` with Init/Save/Load/Delete/FindAll
  - `mysql_store.go` -- async write queue with N workers, dedup, batch flush via UPSERT
  - `redis_store.go` -- sync Redis Hash read/write using sonic JSON
  - `field_meta.go` -- struct tag parsing, `FieldMeta`/`TableMeta`, unsafe pointer offsets
  - `ddl_builder.go` -- AutoMigrate: CREATE TABLE IF NOT EXISTS + ALTER TABLE ADD COLUMN
  - `query_builder.go` -- generic `QueryBuilder[T]` with NULL-safe scanning via `sql.Null*` types
  - `pool.go` -- MySQL/Redis connection pool singleton (regional + optional global)
  - `errors.go` -- error Kind sentinels + `*Error` context carrier (`newError` / `withContext` / `KindOf`)
- **`config/`** -- JSON config loading (`ORMConfig` struct)

### MySQL Write Pipeline

Objects are routed to workers by `hash(pk) % nWorker` (guarantees ordering per key). Each worker has a dedup map -- multiple Saves for the same key collapse to one MySQL write. Workers flush every `FlushIntervalMs` (default 500ms) using `INSERT ... ON DUPLICATE KEY UPDATE`.

### System Auto-Columns

Every table gets `is_deleted`, `create_time`, `update_time` plus indexes `idx_is_deleted`, `idx_update_time` -- no need to declare these in structs.

## Coding Conventions

- **All field access via `FieldPtr(base, offset)` + type assertion** -- never `reflect.Value.Set`
- **`pointerOf(v any)`** extracts raw data pointer from interface without GC write barrier
- Complex types (map/slice/struct) serialize to JSON columns via `sonic.MarshalString`
- SQL scanning uses `sql.Null*` types (never raw `int64`/`string`) for NULL safety on schema evolution
- SQL parameters via `?` placeholders -- never `fmt.Sprintf` for SQL values
- Functions max 100 lines; must refactor at 150
- One main struct per file with matching `_test.go`
- `sync.Map` for metadata caching; `sync.Mutex` in flushQueue; `sync.Once` for worker startup
- No physical DELETE -- always soft delete; UPSERT resets `is_deleted=0`
- All outward-facing errors are `*Error` from `orm/errors.go` (Kind sentinel + Op/Table/Column/PK) -- never a bare `fmt.Errorf`
- Judge errors with `errors.Is(err, orm.ErrNotFound)` / `errors.As(err, &e)`; never match message text
- Only one `*Error` per chain: inner layers tag Kind + Column at the failure site, outer layers call `withContext` to fill Table/PK and prepend the op name -- never wrap an `*Error` inside another
- `Init()` panics on AutoMigrate failure (fast-fail); `Save` failures retry then surface via the `ArchiveError` callback; `Load` failures return error
- TEXT/BLOB/JSON columns must not have DEFAULT clauses
- Do not use `gorm` or other third-party ORMs
- Do not use `time.Sleep` or blocking waits in the MySQL write path

## Tech Stack

- Go 1.24, `go-sql-driver/mysql`, `go-redis/redis/v8`, `bytedance/sonic` (fast JSON)
- Config format: JSON (loaded via `config.LoadFromFile`)
