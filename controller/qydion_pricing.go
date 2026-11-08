package controller

// QYDION 价格表批量导入/导出（§8.4）
// - 列：模型名,输入价(元/百万tokens),输出价(元/百万tokens),计费方式(token|per_call)
// - 换算公式（已按源码核实，见 notes/REPO-MAP.md §4.2）：
//     QuotaPerUnit = 500000 quota = $1（common/constants.go:22，即 ratio=1 时 1M tokens=$2）
//     modelRatio      = 输入价(CNY/百万) ÷ (2 × Price)
//     completionRatio = 输出价 ÷ 输入价（同币种口径相除）
//     per_call：ModelPrice(USD/次) = 每次价格(CNY) ÷ Price
// - 导入前自动备份当前倍率 JSON 到 config/pricing-history/（带时间戳，§8.4）
// - 写入走 model.UpdateOption（原生持久化 + 热加载路径，运行时立即生效）

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

type qydionPricingMaps struct {
	ModelRatio      map[string]float64 `json:"ModelRatio"`
	CompletionRatio map[string]float64 `json:"CompletionRatio"`
	ModelPrice      map[string]float64 `json:"ModelPrice"`
	ExportedAt      string             `json:"exported_at"`
}

func qydionLoadPricingMaps() (*qydionPricingMaps, error) {
	snapshot := &qydionPricingMaps{
		ModelRatio:      map[string]float64{},
		CompletionRatio: map[string]float64{},
		ModelPrice:      map[string]float64{},
		ExportedAt:      time.Now().Format(time.RFC3339),
	}
	decode := func(raw string, target *map[string]float64) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return
		}
		_ = json.Unmarshal([]byte(raw), target)
	}
	decode(ratio_setting.ModelRatio2JSONString(), &snapshot.ModelRatio)
	decode(ratio_setting.CompletionRatio2JSONString(), &snapshot.CompletionRatio)
	decode(ratio_setting.ModelPrice2JSONString(), &snapshot.ModelPrice)
	return snapshot, nil
}

// QydionExportPricingCSV 管理侧：导出 CSV
func QydionExportPricingCSV(c *gin.Context) {
	snapshot, err := qydionLoadPricingMaps()
	if err != nil {
		common.ApiErrorMsg(c, "倍率读取失败")
		return
	}
	price := operation_setting.Price
	var sb strings.Builder
	sb.WriteString("模型名,输入价(元/百万tokens),输出价(元/百万tokens),计费方式\n")
	seen := map[string]bool{}
	writeRow := func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		if p, ok := snapshot.ModelPrice[name]; ok && p > 0 {
			cnyPerCall := decimal.NewFromFloat(p).Mul(decimal.NewFromFloat(price))
			sb.WriteString(fmt.Sprintf("%s,,%.4f,per_call\n", name, cnyPerCall.InexactFloat64()))
			return
		}
		r, ok := snapshot.ModelRatio[name]
		if !ok {
			return
		}
		inputCNY := decimal.NewFromFloat(r).Mul(decimal.NewFromFloat(2 * price))
		outputRatio := snapshot.CompletionRatio[name]
		if outputRatio == 0 {
			outputRatio = 1
		}
		outputCNY := inputCNY.Mul(decimal.NewFromFloat(outputRatio))
		sb.WriteString(fmt.Sprintf("%s,%.4f,%.4f,token\n", name, inputCNY.InexactFloat64(), outputCNY.InexactFloat64()))
	}
	for name := range snapshot.ModelRatio {
		writeRow(qydionCsvSafe(name)) // 全量安全审计 P2：CSV 公式注入防护
	}
	for name := range snapshot.ModelPrice {
		writeRow(qydionCsvSafe(name))
	}
	c.Header("Content-Disposition", "attachment; filename=qydion-pricing-"+time.Now().Format("20060102")+".csv")
	c.Data(200, "text/csv; charset=utf-8", []byte(sb.String()))
}

