package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dujiao-next/internal/constants"
	"github.com/dujiao-next/internal/logger"
	"github.com/dujiao-next/internal/models"
	"github.com/dujiao-next/internal/queue"
	"github.com/dujiao-next/internal/repository"

	"github.com/shopspring/decimal"
	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// PaymentService 支付服务
type PaymentService struct {
	orderRepo             repository.OrderRepository
	productRepo           repository.ProductRepository
	productSKURepo        repository.ProductSKURepository
	paymentRepo           repository.PaymentRepository
	channelRepo           repository.PaymentChannelRepository
	walletRepo            repository.WalletRepository
	userOAuthIdentityRepo repository.UserOAuthIdentityRepository
	queueClient           *queue.Client
	walletSvc             *WalletService
	settingService        *SettingService
	expireMinutes         int
	affiliateSvc          *AffiliateService
	notificationSvc       *NotificationService
	procurementSvc        *ProcurementOrderService
	downstreamCallbackSvc *DownstreamCallbackService
}

// SetProcurementService 设置采购单服务（解决循环依赖）
func (s *PaymentService) SetProcurementService(svc *ProcurementOrderService) {
	s.procurementSvc = svc
}

// SetDownstreamCallbackService 设置下游回调服务（解决循环依赖）
func (s *PaymentService) SetDownstreamCallbackService(svc *DownstreamCallbackService) {
	s.downstreamCallbackSvc = svc
}

// PaymentServiceOptions 支付服务构造参数
type PaymentServiceOptions struct {
	OrderRepo             repository.OrderRepository
	ProductRepo           repository.ProductRepository
	ProductSKURepo        repository.ProductSKURepository
	PaymentRepo           repository.PaymentRepository
	ChannelRepo           repository.PaymentChannelRepository
	WalletRepo            repository.WalletRepository
	UserOAuthIdentityRepo repository.UserOAuthIdentityRepository
	QueueClient           *queue.Client
	WalletService         *WalletService
	SettingService        *SettingService
	ExpireMinutes         int
	AffiliateService      *AffiliateService
	NotificationService   *NotificationService
}

// NewPaymentService 创建支付服务
func NewPaymentService(opts PaymentServiceOptions) *PaymentService {
	return &PaymentService{
		orderRepo:             opts.OrderRepo,
		productRepo:           opts.ProductRepo,
		productSKURepo:        opts.ProductSKURepo,
		paymentRepo:           opts.PaymentRepo,
		channelRepo:           opts.ChannelRepo,
		walletRepo:            opts.WalletRepo,
		userOAuthIdentityRepo: opts.UserOAuthIdentityRepo,
		queueClient:           opts.QueueClient,
		walletSvc:             opts.WalletService,
		settingService:        opts.SettingService,
		expireMinutes:         opts.ExpireMinutes,
		affiliateSvc:          opts.AffiliateService,
		notificationSvc:       opts.NotificationService,
	}
}

// CreatePaymentInput 创建支付请求
type CreatePaymentInput struct {
	OrderID    uint
	ChannelID  uint
	UseBalance bool
	ClientIP   string
	Context    context.Context
}

// CreatePaymentResult 创建支付结果
type CreatePaymentResult struct {
	Payment          *models.Payment
	Channel          *models.PaymentChannel
	OrderPaid        bool
	WalletPaidAmount models.Money
	OnlinePayAmount  models.Money
}

// CreateWalletRechargePaymentInput 创建钱包充值支付请求
type CreateWalletRechargePaymentInput struct {
	UserID    uint
	ChannelID uint
	Amount    models.Money
	Currency  string
	Remark    string
	ClientIP  string
	Context   context.Context
}

// CreateWalletRechargePaymentResult 创建钱包充值支付结果
type CreateWalletRechargePaymentResult struct {
	Recharge *models.WalletRechargeOrder
	Payment  *models.Payment
}

func hasProviderResult(payment *models.Payment) bool {
	if payment == nil {
		return false
	}
	return strings.TrimSpace(payment.PayURL) != "" || strings.TrimSpace(payment.QRCode) != ""
}

