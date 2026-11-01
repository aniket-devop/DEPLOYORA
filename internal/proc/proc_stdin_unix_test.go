//go:build unix

package proc

import (
	"context"
	"strings"
	"testing"
)

func TestStdinIsPassedToTheChild(t *testing.T) {
	spec := helperSpec(t, "stdin")
	spec.Stdin = strings.NewReader("hello")
	res, err := Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.ExitCode != 0 || string(res.Stdout) != "got:hello" {
		t.Errorf("got %+v, stdout %q", res, res.Stdout)
	}
}

func TestNoStdinMeansEmptyInput(t *testing.T) {
	res, err := Run(context.Background(), helperSpec(t, "stdin"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.ExitCode != 0 || string(res.Stdout) != "got:" {
		t.Errorf("got %+v, stdout %q", res, res.Stdout)
	}
}
