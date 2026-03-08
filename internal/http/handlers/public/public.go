package public

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/dujiao-next/internal/cache"
	"github.com/dujiao-next/internal/constants"
	"github.com/dujiao-next/internal/http/handlers/shared"
	"github.com/dujiao-next/internal/http/response"
	"github.com/dujiao-next/internal/i18n"
	"github.com/dujiao-next/internal/models"
	"github.com/dujiao-next/internal/repository"
	"github.com/dujiao-next/internal/service"

	"github.com/gin-gonic/gin"
)

const (
	publicConfigCacheKey = "public:config"
	publicConfigCacheTTL = 60 * time.Second
	publicLowStockLimit  = 5
)

// PublicSKUView 公共 SKU 响应结构，在原始 SKU 基础上附加促销价
type PublicSKUView struct {
	models.ProductSKU
	PromotionPriceAmount *models.Money `json:"promotion_price_amount,omitempty"`
}

// PublicProductView 公共商品响应结构
type PublicProductView struct {
	models.Product
	PromotionID          *uint            `json:"promotion_id,omitempty"`
	PromotionName        string           `json:"promotion_name,omitempty"`
	PromotionType        string           `json:"promotion_type,omitempty"`
	PromotionPriceAmount *models.Money    `json:"promotion_price_amount,omitempty"`
	PublicSKUs           *[]PublicSKUView `json:"skus,omitempty"`
	ManualStockAvailable int              `json:"manual_stock_available"`
	AutoStockAvailable   int64            `json:"auto_stock_available"`
	StockStatus          string           `json:"stock_status"`
	IsSoldOut            bool             `json:"is_sold_out"`
}

// GetConfig 获取全局配置
func (h *Handler) GetConfig(c *gin.Context) {
	// 默认配置
	defaults := map[string]interface{}{
		"languages":                        append([]string(nil), constants.SupportedLocales...),
		constants.SettingFieldSiteCurrency: constants.SiteCurrencyDefault,
		"contact": map[string]interface{}{
			"telegram": "https://t.me/dujiaoka",
			"whatsapp": "https://wa.me/1234567890",
		},
		"scripts": make([]interface{}, 0),
	}

	var cached map[string]interface{}
	if hit, err := cache.GetJSON(c.Request.Context(), publicConfigCacheKey, &cached); err == nil && hit {
		response.Success(c, cached)
		return
	}

	data, err := h.SettingService.GetConfig(defaults)
	if err != nil {
		shared.RespondError(c, response.CodeInternal, "error.config_fetch_failed", err)
		return
	}

	channels, _, err := h.PaymentService.ListChannels(repository.PaymentChannelListFilter{
		Page:       1,
		PageSize:   200,
		ActiveOnly: true,
	})
	if err != nil {
		shared.RespondError(c, response.CodeInternal, "error.config_fetch_failed", err)
		return
	}
	publicChannels := make([]map[string]interface{}, 0, len(channels))
	for _, channel := range channels {
		publicChannels = append(publicChannels, map[string]interface{}{
			"id":               channel.ID,
			"name":             channel.Name,
			"provider_type":    channel.ProviderType,
			"channel_type":     channel.ChannelType,
			"interaction_mode": channel.InteractionMode,
			"fee_rate":         channel.FeeRate,
		})
	}
	data["payment_channels"] = publicChannels

	if h.CaptchaService != nil {
		publicCaptcha, captchaErr := h.CaptchaService.GetPublicSetting()
		if captchaErr != nil {
			shared.RespondError(c, response.CodeInternal, "error.config_fetch_failed", captchaErr)
			return
		}
		data["captcha"] = publicCaptcha
	}
	telegramAuthConfig := map[string]interface{}{
		"enabled":      false,
		"bot_username": "",
	}
	if h.TelegramAuthService != nil {
		telegramAuthConfig = h.TelegramAuthService.PublicConfig()
	} else if h.Config != nil {
		telegramAuthConfig["enabled"] = h.Config.TelegramAuth.Enabled
		telegramAuthConfig["bot_username"] = strings.TrimSpace(h.Config.TelegramAuth.BotUsername)
	}
	data["telegram_auth"] = telegramAuthConfig

	affiliateSetting, err := h.SettingService.GetAffiliateSetting()
	if err != nil {
		shared.RespondError(c, response.CodeInternal, "error.config_fetch_failed", err)
		return
	}
	data["affiliate"] = service.AffiliateSettingToMap(affiliateSetting)

	_ = cache.SetJSON(c.Request.Context(), publicConfigCacheKey, data, publicConfigCacheTTL)
	response.Success(c, data)
}

