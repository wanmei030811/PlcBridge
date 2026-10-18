// CRUD handlers（Channels / Devices / Sources / Points / Targets）。
//
// 设计原则：
//   - JSON 入参/出参；错误统一 gin.H{"error": msg}
//   - 400 参数错误 / 401 未登录（中间件拦）/ 404 不存在 / 409 唯一约束冲突
//   - 列表接口返回 []（绝不返回 null；空时给 []）
//   - 创建返回 id + 完整 DTO；更新返回更新后的 DTO；删除返回 {"ok": true}
package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// helper：parseID
func parseID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return 0, false
	}
	return id, true
}

// handleStoreErr 把 Store 错误翻译成 HTTP（Phase 1.2 仍走 gin.H{"error": msg} 保持兼容）。
//
// 设计：
//   - 保留 {error: msg} 响应体（前端 client.ts 依赖此字段）
//   - 新错误体系 APIError / WriteError 已就位，但 handleStoreErr 是"过渡期 API"，
//     不引入新格式以免一次性破坏前端。
//   - 后续阶段（独立 PR）会同时返回 {code, msg, error} 三字段，前端逐步迁移。
func handleStoreErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
	case errors.Is(err, ErrConflict):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	case isFKConstraintErr(err):
		c.JSON(http.StatusConflict, gin.H{"error": "外键约束失败：" + err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}

// isFKConstraintErr 判断 SQLite 外键约束失败（FOREIGN KEY constraint failed）。
func isFKConstraintErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "FOREIGN KEY constraint failed") ||
		strings.Contains(msg, "foreign key constraint failed")
}

// === 引用完整性预校验 ===

func assertChannelExists(ctx context.Context, s *Store, bizID string) error {
	var x int
	err := s.DB().QueryRowContext(ctx, `SELECT 1 FROM channels WHERE channel_biz_id=?`, bizID).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("channel_biz_id 不存在: " + bizID)
	}
	return err
}

func assertChannelRole(ctx context.Context, s *Store, bizID, wantRole string) error {
	var role string
	err := s.DB().QueryRowContext(ctx, `SELECT role FROM channels WHERE channel_biz_id=?`, bizID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("channel_biz_id 不存在: " + bizID)
	}
	if err != nil {
		return err
	}
	if role != wantRole {
		return fmt.Errorf("channel_biz_id=%s role=%s，需 %s", bizID, role, wantRole)
	}
	return nil
}

func assertDeviceExists(ctx context.Context, s *Store, id int64) error {
	var x int
	err := s.DB().QueryRowContext(ctx, `SELECT 1 FROM devices WHERE id=?`, id).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("device_id 不存在: " + strconv.FormatInt(id, 10))
	}
	return err
}

func assertSourceExists(ctx context.Context, s *Store, id int64) error {
	var x int
	err := s.DB().QueryRowContext(ctx, `SELECT 1 FROM sources WHERE id=?`, id).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("source_id 不存在: " + strconv.FormatInt(id, 10))
	}
	return err
}

// 必须返回 []（不返回 null）的小工具。
func mustSlice[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// ============================================================
// Channels
// ============================================================

func listChannels(db *sql.DB) gin.HandlerFunc {
	store := NewStore(db)
	return func(c *gin.Context) {
		out, err := store.ListChannels(c.Request.Context())
		if err != nil {
			handleStoreErr(c, err)
			return
		}
		if out == nil {
			out = []ChannelDTO{}
		}
		c.JSON(http.StatusOK, gin.H{"items": out})
	}
}

func getChannel(db *sql.DB) gin.HandlerFunc {
	store := NewStore(db)
	return func(c *gin.Context) {
		id, ok := parseID(c)
		if !ok {
			return
		}
		out, err := store.GetChannel(c.Request.Context(), id)
		if err != nil {
			handleStoreErr(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

func createChannel(db *sql.DB) gin.HandlerFunc {
	store := NewStore(db)
	return func(c *gin.Context) {
		var req ChannelDTO
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if req.ChannelBizID == "" || req.Role == "" || req.Type == "" || req.Address == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "channel_biz_id/role/type/address 必填"})
			return
		}
		if req.Role != "collect" && req.Role != "forward" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "role 必须是 collect 或 forward"})
			return
		}
		if !isKnownChannelType(req.Role, req.Type) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "type 与 role 不匹配或未知 type=" + req.Type})
			return
		}
		id, err := store.CreateChannel(c.Request.Context(), &req)
		if err != nil {
			handleStoreErr(c, err)
			return
		}
		out, _ := store.GetChannel(c.Request.Context(), id)
		c.JSON(http.StatusCreated, out)
	}
}

