package service

// QYDION 邀请返佣（§8.7d）：来源 = 被邀请人「实际消费」（非充值，防套利）。
// 结算：累计未结算返佣 ≥ 最低结算额度（默认 ¥10）时转入邀请人 aff_quota（可原生划转余额）。
// 防刷：①注册 24h 内消费不返佣 ②返佣支出计入 invite_boost 活动（Campaign）预算 ③同 IP 注册不返佣
//   —— ③依赖注册 IP 数据源，原生 users 表未存注册 IP，挂 DEVIATIONS D-019，本期实现①②。

import (
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// QydionOnConsumeRebate 消费返佣钩子：在扣费落账之后调用（失败不影响主流程）。
// quotaConsumed：本笔消费的额度（正数）。
func QydionOnConsumeRebate(userId int, quotaConsumed int64, modelName string) {
	defer func() {
		if r := recover(); r != nil {
			common.SysError(common.LogText("QYDION rebate hook panic: %v", r))
		}
	}()
	if quotaConsumed <= 0 {
		return
	}
	// 返佣默认关闭：开启意味着把可划转的真金白银按消费额比例发给邀请人，
	// 必须由运营在后台显式决定，不能在首次启动时就生效。
	if !QydionSettingBool(QydionKeyRebateEnabled, false) {
		return
	}
	firstRate := decimal.NewFromInt(int64(QydionSettingInt(QydionKeyRebateFirstRate, 10)))
	if firstRate.IsNegative() || firstRate.IsZero() {
		return
	}
	secondRate := decimal.NewFromInt(int64(QydionSettingInt(QydionKeyRebateSecondRate, 0)))

	var user model.User
	if err := model.DB.Select("id", "inviter_id", "created_at").Where("id = ?", userId).First(&user).Error; err != nil {
		return
	}
	if user.InviterId == 0 {
		return
	}
	// 防刷①：注册 24h 内消费不返佣
	if user.CreatedAt > 0 && time.Now().Unix()-user.CreatedAt < 24*3600 {
		return
	}

	rebateFirst := decimal.NewFromInt(quotaConsumed).Mul(firstRate).Div(decimal.NewFromInt(100)).Floor().IntPart()
	// 结算：累计未结算（operator=pending）+ 本笔，达到门槛则整体入 aff_quota
	settleFor := func(inviterId int, amount int64, level int) {
		if amount <= 0 || inviterId <= 0 {
			return
		}
		// 全量安全审计补充修复：事务外预读（ActiveCampaigns/设置缓存走 model.DB 外层
		// 连接）——连接池=1 的部署里事务内再取外层连接会自锁（测试环境实测死锁）。
		budgetCampaignId := 0
		var budgetCampaign *model.QydionCampaign
		if campaigns, err := model.ActiveCampaigns(model.CampaignTypeInviteBoost); err == nil && len(campaigns) > 0 {
			budgetCampaignId = campaigns[0].Id
			budgetCampaign = &campaigns[0]
		}
		minSettle := qydionMoneyToQuotaOrZero(QydionSettingFloat(QydionKeyRebateMinSettleCNY, 10))
		_ = model.DB.Transaction(func(tx *gorm.DB) error {
			// P1 修复（全量安全审计）：先对邀请人用户行加 FOR UPDATE 行锁，串行化同一
			// 邀请人的并发结算——否则两个并发事务会读到同一份 pending SUM，aff_quota
			// 重复计入（aff_quota 可划转余额=真金白银）。锁序：users → campaign → ledger。
			var lockRow model.User
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Select("id").Where("id = ?", inviterId).First(&lockRow).Error; err != nil {
				return err
			}
			// 防刷②：返佣必须挂在有预算的 invite_boost 活动下结算。
			// 没有活动、或活动预算为 0 时一律不结算——早期实现把「无活动」当「不限预算」，
			// 等于返佣完全不受约束，自建邀请环（自己拉小号消费）可以无上限套走 aff_quota。
			if budgetCampaign == nil || budgetCampaign.BudgetQuota <= 0 {
				common.SysError(common.LogText("QYDION 返佣未结算：无有效 invite_boost 预算活动（邀请人%d，本笔 %d 额度）。请运营先建带预算的活动。", inviterId, amount))
				return nil
			}
			result := tx.Model(&model.QydionCampaign{}).
				Where("id = ? AND used_quota + ? <= budget_quota", budgetCampaign.Id, amount).
				Update("used_quota", gorm.Expr("used_quota + ?", amount))
			if result.Error != nil || result.RowsAffected == 0 {
				return nil // 预算耗尽：本笔不返佣
			}
			// 累计未结算
			var sum int64
			_ = tx.Model(&model.QydionQuotaTransaction{}).
				Where("user_id = ? AND type = ? AND operator = ?", inviterId, model.QydionTxTypeRebate, "pending").
				Select("COALESCE(SUM(amount_quota),0)").Scan(&sum).Error
			total := sum + amount
			operator := "pending"
			note := fmt.Sprintf("邀请返佣（%d级，来自用户%d消费 %s）", level, userId, modelName)
			if total >= minSettle {
				// 入账：aff_quota 增加累计额，历史 pending 行标记 settled。
				// 本笔流水仍只记增量——若把 total 再写一条，刚被翻成 settled 的历史
				// 金额会被 SUM 数两遍，账本汇总与 aff_quota 就对不上了。
				operator = "settled"
				note = fmt.Sprintf("邀请返佣结算（%d级，累计 %d quota，本笔 %d）", level, total, amount)
				if err := tx.Model(&model.User{}).Where("id = ?", inviterId).
					Update("aff_quota", gorm.Expr("aff_quota + ?", total)).Error; err != nil {
					return err
				}
				if err := tx.Model(&model.QydionQuotaTransaction{}).
					Where("user_id = ? AND type = ? AND operator = ?", inviterId, model.QydionTxTypeRebate, "pending").
					Update("operator", "settled").Error; err != nil {
					return err
				}
			}
			return tx.Create(&model.QydionQuotaTransaction{
				UserId: inviterId, Type: model.QydionTxTypeRebate,
				AmountQuota: amount, Operator: operator, Note: note,
				RefId: budgetCampaignId, CreatedTime: time.Now().Unix(),
			}).Error
		})
	}

	settleFor(user.InviterId, rebateFirst, 1)
	if secondRate.Sign() > 0 {
		var inviter model.User
		if err := model.DB.Select("id", "inviter_id").Where("id = ?", user.InviterId).First(&inviter).Error; err == nil && inviter.InviterId > 0 {
			rebateSecond := decimal.NewFromInt(quotaConsumed).Mul(secondRate).Div(decimal.NewFromInt(100)).Floor().IntPart()
			settleFor(inviter.InviterId, rebateSecond, 2)
		}
	}
}

func qydionMoneyToQuotaOrZero(moneyCNY float64) int64 {
	q, err := qydionMoneyToQuota(moneyCNY)
	if err != nil {
		return 0
	}
	return q
}
