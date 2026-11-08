package service

// QYDION M4 纯逻辑单测（不依赖 DB）

import (
	"testing"
)

func TestMatchBonusTier(t *testing.T) {
	rule := &topupBonusRule{
		Tiers: []topupBonusTier{
			{Min: 100, Bonus: 10},
			{Min: 500, Bonus: 80},
			{Min: 1000, Bonus: 200},
		},
	}
	cases := []struct {
		pay  float64
		want float64
	}{
		{99, 0},     // 未达最低档
		{100, 10},   // 恰好最低档
		{499, 10},   // 未达第二档
		{500, 80},   // 第二档
		{1500, 200}, // 最高档
	}
	for _, c := range cases {
		got := matchBonusTier(rule, c.pay)
		var gotBonus float64
		if got != nil {
			gotBonus = got.Bonus
		}
		if gotBonus != c.want {
			t.Errorf("pay=%.2f 期望赠额 %.2f，实际 %.2f", c.pay, c.want, gotBonus)
		}
	}
}

func TestQydionMoneyToQuota(t *testing.T) {
	// Price 默认 7.3：¥7.3 = $1 = 500000 quota
	q, err := qydionMoneyToQuota(7.3)
	if err != nil {
		t.Fatalf("换算失败: %v", err)
	}
	if q != 500000 {
		t.Errorf("¥7.3 应=500000 quota，实际 %d", q)
	}
	// ¥73 = $10 = 5,000,000
	q2, _ := qydionMoneyToQuota(73)
	if q2 != 5000000 {
		t.Errorf("¥73 应=5000000 quota，实际 %d", q2)
	}
	// 0 元拒绝
	if _, err := qydionMoneyToQuota(0); err == nil {
		t.Errorf("0 元应报错")
	}
}

func TestQydionClampMaxTokens(t *testing.T) {
	if got := QydionClampMaxTokens(999999); got != 8192 {
		t.Errorf("超限应钳到默认 8192，实际 %d", got)
	}
	if got := QydionClampMaxTokens(100); got != 100 {
		t.Errorf("未超限应原样返回，实际 %d", got)
	}
	if got := QydionClampMaxTokens(0); got != 0 {
		t.Errorf("未设置应返回 0，实际 %d", got)
	}
}

// TestQydionClampMaxTokensFields 防破产闸门必须覆盖整个 max_tokens 家族。
// 计费上限取的是同族里的最大值（relay/request_billing.go 用 max(max_tokens,
// max_completion_tokens)，Responses 直接取 max_output_tokens），
// 只钳其中一个字段，客户换个字段名即可绕过。
func TestQydionClampMaxTokensFields(t *testing.T) {
	maxTokens := uint(999999)
	maxCompletionTokens := uint(999999)
	unset := uint(50)
	QydionClampMaxTokensFields(&maxTokens, &maxCompletionTokens, &unset, nil)

	if maxTokens != 8192 {
		t.Errorf("max_tokens 应被钳到 8192，实际 %d", maxTokens)
	}
	if maxCompletionTokens != 8192 {
		t.Errorf("max_completion_tokens 同样要钳，实际 %d —— 不钳就是绕过口", maxCompletionTokens)
	}
	if unset != 50 {
		t.Errorf("未超限应原样保留，实际 %d", unset)
	}
}
