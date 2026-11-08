package payment

// QYDION 支付宝当面付（alipay_f2f，§8.6b M6 必做）
// 纯标准库实现 RSA2(SHA256WithRSA) 签名/验签 —— 不引入 alipay-sdk 依赖（§0.2 升级依赖需报批）。
// 密钥未配置（附录B-4）时 Enabled()=false，全部调用返回明确错误；签约后由用户在后台选项填入：
//   QydionAlipayAppId / QydionAlipayPrivateKey(应用私钥PKCS1) / QydionAlipayPublicKey(支付宝公钥)
//   QydionAlipayGateway（沙箱: https://openapi-sandbox.dl.alipaydev.com/gateway.do）
// 验证方式：单测用测试 RSA 密钥对完成签名/验签闭环（见 qydion_payment_test.go）。

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/service"
)

// AlipayF2FProvider 无状态：渠道回执一律随返回值交出（全局单例上不得留「最近一笔」）。
type AlipayF2FProvider struct{}

func (p *AlipayF2FProvider) Name() string        { return "alipay_f2f" }
func (p *AlipayF2FProvider) DisplayName() string { return "支付宝当面付（扫码）" }

// Enabled 密钥三项齐备才算启用（占位原则：代码完整、配置留空、开关关闭）
func (p *AlipayF2FProvider) Enabled() bool {
	return service.QydionSettingStr("QydionAlipayAppId", "") != "" &&
		service.QydionSettingStr("QydionAlipayPrivateKey", "") != "" &&
		service.QydionSettingStr("QydionAlipayPublicKey", "") != ""
}

func (p *AlipayF2FProvider) requiredOff() error {
	return errors.New("支付宝当面付未配置密钥（QydionAlipay* 选项），无法发起支付")
}

// CreatePayment 预下单（alipay.trade.precreate）→ 返回二维码内容（qrCode 字符串）
func (p *AlipayF2FProvider) CreatePayment(order Order) (PaymentRequest, error) {
	if !p.Enabled() {
		return PaymentRequest{}, p.requiredOff()
	}
	biz := map[string]any{
		"out_trade_no":    order.TradeNo,
		"total_amount":    fmt.Sprintf("%.2f", order.Money),
		"subject":         order.Subject,
		"timeout_express": "30m",
	}
	resp, err := p.call("alipay.trade.precreate", biz, order.NotifyURL)
	if err != nil {
		return PaymentRequest{}, err
	}
	// 响应体：{"alipay_trade_precreate_response":{...code,msg,qrCode...},"sign":"..."}
	node, _ := resp["alipay_trade_precreate_response"].(map[string]any)
	if node == nil {
		return PaymentRequest{}, errors.New("支付宝预下单响应缺失")
	}
	if code, _ := node["code"].(string); code != "10000" {
		msg, _ := node["sub_msg"].(string)
		return PaymentRequest{}, fmt.Errorf("支付宝预下单失败 code=%s msg=%v", code, msg)
	}
	qr, _ := node["qr_code"].(string)
	// 支付宝无渠道侧会话引用，查单以 out_trade_no 为准
	return PaymentRequest{QrContent: qr}, nil
}

// HandleCallback 异步通知验签（RSA2）；验签失败必须返回错误（§10.4）
func (p *AlipayF2FProvider) HandleCallback(req *http.Request) (ChannelReport, error) {
	if !p.Enabled() {
		return ChannelReport{}, p.requiredOff()
	}
	if err := req.ParseForm(); err != nil {
		return ChannelReport{}, err
	}
	params := map[string]string{}
	for k, v := range req.PostForm {
		if len(v) > 0 {
			params[k] = v[0]
		}
	}
	for k, v := range req.URL.Query() {
		if _, ok := params[k]; !ok && len(v) > 0 {
			params[k] = v[0]
		}
	}
	sign := params["sign"]
	signType := params["sign_type"]
	if sign == "" || (signType != "" && signType != "RSA2") {
		return ChannelReport{}, errors.New("支付回调验签失败：sign/sign_type 缺失或类型错误")
	}
	if err := AlipayVerify(params, sign, service.QydionSettingStr("QydionAlipayPublicKey", "")); err != nil {
		return ChannelReport{}, fmt.Errorf("支付回调验签失败：%w", err)
	}
	// M7 全量加固（接线授权：负责人 2026-10-31「支付方式全量做好」）：
	// ①app_id 必须为本应用（防跨应用通知伪造）②notify_time 时间窗 ±24h（防历史通知重放）
	if appID := params["app_id"]; appID != service.QydionSettingStr("QydionAlipayAppId", "") {
		return ChannelReport{}, errors.New("支付回调校验失败：app_id 与本应用不符")
	}
	if nt := params["notify_time"]; nt != "" {
		if t, err := time.ParseInLocation("2006-01-02 15:04:05", nt, time.Local); err == nil {
			if diff := time.Since(t); diff > 24*time.Hour || diff < -24*time.Hour {
				return ChannelReport{}, errors.New("支付回调校验失败：notify_time 超出时间窗（防重放）")
			}
		}
	}
	tradeStatus := params["trade_status"]
	report := ChannelReport{
		OrderId: params["out_trade_no"],
		Paid:    tradeStatus == "TRADE_SUCCESS" || tradeStatus == "TRADE_FINISHED",
	}
	// 渠道侧实收金额随返回值交出；解析不出即留 0，由 controller 按「金额不可信」拒入账
	if amt, err := strconv.ParseFloat(params["total_amount"], 64); err == nil {
		report.AmountCNY = amt
	}
	return report, nil
}

