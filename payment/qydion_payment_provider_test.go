package payment

// QYDION 渠道协议回归：出站请求必须带上本订单的异步回调地址并正确签名，
// 同步响应与平台证书引导都必须验签；冷启动不得递归下载证书。
// 全部 HTTP 走内存替身，不向任何真实网关发起请求。

import (
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// qydionPaymentOptions points the providers at an isolated in-memory options
// table, the only way后台选项 reaches payment without a full gateway boot.
func qydionPaymentOptions(t *testing.T, options map[string]string) {
	t.Helper()
	oldDB := model.DB
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&model.Option{}))
	for k, v := range options {
		require.NoError(t, db.Create(&model.Option{Key: k, Value: v}).Error)
	}
	model.DB = db
	service.QydionSettingsBustCache()
	t.Cleanup(func() {
		model.DB = oldDB
		service.QydionSettingsBustCache()
		_ = sqlDB.Close()
	})
}

type recordingTransport struct {
	handler func(*http.Request) (*http.Response, error)
	forms   []url.Values
}

func (rt *recordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.PostForm != nil || r.Method == http.MethodPost {
		if err := r.ParseForm(); err == nil && len(r.PostForm) > 0 {
			rt.forms = append(rt.forms, r.PostForm)
		}
	}
	return rt.handler(r)
}

func useTransport(t *testing.T, rt *recordingTransport) {
	t.Helper()
	old := http.DefaultTransport
	http.DefaultTransport = rt
	t.Cleanup(func() { http.DefaultTransport = old })
}

// alipayFormSignSource 按「出站请求规范」重建待签串：含 sign_type，排除 sign 与空值。
func alipayFormSignSource(form url.Values) string {
	keys := make([]string, 0, len(form))
	for k := range form {
		if k == "sign" || form.Get(k) == "" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+form.Get(k))
	}
	return strings.Join(parts, "&")
}

func TestAlipayCreatePaymentSendsNotifyURLAndVerifiesResponse(t *testing.T) {
	key, privPEM, pubPEM := testAlipayKeys(t)
	qydionPaymentOptions(t, map[string]string{
		"QydionAlipayAppId":      "qydion-app",
		"QydionAlipayPrivateKey": privPEM,
		"QydionAlipayPublicKey":  pubPEM,
	})

	const notifyURL = "https://api.qydion.cn/api/qydion/payment/notify/alipay_f2f"
	const node = `{"code":"10000","msg":"Success","qr_code":"https://qr.alipay.com/synthetic"}`
	nodeDigest := sha256.Sum256([]byte(node))
	signedNode, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, nodeDigest[:])
	require.NoError(t, err)
	signedBody := `{"alipay_trade_precreate_response":` + node +
		`,"sign":"` + base64.StdEncoding.EncodeToString(signedNode) + `"}`

	rt := &recordingTransport{handler: func(r *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, signedBody), nil
	}}
	useTransport(t, rt)

	req, err := (&AlipayF2FProvider{}).CreatePayment(Order{
		TradeNo: "QYD1790000000000123456", Money: 90, Subject: "QYDION 充值", NotifyURL: notifyURL,
	})
	require.NoError(t, err)
	assert.Equal(t, "https://qr.alipay.com/synthetic", req.QrContent)

	require.Len(t, rt.forms, 1)
	sent := rt.forms[0]

	// notify_url 是公共参数：控制器传了就必须出现在发给网关的表单里，
	// 否则这笔订单从未把回调地址告诉过渠道。
	assert.Equal(t, notifyURL, sent.Get("notify_url"))

	// 独立按请求侧规范验自己发出去的表单。用同一份实现自签自验看不见这个错误。
	requestDigest := sha256.Sum256([]byte(alipayFormSignSource(sent)))
	sig, err := base64.StdEncoding.DecodeString(sent.Get("sign"))
	require.NoError(t, err)
	require.NoError(t, rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, requestDigest[:], sig),
		"出站表单签名未按请求侧规范覆盖 sign_type")
}

func TestAlipayCreatePaymentRejectsUnsignedResponse(t *testing.T) {
	_, privPEM, pubPEM := testAlipayKeys(t)
	qydionPaymentOptions(t, map[string]string{
		"QydionAlipayAppId":      "qydion-app",
		"QydionAlipayPrivateKey": privPEM,
		"QydionAlipayPublicKey":  pubPEM,
	})
	useTransport(t, &recordingTransport{handler: func(r *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK,
			`{"alipay_trade_precreate_response":{"code":"10000","qr_code":"https://qr.alipay.com/forged"}}`), nil
	}})

	_, err := (&AlipayF2FProvider{}).CreatePayment(Order{TradeNo: "QYD1", Money: 1, Subject: "s", NotifyURL: "https://api.qydion.cn/n"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "未签名")
}

