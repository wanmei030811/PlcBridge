// Package secconfig 集中管理 plcbridge 的"安全基线"配置。
//
// 为什么单独成包：
//
//	安全相关的配置（TLS、CORS 白名单、API 密钥强度、HTTP 超时）在三个进程里
//	都会用到（plcbridge / plcbridge-web / 未来 CLI）。放在各自包里会重复实现，
//	而且更危险：web 端做了强度校验、core 端没做 → 部署时容易漏配。
//
// 设计原则：
//  1. 全部通过环境变量或 flag 显式配置，不读配置文件（避免"配置文件泄露 = 密钥泄露"）。
//  2. 弱默认值一律 fail-fast：宁可启动失败，也不要用一个公开在源码里的密钥跑生产。
//  3. 每个校验函数返回明确的 error，调用方决定是拒启动还是仅告警。
package secconfig

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// 环境变量名常量。
//
// 命名统一 PLCBRIDGE_ 前缀，与已有的 PLCBRIDGE_CONFIG / PLCBRIDGE_API_SECRET 一致。
const (
	// EnvAPISecret 是 web ↔ core 内部调用的 HMAC 签名密钥。
	EnvAPISecret = "PLCBRIDGE_API_SECRET"

	// EnvCORSOrigins 是允许跨域访问 web 端 API 的 Origin 白名单（逗号分隔）。
	//
	// 空值 = 禁止一切跨域（同源部署，最安全）。默认不再是"反射任意 Origin"。
	EnvCORSOrigins = "PLCBRIDGE_CORS_ORIGINS"

	// EnvSecureCookie 强制 session cookie 带 Secure 属性（仅 HTTPS 下发时用）。
	// 取值 "1"/"true" 开启。
	EnvSecureCookie = "PLCBRIDGE_SECURE_COOKIE"

	// EnvTLSCert / EnvTLSKey 是 TLS 证书与私钥路径（PEM）。
	EnvTLSCert = "PLCBRIDGE_TLS_CERT"
	EnvTLSKey  = "PLCBRIDGE_TLS_KEY"

	// EnvReadTimeoutSec 等是 HTTP server 超时（秒）。
	EnvReadTimeoutSec      = "PLCBRIDGE_READ_TIMEOUT_SEC"
	EnvWriteTimeoutSec     = "PLCBRIDGE_WRITE_TIMEOUT_SEC"
	EnvIdleTimeoutSec      = "PLCBRIDGE_IDLE_TIMEOUT_SEC"
	EnvReadHeaderTimeout   = "PLCBRIDGE_READ_HEADER_TIMEOUT_SEC"
	EnvMaxHeaderBytes      = "PLCBRIDGE_MAX_HEADER_BYTES"
	EnvMaxBodyBytes        = "PLCBRIDGE_MAX_BODY_BYTES"
	EnvRateLimitRPS        = "PLCBRIDGE_RATE_LIMIT_RPS"
	EnvRateLimitBurst      = "PLCBRIDGE_RATE_LIMIT_BURST"
	EnvAllowInsecureSecret = "PLCBRIDGE_ALLOW_INSECURE_SECRET"
)

// 弱密钥判定阈值。
const (
	// MinSecretLen 是 API 密钥的最小长度（字节）。
	//
	// 32 字节 = 256 bit，够 HMAC-SHA256 用。低于这个长度拒绝启动。
	MinSecretLen = 32

	// secretFileTerminator 是密钥文件的结尾标记（换行）。
	//
	// 它同时承担两个职责：
	//  1. 让文件内容在文本编辑器里可读（不会把最后一行和后续内容粘在一起）。
	//  2. **充当"写入完成"标记** —— 写方总是把换行写在最后，
	//     读方据此判断文件是否已写完，避免读到半截内容。
	//     见 loadSecretFile。
	secretFileTerminator = "\n"

	// secretReadWait 是等待密钥文件写完的最长时间。
	//
	// 覆盖"另一个进程正在创建并写入密钥文件"的窗口。
	// 2 秒对本地文件系统绰绰有余（写入 44 字节是微秒级操作）。
	secretReadWait = 2 * time.Second

	// secretQuietPeriod 是"内容保持不变"的安静期，用于判定写入已结束
	//（兼容没有结尾换行的手工编辑文件）。
	//
	// 必须大于写方两次写之间的最大间隔，否则会把半截内容当成完整内容。
	secretQuietPeriod = 200 * time.Millisecond

	// secretPollInterval 是轮询文件内容的间隔。
	secretPollInterval = 20 * time.Millisecond
)

