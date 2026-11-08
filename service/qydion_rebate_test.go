package service

// QYDION 全量安全审计 P1 回归测试：返佣结算链路（含修复引入的用户行锁路径）。
// 锁子在 SQLite 上被驱动忽略（单写者本就串行），本测试验证的是：结算语义
// （累计 SUM→门槛→aff_quota 入账→pending 置 settled→新流水）在加锁重构后不回归。

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/require"
)

// qydionEnableRebateBudget 打开返佣并挂一个有预算的 invite_boost 活动。
// 两道闸门现在都必须显式满足：QydionRebateEnabled 默认关闭（首次启动就按消费额发
// 可划转的真金白银不合理），且没有预算活动不结算（早期把「无活动」当「不限预算」，
// 自建邀请环可以无上限套走 aff_quota）。
// 返佣的 aff_quota 是可划转余额，测试直接断言结算后的入账额。
func qydionEnableRebateBudget(t *testing.T, budget int64) {
	t.Helper()
	require.NoError(t, model.DB.AutoMigrate(&model.Option{}, &model.QydionCampaign{}))
	require.NoError(t, model.DB.Create(&model.Option{
		Key: QydionKeyRebateEnabled, Value: "true",
	}).Error)
	campaign := model.QydionCampaign{
		Name: "rebate-budget", Type: model.CampaignTypeInviteBoost,
		StartsAt: time.Now().Add(-time.Hour), EndsAt: time.Now().Add(24 * time.Hour),
		Status: model.CampaignStatusActive, BudgetQuota: budget, Rule: "{}",
	}
	require.NoError(t, model.DB.Create(&campaign).Error)
	QydionSettingsBustCache()
	t.Cleanup(func() {
		model.DB.Where("key = ?", QydionKeyRebateEnabled).Delete(&model.Option{})
		model.DB.Where("type = ?", model.CampaignTypeInviteBoost).Delete(&model.QydionCampaign{})
		QydionSettingsBustCache()
	})
}

