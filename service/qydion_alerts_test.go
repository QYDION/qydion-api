package service

// 后台缺口⑦ 告警出口单测：冷却去重 + 三条降级路径（收件人未配置 / SMTP 未配置）。
// 全程零真实外发：SMTP 未配置时 common.SendEmail 直接返回错误，不存在真实投递通道。

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setQydionSettingForTest 注入一条缓存运营键（不依赖 DB），测试结束恢复。
func setQydionSettingForTest(t *testing.T, key, value string) {
	t.Helper()
	qydionSettingMu.Lock()
	prev, hadPrev := qydionSettingCache[key]
	qydionSettingCache[key] = qydionSettingEntry{value: value, expires: time.Now().Add(time.Hour)}
	qydionSettingMu.Unlock()
	t.Cleanup(func() {
		qydionSettingMu.Lock()
		if hadPrev {
			qydionSettingCache[key] = prev
		} else {
			delete(qydionSettingCache, key)
		}
		qydionSettingMu.Unlock()
	})
}

func TestQydionAlertCooldown(t *testing.T) {
	qydionResetAlertCooldownForTest()
	t.Cleanup(qydionResetAlertCooldownForTest)

	now := time.Now()
	require.True(t, tryAcquireAlertSlot("kind-a", now), "首次应放行")
	assert.False(t, tryAcquireAlertSlot("kind-a", now.Add(time.Hour)), "6h 窗口内同 kind 应被冷却")
	assert.False(t, tryAcquireAlertSlot("kind-a", now.Add(qydionAlertCooldownWindow-time.Second)), "恰好卡窗内仍应冷却")
	assert.True(t, tryAcquireAlertSlot("kind-a", now.Add(qydionAlertCooldownWindow+time.Second)), "超窗后应再次放行")
	assert.True(t, tryAcquireAlertSlot("kind-b", now), "不同 kind 互不影响")
}

func TestSendAdminAlertEmailUnconfigured(t *testing.T) {
	qydionResetAlertCooldownForTest()
	t.Cleanup(qydionResetAlertCooldownForTest)

	// 收件人键未配置（缓存清空、无 DB 兜底默认空串）→ 只落日志打 [alert:email-unconfigured]
	assert.Equal(t, "log-only", sendAdminAlert("unconfigured-kind", "[QYDION] 测试", "收件人未配置时应降级日志"))
}

func TestSendAdminAlertSMTPUnconfigured(t *testing.T) {
	qydionResetAlertCooldownForTest()
	t.Cleanup(qydionResetAlertCooldownForTest)

	setQydionSettingForTest(t, QydionKeyAlertEmail, "admin@example.com")
	prevServer, prevAccount := common.SMTPServer, common.SMTPAccount
	common.SMTPServer, common.SMTPAccount = "", "" // SMTP 未配置
	t.Cleanup(func() { common.SMTPServer, common.SMTPAccount = prevServer, prevAccount })

	// 收件人已配置但 SMTP 未配置 → 降级日志 [alert:email-error]，不 panic、不外发
	assert.Equal(t, "email-error", sendAdminAlert("smtp-unconfig-kind", "[QYDION] 测试", "SMTP 未配置时应降级日志"))
}

func TestSendAdminAlertCooldownGateBeforeSend(t *testing.T) {
	qydionResetAlertCooldownForTest()
	t.Cleanup(qydionResetAlertCooldownForTest)

	setQydionSettingForTest(t, QydionKeyAlertEmail, "admin@example.com")
	prevServer, prevAccount := common.SMTPServer, common.SMTPAccount
	common.SMTPServer, common.SMTPAccount = "", ""
	t.Cleanup(func() { common.SMTPServer, common.SMTPAccount = prevServer, prevAccount })

	// 首次触发会走到发送（SMTP 未配置→email-error），但冷却位已被占用；
	// 第二次同 kind 直接在冷却门处拦截，不再触碰 SMTP。
	assert.Equal(t, "email-error", sendAdminAlert("gate-kind", "[QYDION] 测试", "第一次"))
	assert.Equal(t, "cooldown", sendAdminAlert("gate-kind", "[QYDION] 测试", "第二次应被冷却吞掉"))
}