func TestAlipayCreatePaymentReportsGatewayError(t *testing.T) {
	_, privPEM, pubPEM := testAlipayKeys(t)
	qydionPaymentOptions(t, map[string]string{
		"QydionAlipayAppId":      "qydion-app",
		"QydionAlipayPrivateKey": privPEM,
		"QydionAlipayPublicKey":  pubPEM,
	})
	useTransport(t, &recordingTransport{handler: func(r *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK,
			`{"error_response":{"code":"40002","msg":"Invalid arguments","sub_msg":"签名格式不正确"}}`), nil
	}})

	_, err := (&AlipayF2FProvider{}).CreatePayment(Order{TradeNo: "QYD1", Money: 1, Subject: "s", NotifyURL: "https://api.qydion.cn/n"})
	require.ErrorContains(t, err, "40002")
}



func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// ---------- 微信 APIv3 平台证书引导 ----------

// wxTestTimestamp 必须落在回调验签的 ±5 分钟时间窗内，所以取当前时间。
var wxTestTimestamp = strconv.FormatInt(time.Now().Unix(), 10)

const wxTestSerial = "0123456789ABCDEF0123456789ABCDEF01"
const wxTestApiV3Key = "0123456789abcdef0123456789abcdef"

func wxTestPlatformCertificate(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	serial := new(big.Int)
	serial, ok := serial.SetString(wxTestSerial, 16)
	require.True(t, ok)
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "Tenpay.com Authentication"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	return key, der
}

// wxTestEncrypt 模拟微信侧的 APIv3 加密：nonce 在真实应答里是 12 个可打印字符，
// 随机字节过 JSON 会被替换字符改写，测试必须用 ASCII nonce。
func wxTestEncrypt(t *testing.T, apiV3Key, adata string, plain []byte) (nonce string, ciphertextB64 string) {
	t.Helper()
	block, err := aes.NewCipher([]byte(apiV3Key))
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)
	nonce = "wxtestnonce1"
	sealed := gcm.Seal(nil, []byte(nonce), plain, []byte(adata))
	return nonce, base64.StdEncoding.EncodeToString(sealed)
}

// wxTestSigned 用平台私钥按 APIv3 规范给应答签名，并带上证书序列号头。
func wxTestSigned(t *testing.T, key *rsa.PrivateKey, serial, body string) *http.Response {
	t.Helper()
	nonce := base64.RawStdEncoding.EncodeToString(make([]byte, 16))
	digest := sha256.Sum256([]byte(wxTestTimestamp + "\n" + nonce + "\n" + body + "\n"))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	require.NoError(t, err)
	resp := jsonResponse(http.StatusOK, body)
	resp.Header.Set("Wechatpay-Serial", serial)
	resp.Header.Set("Wechatpay-Timestamp", wxTestTimestamp)
	resp.Header.Set("Wechatpay-Nonce", nonce)
	resp.Header.Set("Wechatpay-Signature", base64.StdEncoding.EncodeToString(sig))
	return resp
}

func wxTestPaymentBody() string {
	return `{"code_url":"weixin://wxpay/synthetic"}`
}

func wxTestCertBody(t *testing.T, der []byte) string {
	t.Helper()
	nonce, ciphertext := wxTestEncrypt(t, wxTestApiV3Key, "certificate", der)
	payload := map[string]any{
		"data": []any{map[string]any{
			"serial_no": wxTestSerial,
			"effective_time": time.Now().Add(-time.Hour).Format(time.RFC3339),
			"encrypt_certificate": map[string]any{
				"algorithm": "aes-256-gcm", "nonce": nonce,
				"associated_data": "certificate", "ciphertext": ciphertext,
			},
		}},
	}
	raw, err := common.Marshal(payload)
	require.NoError(t, err)
	return string(raw)
}

func wxTestOptions(t *testing.T, merchantKey *rsa.PrivateKey) {
	t.Helper()
	qydionPaymentOptions(t, map[string]string{
		"QydionWxpayAppId":     "qydion-app",
		"QydionWxpayMchId":     "qydion-mch",
		"QydionWxpayMchSerial": "merchant-serial",
		"QydionWxpayPrivateKey": base64.StdEncoding.EncodeToString(
			x509.MarshalPKCS1PrivateKey(merchantKey)),
		"QydionWxpayApiV3Key": wxTestApiV3Key,
	})
}

// TestWxColdStartDownloadsPlatformCertificatesOnce is the regression for the
// bootstrap recursion: verifying any response needs the platform key, the
// platform key came from a response that was itself verified the same way, so a
// cold start kept fetching /v3/certificates until something gave up.
func TestWxColdStartDownloadsPlatformCertificatesOnce(t *testing.T) {
	merchantKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	platformKey, der := wxTestPlatformCertificate(t)
	wxTestOptions(t, merchantKey)

	certCalls, payCalls := 0, 0
	rt := &recordingTransport{handler: func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v3/certificates":
			certCalls++
			return wxTestSigned(t, platformKey, wxTestSerial, wxTestCertBody(t, der)), nil
		case "/v3/pay/transactions/native":
			payCalls++
			return wxTestSigned(t, platformKey, wxTestSerial, wxTestPaymentBody()), nil
		default:
			return nil, errors.New("synthetic transport received an unexpected path: " + r.URL.Path)
		}
	}}
	useTransport(t, rt)

	req, err := (&WxNativeProvider{}).CreatePayment(Order{
		TradeNo: "QYD1790000000000123456", Money: 10, Subject: "QYDION 充值",
		NotifyURL: "https://api.qydion.cn/api/qydion/payment/notify/wx_native",
	})
	require.NoError(t, err, "cold start must bootstrap the platform certificate without recursion")
	assert.Equal(t, "weixin://wxpay/synthetic", req.QrContent)
	assert.Equal(t, 1, certCalls, "证书引导只能下载一次")
	assert.Equal(t, 1, payCalls)

	// 下单请求体带上本订单的回调地址，且由商户私钥签名。
}

