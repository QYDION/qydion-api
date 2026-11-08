package service

// QYDION 唯一入账点（规格书 §8.6d）
// - QydionCreditCorporateOrder：对公审核通过 → 一个事务内完成 到账+满送+核销+流水
// - QydionOnTopupSettled：在线支付（原生 Provider）提交成功后的挂钩（幂等），补发满送/核销/流水
// 注意：邀请返佣按 §8.7d「被邀请人实际消费」结算，在消费钩子（qydion_rebate.go）里做，
// 不在充值入账里做（§8.6d-4 与 §8.7d 冲突，取更具体的 §8.7d，见 DEVIATIONS D-021）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// TopupStatusReviewing 对公订单状态：用户已提交凭证，待人工审核（§8.6a）。
// paid/closed 复用原生 success 状态（见 D-020 映射说明）。
const TopupStatusReviewing = "reviewing"
const TopupStatusRejected = "rejected"
// TopupStatusCancelled 与原生状态常量同源，model 层的原子关单要引用同一个值。
const TopupStatusCancelled = common.TopUpStatusCancelled

// qydionMoneyToQuota 人民币金额 → 额度（与原生口径一致：money = usd × Price；quota = usd × QuotaPerUnit）。
func qydionMoneyToQuota(moneyCNY float64) (int64, error) {
	price := operation_setting.Price
	if price <= 0 {
		return 0, errors.New("充值单价（Price）未配置或非法")
	}
	if moneyCNY <= 0 {
		return 0, errors.New("金额必须大于 0")
	}
	usd := decimal.NewFromFloat(moneyCNY).Div(decimal.NewFromFloat(price))
	quota := usd.Mul(decimal.NewFromFloat(common.QuotaPerUnit))
	q, err := common.WalletQuotaFromDecimalStrict(quota)
	if err != nil {
		return 0, err
	}
	return int64(q), nil
}

// QydionQuotaForMoney 下单时算定应到账额度，与入账使用同一份换算。
// 快照让「补单」「回调」「查单」不再各自解释 Money：单价中途被改、
// 或订单被误当美元处理，都不会改变用户应得的那笔额度。
func QydionQuotaForMoney(moneyCNY float64) (int64, error) {
	return qydionMoneyToQuota(moneyCNY)
}

// qydionOrderQuota 一笔订单应到账的额度：有快照就用快照，没有才按当前单价折算。
func qydionOrderQuota(locked *model.TopUp) (int64, error) {
	if locked.QuotaSnapshot > 0 {
		return locked.QuotaSnapshot, nil
	}
	if locked.Money <= 0 {
		return 0, errors.New("订单金额非法，且无额度快照可用")
	}
	return qydionMoneyToQuota(locked.Money)
}

// qydionLockOrderTx 按 tradeNo 行级锁订单（兼容 MySQL/PG 引号）。
func qydionLockOrderTx(tx *gorm.DB, tradeNo string) *gorm.DB {
	refCol := "`trade_no`"
	if common.UsingMainDatabase(common.DatabaseTypePostgreSQL) {
		refCol = `"trade_no"`
	}
	return tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(refCol+" = ?", tradeNo)
}

// qydionCreditQuotaTx 事务内加额度（复制原生 creditTopUpQuota 的封顶语义：quota ≤ Max-credit 才加）。
func qydionCreditQuotaTx(tx *gorm.DB, userId int, credit int64) error {
	if credit <= 0 || credit > common.MaxWalletQuota {
		return errors.New("入账额度非法")
	}
	maxCurrent := common.MaxWalletQuota - credit
	result := tx.Model(&model.User{}).
		Where("id = ? AND quota <= ?", userId, maxCurrent).
		Update("quota", gorm.Expr("quota + ?", credit))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return model.ErrTopUpQuotaLimitExceeded
	}
	return nil
}

// topupBonusRule §8.7b 满送规则 schema
type topupBonusRule struct {
	Tiers           []topupBonusTier `json:"tiers"`
	FirstTimeOnly   bool             `json:"first_time_only"`
	BonusExpiryDays int              `json:"bonus_expiry_days"`
}

type topupBonusTier struct {
	Min   float64 `json:"min"`   // 实付金额（元）门槛
	Bonus float64 `json:"bonus"` // 赠送金额（元）
}

// matchBonusTier 命中实付金额 ≥ min 的最高档。
func matchBonusTier(rule *topupBonusRule, payMoney float64) *topupBonusTier {
	var best *topupBonusTier
	for i := range rule.Tiers {
		if payMoney >= rule.Tiers[i].Min {
			if best == nil || rule.Tiers[i].Min >= best.Min {
				t := rule.Tiers[i]
				best = &t
			}
		}
	}
	return best
}

