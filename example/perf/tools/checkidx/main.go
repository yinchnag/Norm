package main

import (
	"database/sql"
	"fmt"
	"strings"

	_ "github.com/go-sql-driver/mysql"
	"github.com/norm/config"
)

func main() {
	cfg, err := config.LoadFromFile("example/perf/config/orm.json")
	if err != nil {
		panic(err)
	}
	dsn := cfg.MySQL.DSN
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		panic(err)
	}

	// 查看所有索引
	fmt.Println("=== SHOW INDEX FROM perf_user ===")
	rows, _ := db.Query("SHOW INDEX FROM perf_user")
	cols, _ := rows.Columns()
	for rows.Next() {
		vals := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		_ = rows.Scan(ptrs...)
		parts := make([]string, len(cols))
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				parts[i] = string(b)
			} else {
				parts[i] = fmt.Sprintf("%v", v)
			}
		}
		fmt.Println(" ", strings.Join(parts, " | "))
	}

	// EXPLAIN
	query := "EXPLAIN SELECT user_id,nick_name,level,score,online,inventory FROM `perf_user` WHERE (`is_deleted`=0) AND (level > 10) ORDER BY score DESC LIMIT 20"
	fmt.Println("\n=== EXPLAIN ===")
	fmt.Println("SQL:", query)
	erows, _ := db.Query(query)
	ecols, _ := erows.Columns()
	fmt.Println(ecols)
	for erows.Next() {
		vals := make([]interface{}, len(ecols))
		ptrs := make([]interface{}, len(ecols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		_ = erows.Scan(ptrs...)
		parts := make([]string, len(ecols))
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				parts[i] = string(b)
			} else {
				parts[i] = fmt.Sprintf("%v", v)
			}
		}
		fmt.Println(strings.Join(parts, " | "))
	}
}
