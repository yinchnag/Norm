package orm

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// DDLBuilder 根据 TableMeta 生成建表 DDL 并执行 AUTO MIGRATE。
// 策略：CREATE TABLE IF NOT EXISTS，再通过 INFORMATION_SCHEMA 补全缺失列（ADD COLUMN）。
// 生产环境建议在灰度期用 dry-run 模式先预览 SQL。
type DDLBuilder struct {
	db *sql.DB
}

func newDDLBuilder() *DDLBuilder {
	return newDDLBuilderWithDB(GetPool().SelectMySQL(false))
}

func newDDLBuilderWithDB(db *sql.DB) *DDLBuilder {
	return &DDLBuilder{db: db}
}

// AutoMigrate 确保表存在且所有字段列存在。
// 不会删除旧列，不会修改列类型（安全迁移原则）。
func (d *DDLBuilder) AutoMigrate(ctx context.Context, meta *TableMeta) error {
	if err := d.createTableIfNotExists(ctx, meta); err != nil {
		return err
	}
	if err := d.addMissingColumns(ctx, meta); err != nil {
		return err
	}
	return d.ensureIndexes(ctx, meta)
}

func (d *DDLBuilder) createTableIfNotExists(ctx context.Context, meta *TableMeta) error {
	colDefs := make([]string, 0, len(meta.Fields)+2)
	var pkCol string

	for _, f := range meta.Fields {
		colDefs = append(colDefs, buildColumnDef(f))
		if f.IsPrimary {
			pkCol = f.ColName
		}
	}
	// 内置系统列：软删除标记 + 创建时间 + 更新时间
	colDefs = append(colDefs, builtInColumnDefs()...)
	if pkCol != "" {
		colDefs = append(colDefs, fmt.Sprintf("PRIMARY KEY (`%s`)", pkCol))
		colDefs = append(colDefs, builtInIndexDefs()...)
		colDefs = append(colDefs, userIndexDefs(meta)...)
	}

	ddl := fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS `%s` (\n  %s\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='auto-created by gameorm'",
		meta.TableName,
		strings.Join(colDefs, ",\n  "),
	)
	_, err := d.db.ExecContext(ctx, ddl)
	return err
}

