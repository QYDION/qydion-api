package payment

// QYDION 退款渠道回归（交接报告 §E）：支付宝 alipay.trade.refund 与微信 v3 退款。
// 全部走内存替身（recordingTransport / 平台证书替身），不向真实网关发起任何请求。

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func signedAlipayRefundBody(t *testing.T, key *rsa.PrivateKey, node string) string {
	t.Helper()
	digest := sha256.Sum256([]byte(node))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	require.NoError(t, err)
	return `{"alipay_trade_refund_response":` + node +
		`,"sign":"` + base64.StdEncoding.EncodeToString(sig) + `"}`
}

// TestAlipayRefundSignsRequestAndParsesCredential 退款出站必须按请求侧规范签名，
// 字段齐全（method/out_trade_no/out_request_no/refund_amount），受理成功取支付宝交易号做凭证。
func TestAlipayRefundSignsRequestAndParsesCredential(t *testing.T) {
	key, privPEM, pubPEM := testAlipayKeys(t)
	qydionPaymentOptions(t, map[string]string{
		"QydionAlipayAppId":      "qydion-app",
		"QydionAlipayPrivateKey": privPEM,
		"QydionAlipayPublicKey":  pubPEM,
	})
	const node = `{"code":"10000","msg":"Success","out_trade_no":"QYD-RF-1","trade_no":"2026110822001000001","fund_change":"Y"}`
	rt := &recordingTransport{handler: func(r *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, signedAlipayRefundBody(t, key, node)), nil
	}}
	useTransport(t, rt)

	result, err := (&AlipayF2FProvider{}).Refund(RefundRequest{
		TradeNo: "QYD-RF-1", OutRequestNo: "QYD-RF-1-R1700000000aa", RefundCents: 3050,
	})
	require.NoError(t, err)
	assert.True(t, result.Succeeded)
	assert.Equal(t, "2026110822001000001", result.Credential)

	require.Len(t, rt.forms, 1)
	sent := rt.forms[0]
	assert.Equal(t, "alipay.trade.refund", sent.Get("method"))
	// 业务参数在 biz_content JSON 内（alipay 协议），不在表单顶层
	var biz map[string]any
	require.NoError(t, json.Unmarshal([]byte(sent.Get("biz_content")), &biz))
	assert.Equal(t, "QYD-RF-1", biz["out_trade_no"])
	assert.Equal(t, "QYD-RF-1-R1700000000aa", biz["out_request_no"])
	assert.Equal(t, "30.50", biz["refund_amount"], "金额按分换算成元两位小数")

	// 出站表单必须按请求侧规范签名（含 sign_type），独立重算验一次
	digest := sha256.Sum256([]byte(alipayFormSignSource(sent)))
	sig, err := base64.StdEncoding.DecodeString(sent.Get("sign"))
	require.NoError(t, err)
	require.NoError(t, rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig),
		"退款出站签名未按请求侧规范覆盖 sign_type")
}

// TestAlipayRefundReportsGatewayRejection 渠道拒绝（如超可退余额）必须返回错误，
// 调用方据此把退款单记为 failed——不能把非 10000 当成功。
func TestAlipayRefundReportsGatewayRejection(t *testing.T) {
	key, privPEM, pubPEM := testAlipayKeys(t)
	qydionPaymentOptions(t, map[string]string{
		"QydionAlipayAppId":      "qydion-app",
		"QydionAlipayPrivateKey": privPEM,
		"QydionAlipayPublicKey":  pubPEM,
	})
	const node = `{"code":"40004","msg":"Business Failed","sub_code":"ACQ.REFUND_AMT_INCONSISTENT","sub_msg":"退款金额超可退"}`
	useTransport(t, &recordingTransport{handler: func(r *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, signedAlipayRefundBody(t, key, node)), nil
	}})

	result, err := (&AlipayF2FProvider{}).Refund(RefundRequest{
		TradeNo: "QYD-RF-2", OutRequestNo: "QYD-RF-2-R1", RefundCents: 100,
	})
	require.Error(t, err)
	assert.False(t, result.Succeeded)
	assert.Contains(t, err.Error(), "ACQ.REFUND_AMT_INCONSISTENT")
}

// TestAlipayRefundRequiresConfig 未配置密钥时明确报错，不发起请求、不假成功。
func TestAlipayRefundRequiresConfig(t *testing.T) {
	rt := &recordingTransport{handler: func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("未配置渠道不应发出任何请求")
	}}
	useTransport(t, rt)

	_, err := (&AlipayF2FProvider{}).Refund(RefundRequest{TradeNo: "QYD", OutRequestNo: "R", RefundCents: 1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "未配置")
}

