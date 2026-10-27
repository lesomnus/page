package app

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestNotificationConfigurationAndRedaction(t *testing.T) {
	t.Setenv("GITHUB_WEBHOOK_SECRET", "")
	c, err := Load(configFile(t, "base_domain: example.com\n"))
	if err != nil || c.RefreshInterval != 5*time.Minute || c.WebhookListen != "" {
		t.Fatalf("unexpected notification defaults: %v", err)
	}
	if _, err := Load(configFile(t, "webhook_listen: '127.0.0.1:8081'\n")); err == nil {
		t.Fatal("enabled an unauthenticated webhook listener")
	}
	const secret = "webhook-fixture-must-be-redacted"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)
	file := configFile(t, "webhook_listen: '127.0.0.1:8081'\n")
	c, err = Load(file)
	if err != nil || c.WebhookSecret != secret {
		t.Fatalf("webhook environment was not loaded: %v", err)
	}
	var out, stderr bytes.Buffer
	if err := Run(context.Background(), []string{"config", "--config", file}, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), secret) || !strings.Contains(out.String(), "webhook_secret: <redacted>") {
		t.Fatal("config did not redact the webhook credential")
	}
	t.Setenv("PAGE_TEST_WEBHOOK_SECRET", "expanded-fixture")
	c, err = Load(configFile(t, "webhook_listen: '127.0.0.1:8081'\nwebhook_secret: ${PAGE_TEST_WEBHOOK_SECRET}\n"))
	if err != nil || c.WebhookSecret != "expanded-fixture" {
		t.Fatalf("explicit webhook secret environment expansion failed: %v", err)
	}
}
