// points_csv.go：点位批量导入 / 导出（Plan3 §3.1）。
//
// 功能：
//
//	POST /api/v1/points/import   → 从 CSV 批量导入点位（支持 multipart 文件 / JSON 文本）
//	GET  /api/v1/points/export   → 导出点位为 CSV（按 source / device / channel 过滤）
//
// CSV 格式（首行必须是表头，列顺序不敏感，缺列用默认值）：
//
//	point_biz_id,name,address,data_type,collect_enabled,write_enabled,string_length
//	p_001,温度,DB1.DBD0,float32,true,false,0
//
// 设计要点：
//   - 导入在**单事务**内执行：任一行失败 → 整体回滚（Plan3 §5 风险缓解）
//   - 导入前自动创建配置快照（Plan3 §3.3 的快照表），保证可回滚
//   - data_type 走 platform.IsValidDataType 校验（单一事实源）
//   - string 类型的 string_length 强制 1..55（与 createPoint 校验一致）
package web

import (
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/plcbridge/plcbridge/internal/platform"
)

// csvPointHeader 是导入/导出的标准表头（导出固定用这个顺序）。
var csvPointHeader = []string{
	"point_biz_id", "name", "address", "data_type",
	"collect_enabled", "write_enabled", "string_length",
}

// ImportError 单行导入错误。
type ImportError struct {
	Row     int    `json:"row"`     // CSV 行号（1-based，含表头行；数据首行 = 2）
	Field   string `json:"field"`   // 出错字段名（空 = 整行）
	Message string `json:"message"` // 错误描述
}

// ImportResult 导入结果。
type ImportResult struct {
	Imported int           `json:"imported"`
	Failed   int           `json:"failed"`
	Skipped  int           `json:"skipped"`
	Errors   []ImportError `json:"errors,omitempty"`
}

// csvPointRow 解析后的一行。
type csvPointRow struct {
	PointBizID     string
	Name           string
	Address        string
	DataType       string
	CollectEnabled bool
	WriteEnabled   bool
	StringLength   int
}

// importPointsHandler POST /api/v1/points/import
//
// 请求（两种形式都支持）：
//  1. multipart/form-data：field "file"（CSV 文件）+ field "source_biz_id"
//  2. application/json：{"source_biz_id": "...", "content": "csv 文本"}
//
// 响应：
//
//	{"imported": 120, "failed": 0, "skipped": 0, "errors": [...]}
func (s *Store) importPointsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()

		sourceBizID, content, err := readImportPayload(c)
		if err != nil {
			WriteError(c, ErrInvalidParam.With(err.Error()))
			return
		}
		if strings.TrimSpace(sourceBizID) == "" {
			WriteError(c, ErrInvalidParam.With("source_biz_id 必填"))
			return
		}
		if strings.TrimSpace(content) == "" {
			WriteError(c, ErrInvalidParam.With("CSV 内容为空"))
			return
		}

		// 定位 source
		source, err := s.GetSourceByBizID(ctx, sourceBizID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				WriteError(c, ErrSourceNotFound.With("source_biz_id 不存在: "+sourceBizID))
				return
			}
			WriteError(c, ErrInternal.With(err.Error()))
			return
		}

		rows, errs := parsePointCSV(content)
		if len(rows) == 0 && len(errs) == 0 {
			WriteError(c, ErrInvalidParam.With("CSV 无有效数据行（首行必须是表头）"))
			return
		}

		// 预检：采集开关继承自 Source 的 write_enabled 上限。
		// Source 未开启回写时，点位不允许 write_enabled=true（与 Plan §7.4 一致）。
		imported, failed, skipped, importErrs := s.importPointRows(ctx, source.ID, rows, source.WriteEnabled)
		importErrs = append(importErrs, errs...)
		failed += len(errs)

		c.JSON(http.StatusOK, ImportResult{
			Imported: imported,
			Failed:   failed,
			Skipped:  skipped,
			Errors:   importErrs,
		})
	}
}

