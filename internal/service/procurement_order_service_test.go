package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dujiao-next/internal/config"
	"github.com/dujiao-next/internal/constants"
	"github.com/dujiao-next/internal/models"
	"github.com/dujiao-next/internal/repository"
	"github.com/dujiao-next/internal/upstream"

	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// ── test helpers ──

func setupProcurementTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:procurement_test_%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		DisableForeignKeyConstraintWhenMigrating: true,
	})
	if err != nil {
		t.Fatalf("open sqlite failed: %v", err)
	}
	if err := db.AutoMigrate(
		&models.Order{},
		&models.OrderItem{},
		&models.Fulfillment{},
		&models.ProcurementOrder{},
		&models.SiteConnection{},
		&models.ProductMapping{},
		&models.SKUMapping{},
	); err != nil {
		t.Fatalf("auto migrate failed: %v", err)
	}
	models.DB = db
	return db
}

// createProcTestOrder 创建一个测试订单
func createProcTestOrder(t *testing.T, db *gorm.DB, orderNo, status, fulfillmentType string) *models.Order {
	t.Helper()
	order := &models.Order{
		OrderNo:        orderNo,
		UserID:         1,
		Status:         status,
		Currency:       "CNY",
		OriginalAmount: models.NewMoneyFromDecimal(decimal.NewFromInt(100)),
		TotalAmount:    models.NewMoneyFromDecimal(decimal.NewFromInt(100)),
	}
	if err := db.Create(order).Error; err != nil {
		t.Fatalf("create order failed: %v", err)
	}
	item := &models.OrderItem{
		OrderID:         order.ID,
		ProductID:       1,
		SKUID:           1,
		Quantity:        1,
		FulfillmentType: fulfillmentType,
		TitleJSON:       models.JSON{"zh-CN": "Test Product"},
		UnitPrice:       models.NewMoneyFromDecimal(decimal.NewFromInt(100)),
		TotalPrice:      models.NewMoneyFromDecimal(decimal.NewFromInt(100)),
	}
	if err := db.Create(item).Error; err != nil {
		t.Fatalf("create order item failed: %v", err)
	}
	// 重新加载以包含 items
	var loaded models.Order
	if err := db.Preload("Items").First(&loaded, order.ID).Error; err != nil {
		t.Fatalf("reload order failed: %v", err)
	}
	return &loaded
}

// createTestProcurementOrder 创建一个测试采购单
func createTestProcurementOrder(t *testing.T, db *gorm.DB, connID, localOrderID uint, localOrderNo, status string) *models.ProcurementOrder {
	t.Helper()
	proc := &models.ProcurementOrder{
		ConnectionID:    connID,
		LocalOrderID:    localOrderID,
		LocalOrderNo:    localOrderNo,
		Status:          status,
		LocalSellAmount: models.NewMoneyFromDecimal(decimal.NewFromInt(100)),
		Currency:        "CNY",
		TraceID:         "test-trace-id",
	}
	if err := db.Create(proc).Error; err != nil {
		t.Fatalf("create procurement order failed: %v", err)
	}
	return proc
}

func newTestProcurementService(
	db *gorm.DB,
	connSvc *SiteConnectionService,
) *ProcurementOrderService {
	return NewProcurementOrderService(
		repository.NewProcurementOrderRepository(db),
		repository.NewOrderRepository(db),
		repository.NewProductMappingRepository(db),
		repository.NewSKUMappingRepository(db),
		connSvc,
		nil, // queueClient
		nil, // settingService
		config.EmailConfig{},
		nil, // fulfillmentService
	)
}

// ── Phase 1 tests: order rollback on procurement failure ──

