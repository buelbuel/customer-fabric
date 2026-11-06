// Package enterprise generates synthetic test assets. Production code must not import it.
package enterprise

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"time"
)

type Config struct {
	Seed          int64              `json:"seed"`
	ReferenceDate string             `json:"reference_date"`
	Population    int                `json:"population"`
	Cohorts       map[string]float64 `json:"cohorts"`
	Anomalies     map[string]float64 `json:"anomalies"`
}

var Cohorts = []string{"crm_only", "marketing_only", "ecommerce_only", "dormant_erp", "multichannel", "fragmented"}
var Anomalies = []string{"duplicate_source", "multiple_emails", "shared_email", "name_change", "phone_change", "reused_identifier", "missing_external_id", "conflicting_values", "partial_migration", "inactive", "deleted_downstream", "missing_reference", "consent_lag", "schema_variation", "late_event", "duplicate_event", "out_of_order", "partial_batch", "malformed", "backfill", "owner_change", "downtime", "retry_storm", "recreated", "identifier_correction", "anonymous", "address_change", "loyalty_delay"}

func DefaultConfig() Config {
	c := Config{Seed: 42, ReferenceDate: "2026-11-01", Population: 10000, Cohorts: map[string]float64{"crm_only": .15, "marketing_only": .15, "ecommerce_only": .15, "dormant_erp": .15, "multichannel": .30, "fragmented": .10}, Anomalies: map[string]float64{}}
	for _, name := range Anomalies {
		c.Anomalies[name] = .04
	}
	return c
}
func LoadConfig(path string) (Config, error) {
	c := DefaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	// A complete config avoids accidental defaults for misspelled scenario keys.
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&c); err != nil {
		return c, err
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		return c, fmt.Errorf("config must contain one JSON object")
	}
	return c, c.Validate()
}
func (c Config) Validate() error {
	if c.Population < 2 || c.Population > 1000000 {
		return fmt.Errorf("population must be 2..1000000")
	}
	ref, err := time.Parse("2006-01-02", c.ReferenceDate)
	if err != nil || ref.Before(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) || ref.After(time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)) {
		return fmt.Errorf("reference_date must be in 2026")
	}
	sum := 0.0
	for k, v := range c.Cohorts {
		if !contains(Cohorts, k) || math.IsNaN(v) || v < 0 || v > 1 {
			return fmt.Errorf("invalid cohort %q", k)
		}
		sum += v
	}
	if math.Abs(sum-1) > 1e-9 {
		return fmt.Errorf("cohort probabilities must sum to 1")
	}
	for k, v := range c.Anomalies {
		if !contains(Anomalies, k) || math.IsNaN(v) || v < 0 || v > 1 {
			return fmt.Errorf("invalid anomaly %q", k)
		}
	}
	return nil
}
func contains(a []string, k string) bool {
	for _, v := range a {
		if v == k {
			return true
		}
	}
	return false
}

// Hash-based independent draws make anomaly frequencies independent of iteration order.
func (c Config) draw(person int, key string) float64 {
	h := sha256.Sum256([]byte(fmt.Sprintf("%d/%d/%s", c.Seed, person, key)))
	return float64(binary.BigEndian.Uint64(h[:8])>>11) / (1 << 53)
}
func (c Config) flags(i int) []string {
	var flags []string
	for _, name := range Anomalies {
		drawPerson := i
		if name == "shared_email" || name == "reused_identifier" {
			drawPerson = i - i%2
		}
		if c.draw(drawPerson, name) < c.Anomalies[name] {
			flags = append(flags, name)
		}
	}
	sort.Strings(flags)
	return flags
}
func (c Config) cohort(i int) string {
	x := c.draw(i, "cohort")
	for _, k := range Cohorts {
		x -= c.Cohorts[k]
		if x < 0 {
			return k
		}
	}
	return Cohorts[len(Cohorts)-1]
}
