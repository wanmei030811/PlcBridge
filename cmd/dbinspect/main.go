package main

import (
	"bufio"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"
	_ "modernc.org/sqlite"
)

func dump(p string) {
	fmt.Printf("=== %s ===\n", p)
	if _, err := os.Stat(p); os.IsNotExist(err) {
		fmt.Println("  missing")
		return
	}
	db, err := sql.Open("sqlite", p)
	if err != nil { fmt.Println("  open:", err); return }
	defer db.Close()
	for _, t := range []string{"accounts","channels","devices","sources","points","targets","target_points","tokens"} {
		if *showSchema {
			rows, err := db.Query("PRAGMA table_info(" + t + ")")
			if err != nil { fmt.Println("    err:", err); continue }
			fmt.Printf("  %s columns:\n", t)
			for rows.Next() {
				var cid int; var name, ctype string; var notnull, pk int; var dflt any
				rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk)
				fmt.Printf("    %d. %s %s\n", cid, name, ctype)
			}
			rows.Close()
			continue
		}
		fmt.Printf("  %s:\n", t)
		rows, err := db.Query("SELECT * FROM " + t)
		if err != nil { fmt.Println("    err:", err); continue }
		cols, _ := rows.Columns()
		vals := make([]any, len(cols))
		for i := range vals { vals[i] = new(any) }
		n := 0
		for rows.Next() {
			if err := rows.Scan(vals...); err != nil { fmt.Println("    scan err:", err); break }
			parts := make([]string, len(cols))
			for i := range cols {
				parts[i] = fmt.Sprintf("%v", *(vals[i].(*any)))
			}
			fmt.Printf("    %d: %s\n", n, fmt.Sprintf("%v", parts))
			n++
		}
		rows.Close()
		if n == 0 { fmt.Println("    (empty)") }
	}
}

// 扫描所有关心的表（含 target_points / tokens）。
// 用法：
//   dbinspect                          # 默认 dump F:\Project\PlcBridge\config\data\plcbridge.db
//   dbinspect <db路径> [<db路径>...]    # dump 任意一个或多个 db
//   dbinspect -                         # 从 stdin 读每行一个 db 路径
// -schema 开启：dump 表 schema（PRAGMA table_info）而不是行内容。
var showSchema = flag.Bool("schema", false, "输出每个表的列定义")

func main() {
	flag.Parse()
	args := flag.Args()
	var paths []string
	if len(args) == 0 {
		// 默认路径：与开发期 F 盘工作区一致
		paths = []string{`F:\Project\PlcBridge\config\data\plcbridge.db`}
	} else if len(args) == 1 && args[0] == "-" {
		// 从 stdin 读路径列表
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line != "" {
				paths = append(paths, line)
			}
		}
		if err := sc.Err(); err != nil {
			fmt.Fprintln(os.Stderr, "stdin:", err)
			os.Exit(2)
		}
	} else {
		paths = args
	}
	for _, p := range paths { dump(p) }
}