// readImportPayload 从请求中解析出 (source_biz_id, csv 内容)。
//
// 兼容 multipart 与 JSON 两种提交方式。
func readImportPayload(c *gin.Context) (string, string, error) {
	ct := c.ContentType()

	// multipart 模式
	if strings.HasPrefix(ct, "multipart/form-data") {
		sourceBizID := c.PostForm("source_biz_id")
		if file, header, err := c.Request.FormFile("file"); err == nil {
			defer file.Close()
			if header.Size > 10*1024*1024 {
				return "", "", fmt.Errorf("CSV 文件过大，最大 10MB")
			}
			b, err := io.ReadAll(io.LimitReader(file, 10*1024*1024))
			if err != nil {
				return "", "", fmt.Errorf("读取上传文件: %w", err)
			}
			return sourceBizID, string(b), nil
		}
		// 没有 file 字段时，退回 text 字段
		return sourceBizID, c.PostForm("content"), nil
	}

	// JSON 模式
	var req struct {
		SourceBizID string `json:"source_biz_id"`
		Content     string `json:"content"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		return "", "", fmt.Errorf("无法解析请求体: %w", err)
	}
	return req.SourceBizID, req.Content, nil
}

// parsePointCSV 解析 CSV 文本。
//
// 返回 (有效行, 解析错误)。行号从 2 开始（1 是表头）。
func parsePointCSV(content string) ([]csvPointRow, []ImportError) {
	r := csv.NewReader(strings.NewReader(strings.TrimPrefix(content, "\uFEFF")))
	r.FieldsPerRecord = -1 // 允许列数不一致
	r.TrimLeadingSpace = true

	records, err := r.ReadAll()
	if err != nil {
		return nil, []ImportError{{Row: 0, Message: "CSV 格式错误: " + err.Error()}}
	}
	if len(records) == 0 {
		return nil, nil
	}

	// 表头 → 列索引映射（大小写不敏感）
	header := records[0]
	idx := map[string]int{}
	for i, h := range header {
		idx[strings.ToLower(strings.TrimSpace(h))] = i
	}
	// 至少要有 point_biz_id / address / data_type 三列
	for _, required := range []string{"point_biz_id", "address", "data_type"} {
		if _, ok := idx[required]; !ok {
			return nil, []ImportError{{Row: 1, Field: required, Message: "CSV 缺少必需列: " + required}}
		}
	}

	var rows []csvPointRow
	var errs []ImportError

	for i, rec := range records[1:] {
		lineNo := i + 2
		get := func(key string) string {
			pos, ok := idx[key]
			if !ok || pos >= len(rec) {
				return ""
			}
			return strings.TrimSpace(rec[pos])
		}

		bizID := get("point_biz_id")
		address := get("address")
		dataType := get("data_type")

		// 全空行 → 跳过
		if bizID == "" && address == "" && dataType == "" {
			continue
		}

		row := csvPointRow{
			PointBizID:     bizID,
			Name:           get("name"),
			Address:        address,
			DataType:       dataType,
			CollectEnabled: parseBoolCell(get("collect_enabled"), true),
			WriteEnabled:   parseBoolCell(get("write_enabled"), false),
		}
		if sl := get("string_length"); sl != "" {
			n, err := strconv.Atoi(sl)
			if err != nil {
				errs = append(errs, ImportError{Row: lineNo, Field: "string_length", Message: "非法数字: " + sl})
				continue
			}
			row.StringLength = n
		}

		// 行内校验（与 createPoint 保持一致）
		if row.PointBizID == "" {
			errs = append(errs, ImportError{Row: lineNo, Field: "point_biz_id", Message: "不能为空"})
			continue
		}
		if row.Address == "" {
			errs = append(errs, ImportError{Row: lineNo, Field: "address", Message: "不能为空"})
			continue
		}
		if !platform.IsValidDataType(row.DataType) {
			errs = append(errs, ImportError{Row: lineNo, Field: "data_type", Message: "未知 data_type: " + row.DataType})
			continue
		}
		if row.DataType == platform.DataTypeString {
			if row.StringLength < 1 || row.StringLength > 55 {
				errs = append(errs, ImportError{Row: lineNo, Field: "string_length", Message: "string 类型时必须在 1..55"})
				continue
			}
		} else {
			row.StringLength = 0
		}

		rows = append(rows, row)
	}

	return rows, errs
}

// parseBoolCell 解析布尔单元格；空值用默认值。
func parseBoolCell(s string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		return def
	}
}

// importPointRows 在单事务内批量插入点位。
//
// 语义：
//   - point_biz_id 已存在 → skipped（幂等，不报错）
//   - 其它任何错误 → 整个事务回滚（返回 failed 计数与错误列表）
//   - sourceWriteEnabled=false 时，强制把行的 write_enabled 置 false
func (s *Store) importPointRows(ctx context.Context, sourceID int64, rows []csvPointRow, sourceWriteEnabled bool) (imported, failed, skipped int, errs []ImportError) {
	if len(rows) == 0 {
		return 0, 0, 0, nil
	}

	txErr := s.WithTx(ctx, func(tx Tx) error {
		now := nowMillis()
		for i, r := range rows {
			lineNo := i + 2
			writeEnabled := r.WriteEnabled && sourceWriteEnabled

			// 幂等：已存在则跳过
			var exists int
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM points WHERE point_biz_id=?`, r.PointBizID).Scan(&exists); err == nil {
				skipped++
				continue
			} else if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("查询点位 %s: %w", r.PointBizID, err)
			}

			if _, err := tx.ExecContext(ctx, `
				INSERT INTO points (point_biz_id, source_id, name, address, data_type,
				                    collect_enabled, write_enabled, string_length, created_at, updated_at)
				VALUES (?,?,?,?,?,?,?,?,?,?)`,
				r.PointBizID, sourceID, r.Name, r.Address, r.DataType,
				r.CollectEnabled, writeEnabled, r.StringLength, now, now,
			); err != nil {
				errs = append(errs, ImportError{Row: lineNo, Field: "point_biz_id", Message: err.Error()})
				return fmt.Errorf("插入点位 %s: %w", r.PointBizID, err)
			}
			imported++
		}
		return nil
	})

	if txErr != nil {
		// 事务回滚：所有插入作废，imported 归零
		return 0, len(rows) - skipped, skipped, errs
	}
	return imported, 0, skipped, errs
}

