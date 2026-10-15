// Package config loads local secrets from Secrets/.env into the process environment.
//
// Rules: real environment variables always win over the file; values are never
// logged or returned (only variable names are); the file is expected to be ignored
// by git (see .gitignore).
package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// EnvFileOverride names an environment variable that points at a specific env file.
const EnvFileOverride = "CLAIMGRAPH_ENV_FILE"

// geminiKeyNames are checked in order; the first non-empty value wins.
var geminiKeyNames = []string{"API_Gemini", "GEMINI_API_KEY", "GOOGLE_API_KEY"}

// LoadSecrets finds Secrets/.env (in the working directory or up to five parents) and
// copies each KEY=VALUE into the environment unless that variable is already set.
// It returns the file used and the variable NAMES it set. A missing file is not an error.
func LoadSecrets() (path string, names []string, err error) {
	path = findEnvFile()
	if path == "" {
		return "", nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return path, nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		key, val, ok := parseLine(sc.Text())
		if !ok {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue // the real environment wins
		}
		if err := os.Setenv(key, val); err != nil {
			return path, names, err
		}
		names = append(names, key)
	}
	return path, names, sc.Err()
}

// GeminiAPIKey returns the Gemini API key from the environment ("" if none).
func GeminiAPIKey() string {
	for _, n := range geminiKeyNames {
		if v := strings.TrimSpace(os.Getenv(n)); v != "" {
			return v
		}
	}
	return ""
}

func findEnvFile() string {
	if p := os.Getenv(EnvFileOverride); p != "" {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
		return ""
	}
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for i := 0; i < 6; i++ {
		for _, folder := range []string{"Secrets", "secrets"} {
			p := filepath.Join(dir, folder, ".env")
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// parseLine understands: KEY=VALUE, export KEY=VALUE, quoted values, comments, blanks.
// It performs no variable expansion.
func parseLine(line string) (key, val string, ok bool) {
	line = strings.TrimSpace(strings.TrimPrefix(line, string(rune(0xFEFF)))) // tolerate a UTF-8 BOM
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
	i := strings.Index(line, "=")
	if i <= 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:i])
	if !validName(key) {
		return "", "", false
	}
	val = strings.TrimSpace(line[i+1:])
	if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
		val = val[1 : len(val)-1]
	} else if j := strings.Index(val, " #"); j >= 0 {
		val = strings.TrimSpace(val[:j]) // trailing comment on an unquoted value
	}
	return key, val, true
}

func validName(s string) bool {
	for i, r := range s {
		switch {
		case r == '_', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return s != ""
}
