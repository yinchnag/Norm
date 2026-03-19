package orm

import (
	"reflect"
	"strings"
	"testing"
)

func TestBuildColumnDef(t *testing.T) {
	cases := []struct {
		fm   *FieldMeta
		want string
	}{
		{
			&FieldMeta{ColName: "user_id", GoType: reflect.TypeOf(int64(0)), IsPrimary: true, IsAutoInc: true, Comment: "用户ID"},
			"`user_id` BIGINT NOT NULL AUTO_INCREMENT COMMENT '用户ID'",
		},
		{
			// VARCHAR with length limit → has DEFAULT ''
			&FieldMeta{ColName: "name", GoType: reflect.TypeOf(""), Length: 100, NotNull: true},
			"`name` VARCHAR(100) NOT NULL DEFAULT ''",
		},
		{
			// float64 → has DEFAULT 0
			&FieldMeta{ColName: "score", GoType: reflect.TypeOf(float64(0))},
			"`score` DOUBLE NULL DEFAULT 0",
		},
		{
			// string without length limit (TEXT) → no DEFAULT
			&FieldMeta{ColName: "desc", GoType: reflect.TypeOf("")},
			"`desc` TEXT NULL",
		},
	}
	for _, c := range cases {
		got := buildColumnDef(c.fm)
		if got != c.want {
			t.Errorf("buildColumnDef:\n got  %q\n want %q", got, c.want)
		}
	}
}

func TestGoTypeToMySQL(t *testing.T) {
	cases := map[reflect.Kind]string{
		reflect.Int64:   "BIGINT",
		reflect.Int32:   "INT",
		reflect.Int8:    "TINYINT",
		reflect.Float64: "DOUBLE",
		reflect.Bool:    "TINYINT(1)",
		reflect.String:  "TEXT",
	}
	for k, want := range cases {
		fm := &FieldMeta{GoType: reflect.New(reflect.ArrayOf(0, reflect.TypeOf(0))).Type()}
		// 伪造 kind 通过构造特定类型
		fm.GoType = kindToType(k)
		got := goTypeToMySQL(fm)
		if got != want {
			t.Errorf("kind=%v: got %q want %q", k, got, want)
		}
	}
}

func kindToType(k reflect.Kind) reflect.Type {
	switch k {
	case reflect.Int64:
		return reflect.TypeOf(int64(0))
	case reflect.Int32:
		return reflect.TypeOf(int32(0))
	case reflect.Int8:
		return reflect.TypeOf(int8(0))
	case reflect.Float64:
		return reflect.TypeOf(float64(0))
	case reflect.Bool:
		return reflect.TypeOf(false)
	case reflect.String:
		return reflect.TypeOf("")
	}
	return reflect.TypeOf(0)
}

func TestBuiltInColumnDefs(t *testing.T) {
	defs := builtInColumnDefs()
	if len(defs) != 3 {
		t.Fatalf("expected 3 built-in defs, got %d", len(defs))
	}
	joined := strings.Join(defs, ",")
	if !strings.Contains(joined, "`is_deleted`") {
		t.Error("built-in defs should contain is_deleted")
	}
	if !strings.Contains(joined, "`create_time`") {
		t.Error("built-in defs should contain create_time")
	}
	if !strings.Contains(joined, "`update_time`") {
		t.Error("built-in defs should contain update_time")
	}
}

func TestPlanUserIndexChangesDropAndCreate(t *testing.T) {
	existing := map[string][]string{
		"idx_level_score": {"level", "score"},
	}
	desired := map[string][]string{
		"idx_score": {"score"},
	}

	toDrop, toCreate := planUserIndexChanges(existing, desired)

	if len(toDrop) != 1 || toDrop[0] != "idx_level_score" {
		t.Fatalf("unexpected toDrop: %+v", toDrop)
	}
	cols, ok := toCreate["idx_score"]
	if !ok {
		t.Fatal("idx_score should be created")
	}
	if len(cols) != 1 || cols[0] != "score" {
		t.Fatalf("unexpected idx_score cols: %+v", cols)
	}
}

func TestPlanUserIndexChangesRecreateWhenColumnChanged(t *testing.T) {
	existing := map[string][]string{
		"idx_a": {"a", "b"},
	}
	desired := map[string][]string{
		"idx_a": {"b", "a"},
	}

	toDrop, toCreate := planUserIndexChanges(existing, desired)

	if len(toDrop) != 1 || toDrop[0] != "idx_a" {
		t.Fatalf("unexpected toDrop: %+v", toDrop)
	}
	cols, ok := toCreate["idx_a"]
	if !ok {
		t.Fatal("idx_a should be recreated")
	}
	if len(cols) != 2 || cols[0] != "b" || cols[1] != "a" {
		t.Fatalf("unexpected recreated idx_a cols: %+v", cols)
	}
}

func TestPlanUserIndexChangesNoChange(t *testing.T) {
	existing := map[string][]string{
		"idx_score": {"score"},
	}
	desired := map[string][]string{
		"idx_score": {"score"},
	}

	toDrop, toCreate := planUserIndexChanges(existing, desired)

	if len(toDrop) != 0 {
		t.Fatalf("expected no drop, got %+v", toDrop)
	}
	if len(toCreate) != 0 {
		t.Fatalf("expected no create, got %+v", toCreate)
	}
}
