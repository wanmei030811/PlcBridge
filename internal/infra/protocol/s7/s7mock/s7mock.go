// Package s7mock 提供一个最小化的 S7 ISO-on-TCP 服务端，
// 用于连接时序 / 断线重连的集成测试。
//
// 它只实现到"让 gos7 客户端认为连接是活的、并且能读回数据"这个程度：
//   - 接受 TCP 连接
//   - 完成 ISO 握手（COTP CR → CC）
//   - 完成 S7 PDU 长度协商（function 0xF0）
//   - 响应读请求（function 0x04），返回指定长度的 0 数据
//   - 响应写请求（function 0x05）
//
// 它**不是**完整的 S7 协议实现，不能用于验证业务数据正确性；
// 它的用途是验证"连接建立 / 断开 / 重建"这条链路 ——
// 也就是本次修复关注的"PLC 停机再恢复后能否自动重连"。
//
// 帧格式（与 gos7 客户端的期望严格对齐）：
//
//	偏移  含义
//	0-3   TPKT 头（03 00 <len_hi> <len_lo>）
//	4-6   COTP 头（02 F0 80）
//	7     S7 protocol id（0x32）
//	8     ROSCTR（0x01 request / 0x03 ack-data）
//	9-10  redundancy id
//	11-12 PDU reference
//	13-14 parameter length
//	15-16 data length
//	17    error class
//	18    error code
//	19-20 保留
//	21    return code（读响应里是 0xFF = 成功）
//	22    transport size
//	23-24 数据长度（字节）
//	25+   数据
package s7mock

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Server 是一个最小 S7 服务端。
type Server struct {
	ln net.Listener
	// Logf 用于输出握手日志；为 nil 时不输出。
	Logf func(format string, args ...any)

	mu    sync.Mutex
	conns map[net.Conn]struct{}

	// connCount 累计接受的连接数（用于断言"客户端确实重建了连接"）。
	connCount atomic.Int64
}

// Listen 在 addr 上启动一个 mock S7 服务端。
//
// 端口传 "127.0.0.1:0" 时由系统分配端口，可用 Server.Addr() 取回实际地址。
func Listen(addr string) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	s := &Server{ln: ln, conns: map[net.Conn]struct{}{}}
	go s.acceptLoop()
	return s, nil
}

// Addr 返回实际监听地址（用于端口 0 的场景）。
func (s *Server) Addr() string {
	return s.ln.Addr().String()
}

// ConnCount 返回累计接受的连接数。
//
// 集成测试用它断言"PLC 恢复后客户端**确实重建了连接**"，
// 而不是仅仅复用了那个已经死掉的 socket。
func (s *Server) ConnCount() int {
	return int(s.connCount.Load())
}

// Close 关闭服务端并断开所有已建立的连接。
//
// 为什么需要主动断开：集成测试要模拟"PLC 停机"，
// 仅关闭 listener 不会让已建立的连接立刻断开（TCP 半开），
// 客户端会长时间阻塞在读上，而不是立刻收到 RST。
func (s *Server) Close() error {
	err := s.ln.Close()
	s.mu.Lock()
	for c := range s.conns {
		// 设置 SO_LINGER 0 → 关闭时直接发 RST（模拟 PLC 掉电）。
		_ = c.(*net.TCPConn).SetLinger(0)
		_ = c.Close()
	}
	s.conns = map[net.Conn]struct{}{}
	s.mu.Unlock()
	return err
}

func (s *Server) acceptLoop() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.connCount.Add(1)
		s.mu.Lock()
		if s.conns == nil {
			s.conns = map[net.Conn]struct{}{}
		}
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		go s.handle(c)
	}
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf == nil {
		return
	}
	s.Logf(format, args...)
}

func (s *Server) handle(nc net.Conn) {
	defer nc.Close()
	s.logf("[%s] connected from %s", time.Now().Format("15:04:05.000"), nc.RemoteAddr())

	for {
		// TPKT: version(1), reserved(1), length(2)
		hdr := make([]byte, 4)
		if _, err := readFull(nc, hdr); err != nil {
			s.logf("[%s] read TPKT: %v", time.Now().Format("15:04:05.000"), err)
			return
		}
		plen := int(binary.BigEndian.Uint16(hdr[2:4]))
		if plen < 4 || plen > 4096 {
			return
		}
		body := make([]byte, plen-4)
		if _, err := readFull(nc, body); err != nil {
			return
		}
		// body[0] = COTP length, body[1] = COTP PDU type
		if len(body) < 2 {
			return
		}
		switch body[1] {
		case 0xE0: // CR — Connection Request
			if _, err := nc.Write(cotpCC()); err != nil {
				return
			}
			s.logf("[%s] COTP CC sent", time.Now().Format("15:04:05.000"))
		case 0xF0: // DT — Data（承载 S7 PDU）
			// S7 PDU 从 body[3] 开始（body = [02 F0 80 <s7...>]）。
			if len(body) < 4 {
				return
			}
			s7 := body[3:]
			// function code 在 S7 头里的固定偏移（与 gos7 的 telegram 定义一致）。
			if len(s7) < 11 {
				return
			}
			function := s7[10]
			switch function {
			case 0xF0: // PDU 长度协商
				resp := pduNegotiationResponse()
				s.logf("[%s] negotiation resp=% x", time.Now().Format("15:04:05.000"), resp)
				if _, err := nc.Write(resp); err != nil {
					return
				}
			case 0x04: // 读
				n := readElementCount(s7)
				resp := readResponse(n)
				s.logf("[%s] read response n=%d bytes=% x", time.Now().Format("15:04:05.000"), n, resp)
				if _, err := nc.Write(resp); err != nil {
					return
				}
			case 0x05: // 写
				if _, err := nc.Write(writeAckResponse()); err != nil {
					return
				}
			default:
				// 其它 function：回一个通用 ack（不实现语义）。
				if _, err := nc.Write(writeAckResponse()); err != nil {
					return
				}
			}
		default:
			return
		}
	}
}