// qydionGrantBonusInsideTx 满送发放（事务内）：预算扣减 + 赠额入账 + 过期记录 + 流水。
// 返回 (bonusQuota, campaignId, error)；无命中返回 0,0,nil。
// campaigns 由调用方在**事务外**预取（ActiveCampaigns 走 model.DB 外层连接，
// 连接池=1 的部署里在事务内调用会自锁——全量安全审计实测发现）。
func qydionGrantBonusInsideTx(tx *gorm.DB, campaigns []model.QydionCampaign, userId int, tradeNo string, payMoney float64) (int64, int, error) {
	// 全量安全审计 P3：先对用户行加 FOR UPDATE，串行化同一用户的并发满送判定
	// （否则两个并发事务都能读到 cnt=0，per_user_limit/first_time_only 被绕过多送）
	var lockRow model.User
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("id").Where("id = ?", userId).First(&lockRow).Error; err != nil {
		return 0, 0, err
	}
	if len(campaigns) == 0 {
		return 0, 0, nil
	}
	for _, campaign := range campaigns {
		var rule topupBonusRule
		if err := json.Unmarshal([]byte(campaign.Rule), &rule); err != nil {
			continue // 规则损坏跳过，不影响主入账
		}
		tier := matchBonusTier(&rule, payMoney)
		if tier == nil || tier.Bonus <= 0 {
			continue
		}
		// 每用户限次（0=不限）：按流水表 bonus 记录计数
		if campaign.PerUserLimit > 0 {
			var cnt int64
			if err := tx.Model(&model.QydionQuotaTransaction{}).
				Where("user_id = ? AND type = ? AND ref_id = ?", userId, model.QydionTxTypeBonus, campaign.Id).
				Count(&cnt).Error; err != nil {
				return 0, 0, err
			}
			if cnt >= int64(campaign.PerUserLimit) {
				continue
			}
		}
		// 仅首充
		if rule.FirstTimeOnly {
			var cnt int64
			if err := tx.Model(&model.QydionQuotaTransaction{}).
				Where("user_id = ? AND type = ?", userId, model.QydionTxTypeTopup).
				Count(&cnt).Error; err != nil {
				return 0, 0, err
			}
			if cnt > 0 {
				continue
			}
		}
		bonusQuota, err := qydionMoneyToQuota(tier.Bonus)
		if err != nil || bonusQuota <= 0 {
			continue
		}
		// 预算（0=不限）
		if campaign.BudgetQuota > 0 {
			result := tx.Model(&model.QydionCampaign{}).
				Where("id = ? AND used_quota + ? <= budget_quota", campaign.Id, bonusQuota).
				Update("used_quota", gorm.Expr("used_quota + ?", bonusQuota))
			if result.Error != nil {
				return 0, 0, result.Error
			}
			if result.RowsAffected == 0 {
				continue // 预算耗尽
			}
		}
		// 赠额入账 + 过期记录
		expiryDays := rule.BonusExpiryDays
		if expiryDays <= 0 {
			expiryDays = 90
		}
		// 消费水位快照：过期回收只动「发放之后还没消费掉的那部分」。
		// 必须在同一事务内读，否则并发消费会把水位记低，回收就多扣。
		var grantee model.User
		if err := tx.Select("used_quota").Where("id = ?", userId).First(&grantee).Error; err != nil {
			return 0, 0, err
		}
		grant := model.QydionBonusGrant{
			UserId: userId, TradeNo: tradeNo, CampaignId: campaign.Id,
			Amount: bonusQuota, ExpireAt: time.Now().AddDate(0, 0, expiryDays),
			CreatedTime: time.Now().Unix(), ConsumedWatermark: int64(grantee.UsedQuota),
		}
		if err := tx.Create(&grant).Error; err != nil {
			return 0, 0, err
		}
		if err := tx.Model(&model.User{}).Where("id = ?", userId).
			Update("quota", gorm.Expr("quota + ?", bonusQuota)).Error; err != nil {
			return 0, 0, err
		}
		// 余额快照（E2E 发现 bonus 行 after=0 的审计瑕疵修复）
		var bonusUser model.User
		balanceAfter := int64(0)
		if err := tx.Select("quota").Where("id = ?", userId).First(&bonusUser).Error; err == nil {
			balanceAfter = int64(bonusUser.Quota)
		}
		ledger := model.QydionQuotaTransaction{
			UserId: userId, TradeNo: tradeNo, Type: model.QydionTxTypeBonus,
			AmountQuota: bonusQuota, Money: tier.Bonus, Operator: "system", BalanceAfter: balanceAfter,
			Note:  fmt.Sprintf("满送活动「%s」档位 %.2f 元", campaign.Name, tier.Bonus),
			RefId: campaign.Id, CreatedTime: time.Now().Unix(),
		}
		if err := tx.Create(&ledger).Error; err != nil {
			return 0, 0, err
		}
		return bonusQuota, campaign.Id, nil
	}
	return 0, 0, nil
}