// weakSecretLiterals 是必须拒绝的密钥字面量（完整匹配，大小写不敏感）。
//
// 这些值都出现在过本项目的历史源码 / 文档里，一旦有人照抄部署，
// 等于全网公开的密钥。必须硬拒。
//
// 注意：这里只列**完整匹配**的弱值（见 weakSecretReason 的匹配方式），
// 不能用子串匹配 —— 否则随机生成的合法密钥只要碰巧含有 "secret" 子串
// 就会被误拒（实测踩过这个坑）。
var weakSecretLiterals = []string{
	"plcbridge-default-secret-change-me",
	"12345678",
}

// weakSecretSubstrings 是出现即拒绝的子串（大小写不敏感）。
//
// 这些词强烈提示"人手敲的密码"而不是"机器生成的随机串"：
// 只要含有它们，几乎可以确定强度不足。
//
// 为什么不把 "secret" 放进来：
// supervisor 生成的 base64 随机串、以及测试用的合法密钥都可能恰好含有
// 该子串 —— 子串匹配会误伤（实测踩过这个坑：合法的 file-provided-secret-…
// 被拒，导致 supervisor 生成的密钥自己通不过检查）。
// "secret" 单独出现强度其实由长度保证，所以不作为子串黑名单。
var weakSecretSubstrings = []string{
	"password",
	"admin",
	"changeme",
	"change-me",
	"qwerty",
	"letmein",
}

// SecretStatus 描述 API 密钥的健康状态。
type SecretStatus struct {
	// Source 是密钥来源："env" / "default" / ""（未设置）。
	Source string
	// Set 表示是否有可用密钥。
	Set bool
	// Weak 表示密钥存在但强度不足。
	Weak bool
	// Reason 是不可用原因的简短描述（用于日志）。
	Reason string
}

// ErrSecretTooWeak 表示密钥强度不足。
type ErrSecretTooWeak struct {
	Reason string
}

func (e *ErrSecretTooWeak) Error() string {
	return "PLCBRIDGE_API_SECRET 强度不足: " + e.Reason
}

// CheckAPISecret 检查 API 密钥强度。
//
// 分三档：
//   - env 设置且强度足够 → Status{Source:"env", Set:true}，无错。
//   - env 未设置但存在强默认密钥文件 → Status{Source:"file", Set:true}，无错。
//   - env 未设置且无强默认 → Status{Set:false} + error（生产必须拒启动）。
//
// 弱密钥（长度不足 / 命中禁用字面量）一律返回 error，
// 除非 PLCBRIDGE_ALLOW_INSECURE_SECRET=1（仅供本地开发与测试）。
func CheckAPISecret() (SecretStatus, error) {
	raw := strings.TrimSpace(os.Getenv(EnvAPISecret))
	if raw == "" {
		return SecretStatus{Source: "", Set: false, Reason: "环境变量未设置"}, ErrSecretMissing()
	}
	if reason := weakSecretReason(raw); reason != "" {
		st := SecretStatus{Source: "env", Set: true, Weak: true, Reason: reason}
		if os.Getenv(EnvAllowInsecureSecret) == "1" {
			return st, nil
		}
		return st, &ErrSecretTooWeak{Reason: reason}
	}
	return SecretStatus{Source: "env", Set: true}, nil
}

// ErrSecretMissing 表示 API 密钥缺失。
func ErrSecretMissing() error {
	return &ErrSecretTooWeak{Reason: "环境变量 " + EnvAPISecret + " 未设置"}
}

