// Package httpclient 提供带连接复用的 HTTP 客户端（Phase 1.7）。
//
// 背景：Go stdlib http.Client 默认每次新建 TCP 连接（短连接）。
// 在高频场景（如 plcbridge influxdb 写 100+ QPS），频繁握手消耗 CPU + 延迟。
//
// 用法：
//
//	cli := httpclient.New(5 * time.Second) // 一次性创建，跨 goroutine 复用
//	defer cli.Close()
//	resp, err := cli.Get(ctx, "http://example.com/api")
//
// 特性：
//   - 共享 *http.Transport（连接池、keep-alive）
//   - 合理默认（MaxIdleConns=100, IdleConnTimeout=90s）
//   - 业务可调超时 + 自定义 Transport
//   - 线程安全（http.Client 本身并发安全）
package httpclient

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// 默认值（参考 Go stdlib + 实测）
const (
	defaultMaxIdleConns        = 100
	defaultMaxIdleConnsPerHost = 32
	defaultIdleConnTimeout     = 90 * time.Second
	defaultDialTimeout         = 5 * time.Second
	defaultKeepAlive           = 30 * time.Second
)

// Client 封装 http.Client，提供 Close() 关闭 idle 连接。
type Client struct {
	*http.Client
	transport *http.Transport
	closeOnce sync.Once
}

// New 创建带连接复用的 HTTP 客户端。
//
// timeout 为整体请求超时（包含连接、读 body）；0 表示不设超时（不推荐）。
// 其他连接池参数用合理默认，业务可用 Options 自定义。
func New(timeout time.Duration, opts ...Option) *Client {
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   defaultDialTimeout,
			KeepAlive: defaultKeepAlive,
		}).DialContext,
		MaxIdleConns:        defaultMaxIdleConns,
		MaxIdleConnsPerHost: defaultMaxIdleConnsPerHost,
		IdleConnTimeout:     defaultIdleConnTimeout,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	for _, opt := range opts {
		opt(tr)
	}
	return &Client{
		Client:    &http.Client{Transport: tr, Timeout: timeout},
		transport: tr,
	}
}

// Option 自定义 Transport 参数。
type Option func(*http.Transport)

// WithMaxIdleConns 设置最大空闲连接数。
func WithMaxIdleConns(n int) Option {
	return func(t *http.Transport) { t.MaxIdleConns = n }
}

// WithMaxIdleConnsPerHost 设置每个 host 的最大空闲连接数。
func WithMaxIdleConnsPerHost(n int) Option {
	return func(t *http.Transport) { t.MaxIdleConnsPerHost = n }
}

// WithIdleConnTimeout 设置空闲连接超时。
func WithIdleConnTimeout(d time.Duration) Option {
	return func(t *http.Transport) { t.IdleConnTimeout = d }
}

// WithDialTimeout 设置 TCP 连接超时。
func WithDialTimeout(d time.Duration) Option {
	return func(t *http.Transport) {
		t.DialContext = (&net.Dialer{
			Timeout:   d,
			KeepAlive: defaultKeepAlive,
		}).DialContext
	}
}

// Close 关闭所有 idle 连接。多次调用幂等。
//
// 用于：进程退出时释放 fd。
// 注意：不会取消正在进行的请求。
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		c.transport.CloseIdleConnections()
	})
}

// Transport 暴露底层 Transport（高级用途，如自定义 RoundTripper 链）。
func (c *Client) Transport() *http.Transport { return c.transport }

// === 便捷方法（带 ctx 透传）===

// Do 等同 http.Client.Do，但显式文档化 ctx 必传。
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	return c.Client.Do(req)
}

// Get 发送 GET 请求。
func (c *Client) Get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return c.Client.Do(req)
}

// Post 发送 POST 请求（Content-Type: application/octet-stream）。
func (c *Client) Post(ctx context.Context, url, contentType string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return nil, err
	}
	req.Body = readCloserFromBytes(body)
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", contentType)
	return c.Client.Do(req)
}

type bytesReadCloser struct {
	b   []byte
	pos int
}

func (r *bytesReadCloser) Read(p []byte) (int, error) {
	if r.pos >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.pos:])
	r.pos += n
	return n, nil
}

func (r *bytesReadCloser) Close() error { return nil }

func readCloserFromBytes(b []byte) *bytesReadCloser { return &bytesReadCloser{b: b} }
