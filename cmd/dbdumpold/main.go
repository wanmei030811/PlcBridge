// dump 老 db（绕过 dbinspect 写死的路径）
package main

import (
	"database/sql"
	"fmt"
	"os"
	_ "modernc.org/sqlite"
)

func main() {
	p := os.Args[1]
	fmt.Printf("=== %s ===\n", p)
	db, err := sql.Open("sqlite", p)
	if err != nil { fmt.Println("open err:", err); return }
	defer db.Close()

	tables := []string{"accounts","channels","devices","sources","points","targets","target_points","tokens"}
	for _, t := range tables {
		rows, err := db.Query("SELECT * FROM " + t)
		if err != nil { fmt.Printf("  %s: ERR %v\n", t, err); continue }
		cols, _ := rows.Columns()
		vals := make([]any, len(cols))
		for i := range vals { vals[i] = new(any) }
		n := 0
		for rows.Next() {
			rows.Scan(vals...)
			parts := make([]string, len(cols))
			for i := range cols { parts[i] = fmt.Sprintf("%v", *(vals[i].(*any))) }
			fmt.Printf("  %s[%d]: %v\n", t, n, parts)
			n++
		}
		rows.Close()
		fmt.Printf("  %s total: %d\n", t, n)
	}
}
