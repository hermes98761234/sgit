package cli

import (
	"errors"
	"fmt"
	"testing"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestExitCode(t *testing.T) {
	withDetail, err := status.New(codes.Aborted, "conflict").WithDetails(&sgitv1.ConflictDetail{DraftId: "aaaaaaaa", Main: "m"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"exitError wins", exitf(ExitCAS, "x"), ExitCAS},
		{"exitError wraps a status", &exitError{code: ExitConflict, err: status.Error(codes.Unauthenticated, "u")}, ExitConflict},
		{"unauthenticated", status.Error(codes.Unauthenticated, "u"), ExitAuth},
		{"permission denied", status.Error(codes.PermissionDenied, "p"), ExitAuth},
		{"aborted with detail", withDetail.Err(), ExitConflict},
		{"aborted without detail", status.Error(codes.Aborted, "a"), ExitError},
		{"resource exhausted", status.Error(codes.ResourceExhausted, "r"), ExitCAS},
		{"not found", status.Error(codes.NotFound, "n"), ExitError},
		{"project not found or no access", status.Error(codes.NotFound, `project "web" not found or no access`), ExitAuth},
		{"plain error", errors.New("boom"), ExitError},
		{"wrapped exitError", fmt.Errorf("context: %w", exitf(ExitAuth, "x")), ExitAuth},
	}
	for _, c := range cases {
		if got := exitCode(c.err); got != c.want {
			t.Errorf("%s: exitCode = %d, want %d", c.name, got, c.want)
		}
	}
}
