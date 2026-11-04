package webretrieve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNetworkIsolatedBridge(t *testing.T) {
	if os.Getenv("BOT_WEB_ISOLATION_TEST") != "1" {
		t.Skip("opt-in Linux network namespace integration test")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("application fetched requested content"))
	}))
	defer server.Close()
	workspace := t.TempDir()
	_, cleanup, err := startBridge(context.Background(), workspace, Retriever{Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	dirs, _ := filepath.Glob(filepath.Join(workspace, ".bot-web-*"))
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "unshare", "--user", "--map-root-user", "--net", executable, "-test.run=^TestIsolatedBridgeHelper$", "-test.v")
	cmd.Env = append(os.Environ(), "BOT_WEB_HELPER_DIR="+dirs[0], "BOT_WEB_HELPER_URL="+server.URL)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("isolated agent: %v\n%s", err, output)
	}
	t.Log(string(output))
}

func TestIsolatedBridgeHelper(t *testing.T) {
	dir := os.Getenv("BOT_WEB_HELPER_DIR")
	if dir == "" {
		t.Skip("subprocess helper")
	}
	target := os.Getenv("BOT_WEB_HELPER_URL")
	client := &http.Client{Timeout: time.Second}
	if response, err := client.Get(target); err == nil {
		response.Body.Close()
		t.Fatal("agent unexpectedly has access to parent network")
	}
	data, _ := json.Marshal(Request{URL: target, Expect: "requested content"})
	if err := os.WriteFile(filepath.Join(dir, "request.tmp"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "request.tmp"), filepath.Join(dir, "request.json")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filepath.Join(dir, "response.json"))
		if err != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		var result Result
		if err = json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		if result.Error != "" || !strings.Contains(result.Content, "application fetched requested content") {
			t.Fatalf("%+v", result)
		}
		t.Log("agent HTTP failed in isolated namespace; application retrieval succeeded through files")
		return
	}
	t.Fatal("timed out waiting for retrieval")
}

func TestFileBridge(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("fixture build log\nfinished"))
	}))
	defer server.Close()
	workspace := t.TempDir()
	instruction, cleanup, err := startBridge(context.Background(), workspace, Retriever{Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	dirs, _ := filepath.Glob(filepath.Join(workspace, ".bot-web-*"))
	if len(dirs) != 1 {
		t.Fatal(dirs)
	}
	if !strings.Contains(instruction, dirs[0]) {
		t.Fatal("missing bridge path")
	}
	outside := filepath.Join(workspace, "outside")
	if err := os.WriteFile(outside, []byte("keep this"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"response.tmp", "response.json"} {
		if err := os.Symlink(outside, filepath.Join(dirs[0], name)); err != nil {
			t.Fatal(err)
		}
	}
	request, _ := json.Marshal(Request{URL: server.URL, Tail: 1, Expect: "build log"})
	tmp := filepath.Join(dirs[0], "request.tmp")
	os.WriteFile(tmp, request, 0600)
	os.Rename(tmp, filepath.Join(dirs[0], "request.json"))
	deadline := time.Now().Add(5 * time.Second)
	var data []byte
	for time.Now().Before(deadline) {
		data, err = os.ReadFile(filepath.Join(dirs[0], "response.json"))
		if err == nil && strings.HasPrefix(string(data), "{") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	var result Result
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Error != "" || result.Content != "2: finished\n" {
		t.Fatalf("%+v", result)
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "keep this" {
		t.Fatal("bridge followed a response symlink")
	}
	cleanup()
	if _, err = os.Stat(dirs[0]); !os.IsNotExist(err) {
		t.Fatal("bridge was not cleaned")
	}
}
