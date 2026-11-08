package controller

// QYDION 统计增强（§8.8）：全部以 qydion_quota_transactions + logs 为源，仅 admin 可访问（路由层把守）。
// 看板卡片 + 趋势 + 四维明细（用户/模型/渠道/日）+ CSV 导出。

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
)

// qydionCsvSafe 全量安全审计 P2：CSV 公式注入防护——以 = + - @ 或 TAB/CR 开头的
// 字段加 ' 前缀，防止用户名等用户可控值在 Excel/WPS 打开导出文件时被当公式执行。
func qydionCsvSafe(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}

// qydionDayExpr 把「unix 秒列」转成分组用的本地日期表达式，按日志库方言分支。
// 本项目硬要求 SQLite / MySQL / PostgreSQL 同时兼容，日志库另可为 ClickHouse；
// 早期实现只写 MySQL 的 DATE(FROM_UNIXTIME(...))，分日报表在 SQLite/PG 上直接报错或恒空。
func qydionDayExpr(col string) string {
	switch common.LogDatabaseType() {
	case common.DatabaseTypeSQLite:
		return fmt.Sprintf("DATE(%s, 'unixepoch', 'localtime')", col)
	case common.DatabaseTypePostgreSQL:
		return fmt.Sprintf("TO_CHAR(TO_TIMESTAMP(%s), 'YYYY-MM-DD')", col)
	case common.DatabaseTypeClickHouse:
		return fmt.Sprintf("formatDateTime(toDateTime(%s), '%%Y-%%m-%%d')", col)
	default: // MySQL
		return fmt.Sprintf("DATE_FORMAT(FROM_UNIXTIME(%s), '%%Y-%%m-%%d')", col)
	}
}

// QydionDashboardCards 看板卡片：今日收入/今日消费tokens/今日新增用户/今日活跃用户/待审核对公订单数
func QydionDashboardCards(c *gin.Context) {
	now := time.Now()
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()

	var todayIncomeQuota, todayTokens, todayActiveUsers int64
	model.LOG_DB.Model(&model.Log{}).
		Where("type = ? AND created_at >= ?", model.LogTypeConsume, startOfDay).
		Select("COALESCE(SUM(quota),0)").Scan(&todayIncomeQuota)
	model.LOG_DB.Model(&model.Log{}).
		Where("type = ? AND created_at >= ?", model.LogTypeConsume, startOfDay).
		Select("COALESCE(SUM(prompt_tokens + completion_tokens),0)").Scan(&todayTokens)
	model.LOG_DB.Model(&model.Log{}).
		Where("type = ? AND created_at >= ?", model.LogTypeConsume, startOfDay).
		Distinct("user_id").Count(&todayActiveUsers)

	var todayNewUsers int64
	model.DB.Model(&model.User{}).Where("created_time >= ?", startOfDay).Count(&todayNewUsers)

	var pendingCorporate int64
	model.DB.Model(&model.TopUp{}).
		Where("status = ? AND payment_provider = ?", service.TopupStatusReviewing, "corporate").
		Count(&pendingCorporate)

	incomeCNY := operation_setting.Price * (float64(todayIncomeQuota) / common.QuotaPerUnit)
	common.ApiSuccess(c, gin.H{
		"today_income_cny":      incomeCNY,
		"today_tokens":          todayTokens,
		"today_new_users":       todayNewUsers,
		"today_active_users":    todayActiveUsers,
		"pending_corporate_cnt": pendingCorporate,
	})
}

// QydionTrend 趋势（7/30/90 天）：收入/消费tokens/请求数/成功率
func QydionTrend(c *gin.Context) {
	days, _ := strconv.Atoi(c.DefaultQuery("days", "7"))
	if days != 7 && days != 30 && days != 90 {
		days = 7
	}
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -(days - 1)).Unix()

	type trendRow struct {
		Day      string `json:"day"`
		Quota    int64  `json:"quota"`
		Tokens   int64  `json:"tokens"`
		Requests int64  `json:"requests"`
	}
	var rows []trendRow
	dayExpr := qydionDayExpr("created_at")
	err := model.LOG_DB.Model(&model.Log{}).
		Select(dayExpr+" as day, COALESCE(SUM(quota),0) as quota, "+
			"COALESCE(SUM(prompt_tokens + completion_tokens),0) as tokens, COUNT(*) as requests").
		Where("type = ? AND created_at >= ?", model.LogTypeConsume, start).
		Group("day").Order("day ASC").Scan(&rows).Error
	if err != nil {
		common.SysError(common.LogText("QYDION 趋势查询失败: %v", err)) // 细节只进日志（全量安全审计 P3）
		common.ApiErrorMsg(c, "趋势查询失败，请稍后重试")
		return
	}
	for i := range rows {
		rows[i].Quota = int64(operation_setting.Price * (float64(rows[i].Quota) / common.QuotaPerUnit))
	}
	common.ApiSuccess(c, rows)
}

