package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestProfilesRefuseACLGrantsDespitePrivateMode(t *testing.T) {
	for _, target := range []string{"file", "directory"} {
		t.Run(target, func(t *testing.T) {
			who, path := fixture(t, validConfig)
			grant := "everyone allow read,write"
			if target == "directory" {
				path = filepath.Dir(path)
				grant = "everyone allow add_file,delete_child"
			}
			if output, err := exec.Command("/bin/chmod", "+a", grant, path).CombinedOutput(); err != nil {
				t.Fatalf("create ACL fixture: %v: %s", err, output)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0600 && target == "file" {
				t.Fatal("fixture must retain private mode despite ACL")
			}
			code, stdout, stderr := invoke(t, who, "--profiles")
			if code != 1 || stdout != "" || stderr == "" {
				t.Fatalf("unsafe ACL accepted: code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
		})
	}
}

func TestProfilesAcceptDenyOnlyACL(t *testing.T) {
	who, _ := fixture(t, validConfig)
	if output, err := exec.Command("/bin/chmod", "+a", "everyone deny delete", who.home).CombinedOutput(); err != nil {
		t.Fatalf("create ACL fixture: %v: %s", err, output)
	}
	t.Cleanup(func() {
		if output, err := exec.Command("/bin/chmod", "-N", who.home).CombinedOutput(); err != nil {
			t.Errorf("remove fixture ACL: %v: %s", err, output)
		}
	})
	if code, _, stderr := invoke(t, who, "--profiles"); code != 0 {
		t.Fatalf("deny-only ACL rejected: %s", stderr)
	}
}
