package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// newTestCtx 构造一个带 ResponseRecorder 的 gin.Context。
func newTestCtx() (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	return c, w
}

// TestAPIError_ImplementsError APIError 必须实现 error 接口（业务代码常当 error 返回）。
func TestAPIError_ImplementsError(t *testing.T) {
	var _ error = ErrChannelNotFound
	var _ error = ErrInvalidParam.With("custom msg")
}

// TestAPIError_IsByCode 验证 errors.Is 按 Code 匹配。
func TestAPIError_IsByCode(t *testing.T) {
	a := &APIError{Code: "x", Message: "a", HTTPStatus: 400}
	b := &APIError{Code: "x", Message: "b", HTTPStatus: 500}
	if !errors.Is(a, b) {
		t.Errorf("errors.Is should match by Code (a.Code==b.Code)")
	}
	c := &APIError{Code: "y", Message: "c", HTTPStatus: 400}
	if errors.Is(a, c) {
		t.Errorf("errors.Is should NOT match different Code")
	}
}

// TestAPIError_With APIError.With 返回新实例，不修改原对象。
func TestAPIError_With(t *testing.T) {
	orig := ErrInvalidParam
	custom := ErrInvalidParam.With("field x 不能为空")
	if orig.Message == custom.Message {
		t.Errorf("With() should not modify original")
	}
	if custom.Code != orig.Code {
		t.Errorf("With() should preserve Code")
	}
	if custom.HTTPStatus != orig.HTTPStatus {
		t.Errorf("With() should preserve HTTPStatus")
	}
	if custom.Message != "field x 不能为空" {
		t.Errorf("With() message: got %q, want custom", custom.Message)
	}
}

// TestWriteError_BusinessError 验证 *APIError 直接翻译为 HTTP 响应。
func TestWriteError_BusinessError(t *testing.T) {
	c, w := newTestCtx()
	WriteError(c, ErrChannelNotFound)

	if w.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if body["code"] != "channel_not_found" {
		t.Errorf("code: got %v, want channel_not_found", body["code"])
	}
	if body["msg"] != "channel 不存在" {
		t.Errorf("msg: got %v", body["msg"])
	}
}

// TestWriteError_CustomMessage 验证 With 自定义消息生效。
func TestWriteError_CustomMessage(t *testing.T) {
	c, w := newTestCtx()
	WriteError(c, ErrInvalidParam.With("device_biz_id 不能为空"))

	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", w.Code)
	}
	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["msg"] != "device_biz_id 不能为空" {
		t.Errorf("custom msg not applied: %v", body["msg"])
	}
	if body["code"] != "invalid_param" {
		t.Errorf("code: got %v", body["code"])
	}
}

// TestWriteError_StoreNotFound 验证 sqlstore.ErrNotFound 翻译。
func TestWriteError_StoreNotFound(t *testing.T) {
	c, w := newTestCtx()
	WriteError(c, ErrNotFound) // sqlstore 的 error

	if w.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", w.Code)
	}
	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["code"] != "not_found" {
		t.Errorf("code: got %v, want not_found", body["code"])
	}
}

// TestWriteError_StoreConflict 验证 sqlstore.ErrConflict 翻译。
func TestWriteError_StoreConflict(t *testing.T) {
	c, w := newTestCtx()
	WriteError(c, ErrConflict)

	if w.Code != http.StatusConflict {
		t.Errorf("status: got %d, want 409", w.Code)
	}
	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["code"] != "conflict" {
		t.Errorf("code: got %v", body["code"])
	}
}

// TestWriteError_FKConstraint 验证外键约束翻译。
func TestWriteError_FKConstraint(t *testing.T) {
	c, w := newTestCtx()
	WriteError(c, errors.New("FOREIGN KEY constraint failed"))

	if w.Code != http.StatusConflict {
		t.Errorf("status: got %d, want 409", w.Code)
	}
	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["code"] != "fk_constraint" {
		t.Errorf("code: got %v, want fk_constraint", body["code"])
	}
}

// TestWriteError_Internal 验证未知错误兜底为 500。
func TestWriteError_Internal(t *testing.T) {
	c, w := newTestCtx()
	WriteError(c, errors.New("some unexpected error"))

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500", w.Code)
	}
	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["code"] != "internal_error" {
		t.Errorf("code: got %v, want internal_error", body["code"])
	}
}

// TestWriteError_NilError nil 不写响应。
func TestWriteError_NilError(t *testing.T) {
	c, w := newTestCtx()
	WriteError(c, nil)
	if w.Code != http.StatusOK {
		t.Errorf("nil error should not write response, got status %d", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("nil error should not write body, got %q", w.Body.String())
	}
}

// TestAllAPIErrors_HaveUniqueCode 防止开发误复制粘贴。
func TestAllAPIErrors_HaveUniqueCode(t *testing.T) {
	all := []*APIError{
		ErrInvalidParam, ErrAPIAuthFailed, ErrForbidden, ErrAPINotFound, ErrAPIConflict,
		ErrFKConstraint, ErrInternal,
		ErrChannelNotFound, ErrChannelRoleMismatch, ErrChannelConflict, ErrChannelConnectFail,
		ErrDeviceNotFound, ErrDeviceConflict, ErrDeviceDisabled,
		ErrSourceNotFound, ErrSourceConflict,
		ErrPointNotFound, ErrPointConflict,
		ErrTargetNotFound, ErrTargetConflict,
		ErrInvalidCredentials, ErrSessionExpired,
	}
	seen := map[string]string{}
	for _, e := range all {
		if other, dup := seen[e.Code]; dup {
			t.Errorf("duplicate code %q (prev msg=%q, this msg=%q)", e.Code, other, e.Message)
		}
		seen[e.Code] = e.Message
	}
}

// TestAllAPIErrors_Format 验证 Code 风格（snake_case）。
func TestAllAPIErrors_Format(t *testing.T) {
	all := []*APIError{
		ErrInvalidParam, ErrAPIAuthFailed, ErrForbidden, ErrAPINotFound, ErrAPIConflict,
		ErrFKConstraint, ErrInternal,
		ErrChannelNotFound, ErrChannelRoleMismatch, ErrChannelConflict, ErrChannelConnectFail,
		ErrDeviceNotFound, ErrDeviceConflict, ErrDeviceDisabled,
		ErrSourceNotFound, ErrSourceConflict,
		ErrPointNotFound, ErrPointConflict,
		ErrTargetNotFound, ErrTargetConflict,
		ErrInvalidCredentials, ErrSessionExpired,
	}
	for _, e := range all {
		if e.Code == "" {
			t.Errorf("code is empty for msg=%q", e.Message)
		}
		if e.HTTPStatus < 400 || e.HTTPStatus >= 600 {
			t.Errorf("HTTPStatus %d out of 4xx/5xx range for code=%s", e.HTTPStatus, e.Code)
		}
		if e.Message == "" {
			t.Errorf("Message is empty for code=%s", e.Code)
		}
	}
}
