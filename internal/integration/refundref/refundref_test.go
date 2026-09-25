package refundref

import "testing"

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		in, kind, uuid string
		journal        int64
	}{
		{"fold:8c88dc3c-2a4b", KindFold, "8c88dc3c-2a4b", 0},
		{"fold:manual-0123abcd", KindFold, "manual-0123abcd", 0},
		{"journal:7008", KindJournal, "", 7008},
		{"none", KindNone, "", 0},
		{" journal:12 ", KindJournal, "", 12},
		{"fold:", "", "", 0},
		{"journal:0", "", "", 0},
		{"journal:x", "", "", 0},
		{"", "", "", 0},
		{"8c88dc3c", "", "", 0},
	} {
		k, u, j := Parse(tc.in)
		if k != tc.kind || u != tc.uuid || j != tc.journal {
			t.Errorf("Parse(%q) = (%q,%q,%d), want (%q,%q,%d)", tc.in, k, u, j, tc.kind, tc.uuid, tc.journal)
		}
	}
	if Fold("u1") != "fold:u1" || Journal(9) != "journal:9" {
		t.Fatal("builders")
	}
	if !Valid("none") || !Valid("journal:3") || Valid("") || Valid("fold:") {
		t.Fatal("Valid")
	}
}
