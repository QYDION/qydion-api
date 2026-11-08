package service

// QYDION 满送赠额过期回收回归（§8.7b / 全量安全审计 N08）。
// 缺陷形态：赠额记录没有随消费减少的余额，过期任务照原始金额扣，
// 已消费完的赠额到期会把新充值的本金再扣一遍。

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestQydionBonusRecoverableAllocation(t *testing.T) {
	cases := []struct {
		name    string
		history []model.QydionBonusGrant
		target  int64
		used    int64
		want    int64
		wantOK  bool
	}{
		{
			name: "全部消费完不再扣本金",
			history: []model.QydionBonusGrant{
				{Id: 1, Amount: 100000, ConsumedWatermark: 0},
			},
			target: 1, used: 100000, want: 0, wantOK: true,
		},
		{
			name: "分文未动则全额回收",
			history: []model.QydionBonusGrant{
				{Id: 1, Amount: 100000, ConsumedWatermark: 0},
			},
			target: 1, used: 0, want: 100000, wantOK: true,
		},
		{
			name: "部分消费只回收剩余额",
			history: []model.QydionBonusGrant{
				{Id: 1, Amount: 100000, ConsumedWatermark: 0},
			},
			target: 1, used: 40000, want: 60000, wantOK: true,
		},
		{
			name: "先发的批次先被消费，后发的批次不被前面的消费吃掉",
			history: []model.QydionBonusGrant{
				{Id: 1, Amount: 100000, ConsumedWatermark: 0},
				{Id: 2, Amount: 100000, ConsumedWatermark: 100000},
			},
			target: 2, used: 150000, want: 50000, wantOK: true,
		},
		{
			name: "消费早于第二批发放时，第二批仍是全额可回收",
			history: []model.QydionBonusGrant{
				{Id: 1, Amount: 100000, ConsumedWatermark: 0},
				{Id: 2, Amount: 100000, ConsumedWatermark: 100000},
			},
			target: 2, used: 100000, want: 100000, wantOK: true,
		},
		{
			name: "消费水位倒退时拒绝回收",
			history: []model.QydionBonusGrant{
				{Id: 1, Amount: 100000, ConsumedWatermark: 500000},
			},
			target: 1, used: 100, wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var target model.QydionBonusGrant
			for _, item := range tc.history {
				if item.Id == tc.target {
					target = item
				}
			}
			got, ok := qydionBonusRecoverable(tc.history, target, tc.used)
			assert.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				assert.Equal(t, tc.want, got)
			}
		})
	}
}

func qydionPrepareBonusExpiryTables(t *testing.T) {
	t.Helper()
	require.NoError(t, model.DB.AutoMigrate(
		&model.User{}, &model.TopUp{}, &model.Log{},
		&model.QydionBonusGrant{}, &model.QydionQuotaTransaction{},
	))
}

// consumeAsRelaySettlement 用系统自身的消费口径扣余额：累计消费计数与余额同时变动。
func consumeAsRelaySettlement(t *testing.T, userId int, quota int) {
	t.Helper()
	model.UpdateUserUsedQuotaAndRequestCount(userId, quota)
	require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", userId).
		Update("quota", gorm.Expr("quota - ?", quota)).Error)
}

func TestQydionExpiredBonusDoesNotReclaimSpentPrincipal(t *testing.T) {
	qydionPrepareBonusExpiryTables(t)
	oldPrice, oldUnit := operation_setting.Price, common.QuotaPerUnit
	operation_setting.Price, common.QuotaPerUnit = 7.3, 500000
	t.Cleanup(func() { operation_setting.Price, common.QuotaPerUnit = oldPrice, oldUnit })

	user := model.User{Username: "bonus-spent", Quota: 100000, AffCode: "aff-bonus-spent"}
	require.NoError(t, model.DB.Create(&user).Error)
	grant := model.QydionBonusGrant{
		UserId: user.Id, TradeNo: "QYD-BONUS-SPENT", Amount: 100000,
		ExpireAt: time.Now().Add(-time.Hour), CreatedTime: time.Now().Unix(),
	}
	require.NoError(t, model.DB.Create(&grant).Error)

	// 赠额全额消费
	consumeAsRelaySettlement(t, user.Id, 100000)
	// 之后新充值到账
	require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", user.Id).
		Update("quota", gorm.Expr("quota + ?", 500000)).Error)

	n, err := QydionRecoverExpiredBonus()
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	var after model.User
	require.NoError(t, model.DB.First(&after, user.Id).Error)
	assert.Equal(t, 500000, after.Quota, "已消费完的赠额到期不得再扣新充值的本金")

	var ledger model.QydionQuotaTransaction
	require.NoError(t, model.DB.Where("type = ?", model.QydionTxTypeBonusExpire).
		Order("id DESC").First(&ledger).Error)
	assert.Equal(t, int64(0), ledger.AmountQuota, "流水必须按实际回收额记录，而不是原始赠额")
}

func TestQydionExpiredBonusReclaimsUnspentBatchOnly(t *testing.T) {
	qydionPrepareBonusExpiryTables(t)

	user := model.User{Username: "bonus-unspent", Quota: 300000, AffCode: "aff-bonus-unspent"}
	require.NoError(t, model.DB.Create(&user).Error)
	// 批次一 10 万全额消费；批次二 10 万发放后才产生消费，未被前面的消费波及，全部过期。
	first := model.QydionBonusGrant{
		UserId: user.Id, TradeNo: "QYD-BONUS-1", Amount: 100000,
		ExpireAt: time.Now().Add(-2 * time.Hour), CreatedTime: time.Now().Unix(),
	}
	require.NoError(t, model.DB.Create(&first).Error)
	consumeAsRelaySettlement(t, user.Id, 100000)

	var watermarked model.User
	require.NoError(t, model.DB.First(&watermarked, user.Id).Error)
	second := model.QydionBonusGrant{
		UserId: user.Id, TradeNo: "QYD-BONUS-2", Amount: 100000,
		ExpireAt: time.Now().Add(-time.Hour), CreatedTime: time.Now().Unix(),
		ConsumedWatermark: int64(watermarked.UsedQuota),
	}
	require.NoError(t, model.DB.Create(&second).Error)

	n, err := QydionRecoverExpiredBonus()
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	var after model.User
	require.NoError(t, model.DB.First(&after, user.Id).Error)
	assert.Equal(t, 100000, after.Quota, "只有第二批改额的 10 万未被消费，可回收")

	var recovered model.QydionBonusGrant
	require.NoError(t, model.DB.First(&recovered, second.Id).Error)
	assert.True(t, recovered.Recovered)
}