func TestRejectProcurement_RollsBackOrderStatus(t *testing.T) {
	db := setupProcurementTestDB(t)

	order := createProcTestOrder(t, db, "PROC-REJECT-001", constants.OrderStatusFulfilling, constants.FulfillmentTypeUpstream)
	proc := createTestProcurementOrder(t, db, 1, order.ID, order.OrderNo, "pending")

	connSvc := NewSiteConnectionService(repository.NewSiteConnectionRepository(db), "test-key", t.TempDir())
	svc := newTestProcurementService(db, connSvc)

	svc.rejectProcurement(proc, "connection not found")

	// 验证采购单状态 = rejected
	var updatedProc models.ProcurementOrder
	if err := db.First(&updatedProc, proc.ID).Error; err != nil {
		t.Fatalf("load procurement: %v", err)
	}
	if updatedProc.Status != "rejected" {
		t.Errorf("expected procurement status 'rejected', got %q", updatedProc.Status)
	}

	// 验证本地订单状态从 fulfilling 回退到 paid
	var updatedOrder models.Order
	if err := db.First(&updatedOrder, order.ID).Error; err != nil {
		t.Fatalf("load order: %v", err)
	}
	if updatedOrder.Status != constants.OrderStatusPaid {
		t.Errorf("expected order status %q, got %q", constants.OrderStatusPaid, updatedOrder.Status)
	}
}

func TestHandleUpstreamCallback_Canceled_RollsBackOrder(t *testing.T) {
	db := setupProcurementTestDB(t)

	order := createProcTestOrder(t, db, "PROC-CANCEL-001", constants.OrderStatusFulfilling, constants.FulfillmentTypeUpstream)
	proc := createTestProcurementOrder(t, db, 1, order.ID, order.OrderNo, "accepted")

	connSvc := NewSiteConnectionService(repository.NewSiteConnectionRepository(db), "test-key", t.TempDir())
	svc := newTestProcurementService(db, connSvc)

	if err := svc.HandleUpstreamCallback(proc.ID, "canceled", nil); err != nil {
		t.Fatalf("HandleUpstreamCallback: %v", err)
	}

	// 验证采购单状态 = canceled
	var updatedProc models.ProcurementOrder
	if err := db.First(&updatedProc, proc.ID).Error; err != nil {
		t.Fatalf("load procurement: %v", err)
	}
	if updatedProc.Status != "canceled" {
		t.Errorf("expected procurement status 'canceled', got %q", updatedProc.Status)
	}

	// 验证本地订单状态从 fulfilling 回退到 paid
	var updatedOrder models.Order
	if err := db.First(&updatedOrder, order.ID).Error; err != nil {
		t.Fatalf("load order: %v", err)
	}
	if updatedOrder.Status != constants.OrderStatusPaid {
		t.Errorf("expected order status %q, got %q", constants.OrderStatusPaid, updatedOrder.Status)
	}
}

func TestHandleUpstreamCallback_Delivered_CreatesFulfillment(t *testing.T) {
	db := setupProcurementTestDB(t)

	order := createProcTestOrder(t, db, "PROC-DELIVER-001", constants.OrderStatusFulfilling, constants.FulfillmentTypeUpstream)
	proc := createTestProcurementOrder(t, db, 1, order.ID, order.OrderNo, "accepted")

	connSvc := NewSiteConnectionService(repository.NewSiteConnectionRepository(db), "test-key", t.TempDir())
	svc := newTestProcurementService(db, connSvc)

	now := time.Now()
	fulfillment := &upstream.UpstreamFulfillment{
		Type:        constants.FulfillmentTypeUpstream,
		Status:      constants.FulfillmentStatusDelivered,
		Payload:     "CDK-001\nCDK-002",
		DeliveredAt: &now,
	}

	if err := svc.HandleUpstreamCallback(proc.ID, "delivered", fulfillment); err != nil {
		t.Fatalf("HandleUpstreamCallback: %v", err)
	}

	// 验证采购单状态 = fulfilled
	var updatedProc models.ProcurementOrder
	if err := db.First(&updatedProc, proc.ID).Error; err != nil {
		t.Fatalf("load procurement: %v", err)
	}
	if updatedProc.Status != "fulfilled" {
		t.Errorf("expected procurement status 'fulfilled', got %q", updatedProc.Status)
	}

	// 验证本地订单状态 = delivered
	var updatedOrder models.Order
	if err := db.First(&updatedOrder, order.ID).Error; err != nil {
		t.Fatalf("load order: %v", err)
	}
	if updatedOrder.Status != constants.OrderStatusDelivered {
		t.Errorf("expected order status %q, got %q", constants.OrderStatusDelivered, updatedOrder.Status)
	}

	// 验证 Fulfillment 记录已创建
	var ff models.Fulfillment
	if err := db.Where("order_id = ?", order.ID).First(&ff).Error; err != nil {
		t.Fatalf("expected fulfillment record to exist: %v", err)
	}
	if ff.Payload != "CDK-001\nCDK-002" {
		t.Errorf("unexpected fulfillment payload: %q", ff.Payload)
	}
	if ff.Type != constants.FulfillmentTypeUpstream {
		t.Errorf("expected fulfillment type %q, got %q", constants.FulfillmentTypeUpstream, ff.Type)
	}
}