// TestWxRefundOutcomeStates 终态判定纯函数：受理成功（SUCCESS/PROCESSING）给凭证号，
// CLOSED/ABNORMAL 与未知状态一律不按成功处理。
func TestWxRefundOutcomeStates(t *testing.T) {
	result, err := wxRefundOutcome(map[string]any{"refund_id": "wx-rf-1", "status": "SUCCESS"})
	require.NoError(t, err)
	assert.True(t, result.Succeeded)
	assert.Equal(t, "wx-rf-1", result.Credential)

	result, err = wxRefundOutcome(map[string]any{"refund_id": "wx-rf-2", "status": "PROCESSING"})
	require.NoError(t, err, "受理中按受理成功记账，终态以微信为准")
	assert.True(t, result.Succeeded)
	assert.Equal(t, "wx-rf-2", result.Credential)
	assert.Equal(t, "PROCESSING", result.ChannelStatus)

	for _, status := range []string{"CLOSED", "ABNORMAL", "", "UNKNOWN"} {
		_, err := wxRefundOutcome(map[string]any{"refund_id": "x", "status": status})
		require.Error(t, err, "status=%q 不得按成功处理", status)
	}
	_, err = wxRefundOutcome(nil)
	require.Error(t, err)
}

// TestWxRefundFullFlowWithPlatformCerts 全链路：商户签名请求 → 平台证书引导 → 应答验签。
// 验证退款请求体带 out_refund_no 与 amount{refund,total}，成功响应取 refund_id 做凭证。
func TestWxRefundFullFlowWithPlatformCerts(t *testing.T) {
	merchantKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	platformKey, der := wxTestPlatformCertificate(t)
	wxTestOptions(t, merchantKey)

	var refundBody map[string]any
	certCalls, refundCalls := 0, 0
	rt := &recordingTransport{handler: func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v3/certificates":
			certCalls++
			return wxTestSigned(t, platformKey, wxTestSerial, wxTestCertBody(t, der)), nil
		case "/v3/refund/domestic/refunds":
			refundCalls++
			raw, readErr := io.ReadAll(r.Body)
			require.NoError(t, readErr)
			require.NoError(t, json.Unmarshal(raw, &refundBody))
			return wxTestSigned(t, platformKey, wxTestSerial,
				`{"refund_id":"wx-rf-9","out_refund_no":"QYD-RF-3-R1","status":"SUCCESS"}`), nil
		default:
			return nil, errors.New("synthetic transport received an unexpected path: " + r.URL.Path)
		}
	}}
	useTransport(t, rt)

	result, err := (&WxNativeProvider{}).Refund(RefundRequest{
		TradeNo: "QYD-RF-3", OutRequestNo: "QYD-RF-3-R1", RefundCents: 5000, TotalCents: 10000,
	})
	require.NoError(t, err)
	assert.True(t, result.Succeeded)
	assert.Equal(t, "wx-rf-9", result.Credential)
	assert.Equal(t, 1, certCalls, "证书引导只能下载一次")

	amount, _ := refundBody["amount"].(map[string]any)
	require.NotNil(t, amount, "退款请求体必须带 amount")
	assert.Equal(t, "QYD-RF-3", refundBody["out_trade_no"])
	assert.Equal(t, "QYD-RF-3-R1", refundBody["out_refund_no"])
	assert.InDelta(t, float64(5000), amount["refund"], 0, "refund=本次退款金额（分）")
	assert.InDelta(t, float64(10000), amount["total"], 0, "total=原单实付（分）")
	assert.Equal(t, "CNY", amount["currency"])
}

// TestWxRefundHttpErrorIsFailure 渠道 HTTP 失败必须按失败透出，调用方记 failed 并回补。
func TestWxRefundHttpErrorIsFailure(t *testing.T) {
	merchantKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	platformKey, der := wxTestPlatformCertificate(t)
	wxTestOptions(t, merchantKey)

	useTransport(t, &recordingTransport{handler: func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v3/certificates":
			return wxTestSigned(t, platformKey, wxTestSerial, wxTestCertBody(t, der)), nil
		case "/v3/refund/domestic/refunds":
			// 微信失败应答为非 2xx（v3 规范：错误应答不在平台证书签名范围内）
			return jsonResponse(http.StatusInternalServerError,
				`{"code":"NOT_ENOUGH","message":"商户余额不足"}`), nil
		default:
			return nil, errors.New("unexpected path: " + r.URL.Path)
		}
	}})

	_, err = (&WxNativeProvider{}).Refund(RefundRequest{
		TradeNo: "QYD-RF-4", OutRequestNo: "QYD-RF-4-R1", RefundCents: 1, TotalCents: 100,
	})
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "商户余额不足"),
		"错误信息应透出渠道原因，实际：%v", err)
}

// TestWxRefundRequiresConfigAndAmount 未配置与非法金额都在出口被拒。
func TestWxRefundRequiresConfigAndAmount(t *testing.T) {
	rt := &recordingTransport{handler: func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("不应发出任何请求")
	}}
	useTransport(t, rt)

	_, err := (&WxNativeProvider{}).Refund(RefundRequest{TradeNo: "QYD", OutRequestNo: "R", RefundCents: 1, TotalCents: 10})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "未配置")

	_, privPEM, pubPEM := testAlipayKeys(t)
	qydionPaymentOptions(t, map[string]string{
		"QydionAlipayAppId":      "qydion-app",
		"QydionAlipayPrivateKey": privPEM,
		"QydionAlipayPublicKey":  pubPEM,
	})
	_, err = (&AlipayF2FProvider{}).Refund(RefundRequest{TradeNo: "QYD", OutRequestNo: "R", RefundCents: 0})
	require.Error(t, err, "退款金额必须 > 0")
}