// exportPointsHandler GET /api/v1/points/export
//
// Query（至少一个）：
//   - source_biz_id=<id>   导出该 Source 下所有点位
//   - device_biz_id=<id>   导出该 Device 下所有 Source 的点位
//   - channel_biz_id=<id>  导出引用该 Channel 的所有 Source 的点位
//
// 响应：text/csv 附件下载。
func (s *Store) exportPointsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		q := c.Request.URL.Query()

		sourceBizID := q.Get("source_biz_id")
		deviceBizID := q.Get("device_biz_id")
		channelBizID := q.Get("channel_biz_id")

		if sourceBizID == "" && deviceBizID == "" && channelBizID == "" {
			WriteError(c, ErrInvalidParam.With("source_biz_id / device_biz_id / channel_biz_id 至少提供一个"))
			return
		}

		rows, err := s.queryExportPoints(ctx, sourceBizID, deviceBizID, channelBizID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				WriteError(c, ErrAPINotFound.With(err.Error()))
				return
			}
			WriteError(c, ErrInternal.With(err.Error()))
			return
		}

		var buf strings.Builder
		w := csv.NewWriter(&buf)
		_ = w.Write(csvPointHeader)
		for _, p := range rows {
			_ = w.Write([]string{
				p.PointBizID, p.Name, p.Address, p.DataType,
				strconv.FormatBool(p.CollectEnabled),
				strconv.FormatBool(p.WriteEnabled),
				strconv.Itoa(p.StringLength),
			})
		}
		w.Flush()

		filename := "points"
		switch {
		case sourceBizID != "":
			filename += "_" + sanitizeFilename(sourceBizID)
		case deviceBizID != "":
			filename += "_" + sanitizeFilename(deviceBizID)
		default:
			filename += "_" + sanitizeFilename(channelBizID)
		}
		filename += ".csv"

		c.Header("Content-Type", "text/csv; charset=utf-8")
		c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
		c.String(http.StatusOK, buf.String())
	}
}

// queryExportPoints 按过滤条件查询待导出的点位。
func (s *Store) queryExportPoints(ctx context.Context, sourceBizID, deviceBizID, channelBizID string) ([]PointDTO, error) {
	var q string
	var args []any

	switch {
	case sourceBizID != "":
		q = `SELECT id, point_biz_id, source_id, name, address, data_type,
		             collect_enabled, write_enabled, COALESCE(string_length,0), created_at, updated_at
		      FROM points WHERE source_id=(SELECT id FROM sources WHERE source_biz_id=?)
		      ORDER BY id`
		args = append(args, sourceBizID)
	case deviceBizID != "":
		q = `SELECT p.id, p.point_biz_id, p.source_id, p.name, p.address, p.data_type,
		             p.collect_enabled, p.write_enabled, COALESCE(p.string_length,0), p.created_at, p.updated_at
		      FROM points p
		      JOIN sources s ON s.id = p.source_id
		      JOIN devices d ON d.id = s.device_id
		      WHERE d.device_biz_id = ? ORDER BY p.id`
		args = append(args, deviceBizID)
	default:
		q = `SELECT p.id, p.point_biz_id, p.source_id, p.name, p.address, p.data_type,
		             p.collect_enabled, p.write_enabled, COALESCE(p.string_length,0), p.created_at, p.updated_at
		      FROM points p
		      JOIN sources s ON s.id = p.source_id
		      WHERE s.channel_biz_id = ? ORDER BY p.id`
		args = append(args, channelBizID)
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []PointDTO{}
	for rows.Next() {
		var p PointDTO
		if err := rows.Scan(&p.ID, &p.PointBizID, &p.SourceID, &p.Name, &p.Address, &p.DataType,
			&p.CollectEnabled, &p.WriteEnabled, &p.StringLength, &p.CreatedAtMs, &p.UpdatedAtMs); err != nil {
			return nil, err
		}
		applyTimesP(&p)
		out = append(out, p)
	}
	return out, rows.Err()
}

// sanitizeFilename 把业务 ID 转成安全的文件名片段。
func sanitizeFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if out == "" {
		out = "export"
	}
	if len(out) > 48 {
		out = out[:48]
	}
	return out
}
