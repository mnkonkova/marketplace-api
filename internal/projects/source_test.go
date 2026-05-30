package projects

import (
	"testing"

	"github.com/google/uuid"
)

func TestDetectSource(t *testing.T) {
	leadID := uuid.New()

	cases := []struct {
		name   string
		leadID *uuid.UUID
		hint   ProjectSource
		want   ProjectSource
	}{
		{"leadID присутствует → marketplace всегда", &leadID, SourceManual, SourceMarketplace},
		{"leadID + hint marketplace → marketplace", &leadID, SourceMarketplace, SourceMarketplace},
		{"leadID + hint referral → marketplace побеждает", &leadID, SourceReferral, SourceMarketplace},
		{"без leadID + manual → manual", nil, SourceManual, SourceManual},
		{"без leadID + referral → referral", nil, SourceReferral, SourceReferral},
		{"без leadID + returning_client → returning_client", nil, SourceReturningClient, SourceReturningClient},
		{"без leadID + marketplace hint → manual (input bug)", nil, SourceMarketplace, SourceManual},
		{"без leadID + пустой hint → manual", nil, "", SourceManual},
		{"без leadID + мусор → manual", nil, ProjectSource("garbage"), SourceManual},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DetectSource(tc.leadID, tc.hint)
			if got != tc.want {
				t.Errorf("DetectSource(leadID=%v, hint=%q) = %q, want %q",
					tc.leadID, tc.hint, got, tc.want)
			}
		})
	}
}