// GetProducts 获取商品列表
func (h *Handler) GetProducts(c *gin.Context) {
	// 获取分页参数
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	page, pageSize = shared.NormalizePagination(page, pageSize)

	// 获取筛选参数
	categoryID := c.Query("category_id")
	search := strings.TrimSpace(c.Query("search"))

	products, total, err := h.ProductService.ListPublic(categoryID, search, page, pageSize)
	if err != nil {
		shared.RespondError(c, response.CodeInternal, "error.product_fetch_failed", err)
		return
	}

	var promotionService *service.PromotionService
	if h.PromotionRepo != nil {
		promotionService = service.NewPromotionService(h.PromotionRepo)
	}

	if err := h.ProductService.ApplyAutoStockCounts(products); err != nil {
		shared.RespondError(c, response.CodeInternal, "error.product_fetch_failed", err)
		return
	}

	decorated := make([]PublicProductView, 0, len(products))
	for i := range products {
		item, derr := h.decoratePublicProduct(&products[i], promotionService)
		if derr != nil {
			shared.RespondError(c, response.CodeInternal, "error.product_fetch_failed", derr)
			return
		}
		decorated = append(decorated, item)
	}

	// 统一响应格式
	pagination := response.BuildPagination(page, pageSize, total)
	response.SuccessWithPage(c, decorated, pagination)
}

// GetProductBySlug 根据 slug 获取商品详情
func (h *Handler) GetProductBySlug(c *gin.Context) {
	slug := c.Param("slug")

	product, err := h.ProductService.GetPublicBySlug(slug)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			shared.RespondError(c, response.CodeNotFound, "error.product_not_found", nil)
			return
		}
		shared.RespondError(c, response.CodeInternal, "error.product_fetch_failed", err)
		return
	}

	var promotionService *service.PromotionService
	if h.PromotionRepo != nil {
		promotionService = service.NewPromotionService(h.PromotionRepo)
	}

	temp := []models.Product{*product}
	if err := h.ProductService.ApplyAutoStockCounts(temp); err != nil {
		shared.RespondError(c, response.CodeInternal, "error.product_fetch_failed", err)
		return
	}
	*product = temp[0]

	decorated, derr := h.decoratePublicProduct(product, promotionService)
	if derr != nil {
		shared.RespondError(c, response.CodeInternal, "error.product_fetch_failed", derr)
		return
	}

	response.Success(c, decorated)
}

func (h *Handler) decoratePublicProduct(product *models.Product, promotionService *service.PromotionService) (PublicProductView, error) {
	if product == nil {
		return PublicProductView{}, nil
	}

	item := PublicProductView{Product: *product}
	displayPrice := resolvePublicDisplayPrice(product)
	item.Product.PriceAmount = displayPrice
	h.decorateProductStock(product, &item)

	// 构建 PublicSKUs 并为每个 active SKU 计算促销价
	// 使用 item.Product.SKUs（decorateProductStock 可能已修改库存字段）
	skuViews := make([]PublicSKUView, 0, len(item.Product.SKUs))
	var bestPromotion *models.Promotion
	var lowestPromotionPrice *models.Money

	for _, sku := range item.Product.SKUs {
		sv := PublicSKUView{ProductSKU: sku}
		if promotionService != nil && sku.IsActive {
			priceCarrier := *product
			priceCarrier.PriceAmount = sku.PriceAmount
			promotion, discountedPrice, err := promotionService.ApplyPromotion(&priceCarrier, 1)
			if err != nil && !errors.Is(err, service.ErrPromotionInvalid) {
				return PublicProductView{}, err
			}
			if promotion != nil && discountedPrice.Decimal.LessThan(sku.PriceAmount.Decimal) {
				sv.PromotionPriceAmount = &discountedPrice
				if bestPromotion == nil {
					bestPromotion = promotion
				}
				if lowestPromotionPrice == nil || discountedPrice.Decimal.LessThan(lowestPromotionPrice.Decimal) {
					cp := discountedPrice
					lowestPromotionPrice = &cp
				}
			}
		}
		skuViews = append(skuViews, sv)
	}

	item.PublicSKUs = &skuViews
	// 隐藏嵌入 Product 中的原始 SKUs，由 PublicSKUs 代替输出
	item.Product.SKUs = nil

	// 产品级促销信息：取最低 SKU 促销价，用于列表页展示
	if bestPromotion != nil && lowestPromotionPrice != nil {
		promotionID := bestPromotion.ID
		item.PromotionID = &promotionID
		item.PromotionName = strings.TrimSpace(bestPromotion.Name)
		item.PromotionType = strings.TrimSpace(bestPromotion.Type)
		item.PromotionPriceAmount = lowestPromotionPrice
	}

	return item, nil
}

