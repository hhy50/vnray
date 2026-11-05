package admin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const logReadLimit = 256 * 1024

type logCursor struct {
	offset int64
	anchor string
}

type logBatch struct {
	Path      string   `json:"path"`
	Lines     []string `json:"lines"`
	Truncated bool     `json:"truncated"`
}

func parseLogCursor(raw string) (*logCursor, error) {
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ":")
	if len(parts) != 2 {
		return nil, errors.New("invalid log cursor")
	}
	offset, err := strconv.ParseInt(parts[0], 10, 64)
	anchor, hashErr := hex.DecodeString(parts[1])
	if err != nil || offset < 0 || hashErr != nil || len(anchor) != sha256.Size {
		return nil, errors.New("invalid log cursor")
	}
	return &logCursor{offset: offset, anchor: parts[1]}, nil
}

func (c logCursor) String() string {
	return strconv.FormatInt(c.offset, 10) + ":" + c.anchor
}

// 游标校验读取位置之前的内容，使断线续接能识别文件截断与轮转。
func logAnchor(f *os.File, offset int64) (string, error) {
	start := max(int64(0), offset-64)
	buf := make([]byte, offset-start)
	if len(buf) > 0 {
		if _, err := f.ReadAt(buf, start); err != nil {
			return "", err
		}
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:]), nil
}

func readLogBatch(path string, cursor *logCursor) (logBatch, *logCursor, error) {
	batch := logBatch{Path: path, Lines: []string{}}
	f, err := os.Open(path)
	if err != nil {
		return batch, cursor, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return batch, cursor, err
	}
	start := int64(0)
	if cursor != nil && cursor.offset <= info.Size() {
		anchor, err := logAnchor(f, cursor.offset)
		if err == nil && anchor == cursor.anchor {
			start = cursor.offset
		}
	}
	if info.Size()-start > logReadLimit {
		start = info.Size() - logReadLimit
		batch.Truncated = true
	}
	buf := make([]byte, info.Size()-start)
	n, err := f.ReadAt(buf, start)
	if err != nil && !errors.Is(err, io.EOF) {
		return batch, cursor, err
	}
	buf = buf[:n]
	end := bytes.LastIndexByte(buf, '\n') + 1
	offset := start + int64(end)
	if end > 0 {
		complete := buf[:end]
		if batch.Truncated {
			complete = complete[bytes.IndexByte(complete, '\n')+1:]
		}
		if len(complete) > 0 {
			batch.Lines = strings.Split(strings.TrimSuffix(string(complete), "\n"), "\n")
			for i := range batch.Lines {
				batch.Lines[i] = strings.TrimSuffix(batch.Lines[i], "\r")
			}
		}
	}
	if cursor == nil && len(batch.Lines) > 300 {
		batch.Lines = batch.Lines[len(batch.Lines)-300:]
		batch.Truncated = true
	}
	anchor, err := logAnchor(f, offset)
	if err != nil {
		return batch, cursor, err
	}
	return batch, &logCursor{offset: offset, anchor: anchor}, nil
}

func writeLogEvent(w http.ResponseWriter, event, id string, data any) error {
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if id != "" {
		if _, err := fmt.Fprintf(w, "id: %s\n", id); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw); err != nil {
		return err
	}
	return rc.Flush()
}

func (s *AdminService) apiLogStream(w http.ResponseWriter, r *http.Request) {
	rawCursor := r.Header.Get("Last-Event-ID")
	if rawCursor == "" {
		rawCursor = r.URL.Query().Get("cursor")
	}
	cursor, err := parseLogCursor(rawCursor)
	if err != nil {
		apiFailKey(w, r, http.StatusBadRequest, msgBadLogCursor)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	defer func() { _ = http.NewResponseController(w).SetWriteDeadline(time.Time{}) }()
	if s.opts.LogPath == "" {
		_ = writeLogEvent(w, "unavailable", "", "日志未写入文件，请在服务端配置 log.output 文件路径。")
		return
	}
	poll := time.NewTicker(500 * time.Millisecond)
	defer poll.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	first := true
	for {
		if s.auth != nil {
			if _, ok := s.auth.SessionByToken(sessionFromRequest(r)); !ok {
				_ = writeLogEvent(w, "unauthorized", "", "登录已失效，请重新登录")
				return
			}
		}
		batch, next, err := readLogBatch(s.opts.LogPath, cursor)
		if err != nil {
			if err := writeLogEvent(w, "warning", "", "暂时无法读取日志文件，正在重试"); err != nil {
				return
			}
		} else {
			if first || cursor == nil || next.String() != cursor.String() {
				if err := writeLogEvent(w, "logs", next.String(), batch); err != nil {
					return
				}
			}
			cursor = next
			first = false
		}
		select {
		case <-r.Context().Done():
			return
		case <-s.streamStop:
			return
		case <-heartbeat.C:
			if err := writeLogEvent(w, "heartbeat", "", nil); err != nil {
				return
			}
		case <-poll.C:
		}
	}
}
