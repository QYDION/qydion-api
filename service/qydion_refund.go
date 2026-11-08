package service

// QYDION 退款业务闭环（交接报告 §E）：管理员发起 → 预校验+预扣额度（事务A）→
// 调渠道退款 API（事务外，由 controller 组合 payment.Refunder）→ 定稿（事务B）。
//
// 为什么「先扣额度、后调渠道」而不是反过来：
//   - 先渠道后扣：渠道已把钱退给用户，此时若扣额度失败（用户恰好消费到不足），
//     就是「钱退了、额度没收回」的真金白银漏出，且无自动补偿手段。
//   - 先扣后渠道：渠道失败则原额回补。回补失败的极端情况（余额封顶等）只会让
//     用户少拿额度，可由管理员手动补，方向是平台安全的。
//   - 「扣额度与写退款记录在同一事务」由事务A保证（预扣+pending 记录同事务）。
//
// 进程在事务A与事务B之间中断会留下 pending 退款单：额度已扣、渠道结果未知。
// 管理员经 /api/qydion/refund/close 对 pending 单执行失败回补（QydionFailRefund），
// 渠道侧是否真的退了款以渠道对账为准——宁可保守留痕，不可假装成功。
//
// N08（赠额回收正确性）交互分析——本实现只动 users.quota：
//   1. 不改 users.used_quota：消费水位是 qydionBonusRecoverable
//     （qydion_guards.go:187）归属消费的唯一依据，退款不是消费，动了它就会把
//     退款金额错记成「消费」，让过期赠额被少收/多收。
//   2. 不改 qydion_bonus_grants（水位/剩余额全部不碰）：批次归属关系原样保留。
//   3. 退款扣减可能落到「账面含未消费赠额」的余额上（消费顺序是先抵赠额），
//     赠额到期回收会照常按未消费额回收——每一步扣减（退款 WHERE quota >= ?、
//     回收 deduct=min(recoverable, quota)）都以当时可用余额为上界，
//     总扣减 ≤ 总发放的不变量成立，余额不会被推成负数。
//   4. 退款额度上限取「订单 QuotaSnapshot」而非余额：退款不会把别的订单/赠额
//     一起退掉，只收回本单按快照单价折算的那部分。

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// QydionRefundChannels 允许在线退款的订单渠道 → 退款渠道映射（本网关 qydion 在线支付体系）。
// 生产路径只读：corporate 是线下人工流（退款走线下转账），stripe 未实现退款 API，
// 二者都必须先落实 payment.Refunder 能力才允许加入，否则 controller 的能力断言会兜底拒绝。
// 测试可临时扩充映射并用后删除（与 payment.Register 的注册制同一模式）。
var QydionRefundChannels = map[string]string{
	"alipay_f2f": model.QydionRefundChannelAlipay,
	"wx_native":  model.QydionRefundChannelWechat,
}

// QydionRefundQuotaForCents 按订单快照单价折算退款扣减额度（纯函数）。
// 单价 = QuotaSnapshot / 实付总额（分）；向下取整：多次部分退款的累计扣减额度
// 因此恒 ≤ QuotaSnapshot（各 floor 之和 ≤ 整体 floor），不会因逐笔进位把快照扣穿。
// 尾差（每笔最多差 1 个额度单位，1 单位 = 1/QuotaPerUnit 美元）留在平台侧。
func QydionRefundQuotaForCents(orderMoneyCents, refundCents, quotaSnapshot int64) (int64, error) {
	if orderMoneyCents <= 0 {
		return 0, errors.New("订单实付金额非法")
	}
	if refundCents <= 0 {
		return 0, errors.New("退款金额必须大于 0")
	}
	if refundCents > orderMoneyCents {
		return 0, errors.New("退款金额超过订单实付金额")
	}
	if quotaSnapshot <= 0 {
		return 0, errors.New("订单缺少额度快照，无法折算退款额度")
	}
	quota := decimal.NewFromInt(refundCents).
		Mul(decimal.NewFromInt(quotaSnapshot)).
		Div(decimal.NewFromInt(orderMoneyCents)).
		Floor()
	q := quota.IntPart()
	if q <= 0 || q > common.MaxWalletQuota {
		return 0, errors.New("退款折算额度非法")
	}
	return q, nil
}