// weakSecretReason 判断密钥是否弱；返回空串表示强度足够。
//
// 判定项：
//  1. 长度 < MinSecretLen → 太短
//  2. 完整匹配禁用字面量 → 已知弱值（照抄默认值）
//  3. 含禁用子串（"password" / "admin" / "changeme" 等）→ 人手密码特征
//  4. 重复模式（如 "changemechangemechangeme"）→ 低熵
//  5. 全部字符相同 → 低熵
//
// 为什么第 2 条用「完整匹配」而第 3 条用「子串包含」：
// 子串匹配会误伤随机密钥 —— 一个合法的 32 字节 base64 串只要碰巧
// 含有 "secret" / "admin" 之类子串就被拒，导致 supervisor 生成的密钥
// 自己都通不过检查（死循环）。所以黑名单分成两档：
//   - 完整匹配：拦住"照抄默认值"
//   - 子串匹配：只拦人手敲的典型弱密码词，且刻意不含 "secret"
func weakSecretReason(s string) string {
	if len(s) < MinSecretLen {
		return fmt.Sprintf("长度 %d < %d 字节", len(s), MinSecretLen)
	}
	lower := strings.ToLower(s)

	for _, bad := range weakSecretLiterals {
		if lower == bad {
			return "使用了已知弱口令字面量 " + strconv.Quote(bad)
		}
	}

	for _, bad := range weakSecretSubstrings {
		if strings.Contains(lower, bad) {
			return "包含弱口令特征词 " + strconv.Quote(bad)
		}
	}

	// 重复模式检测：某个短片段重复 3 次以上（覆盖 "changemechangemechangeme"、
	// "abcabcabcabc" 这类）。
	if hasRepeatedPattern(lower) {
		return "存在重复模式（熵过低）"
	}

	// 低熵检测：所有字符相同。
	allSame := true
	for i := 1; i < len(s); i++ {
		if s[i] != s[0] {
			allSame = false
			break
		}
	}
	if allSame {
		return "所有字符相同（熵过低）"
	}
	return ""
}

// hasRepeatedPattern 判断字符串是否由某个短片段重复构成。
//
// 片段长度取 1..8；能整除且重复 ≥3 次即判定为低熵。
func hasRepeatedPattern(s string) bool {
	n := len(s)
	if n < 6 {
		return false
	}
	for size := 1; size <= 8 && size*3 <= n; size++ {
		if n%size != 0 {
			continue
		}
		unit := s[:size]
		if strings.Repeat(unit, n/size) == s {
			return true
		}
	}
	return false
}

// SecretFileName 是密钥文件名（放在 configDir 下）。
const SecretFileName = "api.secret"

// EnsureAPISecret 在进程启动期强制校验 API 密钥强度。
//
// 行为：
//   - 密钥强度足够 → 返回 nil。
//   - 密钥缺失或过弱 → 返回 error，调用方应拒绝启动。
//
// 逃生舱：设置 PLCBRIDGE_ALLOW_INSECURE_SECRET=1 可跳过校验，
// 仅用于本地开发与自动化测试；生产环境绝不应设置。
//
// 用法：
//
//	if err := secconfig.EnsureAPISecret(); err != nil {
//	    fmt.Fprintf(os.Stderr, "启动被拒绝: %v\n", err)
//	    os.Exit(1)
//	}
func EnsureAPISecret() error {
	if os.Getenv(EnvAllowInsecureSecret) == "1" {
		return nil
	}
	st, err := CheckAPISecret()
	if err != nil {
		return err
	}
	_ = st
	return nil
}

