package main

import (
	"database/sql"
	"fmt"
	_ "modernc.org/sqlite"
)

func main() {
	db, err := sql.Open("sqlite", `C:\ProgramData\plcbridge\data\plcbridge.db`)
	if err != nil {
		fmt.Println("open err:", err)
		return
	}
	defer db.Close()

	tables := []string{"devices", "channels", "sources", "points", "targets", "target_points"}
	cols := map[string][]string{}
	for _, t := range tables {
		fmt.Printf("\n--- %s schema:\n", t)
		rs, err := db.Query("PRAGMA table_info(" + t + ")")
		if err != nil {
			fmt.Println(" err:", err)
			continue
		}
		var names []string
		for rs.Next() {
			var cid int
			var n, ty string
			var nn, dflt sql.NullString
			var pk int
			rs.Scan(&cid, &n, &ty, &nn, &dflt, &pk)
			names = append(names, n)
			fmt.Printf("  %d: %s %s\n", cid, n, ty)
		}
		rs.Close()
		cols[t] = names
	}

	show := func(t string) {
		cs := cols[t]
		if len(cs) == 0 {
			return
		}
		fmt.Printf("\n--- %s rows:\n", t)
		q := "SELECT " + joinCols(cs) + " FROM " + t
		rs, err := db.Query(q)
		if err != nil {
			fmt.Println(" err:", err)
			return
		}
		defer rs.Close()
		for rs.Next() {
			raw := make([]sql.NullString, len(cs))
			ptrs := make([]interface{}, len(cs))
			for i := range raw {
				ptrs[i] = &raw[i]
			}
			if err := rs.Scan(ptrs...); err != nil {
				fmt.Println(" scan err:", err)
				continue
			}
			for i, c := range cs {
				v := ""
				if raw[i].Valid {
					v = raw[i].String
				}
				if len(v) > 200 {
					v = v[:200] + "..."
				}
				fmt.Printf("  %s=%s\n", c, v)
			}
			fmt.Println("  ---")
		}
	}
	show("devices")
	show("channels")
	show("sources")
	show("points")
	show("targets")
	show("target_points")
}

func joinCols(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += ","
		}
		out += v
	}
	return out
}
