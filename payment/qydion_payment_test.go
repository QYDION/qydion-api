package payment

// QYDION M6 支付单元测试：RSA2 签名/验签闭环（测试密钥对，模拟支付宝侧）

import (
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v81/webhook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAlipayRequestSignatureCoversSignType pins the two different Alipay
// signing rules. The outbound request signs over sign_type; the asynchronous
// notification verifies excluding it. Sharing one implementation signs a field
// set the gateway does not expect, and a test that signs and verifies with the
// same wrong rule cannot see it.
func TestAlipayRequestSignatureCoversSignType(t *testing.T) {
	key, privPEM, _ := testAlipayKeys(t)

	pub := map[string]string{
		"app_id":      "2026000000000000",
		"method":      "alipay.trade.precreate",
		"charset":     "utf-8",
		"sign_type":   "RSA2",
		"biz_content": `{"out_trade_no":"CORP1","total_amount":"90.00"}`,
		"empty":       "",
	}
	sig, err := AlipaySign(pub, privPEM)
	require.NoError(t, err)

	// 独立按「请求侧规范」重算：含 sign_type、排除 sign 与空值
	sorted := []string{"app_id=2026000000000000", "biz_content=" + pub["biz_content"],
		"charset=utf-8", "method=alipay.trade.precreate", "sign_type=RSA2"}
	digest := sha256.Sum256([]byte(strings.Join(sorted, "&")))
	sigBytes, err := base64.StdEncoding.DecodeString(sig)
	require.NoError(t, err)
	require.NoError(t, rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sigBytes),
		"出站签名必须覆盖 sign_type")

	// 同一份签名按「通知侧规范」重算必须失败，否则两条规则其实没分家。
	notifyOnly := strings.Join([]string{"app_id=2026000000000000", "biz_content=" + pub["biz_content"],
		"charset=utf-8", "method=alipay.trade.precreate"}, "&")
	notifyDigest := sha256.Sum256([]byte(notifyOnly))
	assert.Error(t, rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, notifyDigest[:], sigBytes))
}

// TestAlipayNotifyVerificationExcludesSignType checks the callback rule the
// other way round: a notification signed by Alipay (which leaves sign_type out
// of the signed text) must verify, and the same fields signed *with* sign_type
// must not.
func TestAlipayNotifyVerificationExcludesSignType(t *testing.T) {
	key, _, pubPEM := testAlipayKeys(t)

	params := map[string]string{
		"app_id":       "2026000000000000",
		"notify_id":    "w1w2w3",
		"out_trade_no": "CORP1",
		"trade_status": "TRADE_SUCCESS",
		"sign_type":    "RSA2",
		"empty":        "",
	}
	signed := strings.Join([]string{"app_id=2026000000000000", "notify_id=w1w2w3",
		"out_trade_no=CORP1", "trade_status=TRADE_SUCCESS"}, "&")
	digest := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	require.NoError(t, err)

	require.NoError(t, AlipayVerify(params, base64.StdEncoding.EncodeToString(sig), pubPEM))

	withSignType := sha256.Sum256([]byte(signed + "&sign_type=RSA2"))
	badSig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, withSignType[:])
	require.NoError(t, err)
	assert.Error(t, AlipayVerify(params, base64.StdEncoding.EncodeToString(badSig), pubPEM),
		"通知验签不得把 sign_type 算进待签串")

	tampered := map[string]string{}
	for k, v := range params {
		tampered[k] = v
	}
	tampered["trade_status"] = "TRADE_FINISHED"
	assert.Error(t, AlipayVerify(tampered, base64.StdEncoding.EncodeToString(sig), pubPEM))
}

func testAlipayKeys(t *testing.T) (*rsa.PrivateKey, string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	privPEM := base64.StdEncoding.EncodeToString(x509.MarshalPKCS1PrivateKey(key))
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	require.NoError(t, err)
	// 支付宝惯例：裸 base64，无 PEM 头尾
	return key, privPEM, base64.StdEncoding.EncodeToString(pubDER)
}