// isKnownChannelType 校验 channel.type 在 role 下是否合法。
//
// collect 支持：s7 / opcua / modbus
// forward 支持：mqtt / influxdb
func isKnownChannelType(role, typ string) bool {
	switch role {
	case "collect":
		switch typ {
		case "s7", "opcua", "modbus":
			return true
		}
	case "forward":
		switch typ {
		case "mqtt", "influxdb":
			return true
		}
	}
	return false
}

func updateChannel(db *sql.DB) gin.HandlerFunc {
	store := NewStore(db)
	return func(c *gin.Context) {
		id, ok := parseID(c)
		if !ok {
			return
		}
		var req ChannelDTO
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := store.UpdateChannel(c.Request.Context(), id, &req); err != nil {
			handleStoreErr(c, err)
			return
		}
		out, _ := store.GetChannel(c.Request.Context(), id)
		c.JSON(http.StatusOK, out)
	}
}

func deleteChannel(db *sql.DB) gin.HandlerFunc {
	store := NewStore(db)
	return func(c *gin.Context) {
		id, ok := parseID(c)
		if !ok {
			return
		}
		if err := store.DeleteChannel(c.Request.Context(), id); err != nil {
			handleStoreErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

// ============================================================
// Devices
// ============================================================

func listDevices(db *sql.DB) gin.HandlerFunc {
	store := NewStore(db)
	return func(c *gin.Context) {
		out, err := store.ListDevices(c.Request.Context())
		if err != nil {
			handleStoreErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": mustSlice(out)})
	}
}

func getDevice(db *sql.DB) gin.HandlerFunc {
	store := NewStore(db)
	return func(c *gin.Context) {
		id, ok := parseID(c)
		if !ok {
			return
		}
		out, err := store.GetDevice(c.Request.Context(), id)
		if err != nil {
			handleStoreErr(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

func createDevice(db *sql.DB) gin.HandlerFunc {
	store := NewStore(db)
	return func(c *gin.Context) {
		var req DeviceDTO
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if req.DeviceBizID == "" || req.Name == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "device_biz_id/name 必填"})
			return
		}
		id, err := store.CreateDevice(c.Request.Context(), &req)
		if err != nil {
			handleStoreErr(c, err)
			return
		}
		out, _ := store.GetDevice(c.Request.Context(), id)
		c.JSON(http.StatusCreated, out)
	}
}

func updateDevice(db *sql.DB) gin.HandlerFunc {
	store := NewStore(db)
	return func(c *gin.Context) {
		id, ok := parseID(c)
		if !ok {
			return
		}
		var req DeviceDTO
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if req.DeviceBizID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "device_biz_id 必填"})
			return
		}
		if req.Name == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name 必填"})
			return
		}
		if err := store.UpdateDevice(c.Request.Context(), id, &req); err != nil {
			handleStoreErr(c, err)
			return
		}
		out, _ := store.GetDevice(c.Request.Context(), id)
		c.JSON(http.StatusOK, out)
	}
}

func deleteDevice(db *sql.DB) gin.HandlerFunc {
	store := NewStore(db)
	return func(c *gin.Context) {
		id, ok := parseID(c)
		if !ok {
			return
		}
		if err := store.DeleteDevice(c.Request.Context(), id); err != nil {
			handleStoreErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func listDeviceSources(db *sql.DB) gin.HandlerFunc {
	store := NewStore(db)
	return func(c *gin.Context) {
		id, ok := parseID(c)
		if !ok {
			return
		}
		out, err := store.ListDeviceSources(c.Request.Context(), id)
		if err != nil {
			handleStoreErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": mustSlice(out)})
	}
}

// ============================================================
// Sources
// ============================================================

func getSource(db *sql.DB) gin.HandlerFunc {
	store := NewStore(db)
	return func(c *gin.Context) {
		id, ok := parseID(c)
		if !ok {
			return
		}
		out, err := store.GetSource(c.Request.Context(), id)
		if err != nil {
			handleStoreErr(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

func createSource(db *sql.DB) gin.HandlerFunc {
	store := NewStore(db)
	return func(c *gin.Context) {
		var req SourceDTO
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if req.SourceBizID == "" || req.DeviceID == 0 || req.ChannelBizID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "source_biz_id/device_id/channel_biz_id 必填"})
			return
		}
		// 检查 device / channel 存在；collect 角色 channel 才能被 source 引用
		if err := assertChannelExists(c.Request.Context(), store, req.ChannelBizID); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := assertChannelRole(c.Request.Context(), store, req.ChannelBizID, "collect"); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := assertDeviceExists(c.Request.Context(), store, req.DeviceID); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		id, err := store.CreateSource(c.Request.Context(), &req)
		if err != nil {
			handleStoreErr(c, err)
			return
		}
		out, _ := store.GetSource(c.Request.Context(), id)
		c.JSON(http.StatusCreated, out)
	}
}

func updateSource(db *sql.DB) gin.HandlerFunc {
	store := NewStore(db)
	return func(c *gin.Context) {
		id, ok := parseID(c)
		if !ok {
			return
		}
		var req SourceDTO
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		// 若请求体包含 channel_biz_id，必须是 collect 角色（防止把 source 绑到 forward channel 导致采集死循环）
		if req.ChannelBizID != "" {
			if err := assertChannelRole(c.Request.Context(), store, req.ChannelBizID, "collect"); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
		}
		if err := store.UpdateSource(c.Request.Context(), id, &req); err != nil {
			handleStoreErr(c, err)
			return
		}
		out, _ := store.GetSource(c.Request.Context(), id)
		c.JSON(http.StatusOK, out)
	}
}

func deleteSource(db *sql.DB) gin.HandlerFunc {
	store := NewStore(db)
	return func(c *gin.Context) {
		id, ok := parseID(c)
		if !ok {
			return
		}
		if err := store.DeleteSource(c.Request.Context(), id); err != nil {
			handleStoreErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func listSourcePoints(db *sql.DB) gin.HandlerFunc {
	store := NewStore(db)
	return func(c *gin.Context) {
		id, ok := parseID(c)
		if !ok {
			return
		}
		out, err := store.ListSourcePoints(c.Request.Context(), id)
		if err != nil {
			handleStoreErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": mustSlice(out)})
	}
}

// ============================================================
// Points
// ============================================================

func getPoint(db *sql.DB) gin.HandlerFunc {
	store := NewStore(db)
	return func(c *gin.Context) {
		id, ok := parseID(c)
		if !ok {
			return
		}
		out, err := store.GetPoint(c.Request.Context(), id)
		if err != nil {
			handleStoreErr(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

func createPoint(db *sql.DB) gin.HandlerFunc {
	store := NewStore(db)
	return func(c *gin.Context) {
		var req PointDTO
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if req.PointBizID == "" || req.SourceID == 0 || req.Address == "" || req.DataType == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "point_biz_id/source_id/address/data_type 必填"})
			return
		}
		if !isKnownDataType(req.DataType) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "未知 data_type=" + req.DataType})
			return
		}
		if err := validateStringLength(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := assertSourceExists(c.Request.Context(), store, req.SourceID); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		id, err := store.CreatePoint(c.Request.Context(), &req)
		if err != nil {
			handleStoreErr(c, err)
			return
		}
		out, _ := store.GetPoint(c.Request.Context(), id)
		c.JSON(http.StatusCreated, out)
	}
}

// validateStringLength data_type=="string" 时强制 string_length 在 1..55（Plan §6.5 spec）；
// 非 string 字段忽略 string_length（前端可省略）。
func validateStringLength(p *PointDTO) error {
	if p.DataType != "string" {
		p.StringLength = 0
		return nil
	}
	if p.StringLength < 1 || p.StringLength > 55 {
		return fmt.Errorf("data_type=string 时 string_length 必须在 1..55 (got %d)", p.StringLength)
	}
	return nil
}

// isKnownDataType 校验 point.data_type 是受支持的类型（与 protocol/target 一致）。
func isKnownDataType(dt string) bool {
	switch dt {
	case "bool", "int8", "uint8",
		"int16", "uint16", "int32", "uint32",
		"int64", "uint64",
		"float32", "float64", "string":
		return true
	}
	return false
}

func updatePoint(db *sql.DB) gin.HandlerFunc {
	store := NewStore(db)
	return func(c *gin.Context) {
		id, ok := parseID(c)
		if !ok {
			return
		}
		var req PointDTO
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if req.Address == "" || req.DataType == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "address/data_type 必填"})
			return
		}
		if !isKnownDataType(req.DataType) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "未知 data_type=" + req.DataType})
			return
		}
		if err := validateStringLength(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := store.UpdatePoint(c.Request.Context(), id, &req); err != nil {
			handleStoreErr(c, err)
			return
		}
		out, _ := store.GetPoint(c.Request.Context(), id)
		c.JSON(http.StatusOK, out)
	}
}

func deletePoint(db *sql.DB) gin.HandlerFunc {
	store := NewStore(db)
	return func(c *gin.Context) {
		id, ok := parseID(c)
		if !ok {
			return
		}
		if err := store.DeletePoint(c.Request.Context(), id); err != nil {
			handleStoreErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

// ============================================================
// Targets
// ============================================================

func listTargets(db *sql.DB) gin.HandlerFunc {
	store := NewStore(db)
	return func(c *gin.Context) {
		out, err := store.ListTargets(c.Request.Context())
		if err != nil {
			handleStoreErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": mustSlice(out)})
	}
}

func getTarget(db *sql.DB) gin.HandlerFunc {
	store := NewStore(db)
	return func(c *gin.Context) {
		id, ok := parseID(c)
		if !ok {
			return
		}
		out, err := store.GetTarget(c.Request.Context(), id)
		if err != nil {
			handleStoreErr(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

func createTarget(db *sql.DB) gin.HandlerFunc {
	store := NewStore(db)
	return func(c *gin.Context) {
		var req TargetDTO
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if req.TargetBizID == "" || req.DeviceID == 0 || req.ForwardChannelBizID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "target_biz_id/device_id/forward_channel_biz_id 必填"})
			return
		}
		if err := assertChannelExists(c.Request.Context(), store, req.ForwardChannelBizID); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := assertChannelRole(c.Request.Context(), store, req.ForwardChannelBizID, "forward"); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := assertDeviceExists(c.Request.Context(), store, req.DeviceID); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		id, err := store.CreateTarget(c.Request.Context(), &req)
		if err != nil {
			handleStoreErr(c, err)
			return
		}
		out, _ := store.GetTarget(c.Request.Context(), id)
		c.JSON(http.StatusCreated, out)
	}
}

func updateTarget(db *sql.DB) gin.HandlerFunc {
	store := NewStore(db)
	return func(c *gin.Context) {
		id, ok := parseID(c)
		if !ok {
			return
		}
		var req TargetDTO
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if req.TargetBizID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "target_biz_id 必填"})
			return
		}
		if req.ForwardChannelBizID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "forward_channel_biz_id 必填"})
			return
		}
		// 校验绑定的 channel 角色必须是 forward（防止 target 绑到 collect channel）
		if err := assertChannelRole(c.Request.Context(), store, req.ForwardChannelBizID, "forward"); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := store.UpdateTarget(c.Request.Context(), id, &req); err != nil {
			handleStoreErr(c, err)
			return
		}
		out, _ := store.GetTarget(c.Request.Context(), id)
		c.JSON(http.StatusOK, out)
	}
}

func deleteTarget(db *sql.DB) gin.HandlerFunc {
	store := NewStore(db)
	return func(c *gin.Context) {
		id, ok := parseID(c)
		if !ok {
			return
		}
		if err := store.DeleteTarget(c.Request.Context(), id); err != nil {
			handleStoreErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

// setTargetPoints / Points 接收 BizID 列表（替换语义）。
func setTargetPoints(db *sql.DB) gin.HandlerFunc {
	store := NewStore(db)
	return func(c *gin.Context) {
		id, ok := parseID(c)
		if !ok {
			return
		}
		var req struct {
			PointBizIDs []string `json:"point_biz_ids"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		t, err := store.GetTarget(c.Request.Context(), id)
		if err != nil {
			handleStoreErr(c, err)
			return
		}
		t.PointBizIDs = req.PointBizIDs
		if err := store.UpdateTarget(c.Request.Context(), id, t); err != nil {
			handleStoreErr(c, err)
			return
		}
		out, _ := store.GetTarget(c.Request.Context(), id)
		c.JSON(http.StatusOK, out)
	}
}

// ============================================================
// Tokens
// ============================================================

func listTokens(db *sql.DB) gin.HandlerFunc {
	store := NewStore(db)
	return func(c *gin.Context) {
		items, err := store.ListTokens(c.Request.Context())
		if err != nil {
			handleStoreErr(c, err)
			return
		}
		// 仅返回元信息，绝不回显 token 明文
		views := make([]TokenView, 0, len(items))
		for _, t := range items {
			views = append(views, TokenView{
				ID:        t.ID,
				Name:      t.Name,
				CreatedAt: t.CreatedAt,
				RevokedAt: t.RevokedAt,
			})
		}
		c.JSON(http.StatusOK, gin.H{"items": views})
	}
}

func createToken(db *sql.DB) gin.HandlerFunc {
	store := NewStore(db)
	return func(c *gin.Context) {
		var req struct {
			Name string `json:"name" binding:"required"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		tok, err := randomToken(32)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		t := TokenDTO{Token: tok, Name: req.Name}
		id, err := store.CreateToken(c.Request.Context(), &t)
		if err != nil {
			handleStoreErr(c, err)
			return
		}
		// 一次性返回明文 token，后续 list 永远不返回
		c.JSON(http.StatusCreated, TokenCreated{ID: id, Token: tok, Name: req.Name})
	}
}

func revokeToken(db *sql.DB) gin.HandlerFunc {
	store := NewStore(db)
	return func(c *gin.Context) {
		id, ok := parseID(c)
		if !ok {
			return
		}
		if err := store.RevokeToken(c.Request.Context(), id); err != nil {
			handleStoreErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}
