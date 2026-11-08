package service

// QYDION 运营设置白名单（后台缺口③）：16 个运营键经既有支付设置端点读写。
//
// 物理落点说明：service/qydion_settings.go 有其他工作线的未提交改动（告警邮箱键），
// 为避免裹入他人改动，运营键白名单在此独立登记，init 时并入 QydionPaymentSettingKeys，
// 复用 controller/qydion_pay.go 的 GET/POST /api/qydion/payment/settings 读写：
// 校验（TrimSpace / 8192 长度 / UpdateOption）与保存后 QydionSettingsBustCache 即时生效均保持原样，只加键。
//
// 键定义见 qydion_settings.go 的运营键常量；各键类型/默认值：
//   - 布尔：RebateEnabled(默认 false)、AutoDisableOnDebt(默认 true)、CorporateEnabled(默认 false)
//   - 数值：RebateFirstRate(10, 0-100%)、RebateSecondRate(0, 0-100%)、RebateMinSettleCNY(10, 元)、
//     MaxTokensLimit(8192, 0=关闭)、DailySpendAlertCNY(500, 元)、EstimatedUsageWarn(1, 0-100%)、
//     ReconDiffTokens(2, 0-100%)、ReconDiffAmount(2, 0-100%)
//   - 文本：CorpAccountName/CorpAccountNo/CorpBankName/CorpNote（对公收款信息，默认空）、
//     AlertEmail（管理员告警收件邮箱，默认空=只落日志并打 [alert:email-unconfigured] 标记）

var qydionOperationSettingKeys = map[string]bool{
	QydionKeyRebateEnabled:      true,
	QydionKeyRebateFirstRate:    true,
	QydionKeyRebateSecondRate:   true,
	QydionKeyRebateMinSettleCNY: true,
	QydionKeyMaxTokensLimit:     true,
	QydionKeyDailySpendAlertCNY: true,
	QydionKeyAutoDisableOnDebt:  true,
	QydionKeyCorpAccountName:    true,
	QydionKeyCorpAccountNo:      true,
	QydionKeyCorpBankName:       true,
	QydionKeyCorpNote:           true,
	QydionKeyEstimatedUsageWarn: true,
	QydionKeyReconDiffTokens:    true,
	QydionKeyReconDiffAmount:    true,
	QydionKeyCorporateEnabled:   true, // 已在支付白名单（qydion_settings.go），重复登记幂等
	QydionKeyAlertEmail:         true,
}

func init() {
	for key := range qydionOperationSettingKeys {
		QydionPaymentSettingKeys[key] = true
	}
}
