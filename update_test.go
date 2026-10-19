package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"2.0.1", "2.0.0", 1},
		{"2.0.0", "2.0.1", -1},
		{"v2.1.0", "2.1.0", 0},
		{"2.1", "2.1.0", 0},    // 缺省段补 0
		{"2.10.0", "2.9.9", 1}, // 数字比较而不是字符串比较
		{"2.0.0-rc1", "2.0.0", 0},
		{"dev", "0.0.1", -1}, // 本地开发版视为更旧
		{"2.0.1", "dev", 1},
		{"dev", "dev", 0},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, 期望 %d", c.a, c.b, got, c.want)
		}
	}
}

func TestParseUpgradeArgs(t *testing.T) {
	cases := []struct {
		in        string
		wantURL   string
		wantSHA   string
		wantForce bool
	}{
		{"", "", "", false},
		{"https://x/release.json", "https://x/release.json", "", false},
		{"https://x/bin --force", "https://x/bin", "", true},
		{"https://x/bin " + strings.Repeat("a", 64), "https://x/bin", strings.Repeat("a", 64), false},
		{"--force https://x/bin " + strings.Repeat("B", 64), "https://x/bin", strings.Repeat("B", 64), true},
	}
	for _, c := range cases {
		u, sha, force := parseUpgradeArgs(c.in)
		if u != c.wantURL || sha != c.wantSHA || force != c.wantForce {
			t.Errorf("parseUpgradeArgs(%q) = (%q, %q, %v), 期望 (%q, %q, %v)",
				c.in, u, sha, force, c.wantURL, c.wantSHA, c.wantForce)
		}
	}
}

func TestResolveArtifactURL(t *testing.T) {
	cases := []struct{ base, ref, want string }{
		{"https://host/pub/release.json", "feishu-terminal-linux-amd64", "https://host/pub/feishu-terminal-linux-amd64"},
		{"https://host/pub/release.json", "/dl/bin", "https://host/dl/bin"},
		{"https://host/pub/release.json", "https://cdn/x/bin", "https://cdn/x/bin"},
	}
	for _, c := range cases {
		if got := resolveArtifactURL(c.base, c.ref); got != c.want {
			t.Errorf("resolveArtifactURL(%q, %q) = %q, 期望 %q", c.base, c.ref, got, c.want)
		}
	}
}

func TestIsManifestURL(t *testing.T) {
	if !isManifestURL("https://x/release.json") || !isManifestURL("https://x/release.json?t=1") {
		t.Error("release.json 应识别为清单")
	}
	if isManifestURL("https://x/feishu-terminal-linux-amd64") {
		t.Error("裸二进制不应识别为清单")
	}
}

func TestFetchManifest(t *testing.T) {
	m := releaseManifest{
		Version: "9.9.9",
		Notes:   "test",
		Artifacts: map[string]releaseArtifact{
			artifactKey(): {URL: "bin", SHA256: strings.Repeat("a", 64)},
		},
	}
	body, _ := json.Marshal(m)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	got, err := fetchManifest(context.Background(), srv.URL+"/release.json")
	if err != nil {
		t.Fatalf("fetchManifest: %v", err)
	}
	if got.Version != "9.9.9" || got.Artifacts[artifactKey()].URL != "bin" {
		t.Fatalf("清单解析结果不对: %+v", got)
	}

	// 缺字段必须报错，避免拿空清单去"升级"。
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"1.0.0"}`))
	}))
	defer bad.Close()
	if _, err := fetchManifest(context.Background(), bad.URL+"/release.json"); err == nil {
		t.Fatal("缺少 artifacts 的清单应报错")
	}

	// 404 不应无限重试（这里只验证返回错误）。
	nf := httptest.NewServer(http.NotFoundHandler())
	defer nf.Close()
	if _, err := fetchManifest(context.Background(), nf.URL+"/release.json"); err == nil {
		t.Fatal("404 应报错")
	}
}

func TestSelfTestAndInstall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("依赖 /bin/sh")
	}
	dir := t.TempDir()

	// 假"新版本"：一个打印版本行的脚本，验证自检路径。
	fake := filepath.Join(dir, "fake-new")
	script := "#!/bin/sh\necho 'feishu-terminal 9.9.9 (test)'\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	line, err := selfTest(fake)
	if err != nil {
		t.Fatalf("selfTest: %v", err)
	}
	if !strings.Contains(line, "9.9.9") {
		t.Fatalf("自检输出 = %q", line)
	}
	// 坏文件必须被挡住。
	broken := filepath.Join(dir, "broken")
	if err := os.WriteFile(broken, []byte("not a binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := selfTest(broken); err == nil {
		t.Fatal("坏文件应自检失败")
	}

	// installBinary：替换目标，并把旧版本留成 .rollback。
	exe := filepath.Join(dir, "feishu-terminal")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	newBin := filepath.Join(dir, "new")
	if err := os.WriteFile(newBin, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := installBinary(newBin, exe); err != nil {
		t.Fatalf("installBinary: %v", err)
	}
	got, _ := os.ReadFile(exe)
	if string(got) != "new" {
		t.Fatalf("替换后内容 = %q", got)
	}
	old, err := os.ReadFile(exe + ".rollback")
	if err != nil || string(old) != "old" {
		t.Fatalf("备份内容 = %q, err=%v", old, err)
	}
}