// EnsureAPISecretWithFallback 在进程启动期确保 API 密钥可用：
//
//  1. 环境变量已设置且强度足够 → 直接用（不碰文件）。
//  2. 环境变量缺失/过弱 → 读 <configDir>/api.secret。
//  3. 文件也不存在 → **自动生成**一个强随机密钥并落盘，两个进程共享它。
//
// 为什么允许自动生成（而不是像原来那样直接拒启动）：
//
//	plcbridge 与 plcbridge-web 是同机双进程，必须共享同一个密钥。
//	现场首次部署时，用户手工启动两个进程是常态 ——
//	要求用户"先设好环境变量再启动"体验很差，且容易只给一个进程设了。
//
//	自动生成 + 共享文件消除了这个坑：
//	  - 谁先启动谁生成，写入 <configDir>/api.secret；
//	  - 后启动的进程读到同一个文件 → 天然共享同一个密钥；
//	  - 运维需要自定义时，仍可用环境变量覆盖（优先级最高）。
//
//	安全性：生成的密钥是 32 字节密码学随机数（base64，44 字符），
//	强度足够；文件已加入 .gitignore；不写进命令行（不泄露到进程列表）。
//
// 逃生舱：PLCBRIDGE_ALLOW_INSECURE_SECRET=1 跳过校验（仅本地开发/测试）。
func EnsureAPISecretWithFallback(configDir string) error {
	if os.Getenv(EnvAllowInsecureSecret) == "1" {
		return nil
	}
	// 环境变量本身够强 → 直接通过（保留环境变量，不动文件）。
	if st, err := CheckAPISecret(); err == nil && !st.Weak {
		return nil
	}

	if configDir == "" {
		// 没有 configDir 就没有共享文件的位置，只能报错。
		if _, err := CheckAPISecret(); err != nil {
			return err
		}
		return nil
	}

	path := filepath.Join(configDir, SecretFileName)

	// 已存在（且完整）→ 读出来用。
	//
	// loadSecretFile 会处理"另一个进程正在写、我们读到半截"的情况：
	// 半截密钥长度可能 ≥32 字节，能通过强度检查，但和文件最终内容不一致
	// → core 缓存的密钥与 web 缓存的不一致 → 内部调用全 401。
	// 所以必须等到文件写完再读（见 loadSecretFile 的完整性判定）。
	secret, err := loadSecretFile(path, secretReadWait)
	if err == nil {
		if reason := weakSecretReason(secret); reason == "" {
			// 写回环境变量，让同进程内的 auth.ServiceSecret 用同一个密钥。
			_ = os.Setenv(EnvAPISecret, secret)
			return nil
		}
		// 文件里是弱密钥 → 不使用，但也不覆盖（可能是运维手工放的）。
		// 报错让运维知道要处理。
		return &ErrSecretTooWeak{Reason: "密钥文件 " + path + " 内容强度不足: " + weakSecretReason(secret)}
	}
	if !errors.Is(err, errSecretFileNotExist) {
		return fmt.Errorf("读取 API 密钥文件 %s 失败: %w", path, err)
	}

	// 不存在 → 生成并落盘。
	secret, err = GenerateAPISecret()
	if err != nil {
		return fmt.Errorf("生成 API 密钥失败: %w", err)
	}
	// 独占创建：并发启动时只有一个进程能真正写入，
	// 另一个发现自己输了竞争 → 读文件里赢家写的密钥。
	// 绝不能两个进程各生成一个（web 与 core 密钥不一致 → 内部调用全 401）。
	if err := createSecretFileExclusive(path, secret); err != nil {
		return fmt.Errorf("写入 API 密钥文件 %s 失败: %w", path, err)
	}

	// 无论谁赢了，最终都以**文件内容**为准 —— 这样并发启动的两个进程
	// 一定拿到同一个密钥。
	final, err := loadSecretFile(path, secretReadWait)
	if err != nil {
		// 读不到就用自己生成的那个（我们可能就是写入者，内容肯定一致）。
		final = secret
	}
	_ = os.Setenv(EnvAPISecret, final)
	return nil
}

