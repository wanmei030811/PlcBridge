// Plan2 主题 C 5.2：Store.WithTx 事务抽象单元测试。
package web

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	_ "modernc.org/sqlite" // SQLite 驱动
)

// newTestStore 创建测试用 Store（内存 SQLite）。
func newTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &Store{db: db}
}

// TestStore_WithTx_CommitSuccess 验证 fn 返回 nil 时事务提交。
func TestStore_WithTx_CommitSuccess(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_, _ = s.db.ExecContext(ctx, `CREATE TABLE kv (k TEXT PRIMARY KEY, v INTEGER)`)

	err := s.WithTx(ctx, func(tx Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO kv VALUES (?, ?)`, "a", 1)
		return err
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
	// 提交后行可见
	var v int
	if err := s.db.QueryRowContext(ctx, `SELECT v FROM kv WHERE k='a'`).Scan(&v); err != nil {
		t.Fatalf("read after commit: %v", err)
	}
	if v != 1 {
		t.Errorf("v = %d, want 1", v)
	}
}

// TestStore_WithTx_RollbackOnError 验证 fn 返回 error 时事务回滚。
func TestStore_WithTx_RollbackOnError(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_, _ = s.db.ExecContext(ctx, `CREATE TABLE kv (k TEXT PRIMARY KEY, v INTEGER)`)

	myErr := errors.New("intentional")
	err := s.WithTx(ctx, func(tx Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO kv VALUES (?, ?)`, "b", 2)
		if err != nil {
			return err
		}
		return myErr // 触发回滚
	})
	if !errors.Is(err, myErr) {
		t.Errorf("WithTx 应透传 fn error: got %v, want %v", err, myErr)
	}
	// 回滚后行不可见
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kv WHERE k='b'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("rollback 后行应不存在, got %d", n)
	}
}

// TestStore_WithTx_MultiStep 验证多步写入要么全成功、要么全回滚。
func TestStore_WithTx_MultiStep(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_, _ = s.db.ExecContext(ctx, `CREATE TABLE t1 (id INTEGER PRIMARY KEY, v TEXT)`)
	_, _ = s.db.ExecContext(ctx, `CREATE TABLE t2 (id INTEGER PRIMARY KEY, v TEXT)`)

	// 模拟第二步失败
	err := s.WithTx(ctx, func(tx Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO t1 (id, v) VALUES (1, 'x')`); err != nil {
			return err
		}
		// 强制主键冲突：t1(1) 已存在 → 失败
		if _, err := tx.ExecContext(ctx, `INSERT INTO t1 (id, v) VALUES (1, 'y')`); err != nil {
			return err
		}
		_, _ = tx.ExecContext(ctx, `INSERT INTO t2 (id, v) VALUES (1, 'z')`)
		return nil
	})
	if err == nil {
		t.Fatal("应返回主键冲突错误")
	}
	// t2 不应有数据
	var n int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM t2`).Scan(&n)
	if n != 0 {
		t.Errorf("第二步失败后 t2 应回滚, got %d 行", n)
	}
	// t1 也不应有数据
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM t1`).Scan(&n)
	if n != 0 {
		t.Errorf("第二步失败后 t1 应回滚, got %d 行", n)
	}
}

// TestStore_WithTx_QueryInsideTx 验证事务内的 QueryRow 能看到本事务的写入。
//
// 隔离性：fn 内插入后立即 QueryRow 应该能看到。
func TestStore_WithTx_QueryInsideTx(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_, _ = s.db.ExecContext(ctx, `CREATE TABLE kv (k TEXT PRIMARY KEY, v INTEGER)`)

	err := s.WithTx(ctx, func(tx Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO kv VALUES (?, ?)`, "k1", 42); err != nil {
			return err
		}
		var v int
		if err := tx.QueryRowContext(ctx, `SELECT v FROM kv WHERE k=?`, "k1").Scan(&v); err != nil {
			return err
		}
		if v != 42 {
			t.Errorf("事务内 QueryRow v = %d, want 42", v)
		}
		return nil
	})
	if err != nil {
		t.Errorf("WithTx: %v", err)
	}
}