func TestQydionRebateSettleChain(t *testing.T) {
	qydionEnableRebateBudget(t, 1000000000)
	if err := model.DB.AutoMigrate(
		&model.QydionQuotaTransaction{},
		&model.QydionCampaign{},
		&model.QydionBonusGrant{},
		&model.QydionTopupVoucher{},
		&model.QydionModelExt{},
	); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	// 造人：A（邀请人）、B（被邀请人，注册 25h 前——过 24h 防刷窗）
	now := time.Now().Unix()
	inviter := model.User{Username: "rebate-A", InviterId: 0, Quota: 0, AffQuota: 0, AffCode: "aff-rebate-A"}
	if err := model.DB.Create(&inviter).Error; err != nil {
		t.Fatalf("造邀请人失败: %v", err)
	}
	invitee := model.User{Username: "rebate-B", InviterId: inviter.Id, Quota: 0, CreatedAt: now - 25*3600, AffCode: "aff-rebate-B"}
	if err := model.DB.Create(&invitee).Error; err != nil {
		t.Fatalf("造被邀请人失败: %v", err)
	}
	// 历史累计 pending：1,000,000 quota（≈¥14.6）
	if err := model.DB.Create(&model.QydionQuotaTransaction{
		UserId: inviter.Id, Type: model.QydionTxTypeRebate, AmountQuota: 1000000,
		Operator: "pending", RefId: 0, CreatedTime: now - 3600,
	}).Error; err != nil {
		t.Fatalf("造历史 pending 失败: %v", err)
	}

	// B 消费 10,000,000 quota → 一级返佣 10% = 1,000,000；累计 2,000,000 ≥ 结算门槛（¥10≈684,931）→ 整体结算
	QydionOnConsumeRebate(int(invitee.Id), 10000000, "mock-gpt")

	var after model.User
	if err := model.DB.Select("aff_quota").Where("id = ?", inviter.Id).First(&after).Error; err != nil {
		t.Fatalf("读邀请人失败: %v", err)
	}
	if after.AffQuota != 2000000 {
		t.Errorf("aff_quota 期望 2000000（历史1M+本笔1M 整体结算），实际 %d", after.AffQuota)
	}
	var pendingLeft int64
	model.DB.Model(&model.QydionQuotaTransaction{}).
		Where("user_id = ? AND type = ? AND operator = ?", inviter.Id, model.QydionTxTypeRebate, "pending").
		Count(&pendingLeft)
	if pendingLeft != 0 {
		t.Errorf("结算后应无 pending 残留，实际 %d 条", pendingLeft)
	}
	var settledLedger model.QydionQuotaTransaction
	if err := model.DB.Where("user_id = ? AND type = ? AND operator = ?", inviter.Id, model.QydionTxTypeRebate, "settled").
		Order("id DESC").First(&settledLedger).Error; err != nil {
		t.Fatalf("应有 settled 流水: %v", err)
	}
	// 结算流水记的是「本笔增量」，历史 pending 行只是被翻成 settled。
	// 若新行改写为累计 total，刚被翻转的历史金额会被 SUM 数两遍。
	if settledLedger.AmountQuota != 1000000 {
		t.Errorf("结算流水应只记本笔增量 1000000，实际 %d", settledLedger.AmountQuota)
	}
	var settledSum int64
	if err := model.DB.Model(&model.QydionQuotaTransaction{}).
		Where("user_id = ? AND type = ? AND operator = ?", inviter.Id, model.QydionTxTypeRebate, "settled").
		Select("COALESCE(SUM(amount_quota),0)").Scan(&settledSum).Error; err != nil {
		t.Fatalf("汇总 settled 流水失败: %v", err)
	}
	if settledSum != int64(after.AffQuota) {
		t.Errorf("settled 流水汇总必须等于 aff_quota：账本 %d，余额 %d", settledSum, after.AffQuota)
	}

	// 第二笔小额消费：返佣 100 quota < 门槛 → 记 pending，不动 aff_quota
	QydionOnConsumeRebate(int(invitee.Id), 1000, "mock-gpt")
	var after2 model.User
	model.DB.Select("aff_quota").Where("id = ?", inviter.Id).First(&after2)
	if after2.AffQuota != 2000000 {
		t.Errorf("小额未达门槛不应入账，aff_quota 应保持 2000000，实际 %d", after2.AffQuota)
	}
	var pendingCnt int64
	model.DB.Model(&model.QydionQuotaTransaction{}).
		Where("user_id = ? AND type = ? AND operator = ?", inviter.Id, model.QydionTxTypeRebate, "pending").
		Count(&pendingCnt)
	if pendingCnt != 1 {
		t.Errorf("小额返佣应记 1 条 pending，实际 %d 条", pendingCnt)
	}
}

// TestQydionRebateRequiresBudgetCampaign 没有预算活动时一分钱都不许返。
// 这是防套利的闸门本身，不是环境缺省的临时状态，必须作为不变量固定下来。
func TestQydionRebateRequiresBudgetCampaign(t *testing.T) {
	require.NoError(t, model.DB.AutoMigrate(&model.Option{}, &model.QydionCampaign{}, &model.QydionQuotaTransaction{}))
	require.NoError(t, model.DB.Create(&model.Option{Key: QydionKeyRebateEnabled, Value: "true"}).Error)
	QydionSettingsBustCache()
	t.Cleanup(func() {
		model.DB.Where("key = ?", QydionKeyRebateEnabled).Delete(&model.Option{})
		QydionSettingsBustCache()
	})

	now := time.Now().Unix()
	inviter := model.User{Username: "rebate-nobudget-A", AffCode: "aff-rebate-nobudget-A"}
	require.NoError(t, model.DB.Create(&inviter).Error)
	invitee := model.User{Username: "rebate-nobudget-B", InviterId: inviter.Id,
		CreatedAt: now - 25*3600, AffCode: "aff-rebate-nobudget-B"}
	require.NoError(t, model.DB.Create(&invitee).Error)

	// 无 invite_boost 活动；即便有，预算为 0 也不得结算
	QydionOnConsumeRebate(int(invitee.Id), 10000000, "mock-gpt")

	var after model.User
	require.NoError(t, model.DB.Select("aff_quota").Where("id = ?", inviter.Id).First(&after).Error)
	if after.AffQuota != 0 {
		t.Errorf("无预算活动不应返佣，aff_quota 期望 0，实际 %d", after.AffQuota)
	}
}

