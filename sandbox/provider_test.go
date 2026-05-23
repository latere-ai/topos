package sandbox

import (
	"strings"
	"testing"
)

// TestAPIError_Error asserts that the formatted string includes all four
// fields: status, code, message, and request_id.
func TestAPIError_Error(t *testing.T) {
	cases := []struct {
		name   string
		err    *APIError
		wantIn []string
	}{
		{
			name: "fully populated",
			err: &APIError{
				Status:    500,
				Code:      "internal",
				Message:   "something went wrong",
				RequestID: "req_abc123",
			},
			wantIn: []string{"500", "internal", "something went wrong", "req_abc123"},
		},
		{
			name: "404 not_found",
			err: &APIError{
				Status:    404,
				Code:      "not_found",
				Message:   "sandbox not found",
				RequestID: "req_xyz",
			},
			wantIn: []string{"404", "not_found", "sandbox not found", "req_xyz"},
		},
		{
			name:   "zero values",
			err:    &APIError{},
			wantIn: []string{"0"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.err.Error()
			for _, sub := range c.wantIn {
				if !strings.Contains(got, sub) {
					t.Errorf("Error() = %q, want substring %q", got, sub)
				}
			}
		})
	}
}