func resolvePublicDisplayPrice(product *models.Product) models.Money {
	if product == nil {
		return models.Money{}
	}
	for _, sku := range product.SKUs {
		if !sku.IsActive {
			continue
		}
		return sku.PriceAmount
	}
	return product.PriceAmount
}

func (h *Handler) decorateProductStock(product *models.Product, item *PublicProductView) {
	if product == nil || item == nil {
		return
	}

	stockStatus := constants.ProductStockStatusInStock
	manualAvailable := 0

	item.ManualStockAvailable = manualAvailable
	item.AutoStockTotal = 0
	item.AutoStockLocked = 0
	item.AutoStockSold = 0
	item.AutoStockAvailable = 0
	item.StockStatus = stockStatus
	item.IsSoldOut = false

	fulfillmentType := strings.TrimSpace(product.FulfillmentType)
	if fulfillmentType == "" {
		fulfillmentType = constants.FulfillmentTypeManual
	}

	// upstream 类型：根据 SKU 映射中的上游库存判断
	if fulfillmentType == constants.FulfillmentTypeUpstream {
		h.decorateUpstreamStock(product, item)
		return
	}

	if fulfillmentType == constants.FulfillmentTypeManual {
		hasActiveSKU := false
		hasUnlimitedSKU := false
		skuRemaining := 0
		for _, sku := range product.SKUs {
			if !sku.IsActive {
				continue
			}
			hasActiveSKU = true
			if sku.ManualStockTotal == constants.ManualStockUnlimited {
				hasUnlimitedSKU = true
				continue
			}
			if sku.ManualStockTotal > 0 {
				skuRemaining += sku.ManualStockTotal
			}
		}
		if hasActiveSKU {
			if hasUnlimitedSKU {
				item.ManualStockAvailable = constants.ManualStockUnlimited
				item.StockStatus = constants.ProductStockStatusUnlimited
				item.IsSoldOut = false
				return
			}
			manualAvailable = skuRemaining
		} else if product.ManualStockTotal == constants.ManualStockUnlimited {
			item.ManualStockAvailable = constants.ManualStockUnlimited
			item.StockStatus = constants.ProductStockStatusUnlimited
			item.IsSoldOut = false
			return
		} else {
			manualAvailable = product.ManualStockTotal
			if manualAvailable < 0 {
				manualAvailable = 0
			}
		}
		item.ManualStockAvailable = manualAvailable

		switch {
		case manualAvailable <= 0:
			item.StockStatus = constants.ProductStockStatusOutOfStock
			item.IsSoldOut = true
		case manualAvailable <= publicLowStockLimit:
			item.StockStatus = constants.ProductStockStatusLowStock
		default:
			item.StockStatus = constants.ProductStockStatusInStock
		}
		return
	}

	autoAvailable := int64(0)
	autoTotal := int64(0)
	autoLocked := int64(0)
	autoSold := int64(0)
	for _, sku := range product.SKUs {
		if !sku.IsActive {
			continue
		}
		autoAvailable += sku.AutoStockAvailable
		autoTotal += sku.AutoStockTotal
		autoLocked += sku.AutoStockLocked
		autoSold += sku.AutoStockSold
	}
	item.AutoStockAvailable = autoAvailable
	item.AutoStockTotal = autoTotal
	item.AutoStockLocked = autoLocked
	item.AutoStockSold = autoSold

	switch {
	case autoAvailable <= 0:
		item.StockStatus = constants.ProductStockStatusOutOfStock
		item.IsSoldOut = true
	case autoAvailable <= int64(publicLowStockLimit):
		item.StockStatus = constants.ProductStockStatusLowStock
	default:
		item.StockStatus = constants.ProductStockStatusInStock
	}
}

