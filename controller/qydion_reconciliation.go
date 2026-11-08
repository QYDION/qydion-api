package controller

// QYDION 每日对账（§8.5c）
// - UpstreamUsageFetcher 适配器接口：上游「用量明细查询」契约待用户提供（附录B-1），
//   当前实现为 stub（记 no_upstream）；拿到契约后补一个实现类注册即可，不动其他代码。
// - 每日 03:00（Asia/Shanghai）由 system task runner 调度（见 system_task_handlers.go 注册）。

import (
	"fmt"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
)

// UpstreamUsageRow 上游账单行
type UpstreamUsageRow struct {
	ModelName    string
	InputTokens  int64
	OutputTokens int64
	Amount       float64 // 上游计费金额（人民币）
}

// UpstreamUsageFetcher 拉某渠道某日的上游用量（§8.5c 接口定义）
type UpstreamUsageFetcher interface {
	FetchDaily(channelId int, date time.Time) ([]UpstreamUsageRow, error)
}

// StubUpstreamUsageFetcher 占位实现：上游契约未提供（附录B-1）。
type StubUpstreamUsageFetcher struct{}

func (s *StubUpstreamUsageFetcher) FetchDaily(channelId int, date time.Time) ([]UpstreamUsageRow, error) {
	return nil, nil // 无数据（非错误）：对账结果标记 no_upstream
}

var qydionUpstreamFetcher UpstreamUsageFetcher = &StubUpstreamUsageFetcher{}

// SetQydionUpstreamFetcher 拿到上游契约后替换实现（供未来注册用）
func SetQydionUpstreamFetcher(f UpstreamUsageFetcher) {
	qydionUpstreamFetcher = f
}

