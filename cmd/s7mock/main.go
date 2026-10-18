// Minimal S7-200 ISO-on-TCP server for connection-timing test.
// Accepts TCP connections on :102, completes the ISO handshake (CR/CC -> DT -> data),
// then echoes any subsequent read requests with 2 bytes of zeros.
// Does NOT implement full S7 protocol — just enough that gos7 thinks the connection is live
// and ProbeActive can succeed.
//
// Handshake flow:
//   Client COTP CR (length 0x10, PDU type 0xE0, dst/tsap=01.00/00.01) →
//   Server COTP CC (length 0x0D, PDU type 0xD0, dst/tsap=00.01/01.00) →
//   Client S7 Setup comm (function 0xF0) →
//   Server S7 Setup ack (function 0xF0, no error) →
//   (then any further client request: server replies with 4-byte S7 header + ack data)
package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"time"
)

func main() {
	addr := "127.0.0.1:102"
	if len(os.Args) > 1 {
		addr = os.Args[1]
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen %s: %v\n", addr, err)
		os.Exit(1)
	}
	fmt.Printf("S7 minimal server listening on %s\n", addr)
	for {
		c, err := ln.Accept()
		if err != nil {
			fmt.Fprintf(os.Stderr, "accept: %v\n", err)
			continue
		}
		go handle(c)
	}
}

func handle(c net.Conn) {
	nc := c
	defer nc.Close()
	fmt.Printf("[%s] connected from %s\n", time.Now().Format("15:04:05.000"), nc.RemoteAddr())

	// Read ISO header (4 bytes) + TPKT (4 bytes)
	// TPKT: version(1), reserved(1), length(2)
	// Then COTP: length(1), PDU type(1), dst_ref(2), src_ref(2), class_option(1)
	hdr := make([]byte, 4)
	if _, err := readFull(nc, hdr); err != nil {
		fmt.Printf("[%s] read TPKT: %v\n", time.Now().Format("15:04:05.000"), err)
		return
	}
	plen := binary.BigEndian.Uint16(hdr[2:4])
	body := make([]byte, plen-4)
	if _, err := readFull(nc, body); err != nil {
		fmt.Printf("[%s] read body: %v\n", time.Now().Format("15:04:05.000"), err)
		return
	}
	if len(body) == 0 || body[0] != 0x02 {
		fmt.Printf("[%s] not COTP, body[0]=%x\n", time.Now().Format("15:04:05.000"), body[0])
		return
	}
	pduType := body[1]
	fmt.Printf("[%s] TPKT len=%d COTP pdu=0x%02x (CR=0xE0 DT=0xF0)\n", time.Now().Format("15:04:05.000"), plen, pduType)

	if pduType == 0xE0 { // CR
		// Send CC
		cc := []byte{
			0x03, 0x00, 0x00, 0x16, // TPKT len=22
			0x02, 0xD0, 0x80, 0x80, // COTP: len=2, CC, dst=0x80, src=0x80
			0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x01, 0x12, 0x04, 0x11, 0x00, 0x00, 0x01, 0x00, 0x01,
		}
		nc.Write(cc)
		// Read S7 Setup comm (function 0xF0)
		hdr2 := make([]byte, 4)
		if _, err := readFull(nc, hdr2); err != nil {
			return
		}
		plen2 := binary.BigEndian.Uint16(hdr2[2:4])
		body2 := make([]byte, plen2-4)
		if _, err := readFull(nc, body2); err != nil {
			return
		}
		fmt.Printf("[%s] S7 setup received, len=%d\n", time.Now().Format("15:04:05.000"), plen2)
		// Send S7 setup ack
		ack := []byte{
			0x03, 0x00, 0x00, 0x19, // TPKT len=25
			0x02, 0xF0, 0x80, // COTP DT
			0x32, 0x01, // S7 header: protocol id 0x32, ack 0x01
			0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x08,
			0x00, 0x00,
			0xF0, 0x00, // function 0xF0 (setup comm)
			0x00, 0x00, 0x00, 0x00, 0x00,
		}
		nc.Write(ack)
		fmt.Printf("[%s] S7 setup ack sent\n", time.Now().Format("15:04:05.000"))
	}

	// Echo any further reads: respond with S7 ack + zero data
	for {
		hdr3 := make([]byte, 4)
		if _, err := readFull(nc, hdr3); err != nil {
			fmt.Printf("[%s] read err: %v\n", time.Now().Format("15:04:05.000"), err)
			return
		}
		plen3 := binary.BigEndian.Uint16(hdr3[2:4])
		body3 := make([]byte, plen3-4)
		if _, err := readFull(nc, body3); err != nil {
			return
		}
		// Reply with S7 ack (function 0x04 read, 2 bytes data)
		resp := []byte{
			0x03, 0x00, 0x00, 0x21, // TPKT len=33
			0x02, 0xF0, 0x80, // COTP DT
			0x32, 0x03, // S7 header: protocol 0x32, ack 0x03
			0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x02, // data length 2
			0xFF, 0x04, // function 0x04 (read), 1 byte follow
			0x01, 0x00, // 1 item, no error
			0x00, 0x00, 0x00, 0x00, // transport size + length
			0x00, 0x04, // data length 4
			0x00, 0x00, // 2 bytes data (zero)
			0x00, 0x00, // padding
		}
		nc.Write(resp)
	}
}

func readFull(c net.Conn, buf []byte) (int, error) {
	read := 0
	for read < len(buf) {
		n, err := c.Read(buf[read:])
		if err != nil {
			return read, err
		}
		read += n
	}
	return read, nil
}