// decorateUpstreamStock 根据 SKU 映射的上游库存信息填充商品及 SKU 级库存状态
func (h *Handler) decorateUpstreamStock(product *models.Product, item *PublicProductView) {
	// 通过本地商品 ID 查找 product mapping
	mapping, err := h.ProductMappingRepo.GetByLocalProductID(product.ID)
	if err != nil || mapping == nil {
		// 没有映射记录，降级为显示有库存（避免误售罄）
		item.Product.FulfillmentType = constants.FulfillmentTypeManual
		item.StockStatus = constants.ProductStockStatusInStock
		item.IsSoldOut = false
		return
	}

	// 根据上游原始交付类型设置展示类型：auto 还是 manual
	displayType := mapping.UpstreamFulfillmentType
	if displayType != constants.FulfillmentTypeAuto {
		displayType = constants.FulfillmentTypeManual
	}
	item.Product.FulfillmentType = displayType

	// 获取该映射下的所有 SKU 映射
	skuMappings, err := h.SKUMappingRepo.ListByProductMapping(mapping.ID)
	if err != nil || len(skuMappings) == 0 {
		item.StockStatus = constants.ProductStockStatusInStock
		item.IsSoldOut = false
		return
	}

	// 按本地 SKU ID 索引映射
	skuMappingByLocal := make(map[uint]*models.SKUMapping, len(skuMappings))
	for i := range skuMappings {
		skuMappingByLocal[skuMappings[i].LocalSKUID] = &skuMappings[i]
	}

	// 填充每个 SKU 的上游库存，同时汇总商品级状态
	hasUnlimited := false
	totalStock := 0
	hasActiveMapping := false

	for i := range item.Product.SKUs {
		sku := &item.Product.SKUs[i]
		sm, ok := skuMappingByLocal[sku.ID]
		if !ok || !sm.UpstreamIsActive {
			sku.UpstreamStock = 0
			continue
		}
		hasActiveMapping = true
		sku.UpstreamStock = sm.UpstreamStock

		// 根据展示类型填充对应的库存字段，让前端详情页的库存判断逻辑正确工作
		if displayType == constants.FulfillmentTypeAuto {
			if sm.UpstreamStock == -1 {
				sku.AutoStockAvailable = -1 // 前端对 auto 类型 -1 不做特殊处理，但总量为负时不限购
			} else {
				sku.AutoStockAvailable = int64(sm.UpstreamStock)
			}
		} else {
			if sm.UpstreamStock == -1 {
				sku.ManualStockTotal = constants.ManualStockUnlimited
			} else {
				sku.ManualStockTotal = sm.UpstreamStock
			}
		}

		if sm.UpstreamStock == -1 {
			hasUnlimited = true
		} else {
			totalStock += sm.UpstreamStock
		}
	}

	if !hasActiveMapping {
		item.StockStatus = constants.ProductStockStatusOutOfStock
		item.IsSoldOut = true
		return
	}

	if hasUnlimited {
		if displayType == constants.FulfillmentTypeAuto {
			item.AutoStockAvailable = -1
		} else {
			item.ManualStockAvailable = constants.ManualStockUnlimited
		}
		item.StockStatus = constants.ProductStockStatusUnlimited
		item.IsSoldOut = false
		return
	}

	if displayType == constants.FulfillmentTypeAuto {
		item.AutoStockAvailable = int64(totalStock)
	} else {
		item.ManualStockAvailable = totalStock
	}

	switch {
	case totalStock <= 0:
		item.StockStatus = constants.ProductStockStatusOutOfStock
		item.IsSoldOut = true
	case totalStock <= publicLowStockLimit:
		item.StockStatus = constants.ProductStockStatusLowStock
	default:
		item.StockStatus = constants.ProductStockStatusInStock
	}
}

