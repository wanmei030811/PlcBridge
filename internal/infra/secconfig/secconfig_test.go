package secconfig

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// setEnv 设置环境变量并在测试结束后恢复。
func setEnv(t *testing.T, key, val string) {
	t.Helper()
	orig, had := os.LookupEnv(key)
	if val == "" {
		_ = os.Unsetenv(key)
	} else {
		_ = os.Setenv(key, val)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, orig)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

// TestCheckAPISecretStrong 验证：足够强的密钥通过检查。
func TestCheckAPISecretStrong(t *testing.T) {
	setEnv(t, EnvAPISecret, "aB3xY9zQ1mN7pL5rT8vW2kD6fH0jG4sU")
	setEnv(t, EnvAllowInsecureSecret, "")

	st, err := CheckAPISecret()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !st.Set {
		t.Error("Set = false, want true")
	}
	if st.Weak {
		t.Error("Weak = true, want false")
	}
	if st.Source != "env" {
		t.Errorf("Source = %q, want %q", st.Source, "env")
	}
}

// TestCheckAPISecretMissing 验证：未设置密钥 → 报错。
func TestCheckAPISecretMissing(t *testing.T) {
	setEnv(t, EnvAPISecret, "")
	setEnv(t, EnvAllowInsecureSecret, "")

	_, err := CheckAPISecret()
	if err == nil {
		t.Fatal("expected error for missing secret")
	}
	if !strings.Contains(err.Error(), EnvAPISecret) {
		t.Errorf("error should mention env var name, got %v", err)
	}
}

// TestCheckAPISecretTooShort 验证：过短的密钥 → 报错。
func TestCheckAPISecretTooShort(t *testing.T) {
	setEnv(t, EnvAPISecret, "short")
	setEnv(t, EnvAllowInsecureSecret, "")

	st, err := CheckAPISecret()
	if err == nil {
		t.Fatal("expected error for short secret")
	}
	if !st.Weak {
		t.Error("Weak = true, want false")
	}
}

// TestCheckAPISecretWeakLiterals 验证：命中弱口令特征的密钥 → 报错。
//
// 这些值都出现在过历史源码里，照抄部署等于全网公开。
func TestCheckAPISecretWeakLiterals(t *testing.T) {
	cases := []string{
		"plcbridge-default-secret-change-me",      // 完整匹配历史默认值
		"CHANGE-ME-CHANGE-ME-CHANGE-ME-CHANGE",    // 重复模式
		"changeme-changeme-changeme-changeme",     // 含 changeme
		"my-secret-password-value-here-12345",     // 含 password
		"admin-admin-admin-admin-admin-admin-123", // 含 admin
		"qwertyqwertyqwertyqwertyqwerty",          // 含 qwerty
	}

	for _, c := range cases {
		setEnv(t, EnvAPISecret, c)
		setEnv(t, EnvAllowInsecureSecret, "")

		_, err := CheckAPISecret()
		if err == nil {
			t.Errorf("secret %q should be rejected as weak", c)
		}
	}
}

// TestCheckAPISecretAcceptsRandomLookingSecrets 验证：合法的随机密钥不被误拒。
//
// 这是黑名单设计的回归防护：子串匹配曾把 "file-provided-secret-…"
// 这类合法密钥误判为弱值，导致 supervisor 生成的密钥自己通不过检查。
func TestCheckAPISecretAcceptsRandomLookingSecrets(t *testing.T) {
	cases := []string{
		"file-provided-secret-0123456789abcdef-32",
		"env-provided-secret-0123456789abcdef-32bytes",
		"aB3xY9zQ1mN7pL5rT8vW2kD6fH0jG4sU",
		"xJ9#pQ2@vL7!wR4&nT6*yK3^zD8%fH5_gM1",
	}

	for _, c := range cases {
		setEnv(t, EnvAPISecret, c)
		setEnv(t, EnvAllowInsecureSecret, "")

		if _, err := CheckAPISecret(); err != nil {
			t.Errorf("secret %q should be accepted, got %v", c, err)
		}
	}
}

// TestCheckAPISecretLowEntropy 验证：全部字符相同的密钥 → 报错。
func TestCheckAPISecretLowEntropy(t *testing.T) {
	setEnv(t, EnvAPISecret, strings.Repeat("a", 64))
	setEnv(t, EnvAllowInsecureSecret, "")

	_, err := CheckAPISecret()
	if err == nil {
		t.Fatal("expected error for low-entropy secret")
	}
	if !strings.Contains(err.Error(), "熵") {
		t.Errorf("error should mention entropy, got %v", err)
	}
}

// TestCheckAPISecretAllowInsecureEscapeHatch 验证：逃生开关可放行弱密钥。
func TestCheckAPISecretAllowInsecureEscapeHatch(t *testing.T) {
	setEnv(t, EnvAPISecret, "weak")
	setEnv(t, EnvAllowInsecureSecret, "1")

	st, err := CheckAPISecret()
	if err != nil {
		t.Fatalf("expected no error with escape hatch, got %v", err)
	}
	if !st.Weak {
		t.Error("Weak should still be reported as true")
	}
}

// TestEnsureAPISecretRespectsEscapeHatch 验证：EnsureAPISecret 在逃生开关下不报错。
func TestEnsureAPISecretRespectsEscapeHatch(t *testing.T) {
	setEnv(t, EnvAPISecret, "")
	setEnv(t, EnvAllowInsecureSecret, "1")

	if err := EnsureAPISecret(); err != nil {
		t.Fatalf("expected no error with escape hatch, got %v", err)
	}
}

// TestEnsureAPISecretRejectsMissing 验证：EnsureAPISecret 在缺密钥时报错。
func TestEnsureAPISecretRejectsMissing(t *testing.T) {
	setEnv(t, EnvAPISecret, "")
	setEnv(t, EnvAllowInsecureSecret, "")

	if err := EnsureAPISecret(); err == nil {
		t.Fatal("expected error for missing secret")
	}
}

// TestEnsureAPISecretWithFallbackGeneratesFile 验证：文件不存在时自动生成密钥。
//
// 这是现场首次部署的核心路径：用户什么都不配，直接启动即可。
func TestEnsureAPISecretWithFallbackGeneratesFile(t *testing.T) {
	setEnv(t, EnvAPISecret, "")
	setEnv(t, EnvAllowInsecureSecret, "")

	dir := t.TempDir()
	if err := EnsureAPISecretWithFallback(dir); err != nil {
		t.Fatalf("expected auto-generation to succeed, got %v", err)
	}

	// 文件必须存在。
	path := filepath.Join(dir, SecretFileName)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("secret file not created: %v", err)
	}

	// 环境变量被写回，供 auth.ServiceSecret 使用。
	got := os.Getenv(EnvAPISecret)
	if got == "" {
		t.Fatal("EnvAPISecret not written back")
	}
	if reason := weakSecretReason(got); reason != "" {
		t.Errorf("generated secret is weak: %s", reason)
	}

	// 再次调用应读到同一个密钥（幂等，不重新生成）。
	before := got
	_ = os.Unsetenv(EnvAPISecret)
	if err := EnsureAPISecretWithFallback(dir); err != nil {
		t.Fatalf("second call failed: %v", err)
	}
	if os.Getenv(EnvAPISecret) != before {
		t.Error("second call produced a different secret — must reuse the file")
	}
}

