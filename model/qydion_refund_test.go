package model

// QYDION 退款记录数据层回归（交接报告 §E）：
// out_request_no 全局唯一（渠道幂等键）、累计封顶只计 pending+succeeded（failed 不占空间）。

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQydionRefundTableName(t *testing.T) {
	if (QydionRefund{}).TableName() != "qydion_refunds" {
		t.Errorf("退款表名应为 qydion_refunds，实际 %s", (QydionRefund{}).TableName())
	}
}

func TestQydionRefundOutRequestNoUnique(t *testing.T) {
	require.NoError(t, DB.AutoMigrate(&QydionRefund{}))
	require.NoError(t, DB.Where("1=1").Delete(&QydionRefund{}).Error)

	first := &QydionRefund{TradeNo: "QYD-UQ-1", UserId: 1, Channel: QydionRefundChannelAlipay,
		OutRequestNo: "QYD-UQ-1-R1", RefundCents: 100, RefundQuota: 100, Status: RefundStatusPending}
	require.NoError(t, DB.Create(first).Error)

	dup := &QydionRefund{TradeNo: "QYD-UQ-1", UserId: 1, Channel: QydionRefundChannelAlipay,
		OutRequestNo: "QYD-UQ-1-R1", RefundCents: 100, RefundQuota: 100, Status: RefundStatusPending}
	require.Error(t, DB.Create(dup).Error, "out_request_no 必须全局唯一（重复退款单号会绕过累计封顶）")
}

// TestQydionRefundActiveSumsSkipsFailed 累计封顶口径：pending+succeeded 计入，failed 不计。
// 渠道失败时钱没有退出去、额度已回补，failed 若占空间，用户将永远退不满实付金额。
func TestQydionRefundActiveSumsSkipsFailed(t *testing.T) {
	require.NoError(t, DB.AutoMigrate(&QydionRefund{}))
	require.NoError(t, DB.Where("1=1").Delete(&QydionRefund{}).Error)

	rows := []QydionRefund{
		{TradeNo: "QYD-SUM-1", UserId: 7, Channel: QydionRefundChannelAlipay, OutRequestNo: "S1",
			RefundCents: 2000, RefundQuota: 137000, Status: RefundStatusSucceeded},
		{TradeNo: "QYD-SUM-1", UserId: 7, Channel: QydionRefundChannelAlipay, OutRequestNo: "S2",
			RefundCents: 1000, RefundQuota: 68500, Status: RefundStatusPending},
		{TradeNo: "QYD-SUM-1", UserId: 7, Channel: QydionRefundChannelAlipay, OutRequestNo: "S3",
			RefundCents: 9900, RefundQuota: 680000, Status: RefundStatusFailed},
		{TradeNo: "QYD-SUM-2", UserId: 8, Channel: QydionRefundChannelWechat, OutRequestNo: "S4",
			RefundCents: 500, RefundQuota: 34200, Status: RefundStatusPending},
	}
	for i := range rows {
		require.NoError(t, DB.Create(&rows[i]).Error)
	}

	cents, quota, err := QydionRefundActiveSums(DB, "QYD-SUM-1")
	require.NoError(t, err)
	assert.Equal(t, int64(3000), cents, "failed 不得计入累计退款金额")
	assert.Equal(t, int64(205500), quota, "failed 不得计入累计扣减额度")

	cents, quota, err = QydionRefundActiveSums(DB, "QYD-NONE")
	require.NoError(t, err)
	assert.Zero(t, cents)
	assert.Zero(t, quota, "无退款记录时累计必须为 0 而非 NULL 污染")
}