// GetPosts 获取文章/公告列表
func (h *Handler) GetPosts(c *gin.Context) {
	// 获取分页参数
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	page, pageSize = shared.NormalizePagination(page, pageSize)

	// 获取类型参数
	postType := c.Query("type") // blog 或 notice

	posts, total, err := h.PostService.ListPublic(postType, page, pageSize)
	if err != nil {
		shared.RespondError(c, response.CodeInternal, "error.post_fetch_failed", err)
		return
	}

	// 统一响应格式
	pagination := response.BuildPagination(page, pageSize, total)
	response.SuccessWithPage(c, posts, pagination)
}

// GetPostBySlug 根据 slug 获取文章详情
func (h *Handler) GetPostBySlug(c *gin.Context) {
	slug := c.Param("slug")

	post, err := h.PostService.GetPublicBySlug(slug)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			shared.RespondError(c, response.CodeNotFound, "error.post_not_found", nil)
			return
		}
		shared.RespondError(c, response.CodeInternal, "error.post_fetch_failed", err)
		return
	}

	response.Success(c, post)
}

// GetCategories 获取分类列表
func (h *Handler) GetCategories(c *gin.Context) {
	categories, err := h.CategoryService.List()
	if err != nil {
		shared.RespondError(c, response.CodeInternal, "error.category_fetch_failed", err)
		return
	}

	response.Success(c, categories)
}

// CreateGuestOrderRequest 游客下单请求
type CreateGuestOrderRequest struct {
	Email               string                       `json:"email" binding:"required"`
	OrderPassword       string                       `json:"order_password" binding:"required"`
	Items               []OrderItemRequest           `json:"items" binding:"required"`
	CouponCode          string                       `json:"coupon_code"`
	AffiliateCode       string                       `json:"affiliate_code"`
	AffiliateVisitorKey string                       `json:"affiliate_visitor_key"`
	ManualFormData      map[string]models.JSON       `json:"manual_form_data"`
	CaptchaPayload      shared.CaptchaPayloadRequest `json:"captcha_payload"`
}

// CreateGuestOrder 游客创建订单
func (h *Handler) CreateGuestOrder(c *gin.Context) {
	var req CreateGuestOrderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		shared.RespondBindError(c, err)
		return
	}
	if h.CaptchaService != nil {
		if captchaErr := h.CaptchaService.Verify(constants.CaptchaSceneGuestCreateOrder, req.CaptchaPayload.ToServicePayload(), c.ClientIP()); captchaErr != nil {
			switch {
			case errors.Is(captchaErr, service.ErrCaptchaRequired):
				shared.RespondError(c, response.CodeBadRequest, "error.captcha_required", nil)
				return
			case errors.Is(captchaErr, service.ErrCaptchaInvalid):
				shared.RespondError(c, response.CodeBadRequest, "error.captcha_invalid", nil)
				return
			case errors.Is(captchaErr, service.ErrCaptchaConfigInvalid):
				shared.RespondError(c, response.CodeInternal, "error.captcha_config_invalid", captchaErr)
				return
			default:
				shared.RespondError(c, response.CodeInternal, "error.captcha_verify_failed", captchaErr)
				return
			}
		}
	}
	var items []service.CreateOrderItem
	for _, item := range req.Items {
		items = append(items, service.CreateOrderItem{
			ProductID:       item.ProductID,
			SKUID:           item.SKUID,
			Quantity:        item.Quantity,
			FulfillmentType: item.FulfillmentType,
		})
	}
	order, err := h.OrderService.CreateGuestOrder(service.CreateGuestOrderInput{
		Email:               req.Email,
		OrderPassword:       req.OrderPassword,
		Locale:              i18n.ResolveLocale(c),
		Items:               items,
		CouponCode:          req.CouponCode,
		AffiliateCode:       req.AffiliateCode,
		AffiliateVisitorKey: req.AffiliateVisitorKey,
		ClientIP:            c.ClientIP(),
		ManualFormData:      req.ManualFormData,
	})
	if err != nil {
		respondGuestOrderCreateError(c, err)
		return
	}
	order.MaskUpstreamFulfillmentType()
	response.Success(c, order)
}

