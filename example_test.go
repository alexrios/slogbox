package slogbox_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/alexrios/slogbox"
)

func ExampleNew() {
	h := slogbox.New(100, nil)
	logger := slog.New(h)

	logger.Info("server started", "port", 8080)
	logger.Warn("high latency", "ms", 250)

	fmt.Println(h.Len())
	// Output:
	// 2
}

func ExampleHandler_Records() {
	h := slogbox.New(10, nil)
	logger := slog.New(h)

	logger.Info("first")
	logger.Info("second")

	for _, r := range h.Records() {
		fmt.Println(r.Message)
	}
	// Output:
	// first
	// second
}

func ExampleHandler_All() {
	h := slogbox.New(10, nil)
	logger := slog.New(h)

	logger.Info("alpha")
	logger.Info("beta")

	for r := range h.All() {
		fmt.Println(r.Message)
	}
	// Output:
	// alpha
	// beta
}

func ExampleHandler_JSON() {
	h := slogbox.New(10, &slogbox.Options{Level: slog.LevelError})
	logger := slog.New(h)

	logger.Info("ignored") // below Error level
	logger.Error("failure", "code", 500)

	data, err := h.JSON()
	if err != nil {
		panic(err)
	}
	fmt.Println(h.Len())
	fmt.Println(len(data) > 0)
	// Output:
	// 1
	// true
}

func ExampleHandler_WriteTo() {
	h := slogbox.New(10, nil)
	logger := slog.New(h)

	logger.Info("first")
	logger.Info("second")

	var buf bytes.Buffer
	if _, err := h.WriteTo(&buf); err != nil {
		panic(err)
	}

	var entries []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entries); err != nil {
		panic(err)
	}
	for _, e := range entries {
		fmt.Println(e["msg"])
	}
	// Output:
	// first
	// second
}
