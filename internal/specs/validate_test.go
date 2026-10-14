package specs

import (
	"strings"
	"testing"
)

func TestCheckExtension(t *testing.T) {
	for _, ok := range []string{"a.yaml", "a.yml", "A.YAML", "dir/b.YML"} {
		if err := CheckExtension(ok); err != nil {
			t.Errorf("expected %q to be accepted: %v", ok, err)
		}
	}
	for _, bad := range []string{"a.json", "a.txt", "a", "ayaml", ".yaml.bak"} {
		if err := CheckExtension(bad); err == nil {
			t.Errorf("expected %q to be rejected", bad)
		}
	}
}

func TestParseAndValidate(t *testing.T) {
	valid := []byte("name: my_app\ndependencies:\n  http: ^1.1.0\n")
	spec, verr := ParseAndValidate(valid)
	if verr != nil {
		t.Fatalf("expected valid spec: %v", verr)
	}
	if spec.Name != "my_app" {
		t.Errorf("expected name my_app, got %q", spec.Name)
	}

	cases := []struct {
		name string
		data string
	}{
		{"empty", ""},
		{"not yaml", ":\t: bad \n [unclosed"},
		{"missing name", "dependencies:\n  http: ^1.1.0\n"},
		{"no deps", "name: lonely\n"},
		{"bad name chars", "name: 'evil;name'\ndependencies:\n  http: ^1.1.0\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, verr := ParseAndValidate([]byte(tc.data)); verr == nil {
				t.Errorf("expected validation error for %s", tc.name)
			}
		})
	}
}

func TestParseAndValidateActionable(t *testing.T) {
	_, verr := ParseAndValidate([]byte("dependencies:\n  http: ^1.1.0\n"))
	if verr == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(verr.Message, "name:") {
		t.Errorf("expected actionable message mentioning 'name:', got %q", verr.Message)
	}
}

func TestSanitizeFilename(t *testing.T) {
	if got := SanitizeFilename("../../etc/passwd"); got != "passwd" {
		t.Errorf("expected base name, got %q", got)
	}
	if got := SanitizeFilename(""); got == "" {
		t.Error("expected fallback name")
	}
	if got := SanitizeFilename("my spec.yaml"); got != "my spec.yaml" {
		t.Errorf("expected spaces preserved, got %q", got)
	}
	long := strings.Repeat("a", 300) + ".yaml"
	if got := SanitizeFilename(long); len(got) > 255 {
		t.Errorf("expected truncation, got len %d", len(got))
	}
}

func TestValidID(t *testing.T) {
	for _, id := range []string{"spec_1-abc", "A", "run_123_xyz"} {
		if !ValidID(id) {
			t.Errorf("expected %q valid", id)
		}
	}
	for _, id := range []string{"", "../x", "a/b", ".", "-lead", "with space", strings.Repeat("a", 65)} {
		if ValidID(id) {
			t.Errorf("expected %q invalid", id)
		}
	}
}