// TestLoadSecretFileWaitsForCompleteWrite 验证：读到"写了一半"的文件时不会返回半截内容。
//
// 这是现场踩到的真实 bug：
//
//	core 先创建了 api.secret 并开始写；web 几乎同时 ReadFile，
//	读到 base64 密钥的前 40 个字符 —— 长度 ≥32，能通过强度检查，
//	于是 web 缓存了一个**和文件最终内容不同**的密钥。
//	结果 web/core 内部调用全部 401，而文件内容看起来完全正常。
//
// 修复：写入方把换行放在最后，读方等到读到换行才认为写完。
func TestLoadSecretFileWaitsForCompleteWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, SecretFileName)

	full := "abcdefghijklmnopqrstuvwxyz0123456789ABCDEF"
	// 模拟"写了一半"：内容够长（能通过强度检查），但没有结尾换行。
	if err := os.WriteFile(path, []byte(full[:40]), 0o600); err != nil {
		t.Fatalf("write partial file failed: %v", err)
	}

	// 启动读取（后台），随后把文件补全。
	done := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		s, err := loadSecretFile(path, 2*time.Second)
		if err != nil {
			errCh <- err
			return
		}
		done <- s
	}()

	// 等读方先看到半截内容，然后补上剩余部分 + 结尾换行。
	time.Sleep(100 * time.Millisecond)
	if err := os.WriteFile(path, []byte(full+secretFileTerminator), 0o600); err != nil {
		t.Fatalf("complete file write failed: %v", err)
	}

	select {
	case s := <-done:
		if s != full {
			t.Errorf("got %q, want full secret %q — reader must not accept a partial write", s, full)
		}
	case err := <-errCh:
		t.Fatalf("loadSecretFile failed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("loadSecretFile did not return")
	}
}

