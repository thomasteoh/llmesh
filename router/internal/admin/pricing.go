package admin

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Per-model token pricing. Rates are configured in the portal and applied to
// recorded token counts at query time, so editing a rate reprices history —
// which is what makes it possible to put a figure on models that were never
// priced when their traffic ran.
//
// Money is handled in integer micro-units of the configured currency
// (1 unit = 1_000_000 micro). Rates are stored as micro-units per million
// tokens, so a $2.50/Mtok rate is 2_500_000. Cost for n tokens is therefore
// n * ppm / 1_000_000, and the division is deferred until after summing a whole
// group so truncation happens once per reported figure rather than per row.

// microPerUnit is the fixed-point scale for currency amounts.
const microPerUnit = 1_000_000

// tokensPerRateUnit is the token count a configured rate applies to.
const tokensPerRateUnit = 1_000_000

// Pricing bases. A rate is either what a provider actually charges, or a figure
// modelled locally. The distinction is the point of the feature: totals are
// reported split, so a modelled number can never be mistaken for a real invoice.
const (
	BasisActual    = "actual"
	BasisEstimated = "estimated"
)

// defaultCurrency is used until an admin sets one.
const defaultCurrency = "USD"

// currencySettingKey is the settings-table key holding the display currency.
const currencySettingKey = "cost.currency"

// maxRatePerMtok caps a configured rate. Well above any real model price, low
// enough that summing tokens * ppm across a 90-day window cannot overflow int64.
const maxRatePerMtok = 1_000_000.0

// ModelPricing is the configured rate for one model.
type ModelPricing struct {
	Model     string
	InputPPM  int64  // micro-currency per million prompt tokens
	OutputPPM int64  // micro-currency per million completion tokens
	Basis     string // BasisActual | BasisEstimated
	UpdatedAt string // RFC3339; empty if never written
}

// Priced reports whether this row would produce any cost. A row of all zeroes is
// indistinguishable from having no row at all, and both must count as unpriced so
// the portal can say how much traffic is missing from a total.
func (p ModelPricing) Priced() bool { return p.InputPPM > 0 || p.OutputPPM > 0 }

// validBasis normalises a basis string, defaulting to estimated. Defaulting to
// estimated rather than actual keeps the system from claiming a charge it cannot
// substantiate.
func validBasis(s string) string {
	if strings.TrimSpace(strings.ToLower(s)) == BasisActual {
		return BasisActual
	}
	return BasisEstimated
}

// ParseRatePerMtok converts a human-entered rate per million tokens ("2.50")
// into micro-units per million tokens. Blank means zero — the way to clear a rate.
func ParseRatePerMtok(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	// Tolerate a leading currency symbol and thousands separators, since rates
	// are usually pasted from a provider's pricing page.
	s = strings.TrimLeft(s, "$£€¥ ")
	s = strings.ReplaceAll(s, ",", "")
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("rate %q is not a number", s)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("rate %q is not a finite number", s)
	}
	if f < 0 {
		return 0, fmt.Errorf("rate must not be negative")
	}
	if f > maxRatePerMtok {
		return 0, fmt.Errorf("rate must not exceed %.0f per million tokens", maxRatePerMtok)
	}
	return int64(math.Round(f * microPerUnit)), nil
}

// FormatRatePerMtok renders a stored rate for display in an input field. Trailing
// zeroes are trimmed so a round rate reads as "2.5" rather than "2.500000", but
// sub-cent rates keep the precision they were entered with.
func FormatRatePerMtok(ppm int64) string {
	if ppm == 0 {
		return ""
	}
	s := strconv.FormatFloat(float64(ppm)/microPerUnit, 'f', 6, 64)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// FormatMoney renders a micro-unit amount. Small amounts keep more decimals: a
// per-request cost rounded to two places would read as 0.00 and look like a bug.
func FormatMoney(micro int64) string {
	neg := micro < 0
	if neg {
		micro = -micro
	}
	v := float64(micro) / microPerUnit
	var s string
	switch {
	case micro == 0:
		return "0.00"
	case v >= 1:
		s = strconv.FormatFloat(v, 'f', 2, 64)
	case v >= 0.01:
		s = strconv.FormatFloat(v, 'f', 4, 64)
	case v >= 0.000001:
		s = strconv.FormatFloat(v, 'f', 6, 64)
	default:
		// Non-zero but below the resolution we store; saying 0.00 would be a lie.
		s = "<0.000001"
	}
	if neg {
		return "-" + s
	}
	return s
}

// CostMicro returns the cost of the given token counts under this rate, in
// micro-units. Used for single-figure computations; bulk aggregation does the
// same arithmetic in SQL.
func (p ModelPricing) CostMicro(promptTokens, completionTokens int64) int64 {
	return (promptTokens*p.InputPPM + completionTokens*p.OutputPPM) / tokensPerRateUnit
}

// --- Storage ---

// ModelPricingAll returns every configured rate, keyed by model.
func (s *State) ModelPricingAll() (map[string]ModelPricing, error) {
	rows, err := s.db.Query(`SELECT model, input_ppm, output_ppm, basis, updated_at FROM model_pricing ORDER BY model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]ModelPricing)
	for rows.Next() {
		var p ModelPricing
		if err := rows.Scan(&p.Model, &p.InputPPM, &p.OutputPPM, &p.Basis, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out[p.Model] = p
	}
	return out, rows.Err()
}

// SetModelPricing upserts one model's rate. A rate of zero on both sides is kept
// rather than deleted, so an explicit "this model is free" survives a page
// reload instead of reappearing as unpriced.
func (s *State) SetModelPricing(model string, inputPPM, outputPPM int64, basis string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return fmt.Errorf("model must not be blank")
	}
	if inputPPM < 0 || outputPPM < 0 {
		return fmt.Errorf("rates must not be negative")
	}
	_, err := s.db.Exec(`
		INSERT INTO model_pricing (model, input_ppm, output_ppm, basis, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(model) DO UPDATE SET
			input_ppm  = excluded.input_ppm,
			output_ppm = excluded.output_ppm,
			basis      = excluded.basis,
			updated_at = excluded.updated_at`,
		model, inputPPM, outputPPM, validBasis(basis), time.Now().UTC().Format(time.RFC3339))
	return err
}

// DeleteModelPricing removes a model's rate, returning it to unpriced.
func (s *State) DeleteModelPricing(model string) error {
	res, err := s.db.Exec(`DELETE FROM model_pricing WHERE model = ?`, model)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no pricing configured for model %q", model)
	}
	return nil
}

// CostCurrency returns the configured display currency, defaulting to USD.
func (s *State) CostCurrency() string {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, currencySettingKey).Scan(&v)
	if err != nil || strings.TrimSpace(v) == "" {
		return defaultCurrency
	}
	return v
}

// SetCostCurrency persists the display currency. It is a label only: llmesh does
// no conversion, so mixing currencies across models would silently add unlike
// amounts together.
func (s *State) SetCostCurrency(code string) error {
	code = strings.TrimSpace(code)
	if code == "" {
		code = defaultCurrency
	}
	if len(code) > 8 {
		return fmt.Errorf("currency code must be 8 characters or fewer")
	}
	_, err := s.db.Exec(
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		currencySettingKey, code)
	return err
}
