// snapshots.go：配置变更历史与回滚（Plan3 3.3）。
//
// 功能：
//
//	GET  /api/v1/config/snapshots        → 列出所有配置快照
//	GET  /api/v1/config/snapshots/:id    → 获取单个快照详情（含 config_json）
//	POST /api/v1/config/snapshots        → 手动创建快照
//	DELETE /api/v1/config/snapshots/:id  → 删除快照
//	POST /api/v1/config/snapshots/:id/rollback → 回滚到指定快照
//	GET  /api/v1/config/snapshots/:id/diff   → 与当前配置对比（diff）
//
// 快照内容：整个数据库实体集合（channels/devices/sources/points/targets）的 JSON。
// 保留策略：最新 50 个（CreateSnapshot 里自动 prune）。
package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/plcbridge/plcbridge/internal/infra/logging"
)

// snapshotDTO 是快照列表项（不含庞大的 config_json）。
type snapshotDTO struct {
	ID          int64  `json:"id"`
	Description string `json:"description"`
	CreatedBy   string `json:"created_by"`
	RecordCount int    `json:"record_count"`
	IsAuto      bool   `json:"is_auto"`
	CreatedAt   int64  `json:"created_at"`
	CreatedAtS  string `json:"created_at_str"`
	SizeBytes   int    `json:"size_bytes"`
}

// snapshotDetailDTO 是单个快照的完整信息。
type snapshotDetailDTO struct {
	snapshotDTO
	ConfigJSON string `json:"config_json"`
}

func toSnapshotDTO(r ConfigSnapshotRow) snapshotDTO {
	return snapshotDTO{
		ID:          r.ID,
		Description: r.Description,
		CreatedBy:   r.CreatedBy,
		RecordCount: r.RecordCount,
		IsAuto:      r.IsAuto,
		CreatedAt:   r.CreatedAt,
		CreatedAtS:  time.UnixMilli(r.CreatedAt).UTC().Format(time.RFC3339),
		SizeBytes:   len(r.ConfigJSON),
	}
}

// listSnapshotsHandler GET /api/v1/config/snapshots
func (s *Store) listSnapshotsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		rows, err := s.ListSnapshots(ctx)
		if err != nil {
			WriteError(c, ErrInternal.With(err.Error()))
			return
		}
		items := make([]snapshotDTO, 0, len(rows))
		for _, r := range rows {
			items = append(items, toSnapshotDTO(r))
		}
		c.JSON(http.StatusOK, gin.H{"items": items})
	}
}

// getSnapshotHandler GET /api/v1/config/snapshots/:id
func (s *Store) getSnapshotHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil {
			WriteError(c, ErrInvalidParam.With("invalid snapshot id"))
			return
		}
		row, err := s.GetSnapshot(c.Request.Context(), id)
		if err == ErrNotFound {
			WriteError(c, ErrAPINotFound.With("snapshot not found"))
			return
		}
		if err != nil {
			WriteError(c, ErrInternal.With(err.Error()))
			return
		}
		dto := snapshotDetailDTO{
			snapshotDTO: toSnapshotDTO(*row),
			ConfigJSON:  row.ConfigJSON,
		}
		c.JSON(http.StatusOK, dto)
	}
}

// createSnapshotHandler POST /api/v1/config/snapshots
//
// body: { "description": "手工快照" }
func (s *Store) createSnapshotHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		var body struct {
			Description string `json:"description"`
		}
		_ = c.ShouldBindJSON(&body) // 可选 body
		if body.Description == "" {
			body.Description = "手动创建快照"
		}
		createdBy := c.GetString("session_user")
		if createdBy == "" {
			createdBy = "unknown"
		}
		cfgJSON, count, err := s.BuildFullConfigJSON(ctx)
		if err != nil {
			WriteError(c, ErrInternal.With(err.Error()))
			return
		}
		id, err := s.CreateSnapshot(ctx, body.Description, createdBy, cfgJSON, count, false)
		if err != nil {
			WriteError(c, ErrInternal.With(err.Error()))
			return
		}
		c.JSON(http.StatusCreated, gin.H{
			"id":           id,
			"description":  body.Description,
			"record_count": count,
		})
	}
}