func shouldMarkFulfilling(order *models.Order) bool {
	if order == nil {
		return false
	}
	if len(order.Items) == 0 {
		return false
	}
	for _, item := range order.Items {
		fulfillmentType := strings.TrimSpace(item.FulfillmentType)
		if fulfillmentType == "" || fulfillmentType == constants.FulfillmentTypeManual || fulfillmentType == constants.FulfillmentTypeUpstream {
			return true
		}
	}
	return false
}

func paymentLogger(kv ...interface{}) *zap.SugaredLogger {
	if len(kv) == 0 {
		return logger.S()
	}
	return logger.SW(kv...)
}

// PaymentCallbackInput 支付回调输入
type PaymentCallbackInput struct {
	PaymentID   uint
	OrderNo     string
	ChannelID   uint
	Status      string
	ProviderRef string
	Amount      models.Money
	Currency    string
	PaidAt      *time.Time
	Payload     models.JSON
}

// CapturePaymentInput 捕获支付输入。
type CapturePaymentInput struct {
	PaymentID uint
	Context   context.Context
}

// WebhookCallbackInput Webhook 回调输入。
type WebhookCallbackInput struct {
	ChannelID uint
	Headers   map[string]string
	Body      []byte
	Context   context.Context
}

// CreatePayment 创建支付单
func (s *PaymentService) CreatePayment(input CreatePaymentInput) (*CreatePaymentResult, error) {
	if input.OrderID == 0 {
		return nil, ErrPaymentInvalid
	}

	log := paymentLogger(
		"order_id", input.OrderID,
		"channel_id", input.ChannelID,
	)

	var payment *models.Payment
	var order *models.Order
	var channel *models.PaymentChannel
	feeRate := decimal.Zero
	reusedPending := false
	orderPaidByWallet := false
	now := time.Now()

	err := s.paymentRepo.Transaction(func(tx *gorm.DB) error {
		var lockedOrder models.Order
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Preload("Items").
			Preload("Children").
			Preload("Children.Items").
			First(&lockedOrder, input.OrderID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrOrderNotFound
			}
			return ErrOrderFetchFailed
		}
		if lockedOrder.ParentID != nil {
			return ErrPaymentInvalid
		}
		if lockedOrder.Status != constants.OrderStatusPendingPayment {
			return ErrOrderStatusInvalid
		}
		if lockedOrder.ExpiresAt != nil && !lockedOrder.ExpiresAt.After(time.Now()) {
			return ErrOrderStatusInvalid
		}

		paymentRepo := s.paymentRepo.WithTx(tx)
		channelRepo := s.channelRepo.WithTx(tx)
		if input.ChannelID != 0 {
			if channel == nil {
				// 事务内必须使用 tx 绑定仓储，避免在单连接池下发生自锁等待。
				resolvedChannel, err := channelRepo.GetByID(input.ChannelID)
				if err != nil {
					return err
				}
				if resolvedChannel == nil {
					return ErrPaymentChannelNotFound
				}
				if !resolvedChannel.IsActive {
					return ErrPaymentChannelInactive
				}
				resolvedFeeRate := resolvedChannel.FeeRate.Decimal.Round(2)
				if resolvedFeeRate.LessThan(decimal.Zero) || resolvedFeeRate.GreaterThan(decimal.NewFromInt(100)) {
					return ErrPaymentChannelConfigInvalid
				}
				channel = resolvedChannel
				feeRate = resolvedFeeRate
			}

			existing, err := paymentRepo.GetLatestPendingByOrderChannel(lockedOrder.ID, channel.ID, time.Now())
			if err != nil {
				return ErrPaymentCreateFailed
			}
			if existing != nil && hasProviderResult(existing) {
				reusedPending = true
				payment = existing
				order = &lockedOrder
				return nil
			}
		}

		if s.walletSvc != nil {
			if input.UseBalance {
				if _, err := s.walletSvc.ApplyOrderBalance(tx, &lockedOrder, true); err != nil {
					return err
				}
			} else if lockedOrder.WalletPaidAmount.Decimal.GreaterThan(decimal.Zero) {
				if _, err := s.walletSvc.ReleaseOrderBalance(tx, &lockedOrder, constants.WalletTxnTypeOrderRefund, "用户改为在线支付，退回余额"); err != nil {
					return err
				}
			}
		}

		onlineAmount := normalizeOrderAmount(lockedOrder.TotalAmount.Decimal.Sub(lockedOrder.WalletPaidAmount.Decimal))
		if onlineAmount.LessThanOrEqual(decimal.Zero) {
			walletPaidAmount := normalizeOrderAmount(lockedOrder.WalletPaidAmount.Decimal)
			paidAt := time.Now()
			payment = &models.Payment{
				OrderID:         lockedOrder.ID,
				ChannelID:       0,
				ProviderType:    constants.PaymentProviderWallet,
				ChannelType:     constants.PaymentChannelTypeBalance,
				InteractionMode: constants.PaymentInteractionBalance,
				Amount:          models.NewMoneyFromDecimal(walletPaidAmount),
				FeeRate:         models.NewMoneyFromDecimal(decimal.Zero),
				FeeAmount:       models.NewMoneyFromDecimal(decimal.Zero),
				Currency:        lockedOrder.Currency,
				Status:          constants.PaymentStatusSuccess,
				CreatedAt:       paidAt,
				UpdatedAt:       paidAt,
				PaidAt:          &paidAt,
			}
			if err := paymentRepo.Create(payment); err != nil {
				return ErrPaymentCreateFailed
			}
			if err := s.markOrderPaid(tx, &lockedOrder, paidAt); err != nil {
				return err
			}
			orderPaidByWallet = true
			order = &lockedOrder
			return nil
		}
		if channel == nil {
			return ErrPaymentInvalid
		}
		if err := validatePaymentCurrencyForChannel(lockedOrder.Currency, channel); err != nil {
			return err
		}

		feeAmount := decimal.Zero
		if feeRate.GreaterThan(decimal.Zero) {
			feeAmount = onlineAmount.Mul(feeRate).Div(decimal.NewFromInt(100)).Round(2)
		}
		payableAmount := onlineAmount.Add(feeAmount).Round(2)
		payment = &models.Payment{
			OrderID:         lockedOrder.ID,
			ChannelID:       channel.ID,
			ProviderType:    channel.ProviderType,
			ChannelType:     channel.ChannelType,
			InteractionMode: channel.InteractionMode,
			Amount:          models.NewMoneyFromDecimal(payableAmount),
			FeeRate:         models.NewMoneyFromDecimal(feeRate),
			FeeAmount:       models.NewMoneyFromDecimal(feeAmount),
			Currency:        lockedOrder.Currency,
			Status:          constants.PaymentStatusInitiated,
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		if shouldUseCNYPaymentCurrency(channel) {
			payment.Currency = "CNY"
		}

		if err := paymentRepo.Create(payment); err != nil {
			return ErrPaymentCreateFailed
		}
		if err := tx.Model(&models.Order{}).Where("id = ?", lockedOrder.ID).Updates(map[string]interface{}{
			"online_paid_amount": models.NewMoneyFromDecimal(onlineAmount),
			"updated_at":         time.Now(),
		}).Error; err != nil {
			return ErrOrderUpdateFailed
		}
		lockedOrder.OnlinePaidAmount = models.NewMoneyFromDecimal(onlineAmount)
		lockedOrder.UpdatedAt = time.Now()
		order = &lockedOrder
		return nil
	})
	if err != nil {
		return nil, err
	}

	if order == nil {
		return nil, ErrOrderFetchFailed
	}

	if reusedPending {
		log.Infow("payment_create_reuse_pending",
			"payment_id", payment.ID,
			"provider_type", payment.ProviderType,
			"channel_type", payment.ChannelType,
		)
		return &CreatePaymentResult{
			Payment:          payment,
			Channel:          channel,
			WalletPaidAmount: order.WalletPaidAmount,
			OnlinePayAmount:  order.OnlinePaidAmount,
		}, nil
	}

	if orderPaidByWallet {
		log.Infow("payment_create_wallet_success",
			"payment_id", payment.ID,
			"provider_type", payment.ProviderType,
			"channel_type", payment.ChannelType,
			"interaction_mode", payment.InteractionMode,
			"currency", payment.Currency,
			"amount", payment.Amount.String(),
			"wallet_paid_amount", order.WalletPaidAmount.String(),
			"online_pay_amount", order.OnlinePaidAmount.String(),
		)
		s.enqueueOrderPaidAsync(order, payment, log)
		return &CreatePaymentResult{
			Payment:          nil,
			Channel:          nil,
			OrderPaid:        true,
			WalletPaidAmount: order.WalletPaidAmount,
			OnlinePayAmount:  models.NewMoneyFromDecimal(decimal.Zero),
		}, nil
	}

	if payment == nil {
		return nil, ErrPaymentCreateFailed
	}

	if err := s.applyProviderPayment(input, order, channel, payment); err != nil {
		rollbackErr := s.paymentRepo.Transaction(func(tx *gorm.DB) error {
			paymentRepo := s.paymentRepo.WithTx(tx)
			payment.Status = constants.PaymentStatusFailed
			payment.UpdatedAt = time.Now()
			if updateErr := paymentRepo.Update(payment); updateErr != nil {
				return updateErr
			}
			if s.walletSvc == nil {
				return nil
			}
			var lockedOrder models.Order
			if findErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&lockedOrder, order.ID).Error; findErr != nil {
				return findErr
			}
			_, refundErr := s.walletSvc.ReleaseOrderBalance(tx, &lockedOrder, constants.WalletTxnTypeOrderRefund, "在线支付创建失败，退回余额")
			return refundErr
		})
		if rollbackErr != nil {
			log.Errorw("payment_create_provider_failed_with_rollback_error",
				"payment_id", payment.ID,
				"order_id", order.ID,
				"provider_type", payment.ProviderType,
				"channel_type", payment.ChannelType,
				"provider_error", err,
				"rollback_error", rollbackErr,
			)
		} else {
			log.Errorw("payment_create_provider_failed",
				"payment_id", payment.ID,
				"provider_type", payment.ProviderType,
				"channel_type", payment.ChannelType,
				"error", err,
			)
		}
		return nil, err
	}

	log.Infow("payment_create_success",
		"payment_id", payment.ID,
		"provider_type", payment.ProviderType,
		"channel_type", payment.ChannelType,
		"interaction_mode", payment.InteractionMode,
		"currency", payment.Currency,
		"amount", payment.Amount.String(),
		"wallet_paid_amount", order.WalletPaidAmount.String(),
		"online_pay_amount", order.OnlinePaidAmount.String(),
	)

	return &CreatePaymentResult{
		Payment:          payment,
		Channel:          channel,
		WalletPaidAmount: order.WalletPaidAmount,
		OnlinePayAmount:  order.OnlinePaidAmount,
	}, nil
}