// QydionPrepareRefund 事务A：校验订单与累计封顶 → 未消耗约束下预扣额度 → 写 pending 退款单。
// 返回的记录带状态 pending，调用方接下来调渠道；成功走 QydionCompleteRefund，
// 失败走 QydionFailRefund（原额回补）。
func QydionPrepareRefund(tradeNo string, refundCents int64, operator string) (*model.QydionRefund, error) {
	if tradeNo == "" {
		return nil, errors.New("订单号缺失")
	}
	if refundCents <= 0 {
		return nil, errors.New("退款金额必须大于 0")
	}
	record := &model.QydionRefund{}
	err := model.DB.Transaction(func(tx *gorm.DB) error {
		// 订单行锁：并发退款在「累计封顶」检查处串行化（MySQL/PG 为 FOR UPDATE；
		// SQLite 单写者 + 单连接池天然串行）
		locked := &model.TopUp{}
		if err := qydionLockOrderTx(tx, tradeNo).First(locked).Error; err != nil {
			return errors.New("订单不存在")
		}
		if locked.PaymentProvider == "corporate" {
			return errors.New("对公转账订单请走线下人工退款，不能经渠道退款 API 处理")
		}
		channel, ok := QydionRefundChannels[locked.PaymentProvider]
		if !ok {
			return errors.New("该订单渠道不支持在线退款")
		}
		if locked.Status != common.TopUpStatusSuccess {
			return fmt.Errorf("订单状态为 %s，仅已支付订单可退款", locked.Status)
		}
		if locked.QuotaSnapshot <= 0 {
			return errors.New("订单缺少额度快照，无法按下单时单价折算退款额度")
		}
		totalCents := int64(math.Round(locked.Money * 100))
		if totalCents <= 0 {
			return errors.New("订单实付金额非法，无法退款")
		}
		// 累计封顶（pending+succeeded；failed 不占空间）：
		// 单次金额 ≤ 订单实付 − 已累计退款，分级精确比较，无浮点误差。
		sumCents, sumQuota, err := model.QydionRefundActiveSums(tx, tradeNo)
		if err != nil {
			return err
		}
		if refundCents > totalCents-sumCents {
			return fmt.Errorf("退款金额超过订单可退余额（实付 %.2f 元，已累计退款 %.2f 元）",
				float64(totalCents)/100, float64(sumCents)/100)
		}
		refundQuota, err := QydionRefundQuotaForCents(totalCents, refundCents, locked.QuotaSnapshot)
		if err != nil {
			return err
		}
		// 折算额度也不得扣穿快照（floor 已保证，这里兜底防御）
		if refundQuota > locked.QuotaSnapshot-sumQuota {
			return errors.New("退款折算额度超过订单快照剩余可扣额度")
		}
		// 未消耗约束：用户当前剩余额度 ≥ 折算额度才放行。
		// 行锁读取给出友好报错；随后的条件更新（WHERE quota >= ?）在同事务内
		// 原子执行，锁失效的方言下也不会把余额扣成负数。
		var user model.User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id", "quota").Where("id = ?", locked.UserId).First(&user).Error; err != nil {
			return errors.New("订单用户不存在")
		}
		if int64(user.Quota) < refundQuota {
			return fmt.Errorf("用户当前剩余额度不足折算额度（剩余 %d，需 %d）：已消耗部分不可退款", user.Quota, refundQuota)
		}
		result := tx.Model(&model.User{}).
			Where("id = ? AND quota >= ?", locked.UserId, refundQuota).
			Update("quota", gorm.Expr("quota - ?", refundQuota))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("用户余额在退款校验期间发生变化，请重试")
		}
		record = &model.QydionRefund{
			TradeNo:      tradeNo,
			UserId:       locked.UserId,
			Channel:      channel,
			OutRequestNo: qydionRefundRequestNo(tradeNo),
			RefundCents:  refundCents,
			RefundQuota:  refundQuota,
			Status:       model.RefundStatusPending,
			Operator:     operator,
			CreatedTime:  time.Now().Unix(),
			UpdatedTime:  time.Now().Unix(),
		}
		if err := tx.Create(record).Error; err != nil {
			return fmt.Errorf("退款单创建失败：%w", err)
		}
		common.SysLog(common.LogText("QYDION 退款预扣完成 trade_no=%s refund=%d cents=%d quota=%d operator=%s",
			tradeNo, record.Id, refundCents, refundQuota, operator))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return record, nil
}