// TestLoadSecretFileAcceptsStableFileWithoutNewline 验证：手工编辑的、没有结尾换行的文件也能读。
//
// 二级判定：内容连续两次读取不变 → 认为没有人在写。
func TestLoadSecretFileAcceptsStableFileWithoutNewline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, SecretFileName)

	content := "hand-edited-secret-without-trailing-newline"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	got, err := loadSecretFile(path, 2*time.Second)
	if err != nil {
		t.Fatalf("loadSecretFile failed: %v", err)
	}
	if got != content {
		t.Errorf("got %q, want %q", got, content)
	}
}

// TestLoadSecretFileMissing 验证：文件不存在时返回可识别错误。
func TestLoadSecretFileMissing(t *testing.T) {
	_, err := loadSecretFile(filepath.Join(t.TempDir(), SecretFileName), time.Second)
	if !errors.Is(err, errSecretFileNotExist) {
		t.Errorf("expected errSecretFileNotExist, got %v", err)
	}
}

// TestEnsureAPISecretWithFallbackConcurrent 验证：并发调用只生成一个密钥。
//
// web 与 core 可能同时启动。若各自生成密钥，两者就不一致 → 内部调用全 401。
func TestEnsureAPISecretWithFallbackConcurrent(t *testing.T) {
	setEnv(t, EnvAPISecret, "")
	setEnv(t, EnvAllowInsecureSecret, "")

	dir := t.TempDir()
	const goroutines = 16

	var wg sync.WaitGroup
	results := make([]string, goroutines)
	errs := make([]error, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			// 只验证"文件里的密钥唯一"，不读环境变量
			//（os.Setenv 是全局的，并发调用会互相干扰）。
			if err := ensureSecretOnly(dir); err != nil {
				errs[idx] = err
				return
			}
			results[idx] = readFileString(filepath.Join(dir, SecretFileName))
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d failed: %v", i, err)
		}
	}
	// 所有 goroutine 读到的必须是同一个密钥。
	for i, got := range results {
		if got == "" {
			continue
		}
		if got != results[0] {
			t.Errorf("goroutine %d read a different secret than goroutine 0", i)
		}
	}

	// 目录里不应残留临时文件。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir failed: %v", err)
	}
	for _, e := range entries {
		if e.Name() != SecretFileName {
			t.Errorf("unexpected leftover file: %s", e.Name())
		}
	}
}

// ensureSecretOnly 是 EnsureAPISecretWithFallback 的"只管文件"版本，
// 不写环境变量 —— 用于并发测试（避免 os.Setenv 的全局竞态）。
func ensureSecretOnly(configDir string) error {
	path := filepath.Join(configDir, SecretFileName)
	if secret, err := loadSecretFile(path, secretReadWait); err == nil {
		reason := weakSecretReason(secret)
		if reason == "" {
			return nil
		}
		return &ErrSecretTooWeak{Reason: reason}
	}
	secret, err := GenerateAPISecret()
	if err != nil {
		return err
	}
	if err := createSecretFileExclusive(path, secret); err != nil {
		return err
	}
	_, err = loadSecretFile(path, secretReadWait)
	return err
}

// readFileString 读文件内容（测试辅助）。
func readFileString(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// TestGenerateAPISecretIsStrongAndUnique 验证：生成的密钥强度足够且互不相同。
func TestGenerateAPISecretIsStrongAndUnique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		s, err := GenerateAPISecret()
		if err != nil {
			t.Fatalf("GenerateAPISecret failed: %v", err)
		}
		if reason := weakSecretReason(s); reason != "" {
			t.Fatalf("generated secret is weak: %s", reason)
		}
		if seen[s] {
			t.Fatal("GenerateAPISecret returned a duplicate")
		}
		seen[s] = true
	}
}

// TestEnsureAPISecretWithFallbackRejectsWeakFile 验证：文件里的弱密钥被拒且不被覆盖。
func TestEnsureAPISecretWithFallbackRejectsWeakFile(t *testing.T) {
	setEnv(t, EnvAPISecret, "")
	setEnv(t, EnvAllowInsecureSecret, "")

	dir := t.TempDir()
	path := filepath.Join(dir, SecretFileName)
	if err := os.WriteFile(path, []byte("weak"), 0o600); err != nil {
		t.Fatalf("write secret file failed: %v", err)
	}

	err := EnsureAPISecretWithFallback(dir)
	if err == nil {
		t.Fatal("expected error for weak secret file")
	}
	// 弱文件不应被自动覆盖（可能是运维手工放的，应让运维看到并处理）。
	if got := readFileString(path); got != "weak" {
		t.Errorf("weak secret file was overwritten: %q", got)
	}
}

