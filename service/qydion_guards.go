package service

// QYDION 余额风控（§8.5d 防破产闸门）+ 赠额过期回收（§8.7b）
// 1. 单请求 max_tokens 钳制（后台可配，默认 8192，0=关闭）
// 2. 余额 < 0 自动禁用该用户全部令牌 + 通知（防上游响应回来后才扣费的透支窗口）
// 3. 单用户日消费异常告警（默认 ¥500）
// 4. 满送赠额过期回收 cron

import (
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/bytedance/gopkg/util/gopool"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// QydionClampMaxTokens 对请求里的 max_tokens 做钳制；返回生效值（0=请求未设置）。
// 在 OpenAI 兼容与 Claude 两条转发路径的请求预处理处调用（原生触点 ≤10 行/处）。
func QydionClampMaxTokens(requested int) int {
	limit := QydionSettingInt(QydionKeyMaxTokensLimit, 8192)
	if limit <= 0 || requested <= 0 {
		return requested
	}
	if requested > limit {
		return limit
	}
	return requested
}

// QydionClampMaxTokensFields 就地钳制一组 max_tokens 家族字段（nil 跳过）。
// 计费上限取的是这一族里的最大值——relay/request_billing.go:34 用
// max(max_tokens, max_completion_tokens)，只钳其中一个，客户换个字段名
// （或换 OpenAI Responses 格式的 max_output_tokens）就绕过了这道防破产闸门。
func QydionClampMaxTokensFields(fields ...*uint) {
	for _, field := range fields {
		if field == nil {
			continue
		}
		if clamped := QydionClampMaxTokens(int(*field)); clamped >= 0 {
			*field = uint(clamped)
		}
	}
}

// QydionAfterConsumeGuard 消费后风控检查（在扣费落账之后调用，失败不影响主流程）：
// 负余额禁令牌 + 单用户日消费告警。
func QydionAfterConsumeGuard(userId int, quotaConsumed int64, modelName string) {
	defer func() {
		if r := recover(); r != nil {
			common.SysError(common.LogText("QYDION guard panic: %v", r))
		}
	}()
	var user model.User
	if err := model.DB.Select("id", "username", "email", "quota").Where("id = ?", userId).First(&user).Error; err != nil {
		return
	}
	// 2. 透支闸门
	if user.Quota < 0 && QydionSettingBool(QydionKeyAutoDisableOnDebt, true) {
		if err := model.DB.Model(&model.Token{}).
			Where("user_id = ? AND status = ?", userId, common.TokenStatusEnabled).
			Update("status", common.TokenStatusDisabled).Error; err == nil {
			common.SysLog(common.LogText("QYDION 风控：用户 %d 余额为负，已禁用其全部令牌", userId))
			// 邮件必须异步：本钩子跑在请求后置协程池里，SMTP 慢或不通时同步发信
			// 会把 worker 连同连接一起占住——这台单机（SQLite 单写者、1.7G 内存）上会被放大成全站变慢。
			mailTo, mailBody := user.Email, "您的账户余额已透支，为防止进一步扣费，全部 API 令牌已被暂停。请充值后联系管理员恢复。"
			adminAlertBody := fmt.Sprintf("用户 %d（%s）余额透支，已自动禁用其全部 API 令牌。请跟进充值或人工恢复。", userId, user.Email)
			gopool.Go(func() {
				defer func() {
					if r := recover(); r != nil {
						common.SysError(common.LogText("QYDION 透支通知邮件 panic: %v", r))
					}
				}()
				common.SendEmail(common.LogText("QYDION 账户额度告警"), mailTo, mailBody)
				// 管理员抄送（后台缺口⑦）：统一告警出口，收件人未配置/SMTP 未配置时降级日志
				sendAdminAlert("debt-token", "[QYDION] 用户透支已禁用令牌", adminAlertBody)
			})
		}
	}
	// 3. 日消费告警
	threshold := QydionSettingFloat(QydionKeyDailySpendAlertCNY, 500)
	if threshold <= 0 {
		return
	}
	spentQuota := qydionTodayConsumeQuota(userId)
	spentCNY := operation_setting.Price * (float64(spentQuota) / common.QuotaPerUnit)
	if spentCNY >= threshold {
		common.SysLog(common.LogText("QYDION 风控：用户 %d 今日消费 %.2f 元，超过告警阈值 %.2f 元（最近模型 %s）", userId, spentCNY, threshold, modelName))
		// 管理员抄送（后台缺口⑦）：异步（同上），出口内自带冷却去重（同 kind 6h 一封防风暴）
		alertBody := fmt.Sprintf("用户 %d 今日消费 %.2f 元，超过告警阈值 %.2f 元（最近模型 %s）。", userId, spentCNY, threshold, modelName)
		gopool.Go(func() {
			sendAdminAlert("daily-spend", "[QYDION] 单用户日消费超阈", alertBody)
		})
	}
}

// qydionTodayConsumeQuota 用户今日（Asia/Shanghai 语义按服务器本地时区）消费额度合计。
func qydionTodayConsumeQuota(userId int) int64 {
	now := time.Now()
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
	var sum int64
	err := model.LOG_DB.Model(&model.Log{}).
		Where("user_id = ? AND type = ? AND created_at >= ?", userId, model.LogTypeConsume, startOfDay).
		Select("COALESCE(SUM(quota),0)").Scan(&sum).Error
	if err != nil {
		return 0
	}
	return sum
}

// QydionRecoverExpiredBonus 满送赠额过期回收（§8.7b cron 调用）。
//
// 消费口径：先抵赠额（按发放先后），抵完才动本金。因此可回收的只有这批赠额里
// 尚未被消费掉的部分——照原始金额扣，会把新充值的本金当成过期赠额收走。
// 每批改额的剩余额由「发放后的累计消费量」推算，见 qydionBonusRecoverable。
func QydionRecoverExpiredBonus() (int, error) {
	var grants []model.QydionBonusGrant
	if err := model.DB.Where("recovered = ? AND expire_at < ?", false, time.Now()).Limit(500).Find(&grants).Error; err != nil {
		return 0, err
	}
	recovered := 0
	for _, grant := range grants {
		err := model.DB.Transaction(func(tx *gorm.DB) error {
			result := tx.Model(&model.QydionBonusGrant{}).
				Where("id = ? AND recovered = ?", grant.Id, false).
				Update("recovered", true)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 0 {
				return nil // 并发下已被回收
			}
			var user model.User
			// 全量安全审计 P3：改行锁读取用户余额——并发消费期间普通读会拿到 stale 值，
			// 使 quota-deduct 把用户推成负数（违背"不产生负数"的设计意图）
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Select("id", "quota", "used_quota").Where("id = ?", grant.UserId).First(&user).Error; err != nil {
				return err
			}
			// 同一用户的全部批次（含已回收的）都要参与分配，否则后发的批次会重复认领
			// 同一笔消费，前面那批的剩余额就被算少了。
			var history []model.QydionBonusGrant
			if err := tx.Where("user_id = ? AND id <= ?", grant.UserId, grant.Id).
				Order("id").Find(&history).Error; err != nil {
				return err
			}
			recoverable, ok := qydionBonusRecoverable(history, grant, int64(user.UsedQuota))
			if !ok {
				common.SysError(common.LogText("QYDION 赠额过期回收跳过：消费水位早于发放水位 grant=%d user=%d", grant.Id, grant.UserId))
				return tx.Create(&model.QydionQuotaTransaction{
					UserId: grant.UserId, TradeNo: grant.TradeNo, Type: model.QydionTxTypeBonusExpire,
					AmountQuota: 0, Operator: "system",
					Note:  "满送赠额到期，但消费水位不可信，未回收（需人工核对）",
					RefId: int(grant.CampaignId), CreatedTime: time.Now().Unix(),
				}).Error
			}
			deduct := int(recoverable)
			if user.Quota < deduct {
				deduct = user.Quota // 余额不足则扣到 0（不产生负数）
			}
			if deduct > 0 {
				if err := tx.Model(&model.User{}).Where("id = ?", grant.UserId).
					Update("quota", gorm.Expr("quota - ?", deduct)).Error; err != nil {
					return err
				}
			}
			return tx.Create(&model.QydionQuotaTransaction{
				UserId: grant.UserId, TradeNo: grant.TradeNo, Type: model.QydionTxTypeBonusExpire,
				AmountQuota: -int64(deduct), Operator: "system",
				Note:  fmt.Sprintf("满送赠额过期回收（发放 %d，已消费 %d，回收 %d）", grant.Amount, grant.Amount-int64(deduct), deduct),
				RefId: int(grant.CampaignId), CreatedTime: time.Now().Unix(),
			}).Error
		})
		if err == nil {
			recovered++
		}
	}
	return recovered, nil
}

// qydionBonusRecoverable 按「先发放的批次先被消费」算出 target 这批还剩多少可回收。
//
// history 必须是同一用户按 id 升序、且包含 target 的全部批次（含已回收的）：
// 漏掉历史批次就会把历史消费错记到后面的批次上，剩余额被算少、本金被多扣。
//
// 做法是按发放时间分段：每段内的消费先冲抵当时已存在的最早批次，冲完才轮到后面的
// 批次，最后一段延伸到当前累计消费。发放之前就已经发生的消费落在本金上，不会
// 被算进任何批次。
//
// 第二个返回 false 表示累计消费水位低于最早批次的发放水位（计数器倒退，一般来自
// 跨库恢复），消费归属无从判断——调用方必须选择不扣钱，而不是退回按原始金额扣。
func qydionBonusRecoverable(history []model.QydionBonusGrant, target model.QydionBonusGrant, usedQuota int64) (int64, bool) {
	if len(history) == 0 || usedQuota < history[0].ConsumedWatermark {
		return 0, false
	}
	remaining := make([]int64, len(history))
	for i, item := range history {
		remaining[i] = item.Amount
	}
	for i := range history {
		segmentEnd := usedQuota
		if i+1 < len(history) {
			segmentEnd = history[i+1].ConsumedWatermark
		}
		spend := segmentEnd - history[i].ConsumedWatermark
		if spend <= 0 {
			continue
		}
		for j := 0; j <= i && spend > 0; j++ {
			take := min(spend, remaining[j])
			remaining[j] -= take
			spend -= take
		}
	}
	for i, item := range history {
		if item.Id == target.Id {
			return remaining[i], true
		}
	}
	return 0, false
}