// PreviewGuestOrder 游客订单金额预览
func (h *Handler) PreviewGuestOrder(c *gin.Context) {
	var req CreateGuestOrderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		shared.RespondBindError(c, err)
		return
	}
	var items []service.CreateOrderItem
	for _, item := range req.Items {
		items = append(items, service.CreateOrderItem{
			ProductID:       item.ProductID,
			SKUID:           item.SKUID,
			Quantity:        item.Quantity,
			FulfillmentType: item.FulfillmentType,
		})
	}
	preview, err := h.OrderService.PreviewGuestOrder(service.CreateGuestOrderInput{
		Email:               req.Email,
		OrderPassword:       req.OrderPassword,
		Locale:              i18n.ResolveLocale(c),
		Items:               items,
		CouponCode:          req.CouponCode,
		AffiliateCode:       req.AffiliateCode,
		AffiliateVisitorKey: req.AffiliateVisitorKey,
		ClientIP:            c.ClientIP(),
		ManualFormData:      req.ManualFormData,
	})
	if err != nil {
		respondGuestOrderPreviewError(c, err)
		return
	}
	response.Success(c, preview)
}

// ListGuestOrders 获取游客订单列表
func (h *Handler) ListGuestOrders(c *gin.Context) {
	email := strings.TrimSpace(c.Query("email"))
	password := strings.TrimSpace(c.Query("order_password"))
	orderNo := strings.TrimSpace(c.Query("order_no"))
	if email == "" {
		shared.RespondError(c, response.CodeBadRequest, "error.guest_email_required", nil)
		return
	}
	if password == "" {
		shared.RespondError(c, response.CodeBadRequest, "error.guest_password_required", nil)
		return
	}

	if orderNo != "" {
		order, err := h.OrderService.GetOrderByGuestOrderNo(orderNo, email, password)
		if err != nil {
			if errors.Is(err, service.ErrGuestOrderNotFound) {
				pagination := response.Pagination{
					Page:      1,
					PageSize:  1,
					Total:     0,
					TotalPage: 1,
				}
				response.SuccessWithPage(c, []models.Order{}, pagination)
				return
			}
			shared.RespondError(c, response.CodeInternal, "error.order_fetch_failed", err)
			return
		}
		order.MaskUpstreamFulfillmentType()
		pagination := response.Pagination{
			Page:      1,
			PageSize:  1,
			Total:     1,
			TotalPage: 1,
		}
		response.SuccessWithPage(c, []models.Order{*order}, pagination)
		return
	}

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	page, pageSize = shared.NormalizePagination(page, pageSize)

	orders, total, err := h.OrderService.ListOrdersByGuest(email, password, page, pageSize)
	if err != nil {
		shared.RespondError(c, response.CodeInternal, "error.order_fetch_failed", err)
		return
	}
	for i := range orders {
		orders[i].MaskUpstreamFulfillmentType()
	}
	pagination := response.BuildPagination(page, pageSize, total)
	response.SuccessWithPage(c, orders, pagination)
}

// GetGuestOrder 获取游客订单详情
func (h *Handler) GetGuestOrder(c *gin.Context) {
	email := strings.TrimSpace(c.Query("email"))
	password := strings.TrimSpace(c.Query("order_password"))
	if email == "" {
		shared.RespondError(c, response.CodeBadRequest, "error.guest_email_required", nil)
		return
	}
	if password == "" {
		shared.RespondError(c, response.CodeBadRequest, "error.guest_password_required", nil)
		return
	}
	orderID, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || orderID == 0 {
		shared.RespondError(c, response.CodeBadRequest, "error.order_item_invalid", nil)
		return
	}
	order, err := h.OrderService.GetOrderByGuest(uint(orderID), email, password)
	if err != nil {
		if errors.Is(err, service.ErrGuestOrderNotFound) {
			shared.RespondError(c, response.CodeNotFound, "error.guest_order_not_found", nil)
			return
		}
		shared.RespondError(c, response.CodeInternal, "error.order_fetch_failed", err)
		return
	}
	order.MaskUpstreamFulfillmentType()
	response.Success(c, order)
}

