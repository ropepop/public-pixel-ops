package store

import (
	"errors"
	"testing"
)

func TestIsSpacetimePrivateRiderTableError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "private rider table",
			err:  errors.New("spacetime sql failed: no such table: `trainbot_rider`. If the table exists, it may be marked private."),
			want: true,
		},
		{
			name: "marked private rider table",
			err:  errors.New("trainbot_rider may be marked private"),
			want: true,
		},
		{
			name: "other table",
			err:  errors.New("spacetime sql failed: no such table: `trainbot_activity`"),
			want: false,
		},
		{
			name: "nil",
			err:  nil,
			want: false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isSpacetimePrivateRiderTableError(tc.err); got != tc.want {
				t.Fatalf("isSpacetimePrivateRiderTableError() = %v, want %v", got, tc.want)
			}
		})
	}
}