// qydionBreakdown 通用四维聚合
func qydionBreakdown(c *gin.Context, dim string) {
	days, _ := strconv.Atoi(c.DefaultQuery("days", "7"))
	if days <= 0 || days > 90 {
		days = 7
	}
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -(days - 1)).Unix()

	groupCol := map[string]string{
		"user":    "username",
		"model":   "model_name",
		"channel": "channel_id",
		"day":     qydionDayExpr("created_at"),
	}[dim]
	if groupCol == "" {
		common.ApiErrorMsg(c, "维度仅支持 user/model/channel/day")
		return
	}

	type row struct {
		Dimension string  `json:"dimension"`
		Requests  int64   `json:"requests"`
		InTokens  int64   `json:"in_tokens"`
		OutTokens int64   `json:"out_tokens"`
		Quota     int64   `json:"quota"`
		CostCNY   float64 `json:"cost_cny"`
		IncomeCNY float64 `json:"income_cny"`
		Margin    float64 `json:"margin"`
	}
	// 成本（上游口径）= quota/QuotaPerUnit*Price 的占位折算；合同成本价导入后由对账修正（附录B-3）
	selectExpr := fmt.Sprintf(
		"%s as dimension, COUNT(*) as requests, "+
			"COALESCE(SUM(prompt_tokens),0) as in_tokens, COALESCE(SUM(completion_tokens),0) as out_tokens, "+
			"COALESCE(SUM(quota),0) as quota",
		groupCol)
	var rows []row
	err := model.LOG_DB.Model(&model.Log{}).
		Select(selectExpr).
		Where("type = ? AND created_at >= ?", model.LogTypeConsume, start).
		Group("dimension").Order("quota DESC").Limit(200).Scan(&rows).Error
	if err != nil {
		common.SysError(common.LogText("QYDION 聚合查询失败: %v", err))
		common.ApiErrorMsg(c, "聚合查询失败（若为 SQLite 日期函数问题请用模型/用户/渠道维度）")
		return
	}
	for i := range rows {
		rows[i].IncomeCNY = operation_setting.Price * (float64(rows[i].Quota) / common.QuotaPerUnit)
		rows[i].CostCNY = rows[i].IncomeCNY // PLACEHOLDER：合同成本价（附录B-3）到位后替换为真实上游口径
		rows[i].Margin = 0
	}
	common.ApiSuccess(c, rows)
}

func QydionBreakdownByUser(c *gin.Context)    { qydionBreakdown(c, "user") }
func QydionBreakdownByModel(c *gin.Context)   { qydionBreakdown(c, "model") }
func QydionBreakdownByChannel(c *gin.Context) { qydionBreakdown(c, "channel") }
func QydionBreakdownByDay(c *gin.Context)     { qydionBreakdown(c, "day") }

// QydionExportBreakdownCSV 四维明细 CSV 导出
func QydionExportBreakdownCSV(c *gin.Context) {
	dim := c.Query("dim")
	if dim == "" {
		dim = "model"
	}
	c.Request.URL.RawQuery = "days=" + c.DefaultQuery("days", "7")
	// 复用聚合：直接内联执行
	days, _ := strconv.Atoi(c.DefaultQuery("days", "7"))
	if days <= 0 || days > 90 {
		days = 7
	}
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -(days - 1)).Unix()
	groupCol := map[string]string{
		"user": "username", "model": "model_name", "channel": "channel_id",
		"day": qydionDayExpr("created_at"),
	}[dim]
	if groupCol == "" {
		common.ApiErrorMsg(c, "维度仅支持 user/model/channel/day")
		return
	}
	type row struct {
		Dimension string
		Requests  int64
		InTokens  int64
		OutTokens int64
		Quota     int64
	}
	var rows []row
	selectExpr := fmt.Sprintf("%s as dimension, COUNT(*) as requests, "+
		"COALESCE(SUM(prompt_tokens),0) as in_tokens, COALESCE(SUM(completion_tokens),0) as out_tokens, "+
		"COALESCE(SUM(quota),0) as quota", groupCol)
	if err := model.LOG_DB.Model(&model.Log{}).Select(selectExpr).
		Where("type = ? AND created_at >= ?", model.LogTypeConsume, start).
		Group("dimension").Order("quota DESC").Limit(5000).Scan(&rows).Error; err != nil {
		common.SysError(common.LogText("QYDION 导出查询失败: %v", err))
		common.ApiErrorMsg(c, "导出失败，请稍后重试")
		return
	}
	var sb strings.Builder
	sb.WriteString("维度,请求数,输入tokens,输出tokens,收入(元)\n")
	for _, r := range rows {
		sb.WriteString(fmt.Sprintf("%s,%d,%d,%d,%.4f\n",
			qydionCsvSafe(r.Dimension), r.Requests, r.InTokens, r.OutTokens,
			operation_setting.Price*(float64(r.Quota)/common.QuotaPerUnit)))
	}
	c.Header("Content-Disposition", "attachment; filename=qydion-breakdown-"+dim+".csv")
	c.Data(200, "text/csv; charset=utf-8", []byte(sb.String()))
}

// QydionListQuotaTransactions 额度流水列表（审计用）
func QydionListQuotaTransactions(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("p", "1"))
	if page < 1 {
		page = 1
	}
	pageSize := 50
	var total int64
	model.DB.Model(&model.QydionQuotaTransaction{}).Count(&total)
	var list []model.QydionQuotaTransaction
	model.DB.Order("id DESC").Limit(pageSize).Offset((page - 1) * pageSize).Find(&list)
	common.ApiSuccess(c, gin.H{"items": list, "total": total, "page": page, "page_size": pageSize})
}