// CreateWalletRechargePayment 创建钱包充值支付单
func (s *PaymentService) CreateWalletRechargePayment(input CreateWalletRechargePaymentInput) (*CreateWalletRechargePaymentResult, error) {
	if input.UserID == 0 || input.ChannelID == 0 {
		return nil, ErrPaymentInvalid
	}
	amount := input.Amount.Decimal.Round(2)
	if amount.LessThanOrEqual(decimal.Zero) {
		return nil, ErrWalletInvalidAmount
	}
	if s.walletRepo == nil {
		return nil, ErrPaymentCreateFailed
	}

	channel, err := s.channelRepo.GetByID(input.ChannelID)
	if err != nil {
		return nil, err
	}
	if channel == nil {
		return nil, ErrPaymentChannelNotFound
	}
	if !channel.IsActive {
		return nil, ErrPaymentChannelInactive
	}

	feeRate := channel.FeeRate.Decimal.Round(2)
	if feeRate.LessThan(decimal.Zero) || feeRate.GreaterThan(decimal.NewFromInt(100)) {
		return nil, ErrPaymentChannelConfigInvalid
	}
	feeAmount := decimal.Zero
	if feeRate.GreaterThan(decimal.Zero) {
		feeAmount = amount.Mul(feeRate).Div(decimal.NewFromInt(100)).Round(2)
	}
	payableAmount := amount.Add(feeAmount).Round(2)
	currency := normalizeWalletCurrency(input.Currency)
	if err := validatePaymentCurrencyForChannel(currency, channel); err != nil {
		return nil, err
	}
	if shouldUseCNYPaymentCurrency(channel) {
		currency = "CNY"
	}
	now := time.Now()

	var payment *models.Payment
	var recharge *models.WalletRechargeOrder
	err = s.paymentRepo.Transaction(func(tx *gorm.DB) error {
		rechargeNo := generateWalletRechargeNo()
		paymentRepo := s.paymentRepo.WithTx(tx)
		payment = &models.Payment{
			OrderID:         0,
			ChannelID:       channel.ID,
			ProviderType:    channel.ProviderType,
			ChannelType:     channel.ChannelType,
			InteractionMode: channel.InteractionMode,
			Amount:          models.NewMoneyFromDecimal(payableAmount),
			FeeRate:         models.NewMoneyFromDecimal(feeRate),
			FeeAmount:       models.NewMoneyFromDecimal(feeAmount),
			Currency:        currency,
			Status:          constants.PaymentStatusInitiated,
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		if err := paymentRepo.Create(payment); err != nil {
			return ErrPaymentCreateFailed
		}

		rechargeRepo := s.walletRepo.WithTx(tx)
		recharge = &models.WalletRechargeOrder{
			RechargeNo:      rechargeNo,
			UserID:          input.UserID,
			PaymentID:       payment.ID,
			ChannelID:       channel.ID,
			ProviderType:    channel.ProviderType,
			ChannelType:     channel.ChannelType,
			InteractionMode: channel.InteractionMode,
			Amount:          models.NewMoneyFromDecimal(amount),
			PayableAmount:   models.NewMoneyFromDecimal(payableAmount),
			FeeRate:         models.NewMoneyFromDecimal(feeRate),
			FeeAmount:       models.NewMoneyFromDecimal(feeAmount),
			Currency:        currency,
			Status:          constants.WalletRechargeStatusPending,
			Remark:          cleanWalletRemark(input.Remark, "余额充值"),
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		if err := rechargeRepo.CreateRechargeOrder(recharge); err != nil {
			return ErrPaymentCreateFailed
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if payment == nil || recharge == nil {
		return nil, ErrPaymentCreateFailed
	}

	// 复用支付网关下单逻辑，使用充值单号作为业务单号。
	virtualOrder := &models.Order{
		OrderNo: recharge.RechargeNo,
		UserID:  recharge.UserID,
	}
	if err := s.applyProviderPayment(CreatePaymentInput{
		ChannelID: input.ChannelID,
		ClientIP:  input.ClientIP,
		Context:   input.Context,
	}, virtualOrder, channel, payment); err != nil {
		_ = s.paymentRepo.Transaction(func(tx *gorm.DB) error {
			rechargeRepo := s.walletRepo.WithTx(tx)
			paymentRepo := s.paymentRepo.WithTx(tx)
			failedAt := time.Now()
			payment.Status = constants.PaymentStatusFailed
			payment.UpdatedAt = failedAt
			if updateErr := paymentRepo.Update(payment); updateErr != nil {
				return updateErr
			}
			lockedRecharge, getErr := rechargeRepo.GetRechargeOrderByPaymentIDForUpdate(payment.ID)
			if getErr != nil || lockedRecharge == nil {
				return getErr
			}
			lockedRecharge.Status = constants.WalletRechargeStatusFailed
			lockedRecharge.UpdatedAt = failedAt
			return rechargeRepo.UpdateRechargeOrder(lockedRecharge)
		})
		return nil, err
	}
	if s.queueClient != nil {
		delay := time.Duration(s.resolveExpireMinutes()) * time.Minute
		if err := s.queueClient.EnqueueWalletRechargeExpire(queue.WalletRechargeExpirePayload{
			PaymentID: payment.ID,
		}, delay); err != nil {
			logger.Errorw("wallet_recharge_enqueue_timeout_expire_failed",
				"payment_id", payment.ID,
				"recharge_no", recharge.RechargeNo,
				"delay_minutes", int(delay/time.Minute),
				"error", err,
			)
			_ = s.paymentRepo.Transaction(func(tx *gorm.DB) error {
				rechargeRepo := s.walletRepo.WithTx(tx)
				paymentRepo := s.paymentRepo.WithTx(tx)
				failedAt := time.Now()
				payment.Status = constants.PaymentStatusFailed
				payment.UpdatedAt = failedAt
				if updateErr := paymentRepo.Update(payment); updateErr != nil {
					return updateErr
				}
				lockedRecharge, getErr := rechargeRepo.GetRechargeOrderByPaymentIDForUpdate(payment.ID)
				if getErr != nil || lockedRecharge == nil {
					return getErr
				}
				if lockedRecharge.Status == constants.WalletRechargeStatusSuccess {
					return nil
				}
				lockedRecharge.Status = constants.WalletRechargeStatusFailed
				lockedRecharge.UpdatedAt = failedAt
				return rechargeRepo.UpdateRechargeOrder(lockedRecharge)
			})
			return nil, ErrQueueUnavailable
		}
	}

	reloadedRecharge, err := s.walletRepo.GetRechargeOrderByPaymentID(payment.ID)
	if err != nil {
		return nil, ErrPaymentUpdateFailed
	}
	if reloadedRecharge != nil {
		recharge = reloadedRecharge
	}
	return &CreateWalletRechargePaymentResult{
		Recharge: recharge,
		Payment:  payment,
	}, nil
}

// HandleCallback 处理支付回调

// ListPayments 管理端支付列表
func (s *PaymentService) ListPayments(filter repository.PaymentListFilter) ([]models.Payment, int64, error) {
	return s.paymentRepo.ListAdmin(filter)
}

// GetPayment 获取支付记录
func (s *PaymentService) GetPayment(id uint) (*models.Payment, error) {
	if id == 0 {
		return nil, ErrPaymentInvalid
	}
	payment, err := s.paymentRepo.GetByID(id)
	if err != nil {
		return nil, ErrPaymentUpdateFailed
	}
	if payment == nil {
		return nil, ErrPaymentNotFound
	}
	return payment, nil
}

// CapturePayment 捕获支付。

// ListChannels 支付渠道列表
func (s *PaymentService) ListChannels(filter repository.PaymentChannelListFilter) ([]models.PaymentChannel, int64, error) {
	return s.channelRepo.List(filter)
}

// GetChannel 获取支付渠道
func (s *PaymentService) GetChannel(id uint) (*models.PaymentChannel, error) {
	if id == 0 {
		return nil, ErrPaymentInvalid
	}
	channel, err := s.channelRepo.GetByID(id)
	if err != nil {
		return nil, err
	}
	if channel == nil {
		return nil, ErrPaymentChannelNotFound
	}
	return channel, nil
}

func generateWalletRechargeNo() string {
	return generateSerialNo("WR")
}

func shouldUseGatewayOrderNo(channel *models.PaymentChannel) bool {
	if channel == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(channel.ProviderType)) {
	case constants.PaymentProviderEpay, constants.PaymentProviderEpusdt, constants.PaymentProviderTokenpay:
		return true
	default:
		return false
	}
}

func buildGatewayOrderNo(payment *models.Payment) string {
	if payment == nil || payment.ID == 0 {
		return ""
	}
	return fmt.Sprintf("DJP%d", payment.ID)
}

func resolveGatewayOrderNo(channel *models.PaymentChannel, payment *models.Payment) string {
	if !shouldUseGatewayOrderNo(channel) {
		return ""
	}
	if gatewayOrderNo := strings.TrimSpace(payment.GatewayOrderNo); gatewayOrderNo != "" {
		return gatewayOrderNo
	}
	return buildGatewayOrderNo(payment)
}

func resolveProviderOrderNo(businessOrderNo string, payment *models.Payment) string {
	if gatewayOrderNo := strings.TrimSpace(payment.GatewayOrderNo); gatewayOrderNo != "" {
		return gatewayOrderNo
	}
	return strings.TrimSpace(businessOrderNo)
}

func matchesBusinessOrderNo(callbackOrderNo string, businessOrderNo string, payment *models.Payment) bool {
	callbackOrderNo = strings.TrimSpace(callbackOrderNo)
	if callbackOrderNo == "" {
		return true
	}
	if callbackOrderNo == strings.TrimSpace(businessOrderNo) {
		return true
	}
	return callbackOrderNo == strings.TrimSpace(payment.GatewayOrderNo)
}

func buildOrderReturnQuery(order *models.Order, marker string, sessionID string) map[string]string {
	params := map[string]string{}
	if order != nil {
		if orderNo := strings.TrimSpace(order.OrderNo); orderNo != "" {
			params["order_no"] = orderNo
		}
		if order.UserID == 0 {
			params["guest"] = "1"
		}
	}
	if marker = strings.TrimSpace(marker); marker != "" {
		params[marker] = "1"
	}
	if sessionID = strings.TrimSpace(sessionID); sessionID != "" {
		params["session_id"] = sessionID
	}
	return params
}

func shouldUseCNYPaymentCurrency(channel *models.PaymentChannel) bool {
	if channel == nil {
		return false
	}
	providerType := strings.ToLower(strings.TrimSpace(channel.ProviderType))
	if providerType != constants.PaymentProviderOfficial {
		return false
	}
	channelType := strings.ToLower(strings.TrimSpace(channel.ChannelType))
	return channelType == constants.PaymentChannelTypeWechat || channelType == constants.PaymentChannelTypeAlipay
}

func validatePaymentCurrencyForChannel(currency string, channel *models.PaymentChannel) error {
	normalized := strings.ToUpper(strings.TrimSpace(currency))
	if !settingCurrencyCodePattern.MatchString(normalized) {
		return ErrPaymentCurrencyMismatch
	}
	if shouldUseCNYPaymentCurrency(channel) && normalized != constants.SiteCurrencyDefault {
		return ErrPaymentCurrencyMismatch
	}
	return nil
}

func (s *PaymentService) resolveExpireMinutes() int {
	defaultMinutes := s.expireMinutes
	if defaultMinutes <= 0 {
		defaultMinutes = 15
	}
	if s.settingService == nil {
		return defaultMinutes
	}
	minutes, err := s.settingService.GetOrderPaymentExpireMinutes(defaultMinutes)
	if err != nil {
		return defaultMinutes
	}
	if minutes <= 0 {
		return defaultMinutes
	}
	return minutes
}

func normalizePaymentStatus(status string) string {
	return strings.ToLower(strings.TrimSpace(status))
}

func isPaymentStatusValid(status string) bool {
	switch status {
	case constants.PaymentStatusInitiated, constants.PaymentStatusPending, constants.PaymentStatusSuccess, constants.PaymentStatusFailed, constants.PaymentStatusExpired:
		return true
	default:
		return false
	}
}

func shouldAutoFulfill(order *models.Order) bool {
	if order == nil || len(order.Items) == 0 {
		return false
	}
	for _, item := range order.Items {
		if strings.TrimSpace(item.FulfillmentType) != constants.FulfillmentTypeAuto {
			return false
		}
	}
	return true
}

func buildOrderSubject(order *models.Order) string {
	if order == nil {
		return ""
	}
	if len(order.Items) > 0 {
		title := pickOrderItemTitle(order.Items[0].TitleJSON)
		if title != "" {
			return title
		}
	}
	return order.OrderNo
}

func pickOrderItemTitle(title models.JSON) string {
	if title == nil {
		return ""
	}
	for _, key := range constants.SupportedLocales {
		if val, ok := title[key]; ok {
			if str, ok := val.(string); ok && strings.TrimSpace(str) != "" {
				return strings.TrimSpace(str)
			}
		}
	}
	for _, val := range title {
		if str, ok := val.(string); ok && strings.TrimSpace(str) != "" {
			return strings.TrimSpace(str)
		}
	}
	return ""
}