// QueryStatus 主动查询（对账兜底）
func (p *AlipayF2FProvider) QueryStatus(order Order) (ChannelReport, error) {
	if !p.Enabled() {
		return ChannelReport{}, p.requiredOff()
	}
	resp, err := p.call("alipay.trade.query", map[string]any{"out_trade_no": order.TradeNo}, "")
	if err != nil {
		return ChannelReport{}, err
	}
	node, _ := resp["alipay_trade_query_response"].(map[string]any)
	if node == nil {
		return ChannelReport{}, errors.New("支付宝查询响应缺失")
	}
	code, _ := node["code"].(string)
	if code == "40004" {
		return ChannelReport{}, nil // 交易不存在=未支付
	}
	if code != "10000" {
		return ChannelReport{}, fmt.Errorf("支付宝查询失败 code=%s", code)
	}
	status, _ := node["trade_status"].(string)
	report := ChannelReport{
		OrderId: order.TradeNo,
		Paid:    status == "TRADE_SUCCESS" || status == "TRADE_FINISHED",
	}
	if totalAmount, _ := node["total_amount"].(string); totalAmount != "" {
		if amt, err := strconv.ParseFloat(strings.TrimSpace(totalAmount), 64); err == nil {
			report.AmountCNY = amt
		}
	}
	return report, nil
}

// ---------- 协议实现 ----------

func (p *AlipayF2FProvider) gateway() string {
	return service.QydionSettingStr("QydionAlipayGateway", "https://openapi.alipay.com/gateway.do")
}

// call 组包签名 → POST 网关 → 校验同步响应签名 → 返回解析后的响应节点 map
func (p *AlipayF2FProvider) call(method string, biz map[string]any, notifyURL string) (map[string]any, error) {
	bizJSON, err := common.Marshal(biz)
	if err != nil {
		return nil, err
	}
	pub := map[string]string{
		"app_id":      service.QydionSettingStr("QydionAlipayAppId", ""),
		"method":      method,
		"format":      "JSON",
		"charset":     "utf-8",
		"sign_type":   "RSA2",
		"timestamp":   time.Now().Format("2006-01-02 15:04:05"),
		"version":     "1.0",
		"biz_content": string(bizJSON),
	}
	// notify_url 是公共参数，不是 biz_content 成员：漏掉它，本订单的异步回调地址
	// 就没有告诉过渠道，渠道只会按应用级配置（或根本不）回调。
	if notifyURL != "" {
		pub["notify_url"] = notifyURL
	}
	sign, err := AlipaySign(pub, service.QydionSettingStr("QydionAlipayPrivateKey", ""))
	if err != nil {
		return nil, fmt.Errorf("签名失败：%w", err)
	}
	pub["sign"] = sign
	form := url.Values{}
	for k, v := range pub {
		form.Set(k, v)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.PostForm(p.gateway(), form)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return AlipayResponseNode(body, service.QydionSettingStr("QydionAlipayPublicKey", ""))
}

// AlipayResponseNode 校验同步响应签名并返回业务节点。
// 未验签的响应体不能直接当作下单成功：网关地址可配置，返回内容不可凭空信任。
func AlipayResponseNode(body []byte, alipayPublicKeyPEM string) (map[string]any, error) {
	var envelope map[string]json.RawMessage
	if err := common.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("支付宝响应解析失败：%w", err)
	}
	nodeKey := ""
	for key := range envelope {
		if key == "sign" || key == "sign_type" || key == "alipay_cert_sn" {
			continue
		}
		if nodeKey != "" {
			return nil, errors.New("支付宝响应含多个业务节点，无法确定签名对象")
		}
		nodeKey = key
	}
	if nodeKey == "" {
		return nil, errors.New("支付宝响应缺失业务节点")
	}
	nodeJSON := envelope[nodeKey]
	if nodeKey == "error_response" {
		// 错误响应不携带可入账的业务数据；透出网关错误码，调用方按失败处理。
		var errNode map[string]any
		_ = common.Unmarshal(nodeJSON, &errNode)
		code, _ := errNode["code"].(string)
		msg, _ := errNode["msg"].(string)
		subMsg, _ := errNode["sub_msg"].(string)
		return nil, fmt.Errorf("支付宝网关返回错误 code=%s msg=%s %s", code, msg, subMsg)
	}
	sign := ""
	if raw, ok := envelope["sign"]; ok {
		var s string
		if err := common.Unmarshal(raw, &s); err == nil {
			sign = s
		}
	}
	if sign == "" {
		return nil, errors.New("支付宝响应未签名，拒绝采信")
	}
	if err := AlipayVerifyResponse(nodeJSON, sign, alipayPublicKeyPEM); err != nil {
		return nil, fmt.Errorf("支付宝响应验签失败：%w", err)
	}
	var node map[string]any
	if err := common.Unmarshal(nodeJSON, &node); err != nil {
		return nil, fmt.Errorf("支付宝响应解析失败：%w", err)
	}
	return map[string]any{nodeKey: node}, nil
}

