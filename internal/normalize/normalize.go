// Package normalize turns raw marketplace text and dates into the canonical
// forms the mapping configs are keyed on.
//
// The assignment warns that "matching is case- and separator-sensitive in ways
// the raw files are not always consistent". Concretely, in these two files:
//
//	payments  type        "Service fee"                  -> SERVICE_FEE
//	payments  description "FBA Inventory Reimbursement - Damaged:Warehouse"
//	                                                     -> FBA_INVENTORY_REIMBURSEMENT_-_DAMAGED:WAREHOUSE
//	settlement transaction-type "other-transaction"      -> OTHER-TRANSACTION
//	settlement amount-type      "FBA Inventory Reimbursement"
//	                                                     -> FBA_INVENTORY_REIMBURSEMENT
//	settlement amount-desc      "Base fee"               -> BASE_FEE
//
// i.e. upper-case, and collapse runs of whitespace to a single underscore.
// Hyphens, colons, parentheses and ampersands are significant and preserved —
// the config keys contain them.
package normalize

import (
	"fmt"
	"strings"
	"time"
)

var wsCollapse = strings.NewReplacer(" ", " ", "\t", " ", "\r", " ", "\n", " ")

// Key canonicalises a raw source value into config-key form.
func Key(s string) string {
	s = wsCollapse.Replace(s)
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// collapse internal whitespace runs, then swap for underscores
	fields := strings.Fields(s)
	return strings.ToUpper(strings.Join(fields, "_"))
}

// Text trims a value but otherwise leaves it alone (used for identifiers such
// as order ids and SKUs, where case and punctuation are meaningful as-is).
func Text(s string) string {
	return strings.TrimSpace(wsCollapse.Replace(s))
}

var monthByPrefix = map[string]time.Month{
	"jan": time.January, "feb": time.February, "mar": time.March,
	"apr": time.April, "may": time.May, "jun": time.June,
	"jul": time.July, "aug": time.August, "sep": time.September,
	"oct": time.October, "nov": time.November, "dec": time.December,
}

// PaymentsTime parses the Amazon payments report timestamp format, e.g.
//
//	"29 June 2026 5:39:32 pm GMT+9"
//	"1 Aug 2026 6:10:51 am GMT+9"      (month is sometimes abbreviated)
//
// and returns it as UTC. The offset in the string is authoritative; we do not
// assume a fixed marketplace timezone.
func PaymentsTime(s string) (time.Time, error) {
	s = Text(s)
	if s == "" {
		return time.Time{}, nil
	}
	parts := strings.Fields(s)
	if len(parts) != 6 {
		return time.Time{}, fmt.Errorf("payments timestamp %q: expected 6 fields, got %d", s, len(parts))
	}
	day, mon, year, clock, meridiem, zone := parts[0], parts[1], parts[2], parts[3], strings.ToLower(parts[4]), parts[5]

	var d, y int
	if _, err := fmt.Sscanf(day, "%d", &d); err != nil {
		return time.Time{}, fmt.Errorf("payments timestamp %q: bad day: %w", s, err)
	}
	if _, err := fmt.Sscanf(year, "%d", &y); err != nil {
		return time.Time{}, fmt.Errorf("payments timestamp %q: bad year: %w", s, err)
	}
	if len(mon) < 3 {
		return time.Time{}, fmt.Errorf("payments timestamp %q: bad month %q", s, mon)
	}
	month, ok := monthByPrefix[strings.ToLower(mon[:3])]
	if !ok {
		return time.Time{}, fmt.Errorf("payments timestamp %q: unknown month %q", s, mon)
	}

	var hh, mm, ss int
	if _, err := fmt.Sscanf(clock, "%d:%d:%d", &hh, &mm, &ss); err != nil {
		return time.Time{}, fmt.Errorf("payments timestamp %q: bad clock: %w", s, err)
	}
	switch meridiem {
	case "am":
		if hh == 12 {
			hh = 0
		}
	case "pm":
		if hh != 12 {
			hh += 12
		}
	default:
		return time.Time{}, fmt.Errorf("payments timestamp %q: bad meridiem %q", s, meridiem)
	}

	offset, err := gmtOffsetSeconds(zone)
	if err != nil {
		return time.Time{}, fmt.Errorf("payments timestamp %q: %w", s, err)
	}
	return time.Date(y, month, d, hh, mm, ss, 0, time.FixedZone(zone, offset)).UTC(), nil
}

// gmtOffsetSeconds parses "GMT+9", "GMT+10", "GMT-3:30", "UTC".
func gmtOffsetSeconds(zone string) (int, error) {
	z := strings.ToUpper(strings.TrimSpace(zone))
	z = strings.TrimPrefix(strings.TrimPrefix(z, "GMT"), "UTC")
	if z == "" {
		return 0, nil
	}
	sign := 1
	switch z[0] {
	case '+':
	case '-':
		sign = -1
	default:
		return 0, fmt.Errorf("unrecognised timezone %q", zone)
	}
	body := z[1:]
	hours, mins := 0, 0
	if h, m, ok := strings.Cut(body, ":"); ok {
		if _, err := fmt.Sscanf(h, "%d", &hours); err != nil {
			return 0, fmt.Errorf("unrecognised timezone %q", zone)
		}
		if _, err := fmt.Sscanf(m, "%d", &mins); err != nil {
			return 0, fmt.Errorf("unrecognised timezone %q", zone)
		}
	} else if _, err := fmt.Sscanf(body, "%d", &hours); err != nil {
		return 0, fmt.Errorf("unrecognised timezone %q", zone)
	}
	return sign * (hours*3600 + mins*60), nil
}

// SettlementTime parses the settlement flat-file timestamp formats:
//
//	"17.07.2026 07:26:32 UTC"   (posted-date-time, settlement-start-date, ...)
//	"17.07.2026"                (posted-date)
func SettlementTime(s string) (time.Time, error) {
	s = Text(s)
	if s == "" {
		return time.Time{}, nil
	}
	s = strings.TrimSuffix(s, " UTC")
	for _, layout := range []string{"02.01.2006 15:04:05", "02.01.2006"} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("settlement timestamp %q: unrecognised format", s)
}

// DateToken renders a timestamp as the canonical `date` component of a
// record_ref. Empty time -> empty string, so that a record_ref built from a
// row with no usable date is visibly incomplete rather than silently wrong.
func DateToken(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02")
}
