package platform

import (
	"testing"
	"time"
)

func TestRetentionKeepsDeliveryGuaranteesAndBounds(t *testing.T) {
	if err := (Retention{}).WithDefaults().Validate(); err != nil {
		t.Fatalf("defaults: %v", err)
	}
	for name, r := range map[string]Retention{
		"kafka beyond 14 days":                 {Outbox: 45 * day, Notifications: 90 * day, Kafka: 15 * day},
		"notifications within recovery window": {Outbox: 45 * day, Notifications: 30 * day, Kafka: 7 * day},
		"outbox within recovery window":        {Outbox: 30 * day, Notifications: 90 * day, Kafka: 7 * day},
		"outbox without a week's margin":       {Outbox: 36 * day, Notifications: 90 * day, Kafka: 7 * day},
		"outbox beyond a year":                 {Outbox: 366 * day, Notifications: 90 * day, Kafka: 7 * day},
		"notifications beyond a year":          {Outbox: 45 * day, Notifications: 400 * day, Kafka: 7 * day},
	} {
		if r.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if (Config{Mode: "development", Retention: Retention{Kafka: 30 * day}}).Validate() == nil {
		t.Fatal("a configuration with unbounded Kafka retention was accepted")
	}
	if got := (Retention{Outbox: 40 * day}).WithDefaults(); got.Outbox != 40*day || got.Notifications != 90*day || got.Kafka != 7*24*time.Hour {
		t.Fatalf("defaults %+v", got)
	}
}
