package controller

// QYDION 定时任务（M4）：
// - 每日对账（§8.5c）：03:00 Asia/Shanghai 窗口执行（经系统任务框架 DB-lease 去重，
//   FirstOrCreate 幂等，窗口内重复触发无害）
// - 满送赠额过期回收（§8.7b）：每小时
// 注册走原生 RegisterScheduledSystemTasks（+2 行，见 CHANGES.md）。

import (
	"context"
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/payment"
	"github.com/QuantumNous/new-api/service"
)

type qydionReconHandler struct{}

func (qydionReconHandler) Type() string { return "qydion_reconciliation_daily" }

func (qydionReconHandler) Enabled() bool { return true }

func (qydionReconHandler) Interval() time.Duration { return time.Hour }

func (qydionReconHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		shanghai = time.FixedZone("CST", 8*3600)
	}
	now := time.Now().In(shanghai)
	if now.Hour() != 3 {
		return // 仅 03:00 窗口执行（逐时触发 + 窗口判定 = 钟点对齐）
	}
	count, err := QydionRunReconciliation("")
	if err != nil {
		common.SysError(common.LogText("QYDION 每日对账失败：%v", err))
		return
	}
	common.SysLog(common.LogText("QYDION 每日对账完成：昨日 %d 行", count))
	qydionCheckEstimatedRatio("")
}

type qydionBonusRecoveryHandler struct{}

func (qydionBonusRecoveryHandler) Type() string { return "qydion_bonus_recovery" }

func (qydionBonusRecoveryHandler) Enabled() bool { return true }

func (qydionBonusRecoveryHandler) Interval() time.Duration { return time.Hour }

func (qydionBonusRecoveryHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	count, err := service.QydionRecoverExpiredBonus()
	if err != nil {
		common.SysError(common.LogText("QYDION 赠额回收失败：%v", err))
		return
	}
	if count > 0 {
		common.SysLog(common.LogText("QYDION 赠额过期回收：%d 笔", count))
	}
}

// qydionCheckEstimatedRatio 估算计费占比检查（§8.5a-3）：> 阈值（默认1%）告警。
// 标记来源：原生本地计数时在日志 Other 写入 local_count_tokens:true（service/log_info_generate.go）。
// 死路径修复（后台缺口⑦）：消费日志在 LOG_DB，统计必须用 LOG_DB 句柄——历史上曾查主库，
// LOG_SQL_DSN 分离部署下主库 logs 恒空 → total 恒 0 → 告警永远不触发（句柄已在 408b713 修正，
// 回归测试 TestQydionCheckEstimatedRatioSeparateLogDB 用双库替身锁住该行为）。
// 返回 (total, estimated, ratio) 供回归测试断言。
func qydionCheckEstimatedRatio(dateStr string) (int64, int64, float64) {
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		shanghai = time.FixedZone("CST", 8*3600)
	}
	var day time.Time
	if dateStr == "" {
		day = time.Now().In(shanghai).AddDate(0, 0, -1)
	} else {
		day, _ = time.ParseInLocation("2006-01-02", dateStr, shanghai)
	}
	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, shanghai).Unix()
	end := start + 86400
	var total, estimated int64
	model.LOG_DB.Model(&model.Log{}).Where("type = ? AND created_at >= ? AND created_at < ?", model.LogTypeConsume, start, end).Count(&total)
	model.LOG_DB.Model(&model.Log{}).
		Where("type = ? AND created_at >= ? AND created_at < ? AND other LIKE ?", model.LogTypeConsume, start, end, `%"local_count_tokens":true%`).
		Count(&estimated)
	if total == 0 {
		return 0, 0, 0
	}
	ratio := float64(estimated) / float64(total) * 100
	threshold := service.QydionSettingFloat(service.QydionKeyEstimatedUsageWarn, 1)
	common.SysLog(common.LogText("QYDION 估算计费占比（%s）：%.2f%%（%d/%d），阈值 %.2f%%", day.Format("2006-01-02"), ratio, estimated, total, threshold))
	if ratio > threshold {
		common.SysError(common.LogText("QYDION 告警：估算计费占比 %.2f%% 超过阈值 %.2f%%，请检查上游 usage 返回", ratio, threshold))
		// 管理员邮件（后台缺口⑦）：cron 场景直接同步调用；出口内自带冷却与降级，零真实外发风险由配置兜底
		service.QydionSendAdminAlert("estimated-ratio", "[QYDION] 估算计费占比超阈",
			fmt.Sprintf("日期 %s：估算计费占比 %.2f%%（%d/%d）超过阈值 %.2f%%，请检查上游 usage 返回。", day.Format("2006-01-02"), ratio, estimated, total, threshold))
	}
	return total, estimated, ratio
}

