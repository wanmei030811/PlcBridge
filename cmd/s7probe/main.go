// s7probe —— 直接用 robinson/gos7 连 127.0.0.1:102 探 M100 / DB1.DBD0，验证 S7 server 可达。
//
// 用法：
//   go run ./cmd/s7probe
//   go run ./cmd/s7probe -addr 127.0.0.1:102 -rack 0 -slot 1
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"time"

	gos7 "github.com/robinson/gos7"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:102", "S7 server address (ip:port)")
	rack := flag.Int("rack", 0, "S7 rack")
	slot := flag.Int("slot", 1, "S7 slot")
	db := flag.Int("db", 1, "DB number for float32 probe")
	dbByte := flag.Int("db-byte", 0, "DB byte offset for float32 probe")
	mbByte := flag.Int("m-byte", 100, "M byte offset for bool probe")
	flag.Parse()

	fmt.Printf("[s7probe] connecting to %s rack=%d slot=%d ...\n", *addr, *rack, *slot)
	handler := gos7.NewTCPClientHandler(*addr, *rack, *slot)
	handler.Timeout = 3 * time.Second
	handler.IdleTimeout = 6 * time.Second
	if err := handler.Connect(); err != nil {
		fmt.Fprintf(os.Stderr, "[s7probe] CONNECT FAILED: %v\n", err)
		os.Exit(2)
	}
	fmt.Println("[s7probe] connected ✓")
	defer handler.Close()

	client := gos7.NewClient(handler)

	// M100 = MB100.0 (1 byte, read byte 100 then bit 0)
	mBuf := make([]byte, 1)
	if err := client.AGReadMB(*mbByte, 1, mBuf); err != nil {
		fmt.Fprintf(os.Stderr, "[s7probe] AGReadMB(%d, 1) FAILED: %v\n", *mbByte, err)
	} else {
		v := mBuf[0] & 1
		fmt.Printf("[s7probe] M%d = %v (raw byte = 0x%02X)\n", *mbByte, v == 1, mBuf[0])
	}

	// DB1.DBD0 = float32
	dbBuf := make([]byte, 4)
	if err := client.AGReadDB(*db, *dbByte, 4, dbBuf); err != nil {
		fmt.Fprintf(os.Stderr, "[s7probe] AGReadDB(%d, %d, 4) FAILED: %v\n", *db, *dbByte, err)
	} else {
		fmt.Printf("[s7probe] DB%d.DBD%d raw bytes = %02X %02X %02X %02X\n",
			*db, *dbByte, dbBuf[0], dbBuf[1], dbBuf[2], dbBuf[3])
	}

	// STRING 探针：DB1.DBB100 读 20 字节
	strBuf := make([]byte, 20)
	if err := client.AGReadDB(1, 100, 20, strBuf); err != nil {
		fmt.Fprintf(os.Stderr, "[s7probe] AGReadDB(1, 100, 20) FAILED: %v\n", err)
	} else {
		fmt.Printf("[s7probe] DB1.DBB100..119 raw = ")
		for _, b := range strBuf {
			fmt.Printf("%02X ", b)
		}
		fmt.Println()
		// 解析为 S7 STRING[16] 标准布局
		maxLen := binary.BigEndian.Uint16(strBuf[0:2])
		curLen := binary.BigEndian.Uint16(strBuf[2:4])
		fmt.Printf("[s7probe]   if STRING[16]: maxLen=%d curLen=%d chars=", maxLen, curLen)
		if int(curLen) <= 16 {
			fmt.Printf("%q\n", string(strBuf[4:4+int(curLen)]))
		} else {
			fmt.Printf("(curLen>%d, too long)\n", 16)
		}
		// 也按"无 header" 16 字节直接打印
		fmt.Printf("[s7probe]   if 16 BYTES (no header): %q\n", string(strBuf[0:16]))
		// 按 14 字节（去掉头 2 字节）打印
		fmt.Printf("[s7probe]   if 14 BYTES (skip 2B): %q\n", string(strBuf[2:16]))
		// 按去掉 4 字节 header 12 字节
		fmt.Printf("[s7probe]   if 12 BYTES (skip 4B header): %q\n", string(strBuf[4:16]))
	}
}
