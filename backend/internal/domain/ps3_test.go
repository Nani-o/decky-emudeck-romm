package domain

import "testing"

func TestParsePS3Identity(t *testing.T) {
	got, err := ParsePS3Identity("EP2007-NPEB00059_00-BZBDSIX000000001", "NPEB00059")
	if err != nil {
		t.Fatal(err)
	}
	if got.ContentID != "EP2007-NPEB00059_00-BZBDSIX000000001" || got.TitleID != "NPEB00059" {
		t.Fatalf("identity %+v", got)
	}
}

func TestParsePS3ContentIDExtractsHeaderProductID(t *testing.T) {
	got, err := ParsePS3ContentID("EP2007-NPEB00059_00-BZBDSIX000000001")
	if err != nil {
		t.Fatal(err)
	}
	if got.ContentID != "EP2007-NPEB00059_00-BZBDSIX000000001" || got.TitleID != "NPEB00059" {
		t.Fatalf("identity %+v", got)
	}
}

func TestParsePS3IdentityRejectsMismatchesAndMalformedIDs(t *testing.T) {
	tests := []struct {
		name, contentID, titleID string
	}{
		{"title mismatch", "EP2007-NPEB00059_00-BZBDSIX000000001", "NPEB00058"},
		{"short content ID", "NPEB00059", "NPEB00059"},
		{"lowercase title", "EP2007-NPEB00059_00-BZBDSIX000000001", "npeb00059"},
		{"bad product", "EP2007-NPEB0005X_00-BZBDSIX000000001", "NPEB0005X"},
		{"bad publisher", "EP20_7-NPEB00059_00-BZBDSIX000000001", "NPEB00059"},
		{"bad description", "EP2007-NPEB00059_00-BZBDSIX00000000!", "NPEB00059"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParsePS3Identity(tt.contentID, tt.titleID); err == nil {
				t.Fatal("expected invalid identity")
			}
		})
	}
}
