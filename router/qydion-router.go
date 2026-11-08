package router

// QYDION 二开路由（M4）：全部挂 /api/qydion/*，管理接口 AdminAuth，用户接口 UserAuth。
// 在 api-router.go 末尾调用 SetQydionRoutes(apiRouter)（原生触点 1 行）。

import (
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/gin-gonic/gin"
)

func SetQydionRoutes(apiRouter *gin.RouterGroup) {
	qydion := apiRouter.Group("/qydion")
	qydion.Use(middleware.RouteTag("qydion"))
	{
		// ---- 用户侧 ----
		user := qydion.Group("", middleware.UserAuth())
		{
			user.GET("/corporate/account", controller.QydionGetCorporateAccount)
			user.POST("/corporate/topup", controller.QydionCreateCorporateTopup)
			user.GET("/corporate/topups", controller.QydionListMyCorporateTopups)
			user.POST("/coupon/validate", controller.QydionValidateCoupon)
			user.GET("/topup-tiers", controller.QydionActiveTopupTiers)
			// M7 在线支付（§8.6b）
			user.GET("/payment/methods", controller.QydionListPaymentMethods)
			user.POST("/payment/create", controller.QydionCreateOnlineTopup)
			user.GET("/payment/status", controller.QydionQueryOnlineTopup)
			// 退款管理（交接报告 §E）：用户侧查本人退款记录
			user.GET("/refund/my", controller.QydionListMyRefunds)
		}

		// ---- 渠道异步回调（公开；安全完全依赖渠道层验签 §10.4）----
		qydion.POST("/payment/notify/:provider", controller.QydionPaymentNotify)

		// ---- 管理侧 ----
		admin := qydion.Group("", middleware.AdminAuth())
		{
			// 模型维护扩展（§8.2b）
			admin.GET("/model-ext", controller.QydionListModelExtList)
			admin.POST("/model-ext", controller.QydionSaveModelExt)
			admin.DELETE("/model-ext/:id", controller.QydionDeleteModelExt)

			// 价格表 CSV（§8.4）
			admin.GET("/pricing/export", controller.QydionExportPricingCSV)
			admin.POST("/pricing/import", controller.QydionImportPricingCSV)

			// 促销系统（§8.7）
			admin.GET("/campaigns", controller.QydionListCampaigns)
			admin.POST("/campaigns", controller.QydionSaveCampaign)
			admin.POST("/campaigns/status", controller.QydionSetCampaignStatus)
			admin.GET("/coupons", controller.QydionListCoupons)
			admin.POST("/coupons/generate", controller.QydionGenerateCouponBatch)
			admin.GET("/coupons/export", controller.QydionExportCouponBatch)
			admin.POST("/coupons/void", controller.QydionVoidCoupon)

			// 对公充值审核（§8.6c）
			admin.GET("/topup/review-queue", controller.QydionListReviewQueue)
			admin.POST("/topup/review", controller.QydionReviewTopup)
			admin.GET("/voucher/:trade_no", controller.QydionDownloadVoucher)
			// 已关单订单凭渠道查证补入账（M7 §8.6a）：回调丢失且订单已被关成 cancelled 时，
			// 原生 /api/topup/complete 只受理 pending，没有这个入口就只能手工改库。
			admin.POST("/topup/force-settle", controller.QydionForceSettleCancelledTopup)

			// 退款管理（交接报告 §E）：创建退款（审核即执行）/ 列表 / 悬挂单关闭兜底
			admin.POST("/refund/create", controller.QydionCreateRefund)
			admin.GET("/refund/list", controller.QydionListRefunds)
			admin.POST("/refund/close", controller.QydionCloseRefund)

			// 支付渠道（§8.6b）
			admin.GET("/payment-providers", controller.QydionListPaymentProviders)
			// M7 支付设置（填参数即启用；密钥掩码读写）
			admin.GET("/payment/settings", controller.QydionGetPaymentSettings)
			admin.POST("/payment/settings", controller.QydionSavePaymentSettings)

			// 对账（§8.5c）
			admin.POST("/reconciliation/run", controller.QydionRunReconciliationAPI)
			admin.GET("/reconciliation", controller.QydionListReconciliation)

			// 统计（§8.8）
			admin.GET("/stats/cards", controller.QydionDashboardCards)
			admin.GET("/stats/trend", controller.QydionTrend)
			admin.GET("/stats/by-user", controller.QydionBreakdownByUser)
			admin.GET("/stats/by-model", controller.QydionBreakdownByModel)
			admin.GET("/stats/by-channel", controller.QydionBreakdownByChannel)
			admin.GET("/stats/by-day", controller.QydionBreakdownByDay)
			admin.GET("/stats/export", controller.QydionExportBreakdownCSV)
			admin.GET("/quota-transactions", controller.QydionListQuotaTransactions)
		}
	}
}