// QydionCompleteRefund 事务B（成功）：pending → succeeded，写渠道凭证号，并落一条
// 负向额度流水（退款扣回，BalanceAfter 为定稿时点余额快照）。
func QydionCompleteRefund(refundId int64, credential, channelStatus string) error {
	return model.DB.Transaction(func(tx *gorm.DB) error {
		refund := &model.QydionRefund{}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(refund, refundId).Error; err != nil {
			return model.ErrQydionRefundNotFound
		}
		if refund.Status != model.RefundStatusPending {
			return fmt.Errorf("退款单 #%d 状态为 %s，不在待处理状态，不能标记成功", refund.Id, refund.Status)
		}
		note := "渠道退款受理"
		if channelStatus != "" && channelStatus != "success" {
			note = fmt.Sprintf("渠道退款受理（渠道状态 %s）", channelStatus)
		}
		result := tx.Model(&model.QydionRefund{}).Where("id = ? AND status = ?", refund.Id, model.RefundStatusPending).
			Updates(map[string]any{
				"status":             model.RefundStatusSucceeded,
				"channel_credential": credential,
				"note":               note,
				"updated_time":       time.Now().Unix(),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("退款单 #%d 已被其他操作定稿", refund.Id)
		}
		var user model.User
		if err := tx.Select("quota").Where("id = ?", refund.UserId).First(&user).Error; err != nil {
			return err
		}
		ledger := model.QydionQuotaTransaction{
			UserId: refund.UserId, TradeNo: refund.TradeNo, Type: model.QydionTxTypeRefund,
			AmountQuota: -refund.RefundQuota, Money: float64(refund.RefundCents) / 100,
			BalanceAfter: int64(user.Quota), Operator: refund.Operator,
			Note:        fmt.Sprintf("退款扣回（退款单号 %s，渠道凭证 %s）", refund.OutRequestNo, credential),
			RefId:       int(refund.Id),
			CreatedTime: time.Now().Unix(),
		}
		if err := tx.Create(&ledger).Error; err != nil {
			return err
		}
		common.SysLog(common.LogText("QYDION 退款成功 trade_no=%s refund=%d cents=%d quota=%d credential=%s operator=%s",
			refund.TradeNo, refund.Id, refund.RefundCents, refund.RefundQuota, credential, refund.Operator))
		return nil
	})
}

// QydionFailRefund 事务B（失败）：pending → failed，并在同一事务内按预扣额原额回补
// （复用通用入账原语 qydionCreditQuotaTx，含钱包封顶守卫；不改赠额批次，见文件头 N08 分析）。
// 回补失败时整体回滚（记录保持 pending），管理员可重试——绝不留下「failed 但额度没回来」的账。
func QydionFailRefund(refundId int64, reason string) error {
	err := model.DB.Transaction(func(tx *gorm.DB) error {
		refund := &model.QydionRefund{}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(refund, refundId).Error; err != nil {
			return model.ErrQydionRefundNotFound
		}
		if refund.Status != model.RefundStatusPending {
			return fmt.Errorf("退款单 #%d 状态为 %s，不在待处理状态，不能标记失败", refund.Id, refund.Status)
		}
		// 先回补、后定稿：回补出错即整体回滚，退款单留在 pending 等待重试
		if err := qydionCreditQuotaTx(tx, refund.UserId, refund.RefundQuota); err != nil {
			return fmt.Errorf("退款失败回补额度未成功（refund=%d user=%d quota=%d），需人工核对：%w",
				refund.Id, refund.UserId, refund.RefundQuota, err)
		}
		note := "渠道退款失败"
		if reason != "" {
			runes := []rune(reason)
			if len(runes) > 100 {
				runes = runes[:100]
			}
			note = note + "：" + string(runes)
		}
		result := tx.Model(&model.QydionRefund{}).Where("id = ? AND status = ?", refund.Id, model.RefundStatusPending).
			Updates(map[string]any{
				"status":       model.RefundStatusFailed,
				"note":         note,
				"updated_time": time.Now().Unix(),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("退款单 #%d 已被其他操作定稿", refund.Id)
		}
		common.SysLog(common.LogText("QYDION 退款失败已回补 trade_no=%s refund=%d cents=%d quota=%d reason=%s",
			refund.TradeNo, refund.Id, refund.RefundCents, refund.RefundQuota, note))
		return nil
	})
	return err
}

// qydionRefundRequestNo 生成退款单号：原单号 + "-R" + 秒级时间 + 8 位十六进制随机。
// out_request_no 全局唯一索引兜底；crypto/rand 随机源与券码/对公单号同一纪律（审计 P3）。
func qydionRefundRequestNo(tradeNo string) string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		// 随机源失效是系统级故障：退化为纳秒时间戳仍保证进程内唯一，唯一索引兜底跨进程
		return fmt.Sprintf("%s-R%dN", tradeNo, time.Now().UnixNano())
	}
	return fmt.Sprintf("%s-R%d%s", tradeNo, time.Now().Unix(), hex.EncodeToString(b))
}
