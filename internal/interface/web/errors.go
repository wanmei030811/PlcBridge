package web

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

// APIError 是 plcbridge-web HTTP API 的统一错误响应体。
//
// 设计目标（Phase 1.2）：
//   - 错误响应格式统一：所有 handler 返回 {code, msg, ...optional}
//   - 业务错误码强枚举（不允出现未登记的 code）
//   - 为未来 i18n 预留：message 字段是"当前文案"，未来可替换为 i18n key
//   - 与 Store 层错误（ErrNotFound / ErrConflict）通过 errors.Is 解耦
//
// 响应格式：
//
//	{"code": "channel_not_found", "msg": "channel 不存在", "http_status": 404}
//
// 调用方：
//  1. 业务错误：用预定义实例（ErrChannelNotFound 等）
//  2. 参数错误：直接用 ErrInvalidParam.With(msg) 自定义消息
//  3. Store 错误：handleStoreErr 翻译成对应 APIError
//  4. 兜底：handleUnknownErr 返回 500
type APIError struct {
	Code       string // 业务错误码（snake_case，强枚举）
	Message    string // 人类可读的默认文案（当前中文，未来可 i18n）
	HTTPStatus int    // HTTP 状态码

	// I18nKey i18n 资源 key（Plan2 主题 B 4.2 预留）。
	//
	// 用途：未来切换 vue-i18n / go-i18n 时，handler / 前端按 I18nKey 查资源，
	//       找不到时 fallback 到 Message（向后兼容）。
	//
	// 当前值：空字符串（未启用 i18n，前端按 msg 字段直接展示）。
	// 启用方法：批量给所有 var (Err*) 赋 I18nKey = "errors." + Code（与前端 i18n key 对齐）。
	I18nKey string
}

// Error 实现 error 接口，方便作为函数返回值。
func (e *APIError) Error() string {
	return e.Code + ": " + e.Message
}

// Is 实现 errors.Is 支持（按 Code 比较）。
func (e *APIError) Is(target error) bool {
	var t *APIError
	if errors.As(target, &t) {
		return t.Code == e.Code
	}
	return false
}

// With 返回带自定义消息的新实例（Code 和 HTTPStatus 不变）。
//
// 用法：ErrInvalidParam.With("device_biz_id 不能为空")
func (e *APIError) With(msg string) *APIError {
	return &APIError{Code: e.Code, Message: msg, HTTPStatus: e.HTTPStatus, I18nKey: e.I18nKey}
}

// WithI18nKey 返回带 i18n key 的新实例（Plan2 主题 B 4.2 预留）。
//
// 用法：ErrChannelNotFound.WithI18nKey("errors.channel_not_found")
//
// 当前未启用 i18n：仅在 errors 资源里登记 key 即可，无需调用方即时改。
// 启用 i18n 时：批量替换 Message 字段为 i18n key，由前端按 key 翻译。
func (e *APIError) WithI18nKey(key string) *APIError {
	return &APIError{Code: e.Code, Message: e.Message, HTTPStatus: e.HTTPStatus, I18nKey: key}
}

// === 业务错误码（与 Plan.md §8 / §9 业务能力对应）===

// 通用错误。
//
// 注意：与 sqlstore.go 的 ErrNotFound / ErrConflict（store 内部 error）共存：
//   - 这里的 *APIError 用于 HTTP 响应（带 HTTPStatus + Code + Message）
//   - sqlstore.go 的 error 用于 store 层 errors.Is 判断
//   - WriteError 负责把后者翻译成前者
var (
	// ErrInvalidParam 参数无效。
	ErrInvalidParam = &APIError{Code: "invalid_param", Message: "参数无效", HTTPStatus: http.StatusBadRequest}

	// ErrAPIAuthFailed 未登录或登录已过期（HTTP 层使用）。
	// 与 auth.go 的 ErrUnauthorized（store 内部 error）区分。
	ErrAPIAuthFailed = &APIError{Code: "unauthorized", Message: "未登录", HTTPStatus: http.StatusUnauthorized}

	// ErrForbidden 权限不足。
	ErrForbidden = &APIError{Code: "forbidden", Message: "权限不足", HTTPStatus: http.StatusForbidden}

	// ErrAPINotFound 资源不存在（HTTP 层使用，与 sqlstore.ErrNotFound 区分）。
	ErrAPINotFound = &APIError{Code: "not_found", Message: "资源不存在", HTTPStatus: http.StatusNotFound}

	// ErrAPIConflict 资源冲突（HTTP 层使用，与 sqlstore.ErrConflict 区分）。
	ErrAPIConflict = &APIError{Code: "conflict", Message: "资源冲突", HTTPStatus: http.StatusConflict}

	// ErrFKConstraint 外键约束失败（删除有子资源的实体）。
	ErrFKConstraint = &APIError{Code: "fk_constraint", Message: "存在依赖资源，无法删除", HTTPStatus: http.StatusConflict}

	// ErrInternal 内部错误（兜底 500）。
	ErrInternal = &APIError{Code: "internal_error", Message: "服务器内部错误", HTTPStatus: http.StatusInternalServerError}

	// ErrPlcbridgeUnavailable plcbridge 主进程暂不可达（web 端代理 :8082 失败）。
	// 用于 protocolsProxyHandler / metrics_proxy 等跨进程调用场景。
	ErrPlcbridgeUnavailable = &APIError{Code: "plcbridge_unavailable", Message: "plcbridge 主进程暂不可达", HTTPStatus: http.StatusServiceUnavailable}
)