// QydionCreditCorporateOrder 对公转账审核通过 → 统一入账（§8.6c/§8.6d）。
// 幂等：订单行锁 + 状态机（仅 reviewing 可入账）；重复调用返回 alreadyDone。
func QydionCreditCorporateOrder(topUp *model.TopUp, operator string, note string) (alreadyDone bool, err error) {
	if topUp == nil || topUp.TradeNo == "" {
		return false, errors.New("订单号缺失")
	}
	var baseQuota int64
	var bonusQuota int64
	// 满送活动预取（事务外，防连接池=1 自锁）
	bonusCampaigns, _ := model.ActiveCampaigns(model.CampaignTypeTopupBonus)
	err = model.DB.Transaction(func(tx *gorm.DB) error {
		locked := &model.TopUp{}
		if err := qydionLockOrderTx(tx, topUp.TradeNo).First(locked).Error; err != nil {
			return model.ErrTopUpNotFound
		}
		if locked.Status == common.TopUpStatusSuccess {
			alreadyDone = true
			return nil
		}
		if locked.Status != TopupStatusReviewing {
			return fmt.Errorf("订单状态为 %s，仅待审核订单可入账", locked.Status)
		}
		baseQuota, err = qydionCreditOrderCore(tx, bonusCampaigns, locked, operator, note)
		if err != nil {
			return err
		}
		bonusQuota = locked.BonusQuota
		topUp.Status = locked.Status
		topUp.BonusQuota = bonusQuota
		return nil
	})
	if err != nil {
		return false, err
	}
	if !alreadyDone {
		model.SyncCreditUserQuotaCachePublic(topUp.UserId, baseQuota, "corporate topup")
		common.SysLog(common.LogText("QYDION 对公入账成功 trade_no=%s user_id=%d base=%d bonus=%d", topUp.TradeNo, topUp.UserId, baseQuota, bonusQuota))
	}
	// 必须回传真实的 alreadyDone：吞掉它会让重复点击「审核通过」的后台
	// 误判为新入账，给用户重复发「已到账」邮件并重复登记流水，把重复操作掩盖成正常流程。
	return alreadyDone, nil
}

// QydionCreditOnlineOrder 在线支付（QYDION Provider：alipay_f2f/wx_native/stripe）
// 到账统一入口（M7）：pending → success，一个事务内 到账+券核销+满送+流水。
// 幂等：订单行锁 + 状态机（仅 pending 可入账；已 success 返回 alreadyDone）。
func QydionCreditOnlineOrder(topUp *model.TopUp, operator string, note string) (alreadyDone bool, err error) {
	if topUp == nil || topUp.TradeNo == "" {
		return false, errors.New("订单号缺失")
	}
	var baseQuota int64
	var bonusQuota int64
	bonusCampaigns, _ := model.ActiveCampaigns(model.CampaignTypeTopupBonus)
	err = model.DB.Transaction(func(tx *gorm.DB) error {
		locked := &model.TopUp{}
		if err := qydionLockOrderTx(tx, topUp.TradeNo).First(locked).Error; err != nil {
			return model.ErrTopUpNotFound
		}
		if locked.Status == common.TopUpStatusSuccess {
			alreadyDone = true
			return nil
		}
		if locked.Status != common.TopUpStatusPending {
			return fmt.Errorf("订单状态为 %s，仅待支付订单可入账", locked.Status)
		}
		baseQuota, err = qydionCreditOrderCore(tx, bonusCampaigns, locked, operator, note)
		if err != nil {
			return err
		}
		bonusQuota = locked.BonusQuota
		topUp.Status = locked.Status
		topUp.BonusQuota = bonusQuota
		return nil
	})
	if err != nil {
		return false, err
	}
	if !alreadyDone {
		model.SyncCreditUserQuotaCachePublic(topUp.UserId, baseQuota, "qydion online topup")
		common.SysLog(common.LogText("QYDION 在线入账成功 provider=%s trade_no=%s user_id=%d base=%d bonus=%d",
			topUp.PaymentProvider, topUp.TradeNo, topUp.UserId, baseQuota, bonusQuota))
	}
	return alreadyDone, nil
}