// GenerateAPISecret 生成一个强随机的 API 密钥（32 字节，base64 编码）。
//
// base64 标准字母表含 '+' '/' '='，均为 cmd/PowerShell 安全字符。
func GenerateAPISecret() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("读取随机数失败: %w", err)
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// createSecretFileExclusive 独占创建密钥文件。
//
// 用 O_CREATE|O_EXCL：内核保证原子性 —— 并发调用只有一个成功。
// 成功者写入密钥；失败者（文件已存在）什么都不做，由调用方读文件。
//
// 这样避免了「两个进程各生成一个密钥、互相覆盖」导致 web/core 密钥不一致的问题。
//
// 写入时**必须把换行放在最后**（secret + "\n"）：
// 换行是"写入完成"的标记 —— 读方据此判断文件是否已写完。
// 见 loadSecretFile 的完整性判定。
func createSecretFileExclusive(path, secret string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建目录失败: %w", err)
	}
	// 0600：仅属主可读写。Windows 上忽略该位，由目录 ACL 兜底。
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil // 别人赢了，读文件即可
		}
		return fmt.Errorf("独占创建失败: %w", err)
	}
	if _, err := f.WriteString(secret + secretFileTerminator); err != nil {
		_ = f.Close()
		// 写失败就把半截文件删掉，否则别的进程会读到空密钥。
		_ = os.Remove(path)
		return fmt.Errorf("写入失败: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("关闭失败: %w", err)
	}
	return nil
}

// errSecretFileNotExist 表示密钥文件不存在（可以安全地生成一个）。
var errSecretFileNotExist = errors.New("secret file does not exist")

// loadSecretFile 读取密钥文件，并确保读到的是**完整**内容。
//
// 为什么不能直接 ReadFile：
//
//	进程 A 用 O_EXCL 创建文件后开始写入；进程 B 立刻 ReadFile，
//	可能读到"写了一半"的内容 —— 比如 base64 密钥的前 40 个字符。
//	这 40 个字符长度 ≥32，能通过强度检查，于是 B 会把一个
//	**和文件最终内容不同**的密钥缓存进内存。
//	结果：web 与 core 各自缓存不同密钥 → 内部调用全部 401，
//	而且现象非常隐蔽（文件内容看起来完全正常）。
//
// 完整性判定（两级）：
//  1. 文件以换行结尾 → 写入完成（我们总是把换行写在最后）。这是主路径。
//  2. 文件内容在 quietPeriod 内**持续不变** → 认为没有人在写了。
//     这一级用于兼容"运维手工编辑、没写结尾换行"的场景。
//
// 关于第 2 级的 quietPeriod：
//
//	不能用"连续两次读到相同内容"就返回 —— 实测踩过这个坑：
//	写方分多次写，读方在两次 poll 之间恰好读到同一个半截内容，
//	就会把半截内容当成完整内容返回。
//	必须要求内容在一段**安静期**内不变，才能认定写入已结束。
//
// timeout 内一直读不到完整内容 → 返回错误。
func loadSecretFile(path string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var (
		lastContent  string
		firstSeen    time.Time
		haveLastSeen bool
	)
	for {
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return "", errSecretFileNotExist
			}
			return "", err
		}
		content := string(data)

		// 一级判定：结尾换行 = 写入完成。
		if strings.HasSuffix(content, secretFileTerminator) {
			s := strings.TrimSpace(content)
			if s == "" {
				return "", fmt.Errorf("secret file %s is empty", path)
			}
			return s, nil
		}

		// 二级判定：内容持续安静超过 quietPeriod → 认为没有人在写了。
		now := time.Now()
		if !haveLastSeen || content != lastContent {
			lastContent = content
			firstSeen = now
			haveLastSeen = true
		} else if now.Sub(firstSeen) >= secretQuietPeriod {
			s := strings.TrimSpace(content)
			if s == "" {
				return "", fmt.Errorf("secret file %s is empty", path)
			}
			return s, nil
		}

		if now.After(deadline) {
			return "", fmt.Errorf("secret file %s 写入不完整（未读到结尾换行，且内容持续变化）", path)
		}
		time.Sleep(secretPollInterval)
	}
}