// ── SubmitToUpstream tests ──

func TestSubmitToUpstream_Success(t *testing.T) {
	db := setupProcurementTestDB(t)

	order := createProcTestOrder(t, db, "PROC-SUBMIT-001", constants.OrderStatusPaid, constants.FulfillmentTypeUpstream)
	// 创建 product mapping 和 sku mapping
	pm := &models.ProductMapping{
		ConnectionID:      1,
		LocalProductID:    1,
		UpstreamProductID: 101,
		IsActive:          true,
	}
	db.Create(pm)
	sm := &models.SKUMapping{
		ProductMappingID: pm.ID,
		LocalSKUID:       1,
		UpstreamSKUID:    201,
		UpstreamIsActive: true,
	}
	db.Create(sm)

	// mock upstream server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":       true,
			"order_id": 999,
			"order_no": "UP-999",
			"status":   "accepted",
			"amount":   "50.00",
			"currency": "CNY",
		})
	}))
	defer server.Close()

	connSvc := NewSiteConnectionService(repository.NewSiteConnectionRepository(db), "test-key", t.TempDir())
	conn, err := connSvc.Create(CreateConnectionInput{
		Name:      "test-upstream",
		BaseURL:   server.URL,
		ApiKey:    "key",
		ApiSecret: "secret",
		Protocol:  constants.ConnectionProtocolDujiaoNext,
	})
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}

	proc := createTestProcurementOrder(t, db, conn.ID, order.ID, order.OrderNo, "pending")

	svc := newTestProcurementService(db, connSvc)

	if err := svc.SubmitToUpstream(proc.ID); err != nil {
		t.Fatalf("SubmitToUpstream: %v", err)
	}

	// 验证采购单状态 = accepted
	var updatedProc models.ProcurementOrder
	db.First(&updatedProc, proc.ID)
	if updatedProc.Status != "accepted" {
		t.Errorf("expected procurement status 'accepted', got %q", updatedProc.Status)
	}
	if updatedProc.UpstreamOrderID != 999 {
		t.Errorf("expected upstream_order_id=999, got %d", updatedProc.UpstreamOrderID)
	}

	// 验证本地订单状态 = fulfilling
	var updatedOrder models.Order
	db.First(&updatedOrder, order.ID)
	if updatedOrder.Status != constants.OrderStatusFulfilling {
		t.Errorf("expected order status %q, got %q", constants.OrderStatusFulfilling, updatedOrder.Status)
	}
}