// deleteSnapshotHandler DELETE /api/v1/config/snapshots/:id
func (s *Store) deleteSnapshotHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil {
			WriteError(c, ErrInvalidParam.With("invalid snapshot id"))
			return
		}
		if err := s.DeleteSnapshot(c.Request.Context(), id); err == ErrNotFound {
			WriteError(c, ErrAPINotFound.With("snapshot not found"))
			return
		} else if err != nil {
			WriteError(c, ErrInternal.With(err.Error()))
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true, "id": id})
	}
}

// rollbackSnapshotHandler POST /api/v1/config/snapshots/:id/rollback
//
// 回滚流程（Plan3 3.3）：
//  1. 取目标快照
//  2. 回滚前先创建"回滚前备份"快照
//  3. 事务中重建所有实体
//  4. 生成新快照（描述="回滚到快照 {id}"）
//  5. **导出 config.json → 触发 plcbridge 热加载**
//
// 第 5 步是必需的（2026-10-24 修复）：
//
//	之前只改了 SQLite，没写 config.json，而 plcbridge 主进程是靠 fsnotify
//	监听 configDir/config.json 变更来热加载的 → 回滚接口返回 200，
//	但主进程仍在跑**旧配置**，用户以为回滚成功、实际没生效。
//	Plan3 3.3 验收明确要求"plcbridge 立即热加载新配置"。
func (s *Store) rollbackSnapshotHandler(configDir string) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil {
			WriteError(c, ErrInvalidParam.With("invalid snapshot id"))
			return
		}
		row, err := s.GetSnapshot(ctx, id)
		if err == ErrNotFound {
			WriteError(c, ErrAPINotFound.With("snapshot not found"))
			return
		}
		if err != nil {
			WriteError(c, ErrInternal.With(err.Error()))
			return
		}

		createdBy := c.GetString("session_user")
		if createdBy == "" {
			createdBy = "unknown"
		}

		// 2. 回滚前自动创建备份快照
		curJSON, curCount, err := s.BuildFullConfigJSON(ctx)
		if err != nil {
			WriteError(c, ErrInternal.With(err.Error()))
			return
		}
		if _, err := s.CreateSnapshot(ctx,
			fmt.Sprintf("回滚前备份（目标快照 #%d）", id),
			createdBy, curJSON, curCount, true); err != nil {
			WriteError(c, ErrInternal.With(err.Error()))
			return
		}

		// 3. 事务中重建
		if _, err := s.RollbackToSnapshot(ctx, id, row.ConfigJSON, ""); err != nil {
			WriteError(c, ErrInternal.With(err.Error()))
			return
		}

		// 4. 生成新快照（描述="回滚到快照 {id}"）
		newJSON, newCount, err := s.BuildFullConfigJSON(ctx)
		if err != nil {
			WriteError(c, ErrInternal.With(err.Error()))
			return
		}
		newID, err := s.CreateSnapshot(ctx,
			fmt.Sprintf("回滚到快照 #%d", id),
			createdBy, newJSON, newCount, true)
		if err != nil {
			WriteError(c, ErrInternal.With(err.Error()))
			return
		}

		// 5. 导出 config.json → plcbridge 通过 fsnotify 自动热加载。
		//
		// 为什么放在响应之前算失败：数据库已经改了，导出失败不能改成 500
		// （会误导用户以为回滚失败、然后重复回滚）。
		// 但必须让用户知道热加载没触发 —— 否则主进程还在跑旧配置，
		// 用户以为回滚成功、实际没生效。
		reloaded := true
		if err := reloadConfigViaExport(context.Background(), s.db, configDir); err != nil {
			logging.Warn(logging.EventConfigExport).
				Str("service", "web").
				Err(err).
				Msg("回滚后导出 config.json 失败，plcbridge 不会自动热加载")
			reloaded = false
		}

		msg := fmt.Sprintf("已回滚到快照 #%d", id)
		if !reloaded {
			msg = fmt.Sprintf("已回滚到快照 #%d（但配置导出失败，plcbridge 未热加载，请手动重载配置）", id)
		}
		c.JSON(http.StatusOK, gin.H{
			"rollback":     true,
			"snapshot_id":  id,
			"new_snapshot": newID,
			"message":      msg,
			"reload":       reloaded,
		})
	}
}