// cotpCC 返回 COTP Connection Confirm 帧（22 字节）。
//
// 注意长度必须**精确**为 22：TPKT 头里的 length 字段声明 22，
// gos7 客户端按这个长度读帧 —— 多一个字节就会让后续所有帧错位
// （表现为 "Error in PDU negotiation"，非常难查）。
func cotpCC() []byte {
	return []byte{
		0x03, 0x00, 0x00, 0x16, // TPKT len=22
		0x02, 0xD0, 0x80, 0x80, // COTP: len=2, CC, dst=0x80, src=0x80
		0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x01, 0x12, 0x04, 0x11, 0x00, 0x00, 0x01, 0x00,
	}
}

// pduNegotiationResponse 返回 S7 PDU 长度协商响应（27 字节）。
//
// gos7 的判定条件：len == 27 && response[17] == 0 && response[18] == 0，
// 且 PDU 长度取自 response[25:27]。
func pduNegotiationResponse() []byte {
	buf := make([]byte, 27)
	buf[0], buf[1] = 0x03, 0x00
	binary.BigEndian.PutUint16(buf[2:4], 27)
	buf[4], buf[5], buf[6] = 0x02, 0xF0, 0x80 // COTP DT
	buf[7] = 0x32                             // S7 protocol id
	buf[8] = 0x01                             // ROSCTR = ack-data
	// 9..24 保持 0（含 error class / error code = 0）
	binary.BigEndian.PutUint16(buf[25:27], 480) // PDU length
	return buf
}

// readResponse 返回 S7 读响应：携带 n 字节的 0 数据。
//
// gos7 的判定条件：response[21] == 0xFF（return code = 成功），
// 数据从 response[25] 开始取 n 字节。
func readResponse(n int) []byte {
	if n < 0 {
		n = 0
	}
	total := 25 + n
	if total%2 == 1 {
		total++ // S7 帧长度为偶数
	}
	buf := make([]byte, total)
	buf[0], buf[1] = 0x03, 0x00
	binary.BigEndian.PutUint16(buf[2:4], uint16(total))
	buf[4], buf[5], buf[6] = 0x02, 0xF0, 0x80
	buf[7] = 0x32 // S7 protocol id
	buf[8] = 0x03 // ROSCTR = ack-data
	buf[21] = 0xFF
	buf[22] = 0x04 // transport size = byte
	binary.BigEndian.PutUint16(buf[23:25], uint16(n))
	// 25.. 已经是 0
	return buf
}

// writeAckResponse 返回 S7 写响应（22 字节）。
//
// gos7 的判定条件：len == 22 && response[21] == 0xFF。
func writeAckResponse() []byte {
	buf := make([]byte, 22)
	buf[0], buf[1] = 0x03, 0x00
	binary.BigEndian.PutUint16(buf[2:4], 22)
	buf[4], buf[5], buf[6] = 0x02, 0xF0, 0x80
	buf[7] = 0x32
	buf[8] = 0x03
	buf[21] = 0xFF
	return buf
}

// readElementCount 从读请求里取出元素个数。
//
// S7 读请求布局（相对 S7 PDU 起点）：
//
//	16-17 function（0x04）
//	18-19 parameter length
//	20-21 data length
//	22    transport size
//	23-24 number of elements
func readElementCount(s7 []byte) int {
	if len(s7) < 25 {
		return 0
	}
	// s7 已经从 body[3] 开始，即帧内偏移 7。
	// 帧内偏移 23-24 → s7 内偏移 16-17。
	n := int(binary.BigEndian.Uint16(s7[16:18]))
	if n < 0 || n > 4096 {
		return 0
	}
	// transport size：0x02 = byte（每元素 1 字节），0x01 = bit。
	// 这里只支持 byte；其它按 1 字节处理，够测试用。
	return n
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
