package fold

import "strings"

// ExtractMerchant pulls a best-guess merchant name out of a fold
// narration string. Returns the raw extracted string — callers
// (the integration sync orchestrator) normalise it with
// integration.NormalizeMerchant before storing/joining.
//
// Patterns observed in production fold data:
//
//	CARD/<id>/<MERCHANT>/<currency>/<amount>/OUTGOING/...
//	  → 3rd field of '/'-split, e.g. "Zomato"
//
//	UPI-<MERCHANT_NAME>-<phone>@<vpa>-<bank>-<txnref>
//	  → 2nd field of '-'-split, e.g. "GOPIKRISHNAN K"
//	  → for VPA-only narrations (UPI-<vpa>-<bank>-...), the 2nd field
//	    is the VPA itself; we still return it (best we can do without
//	    the human supplying a label).
//
// Falls back to the empty string for unrecognised narration shapes —
// callers should treat that as "needs human review" rather than a
// poison value.
func ExtractMerchant(narration, mode string) string {
	n := strings.TrimSpace(narration)
	if n == "" {
		return ""
	}
	upperMode := strings.ToUpper(strings.TrimSpace(mode))
	switch upperMode {
	case "CARD":
		// CARD/<id>/<MERCHANT>/...
		parts := strings.Split(n, "/")
		if len(parts) >= 3 {
			return strings.TrimSpace(parts[2])
		}
	case "UPI", "OTHERS":
		// UPI-<NAME>-<rest>
		// Some fold narrations for non-UPI rails (NEFT, IMPS) also
		// follow this structure under mode=OTHERS, so we try the same
		// pattern.
		if strings.HasPrefix(n, "UPI-") || strings.HasPrefix(n, "upi-") {
			rest := n[len("UPI-"):]
			parts := strings.SplitN(rest, "-", 2)
			if len(parts) >= 1 {
				return strings.TrimSpace(parts[0])
			}
		}
	}
	// Generic fallback: longest token that's at least 4 chars and not
	// a number/currency symbol — best heuristic we have for unseen
	// narration formats. Returns "" if nothing fits, which the caller
	// treats as "queue for human review".
	return ""
}