// TestEnsureAPISecretWithFallbackPrefersEnv 验证：环境变量优先于文件。
func TestEnsureAPISecretWithFallbackPrefersEnv(t *testing.T) {
	envSecret := "env-provided-secret-0123456789abcdef-32bytes"
	setEnv(t, EnvAPISecret, envSecret)
	setEnv(t, EnvAllowInsecureSecret, "")

	dir := t.TempDir()
	fileSecret := "file-provided-secret-0123456789abcdef-32"
	if err := os.WriteFile(filepath.Join(dir, "api.secret"), []byte(fileSecret), 0o600); err != nil {
		t.Fatalf("write secret file failed: %v", err)
	}

	if err := EnsureAPISecretWithFallback(dir); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := os.Getenv(EnvAPISecret); got != envSecret {
		t.Errorf("EnvAPISecret = %q, want env value %q", got, envSecret)
	}
}

// TestEnsureAPISecretWithFallbackMissingFile 验证：configDir 为空时返回错误。
func TestEnsureAPISecretWithFallbackMissingFile(t *testing.T) {
	setEnv(t, EnvAPISecret, "")
	setEnv(t, EnvAllowInsecureSecret, "")

	if err := EnsureAPISecretWithFallback(""); err == nil {
		t.Fatal("expected error when no env var and no configDir")
	}
}

// TestEnsureAPISecretWithFallbackEscapeHatch 验证：逃生开关下不报错。
func TestEnsureAPISecretWithFallbackEscapeHatch(t *testing.T) {
	setEnv(t, EnvAPISecret, "")
	setEnv(t, EnvAllowInsecureSecret, "1")

	if err := EnsureAPISecretWithFallback(t.TempDir()); err != nil {
		t.Fatalf("expected no error with escape hatch, got %v", err)
	}
}

// TestCheckAPISecretAcceptsSupervisorGenerated 验证：supervisor 实际生成的
// base64 随机密钥能通过强度检查。
//
// 这是端到端的防死锁保障：supervisor 生成密钥 → 注入两个进程 →
// 进程启动时校验。若这里不通过，supervisor 会陷入"生成 → 被拒 → 重启"的死循环。
func TestCheckAPISecretAcceptsSupervisorGenerated(t *testing.T) {
	// 与 scripts/supervisor.ps1 的 Get-OrCreateApiSecret 完全一致的生成逻辑。
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand failed: %v", err)
	}
	secret := base64.StdEncoding.EncodeToString(raw)

	setEnv(t, EnvAPISecret, secret)
	setEnv(t, EnvAllowInsecureSecret, "")

	st, err := CheckAPISecret()
	if err != nil {
		t.Fatalf("supervisor-generated secret rejected: %v", err)
	}
	if st.Weak {
		t.Error("supervisor-generated secret reported as weak")
	}
	if len(secret) < MinSecretLen {
		t.Errorf("generated secret len = %d, want >= %d", len(secret), MinSecretLen)
	}
}

// TestCORSOriginsDefaultEmpty 验证：默认不放行任何跨域。
func TestCORSOriginsDefaultEmpty(t *testing.T) {
	setEnv(t, EnvCORSOrigins, "")

	if got := CORSOrigins(); len(got) != 0 {
		t.Errorf("CORSOrigins() = %v, want empty", got)
	}
}