// CORSOrigins 返回允许的跨域 Origin 白名单。
//
// 默认（未设置环境变量）返回空列表 = 不允许任何跨域。
// 这是行为变更：旧实现反射任意 Origin 且允许携带凭证，存在 CSRF 风险。
//
// 格式：逗号分隔的完整 origin，如 "http://10.0.0.5:8080,https://ops.example.com"。
// 通配符 "*" 单独处理：允许任意 origin 但**不**携带凭证（符合 CORS 规范）。
func CORSOrigins() []string {
	raw := strings.TrimSpace(os.Getenv(EnvCORSOrigins))
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		p := strings.TrimSpace(part)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// SecureCookie 返回 session cookie 是否应带 Secure 属性。
//
// 判定：显式环境变量优先；否则按 TLS 是否启用自动推断
// （启用 TLS → Secure=true，因为明文 HTTP 下发 Secure cookie 会导致浏览器拒收）。
func SecureCookie(tlsEnabled bool) bool {
	if v := strings.TrimSpace(os.Getenv(EnvSecureCookie)); v != "" {
		b, err := strconv.ParseBool(v)
		if err == nil {
			return b
		}
	}
	return tlsEnabled
}

// TLSEnabled 返回是否配置了 TLS 证书与私钥（两个都设置才算启用）。
func TLSEnabled() bool {
	return strings.TrimSpace(os.Getenv(EnvTLSCert)) != "" &&
		strings.TrimSpace(os.Getenv(EnvTLSKey)) != ""
}

// TLSFiles 返回 TLS 证书与私钥路径（可能为空字符串）。
func TLSFiles() (certFile, keyFile string) {
	return strings.TrimSpace(os.Getenv(EnvTLSCert)),
		strings.TrimSpace(os.Getenv(EnvTLSKey))
}

// HTTPServerTimeouts 是一组 HTTP server 防护参数。
type HTTPServerTimeouts struct {
	// ReadTimeout 覆盖"读完整请求"的上限（含 body）。
	ReadTimeout int
	// WriteTimeout 覆盖"写完整响应"的上限。
	WriteTimeout int
	// IdleTimeout 是 keep-alive 连接的空闲上限。
	IdleTimeout int
	// ReadHeaderTimeout 是读请求头的上限（防 slowloris）。
	ReadHeaderTimeout int
	// MaxHeaderBytes 是请求头字节上限。
	MaxHeaderBytes int
	// MaxBodyBytes 是请求体字节上限（0 = 不限制）。
	MaxBodyBytes int64
	// RateLimitRPS 是单 IP 每秒请求数上限（0 = 不限流）。
	RateLimitRPS float64
	// RateLimitBurst 是令牌桶突发容量（0 = 用 RPS 的 2 倍兜底）。
	RateLimitBurst int
}

// DefaultHTTPServerTimeouts 返回生产推荐的默认超时。
//
// 取值理由：
//   - ReadTimeout 15s：足够读完一个批量配置请求体；再长会被慢连接占住。
//   - WriteTimeout 30s：回写 API 本身有 timeout_ms 语义（最长几秒），
//     30s 留足余量，避免正常回写被服务端提前掐断。
//   - ReadHeaderTimeout 5s：防 slowloris 的关键项。
//   - IdleTimeout 60s：keep-alive 空闲 60s 后回收，防止连接堆积。
//   - MaxHeaderBytes 1MB：足够大到不会误伤，又小到能挡住 header 炸弹。
//   - MaxBodyBytes 4MB：CSV 批量导入点位最多几百 KB，4MB 足够。
func DefaultHTTPServerTimeouts() HTTPServerTimeouts {
	return HTTPServerTimeouts{
		ReadTimeout:       envInt(EnvReadTimeoutSec, 15),
		WriteTimeout:      envInt(EnvWriteTimeoutSec, 30),
		IdleTimeout:       envInt(EnvIdleTimeoutSec, 60),
		ReadHeaderTimeout: envInt(EnvReadHeaderTimeout, 5),
		MaxHeaderBytes:    envInt(EnvMaxHeaderBytes, 1<<20),
		MaxBodyBytes:      int64(envInt(EnvMaxBodyBytes, 4<<20)),
		RateLimitRPS:      envFloat(EnvRateLimitRPS, 50),
		RateLimitBurst:    envInt(EnvRateLimitBurst, 100),
	}
}

// envInt 读取整型环境变量，非法或未设置时返回 def。
func envInt(name string, def int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return n
}

// envFloat 读取浮点环境变量，非法或未设置时返回 def。
func envFloat(name string, def float64) float64 {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return def
	}
	return f
}