// AlipayVerifyResponse 对响应业务节点的原始 JSON 串验签。
// 节点内的 sign_type 不参与签名，必须原样剔除；其余字节要保持网关返回的顺序，
// 因为签名覆盖的就是这段文本本身。
func AlipayVerifyResponse(nodeJSON []byte, sign string, alipayPublicKeyPEM string) error {
	block, _ := pemDecode(alipayPublicKeyPEM)
	pubAny, err := x509.ParsePKIXPublicKey(block)
	if err != nil {
		return errors.New("支付宝公钥解析失败")
	}
	pub, ok := pubAny.(*rsa.PublicKey)
	if !ok {
		return errors.New("支付宝公钥类型非 RSA")
	}
	sig, err := base64.StdEncoding.DecodeString(sign)
	if err != nil {
		return errors.New("签名 base64 解码失败")
	}
	digest := sha256.Sum256(alipayResponseWithoutSignType(nodeJSON))
	return rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig)
}

// alipayResponseSignTypeMember matches the sign_type member where it can
// appear: last in the node (leading comma) or followed by another member.
// Deleting only this one scalar keeps the rest of the text byte-for-byte what
// the gateway signed; re-marshaling the node would reorder members and break
// the signature.
var alipayResponseSignTypeMember = regexp.MustCompile(`,"sign_type":"[^"]*"|"sign_type":"[^"]*",`)

func alipayResponseWithoutSignType(nodeJSON []byte) []byte {
	return alipayResponseSignTypeMember.ReplaceAll(nodeJSON, nil)
}


// AlipaySign 出站请求签名：公共参数按 key 字典序拼接（含 sign_type，不含 sign 与空值）
// → RSA2 私钥签名。请求侧的 sign_type 参与签名，与回调验签规则不同源。
func AlipaySign(params map[string]string, privateKeyPEM string) (string, error) {
	raw := alipayRequestSignSource(params)
	block, _ := pemDecode(privateKeyPEM)
	key, err := x509.ParsePKCS1PrivateKey(block)
	if err != nil {
		// 兼容 PKCS8
		k8, err8 := x509.ParsePKCS8PrivateKey(block)
		if err8 != nil {
			return "", errors.New("私钥解析失败（需 PKCS1/PKCS8）")
		}
		rsaKey, ok := k8.(*rsa.PrivateKey)
		if !ok {
			return "", errors.New("私钥类型非 RSA")
		}
		key = rsaKey
	}
	digest := sha256.Sum256([]byte(raw))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

// AlipayVerify 支付宝异步通知验签（排除 sign/sign_type 与空值，字典序拼接）
func AlipayVerify(params map[string]string, sign string, alipayPublicKeyPEM string) error {
	raw := alipayNotifySignSource(params)
	block, _ := pemDecode(alipayPublicKeyPEM)
	pubAny, err := x509.ParsePKIXPublicKey(block)
	if err != nil {
		return errors.New("支付宝公钥解析失败")
	}
	pub, ok := pubAny.(*rsa.PublicKey)
	if !ok {
		return errors.New("支付宝公钥类型非 RSA")
	}
	digest := sha256.Sum256([]byte(raw))
	sig, err := base64.StdEncoding.DecodeString(sign)
	if err != nil {
		return errors.New("签名 base64 解码失败")
	}
	return rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig)
}

// alipayRequestSignSource 出站请求的待签串：排除 sign 与空值，sign_type 参与签名。
func alipayRequestSignSource(params map[string]string) string {
	return alipayJoinSorted(params, false)
}

// alipayNotifySignSource 异步通知的待验串：支付宝规范排除 sign 与 sign_type。
// 两条规则不能共用一份实现——出站若按通知规则签，签名覆盖的字段就比实际发送的少
// 一个 sign_type，网关会拒单；而自己签自己验的测试看不到这一点。
func alipayNotifySignSource(params map[string]string) string {
	return alipayJoinSorted(params, true)
}

func alipayJoinSorted(params map[string]string, excludeSignType bool) string {
	keys := make([]string, 0, len(params))
	for k, v := range params {
		if k == "sign" || v == "" {
			continue
		}
		if excludeSignType && k == "sign_type" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+params[k])
	}
	return strings.Join(parts, "&")
}

// pemDecode 剥离 PEM 头尾，返回 DER 块（兼容无头的裸 base64）
func pemDecode(pemStr string) ([]byte, error) {
	s := strings.TrimSpace(pemStr)
	if strings.HasPrefix(s, "-----") {
		var body strings.Builder
		for _, line := range strings.Split(s, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "-----") {
				continue
			}
			body.WriteString(line)
		}
		s = body.String()
	}
	return base64.StdEncoding.DecodeString(s)
}

var _ = common.SysLog // 保留引用（日志按需使用）

func init() {
	Register(&AlipayF2FProvider{})
}