// TestWxUnknownSerialDoesNotHamperCertificateEndpoint covers rotation and
// spoofing: an unknown serial may trigger one refresh, not a download per
// request.
func TestWxUnknownSerialDoesNotHamperCertificateEndpoint(t *testing.T) {
	merchantKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	platformKey, der := wxTestPlatformCertificate(t)
	wxTestOptions(t, merchantKey)

	certCalls := 0
	rt := &recordingTransport{handler: func(r *http.Request) (*http.Response, error) {
		certCalls++
		return wxTestSigned(t, platformKey, wxTestSerial, wxTestCertBody(t, der)), nil
	}}
	useTransport(t, rt)

	provider := &WxNativeProvider{}
	for range 5 {
		_, err := provider.platformPublicKey("ffffffffffffffff")
		require.Error(t, err, "未知序列号不得返回一个公钥")
	}
	assert.Equal(t, 1, certCalls, "冷却期内未知序列号不得反复拉取证书接口")

	known, err := provider.platformPublicKey(wxTestSerial)
	require.NoError(t, err)
	assert.Equal(t, &platformKey.PublicKey, known)
	assert.Equal(t, 1, certCalls)
}

func TestWxRejectsUnsignedCertificateResponse(t *testing.T) {
	merchantKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	_, der := wxTestPlatformCertificate(t)
	wxTestOptions(t, merchantKey)

	certCalls := 0
	rt := &recordingTransport{handler: func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v3/certificates" {
			certCalls++
			return jsonResponse(http.StatusOK, wxTestCertBody(t, der)), nil
		}
		return jsonResponse(http.StatusOK, wxTestPaymentBody()), nil
	}}
	useTransport(t, rt)

	_, err = (&WxNativeProvider{}).CreatePayment(Order{
		TradeNo: "QYD1", Money: 1, Subject: "s",
		NotifyURL: "https://api.qydion.cn/api/qydion/payment/notify/wx_native",
	})
	require.ErrorContains(t, err, "签名头缺失")
	assert.Equal(t, 0, certCalls, "未签名的下单应答不得放行，也不需要拉证书")
}

// TestWxRejectsUnsignedCertificateList 单独覆盖证书引导自身的可信校验：
// 一份没有签名的证书列表等于让任何能改写响应的人决定「谁的身份算微信」。
func TestWxRejectsUnsignedCertificateList(t *testing.T) {
	merchantKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	_, der := wxTestPlatformCertificate(t)
	wxTestOptions(t, merchantKey)

	certCalls := 0
	useTransport(t, &recordingTransport{handler: func(r *http.Request) (*http.Response, error) {
		certCalls++
		return jsonResponse(http.StatusOK, wxTestCertBody(t, der)), nil
	}})

	_, err = (&WxNativeProvider{}).platformPublicKey(wxTestSerial)
	require.ErrorContains(t, err, "未签名")
	assert.Equal(t, 1, certCalls, "验签失败后不得递归重试下载")
}

func TestWxCallbackRejectsSerialOutsidePlatformList(t *testing.T) {
	merchantKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	platformKey, der := wxTestPlatformCertificate(t)
	wxTestOptions(t, merchantKey)

	rt := &recordingTransport{handler: func(r *http.Request) (*http.Response, error) {
		return wxTestSigned(t, platformKey, wxTestSerial, wxTestCertBody(t, der)), nil
	}}
	useTransport(t, rt)

	provider := &WxNativeProvider{}
	// 先用真证书填缓存，再用一把「序列号不在列表中」的伪造签名头走回调。
	_, err = provider.platformPublicKey(wxTestSerial)
	require.NoError(t, err)

	rogue, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	rogueSerial := new(big.Int).SetBytes([]byte("rogue-serial"))
	body := `{"resource":{}}`
	nonce := "0000000000000000"
	digest := sha256.Sum256([]byte("1700000000\n" + nonce + "\n" + body + "\n"))
	sig, err := rsa.SignPKCS1v15(rand.Reader, rogue, crypto.SHA256, digest[:])
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, "/api/qydion/payment/notify/wx_native", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Wechatpay-Serial", rogueSerial.Text(16))
	req.Header.Set("Wechatpay-Timestamp", wxTestTimestamp)
	req.Header.Set("Wechatpay-Nonce", nonce)
	req.Header.Set("Wechatpay-Signature", base64.StdEncoding.EncodeToString(sig))

	_, err = provider.HandleCallback(req)
	require.Error(t, err, "序列号不在平台证书列表中的回调必须拒绝")
	assert.Contains(t, err.Error(), "不在平台证书列表中")
}
