package payment

// QYDION 微信 Native 扫码支付（wx_native，§8.6b M7 全量实做，替代原预留占位）
// 纯标准库实现微信支付 APIv3（商户请求签名 RSA-SHA256 + 回调 AES-256-GCM 解密 +
// 平台证书自动下载轮换），不引入新依赖（§0.2）。
// 后台选项（填入即启用，§0.8 占位原则）：
//   QydionWxpayAppId        公众号/应用 AppID
//   QydionWxpayMchId        商户号
//   QydionWxpayMchSerial    商户 API 证书序列号
//   QydionWxpayPrivateKey   商户 API 私钥（PEM/PKCS8，或裸 base64）
//   QydionWxpayApiV3Key     APIv3 密钥（32 字节）
// 验证方式：单测覆盖签名串构造与 AES-GCM 回调解密闭环（qydion_payment_test.go）。

import (
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/service"
)

const wxpayAPIBase = "https://api.mch.weixin.qq.com"

// WxNativeProvider 的 mu 只护平台证书缓存；回调金额一律随返回值交出，
// 不留「最近一笔」实例字段（全局单例上会跨请求串味）。
type WxNativeProvider struct {
	mu sync.Mutex

	platformCerts   map[string]*rsa.PublicKey // serial -> 公钥
	platformCertsAt time.Time
	lastCertFetch   time.Time
}

func (p *WxNativeProvider) Name() string        { return "wx_native" }
func (p *WxNativeProvider) DisplayName() string { return "微信 Native 扫码" }

func (p *WxNativeProvider) Enabled() bool {
	return service.QydionSettingStr("QydionWxpayAppId", "") != "" &&
		service.QydionSettingStr("QydionWxpayMchId", "") != "" &&
		service.QydionSettingStr("QydionWxpayMchSerial", "") != "" &&
		service.QydionSettingStr("QydionWxpayPrivateKey", "") != "" &&
		service.QydionSettingStr("QydionWxpayApiV3Key", "") != ""
}

func (p *WxNativeProvider) requiredOff() error {
	return errors.New("微信 Native 未配置参数（QydionWxpay* 选项），无法发起支付")
}

// CreatePayment Native 下单 → 返回二维码内容（code_url）
func (p *WxNativeProvider) CreatePayment(order Order) (PaymentRequest, error) {
	if !p.Enabled() {
		return PaymentRequest{}, p.requiredOff()
	}
	cents := int64(order.Money*100 + 0.5)
	if cents <= 0 {
		return PaymentRequest{}, errors.New("微信支付金额非法")
	}
	body := map[string]any{
		"appid":        service.QydionSettingStr("QydionWxpayAppId", ""),
		"mchid":        service.QydionSettingStr("QydionWxpayMchId", ""),
		"description":  order.Subject,
		"out_trade_no": order.TradeNo,
		"time_expire":  time.Now().Add(30 * time.Minute).Format("2006-01-02T15:04:05+08:00"),
		"notify_url":   order.NotifyURL,
		"amount":       map[string]any{"total": cents, "currency": "CNY"},
	}
	raw, err := p.request("POST", "/v3/pay/transactions/native", body)
	if err != nil {
		return PaymentRequest{}, err
	}
	codeURL, _ := raw["code_url"].(string)
	if codeURL == "" {
		return PaymentRequest{}, fmt.Errorf("微信下单响应缺失 code_url：%v", raw)
	}
	// 微信以 out_trade_no 为查询主键，无额外会话引用
	return PaymentRequest{QrContent: codeURL}, nil
}