// QydionImportPricingCSV 管理侧：导入 CSV → 备份 → 换算 → UpdateOption
func QydionImportPricingCSV(c *gin.Context) {
	fileHeader, err := c.FormFile("file")
	if err != nil {
		common.ApiErrorMsg(c, "请上传 CSV 文件")
		return
	}
	// 全量安全审计 P3：导入上限（≤2MB / ≤5000 行），防管理端超大文件全量载入内存
	if fileHeader.Size > 2*1024*1024 {
		common.ApiErrorMsg(c, "CSV 文件不能超过 2MB")
		return
	}
	f, err := fileHeader.Open()
	if err != nil {
		common.ApiErrorMsg(c, "文件读取失败")
		return
	}
	defer f.Close()

	reader := csv.NewReader(f)
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	if err != nil {
		common.SysError(common.LogText("QYDION 定价 CSV 解析失败: %v", err))
		common.ApiErrorMsg(c, "CSV 解析失败，请检查文件格式")
		return
	}
	if len(records) < 2 {
		common.ApiErrorMsg(c, "CSV 至少要有表头和一行数据")
		return
	}
	if len(records) > 5001 {
		common.ApiErrorMsg(c, "CSV 行数超限（最多 5000 行数据）")
		return
	}

	price := operation_setting.Price
	if price <= 0 {
		common.ApiErrorMsg(c, "充值单价（Price）未配置，无法换算")
		return
	}

	// 1. 导入前备份（§8.4）
	backupDir := filepath.Join("config", "pricing-history")
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		common.ApiErrorMsg(c, "备份目录创建失败")
		return
	}
	snapshot, _ := json.MarshalIndent(qydionLoadPricingMapsOrEmpty(), "", "  ")
	backupPath := filepath.Join(backupDir, fmt.Sprintf("pricing-%s.json", time.Now().Format("20060102-150405")))
	if err := os.WriteFile(backupPath, snapshot, 0644); err != nil {
		common.SysError(common.LogText("QYDION 倍率备份失败: %v", err))
		common.ApiErrorMsg(c, "倍率备份失败")
		return
	}

	// 2. 基于当前值解析并换算
	current, err := qydionLoadPricingMaps()
	if err != nil {
		common.ApiErrorMsg(c, "当前倍率读取失败")
		return
	}
	twoPriceCNY := decimal.NewFromFloat(2 * price)
	updated := 0
	skipped := []string{}
	for i, rec := range records {
		if i == 0 {
			continue
		}
		if len(rec) < 4 {
			continue
		}
		name := strings.TrimSpace(rec[0])
		if name == "" || strings.HasPrefix(name, "#") {
			continue
		}
		inputStr := strings.TrimSpace(rec[1])
		outputStr := strings.TrimSpace(rec[2])
		mode := strings.TrimSpace(strings.ToLower(rec[3]))

		switch mode {
		case "per_call":
			callCNY, err := strconv.ParseFloat(outputStr, 64)
			if err != nil || callCNY <= 0 {
				skipped = append(skipped, fmt.Sprintf("%s(每次价格无效)", name))
				continue
			}
			current.ModelPrice[name] = decimal.NewFromFloat(callCNY).Div(decimal.NewFromFloat(price)).InexactFloat64()
			delete(current.ModelRatio, name)
			delete(current.CompletionRatio, name)
		default:
			inputCNY, err1 := strconv.ParseFloat(inputStr, 64)
			outputCNY, err2 := strconv.ParseFloat(outputStr, 64)
			if err1 != nil || err2 != nil || inputCNY <= 0 || outputCNY <= 0 {
				skipped = append(skipped, fmt.Sprintf("%s(价格无效)", name))
				continue
			}
			current.ModelRatio[name] = decimal.NewFromFloat(inputCNY).Div(twoPriceCNY).InexactFloat64()
			current.CompletionRatio[name] = decimal.NewFromFloat(outputCNY).Div(decimal.NewFromFloat(inputCNY)).InexactFloat64()
			delete(current.ModelPrice, name)
		}
		updated++
	}

	// 3. 落库（UpdateOption = 持久化 + 热加载）
	if err := saveRatioOption("ModelRatio", current.ModelRatio); err != nil {
		common.SysError(common.LogText("QYDION ModelRatio 保存失败: %v", err))
		common.ApiErrorMsg(c, "ModelRatio 保存失败")
		return
	}
	if err := saveRatioOption("CompletionRatio", current.CompletionRatio); err != nil {
		common.SysError(common.LogText("QYDION CompletionRatio 保存失败: %v", err))
		common.ApiErrorMsg(c, "CompletionRatio 保存失败")
		return
	}
	if err := saveRatioOption("ModelPrice", current.ModelPrice); err != nil {
		common.SysError(common.LogText("QYDION ModelPrice 保存失败: %v", err))
		common.ApiErrorMsg(c, "ModelPrice 保存失败")
		return
	}
	common.ApiSuccess(c, gin.H{
		"updated": updated,
		"skipped": skipped,
		// 全量安全审计 P3：不回显服务器路径，只回文件名
		"backup": filepath.Base(backupPath),
	})
}

func saveRatioOption(key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return model.UpdateOption(key, string(raw))
}

func qydionLoadPricingMapsOrEmpty() *qydionPricingMaps {
	m, _ := qydionLoadPricingMaps()
	if m == nil {
		return &qydionPricingMaps{
			ModelRatio: map[string]float64{}, CompletionRatio: map[string]float64{},
			ModelPrice: map[string]float64{}, ExportedAt: time.Now().Format(time.RFC3339),
		}
	}
	return m
}
