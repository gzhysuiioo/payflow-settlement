package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testConfig = `{
  "flags": [
    {
      "key": "f",
      "enabled": true,
      "default": false,
      "rules": [
        {
          "id": "r1",
          "value": true,
          "conditions": [
            {"attribute": "country", "op": "eq", "value": "CN"},
            {"attribute": "country", "op": "in", "value": ["CN", "SG"]}
          ]
        }
      ]
    },
    {
      "key": "off",
      "enabled": false,
      "default": true,
      "rules": []
    }
  ]
}`

func writeTempFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runEvaluate(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestRunEvaluateSuccess(t *testing.T) {
	configPath := writeTempFile(t, "config.json", testConfig)
	contextPath := writeTempFile(t, "context.json", `{"country":"CN"}`)

	code, stdout, stderr := runCLI(t, configPath, "f", contextPath)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("stderr should be empty, got: %s", stderr)
	}
	want := `{"key":"f","value":true,"reason":"rule","ruleId":"r1"}` + "\n"
	if stdout != want {
		t.Fatalf("stdout got %q, want %q", stdout, want)
	}
}

func TestRunEvaluateDisabled(t *testing.T) {
	configPath := writeTempFile(t, "config.json", testConfig)
	contextPath := writeTempFile(t, "context.json", `{}`)

	code, stdout, stderr := runCLI(t, configPath, "off", contextPath)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	want := `{"key":"off","value":false,"reason":"disabled","ruleId":null}` + "\n"
	if stdout != want {
		t.Fatalf("stdout got %q, want %q", stdout, want)
	}
}

func TestRunEvaluateDefault(t *testing.T) {
	configPath := writeTempFile(t, "config.json", testConfig)
	contextPath := writeTempFile(t, "context.json", `{"country":"US"}`)

	code, stdout, stderr := runCLI(t, configPath, "f", contextPath)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	want := `{"key":"f","value":false,"reason":"default","ruleId":null}` + "\n"
	if stdout != want {
		t.Fatalf("stdout got %q, want %q", stdout, want)
	}
}

func TestRunEvaluateWrongArgCount(t *testing.T) {
	tests := [][]string{
		{},
		{"only-one"},
		{"a", "b"},
		{"a", "b", "c", "d"},
	}
	for _, args := range tests {
		code, stdout, stderr := runCLI(t, args...)
		if code != 2 {
			t.Errorf("args %v: exit %d, want 2", args, code)
		}
		if stdout != "" {
			t.Errorf("args %v: stdout should be empty, got %q", args, stdout)
		}
		if !strings.Contains(stderr, "用法") {
			t.Errorf("args %v: stderr should explain usage, got %q", args, stderr)
		}
	}
}

func TestRunEvaluateMissingFiles(t *testing.T) {
	contextPath := writeTempFile(t, "context.json", `{}`)

	code, stdout, stderr := runCLI(t, "/nonexistent/config.json", "f", contextPath)
	if code != 1 || stdout != "" {
		t.Fatalf("code=%d stdout=%q", code, stdout)
	}
	if !strings.Contains(stderr, "无法读取配置文件") {
		t.Fatalf("stderr got %q", stderr)
	}

	configPath := writeTempFile(t, "config.json", testConfig)
	code, stdout, stderr = runCLI(t, configPath, "f", "/nonexistent/context.json")
	if code != 1 || stdout != "" {
		t.Fatalf("code=%d stdout=%q", code, stdout)
	}
	if !strings.Contains(stderr, "无法读取上下文文件") {
		t.Fatalf("stderr got %q", stderr)
	}
}

func TestRunEvaluateInvalidConfig(t *testing.T) {
	configPath := writeTempFile(t, "config.json", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[{"id":"r","value":true,"conditions":[]}]}]}`)
	contextPath := writeTempFile(t, "context.json", `{}`)

	code, stdout, stderr := runCLI(t, configPath, "f", contextPath)
	if code == 0 {
		t.Fatalf("expected non-zero exit")
	}
	if stdout != "" {
		t.Fatalf("stdout should be empty, got %q", stdout)
	}
	if !strings.Contains(stderr, "条件组不能为空") {
		t.Fatalf("stderr should explain cause, got %q", stderr)
	}
}

func TestRunEvaluateFlagNotFound(t *testing.T) {
	configPath := writeTempFile(t, "config.json", testConfig)
	contextPath := writeTempFile(t, "context.json", `{}`)

	code, stdout, stderr := runCLI(t, configPath, "nope", contextPath)
	if code != 1 || stdout != "" {
		t.Fatalf("code=%d stdout=%q", code, stdout)
	}
	if !strings.Contains(stderr, `找不到开关 "nope"`) {
		t.Fatalf("stderr got %q", stderr)
	}
}

func TestRunEvaluateInvalidContext(t *testing.T) {
	configPath := writeTempFile(t, "config.json", testConfig)
	contextPath := writeTempFile(t, "context.json", `{"a":1}`)

	code, stdout, stderr := runCLI(t, configPath, "f", contextPath)
	if code != 1 || stdout != "" {
		t.Fatalf("code=%d stdout=%q", code, stdout)
	}
	if !strings.Contains(stderr, `上下文属性 "a" 的值必须是字符串`) {
		t.Fatalf("stderr got %q", stderr)
	}
}