// HandleCallback 验签（平台公钥 + 时间窗）→ AES-256-GCM 解密 → out_trade_no/trade_state/amount
func (p *WxNativeProvider) HandleCallback(req *http.Request) (ChannelReport, error) {
	if !p.Enabled() {
		return ChannelReport{}, p.requiredOff()
	}
	ts := req.Header.Get("Wechatpay-Timestamp")
	nonce := req.Header.Get("Wechatpay-Nonce")
	signature := req.Header.Get("Wechatpay-Signature")
	serial := req.Header.Get("Wechatpay-Serial")
	if ts == "" || nonce == "" || signature == "" || serial == "" {
		return ChannelReport{}, errors.New("微信回调头缺失（Wechatpay-*）")
	}
	// 时间窗 ±5 分钟（官方建议防重放）
	tsInt, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || func() bool { d := time.Since(time.Unix(tsInt, 0)); return d > 5*time.Minute || d < -5*time.Minute }() {
		return ChannelReport{}, errors.New("微信回调时间戳超出 ±5 分钟窗口（防重放）")
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, 1<<20))
	if err != nil {
		return ChannelReport{}, err
	}
	// 验签串：timestamp\nnonce\nbody\n
	pub, err := p.platformPublicKey(serial)
	if err != nil {
		return ChannelReport{}, err
	}
	sig, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return ChannelReport{}, errors.New("微信回调签名 base64 解码失败")
	}
	digest := sha256.Sum256([]byte(ts + "\n" + nonce + "\n" + string(body) + "\n"))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		return ChannelReport{}, errors.New("微信回调验签失败")
	}
	// 解密 resource
	var envelope struct {
		Resource struct {
			Ciphertext     string `json:"ciphertext"`
			Nonce          string `json:"nonce"`
			AssociatedData string `json:"associated_data"`
			OriginalType   string `json:"original_type"`
			Algorithm      string `json:"algorithm"`
		} `json:"resource"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ChannelReport{}, fmt.Errorf("微信回调体解析失败：%w", err)
	}
	plain, err := wxDecryptResource(
		service.QydionSettingStr("QydionWxpayApiV3Key", ""),
		envelope.Resource.Nonce, envelope.Resource.AssociatedData, envelope.Resource.Ciphertext)
	if err != nil {
		return ChannelReport{}, err
	}
	var event struct {
		OutTradeNo string `json:"out_trade_no"`
		TradeState string `json:"trade_state"`
		Amount     struct {
			Total    int64  `json:"total"`
			Currency string `json:"currency"`
		} `json:"amount"`
	}
	if err := json.Unmarshal(plain, &event); err != nil {
		return ChannelReport{}, fmt.Errorf("微信回调明文解析失败：%w", err)
	}
	report := ChannelReport{
		OrderId:   event.OutTradeNo,
		Paid:      event.TradeState == "SUCCESS",
		AmountCNY: float64(event.Amount.Total) / 100, // 渠道侧实收（分 → 元），随返回值交给入账比对
	}
	return report, nil
}

// QueryStatus 主动查单（对账兜底）
func (p *WxNativeProvider) QueryStatus(order Order) (ChannelReport, error) {
	if !p.Enabled() {
		return ChannelReport{}, p.requiredOff()
	}
	path := "/v3/pay/transactions/out-trade-no/" + order.TradeNo +
		"?mchid=" + service.QydionSettingStr("QydionWxpayMchId", "")
	raw, err := p.request("GET", path, nil)
	if err != nil {
		return ChannelReport{}, err
	}
	state, _ := raw["trade_state"].(string)
	report := ChannelReport{OrderId: order.TradeNo, Paid: state == "SUCCESS"}
	if amt, ok := raw["amount"].(map[string]any); ok {
		if total, ok := amt["total"].(float64); ok {
			report.AmountCNY = total / 100
		}
	}
	return report, nil
}

// ---------- APIv3 协议实现 ----------

const (
	// 平台证书轮换以天为单位，12 小时缓存兼顾轮换跟上与接口配额。
	wxPlatformCertTTL = 12 * time.Hour
	// 未知序列号（伪造的回调头也算）不能把证书下载接口打成放大器。
	wxPlatformCertRefill = time.Minute
)

// request 组签名头 → 请求 → 校验应答签名（平台公钥）→ 返回 JSON map
func (p *WxNativeProvider) request(method, path string, body any) (map[string]any, error) {
	status, header, respBody, err := p.doSignedRequest(method, path, body)
	if err != nil {
		return nil, err
	}
	// 应答验签：2xx 一律要求签名覆盖，缺签名头不能当「这个接口不签名」放过。
	if status >= 200 && status < 300 {
		if err := p.verifyResponseSignature(header, respBody); err != nil {
			return nil, err
		}
	}
	var out map[string]any
	if err := common.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("微信响应解析失败（HTTP %d）：%w", status, err)
	}
	if status != 200 {
		msg, _ := out["message"].(string)
		return nil, fmt.Errorf("微信接口失败 HTTP %d：%s", status, msg)
	}
	return out, nil
}

// doSignedRequest 只负责签名、发送与读取，不做应答验签。
// 证书引导必须走这条路径：应答验签要平台公钥，而公钥正来自证书下载响应本身，
// 走 request 就成了「下载证书 → 验签 → 再下载证书」的递归。
func (p *WxNativeProvider) doSignedRequest(method, path string, body any) (int, http.Header, []byte, error) {
	var bodyBytes []byte
	if body != nil {
		b, err := common.Marshal(body)
		if err != nil {
			return 0, nil, nil, err
		}
		bodyBytes = b
	}
	ts := fmt.Sprintf("%d", time.Now().Unix())
	nonce := wxNonce()
	// 签名串：METHOD\nPATH(含query)\nts\nnonce\nbody\n
	digest := sha256.Sum256([]byte(method + "\n" + path + "\n" + ts + "\n" + nonce + "\n" + string(bodyBytes) + "\n"))
	key, err := wxLoadPrivateKey(service.QydionSettingStr("QydionWxpayPrivateKey", ""))
	if err != nil {
		return 0, nil, nil, err
	}
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return 0, nil, nil, err
	}
	auth := fmt.Sprintf(
		`WECHATPAY2-SHA256-RSA2048 mchid="%s",nonce_str="%s",timestamp="%s",serial_no="%s",signature="%s"`,
		service.QydionSettingStr("QydionWxpayMchId", ""), nonce, ts,
		service.QydionSettingStr("QydionWxpayMchSerial", ""),
		base64.StdEncoding.EncodeToString(sig))
	url := wxpayAPIBase + path
	var req *http.Request
	if bodyBytes != nil {
		req, err = http.NewRequest(method, url, strings.NewReader(string(bodyBytes)))
	} else {
		req, err = http.NewRequest(method, url, nil)
	}
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "QYDION-API")
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, nil, err
	}
	return resp.StatusCode, resp.Header, respBody, nil
}

func (p *WxNativeProvider) verifyResponseSignature(h http.Header, body []byte) error {
	ts := h.Get("Wechatpay-Timestamp")
	sigB64 := h.Get("Wechatpay-Signature")
	serial := h.Get("Wechatpay-Serial")
	if ts == "" || sigB64 == "" || serial == "" {
		// 下单/查单应答都由平台证书签名；缺头即不可采信。
		return errors.New("微信应答签名头缺失（Wechatpay-Serial/Timestamp/Signature）")
	}
	pub, err := p.platformPublicKey(serial)
	if err != nil {
		return err
	}
	return wxVerifyResponseSignature(h, body, pub)
}

// wxVerifyResponseSignature 按 APIv3 规范验应答：timestamp\nnonce\nbody\n
func wxVerifyResponseSignature(h http.Header, body []byte, pub *rsa.PublicKey) error {
	sig, err := base64.StdEncoding.DecodeString(h.Get("Wechatpay-Signature"))
	if err != nil {
		return errors.New("微信应答签名解码失败")
	}
	digest := sha256.Sum256([]byte(h.Get("Wechatpay-Timestamp") + "\n" + h.Get("Wechatpay-Nonce") + "\n" + string(body) + "\n"))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		return errors.New("微信应答验签失败")
	}
	return nil
}

// platformPublicKey 平台证书公钥；缓存缺失或序列号未知时下载一次。
// 调用证书下载只能经 refreshPlatformCertsLocked，且全程持锁——既是单飞（避免并发
// 冷启动重复下载），也切断了「验签需要公钥、公钥来自需验签的响应」这条回边。
func (p *WxNativeProvider) platformPublicKey(serial string) (*rsa.PublicKey, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pub, ok := p.platformCerts[serial]; ok && time.Since(p.platformCertsAt) < wxPlatformCertTTL {
		return pub, nil
	}
	if err := p.refreshPlatformCertsLocked(); err != nil {
		return nil, err
	}
	pub, ok := p.platformCerts[serial]
	if !ok {
		return nil, fmt.Errorf("微信回调证书序列号 %s 不在平台证书列表中", serial)
	}
	return pub, nil
}

// refreshPlatformCertsLocked 下载 → 解密 → 用本次下载到的证书自身验签响应 → 装缓存。
// 调用方必须持有 p.mu。
func (p *WxNativeProvider) refreshPlatformCertsLocked() error {
	if len(p.platformCerts) > 0 && time.Since(p.lastCertFetch) < wxPlatformCertRefill {
		return nil
	}
	p.lastCertFetch = time.Now()
	status, header, body, err := p.doSignedRequest("GET", "/v3/certificates", nil)
	if err != nil {
		return fmt.Errorf("微信平台证书下载失败：%w", err)
	}
	if status != 200 {
		return fmt.Errorf("微信平台证书下载失败 HTTP %d：%s", status, string(body))
	}
	var raw map[string]any
	if err := common.Unmarshal(body, &raw); err != nil {
		return fmt.Errorf("微信平台证书响应解析失败：%w", err)
	}
	apiV3Key := service.QydionSettingStr("QydionWxpayApiV3Key", "")
	certs := map[string]*rsa.PublicKey{}
	list, _ := raw["data"].([]any)
	for _, item := range list {
		entry, _ := item.(map[string]any)
		serial, _ := entry["serial_no"].(string)
		enc, _ := entry["encrypt_certificate"].(map[string]any)
		if serial == "" || enc == nil {
			continue
		}
		cipherB64, _ := enc["ciphertext"].(string)
		nonce, _ := enc["nonce"].(string)
		adata, _ := enc["associated_data"].(string)
		der, err := wxDecryptResource(apiV3Key, nonce, adata, cipherB64)
		if err != nil {
			continue
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			continue
		}
		// 证书自身的序列号必须与列表条目一致，否则缓存键就是攻击者可控的。
		// 按数值比较：微信写十六进制带前导零，big.Int.Text(16) 会把前导零去掉。
		wantSerial, ok := new(big.Int).SetString(serial, 16)
		if !ok || wantSerial.Cmp(cert.SerialNumber) != 0 {
			continue
		}
		if pub, ok := cert.PublicKey.(*rsa.PublicKey); ok {
			certs[serial] = pub
		}
	}
	if len(certs) == 0 {
		return errors.New("微信平台证书解析为空")
	}
	// 可信校验：这份响应必须由它自己带回来的证书验得过，且序列号在列表内。
	// 未签名的证书列表等于把「谁的身份算微信」交给任何能改写响应的人。
	bootstrapSerial := header.Get("Wechatpay-Serial")
	if bootstrapSerial == "" || header.Get("Wechatpay-Signature") == "" {
		return errors.New("微信平台证书响应未签名，拒绝采信")
	}
	bootstrap, ok := certs[bootstrapSerial]
	if !ok {
		return fmt.Errorf("微信平台证书响应的序列号 %s 不在证书列表中", bootstrapSerial)
	}
	if err := wxVerifyResponseSignature(header, body, bootstrap); err != nil {
		return fmt.Errorf("微信平台证书响应验签失败：%w", err)
	}
	p.platformCerts = certs
	p.platformCertsAt = time.Now()
	return nil
}

// wxDecryptResource APIv3 AES-256-GCM 解密（返回明文字节）
func wxDecryptResource(apiV3Key, nonce, associatedData, ciphertextB64 string) ([]byte, error) {
	key := []byte(apiV3Key)
	if len(key) != 32 {
		return nil, errors.New("APIv3 密钥长度必须为 32 字节")
	}
	ciphertext, err := base64.StdEncoding.DecodeString(ciphertextB64)
	if err != nil {
		return nil, errors.New("微信回调解密：ciphertext 解码失败")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	// APIv3 固定 12 字节 nonce；长度不符会让 gcm.Open 直接 panic，
	// 而这段代码处理的是外部应答。
	if len(nonce) != gcm.NonceSize() {
		return nil, fmt.Errorf("微信解密 nonce 长度必须为 %d 字节，实际 %d", gcm.NonceSize(), len(nonce))
	}
	plain, err := gcm.Open(nil, []byte(nonce), ciphertext, []byte(associatedData))
	if err != nil {
		return nil, errors.New("微信回调解密失败（APIv3 密钥不匹配或数据被篡改）")
	}
	return plain, nil
}

func wxLoadPrivateKey(pemStr string) (*rsa.PrivateKey, error) {
	block, err := pemDecode(pemStr)
	if err != nil {
		return nil, errors.New("微信商户私钥解析失败")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block); err == nil {
		return k, nil
	}
	k8, err := x509.ParsePKCS8PrivateKey(block)
	if err != nil {
		return nil, errors.New("微信商户私钥解析失败（需 PKCS8/PKCS1）")
	}
	key, ok := k8.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("微信商户私钥类型非 RSA")
	}
	return key, nil
}

func wxNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawStdEncoding.EncodeToString(b)[:22]
}

var _ = common.SysLog

func init() {
	Register(&WxNativeProvider{})
}