// QydionRunReconciliation 执行某日对账（date 为空=昨日，Asia/Shanghai）
func QydionRunReconciliation(dateStr string) (int, error) {
	// LoadLocation 在没有 tzdata 的镜像里会返回 error + nil *Location，
	// 忽略它再传进 time.Date/In 就是 panic；对账跑在无 recover 的系统任务框架里，
	// 一次 panic 直接把整个网关带走。固定 +08:00 偏移是语义等价的兜底。
	shanghai, locErr := time.LoadLocation("Asia/Shanghai")
	if locErr != nil || shanghai == nil {
		shanghai = time.FixedZone("CST", 8*3600)
		common.SysError(common.LogText("QYDION 对账：Asia/Shanghai 时区数据不可用，退回固定 +08:00（%v）", locErr))
	}
	var day time.Time
	if dateStr == "" {
		day = time.Now().In(shanghai).AddDate(0, 0, -1)
	} else {
		var err error
		day, err = time.ParseInLocation("2006-01-02", dateStr, shanghai)
		if err != nil {
			return 0, fmt.Errorf("日期格式无效（应为 YYYY-MM-DD）")
		}
	}
	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, shanghai).Unix()
	end := start + 86400

	// 1. 本地 logs 汇总：昨日各渠道×模型 消费 tokens 与成本（售价口径折算成本：quota/QuotaPerUnit×Price）
	type localAgg struct {
		ChannelId int
		Model     string
		Tokens    int64
		Quota     int64
	}
	var locals []localAgg
	// 消费日志写在 LOG_DB（配了独立日志库时主库的 logs 表是空的，对账会恒得 0 差异）
	err := model.LOG_DB.Model(&model.Log{}).
		Select("channel_id, model_name as model, COALESCE(SUM(prompt_tokens + completion_tokens),0) as tokens, COALESCE(SUM(quota),0) as quota").
		Where("type = ? AND created_at >= ? AND created_at < ?", model.LogTypeConsume, start, end).
		Group("channel_id, model_name").
		Scan(&locals).Error
	if err != nil {
		return 0, err
	}

	// 2. 上游账单（契约 stub → 空）
	upstreamByChannelModel := map[string]UpstreamUsageRow{}
	channelIds := map[int]bool{}
	for _, l := range locals {
		channelIds[l.ChannelId] = true
	}
	for chId := range channelIds {
		rows, err := qydionUpstreamFetcher.FetchDaily(chId, day)
		if err != nil {
			common.SysError(common.LogText("QYDION 对账：渠道 %d 上游数据拉取失败 %v", chId, err))
			continue
		}
		for _, row := range rows {
			key := fmt.Sprintf("%d|%s", chId, row.ModelName)
			agg, ok := upstreamByChannelModel[key]
			if !ok {
				agg = row
			} else {
				agg.InputTokens += row.InputTokens
				agg.OutputTokens += row.OutputTokens
				agg.Amount += row.Amount
			}
			upstreamByChannelModel[key] = agg
		}
	}

	// 3. 逐行比对（tokens 差异>2% 或 金额差异>2% → pending）
	diffThresholdTokens := service.QydionSettingFloat(service.QydionKeyReconDiffTokens, 2.0)
	diffThresholdAmount := service.QydionSettingFloat(service.QydionKeyReconDiffAmount, 2.0)
	dateKey := day.Format("2006-01-02")
	count := 0
	for _, l := range locals {
		key := fmt.Sprintf("%d|%s", l.ChannelId, l.Model)
		up, hasUpstream := upstreamByChannelModel[key]
		localTokens := l.Tokens
		upTokens := up.InputTokens + up.OutputTokens
		localAmount := float64(l.Quota) / common.QuotaPerUnit * operation_setting.Price // 成本口径（§2.3 对账基准）
		upAmount := up.Amount

		diffRatio := 0.0
		if hasUpstream && upTokens > 0 {
			d := localTokens - upTokens
			if d < 0 {
				d = -d
			}
			diffRatio = float64(d) / float64(upTokens) * 100
		}
		status := model.ReconStatusNoUpstream
		note := "上游用量契约未提供（stub）"
		if hasUpstream {
			note = ""
			amountDiff := 0.0
			if upAmount > 0 {
				a := localAmount - upAmount
				if a < 0 {
					a = -a
				}
				amountDiff = a / upAmount * 100
			}
			if diffRatio > diffThresholdTokens || amountDiff > diffThresholdAmount {
				status = model.ReconStatusPending
				note = fmt.Sprintf("tokens差异 %.2f%%，金额差异 %.2f%%", diffRatio, amountDiff)
				common.SysLog(common.LogText("QYDION 对账差异：%s 渠道%d %s %s", dateKey, l.ChannelId, l.Model, note))
			} else {
				status = model.ReconStatusOk
			}
		}
		rec := model.QydionReconciliationDaily{
			Date: dateKey, ChannelId: l.ChannelId, Model: l.Model,
			LocalTokens: localTokens, UpstreamTokens: upTokens,
			LocalAmount: localAmount, UpstreamAmount: upAmount,
			DiffRatio: diffRatio, Status: status, Note: note,
			CreatedTime: time.Now().Unix(),
		}
		if err := model.DB.Where("date = ? AND channel_id = ? AND model = ?", dateKey, l.ChannelId, l.Model).
			Assign(rec).FirstOrCreate(&rec).Error; err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// QydionRunReconciliationAPI 管理侧：手动触发对账
func QydionRunReconciliationAPI(c *gin.Context) {
	var req struct {
		Date string `json:"date"`
	}
	_ = c.ShouldBindJSON(&req) // date 可选
	count, err := QydionRunReconciliation(req.Date)
	if err != nil {
		common.ApiErrorMsg(c, "对账失败："+err.Error())
		return
	}
	common.ApiSuccess(c, gin.H{"rows": count, "date": req.Date})
}

// QydionListReconciliation 管理侧：对账结果列表（date 可选）
func QydionListReconciliation(c *gin.Context) {
	date := strings.TrimSpace(c.Query("date"))
	var list []model.QydionReconciliationDaily
	db := model.DB.Order("date DESC, channel_id ASC, model ASC").Limit(500)
	if date != "" {
		db = db.Where("date = ?", date)
	}
	if err := db.Find(&list).Error; err != nil {
		common.ApiErrorMsg(c, "查询失败")
		return
	}
	common.ApiSuccess(c, list)
}