// Channel 错误。
var (
	// ErrChannelNotFound channel 不存在。
	ErrChannelNotFound = &APIError{Code: "channel_not_found", Message: "channel 不存在", HTTPStatus: http.StatusNotFound}

	// ErrChannelRoleMismatch channel role 与引用方向不匹配（source 引 collect, target 引 forward）。
	ErrChannelRoleMismatch = &APIError{Code: "channel_role_mismatch", Message: "channel role 不匹配", HTTPStatus: http.StatusBadRequest}

	// ErrChannelConflict channel 业务 ID 重复。
	ErrChannelConflict = &APIError{Code: "channel_conflict", Message: "channel_biz_id 已存在", HTTPStatus: http.StatusConflict}

	// ErrChannelConnectFail channel 连接失败（plcbridge 端）。
	ErrChannelConnectFail = &APIError{Code: "channel_connect_fail", Message: "channel 连接失败", HTTPStatus: http.StatusBadGateway}
)

// Device 错误。
var (
	// ErrDeviceNotFound device 不存在。
	ErrDeviceNotFound = &APIError{Code: "device_not_found", Message: "device 不存在", HTTPStatus: http.StatusNotFound}

	// ErrDeviceConflict device 业务 ID 重复。
	ErrDeviceConflict = &APIError{Code: "device_conflict", Message: "device_biz_id 已存在", HTTPStatus: http.StatusConflict}

	// ErrDeviceDisabled device 已被禁用。
	ErrDeviceDisabled = &APIError{Code: "device_disabled", Message: "device 已禁用", HTTPStatus: http.StatusBadRequest}
)

// Source 错误。
var (
	// ErrSourceNotFound source 不存在。
	ErrSourceNotFound = &APIError{Code: "source_not_found", Message: "source 不存在", HTTPStatus: http.StatusNotFound}

	// ErrSourceConflict source 业务 ID 重复。
	ErrSourceConflict = &APIError{Code: "source_conflict", Message: "source_biz_id 已存在", HTTPStatus: http.StatusConflict}
)

// Point 错误。
var (
	// ErrPointNotFound point 不存在。
	ErrPointNotFound = &APIError{Code: "point_not_found", Message: "point 不存在", HTTPStatus: http.StatusNotFound}

	// ErrPointConflict point 业务 ID 重复。
	ErrPointConflict = &APIError{Code: "point_conflict", Message: "point_biz_id 已存在", HTTPStatus: http.StatusConflict}
)

// Target 错误。
var (
	// ErrTargetNotFound target 不存在。
	ErrTargetNotFound = &APIError{Code: "target_not_found", Message: "target 不存在", HTTPStatus: http.StatusNotFound}

	// ErrTargetConflict target 业务 ID 重复。
	ErrTargetConflict = &APIError{Code: "target_conflict", Message: "target_biz_id 已存在", HTTPStatus: http.StatusConflict}
)

// Auth 错误。
var (
	// ErrInvalidCredentials 用户名或密码错误。
	ErrInvalidCredentials = &APIError{Code: "invalid_credentials", Message: "用户名或密码错误", HTTPStatus: http.StatusUnauthorized}

	// ErrSessionExpired 会话已过期。
	ErrSessionExpired = &APIError{Code: "session_expired", Message: "会话已过期", HTTPStatus: http.StatusUnauthorized}
)

// WriteError 把 error 写入 HTTP 响应。
//
// 翻译规则：
//   - *APIError：按预定义 code + status 返回
//   - errors.Is(err, ErrNotFound) / ErrConflict：翻译为对应 APIError
//   - 兜底：500 internal_error
//
// 用法：
//
//	if err != nil {
//	    web.WriteError(c, err)
//	    return
//	}
func WriteError(c *gin.Context, err error) {
	if err == nil {
		return
	}

	// 业务 APIError 直接用
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		body := gin.H{
			"code": apiErr.Code,
			"msg":  apiErr.Message,
		}
		// Plan2 主题 B 4.2 预留：I18nKey 非空时附带，前端按需翻译。
		if apiErr.I18nKey != "" {
			body["i18n_key"] = apiErr.I18nKey
		}
		c.JSON(apiErr.HTTPStatus, body)
		return
	}

	// Store 层错误翻译
	if errors.Is(err, ErrNotFound) {
		c.JSON(ErrAPINotFound.HTTPStatus, gin.H{
			"code": ErrAPINotFound.Code,
			"msg":  ErrAPINotFound.Message,
		})
		return
	}
	if errors.Is(err, ErrConflict) {
		c.JSON(ErrAPIConflict.HTTPStatus, gin.H{
			"code": ErrAPIConflict.Code,
			"msg":  err.Error(), // 保留原始冲突信息
		})
		return
	}
	if isFKConstraintErr(err) {
		c.JSON(ErrFKConstraint.HTTPStatus, gin.H{
			"code": ErrFKConstraint.Code,
			"msg":  ErrFKConstraint.Message,
		})
		return
	}

	// 兜底
	c.JSON(ErrInternal.HTTPStatus, gin.H{
		"code": ErrInternal.Code,
		"msg":  ErrInternal.Message,
	})
}