// QydionResettleCancelledOrder 把「渠道已查证付款、但订单已被关成 cancelled」的单原子转回
// pending，再走统一入账。渠道查证（网络调用）必须由调用方在此之前完成——SQLite 单写者的
// 机器上不能把外部请求包进事务，否则一次慢查询能把整库堵死。
// 入账失败一律回滚成 cancelled，不留「状态已放行但钱没到账」的中间态。
// 原生 /api/topup/complete 只受理 pending，缺的就是这个补入账出口。
func QydionResettleCancelledOrder(tradeNo string, operator string, note string) error {
	if tradeNo == "" {
		return errors.New("订单号缺失")
	}
	res := model.DB.Model(&model.TopUp{}).
		Where("trade_no = ? AND status = ?", tradeNo, TopupStatusCancelled).
		Update("status", common.TopUpStatusPending)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("订单不处于已关单状态，不能补入账（可能已被其他操作处理）")
	}
	revert := func(cause error) error {
		if rb := model.DB.Model(&model.TopUp{}).
			Where("trade_no = ? AND status = ?", tradeNo, common.TopUpStatusPending).
			Update("status", TopupStatusCancelled); rb.Error != nil {
			common.SysError(common.LogText("QYDION 补入账回滚失败 trade_no=%s: %v（原因：%v）", tradeNo, rb.Error, cause))
			return fmt.Errorf("补入账失败且回滚失败，需人工核对：%w", cause)
		}
		return cause
	}
	topUp := model.GetTopUpByTradeNo(tradeNo)
	if topUp == nil {
		return revert(errors.New("订单不存在"))
	}
	if _, err := QydionCreditOnlineOrder(topUp, operator, note); err != nil {
		return revert(err)
	}
	common.SysLog(common.LogText("QYDION 已关单订单凭渠道查证补入账成功 trade_no=%s operator=%s", tradeNo, operator))
	return nil
}

// qydionCreditOrderCore 事务内共用入账核心：置 success + 到账 + 券核销 + 满送 + 流水。
// 返回 baseQuota（bonus 写入 locked.BonusQuota）。调用方须已锁订单行并校验状态。
func qydionCreditOrderCore(tx *gorm.DB, bonusCampaigns []model.QydionCampaign, locked *model.TopUp, operator, note string) (int64, error) {
	// 1. 到账额度：优先用下单时的额度快照；历史订单没有快照才按当前单价折算
	q, err := qydionOrderQuota(locked)
	if err != nil {
		return 0, err
	}
	locked.CompleteTime = time.Now().Unix()
	locked.Status = common.TopUpStatusSuccess
	if err := tx.Save(locked).Error; err != nil {
		return 0, err
	}
	if err := qydionCreditQuotaTx(tx, locked.UserId, q); err != nil {
		return 0, err
	}
	// 3. 优惠券核销。券在下单时已预占为 locked，这里只接受两种来源：
	//    本人名下的 locked 单（在线/对公下单路径），或仍为 unused 的券（未走预占的历史/原生路径）。
	//    RowsAffected 必须为 1：0 行意味着这张券已归他人，本单折后金额来源不成立，绝不能静默通过。
	if locked.CouponId > 0 {
		result := tx.Model(&model.QydionCoupon{}).
			Where("id = ? AND (status = ? OR (status = ? AND used_by = ?))",
				locked.CouponId, model.CouponStatusUnused, model.CouponStatusLocked, locked.UserId).
			Updates(map[string]any{"status": model.CouponStatusUsed, "used_by": locked.UserId, "used_at": time.Now()})
		if result.Error != nil {
			return 0, result.Error
		}
		if result.RowsAffected == 0 {
			return 0, fmt.Errorf("优惠券 #%d 已被其他订单使用，本单按折后金额入账被拒绝，请人工核对", locked.CouponId)
		}
	}
	// 2. 满送（事务内）
	bonusQuota, _, err := qydionGrantBonusInsideTx(tx, bonusCampaigns, locked.UserId, locked.TradeNo, locked.Money)
	if err != nil {
		return 0, err
	}
	locked.BonusQuota = bonusQuota
	if err := tx.Model(&model.TopUp{}).Where("trade_no = ?", locked.TradeNo).
		Update("bonus_quota", bonusQuota).Error; err != nil {
		return 0, err
	}
	// 4. 到账流水（含余额快照）
	var user model.User
	if err := tx.Select("quota").Where("id = ?", locked.UserId).First(&user).Error; err != nil {
		return 0, err
	}
	ledger := model.QydionQuotaTransaction{
		UserId: locked.UserId, TradeNo: locked.TradeNo, Type: model.QydionTxTypeTopup,
		AmountQuota: q, Money: locked.Money, BalanceAfter: int64(user.Quota),
		Operator: operator, Note: note, RefId: int(locked.CouponId),
		CreatedTime: time.Now().Unix(),
	}
	if err := tx.Create(&ledger).Error; err != nil {
		return 0, err
	}
	return q, nil
}

