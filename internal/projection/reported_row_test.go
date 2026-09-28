package projection

import (
	"encoding/json"
	"strings"
	"testing"
)

// A consumer's report arrives in the surface's own naming. Untagged, only MembershipID matched, and a
// report written the way every other body here is written was refused as an unknown field.
func TestAReportedRowReadsTheSurfacesNames(t *testing.T) {
	var row ReportedRow
	decoder := json.NewDecoder(strings.NewReader(`{"membership_id":"01a0e4c2-36dd-7000-97c1-6ab4cae61838","membership_version":4}`))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&row); err != nil {
		t.Fatalf("decoding a reported row: %v", err)
	}
	if row.MembershipID.String() != "01a0e4c2-36dd-7000-97c1-6ab4cae61838" || row.MembershipVersion != 4 {
		t.Errorf("decoded %+v", row)
	}
}