// snapshotDiffEntry 是一条 diff 结果。
type snapshotDiffEntry struct {
	Entity   string `json:"entity"` // channels / devices / sources / points / targets
	Key      string `json:"key"`    // biz_id
	Field    string `json:"field"`  // 字段名（新增/删除时为空）
	OldValue string `json:"old_value"`
	NewValue string `json:"new_value"`
	Change   string `json:"change"` // added / removed / modified
}

// diffSnapshotHandler GET /api/v1/config/snapshots/:id/diff
//
// 与**当前**数据库状态做对比，返回结构化差异列表。
func (s *Store) diffSnapshotHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil {
			WriteError(c, ErrInvalidParam.With("invalid snapshot id"))
			return
		}
		row, err := s.GetSnapshot(ctx, id)
		if err == ErrNotFound {
			WriteError(c, ErrAPINotFound.With("snapshot not found"))
			return
		}
		if err != nil {
			WriteError(c, ErrInternal.With(err.Error()))
			return
		}
		curJSON, _, err := s.BuildFullConfigJSON(ctx)
		if err != nil {
			WriteError(c, ErrInternal.With(err.Error()))
			return
		}
		diffs := diffConfigJSON(row.ConfigJSON, curJSON)
		c.JSON(http.StatusOK, gin.H{
			"snapshot_id": id,
			"diffs":       diffs,
			"count":       len(diffs),
		})
	}
}

// diffConfigJSON 比较两份配置 JSON（结构相同），返回差异列表。
func diffConfigJSON(oldJSON, newJSON string) []snapshotDiffEntry {
	var oldCfg, newCfg map[string]any
	if err := json.Unmarshal([]byte(oldJSON), &oldCfg); err != nil {
		return nil
	}
	if err := json.Unmarshal([]byte(newJSON), &newCfg); err != nil {
		return nil
	}

	entities := []string{"channels", "devices", "sources", "points", "targets"}
	var out []snapshotDiffEntry

	for _, ent := range entities {
		oldItems, _ := oldCfg[ent].([]any)
		newItems, _ := newCfg[ent].([]any)

		oldMap := indexByBizID(ent, oldItems)
		newMap := indexByBizID(ent, newItems)

		keys := make([]string, 0, len(oldMap)+len(newMap))
		seen := map[string]bool{}
		for k := range oldMap {
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
		for k := range newMap {
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)

		for _, k := range keys {
			o, hasOld := oldMap[k]
			n, hasNew := newMap[k]
			switch {
			case !hasOld && hasNew:
				out = append(out, snapshotDiffEntry{
					Entity: ent, Key: k, Change: "added",
					NewValue: compactJSON(n),
				})
			case hasOld && !hasNew:
				out = append(out, snapshotDiffEntry{
					Entity: ent, Key: k, Change: "removed",
					OldValue: compactJSON(o),
				})
			default:
				for _, d := range compareMaps(o, n) {
					d.Entity = ent
					d.Key = k
					out = append(out, d)
				}
			}
		}
	}
	return out
}

// bizIDFieldFor 返回各实体类型用来做唯一标识的字段名。
func bizIDFieldFor(entity string) string {
	switch entity {
	case "channels":
		return "channel_biz_id"
	case "devices":
		return "device_biz_id"
	case "sources":
		return "source_biz_id"
	case "points":
		return "point_biz_id"
	case "targets":
		return "target_biz_id"
	}
	return "id"
}

// indexByBizID 把实体数组转成 biz_id → map 的索引。
func indexByBizID(entity string, items []any) map[string]map[string]any {
	field := bizIDFieldFor(entity)
	out := map[string]map[string]any{}
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		key := fmt.Sprintf("%v", m[field])
		out[key] = m
	}
	return out
}