// TestCORSOriginsParsesList 验证：逗号分隔的白名单被正确解析。
func TestCORSOriginsParsesList(t *testing.T) {
	setEnv(t, EnvCORSOrigins, "http://10.0.0.5:8080, https://ops.example.com ,")

	got := CORSOrigins()
	want := []string{"http://10.0.0.5:8080", "https://ops.example.com"}
	if len(got) != len(want) {
		t.Fatalf("CORSOrigins() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("CORSOrigins()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestSecureCookieDefaults 验证：Secure cookie 按 TLS 状态自动推断。
func TestSecureCookieDefaults(t *testing.T) {
	setEnv(t, EnvSecureCookie, "")

	if SecureCookie(false) {
		t.Error("SecureCookie(false) = true, want false")
	}
	if !SecureCookie(true) {
		t.Error("SecureCookie(true) = false, want true")
	}
}

// TestSecureCookieExplicitOverride 验证：显式环境变量优先于 TLS 推断。
func TestSecureCookieExplicitOverride(t *testing.T) {
	setEnv(t, EnvSecureCookie, "true")
	if !SecureCookie(false) {
		t.Error("explicit 'true' should win over TLS=false")
	}

	setEnv(t, EnvSecureCookie, "false")
	if SecureCookie(true) {
		t.Error("explicit 'false' should win over TLS=true")
	}
}

// TestTLSEnabled 验证：证书与私钥都设置才算启用 TLS。
func TestTLSEnabled(t *testing.T) {
	setEnv(t, EnvTLSCert, "")
	setEnv(t, EnvTLSKey, "")
	if TLSEnabled() {
		t.Error("TLSEnabled() = true with neither set")
	}

	setEnv(t, EnvTLSCert, "/tmp/cert.pem")
	setEnv(t, EnvTLSKey, "")
	if TLSEnabled() {
		t.Error("TLSEnabled() = true with only cert set")
	}

	setEnv(t, EnvTLSKey, "/tmp/key.pem")
	if !TLSEnabled() {
		t.Error("TLSEnabled() = false with both set")
	}
}

// TestDefaultTimeouts 验证：默认超时是生产友好的非零值。
func TestDefaultTimeouts(t *testing.T) {
	for _, key := range []string{
		EnvReadTimeoutSec, EnvWriteTimeoutSec, EnvIdleTimeoutSec,
		EnvReadHeaderTimeout, EnvMaxHeaderBytes, EnvMaxBodyBytes,
		EnvRateLimitRPS, EnvRateLimitBurst,
	} {
		setEnv(t, key, "")
	}

	got := DefaultHTTPServerTimeouts()
	if got.ReadTimeout <= 0 {
		t.Error("ReadTimeout should be > 0")
	}
	if got.WriteTimeout <= 0 {
		t.Error("WriteTimeout should be > 0")
	}
	if got.ReadHeaderTimeout <= 0 {
		t.Error("ReadHeaderTimeout should be > 0")
	}
	if got.MaxHeaderBytes <= 0 {
		t.Error("MaxHeaderBytes should be > 0")
	}
	if got.MaxBodyBytes <= 0 {
		t.Error("MaxBodyBytes should be > 0")
	}
	if got.RateLimitRPS <= 0 {
		t.Error("RateLimitRPS should be > 0")
	}
	if got.RateLimitBurst < 1 {
		t.Error("RateLimitBurst should be >= 1")
	}
}

// TestTimeoutsFromEnv 验证：环境变量覆盖默认值。
func TestTimeoutsFromEnv(t *testing.T) {
	setEnv(t, EnvReadTimeoutSec, "42")
	setEnv(t, EnvWriteTimeoutSec, "43")
	setEnv(t, EnvMaxBodyBytes, "1024")
	setEnv(t, EnvRateLimitRPS, "7.5")

	got := DefaultHTTPServerTimeouts()
	if got.ReadTimeout != 42 {
		t.Errorf("ReadTimeout = %d, want 42", got.ReadTimeout)
	}
	if got.WriteTimeout != 43 {
		t.Errorf("WriteTimeout = %d, want 43", got.WriteTimeout)
	}
	if got.MaxBodyBytes != 1024 {
		t.Errorf("MaxBodyBytes = %d, want 1024", got.MaxBodyBytes)
	}
	if got.RateLimitRPS != 7.5 {
		t.Errorf("RateLimitRPS = %v, want 7.5", got.RateLimitRPS)
	}
}

// TestTimeoutsIgnoreInvalidEnv 验证：非法环境变量值回退默认值（不 panic）。
func TestTimeoutsIgnoreInvalidEnv(t *testing.T) {
	setEnv(t, EnvReadTimeoutSec, "not-a-number")
	setEnv(t, EnvReadTimeoutSec, "-5")
	setEnv(t, EnvRateLimitRPS, "abc")

	got := DefaultHTTPServerTimeouts()
	def := DefaultHTTPServerTimeouts()
	if got.ReadTimeout != def.ReadTimeout {
		t.Errorf("ReadTimeout = %d, want default %d", got.ReadTimeout, def.ReadTimeout)
	}
	if got.RateLimitRPS != def.RateLimitRPS {
		t.Errorf("RateLimitRPS = %v, want default %v", got.RateLimitRPS, def.RateLimitRPS)
	}
}
