package main

import (
	"database/sql"
	"fmt"
	"os"

	_ "modernc.org/sqlite"
)

func main() {
	db, err := sql.Open("sqlite", os.Args[1])
	if err != nil {
		fmt.Println("open:", err)
		os.Exit(1)
	}
	defer db.Close()

	rows, err := db.Query("SELECT id, username, password_hash, updated_at FROM accounts")
	if err != nil {
		fmt.Println("query:", err)
		os.Exit(1)
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var user, hash string
		var updated int64
		rows.Scan(&id, &user, &hash, &updated)
		fmt.Printf("id=%d user=%s hash=%s updated=%d\n", id, user, hash, updated)
	}
}