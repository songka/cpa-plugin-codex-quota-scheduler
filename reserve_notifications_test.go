package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSendQuotaNotificationRejectsProviderErrorAndRedirect(t *testing.T) {
	providerError := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errcode":400,"errmsg":"Invalid request parameters"}`))
	}))
	defer providerError.Close()

	policy := defaultTelemetryPolicy()
	policy.NotificationsEnabled = true
	policy.NotificationKind = "wecom"
	policy.WebhookURL = providerError.URL
	if err := sendQuotaNotification(context.Background(), policy, QuotaNotification{Title: "test"}); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("provider error was accepted: %v", err)
	}

	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/target", http.StatusFound)
	}))
	defer redirect.Close()
	policy.NotificationKind = "generic"
	policy.WebhookURL = redirect.URL
	if err := sendQuotaNotification(context.Background(), policy, QuotaNotification{Title: "test"}); err == nil || !strings.Contains(err.Error(), "HTTP status 302") {
		t.Fatalf("redirect was followed or accepted: %v", err)
	}
}
