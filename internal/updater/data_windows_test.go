//go:build windows

package updater

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestAbortBeforeReplacementWhenDataStillOpen(t *testing.T) {
	p := testPlan(t)
	held, err := os.Open(filepath.Join(p.Options.DataDir, "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	h := helperHooks{wait: func(context.Context, []int) error { return nil }, start: func(Plan) (*exec.Cmd, error) { t.Fatal("started backend despite data lock"); return nil, nil }}
	if err := applyPlan(context.Background(), p, h); err == nil {
		t.Fatal("copied live database")
	}
	hash, _ := HashFile(p.Prepared.TargetPath)
	if hash != p.Prepared.CurrentSHA256 {
		t.Fatal("replaced executable before proving data closed")
	}
}