// compareMaps 比较两个实体的字段差异。
func compareMaps(oldM, newM map[string]any) []snapshotDiffEntry {
	var out []snapshotDiffEntry
	fields := map[string]bool{}
	for k := range oldM {
		fields[k] = true
	}
	for k := range newM {
		fields[k] = true
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		ov, hasOld := oldM[k]
		nv, hasNew := newM[k]
		os := fmt.Sprintf("%v", ov)
		ns := fmt.Sprintf("%v", nv)
		if os == ns {
			continue
		}
		change := "modified"
		if !hasOld {
			change = "added"
		} else if !hasNew {
			change = "removed"
		}
		out = append(out, snapshotDiffEntry{
			Field:    k,
			OldValue: os,
			NewValue: ns,
			Change:   change,
		})
	}
	return out
}

// compactJSON 把 map 序列化为紧凑 JSON 字符串（用于 diff 展示）。
func compactJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// AutoSnapshotOnChange 在配置变更后自动创建快照（供 CRUD handler 调用）。
//
// 这是 Plan3 3.3 "API 调用 Update* 时自动创建快照" 的实现。
// 失败只记日志，不影响主流程。
func (s *Store) AutoSnapshotOnChange(ctx context.Context, username, description string) {
	if username == "" {
		username = "system"
	}
	cfgJSON, count, err := s.BuildFullConfigJSON(ctx)
	if err != nil {
		return
	}
	_, _ = s.CreateSnapshot(ctx, description, username, cfgJSON, count, true)
}

// configWatchedPaths 需要触发「配置已变更」副作用的路径前缀。
//
// 两处使用，必须保持一致：
//  1. autoSnapshotMiddleware —— 配置变更后自动创建快照
//  2. autoExportTriggerMiddleware —— 配置变更后自动导出 config.json
//
// 如果两处判定不一致，会出现"角标说还没导出，但自动导出已经导了"
// 或"自动导出了但快照没建"的矛盾状态。抽成共享变量杜绝这种漂移。
var configWatchedPaths = []string{
	"/api/v1/channels",
	"/api/v1/devices",
	"/api/v1/sources",
	"/api/v1/points",
	"/api/v1/targets",
}

// configExcludedPaths 明确排除的路径前缀（避免快照风暴 / 递归）。
var configExcludedPaths = []string{
	"/api/v1/config/snapshots",
	"/api/v1/config/backup",
	"/api/v1/config/backups",
	"/api/v1/config/restore",
	"/api/v1/auth",
	"/api/v1/tokens",
	"/api/v1/dashboard",
}

// isConfigMutatingPath 判断一个请求路径是否属于"配置变更"。
//
// 规则：方法 ∈ {POST, PUT, DELETE} 且路径命中 configWatchedPaths 且不在排除表里。
func isConfigMutatingPath(method, path string) bool {
	if method != http.MethodPost && method != http.MethodPut && method != http.MethodDelete {
		return false
	}
	for _, p := range configExcludedPaths {
		if path == p || (len(path) > len(p) && path[:len(p)] == p) {
			return false
		}
	}
	for _, p := range configWatchedPaths {
		if path == p || (len(path) > len(p) && path[:len(p)] == p) {
			return true
		}
	}
	return false
}

// autoSnapshotMiddleware 是 gin 中间件：写操作成功后自动创建配置快照。
//
// 触发条件：
//   - HTTP 方法为 POST / PUT / DELETE
//   - 响应状态码为 2xx
//   - 路径命中配置类实体（channels/devices/sources/points/targets）
//
// 排除路径（避免快照风暴 / 递归）：
//   - /config/snapshots*   —— 快照自身操作
//   - /config/backup*      —— 备份 / 恢复
//   - /auth/*              —— 账号相关
//   - /tokens*             —— Token 管理不改配置
//   - /dashboard*          —— 只读
func (s *Store) autoSnapshotMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		method := c.Request.Method

		if !isConfigMutatingPath(method, path) {
			c.Next()
			return
		}

		c.Next() // 先执行 handler

		// 只有成功才快照
		if c.Writer.Status() < 200 || c.Writer.Status() >= 300 {
			return
		}
		user := c.GetString("session_user")
		if user == "" {
			user = "system"
		}
		s.AutoSnapshotOnChange(c.Request.Context(), user,
			fmt.Sprintf("%s %s 修改了配置", user, method))
	}
}
