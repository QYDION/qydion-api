package controller

// QYDION 模型维护扩展（§8.2b）：原生 models 表已有 description/tags/status/名称规则，
// 本模块只补 QYDION 需要的三个字段：display_name / sort_order / pricing_note（伴生扩展表）。

import (
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

// QydionListModelExtList 维护页数据：渠道自动获取的模型 ∪ 原生 Model 元数据 ∪ 扩展字段
func QydionListModelExtList(c *gin.Context) {
	// 1. 渠道已配置的模型（含禁用渠道）
	configured, err := model.GetConfiguredModelChannels()
	if err != nil {
		common.ApiErrorMsg(c, "渠道模型读取失败："+err.Error())
		return
	}
	// 2. 原生元数据
	var metas []model.Model
	if err := model.DB.Find(&metas).Error; err != nil {
		common.ApiErrorMsg(c, "模型元数据读取失败")
		return
	}
	// 3. QYDION 扩展
	exts, err := model.GetQydionModelExtList()
	if err != nil {
		common.ApiErrorMsg(c, "扩展字段读取失败")
		return
	}
	extByName := map[string]model.QydionModelExt{}
	for _, e := range exts {
		extByName[e.ModelName] = e
	}

	seen := map[string]bool{}
	rows := make([]gin.H, 0, len(configured)+len(metas))
	addRow := func(name string, hasMeta bool, meta *model.Model) {
		if seen[name] {
			return
		}
		seen[name] = true
		ext := extByName[name]
		row := gin.H{
			"model_name":   name,
			"channel_count": len(configured[name]),
			"has_metadata": hasMeta,
			"display_name": ext.DisplayName,
			"sort_order":   ext.SortOrder,
			"pricing_note": ext.PricingNote,
			"ext_id":       ext.Id,
		}
		if meta != nil {
			row["description"] = meta.Description
			row["tags"] = meta.Tags
			row["status"] = meta.Status
			row["vendor_id"] = meta.VendorID
		}
		rows = append(rows, row)
	}
	for i := range metas {
		addRow(metas[i].ModelName, true, &metas[i])
	}
	for name := range configured {
		addRow(name, false, nil)
	}
	common.ApiSuccess(c, rows)
}

// QydionSaveModelExt 保存扩展字段（按 model_name upsert）
func QydionSaveModelExt(c *gin.Context) {
	var req model.QydionModelExt
	if err := c.ShouldBindJSON(&req); err != nil || req.ModelName == "" {
		common.ApiErrorMsg(c, "参数无效（需要 model_name）")
		return
	}
	// 查已有记录以保持 Id（实现覆盖式更新）
	var existing model.QydionModelExt
	if err := model.DB.Where("model_name = ?", req.ModelName).First(&existing).Error; err == nil {
		req.Id = existing.Id
	}
	if err := model.UpsertQydionModelExt(&req); err != nil {
		common.ApiErrorMsg(c, "保存失败："+err.Error())
		return
	}
	common.ApiSuccess(c, req)
}

// QydionDeleteModelExt 删除扩展字段
func QydionDeleteModelExt(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		common.ApiErrorMsg(c, "id 无效")
		return
	}
	if err := model.DeleteQydionModelExt(id); err != nil {
		common.ApiErrorMsg(c, "删除失败")
		return
	}
	common.ApiSuccess(c, nil)
}
