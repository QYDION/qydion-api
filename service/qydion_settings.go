package service

// QYDION 运营设置读取（options 表，30 秒进程内缓存）。
// 原生选项体系是「每个 key 一个包变量 + SyncOptions」，QYDION 的扩展配置走通用键值读取，
// 不侵入原生 option.go 的 update 分发（§7.2 独立落点）。

import (
	"strconv"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/model"
)

const qydionSettingCacheTTL = 30 * time.Second

var (
	qydionSettingMu    sync.RWMutex
	qydionSettingCache = map[string]qydionSettingEntry{}
)

type qydionSettingEntry struct {
	value   string
	expires time.Time
}

// QydionSettingKey 统一的选项键前缀约定。
const (
	QydionKeyRebateEnabled      = "QydionRebateEnabled"      // "true"/"false"
	QydionKeyRebateFirstRate    = "QydionRebateFirstRate"    // 一级返佣百分比，默认 10
	QydionKeyRebateSecondRate   = "QydionRebateSecondRate"   // 二级返佣百分比，默认 0
	QydionKeyRebateMinSettleCNY = "QydionRebateMinSettleCNY" // 最低结算额度（元），默认 10
	QydionKeyMaxTokensLimit     = "QydionMaxTokensLimit"     // 单请求 max_tokens 钳制，默认 8192，0=关闭
	QydionKeyDailySpendAlertCNY = "QydionDailySpendAlertCNY" // 单用户日消费告警阈值（元），默认 500
	QydionKeyAutoDisableOnDebt  = "QydionAutoDisableOnDebt"  // 余额<0 自动禁用令牌，默认 "true"
	QydionKeyCorpAccountName    = "QydionCorpAccountName"    // 对公户名
	QydionKeyCorpAccountNo      = "QydionCorpAccountNo"      // 对公账号
	QydionKeyCorpBankName       = "QydionCorpBankName"       // 开户行
	QydionKeyCorpNote           = "QydionCorpNote"           // 对公备注
	QydionKeyEstimatedUsageWarn = "QydionEstimatedUsageWarn" // 估算计费占比告警阈值%（§8.5a-3），默认 1
	QydionKeyReconDiffTokens    = "QydionReconDiffTokens"    // 对账 tokens 差异阈值%，默认 2
	QydionKeyReconDiffAmount    = "QydionReconDiffAmount"    // 对账金额差异阈值%，默认 2
	QydionKeyCorporateEnabled   = "QydionCorporateEnabled"   // 对公转账下单开关，默认 false（需显式开启）
)

func qydionSettingRaw(key string) (string, bool) {
	qydionSettingMu.RLock()
	if e, ok := qydionSettingCache[key]; ok && time.Now().Before(e.expires) {
		qydionSettingMu.RUnlock()
		return e.value, true
	}
	qydionSettingMu.RUnlock()

	// DB 未初始化（极早期启动/纯逻辑单测）：按未配置处理，返回默认值
	if model.DB == nil {
		return "", false
	}
	var opt model.Option
	err := model.DB.Where("key = ?", key).First(&opt).Error
	value := ""
	if err == nil {
		value = opt.Value
	}
	qydionSettingMu.Lock()
	qydionSettingCache[key] = qydionSettingEntry{value: value, expires: time.Now().Add(qydionSettingCacheTTL)}
	qydionSettingMu.Unlock()
	return value, err == nil
}

func QydionSettingStr(key, def string) string {
	if v, ok := qydionSettingRaw(key); ok && v != "" {
		return v
	}
	return def
}

func QydionSettingBool(key string, def bool) bool {
	v := QydionSettingStr(key, "")
	if v == "" {
		return def
	}
	return v == "true" || v == "1"
}

func QydionSettingInt(key string, def int) int {
	v := QydionSettingStr(key, "")
	if v == "" {
		return def
	}
	if n, err := strconv.Atoi(v); err == nil {
		return n
	}
	return def
}

func QydionSettingFloat(key string, def float64) float64 {
	v := QydionSettingStr(key, "")
	if v == "" {
		return def
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return f
	}
	return def
}

// QydionSettingsBustCache 管理端保存支付参数后立即失效缓存（否则最长 30s 滞后）。
func QydionSettingsBustCache() {
	qydionSettingMu.Lock()
	qydionSettingCache = map[string]qydionSettingEntry{}
	qydionSettingMu.Unlock()
}

// M7 支付渠道选项键（后台「支付设置」白名单；密钥类 GET 掩码返回）
const (
	QydionKeyAlipayAppId      = "QydionAlipayAppId"
	QydionKeyAlipayPrivateKey = "QydionAlipayPrivateKey"
	QydionKeyAlipayPublicKey  = "QydionAlipayPublicKey"
	QydionKeyAlipayGateway    = "QydionAlipayGateway"

	QydionKeyWxpayAppId      = "QydionWxpayAppId"
	QydionKeyWxpayMchId      = "QydionWxpayMchId"
	QydionKeyWxpayMchSerial  = "QydionWxpayMchSerial"
	QydionKeyWxpayPrivateKey = "QydionWxpayPrivateKey"
	QydionKeyWxpayApiV3Key   = "QydionWxpayApiV3Key"

	QydionKeyStripeSecretKey     = "QydionStripeSecretKey"
	QydionKeyStripeWebhookSecret = "QydionStripeWebhookSecret"
	QydionKeyStripeCurrency      = "QydionStripeCurrency"
	QydionKeyStripeSuccessUrl    = "QydionStripeSuccessUrl"
	QydionKeyStripeCancelUrl     = "QydionStripeCancelUrl"
)

// QydionPaymentSettingKeys 支付设置可写白名单（controller 校验用）
var QydionPaymentSettingKeys = map[string]bool{
	QydionKeyAlipayAppId: true, QydionKeyAlipayPrivateKey: true,
	QydionKeyAlipayPublicKey: true, QydionKeyAlipayGateway: true,
	QydionKeyWxpayAppId: true, QydionKeyWxpayMchId: true,
	QydionKeyWxpayMchSerial: true, QydionKeyWxpayPrivateKey: true,
	QydionKeyWxpayApiV3Key:   true,
	QydionKeyStripeSecretKey: true, QydionKeyStripeWebhookSecret: true,
	QydionKeyStripeCurrency: true, QydionKeyStripeSuccessUrl: true,
	QydionKeyStripeCancelUrl:  true,
	QydionKeyCorporateEnabled: true,
}

// QydionPaymentSecretKeys 掩码类（读取时只回尾 4 位）
var QydionPaymentSecretKeys = map[string]bool{
	QydionKeyAlipayPrivateKey: true, QydionKeyAlipayPublicKey: true,
	QydionKeyWxpayPrivateKey: true, QydionKeyWxpayApiV3Key: true,
	QydionKeyStripeSecretKey: true, QydionKeyStripeWebhookSecret: true,
}
