package service

// QYDION 管理员告警统一出口（后台缺口⑦）。
// 四个运营触点（估算占比超阈 / 单用户日消费超阈 / 透支禁令牌管理员抄送 / 对账差异落表超阈）
// 统一走这里追加邮件通知；各触点原有的落表与 SysLog 行为保持不变。
//
// 降级语义（绝不 panic、绝不阻塞主流程）：
//   - 收件人（QydionAlertEmail）未配置 → 只落日志并打 [alert:email-unconfigured] 标记；
//   - SMTP 未配置 / 发送失败 → 降级日志 [alert:email-error]，错误不外抛；
//   - 防风暴：同一 kind 默认 6 小时最多发一封（冷却去重，见 qydionAlertCooldownWindow）。
//
// 并发注意：本函数内部同步调 common.SendEmail；跑在请求后置协程池里的调用方
// （如 QydionAfterConsumeGuard）必须包 gopool.Go 异步调用，理由见 qydion_guards.go 的注释——
// SMTP 慢或不通时同步发信会把 worker 连同连接一起占住，放大成全站变慢。
// cron / 系统任务场景（对账、估算占比）本来就在后台任务协程里，可直接同步调用。

import (
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
)

// qydionAlertCooldownWindow 同一 kind 告警的最小间隔（默认 6 小时一封，防风暴）。
const qydionAlertCooldownWindow = 6 * time.Hour

var (
	qydionAlertMu   sync.Mutex
	qydionAlertLast = map[string]time.Time{} // kind -> 上次放行时刻
)

// tryAcquireAlertSlot 判定该 kind 是否已过冷却窗口：放行则记录当前时刻并返回 true。
// 独立成纯函数便于单测注入时刻。
func tryAcquireAlertSlot(kind string, now time.Time) bool {
	qydionAlertMu.Lock()
	defer qydionAlertMu.Unlock()
	if last, ok := qydionAlertLast[kind]; ok && now.Sub(last) < qydionAlertCooldownWindow {
		return false
	}
	qydionAlertLast[kind] = now
	return true
}

// qydionResetAlertCooldownForTest 清空冷却表（仅测试使用）。
func qydionResetAlertCooldownForTest() {
	qydionAlertMu.Lock()
	qydionAlertLast = map[string]time.Time{}
	qydionAlertMu.Unlock()
}

// sendAdminAlert 统一告警出口，返回实际执行的动作：
//
//	"log-only"    收件人未配置，降级日志（打 [alert:email-unconfigured] 标记）
//	"cooldown"    该 kind 处于冷却窗口内，吞掉本次邮件
//	"email-error" SMTP 未配置或发送失败，降级日志（打 [alert:email-error] 标记）
//	"sent"        邮件已提交发送
//
// 日志降级路径不占冷却位：保证每次告警事件都有审计痕迹，冷却只防邮件风暴。
func sendAdminAlert(kind, subject, body string) string {
	defer func() {
		if r := recover(); r != nil {
			common.SysError(common.LogText("QYDION 告警出口 panic（已吞掉，不影响主流程）: %v", r))
		}
	}()
	to := QydionSettingStr(QydionKeyAlertEmail, "")
	if to == "" {
		common.SysLog(common.LogText("[alert:email-unconfigured] QYDION 告警 kind=%s subject=%s（运营键 %s 未配置，请到后台设置管理员收件邮箱）：%s", kind, subject, QydionKeyAlertEmail, body))
		return "log-only"
	}
	if !tryAcquireAlertSlot(kind, time.Now()) {
		common.SysLog(common.LogText("QYDION 告警 kind=%s 在冷却窗口内被去重（%s 一封）", kind, qydionAlertCooldownWindow))
		return "cooldown"
	}
	if err := common.SendEmail(subject, to, body); err != nil {
		common.SysError(common.LogText("[alert:email-error] QYDION 告警 kind=%s 发送失败（降级日志）：%v：%s", kind, err, body))
		return "email-error"
	}
	return "sent"
}

// QydionSendAdminAlert 导出入口：controller 等外部包的告警触点统一从这里走。
func QydionSendAdminAlert(kind, subject, body string) string {
	return sendAdminAlert(kind, subject, body)
}
