// settings_api.go：Web 控制台设置的 HTTP 接口。
//
//	GET  /api/v1/settings          → 读取全部设置
//	PUT  /api/v1/settings/:key     → 修改单个设置
//
// 设置项定义见 settings.go（knownSettings 是唯一的合法键白名单）。
package web

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// settingsResp 设置响应体。
//
// 用 map 而不是固定 struct：加新设置项时不用改后端 DTO，
// 前端 api/settings.ts 也只需要读自己关心的字段。
type settingsResp map[string]any

// listSettingsHandler GET /api/v1/settings
//
// 返回所有设置（未设置过的用默认值填充），前端一次拿全。
func (s *Store) listSettingsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
		defer cancel()

		all, err := s.ListSettings(ctx)
		if err != nil {
			WriteError(c, ErrInternal.With(err.Error()))
			return
		}
		out := settingsResp{}
		for k, v := range all {
			out[string(k)] = v
		}
		c.JSON(http.StatusOK, out)
	}
}

// updateSettingHandler PUT /api/v1/settings/:key
//
// Body: {"value": true}
//
// 兼容两种 body 形态（都接受）：
//   - {"value": true}  —— 推荐，与其他写接口一致
//   - true             —— 裸布尔值，便于 curl / 脚本调用
func (s *Store) updateSettingHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		key := SettingKey(c.Param("key"))
		if _, ok := knownSettings[key]; !ok {
			WriteError(c, ErrInvalidParam.With("未知的设置项: "+string(key)))
			return
		}

		// 先按裸布尔解析，失败再按 {"value": ...} 解析。
		var raw json.RawMessage
		if err := c.ShouldBindJSON(&raw); err != nil {
			WriteError(c, ErrInvalidParam.With("invalid json body: "+err.Error()))
			return
		}

		var val bool
		if err := json.Unmarshal(raw, &val); err != nil {
			var wrapped struct {
				Value *bool `json:"value"`
			}
			if err2 := json.Unmarshal(raw, &wrapped); err2 != nil || wrapped.Value == nil {
				WriteError(c, ErrInvalidParam.With("value 必填（布尔值）"))
				return
			}
			val = *wrapped.Value
		}

		ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
		defer cancel()

		if err := s.SetSettingBool(ctx, key, val); err != nil {
			WriteError(c, ErrInternal.With(err.Error()))
			return
		}

		// 返回最新全量，前端直接替换本地状态（省一次 GET）。
		all, err := s.ListSettings(ctx)
		if err != nil {
			WriteError(c, ErrInternal.With(err.Error()))
			return
		}
		out := settingsResp{}
		for k, v := range all {
			out[string(k)] = v
		}
		c.JSON(http.StatusOK, out)
	}
}