// QydionOnTopupSettled 原生 Provider（易支付等）入账成功后的挂钩（事务已提交，尽力而为+幂等）：
// 补发满送活动额度、核销订单挂的券、写额度流水。
// 额度一律从库内订单行重算，不采信调用方快照，避免并发下按旧值记账。
// 幂等：以流水表 (trade_no, type=topup) 是否存在为准。
// 全量安全审计 P3 加固：幂等检查移入事务内并在其前锁订单行（消除 TOCTOU）；
// 各步错误不再吞掉——记 SysError 供对账（仍不影响主流程返回）。
func QydionOnTopupSettled(tradeNo string) {
	if tradeNo == "" {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			common.SysError(common.LogText("QYDION topup hook panic: %v", r))
		}
	}()
	// 满送活动预取（事务外，防连接池=1 自锁）
	bonusCampaigns, _ := model.ActiveCampaigns(model.CampaignTypeTopupBonus)
	_ = model.DB.Transaction(func(tx *gorm.DB) error {
		topUp := &model.TopUp{}
		if err := qydionLockOrderTx(tx, tradeNo).First(topUp).Error; err != nil {
			common.SysError(common.LogText("QYDION topup hook 订单不存在 trade_no=%s", tradeNo))
			return nil
		}
		var cnt int64
		if err := tx.Model(&model.QydionQuotaTransaction{}).
			Where("trade_no = ? AND type = ?", topUp.TradeNo, model.QydionTxTypeTopup).Count(&cnt).Error; err != nil {
			common.SysError(common.LogText("QYDION topup hook 幂等检查失败 trade_no=%s: %v", topUp.TradeNo, err))
			return nil
		}
		if cnt > 0 {
			return nil // 已入账过（幂等）
		}
		if topUp.CouponId > 0 {
			result := tx.Model(&model.QydionCoupon{}).
				Where("id = ? AND (status = ? OR (status = ? AND used_by = ?))",
					topUp.CouponId, model.CouponStatusUnused, model.CouponStatusLocked, topUp.UserId).
				Updates(map[string]any{"status": model.CouponStatusUsed, "used_by": topUp.UserId, "used_at": time.Now()})
			if result.Error != nil {
				common.SysError(common.LogText("QYDION topup hook 券核销失败 trade_no=%s coupon=%d: %v", topUp.TradeNo, topUp.CouponId, result.Error))
			} else if result.RowsAffected == 0 {
				// 券已被其他单消耗：资金已按折后价收取，此处仅告警供人工对账（不阻断流水）
				common.SysError(common.LogText("QYDION topup hook 券 #%d 已被使用（trade_no=%s），请人工对账", topUp.CouponId, topUp.TradeNo))
			}
		}
		if _, _, err := qydionGrantBonusInsideTx(tx, bonusCampaigns, topUp.UserId, topUp.TradeNo, topUp.Money); err != nil {
			common.SysError(common.LogText("QYDION topup hook 满送失败 trade_no=%s: %v", topUp.TradeNo, err))
		}
		// 入账额度按库内订单行重算（原生 epay 以 Amount=美元计数），不采信调用方快照
		baseQuota, qErr := common.WalletQuotaFromDecimalStrict(
			decimal.NewFromInt(topUp.Amount).Mul(decimal.NewFromFloat(common.QuotaPerUnit)))
		if qErr != nil || baseQuota <= 0 {
			common.SysError(common.LogText("QYDION topup hook 额度换算失败 trade_no=%s amount=%d: %v", topUp.TradeNo, topUp.Amount, qErr))
			return nil
		}
		var user model.User
		if err := tx.Select("quota").Where("id = ?", topUp.UserId).First(&user).Error; err == nil {
			ledger := model.QydionQuotaTransaction{
				UserId: topUp.UserId, TradeNo: topUp.TradeNo, Type: model.QydionTxTypeTopup,
				AmountQuota: int64(baseQuota), Money: topUp.Money, BalanceAfter: int64(user.Quota),
				Operator: "system", Note: "在线支付入账", CreatedTime: time.Now().Unix(),
				RefId: int(topUp.CouponId),
			}
			if err := tx.Create(&ledger).Error; err != nil {
				common.SysError(common.LogText("QYDION topup hook 流水写入失败 trade_no=%s: %v", topUp.TradeNo, err))
			}
		}
		return nil
	})
}
