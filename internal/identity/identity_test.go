package identity

import "testing"

func TestRejectsUnsafeIdentifiers(t *testing.T) {
	for _, name := range []string{"../repo", "/outside", "repo/path", "--git-dir=x", "repo..name", "repo.git"} {
		if Repository(name) == nil {
			t.Fatalf("accepted repository %q", name)
		}
	}
	for _, branch := range []string{"--help", "a..b", "a//b", "a/.hidden", "a.lock", "a/", "bad name"} {
		if Branch(branch) == nil {
			t.Fatalf("accepted branch %q", branch)
		}
	}
	if Repository("my-repo") != nil || Branch("feature/login") != nil {
		t.Fatal("valid identifiers rejected")
	}
}
