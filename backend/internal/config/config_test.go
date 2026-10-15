package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestParseLine(t *testing.T) {
	cases := []struct {
		in, key, val string
		ok           bool
	}{
		{"API_Gemini=abc123", "API_Gemini", "abc123", true},
		{"  API_Gemini = abc123  ", "API_Gemini", "abc123", true},
		{`API_Gemini="abc 123"`, "API_Gemini", "abc 123", true},
		{`API_Gemini='abc123'`, "API_Gemini", "abc123", true},
		{"export API_Gemini=abc123", "API_Gemini", "abc123", true},
		{"API_Gemini=abc123 # my key", "API_Gemini", "abc123", true},
		{"URL=https://x.test/a#frag", "URL", "https://x.test/a#frag", true},
		{string(rune(0xFEFF)) + "API_Gemini=abc123", "API_Gemini", "abc123", true},
		{"EMPTY=", "EMPTY", "", true},
		{"# a comment", "", "", false},
		{"", "", "", false},
		{"no equals sign", "", "", false},
		{"=novalue", "", "", false},
		{"1BAD=x", "", "", false},
		{"BAD KEY=x", "", "", false},
	}
	for _, c := range cases {
		k, v, ok := parseLine(c.in)
		if ok != c.ok || k != c.key || v != c.val {
			t.Errorf("parseLine(%q) = (%q,%q,%v), want (%q,%q,%v)", c.in, k, v, ok, c.key, c.val, c.ok)
		}
	}
}

func writeEnv(t *testing.T, root, content string) {
	t.Helper()
	dir := filepath.Join(root, "Secrets")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadSecretsFindsFileInParentAndSetsEnv(t *testing.T) {
	root := t.TempDir()
	writeEnv(t, root, "# secrets\nAPI_Gemini=key-from-file\nOTHER_VAR=hello\n")
	sub := filepath.Join(root, "backend", "cmd")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	for _, n := range []string{"API_Gemini", "OTHER_VAR", "GEMINI_API_KEY", "GOOGLE_API_KEY", EnvFileOverride} {
		t.Setenv(n, "")
		os.Unsetenv(n)
	}

	path, names, err := LoadSecrets()
	if err != nil || path == "" {
		t.Fatalf("path=%q err=%v", path, err)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"API_Gemini", "OTHER_VAR"}) {
		t.Fatalf("names = %v", names)
	}
	if GeminiAPIKey() != "key-from-file" || os.Getenv("OTHER_VAR") != "hello" {
		t.Fatal("variables not loaded")
	}
}

func TestRealEnvironmentWinsOverFile(t *testing.T) {
	root := t.TempDir()
	writeEnv(t, root, "API_Gemini=from-file\n")
	t.Chdir(root)
	t.Setenv("API_Gemini", "from-environment")
	os.Unsetenv(EnvFileOverride)
	_, names, err := LoadSecrets()
	if err != nil || len(names) != 0 || GeminiAPIKey() != "from-environment" {
		t.Fatalf("env must win: names=%v key=%q err=%v", names, GeminiAPIKey(), err)
	}
}

func TestMissingFileIsNotAnError(t *testing.T) {
	t.Chdir(t.TempDir())
	os.Unsetenv(EnvFileOverride)
	if path, names, err := LoadSecrets(); err != nil || path != "" || names != nil {
		t.Fatalf("path=%q names=%v err=%v", path, names, err)
	}
}

func TestExplicitFileOverride(t *testing.T) {
	f := filepath.Join(t.TempDir(), "custom.env")
	if err := os.WriteFile(f, []byte("CUSTOM_ONLY=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvFileOverride, f)
	os.Unsetenv("CUSTOM_ONLY")
	t.Cleanup(func() { os.Unsetenv("CUSTOM_ONLY") })
	if _, names, err := LoadSecrets(); err != nil || !slices.Equal(names, []string{"CUSTOM_ONLY"}) {
		t.Fatalf("names=%v err=%v", names, err)
	}
}

func TestGeminiKeyPrecedence(t *testing.T) {
	for _, n := range []string{"API_Gemini", "GEMINI_API_KEY", "GOOGLE_API_KEY"} {
		t.Setenv(n, "")
	}
	if GeminiAPIKey() != "" {
		t.Fatal("empty values must not count as a key")
	}
	t.Setenv("GOOGLE_API_KEY", "g")
	t.Setenv("GEMINI_API_KEY", "m")
	if GeminiAPIKey() != "m" {
		t.Fatal("GEMINI_API_KEY outranks GOOGLE_API_KEY")
	}
	t.Setenv("API_Gemini", "  a  ")
	if GeminiAPIKey() != "a" {
		t.Fatal("API_Gemini outranks the others and is trimmed")
	}
}
