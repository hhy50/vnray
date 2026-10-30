package admin

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func mustWriteLog(t *testing.T, path, content string, appendMode bool) {
	t.Helper()
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if appendMode {
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	f, err := os.OpenFile(path, flags, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(content)
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("write log: %v, close: %v", err, closeErr)
	}
}

func TestLogBatchResumeAndRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	mustWriteLog(t, path, "旧日志\r\npartial", false)
	batch, cursor, err := readLogBatch(path, nil)
	if err != nil || !reflect.DeepEqual(batch.Lines, []string{"旧日志"}) {
		t.Fatalf("initial = %+v, %v", batch, err)
	}
	resumed, err := parseLogCursor(cursor.String())
	if err != nil {
		t.Fatal(err)
	}
	mustWriteLog(t, path, " line\nnew\n", true)
	batch, cursor, err = readLogBatch(path, resumed)
	if err != nil || !reflect.DeepEqual(batch.Lines, []string{"partial line", "new"}) {
		t.Fatalf("append = %+v, %v", batch, err)
	}
	batch, next, err := readLogBatch(path, cursor)
	if err != nil || len(batch.Lines) != 0 || next.String() != cursor.String() {
		t.Fatalf("unchanged file replayed: %+v, %v", batch, err)
	}
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	mustWriteLog(t, path, strings.Repeat("rotated\n", 20), false)
	batch, cursor, err = readLogBatch(path, cursor)
	if err != nil || len(batch.Lines) != 20 || batch.Lines[0] != "rotated" {
		t.Fatalf("rotation = %+v, %v", batch, err)
	}
	mustWriteLog(t, path, "reset\n", false)
	batch, _, err = readLogBatch(path, cursor)
	if err != nil || !reflect.DeepEqual(batch.Lines, []string{"reset"}) {
		t.Fatalf("truncation = %+v, %v", batch, err)
	}
}

func TestLogBatchBoundsAndInvalidCursor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	var content strings.Builder
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&content, "%04d %s\n", i, strings.Repeat("x", 200))
	}
	mustWriteLog(t, path, content.String(), false)
	batch, cursor, err := readLogBatch(path, nil)
	if err != nil || !batch.Truncated || len(batch.Lines) != 300 || !strings.HasPrefix(batch.Lines[0], "1700 ") {
		t.Fatalf("tail count = %d, truncated = %v, error = %v", len(batch.Lines), batch.Truncated, err)
	}
	if cursor.offset != int64(content.Len()) {
		t.Fatalf("cursor = %d, want %d", cursor.offset, content.Len())
	}
	for _, raw := range []string{"-1:" + strings.Repeat("0", 64), "bad", "0:bad", "9223372036854775808:" + strings.Repeat("0", 64)} {
		if _, err := parseLogCursor(raw); err == nil {
			t.Fatalf("invalid cursor accepted: %q", raw)
		}
	}
}

func readStreamBatch(t *testing.T, scanner *bufio.Scanner) (logBatch, string) {
	t.Helper()
	var event, id, data string
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "id: "):
			id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		case line == "":
			if event == "logs" {
				var batch logBatch
				if err := json.Unmarshal([]byte(data), &batch); err != nil {
					t.Fatal(err)
				}
				return batch, id
			}
			event, data = "", ""
		}
	}
	t.Fatalf("stream ended before logs: %v", scanner.Err())
	return logBatch{}, ""
}

func TestLogStreamAppendResumeAndCancel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	mustWriteLog(t, path, "old\n", false)
	s := &AdminService{opts: AdminOptions{LogPath: path}, streamStop: make(chan struct{})}
	finished := make(chan struct{}, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { finished <- struct{}{} }()
		s.apiLogStream(w, r)
	}))
	defer srv.Close()
	s.srv = srv.Config
	client := &http.Client{Timeout: 5 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("content type = %s", resp.Header.Get("Content-Type"))
	}
	scanner := bufio.NewScanner(resp.Body)
	batch, _ := readStreamBatch(t, scanner)
	if !reflect.DeepEqual(batch.Lines, []string{"old"}) {
		t.Fatalf("snapshot = %+v", batch)
	}
	mustWriteLog(t, path, "new\n", true)
	batch, cursor := readStreamBatch(t, scanner)
	if !reflect.DeepEqual(batch.Lines, []string{"new"}) {
		t.Fatalf("incremental = %+v", batch)
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("stream did not stop after client cancellation")
	}
	mustWriteLog(t, path, "after reconnect\n", true)
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"?cursor=invalid", nil)
	req.Header.Set("Last-Event-ID", cursor)
	resp2, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	batch, _ = readStreamBatch(t, bufio.NewScanner(resp2.Body))
	if !reflect.DeepEqual(batch.Lines, []string{"after reconnect"}) {
		t.Fatalf("resumed history was replayed: %+v", batch)
	}
	drainCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := s.Drain(drainCtx); err != nil {
		t.Fatalf("drain active stream: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "old\nnew\nafter reconnect\n" {
		t.Fatalf("read-only stream changed log: %q, %v", got, err)
	}
}

func TestLogStreamAuthentication(t *testing.T) {
	s := newTestAdminService(t)
	h := s.wrapOps(s.buildOpsMux())
	req := httptest.NewRequest(http.MethodGet, "/admin/ops/api/logs/stream", nil)
	req.Header.Set("Accept", "text/event-stream")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous stream = %d", w.Code)
	}
	for _, endpoint := range []string{"/admin/ops/api/logs/stream", "/admin/ops/api/logs/stream?cursor=" + url.QueryEscape("bad")} {
		w = doRequest(t, h, s.auth, http.MethodGet, endpoint)
		if strings.Contains(endpoint, "cursor") {
			if w.Code != http.StatusBadRequest {
				t.Fatalf("bad cursor = %d", w.Code)
			}
		} else if !strings.Contains(w.Body.String(), "event: unavailable") {
			t.Fatalf("no log file = %s", w.Body.String())
		}
	}
}