// addMissingColumns 使用 INFORMATION_SCHEMA 检查并补全缺失列。
func (d *DDLBuilder) addMissingColumns(ctx context.Context, meta *TableMeta) error {
	rows, err := d.db.QueryContext(ctx,
		"SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=?",
		meta.TableName,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	existing := make(map[string]struct{}, 16)
	for rows.Next() {
		var col string
		if err = rows.Scan(&col); err != nil {
			return err
		}
		existing[col] = struct{}{}
	}
	if err = rows.Err(); err != nil {
		return err
	}

	for _, f := range meta.Fields {
		if _, ok := existing[f.ColName]; ok {
			continue
		}
		alterSQL := fmt.Sprintf(
			"ALTER TABLE `%s` ADD COLUMN %s",
			meta.TableName, buildColumnDef(f),
		)
		if _, err = d.db.ExecContext(ctx, alterSQL); err != nil {
			return fmt.Errorf("addMissingColumns [%s.%s]: %w", meta.TableName, f.ColName, err)
		}
	}

	for col, def := range builtInColumnAddDefs() {
		if _, ok := existing[col]; ok {
			continue
		}
		alterSQL := fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN %s", meta.TableName, def)
		if _, err = d.db.ExecContext(ctx, alterSQL); err != nil {
			return fmt.Errorf("addMissingColumns [%s.%s]: %w", meta.TableName, col, err)
		}
	}
	return nil
}

func builtInColumnDefs() []string {
	return []string{
		"`is_deleted` TINYINT(1) NOT NULL DEFAULT 0 COMMENT '软删除标记：0-正常 1-已删除'",
		"`create_time` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间'",
		"`update_time` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间'",
	}
}

func builtInIndexDefs() []string {
	return []string{
		"INDEX `idx_is_deleted` (`is_deleted`)",
		"INDEX `idx_update_time` (`update_time`)",
	}
}

func builtInIndexSet() map[string]struct{} {
	return map[string]struct{}{
		"PRIMARY":         {},
		"idx_is_deleted":  {},
		"idx_update_time": {},
	}
}

func builtInColumnAddDefs() map[string]string {
	return map[string]string{
		"is_deleted":  "`is_deleted` TINYINT(1) NOT NULL DEFAULT 0 COMMENT '软删除标记：0-正常 1-已删除'",
		"create_time": "`create_time` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间'",
		"update_time": "`update_time` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间'",
	}
}

// collectUserIndexes 将字段上的 index tag 合并为索引名 -> 列名列表的 map。
// 匿名索引（index tag 为 ""）自动生成索引名 idx_{colName}。
func collectUserIndexes(meta *TableMeta) map[string][]string {
	indexes := make(map[string][]string)
	for _, f := range meta.Fields {
		for _, idxName := range f.Indexes {
			if idxName == "" {
				idxName = "idx_" + f.ColName
			}
			indexes[idxName] = append(indexes[idxName], f.ColName)
		}
	}
	return indexes
}

// userIndexDefs 返回建表 DDL 中用户自定义索引的 INDEX 定义字符串列表。
func userIndexDefs(meta *TableMeta) []string {
	idxMap := collectUserIndexes(meta)
	defs := make([]string, 0, len(idxMap))
	for name, cols := range idxMap {
		quoted := make([]string, len(cols))
		for i, c := range cols {
			quoted[i] = "`" + c + "`"
		}
		defs = append(defs, fmt.Sprintf("INDEX `%s` (%s)", name, strings.Join(quoted, ",")))
	}
	return defs
}

// ensureIndexes 将数据库中的用户索引与 struct tag 声明对齐：
// - 声明新增：创建索引
// - 声明删除：删除索引
// - 同名索引列变化：先删后建
func (d *DDLBuilder) ensureIndexes(ctx context.Context, meta *TableMeta) error {
	desired := collectUserIndexes(meta)
	existing, err := d.queryExistingUserIndexes(ctx, meta.TableName)
	if err != nil {
		return err
	}
	toDrop, toCreate := planUserIndexChanges(existing, desired)

	for _, name := range toDrop {
		sql := fmt.Sprintf("DROP INDEX `%s` ON `%s`", name, meta.TableName)
		if _, err = d.db.ExecContext(ctx, sql); err != nil {
			return fmt.Errorf("ensureIndexes drop [%s.%s]: %w", meta.TableName, name, err)
		}
	}

	for name, cols := range toCreate {
		quoted := make([]string, len(cols))
		for i, c := range cols {
			quoted[i] = "`" + c + "`"
		}
		sql := fmt.Sprintf("CREATE INDEX `%s` ON `%s` (%s)",
			name, meta.TableName, strings.Join(quoted, ","))
		if _, err = d.db.ExecContext(ctx, sql); err != nil {
			return fmt.Errorf("ensureIndexes [%s.%s]: %w", meta.TableName, name, err)
		}
	}
	return nil
}

func planUserIndexChanges(existing map[string][]string, desired map[string][]string) ([]string, map[string][]string) {
	toDrop := make([]string, 0)
	toCreate := make(map[string][]string)

	for name, cols := range existing {
		target, ok := desired[name]
		if !ok {
			toDrop = append(toDrop, name)
			continue
		}
		if !sameColumns(cols, target) {
			toDrop = append(toDrop, name)
			toCreate[name] = target
		}
	}

	for name, cols := range desired {
		if _, ok := existing[name]; ok {
			continue
		}
		toCreate[name] = cols
	}

	sort.Strings(toDrop)
	return toDrop, toCreate
}

func sameColumns(a []string, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (d *DDLBuilder) queryExistingUserIndexes(ctx context.Context, tableName string) (map[string][]string, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT INDEX_NAME, COLUMN_NAME FROM INFORMATION_SCHEMA.STATISTICS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=? ORDER BY INDEX_NAME, SEQ_IN_INDEX",
		tableName,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	builtIn := builtInIndexSet()
	existing := make(map[string][]string)
	for rows.Next() {
		var name string
		var column string
		if err = rows.Scan(&name, &column); err != nil {
			return nil, err
		}
		if _, skip := builtIn[name]; skip {
			continue
		}
		existing[name] = append(existing[name], column)
	}
	return existing, rows.Err()
}

// buildColumnDef 将 FieldMeta 转换为 SQL 列定义片段。
// 为了支持补列时的向后兼容性（已有记录为 NULL），自动补充合理的默认值。
func buildColumnDef(f *FieldMeta) string {
	sqlType := goTypeToMySQL(f)
	var sb strings.Builder
	fmt.Fprintf(&sb, "`%s` %s", f.ColName, sqlType)
	if f.NotNull || f.IsPrimary {
		sb.WriteString(" NOT NULL")
	} else {
		sb.WriteString(" NULL")
	}
	if f.IsPrimary && f.IsAutoInc {
		sb.WriteString(" AUTO_INCREMENT")
	}

	// 为非主键字段补充默认值（补列时兼容已有 NULL 记录）
	if !f.IsPrimary {
		defaultVal := getDefaultValue(f)
		if defaultVal != "" {
			fmt.Fprintf(&sb, " DEFAULT %s", defaultVal)
		}
	}

	if f.Comment != "" {
		fmt.Fprintf(&sb, " COMMENT '%s'", strings.ReplaceAll(f.Comment, "'", "\\'"))
	}
	return sb.String()
}

// getDefaultValue 根据字段类型返回合理的 SQL DEFAULT 值。
// 注意：TEXT/BLOB/JSON 等大对象类型在 MySQL 中不支持 DEFAULT，返回空字符串。
func getDefaultValue(f *FieldMeta) string {
	switch f.GoType.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return "0"
	case reflect.Bool:
		return "0"
	case reflect.String:
		// 仅对 VARCHAR（有明确长度）补充默认值
		// TEXT（Length == 0）不支持 DEFAULT 值
		if f.Length > 0 {
			return "''"
		}
		return ""
	}
	// 其他类型（如 time.Time）不设置默认值，保持 NULL
	return ""
}

// goTypeToMySQL 映射 Go 类型到 MySQL 列类型。
func goTypeToMySQL(f *FieldMeta) string {
	switch f.GoType.Kind() {
	case reflect.Int8:
		return "TINYINT"
	case reflect.Int16:
		return "SMALLINT"
	case reflect.Int32:
		return "INT"
	case reflect.Int, reflect.Int64:
		return "BIGINT"
	case reflect.Uint8:
		return "TINYINT UNSIGNED"
	case reflect.Uint16:
		return "SMALLINT UNSIGNED"
	case reflect.Uint32:
		return "INT UNSIGNED"
	case reflect.Uint, reflect.Uint64:
		return "BIGINT UNSIGNED"
	case reflect.Float32:
		return "FLOAT"
	case reflect.Float64:
		return "DOUBLE"
	case reflect.Bool:
		return "TINYINT(1)"
	case reflect.String:
		if f.Length > 0 {
			return fmt.Sprintf("VARCHAR(%d)", f.Length)
		}
		return "TEXT"
	default:
		return "JSON"
	}
}