func TestSubmitToUpstream_NonRetryableError_Rejects(t *testing.T) {
	db := setupProcurementTestDB(t)

	order := createProcTestOrder(t, db, "PROC-NONRETRY-001", constants.OrderStatusFulfilling, constants.FulfillmentTypeUpstream)
	pm := &models.ProductMapping{ConnectionID: 1, LocalProductID: 1, UpstreamProductID: 101, IsActive: true}
	db.Create(pm)
	sm := &models.SKUMapping{ProductMappingID: pm.ID, LocalSKUID: 1, UpstreamSKUID: 201, UpstreamIsActive: true}
	db.Create(sm)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":            false,
			"error_code":    "product_out_of_stock",
			"error_message": "product out of stock",
		})
	}))
	defer server.Close()

	connSvc := NewSiteConnectionService(repository.NewSiteConnectionRepository(db), "test-key", t.TempDir())
	conn, _ := connSvc.Create(CreateConnectionInput{
		Name: "test-upstream", BaseURL: server.URL,
		ApiKey: "key", ApiSecret: "secret", Protocol: constants.ConnectionProtocolDujiaoNext,
	})

	proc := createTestProcurementOrder(t, db, conn.ID, order.ID, order.OrderNo, "pending")
	svc := newTestProcurementService(db, connSvc)

	// 不可重试错误应返回 error
	_ = svc.SubmitToUpstream(proc.ID)

	// 验证采购单状态 = rejected
	var updatedProc models.ProcurementOrder
	db.First(&updatedProc, proc.ID)
	if updatedProc.Status != "rejected" {
		t.Errorf("expected procurement status 'rejected', got %q", updatedProc.Status)
	}

	// 验证本地订单状态回退到 paid
	var updatedOrder models.Order
	db.First(&updatedOrder, order.ID)
	if updatedOrder.Status != constants.OrderStatusPaid {
		t.Errorf("expected order status %q after rejection, got %q", constants.OrderStatusPaid, updatedOrder.Status)
	}
}

func TestSubmitToUpstream_RetryableError_Retries(t *testing.T) {
	db := setupProcurementTestDB(t)

	order := createProcTestOrder(t, db, "PROC-RETRY-001", constants.OrderStatusFulfilling, constants.FulfillmentTypeUpstream)
	pm := &models.ProductMapping{ConnectionID: 1, LocalProductID: 1, UpstreamProductID: 101, IsActive: true}
	db.Create(pm)
	sm := &models.SKUMapping{ProductMappingID: pm.ID, LocalSKUID: 1, UpstreamSKUID: 201, UpstreamIsActive: true}
	db.Create(sm)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":            false,
			"error_code":    "server_error",
			"error_message": "temporary failure",
		})
	}))
	defer server.Close()

	connSvc := NewSiteConnectionService(repository.NewSiteConnectionRepository(db), "test-key", t.TempDir())
	conn, _ := connSvc.Create(CreateConnectionInput{
		Name: "test-upstream", BaseURL: server.URL,
		ApiKey: "key", ApiSecret: "secret", Protocol: constants.ConnectionProtocolDujiaoNext,
		RetryMax: 3,
	})

	proc := createTestProcurementOrder(t, db, conn.ID, order.ID, order.OrderNo, "pending")
	svc := newTestProcurementService(db, connSvc)

	// 可重试错误不应返回 error（已入队重试）
	if err := svc.SubmitToUpstream(proc.ID); err != nil {
		t.Fatalf("expected no error for retryable failure, got: %v", err)
	}

	// 验证采购单状态 = failed（而非 rejected）
	var updatedProc models.ProcurementOrder
	db.First(&updatedProc, proc.ID)
	if updatedProc.Status != "failed" {
		t.Errorf("expected procurement status 'failed', got %q", updatedProc.Status)
	}
	if updatedProc.RetryCount != 1 {
		t.Errorf("expected retry_count=1, got %d", updatedProc.RetryCount)
	}
}