func TestPemDecodeBareBase64(t *testing.T) {
	der := []byte{0x30, 0x01, 0x02, 0x03}
	b64 := base64.StdEncoding.EncodeToString(der)
	got, err := pemDecode(b64)
	if err != nil {
		t.Fatalf("裸 base64 解码失败: %v", err)
	}
	if len(got) != len(der) {
		t.Errorf("解码长度不符")
	}
	wrapped := "-----BEGIN PRIVATE KEY-----\n" + b64 + "\n-----END PRIVATE KEY-----"
	got2, err := pemDecode(wrapped)
	if err != nil || len(got2) != len(der) {
		t.Fatalf("PEM 头尾剥离失败")
	}
	_ = pem.EncodeToMemory // 保留 import
}

// ---------- M7：微信 APIv3 回调解密 / Stripe webhook 验签 ----------

func TestWxDecryptResourceRoundTrip(t *testing.T) {
	key := "0123456789abcdef0123456789abcdef" // 32 字节
	nonce := "wxb4f7c1e9a2" // 微信 APIv3 规范 12 字节
	adata := "transaction"
	plain := []byte(`{"out_trade_no":"QYD1790000000123456","trade_state":"SUCCESS","amount":{"total":100,"currency":"CNY"}}`)
	// AES-256-GCM 加密（模拟微信侧）
	block, _ := aes.NewCipher([]byte(key))
	gcm, _ := cipher.NewGCM(block)
	ct := gcm.Seal(nil, []byte(nonce), plain, []byte(adata))
	got, err := wxDecryptResource(key, nonce, adata, base64.StdEncoding.EncodeToString(ct))
	if err != nil {
		t.Fatalf("解密失败: %v", err)
	}
	if string(got) != string(plain) {
		t.Errorf("解密结果不一致")
	}
	// 篡改密文必须失败
	ct[len(ct)-1] ^= 0xFF
	if _, err := wxDecryptResource(key, nonce, adata, base64.StdEncoding.EncodeToString(ct)); err == nil {
		t.Errorf("篡改密文应解密失败")
	}
	// 错误密钥长度
	if _, err := wxDecryptResource("short", nonce, adata, ""); err == nil {
		t.Errorf("非法密钥长度应报错")
	}
}

func TestStripeWebhookVerify(t *testing.T) {
	secret := "whsec_test_123"
	payload := `{"id":"evt_1","object":"event","type":"checkout.session.completed","data":{"object":{"id":"cs_test","client_reference_id":"QYD1790000000654321","payment_status":"paid","currency":"usd","amount_total":1370}}}`
	ts := fmt.Sprintf("%d", time.Now().Unix())
	// 官方签名格式：t=ts,v1=hex(HMAC-SHA256(secret, ts+"."+payload))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + payload))
	sig := hex.EncodeToString(mac.Sum(nil))
	req, _ := http.NewRequest("POST", "/notify", strings.NewReader(payload))
	req.Header.Set("Stripe-Signature", "t="+ts+",v1="+sig)
	// 直接验证签名层（不经过 Enabled 门控的 HandleCallback，因其读全局选项）
	_, err := webhook.ConstructEventWithOptions([]byte(payload), req.Header.Get("Stripe-Signature"), secret, webhook.ConstructEventOptions{IgnoreAPIVersionMismatch: true})
	if err != nil {
		t.Fatalf("合法签名验证失败: %v", err)
	}
	// 篡改 payload 必须失败
	_, err = webhook.ConstructEventWithOptions([]byte(payload+" "), req.Header.Get("Stripe-Signature"), secret, webhook.ConstructEventOptions{IgnoreAPIVersionMismatch: true})
	if err == nil {
		t.Errorf("篡改负载应验签失败")
	}
}

func TestStripeMinorUnitsMath(t *testing.T) {
	// Price=7.3：¥100 → $13.70 → 1370 美分；回折 1370 美分 → $13.70 → ¥100.01（容差内）
	units, currency, err := stripeMinorUnits(100)
	if err != nil || currency != "usd" || units != 1370 {
		t.Errorf("¥100 应=1370 usd 分，实际 %d %s err=%v", units, currency, err)
	}
	back := minorUnitsToCNY(units, currency)
	if math.Abs(back-100) > 0.05 {
		t.Errorf("回折误差超容差：%.4f", back)
	}
	if u, c, err := stripeMinorUnits(7.3); err != nil || c != "usd" || u != 100 {
		t.Errorf("¥7.3 应=100 usd 分，实际 %d %s err=%v", u, c, err)
	}
}
