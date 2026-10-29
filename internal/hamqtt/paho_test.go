package hamqtt

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestLibraryStoreNoiseIsOnlyShownWhenDebugging(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	w := pahoLog{log, slog.LevelWarn}
	w.Printf("[store]   memorystore wiped")
	w.Println("[store]", "memorystore del: message1not found")
	if buf.Len() != 0 {
		t.Fatalf("store noise was logged at info level: %s", buf.String())
	}
	w.Println("Connect failed: not Authorized")
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "not Authorized") {
		t.Fatalf("a real warning was lost: %q", buf.String())
	}
}
