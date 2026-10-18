// 仪表盘聚合：一次性返回所有统计，给 H5 Dashboard.vue 渲染卡片。
//
// 错误数取自 Prometheus 指标（如果 plcbridge 运行）；这里先返回 0 作为占位（细节由运维看 Metrics）。
// 存储字节数由 SQLite page_size * page_count 算出。
package web

import (
	"database/sql"
	"net/http"

	"github.com/gin-gonic/gin"
)

func dashboard(db *sql.DB) gin.HandlerFunc {
	store := NewStore(db)
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		d := DashboardDTO{}
		var err error
		if d.Channels, err = store.ChannelCounts(ctx); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if d.Devices, err = store.DeviceCounts(ctx); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if d.Sources, err = store.SourceCounts(ctx); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if d.Points, err = store.PointCounts(ctx); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		tcnt, err := store.TargetCount(ctx)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		d.Targets = TargetStats{Total: tcnt}
		d.TokensActive, _ = store.TokenActiveCount(ctx)
		// 存储字节数：page_size * page_count。
		// pragma 函数返回单列，需要交叉连接；失败时仍返回 0。
		var pageSize, pageCount int64
		if err := db.QueryRowContext(ctx,
			`SELECT (SELECT page_size FROM pragma_page_size()), (SELECT page_count FROM pragma_page_count())`).
			Scan(&pageSize, &pageCount); err == nil {
			d.StorageBytes = pageSize * pageCount
		}
		c.JSON(http.StatusOK, d)
	}
}