// GetGuestOrderByOrderNo 按订单号获取游客订单详情
func (h *Handler) GetGuestOrderByOrderNo(c *gin.Context) {
	email := strings.TrimSpace(c.Query("email"))
	password := strings.TrimSpace(c.Query("order_password"))
	if email == "" {
		shared.RespondError(c, response.CodeBadRequest, "error.guest_email_required", nil)
		return
	}
	if password == "" {
		shared.RespondError(c, response.CodeBadRequest, "error.guest_password_required", nil)
		return
	}
	orderNo := strings.TrimSpace(c.Param("order_no"))
	if orderNo == "" {
		shared.RespondError(c, response.CodeBadRequest, "error.order_item_invalid", nil)
		return
	}
	order, err := h.OrderService.GetOrderByGuestOrderNo(orderNo, email, password)
	if err != nil {
		if errors.Is(err, service.ErrGuestOrderNotFound) {
			shared.RespondError(c, response.CodeNotFound, "error.guest_order_not_found", nil)
			return
		}
		shared.RespondError(c, response.CodeInternal, "error.order_fetch_failed", err)
		return
	}
	order.MaskUpstreamFulfillmentType()
	response.Success(c, order)
}

// CreateGuestPaymentRequest 游客发起支付请求
type CreateGuestPaymentRequest struct {
	Email         string `json:"email" binding:"required"`
	OrderPassword string `json:"order_password" binding:"required"`
	OrderID       uint   `json:"order_id" binding:"required"`
	ChannelID     uint   `json:"channel_id" binding:"required"`
}

type LatestGuestPaymentQuery struct {
	Email         string `form:"email" binding:"required"`
	OrderPassword string `form:"order_password" binding:"required"`
	OrderID       uint   `form:"order_id" binding:"required"`
}

// CreateGuestPayment 游客发起支付
func (h *Handler) CreateGuestPayment(c *gin.Context) {
	var req CreateGuestPaymentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		shared.RespondBindError(c, err)
		return
	}
	email := strings.TrimSpace(req.Email)
	password := strings.TrimSpace(req.OrderPassword)
	if email == "" {
		shared.RespondError(c, response.CodeBadRequest, "error.guest_email_required", nil)
		return
	}
	if password == "" {
		shared.RespondError(c, response.CodeBadRequest, "error.guest_password_required", nil)
		return
	}
	if _, err := h.OrderService.GetOrderByGuest(req.OrderID, email, password); err != nil {
		if errors.Is(err, service.ErrGuestOrderNotFound) {
			shared.RespondError(c, response.CodeNotFound, "error.guest_order_not_found", nil)
			return
		}
		shared.RespondError(c, response.CodeInternal, "error.order_fetch_failed", err)
		return
	}
	result, err := h.PaymentService.CreatePayment(service.CreatePaymentInput{
		OrderID:    req.OrderID,
		ChannelID:  req.ChannelID,
		UseBalance: false,
		ClientIP:   c.ClientIP(),
		Context:    c.Request.Context(),
	})
	if err != nil {
		respondPaymentCreateError(c, err)
		return
	}
	resp := gin.H{
		"order_paid":         result.OrderPaid,
		"wallet_paid_amount": result.WalletPaidAmount,
		"online_pay_amount":  result.OnlinePayAmount,
	}
	if result.Payment != nil {
		resp["payment_id"] = result.Payment.ID
		resp["provider_type"] = result.Payment.ProviderType
		resp["channel_type"] = result.Payment.ChannelType
		resp["interaction_mode"] = result.Payment.InteractionMode
		resp["pay_url"] = result.Payment.PayURL
		resp["qr_code"] = result.Payment.QRCode
		resp["expires_at"] = result.Payment.ExpiredAt
	}
	response.Success(c, resp)
}

// CaptureGuestPaymentRequest 游客捕获支付请求。
type CaptureGuestPaymentRequest struct {
	Email         string `json:"email" binding:"required"`
	OrderPassword string `json:"order_password" binding:"required"`
}

