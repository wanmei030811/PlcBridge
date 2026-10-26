// CRUD handlers（Channels / Devices / Sources / Points / Targets）。
//
// 设计原则：
//   - JSON 入参/出参；错误统一 WriteError（4.1 Plan2） → {code, msg}
//   - 400 参数错误 / 401 未登录（中间件拦）/ 404 不存在 / 409 唯一约束冲突
//   - 列表接口返回 []（绝不返回 null；空时给 []）
//   - 创建返回 id + 完整 DTO；更新返回更新后的 DTO；删除返回 {"ok": true}
package web

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/plcbridge/plcbridge/internal/platform"
)

// helper：parseID
func parseID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		WriteError(c, ErrInvalidParam.With("invalid id"))
		return 0, false
	}
	return id, true
}

// handleStoreErr 把 Store 错误翻译成 HTTP（Plan2 4.3 统一走 WriteError）。
//
// 历史：
//   - Phase 1.2 (4.1) 过渡期：仍走 {error: msg} 旧格式（前端 client.ts 兼容）
//   - 4.3：与业务校验统一为 {code, msg}；前端 client.ts 的 extractErrorMessage
//     已兼容双格式，无破坏性。
//
// 翻译规则（用 WriteError 内置分支，无需 switch）：
//   - sqlstore.ErrNotFound → ErrAPINotFound (404, code="not_found")
//   - sqlstore.ErrConflict → ErrAPIConflict (409, code="conflict", msg=err.Error())
//   - FOREIGN KEY constraint failed → ErrFKConstraint (409, code="fk_constraint")
//   - 其它 → ErrInternal (500, code="internal_error")
func handleStoreErr(c *gin.Context, err error) {
	WriteError(c, err)
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

// === 引用完整性预校验已下沉到 service.go（Plan2 5.3） ===
//
// 旧 assertChannelExists / assertChannelRole / assertDeviceExists / assertSourceExists
// 已删除；现在统一通过 Store 上的 Ensure* / Validate* 方法调用（service.go）。

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
			WriteError(c, ErrInvalidParam.With(err.Error()))
			return
		}
		if req.ChannelBizID == "" || req.Role == "" || req.Type == "" || req.Address == "" {
			WriteError(c, ErrInvalidParam.With("channel_biz_id/role/type/address 必填"))
			return
		}
		if req.Role != "collect" && req.Role != "forward" {
			WriteError(c, ErrInvalidParam.With("role 必须是 collect 或 forward"))
			return
		}
		if err := ValidateChannelType(req.Role, req.Type); err != nil {
			WriteError(c, err)
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

// isKnownChannelType / isKnownDataType 已下沉到 service.go（Plan2 5.3）。
//
// 旧版本：isKnownChannelType 用 switch+查 protocol.Get/target.Get（仅查表，无业务错误码）
// 旧版本：isKnownDataType 用 platform.IsValidDataType（仅查表）
// 现在：handler 直接调 service.ValidateChannelType / platform.IsValidDataType
//
// 别名处理：modbus → modbus_tcp 通过 protocol.CanonicalizeChannelType 自动归一化。

func updateChannel(db *sql.DB) gin.HandlerFunc {
	store := NewStore(db)
	return func(c *gin.Context) {
		id, ok := parseID(c)
		if !ok {
			return
		}
		var req ChannelDTO
		if err := c.ShouldBindJSON(&req); err != nil {
			WriteError(c, ErrInvalidParam.With(err.Error()))
			return
		}
		// Plan2 5.3 bugfix:updateChannel 也校验 type（之前漏了，可被改成 made-up 绕过）
		// 注意：role 字段对 update 不允许改（DB 不更新 role 列），所以只需校验 type
		if req.Type != "" {
			if err := ValidateChannelType(req.Role, req.Type); err != nil {
				WriteError(c, err)
				return
			}
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
			WriteError(c, ErrInvalidParam.With(err.Error()))
			return
		}
		if req.DeviceBizID == "" || req.Name == "" {
			WriteError(c, ErrInvalidParam.With("device_biz_id/name 必填"))
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
			WriteError(c, ErrInvalidParam.With(err.Error()))
			return
		}
		if req.DeviceBizID == "" {
			WriteError(c, ErrInvalidParam.With("device_biz_id 必填"))
			return
		}
		if req.Name == "" {
			WriteError(c, ErrInvalidParam.With("name 必填"))
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
			WriteError(c, ErrInvalidParam.With(err.Error()))
			return
		}
		if req.SourceBizID == "" || req.DeviceID == 0 || req.ChannelBizID == "" {
			WriteError(c, ErrInvalidParam.With("source_biz_id/device_id/channel_biz_id 必填"))
			return
		}
		// 引用完整性 + 业务规则复合校验（Plan2 5.3：下沉到 service 层）
		if err := store.ValidateSourceForCreate(c.Request.Context(), &req); err != nil {
			WriteError(c, err)
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
			WriteError(c, ErrInvalidParam.With(err.Error()))
			return
		}
		// 部分更新：仅当请求体含 channel_biz_id 时校验角色（Plan2 5.3 service 复合校验）
		if err := store.ValidateSourceForUpdate(c.Request.Context(), &req); err != nil {
			WriteError(c, err)
			return
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
			WriteError(c, ErrInvalidParam.With(err.Error()))
			return
		}
		if req.PointBizID == "" || req.SourceID == 0 || req.Address == "" || req.DataType == "" {
			WriteError(c, ErrInvalidParam.With("point_biz_id/source_id/address/data_type 必填"))
			return
		}
		if !isKnownDataType(req.DataType) {
			WriteError(c, ErrInvalidParam.With("未知 data_type="+req.DataType))
			return
		}
		if err := validateStringLength(&req); err != nil {
			WriteError(c, ErrInvalidParam.With(err.Error()))
			return
		}
		if err := store.EnsureSourceExists(c.Request.Context(), req.SourceID); err != nil {
			WriteError(c, err)
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

// isKnownDataType 校验 point.data_type 是受支持的类型（Plan2 主题 A 3.1 查表化）。
//
// 词汇表位置：internal/platform/datatypes.go（Plan §8.5，13 个固定值）。
// 历史：曾在此处硬编码 13 个 case，改 platform 一处即可全部生效——避免词汇表漂移。
// 5.3 后：保留为内部薄包装（其它 handler 可能仍调）；service 层也有 platform.IsValidDataType
// 直接调用路径（validateStringLength 等场景用 service 复合校验）。
func isKnownDataType(dt string) bool {
	return platform.IsValidDataType(dt)
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
			WriteError(c, ErrInvalidParam.With(err.Error()))
			return
		}
		if req.Address == "" || req.DataType == "" {
			WriteError(c, ErrInvalidParam.With("address/data_type 必填"))
			return
		}
		if !isKnownDataType(req.DataType) {
			WriteError(c, ErrInvalidParam.With("未知 data_type="+req.DataType))
			return
		}
		if err := validateStringLength(&req); err != nil {
			WriteError(c, ErrInvalidParam.With(err.Error()))
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
			WriteError(c, ErrInvalidParam.With(err.Error()))
			return
		}
		if req.TargetBizID == "" || req.DeviceID == 0 || req.ForwardChannelBizID == "" {
			WriteError(c, ErrInvalidParam.With("target_biz_id/device_id/forward_channel_biz_id 必填"))
			return
		}
		// 引用完整性 + 业务规则复合校验（Plan2 5.3：下沉到 service 层）
		if err := store.ValidateTargetForCreate(c.Request.Context(), &req); err != nil {
			WriteError(c, err)
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
			WriteError(c, ErrInvalidParam.With(err.Error()))
			return
		}
		if req.TargetBizID == "" {
			WriteError(c, ErrInvalidParam.With("target_biz_id 必填"))
			return
		}
		if req.ForwardChannelBizID == "" {
			WriteError(c, ErrInvalidParam.With("forward_channel_biz_id 必填"))
			return
		}
		// 校验绑定的 channel 角色必须是 forward（Plan2 5.3 service 复合校验）
		if err := store.ValidateTargetForUpdate(c.Request.Context(), &req); err != nil {
			WriteError(c, err)
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
			WriteError(c, ErrInvalidParam.With(err.Error()))
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
			WriteError(c, ErrInvalidParam.With(err.Error()))
			return
		}
		tok, err := randomToken(32)
		if err != nil {
			WriteError(c, ErrInternal.With(err.Error()))
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
