package orm

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"testing"
	"time"
)

func TestWriteLifecycleDrainsAcceptedOperation(t *testing.T) {
	p := &Pool{}
	ctx, done, err := p.beginWrite(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() { p.stopWrites(); close(stopped) }()
	deadline := time.Now().Add(time.Second)
	for {
		p.writes.mu.Lock()
		closing := p.writes.closing
		p.writes.mu.Unlock()
		if closing {
			break
		}
		if time.Now().After(deadline) {
			done()
			t.Fatal("close did not start")
		}
		runtime.Gosched()
	}
	if _, _, err := p.beginWrite(context.Background()); !errors.Is(err, ErrStoreStopped) {
		t.Errorf("new write: %v", err)
	}
	select {
	case <-stopped:
		t.Error("closed before admitted write finished")
	default:
	}
	_, nestedDone, err := p.beginWrite(ctx)
	if err != nil {
		done()
		t.Fatal(err)
	}
	nestedDone()
	done()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("close did not finish")
	}
	p.stopWrites()
}

func TestClosedPoolRejectsSchemaAndRedisWrites(t *testing.T) {
	old := globalPool
	p := &Pool{}
	globalPool = p
	defer func() { globalPool = old }()
	// Bind without migration: no backend should be touched after closing.
	u := &TestUser{UserId: 1234}
	u.meta = GetTableMeta(reflect.TypeOf(u))
	u.selfPtr = pointerOf(u)
	p.stopWrites()
	for name, fn := range map[string]func() error{"Save": u.Save, "SaveR": u.SaveR, "Delete": u.Delete} {
		if err := fn(); !errors.Is(err, ErrStoreStopped) {
			t.Errorf("%s: %v", name, err)
		}
	}
	r := &RedisStore{pool: p}
	if err := r.SetFields(context.Background(), "test_user", 1234, u.meta, u.selfPtr); !errors.Is(err, ErrStoreStopped) {
		t.Fatal(err)
	}
	if err := r.Del(context.Background(), "test_user", 1234); !errors.Is(err, ErrStoreStopped) {
		t.Fatal(err)
	}
}