// CaptureGuestPayment 游客捕获支付。
func (h *Handler) CaptureGuestPayment(c *gin.Context) {
	paymentID, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || paymentID == 0 {
		shared.RespondError(c, response.CodeBadRequest, "error.payment_invalid", nil)
		return
	}
	var req CaptureGuestPaymentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		shared.RespondBindError(c, err)
		return
	}
	email := strings.TrimSpace(req.Email)
	password := strings.TrimSpace(req.OrderPassword)
	if email == "" {
		shared.RespondError(c, response.CodeBadRequest, "error.guest_email_required", nil)
		return
	}
	if password == "" {
		shared.RespondError(c, response.CodeBadRequest, "error.guest_password_required", nil)
		return
	}

	payment, err := h.PaymentService.GetPayment(uint(paymentID))
	if err != nil {
		if errors.Is(err, service.ErrPaymentNotFound) {
			shared.RespondError(c, response.CodeNotFound, "error.payment_not_found", nil)
			return
		}
		shared.RespondError(c, response.CodeInternal, "error.payment_fetch_failed", err)
		return
	}
	if _, err := h.OrderService.GetOrderByGuest(payment.OrderID, email, password); err != nil {
		if errors.Is(err, service.ErrGuestOrderNotFound) {
			shared.RespondError(c, response.CodeNotFound, "error.guest_order_not_found", nil)
			return
		}
		shared.RespondError(c, response.CodeInternal, "error.order_fetch_failed", err)
		return
	}

	updated, err := h.PaymentService.CapturePayment(service.CapturePaymentInput{
		PaymentID: uint(paymentID),
		Context:   c.Request.Context(),
	})
	if err != nil {
		respondPaymentCaptureError(c, err)
		return
	}
	response.Success(c, gin.H{
		"payment_id": updated.ID,
		"status":     updated.Status,
	})
}

// GetGuestLatestPayment 获取游客最新待支付记录
func (h *Handler) GetGuestLatestPayment(c *gin.Context) {
	var query LatestGuestPaymentQuery
	if err := c.ShouldBindQuery(&query); err != nil {
		shared.RespondError(c, response.CodeBadRequest, "error.bad_request", err)
		return
	}
	email := strings.TrimSpace(query.Email)
	password := strings.TrimSpace(query.OrderPassword)
	if email == "" {
		shared.RespondError(c, response.CodeBadRequest, "error.guest_email_required", nil)
		return
	}
	if password == "" {
		shared.RespondError(c, response.CodeBadRequest, "error.guest_password_required", nil)
		return
	}

	order, err := h.OrderService.GetOrderByGuest(query.OrderID, email, password)
	if err != nil {
		if errors.Is(err, service.ErrGuestOrderNotFound) {
			shared.RespondError(c, response.CodeNotFound, "error.guest_order_not_found", nil)
			return
		}
		shared.RespondError(c, response.CodeInternal, "error.order_fetch_failed", err)
		return
	}
	if order.ParentID != nil {
		shared.RespondError(c, response.CodeBadRequest, "error.payment_invalid", nil)
		return
	}
	if order.Status != constants.OrderStatusPendingPayment {
		shared.RespondError(c, response.CodeBadRequest, "error.order_status_invalid", nil)
		return
	}
	if order.ExpiresAt != nil && !order.ExpiresAt.After(time.Now()) {
		shared.RespondError(c, response.CodeBadRequest, "error.order_status_invalid", nil)
		return
	}

	payment, err := h.PaymentRepo.GetLatestPendingByOrder(order.ID, time.Now())
	if err != nil {
		shared.RespondError(c, response.CodeInternal, "error.payment_fetch_failed", err)
		return
	}
	if payment == nil {
		shared.RespondError(c, response.CodeNotFound, "error.payment_not_found", nil)
		return
	}

	response.Success(c, gin.H{
		"payment_id":       payment.ID,
		"order_id":         payment.OrderID,
		"channel_id":       payment.ChannelID,
		"provider_type":    payment.ProviderType,
		"channel_type":     payment.ChannelType,
		"interaction_mode": payment.InteractionMode,
		"pay_url":          payment.PayURL,
		"qr_code":          payment.QRCode,
		"expires_at":       payment.ExpiredAt,
	})
}
