package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSharedDirIsDefault(t *testing.T) {
	varLib := t.TempDir()
	// Two mounts of one directory, as the default shared.source makes /shared
	// and /host/var/lib, are the same file, as is a link to it
	same := filepath.Join(t.TempDir(), "shared")
	if err := os.Symlink(varLib, same); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	tests := []struct {
		name    string
		shared  string
		varLib  string
		want    bool
		wantErr bool
	}{
		{name: "default", shared: same, varLib: varLib, want: true},
		{name: "own", shared: t.TempDir(), varLib: varLib},
		{name: "shared missing", shared: missing, varLib: varLib, wantErr: true},
		{name: "var lib missing", shared: same, varLib: missing, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := sharedDirIsDefault(tt.shared, tt.varLib)
			if got != tt.want || (err != nil) != tt.wantErr {
				t.Errorf("sharedDirIsDefault = %v, %v; want %v, error %v",
					got, err, tt.want, tt.wantErr)
			}
		})
	}
}