// qydionPayExpireHandler 每小时：在线订单 pending 超 30 分钟 → 先查渠道再决定关单（§8.6a 状态机）
type qydionPayExpireHandler struct{}

func (qydionPayExpireHandler) Type() string { return "qydion_payment_expire" }

func (qydionPayExpireHandler) Enabled() bool { return true }

func (qydionPayExpireHandler) Interval() time.Duration { return time.Hour }
func (qydionPayExpireHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	cutoff := time.Now().Unix() - 1800
	var stale []model.TopUp
	if err := model.DB.Where("status = ? AND payment_provider IN ? AND create_time < ?",
		common.TopUpStatusPending, []string{"alipay_f2f", "wx_native", "stripe"}, cutoff).
		Limit(200).Find(&stale).Error; err != nil {
		common.SysError(common.LogText("QYDION 超时关单扫描失败: %v", err))
		return
	}
	cancelled := 0
	for i := range stale {
		select {
		case <-ctx.Done():
			return
		default:
		}
		topUp := &stale[i]
		// 关单前必须查渠道：回调丢失而用户已付款的单，直接关单会变成 cancelled 且此后不再回查，
		// 钱收了不入账，只能手工改库。查到账就入账，查不出才关单。
		provider := payment.Get(topUp.PaymentProvider)
		if provider == nil || !provider.Enabled() {
			// 渠道不可用 = 无法确认终态，不是「未付款」。留到下轮或人工查证。
			common.SysError(common.LogText("QYDION 超时单无法向渠道确认，跳过关单 trade_no=%s provider=%s", topUp.TradeNo, topUp.PaymentProvider))
			continue
		}
		report, err := provider.QueryStatus(payment.Order{
			TradeNo: topUp.TradeNo, UserId: topUp.UserId, Money: topUp.Money,
			ProviderRef: topUp.ProviderRef,
		})
		if err != nil {
			// 渠道查询失败时不关单：宁可留一笔 pending 等下一轮，也不要把已付款的单关掉
			common.SysError(common.LogText("QYDION 超时关单前查单失败，本单跳过 trade_no=%s: %v", topUp.TradeNo, err))
			continue
		}
		if report.Paid {
			if err := qydionSettleReported(provider, topUp, report, "超时关单前查单"); err != nil {
				common.SysError(common.LogText("QYDION 超时单查得到已支付但入账失败 trade_no=%s: %v", topUp.TradeNo, err))
				continue
			}
			model.RecordLog(topUp.UserId, model.LogTypeTopup, common.NewMessage("在线充值成功，实付 {{money}} 元已到账", map[string]any{"money": fmt.Sprintf("%.2f", topUp.Money)}))
			common.SysLog(common.LogText("QYDION 超时单经查单补入账 trade_no=%s", topUp.TradeNo))
			continue
		}
		// 渠道确认未付款：关单与放回本单的券必须原子，否则留下券被永久锁住或券放回但订单可再入账。
		if cancelledNow, err := model.CancelPendingOrderWithCoupon(topUp.TradeNo, topUp.CouponId); err != nil {
			common.SysError(common.LogText("QYDION 超时关单失败 trade_no=%s: %v", topUp.TradeNo, err))
		} else if cancelledNow {
			cancelled++
		}
	}
	if cancelled > 0 {
		common.SysLog(common.LogText("QYDION 超时关单 %d 笔（pending>30min，均已先查渠道确认未支付）", cancelled))
	}
}