func TestHandleSubmitFailure_MaxRetriesExhausted(t *testing.T) {
	db := setupProcurementTestDB(t)

	order := createProcTestOrder(t, db, "PROC-MAXRETRY-001", constants.OrderStatusFulfilling, constants.FulfillmentTypeUpstream)

	connSvc := NewSiteConnectionService(repository.NewSiteConnectionRepository(db), "test-key", t.TempDir())
	conn := &models.SiteConnection{
		RetryMax:       2,
		RetryIntervals: "[30,60]",
	}
	db.Create(conn)

	proc := createTestProcurementOrder(t, db, conn.ID, order.ID, order.OrderNo, "failed")
	// 设置 retry_count 已达上限
	db.Model(proc).Update("retry_count", 2)

	svc := newTestProcurementService(db, connSvc)

	// 模拟调用 handleSubmitFailure（可重试但已达上限）
	_ = svc.handleSubmitFailure(proc, conn, "timeout after retries", true)

	// 验证采购单状态 = rejected
	var updatedProc models.ProcurementOrder
	db.First(&updatedProc, proc.ID)
	if updatedProc.Status != "rejected" {
		t.Errorf("expected procurement status 'rejected', got %q", updatedProc.Status)
	}

	// 验证本地订单回退到 paid
	var updatedOrder models.Order
	db.First(&updatedOrder, order.ID)
	if updatedOrder.Status != constants.OrderStatusPaid {
		t.Errorf("expected order status %q, got %q", constants.OrderStatusPaid, updatedOrder.Status)
	}
}

// ── CreateForOrder tests ──

func TestCreateForOrder_SkipsNonUpstreamItems(t *testing.T) {
	db := setupProcurementTestDB(t)

	order := createProcTestOrder(t, db, "PROC-SKIP-001", constants.OrderStatusPaid, constants.FulfillmentTypeAuto)

	connSvc := NewSiteConnectionService(repository.NewSiteConnectionRepository(db), "test-key", t.TempDir())
	svc := newTestProcurementService(db, connSvc)

	if err := svc.CreateForOrder(order.ID); err != nil {
		t.Fatalf("CreateForOrder: %v", err)
	}

	// 验证没有创建采购单
	var count int64
	db.Model(&models.ProcurementOrder{}).Count(&count)
	if count != 0 {
		t.Errorf("expected no procurement orders for auto fulfillment, got %d", count)
	}
}

func TestCreateForOrder_IdempotentSkipsDuplicate(t *testing.T) {
	db := setupProcurementTestDB(t)

	order := createProcTestOrder(t, db, "PROC-DUP-001", constants.OrderStatusPaid, constants.FulfillmentTypeUpstream)
	pm := &models.ProductMapping{ConnectionID: 1, LocalProductID: 1, UpstreamProductID: 101, IsActive: true}
	db.Create(pm)

	connSvc := NewSiteConnectionService(repository.NewSiteConnectionRepository(db), "test-key", t.TempDir())
	svc := newTestProcurementService(db, connSvc)

	// 第一次创建成功
	if err := svc.CreateForOrder(order.ID); err != nil {
		t.Fatalf("first CreateForOrder: %v", err)
	}

	// 第二次应该返回 ErrProcurementExists
	err := svc.CreateForOrder(order.ID)
	if err != ErrProcurementExists {
		t.Errorf("expected ErrProcurementExists on duplicate, got: %v", err)
	}
}

// ── PollUpstreamStatus test ──

