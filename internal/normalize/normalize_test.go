package normalize

import (
	"testing"
	"time"
)

func TestKey(t *testing.T) {
	cases := map[string]string{
		"Service fee":                     "SERVICE_FEE",
		"  Order  ":                       "ORDER",
		"other-transaction":               "OTHER-TRANSACTION",
		"Base fee":                        "BASE_FEE",
		"FBA Inventory Reimbursement":     "FBA_INVENTORY_REIMBURSEMENT",
		"Fulfilment by Amazon (FBA) Inventory Storage Fee": "FULFILMENT_BY_AMAZON_(FBA)_INVENTORY_STORAGE_FEE",
		"FBA Inventory Reimbursement - Damaged:Warehouse":  "FBA_INVENTORY_REIMBURSEMENT_-_DAMAGED:WAREHOUSE",
		"Amazon Warehousing & Distribution (AWD)":          "AMAZON_WAREHOUSING_&_DISTRIBUTION_(AWD)",
		"To account ending with: 334":                      "TO_ACCOUNT_ENDING_WITH:_334",
		"multiple   internal   spaces":                     "MULTIPLE_INTERNAL_SPACES",
		"":                                                 "",
		"   ":                                              "",
	}
	for in, want := range cases {
		if got := Key(in); got != want {
			t.Errorf("Key(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPaymentsTime(t *testing.T) {
	cases := []struct {
		in   string
		want string // UTC
	}{
		// the offset in the string is authoritative, never an assumed locale
		{"29 June 2026 5:39:32 pm GMT+9", "2026-06-29T08:39:32Z"},
		{"18 July 2026 6:10:51 am GMT+9", "2026-07-17T21:10:51Z"},
		// midnight / noon are the classic 12-hour conversion traps
		{"3 July 2026 12:15:48 am GMT+9", "2026-07-02T15:15:48Z"},
		{"26 July 2026 12:59:44 pm GMT+9", "2026-07-26T03:59:44Z"},
		// negative and half-hour offsets
		{"5 May 2026 10:00:00 am GMT-3", "2026-05-05T13:00:00Z"},
		{"5 May 2026 10:00:00 am GMT+5:30", "2026-05-05T04:30:00Z"},
	}
	for _, c := range cases {
		got, err := PaymentsTime(c.in)
		if err != nil {
			t.Errorf("PaymentsTime(%q): %v", c.in, err)
			continue
		}
		want, err := time.Parse(time.RFC3339, c.want)
		if err != nil {
			t.Fatalf("bad test fixture %q: %v", c.want, err)
		}
		if !got.Equal(want) {
			t.Errorf("PaymentsTime(%q) = %s, want %s", c.in, got.Format(time.RFC3339), c.want)
		}
	}
}

func TestPaymentsTimeAbbreviatedMonth(t *testing.T) {
	got, err := PaymentsTime("1 Aug 2026 6:10:51 am GMT+9")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := time.Date(2026, time.July, 31, 21, 10, 51, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

func TestPaymentsTimeEmptyAndInvalid(t *testing.T) {
	got, err := PaymentsTime("")
	if err != nil || !got.IsZero() {
		t.Errorf("empty input should give zero time and no error, got %v %v", got, err)
	}
	if _, err := PaymentsTime("not a timestamp"); err == nil {
		t.Error("expected an error for malformed input")
	}
	// A silently-wrong date would misfile money, so a bad month must fail loudly.
	if _, err := PaymentsTime("1 Foo 2026 6:10:51 am GMT+9"); err == nil {
		t.Error("expected an error for an unknown month")
	}
}

func TestSettlementTime(t *testing.T) {
	cases := map[string]string{
		"17.07.2026 07:26:32 UTC": "2026-07-17T07:26:32Z",
		"17.07.2026":              "2026-07-17T00:00:00Z",
		"02.08.2026 07:06:35 UTC": "2026-08-02T07:06:35Z",
	}
	for in, w := range cases {
		got, err := SettlementTime(in)
		if err != nil {
			t.Errorf("SettlementTime(%q): %v", in, err)
			continue
		}
		want, _ := time.Parse(time.RFC3339, w)
		if !got.Equal(want) {
			t.Errorf("SettlementTime(%q) = %s, want %s", in, got.Format(time.RFC3339), w)
		}
	}
	if got, err := SettlementTime(""); err != nil || !got.IsZero() {
		t.Errorf("empty input should give zero time and no error")
	}
}

// The payments release date and the settlement posted date must reduce to the
// same token, or nothing reconciles. This is the join the whole report rests on.
func TestDateTokenAlignsBothSources(t *testing.T) {
	pay, err := PaymentsTime("18 July 2026 6:10:51 am GMT+9")
	if err != nil {
		t.Fatal(err)
	}
	set, err := SettlementTime("17.07.2026")
	if err != nil {
		t.Fatal(err)
	}
	if DateToken(pay) != DateToken(set) {
		t.Errorf("date tokens differ: payments %q vs settlement %q", DateToken(pay), DateToken(set))
	}
	if DateToken(pay) != "2026-07-17" {
		t.Errorf("got %q, want 2026-07-17", DateToken(pay))
	}
	if DateToken(time.Time{}) != "" {
		t.Error("zero time should produce an empty token, not a date")
	}
}
