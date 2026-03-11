package worker

import (
	"testing"

	"github.com/dujiao-next/internal/models"
)

func TestBuildBotNotifyRequestURLReplacesPath(t *testing.T) {
	got, err := buildBotNotifyRequestURL("https://bot.example.com/internal/order-fulfilled", "/internal/wallet-recharge-succeeded")
	if err != nil {
		t.Fatalf("build bot notify request url failed: %v", err)
	}
	want := "https://bot.example.com/internal/wallet-recharge-succeeded"
	if got != want {
		t.Fatalf("request url want %s got %s", want, got)
	}
}

func TestBuildOrderFulfillmentEmailPayloadNilOrder(t *testing.T) {
	if got := buildOrderFulfillmentEmailPayload(nil); got != "" {
		t.Fatalf("expected empty payload for nil order, got %q", got)
	}
}

func TestBuildOrderFulfillmentEmailPayloadPreferOrderFulfillment(t *testing.T) {
	order := &models.Order{
		Fulfillment: &models.Fulfillment{Payload: "  MAIN-LINE-1\nMAIN-LINE-2  "},
		Children: []models.Order{
			{
				OrderNo:     "CHILD-1",
				Fulfillment: &models.Fulfillment{Payload: "SECRET-1"},
			},
		},
	}

	got := buildOrderFulfillmentEmailPayload(order)
	want := "MAIN-LINE-1\nMAIN-LINE-2"
	if got != want {
		t.Fatalf("unexpected payload, want %q, got %q", want, got)
	}
}

func TestBuildOrderFulfillmentEmailPayloadFromChildren(t *testing.T) {
	order := &models.Order{
		Children: []models.Order{
			{
				OrderNo:     "DJ-CHILD-01",
				Fulfillment: &models.Fulfillment{Payload: "  SECRET-01  "},
			},
			{
				OrderNo:     "DJ-CHILD-02",
				Fulfillment: nil,
			},
			{
				OrderNo:     "DJ-CHILD-03",
				Fulfillment: &models.Fulfillment{Payload: "    "},
			},
			{
				OrderNo:     "DJ-CHILD-04",
				Fulfillment: &models.Fulfillment{Payload: "SECRET-04-L1\nSECRET-04-L2"},
			},
		},
	}

	got := buildOrderFulfillmentEmailPayload(order)
	want := "[DJ-CHILD-01]\nSECRET-01\n\n[DJ-CHILD-04]\nSECRET-04-L1\nSECRET-04-L2"
	if got != want {
		t.Fatalf("unexpected payload, want %q, got %q", want, got)
	}
}
