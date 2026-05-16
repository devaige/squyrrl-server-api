package subscriptions

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestVerifyStripeSignature_OK(t *testing.T) {
	secret := "whsec_test"
	body := []byte(`{"id":"evt_1","type":"customer.subscription.created"}`)
	ts := fmt.Sprintf("%d", time.Now().Unix())
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))
	header := fmt.Sprintf("t=%s,v1=%s", ts, sig)
	if err := VerifyStripeSignature(body, header, secret, 5*time.Minute); err != nil {
		t.Fatalf("expect verify ok, got %v", err)
	}
}

func TestVerifyStripeSignature_BadHMAC(t *testing.T) {
	ts := fmt.Sprintf("%d", time.Now().Unix())
	header := fmt.Sprintf("t=%s,v1=deadbeef", ts)
	if err := VerifyStripeSignature([]byte("hello"), header, "wrong", time.Minute); err == nil {
		t.Fatal("expect bad-signature error")
	}
}

func TestParseStripeEvent_SubscriptionCreated(t *testing.T) {
	uid := uuid.New().String()
	body := []byte(fmt.Sprintf(`{
		"id":"evt_1",
		"type":"customer.subscription.created",
		"data":{"object":{
			"id":"sub_xyz",
			"status":"active",
			"current_period_start":%d,
			"current_period_end":%d,
			"metadata":{"squyrrl_user_id":"%s","squyrrl_tier":"basic","squyrrl_period":"monthly"}
		}}
	}`, time.Now().Unix(), time.Now().Add(30*24*time.Hour).Unix(), uid))
	evt, err := ParseStripeEvent(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if evt.Tier != "basic" || evt.Status != "active" || evt.BillingPeriod != "monthly" {
		t.Fatalf("unexpected event: %+v", evt)
	}
	if evt.UserID.String() != uid {
		t.Fatalf("user mismatch: %s vs %s", evt.UserID, uid)
	}
}

func TestParseStripeEvent_UnhandledType(t *testing.T) {
	body := []byte(`{"id":"evt_2","type":"invoice.paid","data":{"object":{}}}`)
	if _, err := ParseStripeEvent(body); err != ErrUnknownEvent {
		t.Fatalf("expect ErrUnknownEvent, got %v", err)
	}
}
