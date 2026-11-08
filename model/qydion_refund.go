package model

// QYDION 退款记录（交接报告 §E「退款管理后端闭环」）
// 流程：管理员审核发起 → 调渠道退款 API（支付宝 alipay.trade.refund / 微信 v3 退款）→
// 按订单 QuotaSnapshot 单价折算扣减额度 → 同一事务写退款记录与扣额度。
// 原则不变（model/qydion.go §7.2-4）：新表新 model，Qydion 前缀，经 AutoMigrate 建表。

import (
	"errors"

	"gorm.io/gorm"
)

const (
	RefundStatusPending   = "pending"   // 已预扣额度、渠道调用中（进程中断会停留在此态，管理员可 close）
	RefundStatusSucceeded = "succeeded" // 渠道受理成功，额度已扣回
	RefundStatusFailed    = "failed"    // 渠道拒绝/失败，额度已原额回补，不参与累计封顶

	QydionRefundChannelAlipay = "alipay" // 对应订单渠道 alipay_f2f
	QydionRefundChannelWechat = "wechat" // 对应订单渠道 wx_native
)

// QydionTxTypeRefund 额度流水类型：退款扣回（负向）。
// 不复用 topup/admin 等既有类型，审计查询按类型即可单独拉出全部退款。
const QydionTxTypeRefund = "refund"

// QydionRefund 退款单。金额以「分」为最小记账单位（int64），杜绝浮点累加漂移；
// 折算额度按订单 QuotaSnapshot 单价折算（N07 语义：以下单时快照为准，不按当前单价重算）。
type QydionRefund struct {
	Id                int64  `json:"id"`
	TradeNo           string `json:"trade_no" gorm:"size:64;index"`                 // 关联本网关订单号（TopUp.TradeNo）
	UserId            int    `json:"user_id" gorm:"index"`                          // 订单归属用户
	Channel           string `json:"channel" gorm:"size:16"`                        // alipay / wechat
	OutRequestNo      string `json:"out_request_no" gorm:"uniqueIndex;size:64"`     // 退款单号（渠道 out_request_no/out_refund_no，全局唯一）
	RefundCents       int64  `json:"refund_cents"`                                  // 本次退款金额（分）
	RefundQuota       int64  `json:"refund_quota"`                                  // 本次扣减额度（按订单快照单价折算）
	Status            string `json:"status" gorm:"size:16;index;default:pending"`   // pending/succeeded/failed
	ChannelCredential string `json:"channel_credential" gorm:"size:128;default:''"` // 渠道退款凭证号（支付宝交易号/微信 refund_id）
	Operator          string `json:"operator" gorm:"size:64"`                       // 操作管理员
	Note              string `json:"note" gorm:"size:255"`                          // 渠道状态/失败原因
	CreatedTime       int64  `json:"created_time" gorm:"bigint"`
	UpdatedTime       int64  `json:"updated_time" gorm:"bigint"`
}

func (QydionRefund) TableName() string { return "qydion_refunds" }

// QydionRefundActiveSums 某订单在途+成功的退款累计（金额分、额度）。
// 累计封顶必须把 pending 一并计入：pending 已预扣了额度、并占用了退款额度空间，
// 若只数 succeeded，两个并发退款都能通过「累计未超原额」检查，把同一笔额度退两次。
// failed 不计入：渠道失败时额度已回补、钱没有退出去，不应占用封顶空间。
func QydionRefundActiveSums(tx *gorm.DB, tradeNo string) (cents int64, quota int64, err error) {
	var sums struct {
		Cents int64
		Quota int64
	}
	err = tx.Model(&QydionRefund{}).
		Select("COALESCE(SUM(refund_cents),0) AS cents, COALESCE(SUM(refund_quota),0) AS quota").
		Where("trade_no = ? AND status IN ?", tradeNo, []string{RefundStatusPending, RefundStatusSucceeded}).
		Scan(&sums).Error
	return sums.Cents, sums.Quota, err
}

func QydionGetRefundById(id int64) (*QydionRefund, error) {
	var r QydionRefund
	if err := DB.First(&r, id).Error; err != nil {
		return nil, err
	}
	return &r, nil
}

// QydionListRefundsPage 管理员分页列表；tradeNo 非空时按订单号精确过滤。
func QydionListRefundsPage(page, pageSize int, tradeNo string) (list []QydionRefund, total int64, err error) {
	query := DB.Model(&QydionRefund{})
	if tradeNo != "" {
		query = query.Where("trade_no = ?", tradeNo)
	}
	if err = query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	err = query.Order("id DESC").Limit(pageSize).Offset((page - 1) * pageSize).Find(&list).Error
	return list, total, err
}

// QydionListUserRefunds 用户侧：本人退款记录（时间倒序，限量）。
func QydionListUserRefunds(userId int, limit int) ([]QydionRefund, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	var list []QydionRefund
	err := DB.Where("user_id = ?", userId).Order("id DESC").Limit(limit).Find(&list).Error
	return list, err
}

// QydionRefundCountByUserAndTradeNo 防滥用辅助：同一订单的退款发起次数（含失败）。
func QydionRefundCountByUserAndTradeNo(tradeNo string) (int64, error) {
	var cnt int64
	err := DB.Model(&QydionRefund{}).Where("trade_no = ?", tradeNo).Count(&cnt).Error
	return cnt, err
}

var ErrQydionRefundNotFound = errors.New("退款单不存在")
