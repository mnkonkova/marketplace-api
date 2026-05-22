package leads_test

import (
	"reflect"
	"testing"

	"github.com/google/uuid"

	"marketpclce/internal/leads"
)

func TestDedupUUIDs(t *testing.T) {
	u1 := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	u2 := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	u3 := uuid.MustParse("33333333-3333-3333-3333-333333333333")

	cases := []struct {
		name string
		in   []uuid.UUID
		want []uuid.UUID
	}{
		{name: "nil", in: nil, want: []uuid.UUID{}},
		{name: "empty", in: []uuid.UUID{}, want: []uuid.UUID{}},
		{name: "no dups", in: []uuid.UUID{u1, u2, u3}, want: []uuid.UUID{u1, u2, u3}},
		{name: "with dups", in: []uuid.UUID{u1, u2, u1, u3, u2}, want: []uuid.UUID{u1, u2, u3}},
		{name: "drops nil uuid", in: []uuid.UUID{u1, uuid.Nil, u2}, want: []uuid.UUID{u1, u2}},
		{name: "preserves order", in: []uuid.UUID{u3, u1, u2}, want: []uuid.UUID{u3, u1, u2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := leads.DedupUUIDs(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
