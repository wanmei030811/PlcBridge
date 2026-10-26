// 配置备份（Plan2）端点测试。
//
// 覆盖 2026-10-21 修复的 bug：
//
//	getCurrentDBPath 用 Scan(nil, "main", &path) 读 PRAGMA database_list，
//	而 nil 不是合法 scan 目标 → database/sql 报 "destination not a pointer"，
//	错误又被 `_ =` 吞掉 → path 恒为空 → 导出备份 500 "无法读取 SQLite 数据库文件"。
package web

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestGetCurrentDBPath 验证能正确解析出 SQLite 主库文件路径。
//
// 回归测试：修复前该函数恒返回 ""。
func TestGetCurrentDBPath(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}

	got := getCurrentDBPath(db)
	if got == "" {
		t.Fatal("getCurrentDBPath 返回空串（PRAGMA database_list 解析失败）")
	}
	// 路径应指向我们创建的库文件
	if filepath.Base(got) != "test.db" {
		t.Errorf("路径 = %q, want 结尾为 test.db", got)
	}
	// 实际可读
	if _, err := os.Stat(got); err != nil {
		t.Errorf("返回的路径不可读: %v", err)
	}
}

// TestGetCurrentDBPath_ClosedDB 验证 DB 不可用时返回空串而非 panic。
func TestGetCurrentDBPath_ClosedDB(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if got := getCurrentDBPath(db); got != "" {
		t.Errorf("已关闭的 DB 应返回空串, got %q", got)
	}
}

// TestExportBackup 验证导出备份端点不再 500。
//
// 回归测试：修复前返回 500 {"code":"internal_error","msg":"无法读取 SQLite 数据库文件"}。
func TestExportBackup(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()
	cookie := loginAndCookie(t, r)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodPost, "/api/v1/config/backup", cookie, nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		OK       bool   `json:"ok"`
		Filename string `json:"filename"`
	}
	decodeJSON(t, w, &resp)
	if !resp.OK {
		t.Error("ok 应为 true")
	}
	if resp.Filename == "" {
		t.Error("filename 不应为空")
	}
}

// TestExportBackup_ZipContainsDB 验证导出的 zip 里含有一致的数据库快照。
//
// 2026-10-21：改用 VACUUM INTO 生成快照，保证 WAL 里的数据也被包含。
func TestExportBackup_ZipContainsDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureAccount(db, "admin", "Password123!"); err != nil {
		t.Fatal(err)
	}

	store := &Store{db: db}
	cfgDir := filepath.Join(dir, "cfg")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}

	bytesOut, err := store.snapshotDB(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(bytesOut) == 0 {
		t.Fatal("snapshotDB 返回空内容")
	}
	// 快照应是一个可打开的 SQLite 库
	tmp := filepath.Join(dir, "snap.db")
	if err := os.WriteFile(tmp, bytesOut, 0o644); err != nil {
		t.Fatal(err)
	}
	snap, err := sql.Open("sqlite", tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	var n int
	if err := snap.QueryRow("SELECT COUNT(*) FROM accounts").Scan(&n); err != nil {
		t.Fatalf("快照不是有效 SQLite 库: %v", err)
	}
	if n != 1 {
		t.Errorf("快照里 accounts = %d, want 1（WAL 数据未包含）", n)
	}
}

// TestExportBackupHandler_Zip 端到端：调 handler 后检查 zip 内容。
func TestExportBackupHandler_Zip(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	store := &Store{db: db}
	cfgDir := filepath.Join(dir, "cfg")

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/config/backup", nil)
	store.exportBackupHandler(cfgDir)(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Filename string `json:"filename"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	zipPath := filepath.Join(cfgDir, "backups", resp.Filename)
	raw, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatalf("备份文件不存在: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	if !names["store.db"] {
		t.Errorf("zip 缺 store.db，实际包含: %v", names)
	}
	if !names["meta.json"] {
		t.Errorf("zip 缺 meta.json，实际包含: %v", names)
	}
	// store.db 应非空
	for _, f := range zr.File {
		if f.Name != "store.db" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(rc)
		rc.Close()
		if len(data) == 0 {
			t.Error("store.db 内容为空")
		}
	}
}