func TestQydionRebate24hWindow(t *testing.T) {
	now := time.Now().Unix()
	inviter := model.User{Username: "rebate-C", AffCode: "aff-rebate-C"}
	model.DB.Create(&inviter)
	fresh := model.User{Username: "rebate-D", InviterId: inviter.Id, CreatedAt: now, AffCode: "aff-rebate-D"} // 刚注册
	model.DB.Create(&fresh)
	QydionOnConsumeRebate(int(fresh.Id), 10000000, "mock-gpt")
	var after model.User
	model.DB.Select("aff_quota").Where("id = ?", inviter.Id).First(&after)
	if after.AffQuota != 0 {
		t.Errorf("注册 24h 内消费不应返佣，aff_quota 应为 0，实际 %d", after.AffQuota)
	}
}

// TestQydionCreditOnlineOrder M7：在线订单统一入账（pending→success 全链）
func TestQydionCreditOnlineOrder(t *testing.T) {
	if err := model.DB.AutoMigrate(&model.QydionQuotaTransaction{}, &model.QydionCampaign{}, &model.QydionBonusGrant{}, &model.QydionCoupon{}); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	user := model.User{Username: "pay-online-A", Quota: 0, AffCode: "aff-pay-online-A"}
	if err := model.DB.Create(&user).Error; err != nil {
		t.Fatalf("造用户失败: %v", err)
	}
	// 造券（面值 20，门槛 0）并挂单
	coupon := model.QydionCoupon{Code: "M7TEST20", Type: "amount", Value: 20, MinAmount: 0,
		Status: model.CouponStatusUnused, BatchId: 907, ExpiresAt: time.Now().Add(24 * time.Hour)}
	if err := model.DB.Create(&coupon).Error; err != nil {
		t.Fatalf("造券失败: %v", err)
	}
	// 实付 80 = 100 面额 - 20 券
	topUp := &model.TopUp{
		UserId: int(user.Id), Money: 80, TradeNo: "QYDM7TEST00001",
		PaymentMethod: "alipay_f2f", PaymentProvider: "alipay_f2f",
		CreateTime: time.Now().Unix(), Status: "pending", CouponId: coupon.Id,
	}
	if err := topUp.Insert(); err != nil {
		t.Fatalf("造订单失败: %v", err)
	}
	// 第一次入账
	already, err := QydionCreditOnlineOrder(topUp, "system", "test")
	if err != nil || already {
		t.Fatalf("入账失败: already=%v err=%v", already, err)
	}
	// ¥80 → 80/7.3*500000 = 5,479,452 quota
	var after model.User
	model.DB.Select("quota").Where("id = ?", user.Id).First(&after)
	expectF := 80.0 / 7.3 * 500000
	expect := int64(expectF + 0.5)
	if int64(after.Quota) != expect {
		t.Errorf("入账额度期望 %d，实际 %d", expect, after.Quota)
	}
	// 券应已核销
	var used model.QydionCoupon
	model.DB.Where("id = ?", coupon.Id).First(&used)
	if used.Status != model.CouponStatusUsed {
		t.Errorf("券应已核销，状态=%s", used.Status)
	}
	// 流水存在
	var cnt int64
	model.DB.Model(&model.QydionQuotaTransaction{}).
		Where("trade_no = ? AND type = ?", "QYDM7TEST00001", model.QydionTxTypeTopup).Count(&cnt)
	if cnt != 1 {
		t.Errorf("到账流水应 1 条，实际 %d", cnt)
	}
	// 幂等：重复入账返回 alreadyDone，不再加额度
	again := &model.TopUp{TradeNo: "QYDM7TEST00001", UserId: int(user.Id), Money: 80}
	already2, err2 := QydionCreditOnlineOrder(again, "system", "test")
	if err2 != nil || !already2 {
		t.Errorf("重复入账应 alreadyDone=true，实际 already=%v err=%v", already2, err2)
	}
	model.DB.Select("quota").Where("id = ?", user.Id).First(&after)
	if int64(after.Quota) != expect {
		t.Errorf("幂等失败：额度被重复入账，实际 %d", int64(after.Quota))
	}
}