func TestPollUpstreamStatus_Delivered(t *testing.T) {
	db := setupProcurementTestDB(t)

	order := createProcTestOrder(t, db, "PROC-POLL-001", constants.OrderStatusFulfilling, constants.FulfillmentTypeUpstream)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		now := time.Now()
		json.NewEncoder(w).Encode(map[string]any{
			"order_id": 999,
			"order_no": "UP-999",
			"status":   "delivered",
			"amount":   "50.00",
			"currency": "CNY",
			"fulfillment": map[string]any{
				"type":         "auto",
				"status":       "delivered",
				"payload":      "KEY-001\nKEY-002",
				"delivered_at": now.Format(time.RFC3339),
			},
		})
	}))
	defer server.Close()

	connSvc := NewSiteConnectionService(repository.NewSiteConnectionRepository(db), "test-key", t.TempDir())
	conn, _ := connSvc.Create(CreateConnectionInput{
		Name: "poll-upstream", BaseURL: server.URL,
		ApiKey: "key", ApiSecret: "secret", Protocol: constants.ConnectionProtocolDujiaoNext,
	})

	proc := createTestProcurementOrder(t, db, conn.ID, order.ID, order.OrderNo, "accepted")
	db.Model(proc).Updates(map[string]interface{}{
		"upstream_order_id": uint(999),
		"upstream_order_no": "UP-999",
	})

	svc := newTestProcurementService(db, connSvc)

	if err := svc.PollUpstreamStatus(proc.ID); err != nil {
		t.Fatalf("PollUpstreamStatus: %v", err)
	}

	// 验证采购单状态 = fulfilled
	var updatedProc models.ProcurementOrder
	db.First(&updatedProc, proc.ID)
	if updatedProc.Status != "fulfilled" {
		t.Errorf("expected procurement status 'fulfilled', got %q", updatedProc.Status)
	}

	// 验证本地订单状态 = delivered
	var updatedOrder models.Order
	db.First(&updatedOrder, order.ID)
	if updatedOrder.Status != constants.OrderStatusDelivered {
		t.Errorf("expected order status %q, got %q", constants.OrderStatusDelivered, updatedOrder.Status)
	}
}

// ── Unit tests for pure functions ──

func TestParseRetryIntervals(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []time.Duration
	}{
		{
			name:     "empty string returns defaults",
			input:    "",
			expected: []time.Duration{30 * time.Second, 60 * time.Second, 300 * time.Second},
		},
		{
			name:     "valid array",
			input:    "[10,20,30]",
			expected: []time.Duration{10 * time.Second, 20 * time.Second, 30 * time.Second},
		},
		{
			name:     "with spaces",
			input:    "[ 10 , 20 , 30 ]",
			expected: []time.Duration{10 * time.Second, 20 * time.Second, 30 * time.Second},
		},
		{
			name:     "invalid entries skipped",
			input:    "[10,abc,30]",
			expected: []time.Duration{10 * time.Second, 30 * time.Second},
		},
		{
			name:     "all invalid returns defaults",
			input:    "[abc,def]",
			expected: []time.Duration{30 * time.Second, 60 * time.Second, 300 * time.Second},
		},
		{
			name:     "negative values skipped",
			input:    "[10,-5,30]",
			expected: []time.Duration{10 * time.Second, 30 * time.Second},
		},
		{
			name:     "zero values skipped",
			input:    "[0,10]",
			expected: []time.Duration{10 * time.Second},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseRetryIntervals(tt.input)
			if len(result) != len(tt.expected) {
				t.Fatalf("expected %d intervals, got %d: %v", len(tt.expected), len(result), result)
			}
			for i, d := range result {
				if d != tt.expected[i] {
					t.Errorf("interval[%d]: expected %v, got %v", i, tt.expected[i], d)
				}
			}
		})
	}
}

func TestIsRetryableErrorCode(t *testing.T) {
	nonRetryable := []string{
		"insufficient_balance",
		"payment_failed",
		"product_unavailable",
		"sku_unavailable",
		"invalid_request",
		"unauthorized",
		"forbidden",
		"duplicate_order",
		"product_out_of_stock",
	}
	for _, code := range nonRetryable {
		if isRetryableErrorCode(code) {
			t.Errorf("expected %q to be non-retryable", code)
		}
	}

	retryable := []string{
		"server_error",
		"timeout",
		"network_error",
		"unknown_error",
		"",
	}
	for _, code := range retryable {
		if !isRetryableErrorCode(code) {
			t.Errorf("expected %q to be retryable", code)
		}
	}

	// 测试带空格的情况
	if isRetryableErrorCode("  unauthorized  ") {
		t.Error("expected trimmed 'unauthorized' to be non-retryable")
	}
}
