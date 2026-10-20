package durable

import "testing"

func TestStateDirectoryHasOneOwner(t *testing.T) {
	dir := t.TempDir()
	owner, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := Acquire(dir); err == nil {
		second.Close()
		t.Fatal("second owner accepted")
	}
	owner.Close()
	owner, err = Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	owner.Close()
}
